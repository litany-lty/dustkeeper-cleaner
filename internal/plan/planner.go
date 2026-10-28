package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/store"
)

// DefaultTTL is the plan approval window. It starts after the item list is
// fixed, so a long deep scan does not consume the user's approval time.
const DefaultTTL = 15 * time.Minute

// Planner turns candidates into stored plans.
type Planner struct {
	Store *store.Store
	Clock domain.Clock
}

// RulePlanRequest describes a lifecycle plan.
type RulePlanRequest struct {
	Scope           store.Scope
	Rule            *rules.Rule
	RuleRef         domain.RuleRef
	Kind            string // manual_rule or scheduled_rule
	Guard           *policy.Guard
	MaxFiles        int
	MaxBytes        int64
	TTL             time.Duration
	QuarantineGrant *store.Grant
	PurgeGrant      *store.Grant
	Config          config.Config
	// AncestorsOf returns the identity of a directory and of every directory
	// above it, nearest first, read through the scope's verified descriptors.
	//
	// The observation index records only the immediate parent, which is enough
	// for an exact protection and for a subtree protection spelled the same way.
	// It is not enough for a subtree protection several levels up whose spelling
	// differs from the one the scan produced - on a case-insensitive volume those
	// are one directory, and only the kernel can say so. The caller supplies the
	// walk because only it holds the scope's descriptors; a plan package that
	// opened its own would be deciding which directory it is looking at.
	//
	// A nil function keeps the older behaviour (compare the recorded parent
	// only); an error means the ancestry cannot be inspected at all, which makes
	// the object ineligible rather than unprotected.
	AncestorsOf func(dir string) ([]domain.FileIdentity, error)
	// NowNS is the evaluation instant. File age is always measured from the
	// start of the scan batch, not from some later moment.
	NowNS int64
	// DryRun builds the plan and its digest without storing anything. Preview
	// uses it, so looking at a plan never creates one.
	DryRun bool
}

// RulePlanResult reports what the planner froze.
type RulePlanResult struct {
	Plan      store.Plan     `json:"plan"`
	Items     []store.Item   `json:"items"`
	Reasons   map[string]int `json:"reasons"`
	Truncated bool           `json:"truncated"`
	Bytes     int64          `json:"selected_logical_bytes"`
	Scanned   int            `json:"scanned"`
}

// CreateRulePlan freezes the current candidates into a plan.
func (p *Planner) CreateRulePlan(ctx context.Context, req RulePlanRequest) (RulePlanResult, error) {
	res := RulePlanResult{Reasons: map[string]int{}}
	if req.TTL <= 0 {
		req.TTL = DefaultTTL
	}
	if req.NowNS == 0 {
		req.NowNS = p.Clock.Wall().UnixNano()
	}
	if req.MaxFiles <= 0 {
		return res, errs.New(errs.CodeInvalidArgument,
			"the action limit for this plan is zero, which means this action is forbidden")
	}

	persistentDigest, rootID, err := scopeIdentity(req.Scope)
	if err != nil {
		return res, err
	}

	var (
		docs    []PlanItemDoc
		items   []store.Item
		total   int64
		ordinal int
	)

	// The ancestry of a candidate depends only on the directory that holds it, so
	// one walk serves every file in that directory. Without the cache a large
	// tree would be walked once per file.
	type ancestry struct {
		ids []domain.FileIdentity
		err error
	}
	walks := map[string]ancestry{}
	ancestorsOf := func(dir string) ([]domain.FileIdentity, error) {
		if req.AncestorsOf == nil {
			return nil, nil
		}
		if got, ok := walks[dir]; ok {
			return got.ids, got.err
		}
		ids, werr := req.AncestorsOf(dir)
		walks[dir] = ancestry{ids: ids, err: werr}
		return ids, werr
	}

	err = p.Store.IterateObservations(ctx, req.Scope.ID, func(o store.Observation) error {
		res.Scanned++
		if len(items) >= req.MaxFiles {
			res.Truncated = true
			// Finish the histogram without selecting anything else so the
			// preview still explains what was left out.
			return nil
		}
		rel := string(o.RawRelative)

		id, err := decodeIdentity(o.IdentityJSON)
		if err != nil {
			res.Reasons[policy.ReasonProtected]++
			return nil
		}
		tok, err := decodeToken(o.TokenJSON)
		if err != nil {
			res.Reasons[policy.ReasonProtected]++
			return nil
		}
		parent, err := decodeIdentity(o.ParentJSON)
		if err != nil {
			res.Reasons[policy.ReasonProtected]++
			return nil
		}
		if o.Status == domain.ObsUnusable {
			res.Reasons[policy.ReasonProtected]++
			return nil
		}

		// The whole chain above the object, not only the recorded parent: a
		// subtree protection recorded for a directory several levels up is
		// invisible from the immediate parent, and the executor honours it. A
		// plan that offered the object anyway would promise an action the run
		// then refuses. When the chain cannot be read at all, the protection
		// cannot be ruled out, which is also a refusal.
		//
		// With no protections at all there is nothing to compare, and walking
		// would only make an unreadable directory look like protection that does
		// not exist.
		ancestors := []domain.FileIdentity{parent}
		if !req.Guard.Empty() {
			got, aerr := ancestorsOf(dirOf(rel))
			if aerr != nil {
				res.Reasons[policy.ReasonProtected]++
				return nil
			}
			if len(got) > 0 {
				ancestors = got
			}
		}

		protected := false
		reason := ""
		if ok, r := policy.GlobalProtected(rel); ok {
			protected, reason = true, "global protection: "+r
		} else if ok, r := req.Guard.CheckFile(rel, id, ancestors...); ok {
			// The planner asks the same question the executor asks, with the
			// parent identity the scan recorded and the identities of every
			// directory above it. CheckExact alone let a subtree protection added
			// after a scan be ignored by a plan created after it.
			protected, reason = true, r
		}

		v := policy.Decide(req.Rule, policy.Input{
			Rel:             rel,
			SizeBytes:       o.SizeBytes,
			MTimeNS:         o.MTimeNS,
			NowNS:           req.NowNS,
			Protected:       protected,
			ProtectedReason: reason,
		})
		res.Reasons[v.ReasonCode]++
		if !v.Candidate {
			return nil
		}
		if total+o.SizeBytes > req.MaxBytes && req.MaxBytes > 0 {
			res.Truncated = true
			return nil
		}

		objectName := domain.NewID("obj")
		itemID := domain.ItemID(objectName)

		binding := ItemBindingV1{
			BindingVersion:           BindingVersion,
			RuleID:                   strPtr(string(req.RuleRef.ID)),
			RuleRevision:             i64Ptr(req.RuleRef.Revision),
			RuleDigest:               strPtr(string(req.RuleRef.Digest)),
			ScopeID:                  string(req.Scope.ID),
			ScopeGeneration:          req.Scope.Generation,
			PersistentIdentityDigest: persistentDigest,
			ProtectionRevision:       req.Scope.ProtectionRevision,
			SafetyPolicyVersion:      SafetyPolicyVersion,
			SourceKind:               req.Kind,
			BoundRetentionSeconds:    req.Rule.RetentionSeconds,
			MetadataPolicyVersion:    MetadataPolicyVersion,
		}
		if req.QuarantineGrant != nil {
			binding.AutoQuarantineGrantID = strPtr(string(req.QuarantineGrant.ID))
			binding.AutoQuarantineGrantDigest = strPtr(string(req.QuarantineGrant.Digest))
		}
		if req.PurgeGrant != nil {
			binding.AutoPurgeGrantID = strPtr(string(req.PurgeGrant.ID))
			binding.AutoPurgeGrantDigest = strPtr(string(req.PurgeGrant.Digest))
		}
		bindingJSON, err := json.Marshal(binding)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}

		item := store.Item{
			ID:                       itemID,
			Ordinal:                  ordinal,
			ScopeID:                  req.Scope.ID,
			RawRelative:              []byte(rel),
			ObjectName:               objectName,
			Reason:                   domain.ReasonLifecycle,
			SourceKind:               req.Kind,
			BindingJSON:              string(bindingJSON),
			SourceParentIdentityJSON: o.ParentJSON,
			PreIdentityJSON:          o.IdentityJSON,
			PreTokenJSON:             o.TokenJSON,
			BoundRetentionSeconds:    req.Rule.RetentionSeconds,
			State:                    domain.StatePlanned,
		}
		if req.PurgeGrant != nil {
			g := req.PurgeGrant.ID
			item.AutoPurgeGrantID = &g
		}

		items = append(items, item)
		docs = append(docs, PlanItemDoc{
			Ordinal:               ordinal,
			ItemID:                string(itemID),
			ScopeID:               string(req.Scope.ID),
			RawRelativeB64:        B64([]byte(rel)),
			ObjectName:            objectName,
			Reason:                domain.ReasonLifecycle,
			SourceKind:            req.Kind,
			BoundRetentionSeconds: req.Rule.RetentionSeconds,
			RuleID:                strPtr(string(req.RuleRef.ID)),
			RuleRevision:          i64Ptr(req.RuleRef.Revision),
			RuleDigest:            strPtr(string(req.RuleRef.Digest)),
			SourceParentIdentity:  parent,
			PreIdentity:           id,
			PreToken:              tok,
			Keeper:                nil,
		})
		total += o.SizeBytes
		ordinal++
		return nil
	})
	if err != nil {
		return res, err
	}

	if len(items) == 0 {
		return res, errs.New(errs.CodeNoCandidates,
			"no file in this scope currently matches the rule")
	}

	planID := domain.PlanID(domain.NewID("plan"))
	nowNS := p.Clock.Wall().UnixNano()
	expiresNS := nowNS + int64(req.TTL)

	configDigest, err := ConfigDigestOf(configDigestInput(req.Config))
	if err != nil {
		return res, err
	}

	doc := PlanDoc{
		PlanID:       string(planID),
		Kind:         req.Kind,
		CreatedAtNS:  nowNS,
		ExpiresAtNS:  expiresNS,
		Bindings:     []PlanBinding{{ScopeID: string(req.Scope.ID), Generation: req.Scope.Generation, PersistentIdentityDigest: persistentDigest}},
		PolicyDigest: string(policy.PolicyDigest()),
		ConfigDigest: string(configDigest),
		Items:        docs,
	}
	digest, err := doc.ComputeDigest()
	if err != nil {
		return res, err
	}

	header := domain.PlanHeader{
		ID:           planID,
		Kind:         req.Kind,
		CreatedAtNS:  nowNS,
		ExpiresAtNS:  expiresNS,
		Bindings:     []domain.ScopeBinding{{Scope: req.Scope.ID, Generation: req.Scope.Generation, Root: rootID}},
		PolicyDigest: policy.PolicyDigest(),
		ConfigDigest: configDigest,
		Digest:       digest,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	stored := store.Plan{
		ID:          planID,
		Kind:        req.Kind,
		HeaderJSON:  string(headerJSON),
		Digest:      digest,
		Status:      domain.PlanReady,
		CreatedAtNS: nowNS,
		ExpiresAtNS: expiresNS,
	}
	if !req.DryRun {
		if err := p.Store.InsertPlan(ctx, stored, items); err != nil {
			return res, err
		}
	}

	res.Plan = stored
	res.Items = items
	res.Bytes = total
	return res, nil
}

// BuildDocFromStore reconstructs the digest document from stored rows. It is
// used to verify that the plan on disk still hashes to the approved digest.
func BuildDocFromStore(p store.Plan, items []store.Item) (PlanDoc, error) {
	var header domain.PlanHeader
	if err := json.Unmarshal([]byte(p.HeaderJSON), &header); err != nil {
		return PlanDoc{}, errs.Newf(errs.CodeStateWriteFailed, "plan %s header is unreadable: %v", p.ID, err)
	}
	doc := PlanDoc{
		PlanID:       string(p.ID),
		Kind:         p.Kind,
		CreatedAtNS:  p.CreatedAtNS,
		ExpiresAtNS:  p.ExpiresAtNS,
		PolicyDigest: string(header.PolicyDigest),
		ConfigDigest: string(header.ConfigDigest),
	}
	for _, b := range header.Bindings {
		d, err := Digest("dustkeeper/root-identity/v1", b.Root.Persistent)
		if err != nil {
			return PlanDoc{}, err
		}
		doc.Bindings = append(doc.Bindings, PlanBinding{
			ScopeID:                  string(b.Scope),
			Generation:               b.Generation,
			PersistentIdentityDigest: string(d),
		})
	}

	for _, it := range items {
		d, err := itemDocOf(it)
		if err != nil {
			return PlanDoc{}, err
		}
		doc.Items = append(doc.Items, d)
	}
	return doc, nil
}

// itemDocOf rebuilds the immutable per-item contract from a stored row.
//
// The planner and the verifier both call it. If they built the document
// separately the digests would drift the first time a field was added, and a
// drift shows up as a plan that cannot be applied.
func itemDocOf(it store.Item) (PlanItemDoc, error) {
	preID, err := decodeIdentity(it.PreIdentityJSON)
	if err != nil {
		return PlanItemDoc{}, errs.Newf(errs.CodeStateWriteFailed, "item %s identity unreadable: %v", it.ID, err)
	}
	preTok, err := decodeToken(it.PreTokenJSON)
	if err != nil {
		return PlanItemDoc{}, errs.Newf(errs.CodeStateWriteFailed, "item %s token unreadable: %v", it.ID, err)
	}
	parent, err := decodeIdentity(it.SourceParentIdentityJSON)
	if err != nil {
		return PlanItemDoc{}, errs.Newf(errs.CodeStateWriteFailed, "item %s parent identity unreadable: %v", it.ID, err)
	}

	var binding ItemBindingV1
	_ = json.Unmarshal([]byte(it.BindingJSON), &binding)

	var keeper *KeeperEvidenceV1
	if it.KeeperJSON != nil {
		var k KeeperEvidenceV1
		if err := json.Unmarshal([]byte(*it.KeeperJSON), &k); err == nil {
			keeper = &k
		}
	}

	return PlanItemDoc{
		Ordinal:               it.Ordinal,
		ItemID:                string(it.ID),
		ScopeID:               string(it.ScopeID),
		RawRelativeB64:        B64(it.RawRelative),
		ObjectName:            it.ObjectName,
		Reason:                it.Reason,
		SourceKind:            it.SourceKind,
		BoundRetentionSeconds: it.BoundRetentionSeconds,
		RuleID:                binding.RuleID,
		RuleRevision:          binding.RuleRevision,
		RuleDigest:            binding.RuleDigest,
		SourceParentIdentity:  parent,
		PreIdentity:           preID,
		PreToken:              preTok,
		Keeper:                keeper,
	}, nil
}

// VerifyPlanDigest recomputes the plan digest and compares it with the stored and
// approved values. A mismatch means the plan was modified outside this program.
// LoadMembers returns a plan's members in the shape the digest was computed
// from, and verifies the approved digest against them.
//
// A purge plan keeps its members in purge_members, not in items.plan_id. Two CLI
// routes loaded members themselves, and the generic `plan apply` loaded the
// wrong table: for a purge plan it found no items and refused before reaching
// the executor, while `purge plan apply` worked. Loading by kind, in one place,
// is what keeps the two routes from disagreeing again.
func LoadMembers(ctx context.Context, st *store.Store, p store.Plan, approved domain.Digest) ([]store.Item, error) {
	var items []store.Item
	switch p.Kind {
	case domain.PlanManualPurge, domain.PlanScheduledPurge:
		members, err := st.ListPurgeMembers(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			items = append(items, m.Item)
		}
	default:
		list, err := st.ListItemsByPlan(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		items = list
	}
	if len(items) == 0 {
		return nil, errs.Newf(errs.CodePlanStale, "plan %s has no members", p.ID)
	}
	if err := VerifyPlanDigest(p, items, approved); err != nil {
		return nil, err
	}
	return items, nil
}

func VerifyPlanDigest(p store.Plan, items []store.Item, approved domain.Digest) error {
	if approved != "" && approved != p.Digest {
		return errs.Newf(errs.CodeDigestMismatch,
			"approved digest %s does not match plan %s digest %s", approved, p.ID, p.Digest)
	}
	doc, err := BuildDocFromStore(p, items)
	if err != nil {
		return err
	}
	recomputed, err := doc.ComputeDigest()
	if err != nil {
		return err
	}
	if recomputed != p.Digest {
		return errs.Newf(errs.CodeDigestMismatch,
			"plan %s no longer matches its digest (recomputed %s)", p.ID, recomputed)
	}
	return nil
}

// scopeIdentity parses the stored root identity and hashes its persistent half.
func scopeIdentity(sc store.Scope) (string, domain.RootIdentity, error) {
	var id domain.RootIdentity
	if err := json.Unmarshal([]byte(sc.RootIdentityJSON), &id); err != nil {
		return "", domain.RootIdentity{}, errs.Newf(errs.CodeStateWriteFailed,
			"scope %s identity is unreadable: %v", sc.ID, err)
	}
	d, err := Digest("dustkeeper/root-identity/v1", id.Persistent)
	if err != nil {
		return "", domain.RootIdentity{}, err
	}
	return string(d), id, nil
}

func configDigestInput(c config.Config) ConfigDigestInput {
	return ConfigDigestInput{
		MaxAutoQuarantineFiles: c.Runtime.MaxAutoQuarantineFiles,
		MaxAutoQuarantineBytes: c.Runtime.MaxAutoQuarantineBytes,
		MaxAutoPurgeFiles:      c.Runtime.MaxAutoPurgeFiles,
		MaxAutoPurgeBytes:      c.Runtime.MaxAutoPurgeBytes,
		RunSoftSeconds:         c.Runtime.RunSoftSeconds,
		RunReadBudgetBytes:     c.Runtime.RunReadBudgetBytes,
		ReadRateBytesPerSecond: c.Runtime.ReadRateBytesPerSecond,
		MinFreeBytes:           c.Runtime.MinFreeBytes,
		MinFreePercent:         c.Runtime.MinFreePercent,
		MetadataPolicyVersion:  MetadataPolicyVersion,
	}
}

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

func decodeIdentity(s string) (domain.FileIdentity, error) {
	var id domain.FileIdentity
	err := json.Unmarshal([]byte(s), &id)
	return id, err
}

func decodeToken(s string) (domain.ChangeToken, error) {
	var t domain.ChangeToken
	err := json.Unmarshal([]byte(s), &t)
	return t, err
}

// DescribeDiff renders a short explanation of why a digest changed, for the CLI.
func DescribeDiff(want, got domain.Digest) string {
	if want == got {
		return "identical"
	}
	return fmt.Sprintf("expected %s, got %s", want, got)
}

// dirOf returns the directory part of a scope-relative path, in the form the
// ancestry walk expects: no trailing slash, and empty for the scope root.
func dirOf(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}
