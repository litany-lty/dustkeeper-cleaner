package execute

import (
	"context"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/outcome"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// keepItem records that a planned item was left exactly where it is, and why.
//
// Leaving it unrecorded was the earlier behaviour and it produced the worst
// possible output: a run that reported "0 deleted, 0 moved" and no explanation,
// so the operator could not tell "nothing qualified" from "the model service
// answered, and said no".
//
// Nothing about the item's state changes: it stays planned, keeps no pending
// marker and does not hold the scope. A keep is an answer about this run, not a
// new lifecycle state - and it must never make the next run refuse to work.
func (e *Executor) keepItem(ctx context.Context, sc *ScopeCtx, p store.Plan, it store.Item, reviewed review.Outcome) (domain.OperationResult, error) {
	return e.recordKeep(ctx, sc, it, &p.ID, reviewed)
}

// recordKeep is the same record for an object that is already in the
// observation area and whose expiry review did not authorise clearing.
func (e *Executor) recordKeep(ctx context.Context, sc *ScopeCtx, it store.Item, planID *domain.PlanID, reviewed review.Outcome) (domain.OperationResult, error) {
	res := domain.OperationResult{Item: it.ID, State: it.State, Control: domain.ControlContinue, Evidence: domain.EvidenceNotStarted}

	payload := `{"source":"decision","disposition":` + quoteJSON(domain.DispositionKeep) +
		`,"reason":` + quoteJSON(reviewed.Reason) +
		`,"reason_text":` + quoteJSON(outcome.ReasonText(reviewed.Reason)) +
		`,"verdict":` + quoteJSON(reviewed.Decision.Verdict) +
		`,"model_error":` + quoteJSON(reviewed.Decision.ModelError) + `}`
	if err := e.Store.AppendEvent(ctx, store.Event{
		EventID:         domain.NewID("ev"),
		ItemID:          &it.ID,
		ExecutionPlanID: planID,
		Action:          "keep",
		Phase:           domain.PhaseInfo,
		OccurredAtNS:    e.Clock.Wall().UnixNano(),
		ContractJSON:    `{"scope_id":` + quoteJSON(string(sc.Scope.ID)) + `,"raw_relative_b64":` + quoteJSON(plan.B64(it.RawRelative)) + `}`,
		ResultJSON:      &payload,
	}); err != nil {
		return res, err
	}

	// A review that could not be completed is not a verdict about the file, and
	// it is the one keep reason that must not be reported as a finished run:
	// every further item would fail the same way, and telling the user
	// "0 deleted" would describe an outage as a clean sweep.
	if reviewed.Reason == outcome.ReasonReviewUnavailable {
		res.Code = string(errs.CodeModelUnavailable)
		res.Message = "the review could not be completed, so nothing was done: " + reviewed.Decision.ModelError
		res.Control = domain.ControlStopScope
		return res, errs.Newf(errs.CodeModelUnavailable,
			"review unavailable (%s); no file was moved or deleted", reviewed.Decision.ModelError)
	}

	res.Code = string(errs.CodeKept)
	res.Message = outcome.ReasonText(reviewed.Reason)
	return res, nil
}
