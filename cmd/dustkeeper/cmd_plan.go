package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/store"
)

// cmdPlan routes plan create/show/apply.
func cmdPlan(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "plan needs a subcommand: create, show, apply")
	}
	switch args[0] {
	case "create":
		return planCreate(ctx, a, args[1:])
	case "show":
		return planShow(ctx, a, args[1:])
	case "apply":
		return planApply(ctx, a, args[1:])
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown plan subcommand %q", args[0])
}

// planCreate freezes candidates into a concrete, expiring plan.
func planCreate(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("plan create", io.Discard)
	id := fs.String("rule", "", "rule ID")
	rev := fs.Int64("revision", 0, "rule revision; 0 means the active one")
	maxFiles := fs.Int("max-files", 0, "how many objects to plan")
	maxBytes := fs.Int64("max-bytes", 0, "logical byte ceiling")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--rule is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	var res plan.RulePlanResult
	out, err := a.withInstance(func() (output, error) {
		r, err := ruleAt(ctx, a.store, *id, *rev)
		if err != nil {
			return output{}, err
		}
		sc, err := resolveScope(ctx, a.store, string(r.ScopeID))
		if err != nil {
			return output{}, err
		}
		parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
		if err != nil {
			return output{}, err
		}
		files, bytes := *maxFiles, *maxBytes
		if files <= 0 {
			files = a.cfg.Runtime.MaxAutoQuarantineFiles
		}
		if bytes <= 0 {
			bytes = a.cfg.Runtime.MaxAutoQuarantineBytes
		}
		if files <= 0 || bytes <= 0 {
			return output{}, errs.New(errs.CodeInvalidArgument,
				"the configured action ceiling is zero, which forbids quarantine; pass --max-files and --max-bytes")
		}

		sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), sc.ID)
		if err != nil {
			return output{}, err
		}
		defer sctx.Close()
		guard, err := guardFor(ctx, a.store, sctx)
		if err != nil {
			return output{}, err
		}

		planner := plan.Planner{Store: a.store, Clock: a.clock}
		var perr error
		res, perr = planner.CreateRulePlan(ctx, plan.RulePlanRequest{
			Scope:    sc,
			Rule:     parsed,
			RuleRef:  domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
			Kind:     domain.PlanManualRule,
			Guard:    guard,
			MaxFiles: files,
			MaxBytes: bytes,
			Config:   a.cfg,
			NowNS:    a.clock.Wall().UnixNano(),
		})
		if perr != nil {
			return output{}, perr
		}

		o := ok()
		o.ScopeID = string(sc.ID)
		o.PlanID = string(res.Plan.ID)
		o.Counts = &counts{Planned: len(res.Items)}
		o.Data = map[string]any{
			"plan_id":       string(res.Plan.ID),
			"digest":        string(res.Plan.Digest),
			"kind":          res.Plan.Kind,
			"expires_at_ns": res.Plan.ExpiresAtNS,
			"matched":       len(res.Items),
			"logical_bytes": res.Bytes,
			"truncated":     res.Truncated,
			"reasons":       res.Reasons,
			"items":         previewItems(res.Items),
		}
		o.Human = []string{
			fmt.Sprintf("plan %s frozen: %d object(s), %d bytes", res.Plan.ID, len(res.Items), res.Bytes),
			fmt.Sprintf("  expires: %s", time.Unix(0, res.Plan.ExpiresAtNS).Format(time.RFC3339)),
		}
		for _, it := range res.Items {
			o.Human = append(o.Human, "  "+string(it.ID)+"  "+string(it.RawRelative))
		}
		o.Human = append(o.Human, "", "plan digest (repeat it to apply):", "  "+string(res.Plan.Digest))
		o.Next = []string{
			fmt.Sprintf("dustkeeper plan apply --id %s --digest %s", res.Plan.ID, res.Plan.Digest),
		}
		return o, nil
	})
	return out, err
}

func planShow(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("plan show", io.Discard)
	id := fs.String("id", "", "plan ID")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	p, err := a.store.GetPlan(ctx, domain.PlanID(*id))
	if err != nil {
		return output{}, err
	}
	items, err := a.store.ListItemsByPlan(ctx, p.ID)
	if err != nil {
		return output{}, err
	}

	// Recompute the digest so a plan edited outside this program is visible.
	digestNote := "recomputed digest matches the stored one"
	if verr := plan.VerifyPlanDigest(p, items, ""); verr != nil {
		digestNote = "WARNING: " + messageOf(verr)
	}

	var members []store.PurgeMemberRow
	if p.Kind == domain.PlanManualPurge || p.Kind == domain.PlanScheduledPurge {
		// A purge plan has no rows of its own: its members are existing
		// quarantined objects. The digest must be rebuilt from those.
		members, err = a.store.ListPurgeMembers(ctx, p.ID)
		if err != nil {
			return output{}, err
		}
		items = items[:0]
		for _, m := range members {
			items = append(items, m.Item)
		}
		digestNote = "recomputed digest matches the stored one"
		if verr := plan.VerifyPlanDigest(p, items, ""); verr != nil {
			digestNote = "WARNING: " + messageOf(verr)
		}
	}

	out := ok()
	out.PlanID = string(p.ID)
	out.Data = map[string]any{
		"plan":        p,
		"items":       previewItems(items),
		"count":       len(items),
		"members":     members,
		"digest_note": digestNote,
	}
	out.Human = []string{
		fmt.Sprintf("plan %s (%s) %s", p.ID, p.Kind, p.Status),
		fmt.Sprintf("  created: %s   expires: %s",
			time.Unix(0, p.CreatedAtNS).Format(time.RFC3339),
			time.Unix(0, p.ExpiresAtNS).Format(time.RFC3339)),
		"  digest:  " + string(p.Digest),
		"  " + digestNote,
	}
	for _, it := range items {
		out.Human = append(out.Human, fmt.Sprintf("  %-28s %-24s %s", it.ID, it.State, string(it.RawRelative)))
	}
	if p.Status == domain.PlanReady {
		out.Next = []string{
			fmt.Sprintf("dustkeeper plan apply --id %s --digest %s", p.ID, p.Digest),
		}
	}
	return out, nil
}

// planApply executes one approved, unexpired plan.
func planApply(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("plan apply", io.Discard)
	id := fs.String("id", "", "plan ID")
	digest := fs.String("digest", "", "the full plan digest that was reviewed")
	writersStopped := fs.Bool("writers-stopped", false, "confirm that writers of the scope were stopped")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" || *digest == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id and --digest are required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	return a.withInstance(func() (output, error) {
		p, err := a.store.GetPlan(ctx, domain.PlanID(*id))
		if err != nil {
			return output{}, err
		}
		if err := checkPlanUsable(p, a.clock.Wall().UnixNano(), a.store, ctx); err != nil {
			return output{}, err
		}
		items, err := plan.LoadMembers(ctx, a.store, p, domain.Digest(*digest))
		if err != nil {
			return output{}, err
		}

		sc, err := a.store.GetScope(ctx, items[0].ScopeID)
		if err != nil {
			return output{}, err
		}
		// Section 2.1: an assisted scope is only ever changed while the producer
		// is stopped, and that is an operator statement the program cannot make
		// for itself.
		if !*writersStopped && sc.Kind == domain.KindAssisted {
			return output{}, errs.New(errs.CodeInvalidArgument,
				"this plan changes files in an assisted scope; pass --writers-stopped to confirm that the producers are stopped and the directory is quiet")
		}
		sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), sc.ID)
		if err != nil {
			return output{}, err
		}
		defer sctx.Close()
		guard, err := guardFor(ctx, a.store, sctx)
		if err != nil {
			return output{}, err
		}

		exec := a.executor()
		exec.Guard = guard
		budget := domain.NewBudget(a.cfg.Runtime.RunSoftSeconds, a.cfg.Runtime.RunReadBudgetBytes,
			len(items), planBytes(items), a.cfg.Runtime.ReadRateBytesPerSecond)

		var res execute.ApplyResult
		switch p.Kind {
		case domain.PlanManualPurge, domain.PlanScheduledPurge:
			// Retention is only provable if the clock is. Section 10.4 lists a
			// trustworthy clock among the per-purge conditions, and a manual
			// approval does not make a jumped clock trustworthy.
			//
			// The assertion is passed as a function so the executor re-evaluates
			// it for every object, not once per batch: a clock that jumps
			// halfway through must stop the batch, not finish it.
			trust := clocktrust.Start(a.store, a.clock)
			res, err = exec.ApplyPurgePlan(ctx, sctx, p.ID, domain.Digest(*digest), budget, trust.Assert)
		default:
			res, err = exec.ApplyQuarantinePlan(ctx, sctx, p.ID, domain.Digest(*digest), budget)
		}
		if err != nil {
			return output{}, err
		}

		o := ok()
		o.ScopeID = string(sc.ID)
		o.PlanID = string(p.ID)
		o.Counts = &counts{
			Planned:   res.Planned,
			Completed: res.Completed,
			Skipped:   res.Skipped,
			Failed:    res.Failed,
		}
		o.Data = res
		o.Human = []string{
			fmt.Sprintf("plan %s: %d of %d completed, %d skipped, %d failed",
				p.ID, res.Completed, res.Planned, res.Skipped, res.Failed),
		}
		for _, r := range res.Results {
			if r.Code == string(errs.CodeOK) {
				continue
			}
			o.Human = append(o.Human, fmt.Sprintf("  %s  %s  %s", r.Item, r.Code, r.Message))
		}
		if res.Code != string(errs.CodeOK) {
			o.Code = errs.Code(res.Code)
			o.Status = string(errs.StatusOf(o.Code))
			o.Message = res.Message
		}
		if res.Skipped > 0 || res.Failed > 0 {
			o.Next = []string{"dustkeeper recover inspect"}
		} else {
			o.Next = []string{"dustkeeper status"}
		}
		return o, nil
	})
}

// checkPlanUsable rejects an expired or already-consumed plan before any file is
// touched.
func checkPlanUsable(p store.Plan, nowNS int64, st *store.Store, ctx context.Context) error {
	switch p.Status {
	case domain.PlanReady, domain.PlanPartial:
	default:
		return errs.Newf(errs.CodePlanNotReady, "plan %s is %s", p.ID, p.Status)
	}
	if nowNS > p.ExpiresAtNS {
		_ = st.SetPlanStatus(ctx, p.ID, domain.PlanExpired)
		return errs.Newf(errs.CodePlanExpired,
			"plan %s expired at %s; create a new plan so you review what is actually there",
			p.ID, time.Unix(0, p.ExpiresAtNS).Format(time.RFC3339))
	}
	return nil
}

func planBytes(items []store.Item) int64 {
	var total int64
	for _, it := range items {
		var tok domain.ChangeToken
		if err := jsonUnmarshal([]byte(it.PreTokenJSON), &tok); err == nil {
			total += tok.Size
		}
	}
	if total <= 0 {
		return 1
	}
	return total
}

