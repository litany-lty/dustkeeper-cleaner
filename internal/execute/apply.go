package execute

import (
	"context"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/plan"
)

// ApplyResult summarises one apply.
type ApplyResult struct {
	Plan      domain.PlanID            `json:"plan_id"`
	Status    string                   `json:"status"`
	Planned   int                      `json:"planned_count"`
	Completed int                      `json:"completed_count"`
	Skipped   int                      `json:"skipped_count"`
	Failed    int                      `json:"failed_count"`
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
		string(errs.CodePaused):
		return true
	}
	return false
}

// ApplyQuarantinePlan executes an approved quarantine plan.
func (e *Executor) ApplyQuarantinePlan(ctx context.Context, sc *ScopeCtx, planID domain.PlanID,
	approval domain.Digest, budget *domain.Budget) (ApplyResult, error) {

	out := ApplyResult{Plan: planID, Code: string(errs.CodeOK)}

	p, err := e.Store.GetPlan(ctx, planID)
	if err != nil {
		return out, err
	}
	if p.Kind != domain.PlanManualRule && p.Kind != domain.PlanScheduledRule && p.Kind != domain.PlanManualDuplicate {
		return out, errs.Newf(errs.CodePlanStale, "plan %s is a %s plan, not a quarantine plan", p.ID, p.Kind)
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
	out.Planned = len(items)

	if err := e.Store.SetPlanStatus(ctx, p.ID, domain.PlanApplying); err != nil {
		return out, err
	}

	stopped := false
	for _, it := range items {
		if stopped {
			out.Failed++
			continue
		}
		if err := ctx.Err(); err != nil {
			out.Code = string(errs.CodeCancelled)
			break
		}
		res, execErr := e.Quarantine(ctx, sc, p, it, budget)
		if execErr != nil {
			out.Failed++
			out.Results = append(out.Results, res)
			code := errs.CodeOf(execErr)
			if stopCode(string(code)) {
				stopped = true
				out.Code = string(code)
				out.Message = execErr.Error()
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

// ApplyPurgePlan executes an approved purge plan over existing objects.
func (e *Executor) ApplyPurgePlan(ctx context.Context, sc *ScopeCtx, planID domain.PlanID,
	approval domain.Digest, budget *domain.Budget, clockTrusted bool) (ApplyResult, error) {

	out := ApplyResult{Plan: planID, Code: string(errs.CodeOK)}

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
	if err := e.Store.SetPlanStatus(ctx, p.ID, domain.PlanApplying); err != nil {
		return out, err
	}

	stopped := false
	for _, m := range members {
		if stopped {
			out.Failed++
			continue
		}
		if err := ctx.Err(); err != nil {
			out.Code = string(errs.CodeCancelled)
			break
		}
		res, execErr := e.Purge(ctx, sc, m.Item, PurgeOptions{
			Automatic:    p.Kind == domain.PlanScheduledPurge,
			Plan:         &p,
			Budget:       budget,
			ClockTrusted: clockTrusted,
		})
		if execErr != nil {
			out.Failed++
			out.Results = append(out.Results, res)
			code := errs.CodeOf(execErr)
			if stopCode(string(code)) {
				stopped = true
				out.Code = string(code)
				out.Message = execErr.Error()
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
