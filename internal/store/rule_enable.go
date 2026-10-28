package store

import (
	"context"
	"database/sql"
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// EnableRuleRequest is one atomic enable: the exact rule revision, the exact
// scope state that was on screen, and the exact set of capabilities selected.
//
// Every expected field comes from a row the caller re-read immediately before
// the call, and the root identity is the byte-for-byte JSON of that row rather
// than a re-derived digest. "The scope is the same directory" is therefore
// decided by comparison, not by recomputation.
type EnableRuleRequest struct {
	RuleID                     domain.RuleID
	Revision                   int64
	ExpectedRuleDigest         domain.Digest
	ExpectedScopeID            domain.ScopeID
	ExpectedScopeGeneration    int64
	ExpectedProtectionRevision int64
	ExpectedRootIdentityJSON   string
	ReviewBindingJSON          string
	// Grants is the complete selected set. Capabilities that are not listed are
	// revoked: switching back to quarantine must take the purge authority away,
	// not leave it in place.
	Grants []Grant
}

// EnableRule activates one revision and installs the selected capabilities in a
// single transaction.
//
// The order inside the transaction is the contract:
//
//  1. every expected value is checked against the stored rows, and a mismatch
//     refuses the whole call - no paused or held scope is released, no scope
//     classification is changed;
//  2. the review binding is written and the revision is activated, which freezes
//     the other revisions of the same rule;
//  3. capabilities that were not selected are revoked;
//  4. a selected capability that already exists as an active grant with the same
//     binding keeps its id (a retried enable is idempotent), while a revoked or
//     needs_reapproval grant is never revived - a new id is created instead;
//  5. any failure rolls the whole thing back.
//
// Nothing here touches items: enabling purge authority never backfills the
// objects that were already isolated, because that would widen an old approval.
func (s *Store) EnableRule(ctx context.Context, req EnableRuleRequest) ([]Grant, error) {
	if req.RuleID == "" || req.Revision <= 0 {
		return nil, errs.New(errs.CodeInvalidArgument,
			"enable needs a rule id and a revision")
	}
	if err := validateGrants(req); err != nil {
		return nil, err
	}

	var out []Grant
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := checkEnableScope(ctx, tx, req); err != nil {
			return err
		}
		if req.ReviewBindingJSON != "" {
			if err := setRuleReviewBindingTx(ctx, tx, req.RuleID, req.Revision, req.ReviewBindingJSON); err != nil {
				return err
			}
		}
		if err := activateRuleTx(ctx, tx, req.RuleID, req.Revision); err != nil {
			return err
		}
		// Old grants of the previous revision were frozen by activation; the ones
		// of this revision that are no longer selected go now.
		if err := revokeUnselectedTx(ctx, tx, req); err != nil {
			return err
		}

		installed := make([]Grant, 0, len(req.Grants))
		for _, g := range req.Grants {
			kept, err := keepOrInsertGrantTx(ctx, tx, g)
			if err != nil {
				return err
			}
			installed = append(installed, kept)
		}
		out = installed
		return nil
	})
	if err != nil {
		// A failed transaction must not leave the caller believing part of it
		// happened.
		return nil, err
	}
	return out, nil
}

// validateGrants refuses a selection that could mean two things at once.
func validateGrants(req EnableRuleRequest) error {
	seen := make(map[string]bool, len(req.Grants))
	for _, g := range req.Grants {
		switch g.Capability {
		case domain.CapAutoQuarantine, domain.CapAutoPurge:
		default:
			return errs.Newf(errs.CodeInvalidArgument,
				"capability %q is not one of %s, %s",
				g.Capability, domain.CapAutoQuarantine, domain.CapAutoPurge)
		}
		if seen[g.Capability] {
			return errs.Newf(errs.CodeInvalidArgument, "capability %q was selected twice", g.Capability)
		}
		seen[g.Capability] = true
		if g.RuleID != req.RuleID || g.RuleRevision != req.Revision {
			return errs.Newf(errs.CodeInvalidArgument,
				"the %s grant is for rule %s revision %d, not %s revision %d",
				g.Capability, g.RuleID, g.RuleRevision, req.RuleID, req.Revision)
		}
		if g.ScopeID != req.ExpectedScopeID {
			return errs.Newf(errs.CodeInvalidArgument,
				"the %s grant is for scope %s, not %s", g.Capability, g.ScopeID, req.ExpectedScopeID)
		}
		if g.Digest == "" {
			return errs.Newf(errs.CodeInvalidArgument,
				"the %s grant carries no binding digest", g.Capability)
		}
	}
	return nil
}

// checkEnableScope compares every expected value with the stored rows.
func checkEnableScope(ctx context.Context, tx *sql.Tx, req EnableRuleRequest) error {
	var scopeID, digest string
	err := tx.QueryRowContext(ctx,
		"SELECT scope_id, digest FROM rules WHERE rule_id=? AND revision=?",
		string(req.RuleID), req.Revision).Scan(&scopeID, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.Newf(errs.CodeRuleNotFound, "rule %s revision %d is not stored", req.RuleID, req.Revision)
	}
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if domain.Digest(digest) != req.ExpectedRuleDigest {
		return errs.Newf(errs.CodePlanStale,
			"rule %s revision %d changed after it was shown; nothing was enabled", req.RuleID, req.Revision)
	}
	if domain.ScopeID(scopeID) != req.ExpectedScopeID {
		return errs.Newf(errs.CodePlanStale,
			"rule %s revision %d belongs to scope %s, not %s", req.RuleID, req.Revision, scopeID, req.ExpectedScopeID)
	}

	var generation, protectionRevision int64
	var rootIdentity string
	err = tx.QueryRowContext(ctx,
		"SELECT generation, protection_revision, root_identity_json FROM scopes WHERE scope_id=?",
		string(req.ExpectedScopeID)).Scan(&generation, &protectionRevision, &rootIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.Newf(errs.CodeScopeNotFound, "scope %s is not registered", req.ExpectedScopeID)
	}
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	switch {
	case generation != req.ExpectedScopeGeneration:
		return errs.Newf(errs.CodePlanStale,
			"scope %s was re-registered after it was shown; nothing was enabled", req.ExpectedScopeID)
	case protectionRevision != req.ExpectedProtectionRevision:
		return errs.Newf(errs.CodePlanStale,
			"the protections of scope %s changed after they were shown; nothing was enabled", req.ExpectedScopeID)
	case rootIdentity != req.ExpectedRootIdentityJSON:
		return errs.Newf(errs.CodePlanStale,
			"the root of scope %s is no longer the directory that was approved; nothing was enabled", req.ExpectedScopeID)
	}
	return nil
}

// revokeUnselectedTx takes authority away from capabilities the user did not
// choose this time.
func revokeUnselectedTx(ctx context.Context, tx *sql.Tx, req EnableRuleRequest) error {
	keep := make(map[string]bool, len(req.Grants))
	for _, g := range req.Grants {
		keep[g.Capability] = true
	}
	for _, capability := range []string{domain.CapAutoQuarantine, domain.CapAutoPurge} {
		if keep[capability] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE grants SET status='revoked' WHERE rule_id=? AND rule_revision=? AND capability=? AND status='active'",
			string(req.RuleID), req.Revision, capability); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
	}
	// A grant can also be left over on an older revision of the same rule. The
	// activation step already froze those, which is why nothing else is done here.
	return nil
}

// keepOrInsertGrantTx installs one selected capability inside the enable
// transaction. It delegates to replaceGrantTx so that the enable path and the
// single-capability authorize path cannot disagree about what "one active grant"
// means.
func keepOrInsertGrantTx(ctx context.Context, tx *sql.Tx, g Grant) (Grant, error) {
	return replaceGrantTx(ctx, tx, g)
}

// replaceGrantTx leaves exactly one active grant for one capability.
//
// The order matters and used to be wrong: the identical-approval case returned
// before the loop that retires the other active grants, so an earlier, larger
// approval stayed active next to the one the user had just confirmed. The
// replacement now happens first and the reuse happens afterwards, which makes
// "keep the id I already have" and "retire what I replaced" the same transaction
// rather than two branches one of which skipped the retirement.
//
// Only `active` counts. A revoked grant stays revoked even when the same
// capability is approved again with identical numbers: reviving the old id would
// make "which approval is this object running under" unanswerable, because the
// old id was recorded on objects that were stopped by the revocation.
func replaceGrantTx(ctx context.Context, tx *sql.Tx, g Grant) (Grant, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+grantColumns+
		" FROM grants WHERE rule_id=? AND rule_revision=? AND capability=? AND status='active'"+
		" ORDER BY approved_at_ns, grant_id",
		string(g.RuleID), g.RuleRevision, g.Capability)
	if err != nil {
		return Grant{}, errs.Wrap(errs.CodeDBError, err)
	}
	var actives []Grant
	for rows.Next() {
		existing, err := scanGrant(rows)
		if err != nil {
			rows.Close()
			return Grant{}, errs.Wrap(errs.CodeDBError, err)
		}
		actives = append(actives, existing)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Grant{}, errs.Wrap(errs.CodeDBError, err)
	}
	rows.Close()

	// The same approval, already active. Keep its identity and its original
	// approval time - a retry must not look like a fresh grant - and retire any
	// other active row for this capability, which is what an older build could
	// leave behind.
	for _, existing := range actives {
		if existing.Digest != g.Digest {
			continue
		}
		for _, other := range actives {
			if other.ID == existing.ID {
				continue
			}
			if err := revokeGrantTx(ctx, tx, other.ID); err != nil {
				return Grant{}, err
			}
		}
		return existing, nil
	}

	// A different approval replaces every active grant of this capability: it was
	// granted for a different ceiling or binding, and keeping it would leave the
	// authority the user did not just confirm in place.
	for _, existing := range actives {
		if err := revokeGrantTx(ctx, tx, existing.ID); err != nil {
			return Grant{}, err
		}
	}

	if g.ID == "" || grantIDTaken(ctx, tx, g.ID) {
		// A new capability always gets a new identity. An id that is already in
		// the table belongs to some earlier approval - possibly a revoked one -
		// and handing it back would re-label that history as this approval.
		g.ID = domain.GrantID(domain.NewID("grant"))
	}
	if g.Status == "" {
		g.Status = domain.GrantActive
	}
	if err := insertGrantTx(ctx, tx, g); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// revokeGrantTx retires one grant row.
func revokeGrantTx(ctx context.Context, tx *sql.Tx, id domain.GrantID) error {
	if _, err := tx.ExecContext(ctx,
		"UPDATE grants SET status='revoked' WHERE grant_id=?", string(id)); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return nil
}

// grantIDTaken reports whether a grant id is already in use, whatever its
// status. It is only consulted when the caller supplied an id of its own; the
// normal path has none and mints a fresh one.
func grantIDTaken(ctx context.Context, tx *sql.Tx, id domain.GrantID) bool {
	var one int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM grants WHERE grant_id=?", string(id)).Scan(&one)
	return err == nil
}
