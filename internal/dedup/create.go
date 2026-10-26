package dedup

import (
	"context"
	"encoding/json"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// duplicateRetentionSeconds is the fixed retention for exact-duplicate objects.
// It is shown in the plan and included in the digest; there is no second,
// hidden retention that could shorten it.
const duplicateRetentionSeconds int64 = 30 * 86400

// RetentionSeconds reports the fixed retention shown to the user. It exists so
// the CLI prints the same constant the plan bound, not a copy of it.
func RetentionSeconds() int64 { return duplicateRetentionSeconds }

// CreatePlan freezes one keeper per group and one item per remaining copy.
//
// The keeper is never planned for movement. Every candidate records the keeper
// evidence that will have to be re-proved before the copy can be purged.
func (d *Detector) CreatePlan(ctx context.Context, sc *execute.ScopeCtx, groups []Group) (store.Plan, []store.Item, error) {
	nowNS := d.Clock.Wall().UnixNano()
	expiresNS := nowNS + int64(plan.DefaultTTL)

	persistentDigest, rootID, err := scopeIdentityOf(sc.Scope)
	if err != nil {
		return store.Plan{}, nil, err
	}
	configDigest, err := plan.ConfigDigestOf(plan.ConfigDigestInput{MetadataPolicyVersion: plan.MetadataPolicyVersion})
	if err != nil {
		return store.Plan{}, nil, err
	}

	planID := domain.PlanID(domain.NewID("plan"))
	var (
		items []store.Item
		docs  []plan.PlanItemDoc
		total int64
	)

	for _, g := range groups {
		if !g.MetadataCompatible || len(g.Others) == 0 {
			continue
		}
		keeper := plan.KeeperEvidenceV1{
			ScopeID:        string(g.Keeper.ScopeID),
			RawRelativeB64: plan.B64(g.Keeper.Rel),
			Identity:       g.Keeper.Identity,
			Token:          g.Keeper.Token,
			ContentHash:    string(g.Keeper.Hash),
			MetadataHash:   string(g.Keeper.MetaHash),
			MetadataPolicy: plan.MetadataPolicyVersion,
		}
		keeperJSON, err := json.Marshal(keeper)
		if err != nil {
			return store.Plan{}, nil, errs.Wrap(errs.CodeStateWriteFailed, err)
		}

		for _, other := range g.Others {
			objectName := domain.NewID("obj")
			itemID := domain.ItemID(objectName)

			binding := plan.ItemBindingV1{
				BindingVersion:           plan.BindingVersion,
				ScopeID:                  string(sc.Scope.ID),
				ScopeGeneration:          sc.Scope.Generation,
				PersistentIdentityDigest: persistentDigest,
				ProtectionRevision:       sc.Scope.ProtectionRevision,
				SafetyPolicyVersion:      plan.SafetyPolicyVersion,
				SourceKind:               domain.SourceManualDuplicate,
				BoundRetentionSeconds:    duplicateRetentionSeconds,
				MetadataPolicyVersion:    plan.MetadataPolicyVersion,
			}
			bindingJSON, err := json.Marshal(binding)
			if err != nil {
				return store.Plan{}, nil, errs.Wrap(errs.CodeStateWriteFailed, err)
			}

			if other.ParentJSON == "" || other.Parent.IsZero() {
				// Without the parent identity the executor cannot tell whether
				// the path still points at the same directory. Planning it would
				// create an item that can never be applied.
				continue
			}
			contentHash := string(other.Hash)
			metaHash := string(other.MetaHash)
			keeperScope := g.Keeper.ScopeID
			item := store.Item{
				ID:                       itemID,
				Ordinal:                  len(items),
				ScopeID:                  sc.Scope.ID,
				RawRelative:              other.Rel,
				ObjectName:               objectName,
				Reason:                   domain.ReasonExactDuplicate,
				SourceKind:               domain.SourceManualDuplicate,
				BindingJSON:              string(bindingJSON),
				SourceParentIdentityJSON: other.ParentJSON,
				PreIdentityJSON:          mustMarshal(other.Identity),
				PreTokenJSON:             mustMarshal(other.Token),
				BoundRetentionSeconds:    duplicateRetentionSeconds,
				ContentHash:              &contentHash,
				MetadataHash:             &metaHash,
				KeeperJSON:               ptr(string(keeperJSON)),
				KeeperScopeID:            &keeperScope,
				KeeperRawRelative:        g.Keeper.Rel,
				KeeperPinActive:          true,
				State:                    domain.StatePlanned,
			}

			items = append(items, item)
			docs = append(docs, plan.PlanItemDoc{
				Ordinal:               item.Ordinal,
				ItemID:                string(itemID),
				ScopeID:               string(sc.Scope.ID),
				RawRelativeB64:        plan.B64(other.Rel),
				ObjectName:            objectName,
				Reason:                domain.ReasonExactDuplicate,
				SourceKind:            domain.SourceManualDuplicate,
				BoundRetentionSeconds: duplicateRetentionSeconds,
				SourceParentIdentity:  other.Parent,
				PreIdentity:           other.Identity,
				PreToken:              other.Token,
				Keeper:                &keeper,
			})
			total += other.Token.Size
		}
	}

	if len(items) == 0 {
		return store.Plan{}, nil, errs.New(errs.CodeNoCandidates,
			"no group had both identical bytes and compatible metadata")
	}

	doc := plan.PlanDoc{
		PlanID:       string(planID),
		Kind:         domain.PlanManualDuplicate,
		CreatedAtNS:  nowNS,
		ExpiresAtNS:  expiresNS,
		Bindings:     []plan.PlanBinding{{ScopeID: string(sc.Scope.ID), Generation: sc.Scope.Generation, PersistentIdentityDigest: persistentDigest}},
		PolicyDigest: string(policyDigest()),
		ConfigDigest: string(configDigest),
		Items:        docs,
	}
	digest, err := doc.ComputeDigest()
	if err != nil {
		return store.Plan{}, nil, err
	}

	header := domain.PlanHeader{
		ID:           planID,
		Kind:         domain.PlanManualDuplicate,
		CreatedAtNS:  nowNS,
		ExpiresAtNS:  expiresNS,
		Bindings:     []domain.ScopeBinding{{Scope: sc.Scope.ID, Generation: sc.Scope.Generation, Root: rootID}},
		PolicyDigest: policyDigest(),
		ConfigDigest: configDigest,
		Digest:       digest,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return store.Plan{}, nil, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	stored := store.Plan{
		ID:          planID,
		Kind:        domain.PlanManualDuplicate,
		HeaderJSON:  string(headerJSON),
		Digest:      digest,
		Status:      domain.PlanReady,
		CreatedAtNS: nowNS,
		ExpiresAtNS: expiresNS,
	}
	if err := d.Store.InsertPlan(ctx, stored, items); err != nil {
		return store.Plan{}, nil, err
	}
	_ = total
	return stored, items, nil
}

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{}`
	}
	return string(b)
}

func ptr(s string) *string { return &s }
