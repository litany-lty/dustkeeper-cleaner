package execute

import (
	"context"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/plan"
)

// ApplyResult summarises one apply.
//
// Control is the strongest control signal the batch produced. A caller must act
// on it instead of reading "error is nil" as "keep going": a held scope used to
// be reported through the error channel only, so a runner that saw nil went on
// to hand the same scope to the next rule.
type ApplyResult struct {
	Plan      domain.PlanID            `json:"plan_id"`
	Status    string                   `json:"status"`
	Planned   int                      `json:"planned_count"`
	Completed int                      `json:"completed_count"`
	Skipped   int                      `json:"skipped_count"`
	Failed    int                      `json:"failed_count"`
	Control   string                   `json:"control"`
	Results   []domain.OperationResult `json:"results"`
	Code      string                   `json:"code"`
	Message   string                   `json:"message,omitempty"`
}

// stopCodes end a batch. They mean the world stopped matching the plan, or
// persistence became unreliable; continuing would compound the problem.
func stopCode(code string) bool {
	switch code {
	case string(errs.CodeHeld),
		string(errs.CodePlanStale),
		string(errs.CodePlanExpired),
		string(errs.CodeScopeChanged),
		string(errs.CodeDurabilityFailed),
		string(errs.CodeIOError),
		string(errs.CodeStateWriteFailed),
		string(errs.CodeDBError),
		string(errs.CodeClockUntrusted),
		string(errs.CodeProtectionAliasUnresolved),
		string(errs.CodeUnsupportedMetadata),
		string(errs.CodePaused):
		return true
	}
	return false
}

// strongerControl keeps the most restrictive signal seen in a batch.
func strongerControl(a, b string) string {
	rank := func(s string) int {
		switch s {
		case domain.ControlStopRun:
			return 3
		case domain.ControlStopScope:
			return 2
		case domain.ControlStopPlan:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// ApplyQuarantinePlan executes an approved quarantine plan.
func (e *Executor) ApplyQuarantinePlan(ctx context.Context, sc *ScopeCtx, planID domain.PlanID,
	approval domain.Digest, budget *domain.Budget) (ApplyResult, error) {

	out := ApplyResult{Plan: planID, Code: string(errs.CodeOK), Control: domain.ControlContinue}

	p, err := e.Store.GetPlan(ctx, planID)
	if err != nil {
		return out, err
	}
	if p.Kind != domain.PlanManualRule && p.Kind != domain.PlanScheduledRule {
		return out, errs.Newf(errs.CodePlanStale,
			"plan %s is a %s plan; this build applies rule plans for quarantine", p.ID, p.Kind)
	}
	if approval != "" && approval != p.Digest {
		return out, errs.Newf(errs.CodeDigestMismatch,
			"the approved digest does not match this plan; nothing was executed")
	}
	if e.Clock.Wall().UnixNano() > p.ExpiresAtNS {
		return out, errs.Newf(errs.CodePlanExpired, "plan %s expired; plan again", p.ID)
	}

	items, err := e.Store.ListItemsByPlan(ctx, planID)
	if err != nil {
		return out, err
	}
	// Whole-plan verification before the first file moves.
	if err := plan.VerifyPlanDigest(p, items, approval); err != nil {
		return out, err
	}
	// The same rule for every member, before the plan is marked applying. A plan
	// that mixes in a historical duplicate is refused as a whole: applying the
	// legal members and stopping at the first duplicate would leave a
	// half-executed plan and would have already run the delete the check exists to
	// prevent.
	if err := requireLifecycleMembers(items); err != nil {
		return out, err
	}
	out.Planned = len(items)

	if err := e.Store.SetPlanStatus(ctx, p.ID, domain.PlanApplying); err != nil {
		return out, err
	}

	stopped := false
	for _, it := range items {
		if stopped {
			out.Failed++
			out.Results = append(out.Results, domain.OperationResult{
				Item: it.ID, State: it.State, Code: string(errs.CodePaused),
				Message: "not started: the batch stopped at an earlier item",
				Control: domain.ControlContinue, Evidence: domain.EvidenceNotStarted,
			})
			continue
		}
		if err := ctx.Err(); err != nil {
			out.Code = string(errs.CodeCancelled)
			out.Control = strongerControl(out.Control, domain.ControlStopRun)
			break
		}
		res, execErr := e.Quarantine(ctx, sc, p, it, budget)
		if execErr != nil {
			out.Failed++
			// The per-item record has to name the reason. Without this the caller
			// sees failed_count rise and every result row carry an empty code, so
			// "which item, and why" is only answerable by re-running the command.
			res = annotateFailure(res, execErr)
			res.Control = strongerControl(res.Control, ControlOf(string(errs.CodeOf(execErr))))
			out.Results = append(out.Results, res)
			code := errs.CodeOf(execErr)
			if stopCode(string(code)) {
				stopped = true
				out.Code = string(code)
				out.Message = execErr.Error()
				out.Control = strongerControl(out.Control, ControlOf(out.Code))
			}
			continue
		}
		out.Results = append(out.Results, res)
		switch res.Code {
		case string(errs.CodeOK):
			out.Completed++
		case string(errs.CodeSkippedProtected), string(errs.CodeUnsupportedMetadata):
			out.Skipped++
		default:
			out.Failed++
		}
	}

	out.Status = summarise(out)
	if err := e.Store.SetPlanStatus(ctx, p.ID, out.Status); err != nil {
		return out, err
	}
	// A specific stop code (PAUSED, HELD, PLAN_STALE, ...) is more useful than
	// the generic PARTIAL: the caller's exit status depends on which condition
	// actually stopped the batch.
	if (out.Status == domain.PlanPartial || out.Status == domain.PlanBlocked) &&
		out.Code == string(errs.CodeOK) {
		out.Code = string(errs.CodePartial)
	}
	return out, nil
}

// ApplyPurgePlan executes an approved purge plan.
//
// Both the generic `plan apply` route and the dedicated `purge apply` route end
// here, so they read the same member list (purge_members) and make the same
// decision for the same object. They used to differ: the generic route loaded
// items by plan id, which is empty for a purge plan, and the dedicated route
// skipped checks the generic one performed.
func (e *Executor) ApplyPurgePlan(ctx context.Context, sc *ScopeCtx, planID domain.PlanID,
	approval domain.Digest, budget *domain.Budget, trust TrustFunc) (ApplyResult, error) {

	out := ApplyResult{Plan: planID, Code: string(errs.CodeOK), Control: domain.ControlContinue}

	p, err := e.Store.GetPlan(ctx, planID)
	if err != nil {
		return out, err
	}
	if p.Kind != domain.PlanManualPurge && p.Kind != domain.PlanScheduledPurge {
		return out, errs.Newf(errs.CodePlanStale, "plan %s is not a purge plan", p.ID)
	}
	if approval != "" && approval != p.Digest {
		return out, errs.Newf(errs.CodeDigestMismatch, "the approved digest does not match this purge plan")
	}
	if e.Clock.Wall().UnixNano() > p.ExpiresAtNS {
		return out, errs.Newf(errs.CodePlanExpired, "plan %s expired; plan again", p.ID)
	}

	members, err := e.Store.ListPurgeMembers(ctx, planID)
	if err != nil {
		return out, err
	}
	out.Planned = len(members)
	// A purge plan is refused as a whole before the plan is marked applying. The
	// historical duplicate rows are still loadable through `plan apply`, and a
	// member list that contains one is not a list this build may act on.
	if err := requireLifecycleMembers(purgeMemberItems(members)); err != nil {
		return out, err
	}
	if err := e.Store.SetPlanStatus(ctx, p.ID, domain.PlanApplying); err != nil {
		return out, err
	}

	stopped := false
	for _, m := range members {
		if stopped {
			out.Failed++
			out.Results = append(out.Results, domain.OperationResult{
				Item: m.Item.ID, State: m.Item.State, Code: string(errs.CodePaused),
				Message: "not started: the batch stopped at an earlier item",
				Control: domain.ControlContinue, Evidence: domain.EvidenceNotStarted,
			})
			continue
		}
		if err := ctx.Err(); err != nil {
			out.Code = string(errs.CodeCancelled)
			out.Control = strongerControl(out.Control, domain.ControlStopRun)
			break
		}
		// The plan's own window is re-checked per item: a batch that was valid
		// when it started must not still be executing an approval that expired
		// halfway through a large vault.
		if now := e.Clock.Wall().UnixNano(); now > p.ExpiresAtNS {
			out.Code = string(errs.CodePlanExpired)
			out.Message = "the plan expired during the batch; no further object was removed"
			out.Control = strongerControl(out.Control, domain.ControlStopPlan)
			stopped = true
			out.Failed++
			continue
		}
		res, execErr := e.Purge(ctx, sc, m.Item, PurgeOptions{
			Automatic: p.Kind == domain.PlanScheduledPurge,
			Plan:      &p,
			Budget:    budget,
			Trust:     trust,
		})
		if execErr != nil {
			out.Failed++
			res = annotateFailure(res, execErr)
			res.Control = strongerControl(res.Control, ControlOf(string(errs.CodeOf(execErr))))
			out.Results = append(out.Results, res)
			code := errs.CodeOf(execErr)
			if stopCode(string(code)) {
				stopped = true
				out.Code = string(code)
				out.Message = execErr.Error()
				out.Control = strongerControl(out.Control, ControlOf(out.Code))
			}
			continue
		}
		out.Results = append(out.Results, res)
		if res.Code == string(errs.CodeOK) {
			out.Completed++
		} else {
			out.Failed++
		}
	}

	out.Status = summarise(out)
	if err := e.Store.SetPlanStatus(ctx, p.ID, out.Status); err != nil {
		return out, err
	}
	// A specific stop code (PAUSED, HELD, PLAN_STALE, ...) is more useful than
	// the generic PARTIAL: the caller's exit status depends on which condition
	// actually stopped the batch.
	if (out.Status == domain.PlanPartial || out.Status == domain.PlanBlocked) &&
		out.Code == string(errs.CodeOK) {
		out.Code = string(errs.CodePartial)
	}
	return out, nil
}

// annotateFailure puts the reason on the item's own record.
//
// The batch already carries a code, but only for the condition that stopped it.
// A failed item used to be appended with an empty code, so a script reading
// results could not tell a stale plan from a full disk from a held scope.
func annotateFailure(res domain.OperationResult, err error) domain.OperationResult {
	if res.Code == "" {
		res.Code = string(errs.CodeOf(err))
	}
	if res.Message == "" {
		res.Message = err.Error()
	}
	if res.Control == "" {
		res.Control = ControlOf(res.Code)
	}
	return res
}

func summarise(r ApplyResult) string {
	switch {
	case r.Failed == 0 && r.Completed+r.Skipped == r.Planned:
		return domain.PlanComplete
	case r.Completed == 0 && r.Failed > 0:
		return domain.PlanBlocked
	default:
		return domain.PlanPartial
	}
}
