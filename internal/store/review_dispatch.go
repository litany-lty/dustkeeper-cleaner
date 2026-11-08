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
	"errors"
	"strings"

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
	// Resolution is a person's separate, append-only conclusion about this one
	// prepared request. It is nil until someone runs `review dispatch resolve`.
	// It never rewrites the original State, CostStatus, Attempts or usage above.
	Resolution *DispatchResolution `json:"resolution,omitempty"`
}

// DispatchResolution is one manual reconciliation of a prepared request. It is
// deliberately a different table and a different object from a settlement: it
// records what a person checked, not what a model or the transport answered.
type DispatchResolution struct {
	ResolvedNS    int64  `json:"resolved_ns"`
	RequestStatus string `json:"request_status"`
	BillingStatus string `json:"billing_status"`
	Note          string `json:"note"`
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

// MaxDispatchResolutionNote bounds a manual note. The limit is a single
// decision made here, at the one place that writes the table.
const MaxDispatchResolutionNote = 2000

// ValidateDispatchResolution is the one place the input boundary rules for a
// manual resolution live. Both the CLI (before it opens the state database) and
// the store (before it writes) call it, so a checked conclusion cannot be
// bypassed by another caller.
//
// The two status fields are deliberately enumerations: a request result or a
// billing result that is still "unknown" (or any other value) keeps the row
// pending and is refused. "not_sent" with "charged" is contradictory - a
// request that never left cannot have been billed - and is refused too.
func ValidateDispatchResolution(requestStatus, billingStatus, note string) error {
	switch requestStatus {
	case "not_sent", "completed", "failed":
	default:
		return errs.Newf(errs.CodeInvalidArgument,
			"request-status %q is not a checked result; use not_sent|completed|failed (an unknown result stays pending)", requestStatus)
	}
	switch billingStatus {
	case "not_charged", "charged":
	default:
		return errs.Newf(errs.CodeInvalidArgument,
			"billing-status %q is not a checked result; use not_charged|charged (an unknown cost stays pending)", billingStatus)
	}
	if requestStatus == "not_sent" && billingStatus == "charged" {
		return errs.New(errs.CodeInvalidArgument,
			"not_sent with charged is contradictory: a request that was never sent cannot have been billed")
	}
	if strings.TrimSpace(note) == "" {
		return errs.New(errs.CodeNeedsInput, "say what was checked with --note; an empty note is not a record")
	}
	if len(note) > MaxDispatchResolutionNote {
		return errs.Newf(errs.CodeInvalidArgument,
			"--note is %d bytes; the limit is %d", len(note), MaxDispatchResolutionNote)
	}
	return nil
}

// ResolveReviewDispatch records a person's conclusion about exactly one
// prepared request. It is one transaction: the target must still be pending, the
// append-only resolution is inserted, and an independent manual event is
// appended. If any part fails - in particular the event write - the whole thing
// rolls back, so a conclusion can never land without its event (and an event can
// never "unblock" a request whose conclusion was not written).
//
// The dispatch_id primary key is the idempotence guard: a second confirmation
// hits a unique violation and is refused, leaving the first record intact. The
// original request row keeps state='pending' and its preparation-time facts -
// a human decision is never disguised as settled or refused.
func (s *Store) ResolveReviewDispatch(ctx context.Context, dispatchID string, res DispatchResolution, ev Event) error {
	if err := ValidateDispatchResolution(res.RequestStatus, res.BillingStatus, res.Note); err != nil {
		return err
	}
	if dispatchID == "" {
		return errs.New(errs.CodeInvalidArgument, "a resolution needs the dispatch id it belongs to")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx,
			"SELECT state FROM review_dispatch WHERE dispatch_id=?", dispatchID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return errs.Newf(errs.CodeItemNotFound, "there is no prepared review dispatch with id %q", dispatchID)
		}
		if err != nil {
			return errs.Wrap(errs.CodeDBError, err)
		}
		if state != "pending" {
			return errs.Newf(errs.CodeItemNotFound,
				"review dispatch %q is %s, not pending; a settled or refused request cannot be reopened", dispatchID, state)
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO review_dispatch_resolution(
			dispatch_id, resolved_ns, request_status, billing_status, note)
			VALUES(?,?,?,?,?)`,
			dispatchID, res.ResolvedNS, res.RequestStatus, res.BillingStatus, res.Note)
		if err != nil {
			if isUniqueViolation(err) {
				return errs.Newf(errs.CodeStateWriteFailed,
					"review dispatch %q already has a manual resolution; the first record is kept", dispatchID)
			}
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		return appendEventTx(ctx, tx, ev)
	})
}

// reviewDispatchColumns and reviewDispatchResolutionColumns keep the two reads
// (OpenDispatchFor and ListReviewDispatch) and their one shared scanner aligned.
// Both read from review_dispatch d LEFT JOIN review_dispatch_resolution r, so a
// row always carries both the preparation-time facts and, when present, the
// independent manual conclusion - shown separately, never conflated.
const reviewDispatchColumns = `d.dispatch_id, d.review_id, d.run_id, d.item_id, d.scope_id, d.action,
	d.identity_json, d.token_json, d.budget_json, d.created_ns, d.state, d.attempts,
	d.failure, d.cost_status, d.elapsed_ms, d.http_status, d.input_tokens, d.output_tokens,
	d.settled_ns, d.note`

const reviewDispatchResolutionColumns = `r.resolved_ns, r.request_status, r.billing_status, r.note`

const reviewDispatchFrom = ` FROM review_dispatch d
	LEFT JOIN review_dispatch_resolution r ON r.dispatch_id = d.dispatch_id`

// OpenDispatchFor returns unresolved requests for one object and action. A
// pending row that already has a manual resolution is NOT unresolved: the
// person's conclusion lifts exactly this one request's hold, while every other
// pending dispatch, every other action, and every review_unknown row keeps its
// own independent hold. The lookup is by object and action rather than by item
// id, so a re-planned item cannot be asked twice for the same evidence.
func (s *Store) OpenDispatchFor(ctx context.Context, itemID, action, identityJSON, tokenJSON string) ([]ReviewDispatch, error) {
	rows, err := s.read(ctx, `SELECT `+reviewDispatchColumns+`, `+reviewDispatchResolutionColumns+
		reviewDispatchFrom+` WHERE d.item_id=? AND d.action=? AND d.state='pending'
		AND r.dispatch_id IS NULL
		ORDER BY d.created_ns DESC LIMIT 4`, itemID, action)
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

// ListReviewDispatch lists unresolved rows, or all rows when includeSettled is
// set. "Unresolved" means pending AND without a manual resolution: the default
// view is exactly the set that still blocks a re-ask. --all shows the original
// request facts alongside any independent manual conclusion.
func (s *Store) ListReviewDispatch(ctx context.Context, includeSettled bool, limit int) ([]ReviewDispatch, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + reviewDispatchColumns + `, ` + reviewDispatchResolutionColumns + reviewDispatchFrom
	if !includeSettled {
		q += " WHERE d.state='pending' AND r.dispatch_id IS NULL"
	}
	q += " ORDER BY d.created_ns DESC, d.dispatch_id LIMIT ?"
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
	var rNS sql.NullInt64
	var rReq, rBill, rNote sql.NullString
	if err := rows.Scan(&d.DispatchID, &d.ReviewID, &d.RunID, &d.ItemID, &d.ScopeID, &d.Action,
		&d.IdentityJSON, &d.TokenJSON, &d.BudgetJSON, &d.CreatedNS, &d.State, &d.Attempts,
		&d.Failure, &d.CostStatus, &d.ElapsedMS, &d.HTTPStatus, &d.InputTokens, &d.OutputTokens,
		&settled, &note, &rNS, &rReq, &rBill, &rNote); err != nil {
		return ReviewDispatch{}, errs.Wrap(errs.CodeDBError, err)
	}
	if settled.Valid {
		v := settled.Int64
		d.SettledNS = &v
	}
	d.Note = note.String
	if rNS.Valid {
		d.Resolution = &DispatchResolution{
			ResolvedNS:    rNS.Int64,
			RequestStatus: rReq.String,
			BillingStatus: rBill.String,
			Note:          rNote.String,
		}
	}
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

// NewReviewDispatchResolutionEvent builds the independent, informational event
// written with a manual resolution. It is deliberately NOT a "review" decision
// event: it carries no verdict, no attempts, no token/usage counts and no file
// action, so it does not increase model review counts, HTTP attempts, token
// usage or file-action counts. The dispatch id in the contract is what ties it
// back to the exact prepared request.
func NewReviewDispatchResolutionEvent(dispatchID string, res DispatchResolution, atNS int64) Event {
	contract, _ := json.Marshal(map[string]string{"dispatch_id": dispatchID})
	result, _ := json.Marshal(map[string]any{
		"source":         "manual_review_dispatch_resolution",
		"dispatch_id":    dispatchID,
		"request_status": res.RequestStatus,
		"billing_status": res.BillingStatus,
		"resolved_ns":    res.ResolvedNS,
	})
	body := string(result)
	return Event{
		EventID:      domain.NewID("ev"),
		Action:       "review_dispatch_resolution",
		Phase:        domain.PhaseInfo,
		OccurredAtNS: atNS,
		ContractJSON: string(contract),
		ResultJSON:   &body,
	}
}
