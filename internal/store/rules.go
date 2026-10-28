package store

import (
	"context"
	"database/sql"
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Rule is one immutable revision of a rule.
type Rule struct {
	ID                domain.RuleID
	Revision          int64
	ScopeID           domain.ScopeID
	DSLJSON           string
	Digest            domain.Digest
	ReviewBindingJSON string
	Status            string
	CreatedAtNS       int64
}

// InsertRule stores a new revision. Revisions are never edited in place.
func (s *Store) InsertRule(ctx context.Context, r Rule) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO rules(rule_id, revision, scope_id, dsl_json, digest, review_binding_json, status, created_at_ns)
VALUES(?,?,?,?,?,?,?,?)`,
			string(r.ID), r.Revision, string(r.ScopeID), r.DSLJSON, string(r.Digest),
			r.ReviewBindingJSON, r.Status, r.CreatedAtNS)
		if err != nil {
			return errs.Newf(errs.CodeStateWriteFailed, "insert rule %s rev %d: %v", r.ID, r.Revision, err)
		}
		return nil
	})
}

const ruleColumns = `rule_id, revision, scope_id, dsl_json, digest, review_binding_json, status, created_at_ns`

func scanRule(rows *sql.Rows) (Rule, error) {
	var r Rule
	var id, scope, digest string
	err := rows.Scan(&id, &r.Revision, &scope, &r.DSLJSON, &digest, &r.ReviewBindingJSON, &r.Status, &r.CreatedAtNS)
	r.ID = domain.RuleID(id)
	r.ScopeID = domain.ScopeID(scope)
	r.Digest = domain.Digest(digest)
	return r, err
}

// GetRule loads one revision.
func (s *Store) GetRule(ctx context.Context, id domain.RuleID, rev int64) (Rule, error) {
	rows, err := s.read(ctx, "SELECT "+ruleColumns+" FROM rules WHERE rule_id=? AND revision=?", string(id), rev)
	if err != nil {
		return Rule{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Rule{}, errs.Newf(errs.CodeRuleNotFound, "rule %s revision %d is not stored", id, rev)
	}
	return scanRule(rows)
}

// ActiveRule returns the currently active revision of a rule.
func (s *Store) ActiveRule(ctx context.Context, id domain.RuleID) (Rule, error) {
	rows, err := s.read(ctx, "SELECT "+ruleColumns+" FROM rules WHERE rule_id=? AND status='active'", string(id))
	if err != nil {
		return Rule{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Rule{}, errs.Newf(errs.CodeRuleNotFound, "rule %q has no active revision", id)
	}
	return scanRule(rows)
}

// ListRules returns all revisions for one scope, newest first.
func (s *Store) ListRules(ctx context.Context, scope domain.ScopeID) ([]Rule, error) {
	rows, err := s.read(ctx, "SELECT "+ruleColumns+" FROM rules WHERE scope_id=? ORDER BY rule_id, revision DESC", string(scope))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, r)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// ListActiveRules returns every active rule, ordered by rule ID.
func (s *Store) ListActiveRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.read(ctx, "SELECT "+ruleColumns+" FROM rules WHERE status='active' ORDER BY rule_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, r)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// NextRuleRevision returns the next revision number for a rule ID.
func (s *Store) NextRuleRevision(ctx context.Context, id domain.RuleID) (int64, error) {
	var rev sql.NullInt64
	if err := s.queryRow(ctx,
		"SELECT MAX(revision) FROM rules WHERE rule_id=?", string(id)).Scan(&rev); err != nil {
		return 0, errs.Wrap(errs.CodeDBError, err)
	}
	if !rev.Valid {
		return 1, nil
	}
	return rev.Int64 + 1, nil
}

// ActivateRule makes one revision active and disables every other revision of
// the same rule in the same transaction. The old revision's background grants
// are marked needs_reapproval in the same step, so a new revision never
// silently inherits the previous authority.
func (s *Store) ActivateRule(ctx context.Context, id domain.RuleID, rev int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return activateRuleTx(ctx, tx, id, rev)
	})
}

// activateRuleTx is the activation step without its own transaction, so the
// single-command activation and the atomic enable share one implementation. Two
// copies of "make this revision active and freeze the others" would drift, and
// the one that drifted would be the one nobody reviewed.
func activateRuleTx(ctx context.Context, tx *sql.Tx, id domain.RuleID, rev int64) error {
	var status string
	err := tx.QueryRowContext(ctx,
		"SELECT status FROM rules WHERE rule_id=? AND revision=?", string(id), rev).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.Newf(errs.CodeRuleNotFound, "rule %s revision %d is not stored", id, rev)
	}
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if status == domain.RuleActive {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE rules SET status='disabled' WHERE rule_id=? AND status='active'", string(id)); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE grants SET status='needs_reapproval' WHERE rule_id=? AND status='active'", string(id)); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE rules SET status='active' WHERE rule_id=? AND revision=?", string(id), rev); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return nil
}

// DisableRule turns off a rule and freezes its grants.
func (s *Store) DisableRule(ctx context.Context, id domain.RuleID, rev int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE rules SET status='disabled' WHERE rule_id=? AND revision=?", string(id), rev); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE grants SET status='revoked' WHERE rule_id=? AND rule_revision=? AND status='active'",
			string(id), rev); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return nil
	})
}

// SetRuleReviewBinding replaces the stored review binding of one revision.
//
// The binding records the scope state that was on screen when the user
// approved. It is refreshed only by an explicit activation that presented the
// matching digest, never by a background pass.
func (s *Store) SetRuleReviewBinding(ctx context.Context, id domain.RuleID, rev int64, bindingJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return setRuleReviewBindingTx(ctx, tx, id, rev, bindingJSON)
	})
}

func setRuleReviewBindingTx(ctx context.Context, tx *sql.Tx, id domain.RuleID, rev int64, bindingJSON string) error {
	res, err := tx.ExecContext(ctx,
		"UPDATE rules SET review_binding_json=? WHERE rule_id=? AND revision=?",
		bindingJSON, string(id), rev)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.Newf(errs.CodeRuleNotFound, "rule %s revision %d is not stored", id, rev)
	}
	return nil
}

// ActivateRuleWithReviewBinding records what the user approved and makes the
// revision active in one transaction.
//
// Activating first and recording afterwards leaves a window where the active
// revision has no approval behind it; recording first and activating afterwards
// leaves the opposite. Neither half is a state anything else should observe, so
// this command does not produce it. Activation still grants no capability: only
// EnableRule (or an explicit authorize) does.
func (s *Store) ActivateRuleWithReviewBinding(ctx context.Context, id domain.RuleID, rev int64, bindingJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if bindingJSON != "" {
			if err := setRuleReviewBindingTx(ctx, tx, id, rev, bindingJSON); err != nil {
				return err
			}
		}
		return activateRuleTx(ctx, tx, id, rev)
	})
}

// Grant is one independently revocable capability bound to a rule revision.
type Grant struct {
	ID           domain.GrantID
	ScopeID      domain.ScopeID
	RuleID       domain.RuleID
	RuleRevision int64
	Capability   string
	BindingJSON  string
	Digest       domain.Digest
	Status       string
	ApprovedAtNS int64
}

// InsertGrant stores a new grant and retires the ones it replaces.
//
// "Stores a grant" used to mean exactly that: an insert. Two commands could then
// leave two active grants for the same capability - one approval for 100 files
// and a later one for 10 - and "what did the user last approve" had two answers.
// The write now goes through the same replacement helper the enable transaction
// uses (see replaceGrantTx), so one capability has at most one active grant.
func (s *Store) InsertGrant(ctx context.Context, g Grant) error {
	_, err := s.ReplaceGrant(ctx, g)
	return err
}

// ReplaceGrant installs one capability and returns the grant that is active
// afterwards, which is not necessarily the one passed in: an identical approval
// that is already active keeps its identity and its original approval time.
func (s *Store) ReplaceGrant(ctx context.Context, g Grant) (Grant, error) {
	var out Grant
	err := s.write(ctx, func(tx *sql.Tx) error {
		kept, err := replaceGrantTx(ctx, tx, g)
		if err != nil {
			return err
		}
		out = kept
		return nil
	})
	if err != nil {
		return Grant{}, err
	}
	return out, nil
}

func insertGrantTx(ctx context.Context, tx *sql.Tx, g Grant) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO grants(grant_id, scope_id, rule_id, rule_revision, capability,
                   binding_json, digest, status, approved_at_ns)
VALUES(?,?,?,?,?,?,?,?,?)`,
		string(g.ID), string(g.ScopeID), string(g.RuleID), g.RuleRevision,
		g.Capability, g.BindingJSON, string(g.Digest), g.Status, g.ApprovedAtNS)
	if err != nil {
		return errs.Newf(errs.CodeStateWriteFailed, "insert grant: %v", err)
	}
	return nil
}

const grantColumns = `grant_id, scope_id, rule_id, rule_revision, capability,
	binding_json, digest, status, approved_at_ns`

func scanGrant(rows *sql.Rows) (Grant, error) {
	var g Grant
	var id, scope, rule, digest string
	err := rows.Scan(&id, &scope, &rule, &g.RuleRevision, &g.Capability,
		&g.BindingJSON, &digest, &g.Status, &g.ApprovedAtNS)
	g.ID = domain.GrantID(id)
	g.ScopeID = domain.ScopeID(scope)
	g.RuleID = domain.RuleID(rule)
	g.Digest = domain.Digest(digest)
	return g, err
}

// GetGrant loads one grant.
func (s *Store) GetGrant(ctx context.Context, id domain.GrantID) (Grant, error) {
	rows, err := s.read(ctx, "SELECT "+grantColumns+" FROM grants WHERE grant_id=?", string(id))
	if err != nil {
		return Grant{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Grant{}, errs.Newf(errs.CodeGrantRequired, "grant %q does not exist", id)
	}
	return scanGrant(rows)
}

// ActiveGrant returns the active grant for one capability, or a GRANT_REQUIRED
// error. Callers must treat absence as "no authority", never as "default".
//
// More than one active grant for the same capability is not a tie to break: it
// means two approvals exist for one action, and whichever row a SELECT happened
// to return first would become the authority the object runs under. The rows are
// therefore read in full and a second one is refused rather than picked.
func (s *Store) ActiveGrant(ctx context.Context, scope domain.ScopeID, rule domain.RuleID, rev int64, capability string) (Grant, error) {
	rows, err := s.read(ctx, "SELECT "+grantColumns+` FROM grants
WHERE scope_id=? AND rule_id=? AND rule_revision=? AND capability=? AND status='active'
ORDER BY approved_at_ns, grant_id`,
		string(scope), string(rule), rev, capability)
	if err != nil {
		return Grant{}, err
	}
	defer rows.Close()
	var found []Grant
	for rows.Next() {
		g, serr := scanGrant(rows)
		if serr != nil {
			return Grant{}, errs.Wrap(errs.CodeDBError, serr)
		}
		found = append(found, g)
	}
	if err := rows.Err(); err != nil {
		return Grant{}, errs.Wrap(errs.CodeDBError, err)
	}
	switch len(found) {
	case 0:
		return Grant{}, errs.Newf(errs.CodeGrantRequired,
			"no active %s grant for rule %s revision %d", capability, rule, rev)
	case 1:
		return found[0], nil
	default:
		ids := make([]string, 0, len(found))
		for _, g := range found {
			ids = append(ids, string(g.ID))
		}
		return Grant{}, errs.Newf(errs.CodeGrantRequired,
			"%d active %s grants exist for rule %s revision %d (%v); the authority is ambiguous, so no action is taken - re-enable the rule to keep exactly the approval that was last confirmed",
			len(found), capability, rule, rev, ids)
	}
}

// SetGrantStatus changes a grant's state.
func (s *Store) SetGrantStatus(ctx context.Context, id domain.GrantID, status string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE grants SET status=? WHERE grant_id=?", status, string(id))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errs.Newf(errs.CodeGrantRequired, "grant %q does not exist", id)
		}
		return nil
	})
}

// RevokeGrants freezes every active grant of a rule revision or capability and
// marks the affected scopes needs_reapproval. It returns the number revoked; a
// zero count is reported to the user rather than being treated as success.
func (s *Store) RevokeGrants(ctx context.Context, rule domain.RuleID, rev int64, capability string) (int, error) {
	n := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		q := "UPDATE grants SET status='revoked' WHERE rule_id=? AND rule_revision=? AND status='active'"
		args := []any{string(rule), rev}
		if capability != "" {
			q += " AND capability=?"
			args = append(args, capability)
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		affected, _ := res.RowsAffected()
		n = int(affected)
		// Freeze the scopes that hosted those grants.
		rows, err := tx.QueryContext(ctx,
			"SELECT DISTINCT scope_id FROM grants WHERE rule_id=? AND rule_revision=?", string(rule), rev)
		if err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		var scopes []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return errs.Wrap(errs.CodeDBError, err)
			}
			scopes = append(scopes, id)
		}
		rows.Close()
		for _, id := range scopes {
			if _, err := tx.ExecContext(ctx,
				"UPDATE scopes SET status='needs_reapproval' WHERE scope_id=? AND status='active'", id); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
	return n, err
}

// ListGrants returns all grants for a scope.
func (s *Store) ListGrants(ctx context.Context, scope domain.ScopeID) ([]Grant, error) {
	rows, err := s.read(ctx, "SELECT "+grantColumns+" FROM grants WHERE scope_id=? ORDER BY approved_at_ns", string(scope))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, g)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}
