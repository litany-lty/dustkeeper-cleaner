package store

import (
	"context"
	"database/sql"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Protection is a path this program must not quarantine or purge.
type Protection struct {
	ID           domain.ProtectionID
	ScopeID      domain.ScopeID
	RawRelative  []byte
	MatchKind    string
	Source       string
	EvidenceJSON string
	Active       bool
	CreatedAtNS  int64
}

// insertProtectionTx adds or reactivates one protection inside an existing
// transaction, so a restore can persist its protection together with its intent.
func insertProtectionTx(ctx context.Context, tx *sql.Tx, p Protection) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO protections(protection_id, scope_id, raw_relative, match_kind, source,
                        evidence_json, active, created_at_ns)
VALUES(?,?,?,?,?,?,1,?)
ON CONFLICT(scope_id, raw_relative, match_kind) DO UPDATE SET
  active=1, source=excluded.source, evidence_json=excluded.evidence_json`,
		string(p.ID), string(p.ScopeID), p.RawRelative, p.MatchKind, p.Source,
		p.EvidenceJSON, p.CreatedAtNS)
	if err != nil {
		return errs.Newf(errs.CodeStateWriteFailed, "insert protection: %v", err)
	}
	return nil
}

// InsertProtection adds or reactivates a protection and bumps the scope's
// protection revision in the same transaction, so every plan bound to the old
// revision becomes stale at once.
func (s *Store) InsertProtection(ctx context.Context, p Protection) (int64, error) {
	var rev int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := insertProtectionTx(ctx, tx, p); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE scopes SET protection_revision=protection_revision+1 WHERE scope_id=?",
			string(p.ScopeID)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return tx.QueryRowContext(ctx,
			"SELECT protection_revision FROM scopes WHERE scope_id=?", string(p.ScopeID)).Scan(&rev)
	})
	return rev, err
}

const protectionColumns = `protection_id, scope_id, raw_relative, match_kind, source,
	evidence_json, active, created_at_ns`

func scanProtection(rows *sql.Rows) (Protection, error) {
	var p Protection
	var id, scope string
	err := rows.Scan(&id, &scope, &p.RawRelative, &p.MatchKind, &p.Source,
		&p.EvidenceJSON, &p.Active, &p.CreatedAtNS)
	p.ID = domain.ProtectionID(id)
	p.ScopeID = domain.ScopeID(scope)
	return p, err
}

// ListProtections returns protections for a scope.
func (s *Store) ListProtections(ctx context.Context, scope domain.ScopeID, activeOnly bool) ([]Protection, error) {
	q := "SELECT " + protectionColumns + " FROM protections WHERE scope_id=?"
	if activeOnly {
		q += " AND active=1"
	}
	q += " ORDER BY raw_relative, match_kind"
	rows, err := s.read(ctx, q, string(scope))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Protection
	for rows.Next() {
		p, err := scanProtection(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, p)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// DeactivateProtection clears a protection. Removing it is always explicit and
// always recorded, never implied by a later run.
func (s *Store) DeactivateProtection(ctx context.Context, id domain.ProtectionID) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var scope string
		err := tx.QueryRowContext(ctx,
			"SELECT scope_id FROM protections WHERE protection_id=?", string(id)).Scan(&scope)
		if err != nil {
			if err == sql.ErrNoRows {
				return errs.Newf(errs.CodeInvalidArgument, "protection %q does not exist", id)
			}
			return errs.Wrap(errs.CodeDBError, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE protections SET active=0 WHERE protection_id=?", string(id)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE scopes SET protection_revision=protection_revision+1 WHERE scope_id=?", scope)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// DeactivateProtectionByPath clears a protection by scope and relative path.
func (s *Store) DeactivateProtectionByPath(ctx context.Context, scope domain.ScopeID, rel []byte) (int, error) {
	n := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE protections SET active=0 WHERE scope_id=? AND raw_relative=? AND active=1",
			string(scope), rel)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		affected, _ := res.RowsAffected()
		n = int(affected)
		if n > 0 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE scopes SET protection_revision=protection_revision+1 WHERE scope_id=?",
				string(scope)); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
	return n, err
}
