package execute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/store"
)

// KeeperVerifier is implemented by the dedup package. It is an interface here so
// that the executor does not depend on duplicate detection.
type KeeperVerifier interface {
	VerifyKeeper(ctx context.Context, it store.Item) error
}

// PurgeOptions describes one purge request.
type PurgeOptions struct {
	Automatic bool
	Plan      *store.Plan
	Budget    *domain.Budget
	// ClockTrusted is false when the run detected a wall-clock anomaly. Purge
	// then refuses: retention cannot be proven, so it is not served.
	ClockTrusted bool
}

// Purge permanently removes one quarantined object.
//
// It only ever unlinks a generated name inside the private vault. The original
// relative path is never passed to unlink, and every condition is re-checked at
// the last moment: the retention window having elapsed is not permission.
func (e *Executor) Purge(ctx context.Context, sc *ScopeCtx, it store.Item, opt PurgeOptions) (domain.OperationResult, error) {
	res := domain.OperationResult{Item: it.ID, State: it.State}

	if it.State != domain.StateQuarantined {
		return res, errs.Newf(errs.CodePlanStale, "item %s is %s, not quarantined", it.ID, it.State)
	}
	if !opt.ClockTrusted {
		return res, errs.New(errs.CodeClockUntrusted,
			"the wall clock is not trustworthy in this run; automatic purging is suspended")
	}
	nowNS := e.Clock.Wall().UnixNano()
	if it.PurgeNotBeforeNS == nil || nowNS < *it.PurgeNotBeforeNS {
		return res, errs.New(errs.CodePlanStale, "the retention period for this object has not elapsed")
	}
	if it.CapturedIdentityJSON == nil || it.CapturedTokenJSON == nil || it.ContentHash == nil {
		return res, errs.New(errs.CodeHeld, "this object has no complete post-move evidence; reconcile first")
	}

	binding, err := PlanBindingOf(it)
	if err != nil {
		return res, err
	}

	// Authority. A manual purge needs an approved plan bound to the current
	// policy; an automatic purge needs the grant that was bound at quarantine
	// time, and that grant must still be active.
	if opt.Automatic {
		if err := e.checkAutomaticAuthority(ctx, sc, it, binding); err != nil {
			return res, err
		}
	} else {
		if opt.Plan == nil {
			return res, errs.New(errs.CodeGrantRequired, "a purge requires an explicit plan")
		}
		if opt.Plan.Kind != domain.PlanManualPurge && opt.Plan.Kind != domain.PlanScheduledPurge {
			return res, errs.New(errs.CodePlanStale, "this plan is not a purge plan")
		}
	}

	// Protection is checked against the recorded original path, not against a
	// path that currently exists. A missing file does not mean the protection
	// went away.
	if blocked, why, err := e.purgeBlockedByProtection(ctx, sc, it); err != nil {
		return res, err
	} else if blocked {
		return res, errs.Newf(errs.CodeHeld, "the original path is protected: %s", why)
	}

	capturedID, err := decodeID(*it.CapturedIdentityJSON)
	if err != nil {
		return res, err
	}
	capturedTok, err := decodeTok(*it.CapturedTokenJSON)
	if err != nil {
		return res, err
	}

	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return res, errs.Newf(errs.CodeHeld,
			"the quarantined object is missing or unreadable: %v; reconcile before purging", err)
	}
	defer obj.Close()

	if !obj.Identity.Equal(capturedID) {
		return res, errs.New(errs.CodeHeld, "the quarantined object is not the recorded object")
	}
	if obj.Token.Size != capturedTok.Size || obj.Token.MTimeNS != capturedTok.MTimeNS {
		return res, errs.New(errs.CodeHeld, "the quarantined object changed after it was isolated")
	}

	ev, err := MetadataOf(obj)
	if err != nil {
		return res, err
	}
	if ev.Unsupported != "" {
		return res, errs.Newf(errs.CodeUnsupportedMetadata,
			"metadata cannot be re-verified: %s", ev.Unsupported)
	}
	if it.MetadataHash != nil {
		if got := MetadataDigest(ev, obj.Token); string(got) != *it.MetadataHash {
			return res, errs.New(errs.CodeHeld, "the quarantined object's metadata changed")
		}
	}

	if opt.Budget != nil && opt.Budget.RemainingRead() < obj.Token.Size {
		return res, errs.New(errs.CodeBudgetExhausted, "not enough read budget to re-verify this object")
	}
	vopt := e.VerifyOpt(obj.Token.Size)
	vopt.Budget = opt.Budget
	hash, _, err := HashFile(obj, vopt)
	if err != nil {
		return res, err
	}
	if string(hash) != *it.ContentHash {
		return res, errs.New(errs.CodeHeld, "the quarantined object's content changed")
	}

	// Exact duplicates must still have a living keeper.
	if it.Reason == domain.ReasonExactDuplicate {
		if e.Keeper == nil {
			return res, errs.New(errs.CodeHeld, "no keeper verifier is configured; refusing to purge a duplicate")
		}
		if err := e.Keeper.VerifyKeeper(ctx, it); err != nil {
			return res, err
		}
	}

	if opt.Budget != nil && !opt.Budget.CanReserve(obj.Token.Size) {
		return res, errs.New(errs.CodeBudgetExhausted, "the action budget for this run is exhausted")
	}

	opID := domain.OperationID(domain.NewID("op"))
	contract := plan.OperationContractV1{
		BindingVersion:  plan.BindingVersion,
		ItemID:          string(it.ID),
		PriorState:      domain.StateQuarantined,
		NextState:       domain.StatePIntent,
		Purpose:         plan.PurposeNormal,
		ScopeID:         string(sc.Scope.ID),
		RawRelativeB64:  plan.B64(it.RawRelative),
		ObjectName:      it.ObjectName,
		ExpectedIdent:   capturedID,
		ExpectedToken:   capturedTok,
		ContentHash:     it.ContentHash,
		MetadataHash:    it.MetadataHash,
		Binding:         binding,
		MaxFiles:        e.Config.MaxFiles,
		MaxLogicalBytes: e.Config.MaxBytes,
	}
	if opt.Plan != nil {
		contract.ExecutionPlanID = strPtr(string(opt.Plan.ID))
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	intent := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "purge",
		PriorState:   domain.StateQuarantined,
		NextState:    domain.StatePIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: nowNS,
	}
	if opt.Plan != nil {
		intent.PlanID = &opt.Plan.ID
	}

	// The pause gate also covers purging: a pause request must be able to stop
	// the next permanent deletion, not only the next quarantine.
	if err := e.Gate.CheckAndCommit(func() error { return e.Store.BeginIntent(ctx, intent) }); err != nil {
		return res, err
	}

	if err := fsops.UnlinkAt(sc.Objects, it.ObjectName); err != nil {
		if errors.Is(err, unix.ENOENT) {
			// The object vanished without our unlink; that is not a purge we
			// performed, so it must be reconciled rather than declared success.
			_ = e.Store.HoldIntent(ctx, intent, "OBJECT_VANISHED",
				`{"source":"observed","result":"object was gone before unlink"}`)
			return res, errs.New(errs.CodeHeld, "the object was already gone; reconcile this item")
		}
		_ = e.Store.AbortIntent(ctx, intent, domain.StateQuarantined,
			`{"reason":"unlink failed","detail":`+quoteJSON(err.Error())+`}`)
		return res, errs.Newf(errs.CodeIOError, "unlink failed; the object is still quarantined: %v", err)
	}

	if err := fsops.DurableDir(sc.Objects); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if sc.AnchorFD() >= 0 {
		if err := fsops.Barrier(sc.AnchorFD(), []byte("purge")); err != nil {
			return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
		}
	}

	if err := e.Store.CompletePurge(ctx, intent, `{"source":"observed"}`); err != nil {
		return res, err
	}
	if err := e.Store.ReleaseItemKeeperPin(ctx, it.ID); err != nil {
		return res, err
	}

	// The handle stays open until the directory is synced; only then is the
	// space actually released for observers.
	_ = obj.Close()

	if opt.Budget != nil {
		opt.Budget.AddAction(obj.Token.Size)
	}

	res.State = domain.StatePurged
	res.Code = string(errs.CodeOK)
	res.Operation = opID
	return res, nil
}

// checkAutomaticAuthority verifies the standing grant that allows unattended
// permanent deletion, and refuses the history that never had one.
func (e *Executor) checkAutomaticAuthority(ctx context.Context, sc *ScopeCtx, it store.Item, b plan.ItemBindingV1) error {
	if it.SourceKind != domain.SourceScheduledRule {
		return errs.New(errs.CodeGrantRequired,
			"this object was isolated by a manual plan; automatic purge does not extend to it")
	}
	if it.AutoPurgeGrantID == nil {
		return errs.New(errs.CodeGrantRequired,
			"this object was isolated without an auto-purge grant; grant it is not retroactive")
	}
	if sc.Scope.Kind != domain.KindManagedDisposable {
		return errs.Newf(errs.CodeGrantRequired,
			"scope %s is %s; automatic purge requires managed_disposable", sc.Scope.ID, sc.Scope.Kind)
	}
	g, err := e.Store.GetGrant(ctx, *it.AutoPurgeGrantID)
	if err != nil {
		return err
	}
	if g.Status != domain.GrantActive {
		return errs.Newf(errs.CodeGrantRequired, "the auto-purge grant is %s", g.Status)
	}
	if g.Capability != domain.CapAutoPurge {
		return errs.New(errs.CodeGrantRequired, "the bound grant is not an auto-purge grant")
	}
	if b.RuleID == nil || b.RuleRevision == nil {
		return errs.New(errs.CodeGrantRequired, "this item has no rule binding")
	}
	if string(g.RuleID) != *b.RuleID || g.RuleRevision != *b.RuleRevision {
		return errs.New(errs.CodeGrantRequired,
			"the grant is bound to a different rule revision than this object")
	}
	if g.ScopeID != sc.Scope.ID {
		return errs.New(errs.CodeGrantRequired, "the grant belongs to a different scope")
	}

	// The rule that produced this object must still be the active revision.
	active, err := e.Store.ActiveRule(ctx, domain.RuleID(*b.RuleID))
	if err != nil {
		return errs.New(errs.CodeGrantRequired, "the rule for this object is no longer active")
	}
	if active.Revision != *b.RuleRevision {
		return errs.New(errs.CodeGrantRequired,
			"the rule was revised after this object was isolated; the old grant does not carry over")
	}
	if b.RuleDigest != nil && string(active.Digest) != *b.RuleDigest {
		return errs.New(errs.CodeGrantRequired, "the rule content changed after this object was isolated")
	}
	if b.BoundRetentionSeconds != it.BoundRetentionSeconds {
		return errs.New(errs.CodeGrantRequired, "the retention bound on this item is inconsistent")
	}
	return nil
}

// purgeBlockedByProtection evaluates protections against the recorded original
// path slot.
//
// The path usually no longer exists, so "ENOENT means no protection" would be
// exactly wrong. There are two entries, as section 11.4 requires:
//
//  1. a literal comparison of the recorded relative path (exact match or full
//     segment prefix), which holds even though nothing is there any more;
//  2. alias resolution by kernel identity — a protection spelled differently
//     but living in the same directory, or even resolving to the very object
//     that now sits in the vault, still protects this slot.
//
// When the original directory itself cannot be resolved, nothing can be
// compared by identity any more. The answer is then PROTECTION_ALIAS_UNRESOLVED
// rather than a guess: freezing a scope is recoverable, deleting a file that a
// protection was meant to keep is not.
func (e *Executor) purgeBlockedByProtection(ctx context.Context, sc *ScopeCtx, it store.Item) (bool, string, error) {
	ps, err := e.Store.ListProtections(ctx, sc.Scope.ID, true)
	if err != nil {
		return false, "", err
	}
	rel := string(it.RawRelative)
	for _, p := range ps {
		target := string(p.RawRelative)
		switch p.MatchKind {
		case domain.MatchExact:
			if target == rel {
				return true, "exact protection held by " + p.Source, nil
			}
		case domain.MatchSubtree:
			if policy.HasDirPrefix(rel, target) {
				return true, "subtree protection held by " + p.Source, nil
			}
		}
	}
	if len(ps) == 0 {
		return false, "", nil
	}

	storedParent, err := decodeID(it.SourceParentIdentityJSON)
	if err != nil {
		return false, "", err
	}
	var captured domain.FileIdentity
	if it.CapturedIdentityJSON != nil {
		if captured, err = decodeID(*it.CapturedIdentityJSON); err != nil {
			return false, "", err
		}
	}

	itemParent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the original directory of %q cannot be resolved, so protections cannot be compared by identity; resolve or remove them before purging", rel)
	}
	defer itemParent.Close()
	itemParentID, _, err := itemParent.Stat()
	if err != nil {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the original directory of %q cannot be inspected: %v", rel, err)
	}
	if !itemParentID.Equal(storedParent) {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the original directory of %q was replaced, so protections cannot be matched by identity", rel)
	}

	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}

	for _, p := range ps {
		target := strings.TrimSuffix(string(p.RawRelative), "/")
		if target == "" {
			continue
		}
		parent, err := sc.Root.OpenParent([]byte(target))
		if err != nil {
			// This protection's own parent is gone; it says nothing about the
			// slot being purged, so it is not evidence either way.
			continue
		}
		pid, _, serr := parent.Stat()
		if serr != nil {
			parent.Close()
			continue
		}
		if !pid.Equal(itemParentID) {
			parent.Close()
			continue
		}
		entryID, _, eerr := parent.StatAtNoFollow()
		parent.Close()
		if eerr == nil {
			// The entry exists under another spelling. If it is the object now
			// held in the vault, the kernel has proven an alias.
			if !captured.IsZero() && entryID.Equal(captured) {
				return true, "an equivalent spelling of this path is protected by " + p.Source, nil
			}
			continue
		}
		if baseOf(target) == base {
			return true, "the original path slot is protected by " + p.Source, nil
		}
		// Same directory, no entry, different spelling. A case or normalisation
		// alias cannot be ruled out without inventing a casefold, so the scope is
		// frozen instead.
		if strings.EqualFold(base, baseOf(target)) {
			return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
				"a protection in the same directory (%q) differs only in spelling; resolve or remove it before purging", target)
		}
	}
	return false, "", nil
}

// baseOf returns the last path segment of a relative path.
func baseOf(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
