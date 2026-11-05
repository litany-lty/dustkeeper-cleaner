package execute

import (
	"context"
	"encoding/json"
	"time"

	"dustkeeper/internal/cleanup"
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
		`,"model_error":` + quoteJSON(reviewed.Decision.ModelError) +
		// The version this decision was about. Without it a later run can only
		// ask "was this path kept", which would also silence a different file
		// that has since been created at the same place.
		`,"identity":` + quoteJSON(it.PreIdentityJSON) +
		`,"token":` + quoteJSON(it.PreTokenJSON) + `}`
	ruleDigest := ""
	if binding, berr := PlanBindingOf(it); berr == nil && binding.RuleDigest != nil {
		ruleDigest = *binding.RuleDigest
	}
	contractJSON := `{"scope_id":` + quoteJSON(string(sc.Scope.ID)) +
		`,"raw_relative_b64":` + quoteJSON(plan.B64(it.RawRelative)) +
		`,"rule_digest":` + quoteJSON(ruleDigest) + `}`
	if err := e.Store.AppendEvent(ctx, store.Event{
		EventID:         domain.NewID("ev"),
		ItemID:          &it.ID,
		ExecutionPlanID: planID,
		Action:          "keep",
		Phase:           domain.PhaseInfo,
		OccurredAtNS:    e.Clock.Wall().UnixNano(),
		ContractJSON:    contractJSON,
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

// keptRecently reports whether this exact version of this path already received
// a keep decision that is still inside its retry interval.
//
// The interval is a spending rule, not a safety rule: a kept file cannot be
// moved or deleted by this program either way, so asking the models again the
// next minute buys nothing and costs a call. It is deliberately narrower than
// "this path was kept once":
//
//   - the file must be the same object with the same content token, so editing
//     or replacing it makes it eligible again;
//   - the rule binding is part of the comparison, so a revised rule is asked
//     about even when the file did not change;
//   - a keep that records a failed review is not a decision about the file and
//     does not suppress anything, because the next run should try again.
func (e *Executor) keptRecently(ctx context.Context, sc *ScopeCtx, it store.Item,
	preID domain.FileIdentity, preTok domain.ChangeToken, pol cleanup.Policy) (bool, error) {

	if pol.KeepRetryDays <= 0 {
		return false, nil
	}
	since := e.Clock.Wall().Add(-time.Duration(pol.KeepRetryDays) * 24 * time.Hour).UnixNano()
	events, err := e.Store.KeepEventsSince(ctx, since, 500)
	if err != nil {
		return false, err
	}
	binding, err := PlanBindingOf(it)
	if err != nil {
		return false, err
	}
	for _, ev := range events {
		var contract struct {
			ScopeID     string `json:"scope_id"`
			RawRelative string `json:"raw_relative_b64"`
			RuleDigest  string `json:"rule_digest"`
		}
		if ev.ContractJSON != "" {
			if err := json.Unmarshal([]byte(ev.ContractJSON), &contract); err != nil {
				continue
			}
		}
		if contract.ScopeID != "" && contract.ScopeID != string(sc.Scope.ID) {
			continue
		}
		raw, derr := plan.UnB64(contract.RawRelative)
		if derr != nil || string(raw) != string(it.RawRelative) {
			continue
		}
		if contract.RuleDigest != "" && binding.RuleDigest != nil && contract.RuleDigest != *binding.RuleDigest {
			continue
		}
		if ev.ResultJSON == nil {
			continue
		}
		var payload struct {
			Identity   string `json:"identity"`
			Token      string `json:"token"`
			Reason     string `json:"reason"`
			ModelError string `json:"model_error"`
		}
		if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
			continue
		}
		if payload.ModelError != "" || payload.Reason == outcome.ReasonReviewUnavailable {
			// Nothing was decided about this file, so there is nothing to
			// remember. A retry is the whole point of the next run.
			continue
		}
		if payload.Identity == "" || payload.Token == "" {
			continue
		}
		var keptID domain.FileIdentity
		var keptTok domain.ChangeToken
		if json.Unmarshal([]byte(payload.Identity), &keptID) != nil ||
			json.Unmarshal([]byte(payload.Token), &keptTok) != nil {
			continue
		}
		if ok, _ := sameStableObject(preID, preTok, keptID, keptTok); ok {
			return true, nil
		}
	}
	return false, nil
}
