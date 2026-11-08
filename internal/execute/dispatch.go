package execute

// One review = one prepared request, one settlement, one correlation id.
//
// The record is written *before* the first HTTP call and settled together with
// the decision event. A process that dies in between leaves a pending row; the
// next run treats that row as an unanswered question and does not ask it again
// automatically. If the preparation cannot be written, no request is sent and
// no approval is used - there is no way to explain a paid question without it.

import (
	"context"
	"encoding/json"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// ReviewDiag is the bounded, payload-free review record the operational log
// writes. It carries correlation ids and measured request facts; never a prompt,
// response body, credential or file path. Budget is a small JSON summary of the
// allowance the request was prepared under, not the authority for the action.
type ReviewDiag struct {
	RunID        string
	ReviewID     string
	ItemID       string
	Action       string
	State        string // pending, settled, refused, not_started, awaiting_request_check
	Journal      bool   // a review_dispatch row backs this record
	Attempts     int
	Failure      string
	CostStatus   string
	ElapsedMS    int64
	HTTPStatus   int
	InputTokens  int
	OutputTokens int
	Budget       string
	Note         string
	// RequestTimings are the measured, payload-free timings of the requests this
	// review actually sent, keyed to the same ReviewID. Empty when no request
	// was dispatched; never padded with a zero-duration placeholder.
	RequestTimings []review.RequestTiming
}

// emitDiag drops the record when no logger was attached. A debug command that
// owns no state directory still goes through the reviewer; there is nothing to
// correlate and nothing to collect.
func (e *Executor) emitDiag(d ReviewDiag) {
	if e.Diag == nil {
		return
	}
	if d.RunID == "" {
		d.RunID = e.RunID
	}
	e.Diag(d)
}

// diagNoRequest records a candidate that was considered but never sent. It has
// no review id on purpose: there is no request to correlate, and inventing one
// would make the log claim a question was asked.
func (e *Executor) diagNoRequest(it store.Item, action, state, reason, budgetJSON string) {
	d := ReviewDiag{
		ItemID: string(it.ID), Action: action, State: state, Journal: false,
		CostStatus: review.CostNotSent, Budget: budgetJSON, Note: reason,
	}
	// A budget classification is a real failure category; a hold reason is a
	// disposition, not a provider failure, and must not be reported as one.
	if state == "not_started" {
		d.Failure = reason
	}
	e.emitDiag(d)
}

// prepareReview writes the pending dispatch record. Callers must stop on error.
func (e *Executor) prepareReview(ctx context.Context, it store.Item, scopeID, action, reviewID,
	budgetJSON, identity, token string) error {
	if e.Store == nil {
		// Debug and single-candidate commands may not own a state database.
		// They still go through the reviewer, but there is no journal to prepare.
		e.emitDiag(ReviewDiag{
			ReviewID: reviewID, ItemID: string(it.ID), Action: action,
			State: "pending", Journal: false, CostStatus: review.CostNotSent,
			Budget: budgetJSON,
		})
		return nil
	}
	runID := e.RunID
	if runID == "" {
		runID = "unattributed"
	}
	if err := e.Store.PrepareReviewDispatch(ctx, store.ReviewDispatch{
		DispatchID:   domain.NewID("disp"),
		ReviewID:     reviewID,
		RunID:        runID,
		ItemID:       string(it.ID),
		ScopeID:      scopeID,
		Action:       action,
		IdentityJSON: identity,
		TokenJSON:    token,
		BudgetJSON:   budgetJSON,
		CreatedNS:    e.Clock.Wall().UnixNano(),
		State:        "pending",
		CostStatus:   review.CostNotSent,
	}); err != nil {
		return err
	}
	e.emitDiag(ReviewDiag{
		RunID: runID, ReviewID: reviewID, ItemID: string(it.ID), Action: action,
		State: "pending", Journal: true, CostStatus: review.CostNotSent,
		Budget: budgetJSON,
	})
	return nil
}

// settleReview commits the request facts and the decision event together.
// A failure here means the approval must not be used for a file action.
func (e *Executor) settleReview(ctx context.Context, it store.Item, planID *domain.PlanID,
	reviewID, action string, started time.Time, out review.Outcome, budgetJSON string) error {
	d := out.Decision
	inTok, outTok := int64(0), int64(0)
	if d.JEV != nil && d.JEV.Usage != nil {
		inTok += d.JEV.Usage.InputTokens
		outTok += d.JEV.Usage.OutputTokens
	}
	if d.LLMUsage != nil {
		inTok += d.LLMUsage.InputTokens
		outTok += d.LLMUsage.OutputTokens
	}
	st := store.DispatchSettlement{
		Attempts:     d.Attempts,
		Failure:      d.Failure,
		CostStatus:   d.CostStatus,
		ElapsedMS:    time.Since(started).Milliseconds(),
		HTTPStatus:   d.HTTPStatus,
		InputTokens:  int(inTok),
		OutputTokens: int(outTok),
		Note:         out.Reason,
		SettledNS:    e.Clock.Wall().UnixNano(),
	}
	if e.Store == nil {
		e.emitDiag(ReviewDiag{
			ReviewID: reviewID, ItemID: string(it.ID), Action: action,
			State: "settled", Journal: false, Attempts: st.Attempts,
			Failure: st.Failure, CostStatus: st.CostStatus, ElapsedMS: st.ElapsedMS,
			HTTPStatus: st.HTTPStatus, InputTokens: st.InputTokens,
			OutputTokens: st.OutputTokens, Budget: budgetJSON, Note: out.Reason,
			RequestTimings: d.RequestTimings,
		})
		return nil
	}
	ev := store.NewReviewEvent(reviewID, it.ID, planID, st.SettledNS, st, d.Verdict)
	// The settlement event is the one authoritative record of a prepared
	// request and already carries the real review id. Attach the measured
	// timings there so the result page can correlate them; an empty slice adds
	// nothing, so a request that was never sent stays unrecorded.
	ev.ResultJSON = withRequestTimings(ev.ResultJSON, d.RequestTimings)
	if err := e.Store.SettleReviewDispatch(ctx, reviewID, st, ev); err != nil {
		return err
	}
	e.emitDiag(ReviewDiag{
		ReviewID: reviewID, ItemID: string(it.ID), Action: action,
		State: "settled", Journal: true, Attempts: st.Attempts,
		Failure: st.Failure, CostStatus: st.CostStatus, ElapsedMS: st.ElapsedMS,
		HTTPStatus: st.HTTPStatus, InputTokens: st.InputTokens,
		OutputTokens: st.OutputTokens, Budget: budgetJSON, Note: out.Reason,
		RequestTimings: d.RequestTimings,
	})
	return nil
}

// withRequestTimings merges the measured, payload-free request timings into the
// settlement event's result JSON, keeping every field the dispatcher wrote.
// RequestTiming carries only provider, outcome, phase, elapsed_ms and stages_ms,
// so no URL, header, body or credential can enter the record.
func withRequestTimings(result *string, timings []review.RequestTiming) *string {
	if result == nil || len(timings) == 0 {
		return result
	}
	var body map[string]any
	if json.Unmarshal([]byte(*result), &body) != nil {
		return result
	}
	body["request_timings"] = timings
	merged, err := json.Marshal(body)
	if err != nil {
		return result
	}
	s := string(merged)
	return &s
}

// refuseReview closes a prepared request whose review never started. A local
// refusal (pause, missing reviewer) happens before any provider call, so there
// is no unanswered request to settle and no unknown cost to carry forward.
func (e *Executor) refuseReview(ctx context.Context, it store.Item, planID *domain.PlanID,
	reviewID, action string, atNS int64, reason, budgetJSON string) error {
	e.emitDiag(ReviewDiag{
		ReviewID: reviewID, ItemID: string(it.ID), Action: action,
		State: "refused", Journal: e.Store != nil, CostStatus: review.CostNotSent,
		Budget: budgetJSON, Note: reason,
	})
	if e.Store == nil {
		return nil
	}
	st := store.DispatchSettlement{CostStatus: review.CostNotSent, SettledNS: atNS}
	ev := store.NewReviewEvent(reviewID, it.ID, planID, atNS, st, "not_started")
	return e.Store.RefuseReviewDispatch(ctx, reviewID, reason, atNS, ev)
}

// dispatchBudgetJSON records the allowance the request was prepared under.
// It is diagnostic, not an authority: the reviewer and the executor still own
// the real budget objects.
func dispatchBudgetJSON(action string, spend *domain.Budget) string {
	body := map[string]any{"action": action}
	if spend != nil {
		body["max_files"] = spend.MaxFiles
		body["max_bytes"] = spend.MaxBytes
		if spend.Run != nil {
			body["soft_seconds"] = int(spend.Run.SoftTime.Seconds())
			body["remaining_read_bytes"] = spend.RemainingRead()
		}
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}
