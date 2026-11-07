package store

// A review request is prepared before it is sent and settled together with the
// record of its answer.
//
// The point is the gap between those two moments. If the process dies after the
// first HTTP call but before the decision is recorded, the next run otherwise
// sees no evidence that a question was ever asked and asks it again. An open
// dispatch row is that evidence: it is not an approval, not a keep, and not a
// reason to retry - it is a fact for a person to inspect.

import (
	"context"
	"database/sql"
	"encoding/json"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// ReviewDispatch is one prepared request. Pending rows are deliberately kept
// forever until settled; nothing here deletes history.
type ReviewDispatch struct {
	DispatchID   string `json:"dispatch_id"`
	ReviewID     string `json:"review_id"`
	RunID        string `json:"run_id"`
	ItemID       string `json:"item_id"`
	ScopeID      string `json:"scope_id"`
	Action       string `json:"action"`
	IdentityJSON string `json:"-"`
	TokenJSON    string `json:"-"`
	BudgetJSON   string `json:"budget,omitempty"`
	CreatedNS    int64  `json:"created_ns"`
	State        string `json:"state"`
	Attempts     int    `json:"attempts"`
	Failure      string `json:"failure,omitempty"`
	CostStatus   string `json:"cost_status"`
	ElapsedMS    int64  `json:"elapsed_ms,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	SettledNS    *int64 `json:"settled_ns,omitempty"`
	Note         string `json:"note,omitempty"`
}

// DispatchSettlement is what the review produced. It is written in the same
// transaction as the decision event, so a settled row always has an answer
// beside it and an unanswered row can never be mistaken for a decision.
type DispatchSettlement struct {
	Attempts     int
	Failure      string
	CostStatus   string
	ElapsedMS    int64
	HTTPStatus   int
	InputTokens  int
	OutputTokens int
	Note         string
	SettledNS    int64
}

// PrepareReviewDispatch records that a request is about to be sent. The caller
// must refuse to ask the question if this returns an error: a spent approval
// without a durable record of the question is exactly what this table prevents.
func (s *Store) PrepareReviewDispatch(ctx context.Context, d ReviewDispatch) error {
	if d.DispatchID == "" || d.ReviewID == "" || d.ItemID == "" || d.Action == "" ||
		d.IdentityJSON == "" || d.TokenJSON == "" {
		return errs.New(errs.CodeInvalidArgument, "a prepared review needs its object, action and evidence")
	}
	if d.State == "" {
		d.State = "pending"
	}
	if d.CostStatus == "" {
		d.CostStatus = "not_sent"
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_dispatch(
			dispatch_id, review_id, run_id, item_id, scope_id, action,
			identity_json, token_json, budget_json, created_ns, state, cost_status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			d.DispatchID, d.ReviewID, d.RunID, d.ItemID, d.ScopeID, d.Action,
			d.IdentityJSON, d.TokenJSON, d.BudgetJSON, d.CreatedNS, d.State, d.CostStatus)
		if err != nil {
			if isUniqueViolation(err) {
				return errs.New(errs.CodeStateWriteFailed, "this review already has a pending dispatch record")
			}
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return nil
	})
}

// SettleReviewDispatch closes one prepared request and writes its decision
// event in the same transaction. A failed settlement leaves the row pending,
// which is the state a person has to investigate.
func (s *Store) SettleReviewDispatch(ctx context.Context, reviewID string, st DispatchSettlement, ev Event) error {
	if reviewID == "" {
		return errs.New(errs.CodeInvalidArgument, "settlement needs the review it belongs to")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE review_dispatch SET
			state='settled', attempts=?, failure=?, cost_status=?, elapsed_ms=?,
			http_status=?, input_tokens=?, output_tokens=?, settled_ns=?, note=?
			WHERE review_id=? AND state='pending'`,
			st.Attempts, st.Failure, st.CostStatus, st.ElapsedMS, st.HTTPStatus,
			st.InputTokens, st.OutputTokens, st.SettledNS, st.Note, reviewID)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errs.New(errs.CodeItemNotFound, "there is no pending dispatch record for this review")
		}
		return appendEventTx(ctx, tx, ev)
	})
}

// RefuseReviewDispatch closes a prepared request that never reached a provider.
// The row stays on record, but it is no longer pending: the next run may ask the
// question again because there is no unanswered request to settle.
func (s *Store) RefuseReviewDispatch(ctx context.Context, reviewID, reason string, atNS int64, ev Event) error {
	if reviewID == "" {
		return errs.New(errs.CodeInvalidArgument, "refusal needs the review it belongs to")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE review_dispatch SET state='refused',
			cost_status='not_sent', settled_ns=?, note=? WHERE review_id=? AND state='pending'`,
			atNS, reason, reviewID)
		if err != nil {
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errs.New(errs.CodeItemNotFound, "there is no pending dispatch record for this review")
		}
		return appendEventTx(ctx, tx, ev)
	})
}

// OpenDispatchFor returns unsettled requests for one object and action. The
// lookup is by object and action rather than by item id, so a re-planned item
// cannot be asked twice for the same evidence.
func (s *Store) OpenDispatchFor(ctx context.Context, itemID, action, identityJSON, tokenJSON string) ([]ReviewDispatch, error) {
	rows, err := s.read(ctx, `SELECT dispatch_id, review_id, run_id, item_id, scope_id, action,
		identity_json, token_json, budget_json, created_ns, state, attempts, failure,
		cost_status, elapsed_ms, http_status, input_tokens, output_tokens, settled_ns, note
		FROM review_dispatch WHERE item_id=? AND action=? AND state='pending'
		ORDER BY created_ns DESC LIMIT 4`, itemID, action)
	if err != nil {
		return nil, err
	}
	var out []ReviewDispatch
	for rows.Next() {
		d, err := scanReviewDispatch(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if d.IdentityJSON == identityJSON && d.TokenJSON == tokenJSON {
			out = append(out, d)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	return out, nil
}

// ListReviewDispatch lists pending rows, or all rows when includeSettled is set.
func (s *Store) ListReviewDispatch(ctx context.Context, includeSettled bool, limit int) ([]ReviewDispatch, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT dispatch_id, review_id, run_id, item_id, scope_id, action,
		identity_json, token_json, budget_json, created_ns, state, attempts, failure,
		cost_status, elapsed_ms, http_status, input_tokens, output_tokens, settled_ns, note
		FROM review_dispatch`
	if !includeSettled {
		q += " WHERE state='pending'"
	}
	q += " ORDER BY created_ns DESC LIMIT ?"
	rows, err := s.read(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ReviewDispatch, 0, 8)
	for rows.Next() {
		d, err := scanReviewDispatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.CodeDBError, err)
	}
	return out, nil
}

func scanReviewDispatch(rows *sql.Rows) (ReviewDispatch, error) {
	var d ReviewDispatch
	var settled sql.NullInt64
	var note sql.NullString
	if err := rows.Scan(&d.DispatchID, &d.ReviewID, &d.RunID, &d.ItemID, &d.ScopeID, &d.Action,
		&d.IdentityJSON, &d.TokenJSON, &d.BudgetJSON, &d.CreatedNS, &d.State, &d.Attempts,
		&d.Failure, &d.CostStatus, &d.ElapsedMS, &d.HTTPStatus, &d.InputTokens, &d.OutputTokens,
		&settled, &note); err != nil {
		return ReviewDispatch{}, errs.Wrap(errs.CodeDBError, err)
	}
	if settled.Valid {
		v := settled.Int64
		d.SettledNS = &v
	}
	d.Note = note.String
	return d, nil
}

// dispatchEventJSON is the decision half of a settlement. It carries only the
// facts a person needs to correlate one answer with one request.
func dispatchEventJSON(reviewID string, st DispatchSettlement, verdict string) string {
	body, _ := json.Marshal(map[string]any{
		"review_id":     reviewID,
		"verdict":       verdict,
		"failure":       st.Failure,
		"attempts":      st.Attempts,
		"cost_status":   st.CostStatus,
		"elapsed_ms":    st.ElapsedMS,
		"http_status":   st.HTTPStatus,
		"input_tokens":  st.InputTokens,
		"output_tokens": st.OutputTokens,
	})
	return string(body)
}

// NewReviewEvent builds the event written with a settlement.
func NewReviewEvent(reviewID string, item domain.ItemID, planID *domain.PlanID, atNS int64, st DispatchSettlement, verdict string) Event {
	body := dispatchEventJSON(reviewID, st, verdict)
	contract, _ := json.Marshal(map[string]string{"review_id": reviewID})
	return Event{
		EventID:         domain.NewID("ev"),
		ItemID:          &item,
		ExecutionPlanID: planID,
		Action:          "review",
		Phase:           "info",
		OccurredAtNS:    atNS,
		ContractJSON:    string(contract),
		ResultJSON:      &body,
	}
}
