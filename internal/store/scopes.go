package store

import (
	"context"
	"database/sql"
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Scope is a registered directory plus its identity and authority state.
type Scope struct {
	ID                 domain.ScopeID
	RawRoot            []byte
	RootIdentityJSON   string
	VaultIdentityJSON  *string
	InstanceID         string
	Kind               string
	Generation         int64
	ProtectionRevision int64
	Status             string
	ScanEpoch          int64
	CreatedAtNS        int64
}

// InsertScope registers a new scope.
func (s *Store) InsertScope(ctx context.Context, sc Scope) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO scopes(scope_id, raw_root, root_identity_json, vault_identity_json,
                   instance_id, kind, generation, protection_revision, status,
                   scan_epoch, created_at_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			string(sc.ID), sc.RawRoot, sc.RootIdentityJSON, sc.VaultIdentityJSON,
			sc.InstanceID, sc.Kind, sc.Generation, sc.ProtectionRevision,
			sc.Status, sc.ScanEpoch, sc.CreatedAtNS)
		if err != nil {
			return errs.Newf(errs.CodeStateWriteFailed, "insert scope %s: %v", sc.ID, err)
		}
		return nil
	})
}

const scopeColumns = `scope_id, raw_root, root_identity_json, vault_identity_json,
	instance_id, kind, generation, protection_revision, status, scan_epoch, created_at_ns`

func scanScope(sc *sql.Rows) (Scope, error) {
	var out Scope
	var id string
	err := sc.Scan(&id, &out.RawRoot, &out.RootIdentityJSON, &out.VaultIdentityJSON,
		&out.InstanceID, &out.Kind, &out.Generation, &out.ProtectionRevision,
		&out.Status, &out.ScanEpoch, &out.CreatedAtNS)
	out.ID = domain.ScopeID(id)
	return out, err
}

// GetScope loads one scope.
func (s *Store) GetScope(ctx context.Context, id domain.ScopeID) (Scope, error) {
	rows, err := s.read(ctx, "SELECT "+scopeColumns+" FROM scopes WHERE scope_id=?", string(id))
	if err != nil {
		return Scope{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Scope{}, errs.Wrap(errs.CodeDBError, err)
		}
		return Scope{}, errs.Newf(errs.CodeScopeNotFound, "scope %q is not registered", id)
	}
	sc, err := scanScope(rows)
	if err != nil {
		return Scope{}, errs.Wrap(errs.CodeDBError, err)
	}
	return sc, nil
}

// ListScopes returns every registered scope ordered by ID, which is also the
// lock order used for multi-scope operations.
func (s *Store) ListScopes(ctx context.Context) ([]Scope, error) {
	rows, err := s.read(ctx, "SELECT "+scopeColumns+" FROM scopes ORDER BY scope_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Scope
	for rows.Next() {
		sc, err := scanScope(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, sc)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// ListScopesByKind returns scopes of one kind, ordered by ID.
func (s *Store) ListScopesByKind(ctx context.Context, kind string) ([]Scope, error) {
	all, err := s.ListScopes(ctx)
	if err != nil {
		return nil, err
	}
	var out []Scope
	for _, sc := range all {
		if sc.Kind == kind {
			out = append(out, sc)
		}
	}
	return out, nil
}

// releaseScopeHoldTx takes a scope out of "held" because the item that was just
// settled in the same transaction was the reason for that hold.
//
// It is deliberately three conditions, not one unconditional write:
//
//   - only a scope that is actually held is touched. A restore in an active scope
//     changes nothing about the scope;
//   - only when no other item in the scope is unresolved. Repairing one object
//     must not hand the scope back while another is still broken;
//   - and it lands on needs_reapproval, never on active. The hold was a verdict
//     about this scope; clearing it needs a human, and `scope resume` is that
//     human. Jumping straight to active would make a repair look like an approval.
func releaseScopeHoldTx(ctx context.Context, tx *sql.Tx, scope domain.ScopeID) error {
	var unresolved int64
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM items
WHERE scope_id=? AND (pending_op_id IS NOT NULL
  OR state IN ('held','unknown','q_intent','r_intent','p_intent'))`,
		string(scope)).Scan(&unresolved); err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if unresolved > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE scopes SET status=? WHERE scope_id=? AND status=?",
		domain.ScopeNeedsReapproval, string(scope), domain.ScopeHeld); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return nil
}

// SetScopeStatus changes the run state of a scope.
func (s *Store) SetScopeStatus(ctx context.Context, id domain.ScopeID, status string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE scopes SET status=? WHERE scope_id=?", status, string(id))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errs.Newf(errs.CodeScopeNotFound, "scope %q is not registered", id)
		}
		return nil
	})
}

// BumpProtectionRevision records that the protection set changed. Every plan
// bound to the old revision becomes stale, which is how an added protection
// immediately narrows what may run.
func (s *Store) BumpProtectionRevision(ctx context.Context, id domain.ScopeID) (int64, error) {
	var rev int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE scopes SET protection_revision=protection_revision+1 WHERE scope_id=?", string(id)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return tx.QueryRowContext(ctx,
			"SELECT protection_revision FROM scopes WHERE scope_id=?", string(id)).Scan(&rev)
	})
	return rev, err
}

// BumpGeneration marks a new identity generation after a re-registration.
func (s *Store) BumpGeneration(ctx context.Context, id domain.ScopeID) (int64, error) {
	var gen int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE scopes SET generation=generation+1 WHERE scope_id=?", string(id)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return tx.QueryRowContext(ctx,
			"SELECT generation FROM scopes WHERE scope_id=?", string(id)).Scan(&gen)
	})
	return gen, err
}

// UpdateScopeIdentity rewrites the stored root/vault identity and increments the
// generation.
func (s *Store) UpdateScopeIdentity(ctx context.Context, id domain.ScopeID, rootJSON string, vaultJSON *string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE scopes SET root_identity_json=?, vault_identity_json=?, generation=generation+1
WHERE scope_id=?`, rootJSON, vaultJSON, string(id))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return nil
	})
}

// SetScanEpoch stores the epoch a scan is working on.
func (s *Store) SetScanEpoch(ctx context.Context, id domain.ScopeID, epoch int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE scopes SET scan_epoch=? WHERE scope_id=?", epoch, string(id))
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// ScopeExists reports whether a scope ID is taken.
func (s *Store) ScopeExists(ctx context.Context, id domain.ScopeID) (bool, error) {
	var n int
	err := s.queryRow(ctx, "SELECT COUNT(*) FROM scopes WHERE scope_id=?", string(id)).Scan(&n)
	if err != nil {
		return false, errs.Wrap(errs.CodeDBError, err)
	}
	return n > 0, nil
}

// ErrNoRows marks a lookup miss for callers that prefer errors.Is.
var ErrNoRows = errors.New("store: no rows")
