package plan

import (
	"context"
	"encoding/json"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/store"
)

// CreatePurgePlan builds an independent plan over objects that are already
// quarantined.
//
// A purge is never implied by a quarantine plan: it gets its own identifier, its
// own digest, its own expiry and its own approval. The frozen evidence recorded
// here is what the executor re-checks before unlinking anything.
func CreatePurgePlan(ctx context.Context, st *store.Store, sc store.Scope, items []store.Item,
	kind string, nowNS int64, ttl int64) (store.Plan, error) {

	if len(items) == 0 {
		return store.Plan{}, errs.New(errs.CodeNoCandidates, "no object is due for purging")
	}
	if kind != domain.PlanManualPurge && kind != domain.PlanScheduledPurge {
		return store.Plan{}, errs.Newf(errs.CodeInvalidArgument, "%q is not a purge plan kind", kind)
	}

	persistentDigest, rootID, err := scopeIdentity(sc)
	if err != nil {
		return store.Plan{}, err
	}
	configDigest, err := ConfigDigestOf(ConfigDigestInput{MetadataPolicyVersion: MetadataPolicyVersion})
	if err != nil {
		return store.Plan{}, err
	}

	planID := domain.PlanID(domain.NewID("plan"))
	expires := nowNS + ttl

	docs := make([]PlanItemDoc, 0, len(items))
	members := make([]store.PurgeMember, 0, len(items))
	for i, it := range items {
		d, err := itemDocOf(it)
		if err != nil {
			return store.Plan{}, err
		}
		// The item keeps the ordinal it was created with. Renumbering here would
		// make the digest impossible to rebuild from stored rows, and a plan
		// whose digest cannot be rebuilt cannot be verified before deletion.
		docs = append(docs, d)

		verification := struct {
			ContentHash  string  `json:"content_hash"`
			MetadataHash *string `json:"metadata_hash"`
			Identity     *string `json:"captured_identity"`
			Token        *string `json:"captured_token"`
			PurgeAfter   *int64  `json:"purge_not_before_ns"`
			CheckedAtNS  int64   `json:"checked_at_ns"`
		}{
			MetadataHash: it.MetadataHash,
			Identity:     it.CapturedIdentityJSON,
			Token:        it.CapturedTokenJSON,
			PurgeAfter:   it.PurgeNotBeforeNS,
			CheckedAtNS:  nowNS,
		}
		if it.ContentHash != nil {
			verification.ContentHash = *it.ContentHash
		}
		vb, err := json.Marshal(verification)
		if err != nil {
			return store.Plan{}, errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		members = append(members, store.PurgeMember{
			Ordinal:          i,
			ItemID:           it.ID,
			VerificationJSON: string(vb),
		})
	}

	doc := PlanDoc{
		PlanID:       string(planID),
		Kind:         kind,
		CreatedAtNS:  nowNS,
		ExpiresAtNS:  expires,
		Bindings:     []PlanBinding{{ScopeID: string(sc.ID), Generation: sc.Generation, PersistentIdentityDigest: persistentDigest}},
		PolicyDigest: string(policy.PolicyDigest()),
		ConfigDigest: string(configDigest),
		Items:        docs,
	}
	digest, err := doc.ComputeDigest()
	if err != nil {
		return store.Plan{}, err
	}

	header := domain.PlanHeader{
		ID:           planID,
		Kind:         kind,
		CreatedAtNS:  nowNS,
		ExpiresAtNS:  expires,
		Bindings:     []domain.ScopeBinding{{Scope: sc.ID, Generation: sc.Generation, Root: rootID}},
		PolicyDigest: policy.PolicyDigest(),
		ConfigDigest: configDigest,
		Digest:       digest,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return store.Plan{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	stored := store.Plan{
		ID:          planID,
		Kind:        kind,
		HeaderJSON:  string(headerJSON),
		Digest:      digest,
		Status:      domain.PlanReady,
		CreatedAtNS: nowNS,
		ExpiresAtNS: expires,
	}
	if err := st.InsertPlan(ctx, stored, nil); err != nil {
		return store.Plan{}, err
	}
	if err := st.InsertPurgeMembers(ctx, planID, members); err != nil {
		return store.Plan{}, err
	}
	return stored, nil
}
