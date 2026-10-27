package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Event is one audit record. events is authoritative: the JSONL export and the
// terminal output are derived views and must never drive a purge.
type Event struct {
	Sequence        int64
	EventID         string
	OpID            *domain.OperationID
	ItemID          *domain.ItemID
	ExecutionPlanID *domain.PlanID
	Action          string
	Phase           string
	OccurredAtNS    int64
	ContractJSON    string
	ResultJSON      *string
}

func appendEventTx(ctx context.Context, tx *sql.Tx, ev Event) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO events(event_id, op_id, item_id, execution_plan_id, action, phase,
                   occurred_at_ns, contract_json, result_json)
VALUES(?,?,?,?,?,?,?,?,?)`,
		ev.EventID, ev.OpID, ev.ItemID, ev.ExecutionPlanID, ev.Action, ev.Phase,
		ev.OccurredAtNS, ev.ContractJSON, ev.ResultJSON)
	if err != nil {
		// The unique indexes on op_id make a second intent or a second terminal
		// event for one operation impossible; report it as a state conflict
		// rather than retrying.
		return errs.Newf(errs.CodeStateWriteFailed, "append event %s/%s: %v", ev.Action, ev.Phase, err)
	}
	return nil
}

// AppendEvent records an informational event outside any operation.
func (s *Store) AppendEvent(ctx context.Context, ev Event) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return appendEventTx(ctx, tx, ev)
	})
}

const eventColumns = `sequence, event_id, op_id, item_id, execution_plan_id, action, phase,
	occurred_at_ns, contract_json, result_json`

func scanEvent(rows *sql.Rows) (Event, error) {
	var ev Event
	var op, item, plan, result sql.NullString
	err := rows.Scan(&ev.Sequence, &ev.EventID, &op, &item, &plan, &ev.Action, &ev.Phase,
		&ev.OccurredAtNS, &ev.ContractJSON, &result)
	if err != nil {
		return ev, err
	}
	if op.Valid {
		v := domain.OperationID(op.String)
		ev.OpID = &v
	}
	if item.Valid {
		v := domain.ItemID(item.String)
		ev.ItemID = &v
	}
	if plan.Valid {
		v := domain.PlanID(plan.String)
		ev.ExecutionPlanID = &v
	}
	if result.Valid {
		v := result.String
		ev.ResultJSON = &v
	}
	return ev, nil
}

// ListEvents returns audit events in sequence order.
func (s *Store) ListEvents(ctx context.Context, limit int, afterSeq int64) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.read(ctx, "SELECT "+eventColumns+
		" FROM events WHERE sequence > ? ORDER BY sequence LIMIT ?", afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, ev)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// EventsForItem returns the audit trail of one object.
func (s *Store) EventsForItem(ctx context.Context, itemID domain.ItemID) ([]Event, error) {
	rows, err := s.read(ctx, "SELECT "+eventColumns+
		" FROM events WHERE item_id=? ORDER BY sequence", string(itemID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		out = append(out, ev)
	}
	return out, errs.Wrap(errs.CodeDBError, rows.Err())
}

// PendingOp is an operation whose intent committed but whose outcome is not
// recorded. Reconciliation works from this list.
type PendingOp struct {
	OpID         domain.OperationID
	Item         Item
	Action       string
	ContractJSON string
	IntentAtNS   int64
}

// PendingOps returns every unfinished operation. A non-empty result blocks new
// file changes until it is reconciled.
func (s *Store) PendingOps(ctx context.Context) ([]PendingOp, error) {
	rows, err := s.read(ctx, "SELECT item_id, pending_op_id, state FROM items WHERE pending_op_id IS NOT NULL ORDER BY item_id")
	if err != nil {
		return nil, err
	}
	type ref struct {
		itemID string
		opID   string
	}
	var refs []ref
	for rows.Next() {
		var r ref
		var state string
		if err := rows.Scan(&r.itemID, &r.opID, &state); err != nil {
			rows.Close()
			return nil, errs.Wrap(errs.CodeDBError, err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	rows.Close()

	out := make([]PendingOp, 0, len(refs))
	for _, r := range refs {
		it, err := s.GetItem(ctx, domain.ItemID(r.itemID))
		if err != nil {
			return nil, err
		}
		evRows, err := s.read(ctx, "SELECT "+eventColumns+
			" FROM events WHERE op_id=? AND phase='intent'", r.opID)
		if err != nil {
			return nil, err
		}
		var po PendingOp
		if evRows.Next() {
			ev, err := scanEvent(evRows)
			if err != nil {
				evRows.Close()
				return nil, errs.Wrap(errs.CodeDBError, err)
			}
			po = PendingOp{
				OpID:         domain.OperationID(r.opID),
				Item:         it,
				Action:       ev.Action,
				ContractJSON: ev.ContractJSON,
				IntentAtNS:   ev.OccurredAtNS,
			}
		}
		evRows.Close()
		if po.OpID == "" {
			// An item claims a pending operation with no intent event. That is
			// not explainable, so it is surfaced rather than repaired.
			po = PendingOp{OpID: domain.OperationID(r.opID), Item: it, Action: "unknown"}
		}
		out = append(out, po)
	}
	return out, nil
}

// LastIntentForItem returns the most recent intent event for one item, whether
// or not the item still carries a pending marker.
//
// Adoption needs exactly this: an older build could clear pending_op_id while
// leaving the object in the vault, so the item row no longer points at its
// operation even though the audit trail still does. Reading the trail is the
// only way to converge such an item without inventing facts.
func (s *Store) LastIntentForItem(ctx context.Context, id domain.ItemID) (PendingOp, bool, error) {
	rows, err := s.read(ctx, "SELECT op_id, action, contract_json, occurred_at_ns FROM events"+
		" WHERE item_id=? AND phase='intent' ORDER BY sequence DESC LIMIT 1", string(id))
	if err != nil {
		return PendingOp{}, false, err
	}
	var opID, action, contract string
	var atNS int64
	found := false
	if rows.Next() {
		if err := rows.Scan(&opID, &action, &contract, &atNS); err != nil {
			rows.Close()
			return PendingOp{}, false, errs.Wrap(errs.CodeDBError, err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return PendingOp{}, false, errs.Wrap(errs.CodeDBError, err)
	}
	// The rows are closed before the item is read: the store runs on a single
	// connection, so a query issued while these rows are open would wait for
	// itself.
	rows.Close()
	if !found {
		return PendingOp{}, false, nil
	}
	it, err := s.GetItem(ctx, id)
	if err != nil {
		return PendingOp{}, false, err
	}
	return PendingOp{
		OpID:         domain.OperationID(opID),
		Item:         it,
		Action:       action,
		ContractJSON: contract,
		IntentAtNS:   atNS,
	}, true, nil
}

// ReconcileAttempts counts the non-terminal reconciliation attempts recorded for
// one item. It is informational: the count exists so an operator can see that an
// item is not progressing, never so that the item can be dropped.
func (s *Store) ReconcileAttempts(ctx context.Context, id domain.ItemID) (int, error) {
	rows, err := s.read(ctx, "SELECT COUNT(*) FROM events"+
		" WHERE item_id=? AND action='reconcile' AND phase='info'", string(id))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	if !rows.Next() {
		return 0, errs.Wrap(errs.CodeDBError, rows.Err())
	}
	if err := rows.Scan(&n); err != nil {
		return 0, errs.Wrap(errs.CodeDBError, err)
	}
	return n, nil
}

// Intent is a request to begin one operation on one item.
type Intent struct {
	OpID         domain.OperationID
	ItemID       domain.ItemID
	PlanID       *domain.PlanID
	Action       string
	PriorState   string
	NextState    string
	ContractJSON string
	OccurredAtNS int64
	PinKeeper    bool
}

// BeginIntent commits the operation intent and the item transition together.
//
// After this returns nil the operation is on record; before it returns, nothing
// may have been moved. The caller relies on that ordering to decide whether a
// crash is explainable.
func (s *Store) BeginIntent(ctx context.Context, in Intent) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var state string
		var pending sql.NullString
		err := tx.QueryRowContext(ctx,
			"SELECT state, pending_op_id FROM items WHERE item_id=?", string(in.ItemID)).Scan(&state, &pending)
		if errors.Is(err, sql.ErrNoRows) {
			return errs.Newf(errs.CodeItemNotFound, "item %q does not exist", in.ItemID)
		}
		if err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		if pending.Valid {
			return errs.Newf(errs.CodePlanStale,
				"item %s already has pending operation %s", in.ItemID, pending.String)
		}
		if state != in.PriorState {
			return errs.Newf(errs.CodePlanStale,
				"item %s is %q, expected %q", in.ItemID, state, in.PriorState)
		}

		res, err := tx.ExecContext(ctx, `
UPDATE items SET state=?, pending_op_id=?, keeper_pin_active=CASE WHEN ?=1 THEN 1 ELSE keeper_pin_active END
WHERE item_id=? AND state=? AND pending_op_id IS NULL`,
			in.NextState, string(in.OpID), boolToInt(in.PinKeeper), string(in.ItemID), in.PriorState)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errs.Newf(errs.CodePlanStale, "item %s changed while starting the operation", in.ItemID)
		}

		itemID := in.ItemID
		opID := in.OpID
		return appendEventTx(ctx, tx, Event{
			EventID:         domain.NewID("ev"),
			OpID:            &opID,
			ItemID:          &itemID,
			ExecutionPlanID: in.PlanID,
			Action:          in.Action,
			Phase:           domain.PhaseIntent,
			OccurredAtNS:    in.OccurredAtNS,
			ContractJSON:    in.ContractJSON,
		})
	})
}

// QuarantineResult is everything learned after the move completed.
type QuarantineResult struct {
	CapturedIdentityJSON string
	CapturedTokenJSON    string
	QuarantinedAtNS      int64
	PurgeNotBeforeNS     int64
	ContentHash          string
	MetadataHash         string
}

// CompleteQuarantine records a verified quarantine.
//
// The state gate accepts both q_intent and unknown. "unknown" here means the
// operation was parked after the move but before it could be proven, which is
// precisely the case reconciliation exists to finish. The pending marker must
// still match, so an item that was terminal-held (pending cleared) is not
// silently completed.
func (s *Store) CompleteQuarantine(ctx context.Context, in Intent, res QuarantineResult, resultJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `
UPDATE items SET state='quarantined', pending_op_id=NULL,
  captured_identity_json=?, captured_token_json=?, quarantined_at_ns=?,
  purge_not_before_ns=?, content_hash=?, metadata_hash=?
WHERE item_id=? AND pending_op_id=? AND state IN ('q_intent','unknown')`,
			res.CapturedIdentityJSON, res.CapturedTokenJSON, res.QuarantinedAtNS,
			res.PurgeNotBeforeNS, res.ContentHash, res.MetadataHash,
			string(in.ItemID), string(in.OpID))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			// Already completed: this is the idempotent case, not an error.
			var state string
			var pending sql.NullString
			if err := tx.QueryRowContext(ctx,
				"SELECT state, pending_op_id FROM items WHERE item_id=?", string(in.ItemID)).Scan(&state, &pending); err != nil {
				return errs.Wrap(errs.CodeDBError, err)
			}
			if state == domain.StateQuarantined && !pending.Valid {
				return nil
			}
			return errs.Newf(errs.CodePlanStale, "item %s is not awaiting quarantine completion", in.ItemID)
		}
		return completeTx(ctx, tx, in, resultJSON)
	})
}

// CompleteRestore records a completed restore.
func (s *Store) CompleteRestore(ctx context.Context, in Intent, resultJSON string) error {
	return s.finishSimple(ctx, in, domain.StateRIntent, domain.StateRestored, resultJSON)
}

// CompletePurge records a completed purge.
func (s *Store) CompletePurge(ctx context.Context, in Intent, resultJSON string) error {
	return s.finishSimple(ctx, in, domain.StatePIntent, domain.StatePurged, resultJSON)
}

func (s *Store) finishSimple(ctx context.Context, in Intent, fromState, toState, resultJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		// "unknown" is accepted for the same reason as in CompleteQuarantine: a
		// parked operation is unfinished, not held.
		r, err := tx.ExecContext(ctx, `
UPDATE items SET state=?, pending_op_id=NULL, hold_code=NULL
WHERE item_id=? AND pending_op_id=? AND state IN (?, 'unknown')`,
			toState, string(in.ItemID), string(in.OpID), fromState)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			var state string
			var pending sql.NullString
			if err := tx.QueryRowContext(ctx,
				"SELECT state, pending_op_id FROM items WHERE item_id=?", string(in.ItemID)).Scan(&state, &pending); err != nil {
				return errs.Wrap(errs.CodeDBError, err)
			}
			if state == toState && !pending.Valid {
				return nil
			}
			return errs.Newf(errs.CodePlanStale, "item %s is not awaiting %s", in.ItemID, toState)
		}
		return completeTx(ctx, tx, in, resultJSON)
	})
}

func completeTx(ctx context.Context, tx *sql.Tx, in Intent, resultJSON string) error {
	itemID := in.ItemID
	opID := in.OpID
	var result *string
	if resultJSON != "" {
		result = &resultJSON
	}
	return appendEventTx(ctx, tx, Event{
		EventID:         domain.NewID("ev"),
		OpID:            &opID,
		ItemID:          &itemID,
		ExecutionPlanID: in.PlanID,
		Action:          in.Action,
		Phase:           domain.PhaseComplete,
		OccurredAtNS:    in.OccurredAtNS,
		ContractJSON:    in.ContractJSON,
		ResultJSON:      result,
	})
}

// ParkIntent records an operation whose outcome cannot be proven while keeping
// its pending marker.
//
// This is the only correct response to a failure after the filesystem already
// changed: the item moves to "unknown", reconciliation still examines it, and
// nothing is claimed to have succeeded. Clearing the pending marker here would
// orphan a file that really did move.
//
// The event phase is `park`, not `hold`. A hold is a verdict and is unique per
// operation; writing one here used to make the later completion impossible,
// because one_terminal_per_op then rejected the completion event for the same
// operation. A park says "still unknown, keep looking".
func (s *Store) ParkIntent(ctx context.Context, in Intent, code, resultJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `
UPDATE items SET state='unknown', hold_code=?
WHERE item_id=? AND pending_op_id=?`, code, string(in.ItemID), string(in.OpID))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errs.Newf(errs.CodePlanStale, "item %s is not awaiting a parked outcome", in.ItemID)
		}
		itemID := in.ItemID
		opID := in.OpID
		result := resultJSON
		if result == "" {
			result = `{"hold_code":` + quoteJSONString(code) + `}`
		}
		return appendEventTx(ctx, tx, Event{
			EventID:         domain.NewID("ev"),
			OpID:            &opID,
			ItemID:          &itemID,
			ExecutionPlanID: in.PlanID,
			Action:          in.Action,
			Phase:           domain.PhasePark,
			OccurredAtNS:    in.OccurredAtNS,
			ContractJSON:    in.ContractJSON,
			ResultJSON:      &result,
		})
	})
}

// NoteParked records another failed reconciliation attempt without closing the
// operation. Pending is kept on purpose: the object may still be finished once
// the cause (an I/O error, a full disk, a temporary permission problem) clears.
//
// `attempts` is informational. There is no automatic give-up: a parked object
// that keeps failing stays visible and keeps its evidence, which is what an
// operator needs to decide what to do. Bounded retry exists so that the state
// can be read as "not progressing", not to silently drop the item.
func (s *Store) NoteParked(ctx context.Context, id domain.ItemID, code, detail string, attempts int) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE items SET hold_code=? WHERE item_id=? AND pending_op_id IS NOT NULL",
			code, string(id)); err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		itemID := id
		result := `{"source":"reconciled","result":"still parked","hold_code":` +
			quoteJSONString(code) + `,"attempts":` + itoa64(int64(attempts)) + `}`
		if detail != "" {
			result = `{"source":"reconciled","result":"still parked","hold_code":` +
				quoteJSONString(code) + `,"attempts":` + itoa64(int64(attempts)) +
				`,"detail":` + quoteJSONString(detail) + `}`
		}
		return appendEventTx(ctx, tx, Event{
			EventID:      domain.NewID("ev"),
			ItemID:       &itemID,
			Action:       "reconcile",
			Phase:        domain.PhaseInfo,
			OccurredAtNS: s.now(),
			ContractJSON: `{"source":"reconciled"}`,
			ResultJSON:   &result,
		})
	})
}

// HoldIntentWithCapture is HoldIntent for the case where the object that
// actually moved is known but is not the planned object.
//
// The captured identity and token are written in the same transaction as the
// terminal hold, because a hold without that evidence is a file in the vault
// that the product can neither purge nor restore. Section 11.3 requires the
// expected and the actual evidence to be stored separately, and this column pair
// is where "actual" lives.
func (s *Store) HoldIntentWithCapture(ctx context.Context, in Intent, code, resultJSON string,
	capturedIdentityJSON, capturedTokenJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `
UPDATE items SET state='held', hold_code=?, pending_op_id=NULL,
  captured_identity_json=?, captured_token_json=?
WHERE item_id=? AND pending_op_id=?`,
			code, capturedIdentityJSON, capturedTokenJSON, string(in.ItemID), string(in.OpID))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errs.Newf(errs.CodePlanStale, "item %s is not awaiting a hold", in.ItemID)
		}
		itemID := in.ItemID
		opID := in.OpID
		var result *string
		if resultJSON != "" {
			result = &resultJSON
		}
		return appendEventTx(ctx, tx, Event{
			EventID:         domain.NewID("ev"),
			OpID:            &opID,
			ItemID:          &itemID,
			ExecutionPlanID: in.PlanID,
			Action:          in.Action,
			Phase:           domain.PhaseHold,
			OccurredAtNS:    in.OccurredAtNS,
			ContractJSON:    in.ContractJSON,
			ResultJSON:      result,
		})
	})
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// AbortIntent closes an attempt that provably did not change the file. backState
// is where the item returns to: "aborted" after a quarantine that never moved
// anything, or the prior quarantine state after a failed restore/purge.
func (s *Store) AbortIntent(ctx context.Context, in Intent, backState, resultJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `
UPDATE items SET state=?, pending_op_id=NULL
WHERE item_id=? AND pending_op_id=?`, backState, string(in.ItemID), string(in.OpID))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errs.Newf(errs.CodePlanStale, "item %s is not awaiting an abort", in.ItemID)
		}
		itemID := in.ItemID
		opID := in.OpID
		var result *string
		if resultJSON != "" {
			result = &resultJSON
		}
		return appendEventTx(ctx, tx, Event{
			EventID:         domain.NewID("ev"),
			OpID:            &opID,
			ItemID:          &itemID,
			ExecutionPlanID: in.PlanID,
			Action:          in.Action,
			Phase:           domain.PhaseAbort,
			OccurredAtNS:    in.OccurredAtNS,
			ContractJSON:    in.ContractJSON,
			ResultJSON:      result,
		})
	})
}

// HoldIntent freezes an item whose real state cannot be explained or whose
// post-checks failed. A held item never becomes purge-eligible automatically.
func (s *Store) HoldIntent(ctx context.Context, in Intent, code, resultJSON string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `
UPDATE items SET state='held', hold_code=?, pending_op_id=NULL
WHERE item_id=? AND pending_op_id=?`, code, string(in.ItemID), string(in.OpID))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errs.Newf(errs.CodePlanStale, "item %s is not awaiting a hold", in.ItemID)
		}
		itemID := in.ItemID
		opID := in.OpID
		var result *string
		if resultJSON != "" {
			result = &resultJSON
		}
		return appendEventTx(ctx, tx, Event{
			EventID:         domain.NewID("ev"),
			OpID:            &opID,
			ItemID:          &itemID,
			ExecutionPlanID: in.PlanID,
			Action:          in.Action,
			Phase:           domain.PhaseHold,
			OccurredAtNS:    in.OccurredAtNS,
			ContractJSON:    in.ContractJSON,
			ResultJSON:      result,
		})
	})
}

// MarkItemState changes an item state outside an operation. It exists for
// reconciliation only: a state that cannot be explained must be recorded, and
// "unknown" is a real state, not a missing one.
func (s *Store) MarkItemState(ctx context.Context, id domain.ItemID, state string, holdCode string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var code *string
		if holdCode != "" {
			code = &holdCode
		}
		_, err := tx.ExecContext(ctx,
			"UPDATE items SET state=?, hold_code=?, pending_op_id=NULL WHERE item_id=?",
			state, code, string(id))
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		itemID := id
		return appendEventTx(ctx, tx, Event{
			EventID:      domain.NewID("ev"),
			ItemID:       &itemID,
			Action:       "reconcile",
			Phase:        domain.PhaseInfo,
			OccurredAtNS: s.now(),
			ContractJSON: `{"source":"reconciled"}`,
			ResultJSON:   code,
		})
	})
}

// RestoreIntent is a restore intent that also persists its protections in the
// same transaction, as required by section 10.3 step 4.
func (s *Store) RestoreIntent(ctx context.Context, in Intent, protections []Protection) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.beginIntentTx(ctx, tx, in); err != nil {
			return err
		}
		for _, p := range protections {
			if err := insertProtectionTx(ctx, tx, p); err != nil {
				return err
			}
		}
		if len(protections) > 0 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE scopes SET protection_revision=protection_revision+1 WHERE scope_id=?",
				string(protections[0].ScopeID)); err != nil {
				return errs.Wrap(errs.CodeStateWriteFailed, err)
			}
		}
		return nil
	})
}

// beginIntentTx is BeginIntent without its own transaction wrapper.
func (s *Store) beginIntentTx(ctx context.Context, tx *sql.Tx, in Intent) error {
	var state string
	var pending sql.NullString
	err := tx.QueryRowContext(ctx,
		"SELECT state, pending_op_id FROM items WHERE item_id=?", string(in.ItemID)).Scan(&state, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.Newf(errs.CodeItemNotFound, "item %q does not exist", in.ItemID)
	}
	if err != nil {
		return errs.Wrap(errs.CodeDBError, err)
	}
	if pending.Valid {
		return errs.Newf(errs.CodePlanStale,
			"item %s already has pending operation %s", in.ItemID, pending.String)
	}
	if state != in.PriorState {
		return errs.Newf(errs.CodePlanStale, "item %s is %q, expected %q", in.ItemID, state, in.PriorState)
	}
	res, err := tx.ExecContext(ctx, `
UPDATE items SET state=?, pending_op_id=? WHERE item_id=? AND state=? AND pending_op_id IS NULL`,
		in.NextState, string(in.OpID), string(in.ItemID), in.PriorState)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errs.Newf(errs.CodePlanStale, "item %s changed while starting the operation", in.ItemID)
	}
	itemID := in.ItemID
	opID := in.OpID
	return appendEventTx(ctx, tx, Event{
		EventID:         domain.NewID("ev"),
		OpID:            &opID,
		ItemID:          &itemID,
		ExecutionPlanID: in.PlanID,
		Action:          in.Action,
		Phase:           domain.PhaseIntent,
		OccurredAtNS:    in.OccurredAtNS,
		ContractJSON:    in.ContractJSON,
	})
}
