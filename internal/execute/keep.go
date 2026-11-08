package execute

import (
	"context"
	"encoding/json"
	"time"

	"strconv"

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

	payload := reviewPayload(reviewed, it)
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
	switch reviewed.Reason {
	case outcome.ReasonNotStartedBudget:
		// The allowance stopped the dispatch. Nothing was sent, so this is not a
		// provider outage and must not be reported as one: the remaining work of
		// this batch simply has not started.
		res.Code = string(errs.CodeBudgetExhausted)
		res.Message = "review was not started: " + reviewed.Decision.ModelError
		res.ReviewState = domain.ReviewNotStartedBudget
		res.Control = domain.ControlStopScope
		if reviewed.Decision.Failure == outcome.ReviewFailureRunBudget {
			res.Control = domain.ControlStopRun
		}
		return res, errs.New(errs.CodeBudgetExhausted, res.Message)
	case outcome.ReasonReviewUnknown:
		// A request was made and its result is unknown. The file stays, and the
		// cost stays unknown: it is not zero, and this run does not ask again.
		res.Code = string(errs.CodeModelUnavailable)
		res.Message = "the review request was sent and no result was received; the file was not touched and the request cost is unknown: " + reviewed.Decision.ModelError
		res.ReviewState = domain.ReviewUnknownResult
		res.Control = domain.ControlStopScope
		return res, errs.Newf(errs.CodeModelUnavailable,
			"review unanswered after %d request(s); no file was moved or deleted", reviewed.Decision.Attempts)
	case outcome.ReasonRequestCheckHold:
		// An earlier request for this object and action is still unresolved.
		// Asking again would pay twice for the same question.
		res.Code = string(errs.CodeKept)
		res.Message = outcome.ReasonText(reviewed.Reason)
		res.ReviewState = domain.ReviewUnknownHold
		res.Control = domain.ControlContinue
		return res, nil
	case outcome.ReasonReviewUnavailable:
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

// reviewFactsJSON is the request half of one decision. It is written with every
// keep so that "was anything actually sent" is answerable from the record
// instead of being inferred from the verdict.
func reviewFactsJSON(d review.Decision) string {
	return `{"failure":` + quoteJSON(d.Failure) +
		`,"http_attempts":` + strconv.Itoa(d.Attempts) +
		`,"jev_sent":` + strconv.FormatBool(d.JEVSent) +
		`,"minimax_sent":` + strconv.FormatBool(d.LLMSent) +
		`,"minimax_consulted":` + strconv.FormatBool(d.LLMConsulted) +
		`,"cost_status":` + quoteJSON(d.CostStatus) + `}`
}

// noteUnknownRequest persists the request that was sent and never answered.
//
// It is written before the keep record so a crash between the two loses the
// decision but not the possibility that the request was billed. The next run
// reads this row and does not ask again until a person resolves it.
func (e *Executor) noteUnknownRequest(ctx context.Context, sc *ScopeCtx, it store.Item, action string,
	d review.Decision, identity, token string) error {
	if d.Attempts <= 0 {
		return nil
	}
	return e.Store.RecordReviewUnknown(ctx, store.ReviewUnknown{
		ItemID:       it.ID,
		Action:       action,
		ScopeID:      string(sc.Scope.ID),
		PathB64:      plan.B64(it.RawRelative),
		IdentityJSON: identity,
		TokenJSON:    token,
		CreatedNS:    e.Clock.Wall().UnixNano(),
		Attempts:     d.Attempts,
		Failure:      d.Failure,
		CostStatus:   d.CostStatus,
	})
}

// heldUnknownRequest reports whether an unresolved request exists for exactly
// these bytes. It checks both the unanswered-request table and a pending
// dispatch row that was prepared but never settled - a process that died
// between sending the first request and recording its answer leaves the second
// kind behind. OpenDispatchFor excludes a pending dispatch that already has a
// manual resolution, so `review dispatch resolve` lifts exactly that one
// request's hold: other pending dispatches, other actions and review_unknown
// rows keep their own independent hold. No logic here needs to change for that.
// A file that has changed since is a different question, so the old row does
// not suppress a new one - it stays on record either way.
func (e *Executor) heldUnknownRequest(ctx context.Context, sc *ScopeCtx, it store.Item,
	action, identity, token string) (bool, error) {
	rows, err := e.Store.OpenReviewUnknown(ctx, string(sc.Scope.ID), plan.B64(it.RawRelative), action)
	if err != nil {
		return false, err
	}
	for _, u := range rows {
		if u.IdentityJSON == identity && u.TokenJSON == token {
			return true, nil
		}
	}
	pending, err := e.Store.OpenDispatchFor(ctx, string(it.ID), action, identity, token)
	if err != nil {
		return false, err
	}
	return len(pending) > 0, nil
}

// recordReviewInfo writes an item-scoped review fact that is neither a decision
// nor an action: a batch that never started, or a standing unresolved request.
func (e *Executor) recordReviewInfo(ctx context.Context, sc *ScopeCtx, planID *domain.PlanID, it store.Item,
	action, eventAction, reason string, d review.Decision) error {
	ruleDigest := ""
	if binding, err := PlanBindingOf(it); err == nil && binding.RuleDigest != nil {
		ruleDigest = *binding.RuleDigest
	}
	payload := `{"source":"review_state","action":` + quoteJSON(action) +
		`,"reason":` + quoteJSON(reason) +
		`,"reason_text":` + quoteJSON(outcome.ReasonText(reason)) +
		`,"model_error":` + quoteJSON(d.ModelError) +
		`,"review":` + reviewFactsJSON(d) + `}`
	return e.Store.AppendEvent(ctx, store.Event{
		EventID:         domain.NewID("ev"),
		ItemID:          &it.ID,
		ExecutionPlanID: planID,
		Action:          eventAction,
		Phase:           domain.PhaseInfo,
		OccurredAtNS:    e.Clock.Wall().UnixNano(),
		ContractJSON: `{"scope_id":` + quoteJSON(string(sc.Scope.ID)) +
			`,"raw_relative_b64":` + quoteJSON(plan.B64(it.RawRelative)) +
			`,"rule_digest":` + quoteJSON(ruleDigest) + `}`,
		ResultJSON: &payload,
	})
}

// keepRecord is the payload of a keep, kept in one place so a new field cannot
// be added to one branch and forgotten in the other.
func reviewPayload(reviewed review.Outcome, it store.Item) string {
	// The correlation id is present only when a request was actually prepared.
	// A local refusal or a standing hold has no review to join, and writing an
	// invented id there would let a result page claim a request that never was.
	correlation := ""
	if reviewed.ReviewID != "" {
		correlation = `,"review_id":` + quoteJSON(reviewed.ReviewID)
	}
	return `{"source":"decision","disposition":` + quoteJSON(domain.DispositionKeep) +
		`,"reason":` + quoteJSON(reviewed.Reason) +
		`,"reason_text":` + quoteJSON(outcome.ReasonText(reviewed.Reason)) +
		`,"verdict":` + quoteJSON(reviewed.Decision.Verdict) +
		`,"model_error":` + quoteJSON(reviewed.Decision.ModelError) +
		`,"review":` + reviewFactsJSON(reviewed.Decision) + correlation +
		// The version this decision was about. Without it a later run can only
		// ask "was this path kept", which would also silence a different file
		// that has since been created at the same place.
		`,"identity":` + quoteJSON(it.PreIdentityJSON) +
		`,"token":` + quoteJSON(it.PreTokenJSON) + `}`
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
