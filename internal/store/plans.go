package store

import (
	"context"
	"database/sql"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Plan is the immutable head of a set of items.
type Plan struct {
	ID           domain.PlanID
	Kind         string
	SourcePlanID *domain.PlanID
	HeaderJSON   string
	Digest       domain.Digest
	Status       string
	CreatedAtNS  int64
	ExpiresAtNS  int64
}

// PurgeMember links a purge plan to an existing quarantine object. It never
// creates a second object for the same file.
type PurgeMember struct {
	Ordinal          int
	ItemID           domain.ItemID
	VerificationJSON string
}

// PurgeMemberRow is a purge member joined with its item.
type PurgeMemberRow struct {
	PurgeMember
	Item Item
}

// InsertPlan stores a plan and all of its items in one transaction. A plan is
// either fully visible or not visible at all.
func (s *Store) InsertPlan(ctx context.Context, p Plan, items []Item) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO plans(plan_id, kind, source_plan_id, header_json, digest, status, created_at_ns, expires_at_ns)
VALUES(?,?,?,?,?,?,?,?)`,
			string(p.ID), p.Kind, p.SourcePlanID, p.HeaderJSON, string(p.Digest), p.Status, p.CreatedAtNS, p.ExpiresAtNS)
		if err != nil {
			return errs.Newf(errs.CodeStateWriteFailed, "insert plan %s: %v", p.ID, err)
		}
		for _, it := range items {
			it.PlanID = p.ID
			if err := insertItemTx(ctx, tx, it); err != nil {
				return err
			}
		}
		return nil
	})
}

const planColumns = `plan_id, kind, source_plan_id, header_json, digest, status, created_at_ns, expires_at_ns`

func scanPlan(rows *sql.Rows) (Plan, error) {
	var p Plan
	var id, digest string
	var src sql.NullString
	err := rows.Scan(&id, &p.Kind, &src, &p.HeaderJSON, &digest, &p.Status, &p.CreatedAtNS, &p.ExpiresAtNS)
	p.ID = domain.PlanID(id)
	p.Digest = domain.Digest(digest)
	if src.Valid {
		v := domain.PlanID(src.String)
		p.SourcePlanID = &v
	}
	return p, err
}

// GetPlan loads one plan.
func (s *Store) GetPlan(ctx context.Context, id domain.PlanID) (Plan, error) {
	rows, err := s.read(ctx, "SELECT "+planColumns+" FROM plans WHERE plan_id=?", string(id))
	if err != nil {
		return Plan{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Plan{}, errs.Newf(errs.CodePlanNotFound, "plan %q does not exist", id)
	}
	return scanPlan(rows)
}

// SetPlanStatus records the outcome of an apply.
func (s *Store) SetPlanStatus(ctx context.Context, id domain.PlanID, status string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE plans SET status=? WHERE plan_id=?", status, string(id))
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// ListPlans returns plans in creation order, optionally filtered by status.
func (s *Store) ListPlans(ctx context.Context, status string, limit int) ([]Plan, error) {
	q := "SELECT " + planColumns + " FROM plans"
	args := []any{}
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	q += " ORDER BY created_at_ns DESC LIMIT ?"
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)

	rows, err := s.read(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, p)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// ExpirePlans marks ready plans past their window as expired. An expired plan is
// never extended; the user plans again.
func (s *Store) ExpirePlans(ctx context.Context, nowNS int64) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE plans SET status='expired' WHERE status='ready' AND expires_at_ns < ?`, nowNS)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// InsertPurgeMembers attaches existing objects to a purge plan.
func (s *Store) InsertPurgeMembers(ctx context.Context, planID domain.PlanID, members []PurgeMember) error {
	if len(members) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, m := range members {
			_, err := tx.ExecContext(ctx, `
INSERT INTO purge_members(plan_id, ordinal, item_id, verification_json) VALUES(?,?,?,?)`,
				string(planID), m.Ordinal, string(m.ItemID), m.VerificationJSON)
			if err != nil {
				return errs.Newf(errs.CodeStateWriteFailed, "insert purge member: %v", err)
			}
		}
		return nil
	})
}

// ListPurgeMembers returns a purge plan's members joined with their items.
//
// Members are read first and closed, then each item is loaded: the pool has one
// connection, so a nested query during iteration would deadlock.
func (s *Store) ListPurgeMembers(ctx context.Context, planID domain.PlanID) ([]PurgeMemberRow, error) {
	rows, err := s.read(ctx, `
SELECT ordinal, item_id, verification_json
FROM purge_members WHERE plan_id=? ORDER BY ordinal`, string(planID))
	if err != nil {
		return nil, err
	}
	var members []PurgeMemberRow
	for rows.Next() {
		var r PurgeMemberRow
		var itemID string
		if err := rows.Scan(&r.Ordinal, &itemID, &r.VerificationJSON); err != nil {
			rows.Close()
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		r.ItemID = domain.ItemID(itemID)
		members = append(members, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	rows.Close()

	for i := range members {
		it, err := s.GetItem(ctx, members[i].ItemID)
		if err != nil {
			return nil, err
		}
		members[i].Item = it
	}
	return members, nil
}
