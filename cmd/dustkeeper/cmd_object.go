package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/dedup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/recovery"
	"dustkeeper/internal/rescue"
	"dustkeeper/internal/store"
)

// cmdQuarantine routes quarantine list/restore.
func cmdQuarantine(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "quarantine needs a subcommand: list, restore")
	}
	switch args[0] {
	case "list":
		return quarantineList(ctx, a, args[1:])
	case "restore":
		return quarantineRestore(ctx, a, args[1:])
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown quarantine subcommand %q", args[0])
}

func quarantineList(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("quarantine list", io.Discard)
	scopeRef := fs.String("scope", "", "limit to one scope")
	limit := fs.Int("limit", 200, "maximum objects to list")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	nowNS := a.clock.Wall().UnixNano()
	type row struct {
		ItemID        string `json:"item_id"`
		ScopeID       string `json:"scope_id"`
		RelativePath  string `json:"relative_path"`
		ObjectName    string `json:"object_name"`
		Reason        string `json:"reason"`
		Retention     int64  `json:"bound_retention_seconds"`
		QuarantinedAt int64  `json:"quarantined_at_ns"`
		DueAt         int64  `json:"due_at_ns"`
		Due           bool   `json:"due"`
		AutoPurgeOK   bool   `json:"auto_purge_granted"`
	}
	var rows []row

	collect := func(sc store.Scope) error {
		items, err := a.store.ListItemsByState(ctx, sc.ID, domain.StateQuarantined, *limit)
		if err != nil {
			return err
		}
		for _, it := range items {
			dueAt := int64(0)
			if it.QuarantinedAtNS != nil && it.BoundRetentionSeconds > 0 {
				dueAt = *it.QuarantinedAtNS + it.BoundRetentionSeconds*int64(time.Second)
			}
			rows = append(rows, row{
				ItemID:        string(it.ID),
				ScopeID:       string(it.ScopeID),
				RelativePath:  string(it.RawRelative),
				ObjectName:    it.ObjectName,
				Reason:        it.Reason,
				Retention:     it.BoundRetentionSeconds,
				QuarantinedAt: ptrVal(it.QuarantinedAtNS),
				DueAt:         dueAt,
				Due:           dueAt > 0 && nowNS >= dueAt,
				AutoPurgeOK:   it.AutoPurgeGrantID != nil,
			})
		}
		return nil
	}

	if *scopeRef != "" {
		sc, err := resolveScope(ctx, a.store, *scopeRef)
		if err != nil {
			return output{}, err
		}
		if err := collect(sc); err != nil {
			return output{}, err
		}
	} else {
		scopes, err := a.store.ListScopes(ctx)
		if err != nil {
			return output{}, err
		}
		for _, sc := range scopes {
			if err := collect(sc); err != nil {
				return output{}, err
			}
		}
	}

	out := ok()
	out.Data = map[string]any{"objects": rows, "count": len(rows)}
	if len(rows) == 0 {
		out.Human = append(out.Human, "no quarantined objects")
	}
	for _, r := range rows {
		state := "recoverable"
		if r.Due {
			state = "due for purge"
		}
		out.Human = append(out.Human, fmt.Sprintf("%-28s %-10s %-20s %s",
			r.ItemID, state, r.ScopeID, r.RelativePath))
	}
	if len(rows) > 0 {
		out.Human = append(out.Human, "", "restore one with: dustkeeper quarantine restore --object <item-id>")
		out.Next = []string{"dustkeeper quarantine restore --object <item-id>"}
	}
	return out, nil
}

func ptrVal(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// quarantineRestore returns one object without overwriting anything.
func quarantineRestore(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("quarantine restore", io.Discard)
	object := fs.String("object", "", "item ID")
	dest := fs.String("destination-relative", "", "restore to this path inside the scope instead of the original")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *object == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--object is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	return a.withInstance(func() (output, error) {
		it, err := a.store.GetItem(ctx, domain.ItemID(*object))
		if err != nil {
			return output{}, err
		}
		if it.State != domain.StateQuarantined {
			return output{}, errs.Newf(errs.CodeItemNotFound,
				"object %s is %s, not quarantined", it.ID, it.State)
		}
		sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), it.ScopeID)
		if err != nil {
			return output{}, err
		}
		defer sctx.Close()

		exec := a.executor(a.detector())
		opt := execute.RestoreOptions{}
		if *dest != "" {
			opt.DestinationRelative = []byte(*dest)
		}

		preview, occupied, err := exec.RestoreDestinationPreview(ctx, sctx, it.ID, opt.DestinationRelative)
		if err != nil {
			return output{}, err
		}
		if occupied {
			return output{}, errs.Newf(errs.CodeRestoreConflict,
				"%s already exists; the quarantined object was left alone. Choose another destination with --destination-relative",
				preview)
		}

		res, err := exec.Restore(ctx, sctx, it.ID, opt)
		if err != nil {
			return output{}, err
		}

		out := ok()
		out.ScopeID = string(it.ScopeID)
		out.ObjectID = string(it.ID)
		out.Data = map[string]any{
			"result":    res,
			"restored":  preview,
			"protected": true,
		}
		out.Human = []string{
			fmt.Sprintf("restored %s to %s", it.ID, preview),
			"  the original path and the actual path are now protected",
			"  protection does not expire; remove it explicitly with `protect remove`",
		}
		out.Next = []string{fmt.Sprintf("dustkeeper protect list --scope %s", it.ScopeID)}
		return out, nil
	})
}

// cmdProtect routes protect add/list/remove.
func cmdProtect(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "protect needs a subcommand: add, list, remove")
	}
	switch args[0] {
	case "add":
		return protectAdd(ctx, a, args[1:])
	case "list":
		return protectList(ctx, a, args[1:])
	case "remove":
		return protectRemove(ctx, a, args[1:])
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown protect subcommand %q", args[0])
}

func protectAdd(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("protect add", io.Discard)
	scopeRef := fs.String("scope", "", "scope ID")
	rel := fs.String("relative", "", "path relative to the scope root")
	subtree := fs.Bool("subtree", false, "protect the whole subtree, not just this path")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *scopeRef == "" || *rel == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--scope and --relative are required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	sc, err := resolveScope(ctx, a.store, *scopeRef)
	if err != nil {
		return output{}, err
	}
	match := domain.MatchExact
	if *subtree {
		match = domain.MatchSubtree
	}
	prot := store.Protection{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.ID,
		RawRelative:  []byte(*rel),
		MatchKind:    match,
		Source:       domain.ProtectUser,
		EvidenceJSON: `{"source":"user"}`,
		Active:       true,
		CreatedAtNS:  a.clock.Wall().UnixNano(),
	}
	rev, err := a.store.InsertProtection(ctx, prot)
	if err != nil {
		return output{}, err
	}
	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"protection_id":       string(prot.ID),
		"match_kind":          match,
		"protection_revision": rev,
	}
	out.Human = []string{
		fmt.Sprintf("protecting %q (%s) in scope %s", *rel, match, sc.ID),
		fmt.Sprintf("  protection revision is now %d; existing rules must be re-previewed", rev),
	}
	return out, nil
}

func protectList(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("protect list", io.Discard)
	scopeRef := fs.String("scope", "", "limit to one scope")
	all := fs.Bool("all", false, "include inactive protections")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	var prot []store.Protection
	if *scopeRef != "" {
		sc, err := resolveScope(ctx, a.store, *scopeRef)
		if err != nil {
			return output{}, err
		}
		prot, err = a.store.ListProtections(ctx, sc.ID, !*all)
		if err != nil {
			return output{}, err
		}
	} else {
		scopes, err := a.store.ListScopes(ctx)
		if err != nil {
			return output{}, err
		}
		for _, sc := range scopes {
			ps, err := a.store.ListProtections(ctx, sc.ID, !*all)
			if err != nil {
				return output{}, err
			}
			prot = append(prot, ps...)
		}
	}
	out := ok()
	out.Data = map[string]any{"protections": prot, "count": len(prot)}
	if len(prot) == 0 {
		out.Human = append(out.Human, "no path protections")
	}
	for _, p := range prot {
		out.Human = append(out.Human, fmt.Sprintf("%-24s %-18s %-8s %-12s %s",
			p.ID, p.ScopeID, p.MatchKind, p.Source, string(p.RawRelative)))
	}
	return out, nil
}

func protectRemove(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("protect remove", io.Discard)
	id := fs.String("id", "", "protection ID")
	scopeRef := fs.String("scope", "", "scope ID, with --relative")
	rel := fs.String("relative", "", "path relative to the scope root")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	out := ok()
	switch {
	case *id != "":
		if err := a.store.DeactivateProtection(ctx, domain.ProtectionID(*id)); err != nil {
			return output{}, err
		}
		out.Data = map[string]any{"protection_id": *id, "active": false}
		out.Human = []string{"protection " + *id + " deactivated"}
	case *scopeRef != "" && *rel != "":
		sc, err := resolveScope(ctx, a.store, *scopeRef)
		if err != nil {
			return output{}, err
		}
		n, err := a.store.DeactivateProtectionByPath(ctx, sc.ID, []byte(*rel))
		if err != nil {
			return output{}, err
		}
		out.ScopeID = string(sc.ID)
		out.Data = map[string]any{"deactivated": n, "relative_path": *rel}
		out.Human = []string{fmt.Sprintf("deactivated %d protection(s) for %q", n, *rel)}
	default:
		return output{}, errs.New(errs.CodeInvalidArgument, "pass --id, or --scope with --relative")
	}
	out.Human = append(out.Human, "  removing a protection is a deliberate act; it does not quarantine anything by itself")
	return out, nil
}

// cmdPurge routes purge plan/show/apply.
func cmdPurge(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "purge needs a subcommand: plan, show, apply")
	}
	switch args[0] {
	case "plan":
		return purgePlan(ctx, a, args[1:])
	case "show":
		return planShow(ctx, a, args[1:])
	case "apply":
		return purgeApply(ctx, a, args[1:])
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown purge subcommand %q", args[0])
}

// purgePlan builds a separate plan for objects whose retention has elapsed.
func purgePlan(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("purge plan", io.Discard)
	scopeRef := fs.String("scope", "", "limit to one scope")
	limit := fs.Int("limit", 0, "maximum objects (defaults to the configured ceiling)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	return a.withInstance(func() (output, error) {
		nowNS := a.clock.Wall().UnixNano()
		var scopes []store.Scope
		if *scopeRef != "" {
			sc, err := resolveScope(ctx, a.store, *scopeRef)
			if err != nil {
				return output{}, err
			}
			scopes = []store.Scope{sc}
		} else {
			all, err := a.store.ListScopes(ctx)
			if err != nil {
				return output{}, err
			}
			scopes = all
		}

		maxItems := *limit
		if maxItems <= 0 {
			maxItems = a.cfg.Runtime.MaxAutoPurgeFiles
		}
		if maxItems <= 0 {
			maxItems = 1000
		}

		var selected []store.Item
		blocked := 0
		chosen := store.Scope{}
		withDue := 0
		for _, sc := range scopes {
			due, err := a.store.ItemsDueForPurge(ctx, sc.ID, nowNS, maxItems)
			if err != nil {
				return output{}, err
			}
			if len(due) == 0 {
				continue
			}
			withDue++
			chosen = sc
			for _, it := range due {
				// Every purgeable object must carry the retention bound and the
				// captured evidence from its quarantine. Anything else is not
				// silently skipped: it is reported.
				if it.CapturedIdentityJSON == nil || it.ContentHash == nil || it.QuarantinedAtNS == nil {
					blocked++
					continue
				}
				selected = append(selected, it)
			}
		}
		// A purge plan binds exactly one scope: its digest covers that scope's
		// identity. Mixing scopes into one plan would make the binding a lie.
		if withDue > 1 {
			return output{}, errs.Newf(errs.CodeInvalidArgument,
				"%d scopes have objects due for purge; pass --scope to choose one", withDue)
		}
		if len(selected) == 0 {
			out := ok()
			out.Code = errs.CodeNoCandidates
			out.Data = map[string]any{"due": 0, "blocked": blocked}
			out.Human = []string{"no object is due for permanent deletion"}
			if blocked > 0 {
				out.Human = append(out.Human, fmt.Sprintf("  %d due object(s) lack complete evidence and stay recoverable", blocked))
			}
			return out, nil
		}

		sc := chosen
		p, err := plan.CreatePurgePlan(ctx, a.store, sc, selected, domain.PlanManualPurge, nowNS, int64(plan.DefaultTTL))
		if err != nil {
			return output{}, err
		}

		out := ok()
		out.ScopeID = string(selected[0].ScopeID)
		out.PlanID = string(p.ID)
		out.Counts = &counts{Planned: len(selected)}
		out.Data = map[string]any{
			"plan_id":       string(p.ID),
			"digest":        string(p.Digest),
			"objects":       len(selected),
			"blocked":       blocked,
			"expires_at_ns": p.ExpiresAtNS,
			"items":         previewItems(selected),
		}
		out.Human = []string{
			fmt.Sprintf("purge plan %s covers %d expired object(s)", p.ID, len(selected)),
			fmt.Sprintf("  expires: %s", time.Unix(0, p.ExpiresAtNS).Format(time.RFC3339)),
		}
		for _, it := range selected {
			out.Human = append(out.Human, fmt.Sprintf("  %-28s %s", it.ID, string(it.RawRelative)))
		}
		if blocked > 0 {
			out.Human = append(out.Human, fmt.Sprintf("  %d due object(s) lack complete evidence and were left out", blocked))
		}
		out.Human = append(out.Human,
			"",
			"permanent deletion is irreversible. Applying requires this digest:",
			"  "+string(p.Digest))
		out.Next = []string{
			fmt.Sprintf("dustkeeper purge apply --id %s --digest %s", p.ID, p.Digest),
		}
		return out, nil
	})
}

func purgeApply(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("purge apply", io.Discard)
	id := fs.String("id", "", "plan ID")
	digest := fs.String("digest", "", "the full plan digest that was reviewed")
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
		if p.Kind != domain.PlanManualPurge && p.Kind != domain.PlanScheduledPurge {
			return output{}, errs.Newf(errs.CodePlanStale, "plan %s is a %s plan, not a purge plan", p.ID, p.Kind)
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
		sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), sc.ID)
		if err != nil {
			return output{}, err
		}
		defer sctx.Close()

		exec := a.executor(a.detector())
		guard, err := guardFor(ctx, a.store, sctx)
		if err != nil {
			return output{}, err
		}
		exec.Guard = guard
		budget := domain.NewBudget(a.cfg.Runtime.RunSoftSeconds, a.cfg.Runtime.RunReadBudgetBytes,
			len(items), planBytes(items), a.cfg.Runtime.ReadRateBytesPerSecond)

		// A manual purge is an interactive decision, but "the user is present"
		// is not evidence that the wall clock is coherent. Retention is proven
		// from the recorded quarantine instant, so the same clock assertion the
		// background path uses applies here too.
		trust := clocktrust.Start(a.store, a.clock)
		res, err := exec.ApplyPurgePlan(ctx, sctx, p.ID, domain.Digest(*digest), budget, trust.Assert)
		if err != nil {
			return output{}, err
		}

		out := ok()
		out.ScopeID = string(sc.ID)
		out.PlanID = string(p.ID)
		out.Counts = &counts{
			Planned:   res.Planned,
			Completed: res.Completed,
			Skipped:   res.Skipped,
			Failed:    res.Failed,
		}
		out.Data = res
		out.Human = []string{
			fmt.Sprintf("purged %d of %d object(s), %d skipped, %d failed",
				res.Completed, res.Planned, res.Skipped, res.Failed),
		}
		for _, r := range res.Results {
			if r.Code == string(errs.CodeOK) {
				continue
			}
			out.Human = append(out.Human, fmt.Sprintf("  %s  %s  %s", r.Item, r.Code, r.Message))
		}
		if res.Code != string(errs.CodeOK) {
			out.Code = errs.Code(res.Code)
			out.Status = string(errs.StatusOf(out.Code))
			out.Message = res.Message
		}
		if res.Failed > 0 || res.Skipped > 0 {
			out.Next = []string{"dustkeeper recover inspect"}
		}
		return out, nil
	})
}

// cmdRecover routes recover inspect/reconcile.
func cmdRecover(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "recover needs a subcommand: inspect, reconcile, adopt")
	}
	switch args[0] {
	case "inspect":
		fs := flagset("recover inspect", a.stderr)
		root := fs.String("root", "", "inventory this scope root without opening the database")
		if err := parseFlags(fs, args[1:]); err != nil {
			return output{}, err
		}
		// The manifest-only path deliberately runs before openStore. R13 records
		// that a damaged database made the independent record unreadable, which is
		// the one situation an operator needs it in.
		if *root != "" {
			clean, err := rescue.ParseRootArg(*root)
			if err != nil {
				return output{}, err
			}
			rep, err := rescue.Inspect(clean)
			if err != nil {
				return output{}, err
			}
			out := ok()
			out.Data = rep
			out.Human = []string{
				fmt.Sprintf("inventory of %s (no database was opened)", rep.Root),
				fmt.Sprintf("  %d object(s): %d complete, %d manifest without object, %d orphan, %d conflict, %d unknown",
					rep.Objects, rep.Complete, rep.Missing, rep.Orphans, rep.Conflicts, rep.Unknown),
			}
			if rep.InstanceID != "" {
				out.Human = append(out.Human, "  owner instance: "+rep.InstanceID)
			}
			for _, e := range rep.Entries {
				line := fmt.Sprintf("  %-28s %-22s %s", e.Object, e.Status, e.RawRelative)
				out.Human = append(out.Human, line)
				for _, n := range e.Notes {
					out.Human = append(out.Human, "      "+n)
				}
			}
			for _, n := range rep.Notes {
				out.Human = append(out.Human, "  note: "+n)
			}
			if rep.Orphans > 0 || rep.Conflicts > 0 || rep.Missing > 0 {
				out.Code = errs.CodeRecoveryRequired
				out.Status = string(errs.StatusOf(out.Code))
				out.Message = "the directory holds objects this inventory cannot explain; nothing was changed"
			}
			return out, nil
		}

		if err := a.openStore(); err != nil {
			return output{}, err
		}
		pending, err := a.reconciler().Inspect(ctx)
		if err != nil {
			return output{}, err
		}
		out := ok()
		out.Data = map[string]any{"pending": pending, "count": len(pending)}
		if len(pending) == 0 {
			out.Human = append(out.Human, "no operation is waiting for reconciliation")
			return out, nil
		}
		out.Human = append(out.Human, fmt.Sprintf("%d operation(s) committed an intent but have no recorded outcome:", len(pending)))
		for _, p := range pending {
			out.Human = append(out.Human, fmt.Sprintf("  %s  %s  %s  since %s",
				p.Item.ID, p.Action, string(p.Item.RawRelative),
				time.Unix(0, p.IntentAtNS).Format(time.RFC3339)))
		}
		out.Human = append(out.Human, "", "run `dustkeeper recover reconcile` to finish or explain them")
		out.Next = []string{"dustkeeper recover reconcile"}
		return out, nil
	case "reconcile":
		if err := a.openStore(); err != nil {
			return output{}, err
		}
		return a.withInstance(func() (output, error) {
			rep, err := a.reconciler().Run(ctx)
			if err != nil {
				return output{}, err
			}
			out := ok()
			out.Data = rep
			out.Human = []string{
				fmt.Sprintf("reconciled %d operation(s): %d completed, %d aborted, %d held, %d parked, %d busy, %d blocked",
					rep.Examined, rep.Completed, rep.Aborted, rep.Held, rep.Parked, rep.Busy, rep.Blocked),
			}
			for _, r := range rep.Results {
				out.Human = append(out.Human, fmt.Sprintf("  %-28s %-8s %-10s %s", r.Item, r.Action, r.Outcome, r.Detail))
			}
			if len(rep.Scopes) > 0 {
				out.Human = append(out.Human, "  scopes held: "+fmt.Sprint(rep.Scopes))
			}
			switch {
			case rep.Held > 0:
				// A hold is a verdict: nothing is retried, so the operator needs
				// to be told what the exits are.
				out.Code = errs.CodeRecoveryRequired
				out.Status = string(errs.StatusOf(errs.CodeRecoveryRequired))
				out.Message = "some operations were held; they stay visible and nothing was guessed"
				out.Next = []string{"dustkeeper recover adopt --item <id> --decision restore|quarantine --reason <why>"}
			case rep.Parked > 0 || rep.Busy > 0 || rep.Blocked > 0:
				// Still unfinished, still visible, and their pending markers are
				// intact, so the next pass picks them up.
				out.Code = errs.CodeRecoveryRequired
				out.Status = string(errs.StatusOf(errs.CodeRecoveryRequired))
				out.Message = "some operations could not be finished yet; nothing was dropped and they will be retried"
				out.Next = []string{"dustkeeper recover reconcile"}
			}
			return out, nil
		})
	case "adopt":
		fs := flagset("recover adopt", a.stderr)
		item := fs.String("item", "", "item ID to adopt")
		decision := fs.String("decision", "", "restore or quarantine")
		to := fs.String("to", "", "restore destination, relative to the scope root (default: the original path)")
		reason := fs.String("reason", "", "why this decision is being taken (recorded verbatim)")
		if err := parseFlags(fs, args[1:]); err != nil {
			return output{}, err
		}
		if *item == "" {
			return output{}, errs.New(errs.CodeInvalidArgument, "--item is required")
		}
		if *decision == "" {
			return output{}, errs.New(errs.CodeInvalidArgument,
				"--decision is required: restore puts the object back, quarantine accepts it as quarantined")
		}
		if *reason == "" {
			// An adoption is a human decision about a file that could not be
			// explained automatically. "Why" has to be part of the record.
			return output{}, errs.New(errs.CodeInvalidArgument,
				"--reason is required: the decision is recorded in the audit trail")
		}
		if err := a.openStore(); err != nil {
			return output{}, err
		}
		return a.withInstance(func() (output, error) {
			res, err := a.reconciler().Adopt(ctx, recovery.AdoptOptions{
				ItemID:   domain.ItemID(*item),
				Decision: *decision,
				To:       []byte(*to),
				Reason:   *reason,
				Actor:    "operator",
			})
			if err != nil {
				return output{}, err
			}
			out := ok()
			out.ObjectID = string(res.Item)
			out.Data = res
			out.Human = []string{
				fmt.Sprintf("adopted %s: %s -> %s (operation %s, converging %s)",
					res.Item, res.PriorState, res.State, res.NewOpID, res.PriorOpID),
			}
			if res.Destination != "" {
				out.Human = append(out.Human, "  destination: "+res.Destination)
			}
			if res.Detail != "" {
				out.Human = append(out.Human, "  "+res.Detail)
			}
			return out, nil
		})
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown recover subcommand %q", args[0])
}

// detector builds the duplicate detector. The executor uses it as its keeper
// verifier, so a duplicate purge re-proves the surviving copy.
func (a *app) detector() *dedup.Detector {
	return &dedup.Detector{
		Store:      a.store,
		Registrar:  a.registrar(),
		Clock:      a.clock,
		Buffer:     make([]byte, a.cfg.Runtime.IOBufferBytes),
		RatePerSec: a.cfg.Runtime.ReadRateBytesPerSecond,
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
