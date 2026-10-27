package store

import (
	"context"
	"database/sql"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Observation is one indexed path.
type Observation struct {
	ScopeID       domain.ScopeID
	RawRelative   []byte
	IdentityJSON  string
	TokenJSON     string
	ParentJSON    string
	SizeBytes     int64
	MTimeNS       int64
	FirstSeenNS   int64
	LastSeenNS    int64
	ScanEpoch     int64
	Status        string
	CachedHash    *string
	HashTokenJSON *string
}

// UpsertObservations writes a metadata batch in one transaction. A re-seen path
// keeps its original first_seen, so "first seen" keeps its meaning across runs.
func (s *Store) UpsertObservations(ctx context.Context, batch []Observation) error {
	if len(batch) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO observations(scope_id, raw_relative, identity_json, token_json, parent_identity_json,
                         size_bytes, mtime_ns, first_seen_ns, last_seen_ns, scan_epoch, status)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(scope_id, raw_relative) DO UPDATE SET
  identity_json=excluded.identity_json,
  token_json=excluded.token_json,
  parent_identity_json=excluded.parent_identity_json,
  size_bytes=excluded.size_bytes,
  mtime_ns=excluded.mtime_ns,
  last_seen_ns=excluded.last_seen_ns,
  scan_epoch=excluded.scan_epoch,
  status=excluded.status`)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		defer stmt.Close()
		for _, o := range batch {
			if _, err := stmt.ExecContext(ctx, string(o.ScopeID), o.RawRelative, o.IdentityJSON,
				o.TokenJSON, o.ParentJSON, o.SizeBytes, o.MTimeNS, o.FirstSeenNS, o.LastSeenNS,
				o.ScanEpoch, o.Status); err != nil {
				return errs.Newf(errs.CodeStateWriteFailed, "upsert observation %q: %v", string(o.RawRelative), err)
			}
		}
		return nil
	})
}

const obsColumns = `scope_id, raw_relative, identity_json, token_json, parent_identity_json,
	size_bytes, mtime_ns, first_seen_ns, last_seen_ns, scan_epoch, status, cached_hash, hash_token_json`

func scanObs(rows *sql.Rows) (Observation, error) {
	var o Observation
	var scope string
	err := rows.Scan(&scope, &o.RawRelative, &o.IdentityJSON, &o.TokenJSON, &o.ParentJSON,
		&o.SizeBytes, &o.MTimeNS, &o.FirstSeenNS, &o.LastSeenNS, &o.ScanEpoch,
		&o.Status, &o.CachedHash, &o.HashTokenJSON)
	o.ScopeID = domain.ScopeID(scope)
	return o, err
}

// IterateObservations streams every live observation of a scope.
//
// fn must be pure: the pool has a single connection, so issuing another query
// from inside fn would deadlock. Collect decisions and write them afterwards.
func (s *Store) IterateObservations(ctx context.Context, scope domain.ScopeID, fn func(Observation) error) error {
	rows, err := s.read(ctx, "SELECT "+obsColumns+
		" FROM observations WHERE scope_id=? AND status!='stale' ORDER BY raw_relative", string(scope))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanObs(rows)
		if err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return errs.Wrap(errs.CodeDBError, rows.Err())
}

// GetObservation loads one indexed path.
func (s *Store) GetObservation(ctx context.Context, scope domain.ScopeID, rel []byte) (Observation, error) {
	rows, err := s.read(ctx, "SELECT "+obsColumns+" FROM observations WHERE scope_id=? AND raw_relative=?",
		string(scope), rel)
	if err != nil {
		return Observation{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Observation{}, errs.Newf(errs.CodeItemNotFound, "path %q is not indexed in %s", string(rel), scope)
	}
	return scanObs(rows)
}

// CountObservations returns the number of live indexed paths.
func (s *Store) CountObservations(ctx context.Context, scope domain.ScopeID) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM observations WHERE scope_id=? AND status!='stale'", string(scope)).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(errs.CodeDBError, err)
	}
	return n, nil
}

// DeleteObservation removes one indexed path.
func (s *Store) DeleteObservation(ctx context.Context, scope domain.ScopeID, rel []byte) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM observations WHERE scope_id=? AND raw_relative=?", string(scope), rel)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// MarkStaleObservations retires entries not seen in the completed epoch. This is
// only legal after a whole epoch finished: a partial scan cannot prove a file
// disappeared.
func (s *Store) MarkStaleObservations(ctx context.Context, scope domain.ScopeID, epoch int64) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE observations SET status='stale'
WHERE scope_id=? AND scan_epoch < ? AND status!='stale'`, string(scope), epoch)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// SetCachedHash stores a hash bound to the token that produced it, so a later
// run can tell whether the cache still describes the current bytes.
func (s *Store) SetCachedHash(ctx context.Context, scope domain.ScopeID, rel []byte, hash, tokenJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE observations SET cached_hash=?, hash_token_json=? WHERE scope_id=? AND raw_relative=?`,
			hash, tokenJSON, string(scope), rel)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// ScanQueueState reports the pending directory count for the current epoch.
//
// Pending counts every directory whose contents this epoch has not proven:
// pending, active (a run was interrupted while holding it) and failed (a run
// could not read it). Failed is included deliberately: an epoch that could not
// read a directory must not be able to call itself complete, because completing
// it retires every observation the epoch never saw.
type ScanQueueState struct {
	Epoch   int64
	Pending int64
	Done    int64
	Failed  int64
}

// ScanQueueStateOf reads progress for one epoch.
func (s *Store) ScanQueueStateOf(ctx context.Context, scope domain.ScopeID, epoch int64) (ScanQueueState, error) {
	st := ScanQueueState{Epoch: epoch}
	rows, err := s.read(ctx, `
SELECT status, COUNT(*) FROM scan_queue WHERE scope_id=? AND scan_epoch=? GROUP BY status`,
		string(scope), epoch)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return st, errs.Wrap(errs.CodeDBError, err)
		}
		switch status {
		case "pending", "active":
			st.Pending += n
		case "failed":
			st.Pending += n
			st.Failed = n
		case "done":
			st.Done = n
		}
	}
	return st, errs.Wrap(errs.CodeDBError, rows.Err())
}

// StartScanEpoch opens a new epoch and seeds it with the scope root directory.
// The previous epoch's leftovers are discarded: directory positions are not a
// durable cursor, so a fresh epoch re-enumerates rather than resuming offsets.
func (s *Store) StartScanEpoch(ctx context.Context, scope domain.ScopeID) (int64, error) {
	var epoch int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			"SELECT scan_epoch FROM scopes WHERE scope_id=?", string(scope)).Scan(&epoch); err != nil {
			if err == sql.ErrNoRows {
				return errs.Newf(errs.CodeScopeNotFound, "scope %q is not registered", scope)
			}
			return errs.Wrap(errs.CodeDBError, err)
		}
		epoch++
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM scan_queue WHERE scope_id=?", string(scope)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE scopes SET scan_epoch=? WHERE scope_id=?", epoch, string(scope)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO scan_queue(scope_id, scan_epoch, raw_relative_dir, status) VALUES(?,?,?,'pending')",
			string(scope), epoch, []byte(""))
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
	return epoch, err
}

// EnqueueDir adds a directory to the current epoch, ignoring duplicates.
func (s *Store) EnqueueDir(ctx context.Context, scope domain.ScopeID, epoch int64, rel []byte) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO scan_queue(scope_id, scan_epoch, raw_relative_dir, status)
VALUES(?,?,?,'pending') ON CONFLICT(scope_id, scan_epoch, raw_relative_dir) DO NOTHING`,
			string(scope), epoch, rel)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// ClaimDirs takes up to limit directories and marks them active. A crash leaves
// them active, which the next run requeues explicitly.
//
// Order is least-recently-claimed first, then path, with the rows that were never
// really opened (NULL sequence) ahead of everything. Path order alone let one very
// large directory consume the whole budget every round: it was claimed first,
// interrupted, requeued, and claimed first again, so everything behind it in path
// order never ran — for ever, because the epoch never advanced either.
//
// The sequence is assigned here, not read from the clock. A wall-clock column ties
// every directory of one batch together, and ties fall back to path order, which is
// the order that starved them: with a batch of [a, b, c] where a is interrupted,
// the next round would begin at a again, every round.
//
// A failed directory is claimable again: the failure may have been a permission the
// operator has since fixed.
func (s *Store) ClaimDirs(ctx context.Context, scope domain.ScopeID, epoch int64, limit int) ([][]byte, error) {
	var out [][]byte
	err := s.write(ctx, func(tx *sql.Tx) error {
		var next int64
		if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(claim_seq), 0) + 1 FROM scan_queue WHERE scope_id=? AND scan_epoch=?`,
			string(scope), epoch).Scan(&next); err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		rows, err := tx.QueryContext(ctx, `
SELECT raw_relative_dir FROM scan_queue
WHERE scope_id=? AND scan_epoch=? AND status IN ('pending','failed')
ORDER BY claim_seq IS NOT NULL, claim_seq, raw_relative_dir LIMIT ?`,
			string(scope), epoch, limit)
		if err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		for rows.Next() {
			var rel []byte
			if err := rows.Scan(&rel); err != nil {
				rows.Close()
				return errs.Wrap(errs.CodeDBError, err)
			}
			out = append(out, rel)
		}
		rows.Close()
		nowNS := s.now()
		for i, rel := range out {
			if _, err := tx.ExecContext(ctx, `
UPDATE scan_queue SET status='active', claim_seq=?, claimed_at_ns=?, attempts=attempts+1
WHERE scope_id=? AND scan_epoch=? AND raw_relative_dir=?`,
				next+int64(i), nowNS, string(scope), epoch, rel); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
	return out, err
}

// ReleaseClaimed puts directories that were claimed ahead of an interruption back
// to pending, without counting them as attempted.
//
// It is what makes the claim ordering mean something. A batch of eight directories
// where the first one exhausts the budget would otherwise leave seven rows holding
// a fresh sequence, so the next round would sort them behind the directory that was
// actually interrupted — and the small directories behind a large one would still
// wait for ever. Releasing them with no sequence says the truth: they were claimed,
// nobody looked inside.
func (s *Store) ReleaseClaimed(ctx context.Context, scope domain.ScopeID, epoch int64, dirs [][]byte) error {
	if len(dirs) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, rel := range dirs {
			if _, err := tx.ExecContext(ctx, `
UPDATE scan_queue SET status='pending', claim_seq=NULL, claimed_at_ns=NULL
WHERE scope_id=? AND scan_epoch=? AND raw_relative_dir=? AND status='active'`,
				string(scope), epoch, rel); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
}

// MarkDirDone retires a directory from the queue.
func (s *Store) MarkDirDone(ctx context.Context, scope domain.ScopeID, epoch int64, rel []byte) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE scan_queue SET status='done', last_error=NULL
WHERE scope_id=? AND scan_epoch=? AND raw_relative_dir=?`,
			string(scope), epoch, rel)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// MarkDirFailed records that a directory could not be read, without retiring it.
//
// The row stays in the queue as 'failed', which keeps the epoch open and keeps its
// observations from being retired as stale. The next round claims it again; when a
// round finally reads it, MarkDirDone replaces this record.
func (s *Store) MarkDirFailed(ctx context.Context, scope domain.ScopeID, epoch int64, rel []byte, reason string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE scan_queue SET status='failed', last_error=?
WHERE scope_id=? AND scan_epoch=? AND raw_relative_dir=?`,
			reason, string(scope), epoch, rel)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// FailedDirs lists the directories this epoch has not been able to read, so a
// report can name them instead of counting them.
func (s *Store) FailedDirs(ctx context.Context, scope domain.ScopeID, epoch int64) ([]string, error) {
	rows, err := s.read(ctx, `
SELECT raw_relative_dir FROM scan_queue
WHERE scope_id=? AND scan_epoch=? AND status='failed' ORDER BY raw_relative_dir`,
		string(scope), epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel []byte
		if err := rows.Scan(&rel); err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, string(rel))
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// BumpScanInterrupts counts one more interrupted round for a scope and returns
// the new value. It is the persistent half of "the last full pass did not
// finish": the run that observed the interruption is gone by the next one.
func (s *Store) BumpScanInterrupts(ctx context.Context, scope domain.ScopeID) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE scopes SET scan_interrupts=scan_interrupts+1 WHERE scope_id=?", string(scope)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return errs.Wrap(errs.CodeDBError, tx.QueryRowContext(ctx,
			"SELECT scan_interrupts FROM scopes WHERE scope_id=?", string(scope)).Scan(&n))
	})
	return n, err
}

// ResetScanInterrupts clears the counter after an epoch completed.
func (s *Store) ResetScanInterrupts(ctx context.Context, scope domain.ScopeID) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE scopes SET scan_interrupts=0 WHERE scope_id=?", string(scope))
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	})
}

// RequeueActive returns interrupted directories to pending. Used after a crash
// or a budget stop so the work is retried rather than silently dropped.
func (s *Store) RequeueActive(ctx context.Context, scope domain.ScopeID, epoch int64) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE scan_queue SET status='pending' WHERE scope_id=? AND scan_epoch=? AND status='active'`,
			string(scope), epoch)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// EnqueueDirs adds many directories in one transaction.
func (s *Store) EnqueueDirs(ctx context.Context, scope domain.ScopeID, epoch int64, dirs [][]byte) error {
	if len(dirs) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO scan_queue(scope_id, scan_epoch, raw_relative_dir, status)
VALUES(?,?,?,'pending') ON CONFLICT(scope_id, scan_epoch, raw_relative_dir) DO NOTHING`)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		defer stmt.Close()
		for _, rel := range dirs {
			if _, err := stmt.ExecContext(ctx, string(scope), epoch, rel); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
}
