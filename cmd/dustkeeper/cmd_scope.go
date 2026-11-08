package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// cmdScope routes the scope subcommands.
func cmdScope(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"scope needs a subcommand: add, list, show, pause, resume, rebind")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return scopeAdd(ctx, a, rest)
	case "list":
		return scopeList(ctx, a, rest)
	case "show":
		return scopeShow(ctx, a, rest)
	case "pause":
		return scopePause(ctx, a, rest, false)
	case "resume":
		return scopePause(ctx, a, rest, true)
	case "rebind":
		return scopeRebind(ctx, a, rest)
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown scope subcommand %q", sub)
}

func scopeAdd(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("scope add", io.Discard)
	path := fs.String("path", "", "absolute path of the directory")
	kind := fs.String("kind", "", "observe, assisted, managed_disposable or reference")
	name := fs.String("name", "", "scope ID; defaults to the directory name")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *path == "" || *kind == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--path and --kind are required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	sc, err := a.registrar().Add(ctx, scope.AddOptions{Path: *path, Kind: *kind, Name: *name})
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"scope_id": sc.ID,
		"kind":     sc.Kind,
		"root":     string(sc.RawRoot),
		"status":   sc.Status,
		"vault":    sc.VaultIdentityJSON != nil,
	}
	out.Human = []string{
		fmt.Sprintf("registered scope %s (%s)", sc.ID, sc.Kind),
		fmt.Sprintf("  root: %q", string(sc.RawRoot)),
	}
	if domain.MutableKind(sc.Kind) {
		out.Human = append(out.Human,
			"  a private .dustkeeper directory was created for the quarantine vault",
			"  registering it does not authorise any change: rules and grants do")
	} else {
		out.Human = append(out.Human,
			"  this kind is read-only; nothing inside it can be moved or deleted by this program")
	}
	out.Next = []string{
		fmt.Sprintf("dustkeeper doctor --scope %s", sc.ID),
		fmt.Sprintf("dustkeeper scan --scope %s", sc.ID),
	}
	return out, nil
}

func scopeList(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("scope list", io.Discard)
	kind := fs.String("kind", "", "filter by kind")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *kind != "" {
		switch *kind {
		case domain.KindObserve, domain.KindAssisted, domain.KindManagedDisposable, domain.KindReference:
		default:
			return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown scope kind %q", *kind)
		}
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	var scopes []store.Scope
	var err error
	if *kind != "" {
		scopes, err = a.store.ListScopesByKind(ctx, *kind)
	} else {
		scopes, err = a.store.ListScopes(ctx)
	}
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.Data = map[string]any{"scopes": scopes, "count": len(scopes)}
	if len(scopes) == 0 {
		out.Human = append(out.Human, "no scopes registered")
		out.Next = []string{"dustkeeper scope add --path <dir> --kind observe --name <id>"}
		return out, nil
	}
	for _, sc := range scopes {
		out.Human = append(out.Human, fmt.Sprintf("%-18s %-20s %-8s %q",
			sc.ID, sc.Kind, sc.Status, string(sc.RawRoot)))
	}
	return out, nil
}

func scopeShow(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("scope show", io.Discard)
	ref := fs.String("id", "", "scope ID")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *ref == "" && fs.NArg() > 0 {
		*ref = fs.Arg(0)
	}
	if *ref == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	sc, err := resolveScope(ctx, a.store, *ref)
	if err != nil {
		return output{}, err
	}

	// The counts are reads that either succeed or stop the command. Ignoring the
	// error here would print an empty scope whenever the database could not be
	// read, which is the one reading a person must not get from this command.
	obs, err := a.store.CountObservations(ctx, sc.ID)
	if err != nil {
		return output{}, err
	}
	planned, err := a.store.CountItemsByState(ctx, sc.ID, domain.StatePlanned)
	if err != nil {
		return output{}, err
	}
	quarantined, err := a.store.CountItemsByState(ctx, sc.ID, domain.StateQuarantined)
	if err != nil {
		return output{}, err
	}
	held, err := a.store.CountItemsByState(ctx, sc.ID, domain.StateHeld)
	if err != nil {
		return output{}, err
	}
	unknown, err := a.store.CountItemsByState(ctx, sc.ID, domain.StateUnknown)
	if err != nil {
		return output{}, err
	}
	due, err := a.store.CountDueForPurge(ctx, sc.ID, a.clock.Wall().UnixNano())
	if err != nil {
		return output{}, err
	}
	protections, err := a.store.ListProtections(ctx, sc.ID, true)
	if err != nil {
		return output{}, err
	}
	rules, err := a.store.ListRules(ctx, sc.ID)
	if err != nil {
		return output{}, err
	}
	grants, err := a.store.ListGrants(ctx, sc.ID)
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"scope":        sc,
		"observations": obs,
		"items": map[string]int64{
			"planned": planned, "quarantined": quarantined, "held": held, "unknown": unknown,
		},
		"due_for_purge": due,
		"protections":   protections,
		"rules":         rules,
		"grants":        grants,
	}
	out.Human = []string{
		fmt.Sprintf("scope %s (%s) %s", sc.ID, sc.Kind, sc.Status),
		fmt.Sprintf("  root:        %q", string(sc.RawRoot)),
		fmt.Sprintf("  generation:  %d   protection revision: %d", sc.Generation, sc.ProtectionRevision),
		fmt.Sprintf("  observed:    %d file(s)", obs),
		fmt.Sprintf("  quarantine:  %d object(s), %d due for purge", quarantined, due),
		fmt.Sprintf("  awaiting:    %d planned, %d held, %d unknown", planned, held, unknown),
		fmt.Sprintf("  rules:       %d revision(s), grants: %d", len(rules), len(grants)),
		"  created:     " + time.Unix(0, sc.CreatedAtNS).Format("2006-01-02 15:04:05"),
	}
	if sc.Status != domain.ScopeActive {
		out.Human = append(out.Human, "  this scope will not be processed until it is resumed")
	}
	return out, nil
}

// scopePause changes only the database status of one scope. It still takes the
// instance lock, because a run reads that status to decide what to touch.
func scopePause(ctx context.Context, a *app, args []string, resume bool) (output, error) {
	name := "scope pause"
	if resume {
		name = "scope resume"
	}
	fs := flagset(name, io.Discard)
	ref := fs.String("id", "", "scope ID")
	acceptStale := fs.Bool("accept-stale-hold", false,
		"resume a held scope whose hold no longer describes anything; refused while any object is unresolved")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if !resume && *acceptStale {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"--accept-stale-hold is valid only with scope resume")
	}
	if *ref == "" && fs.NArg() > 0 {
		*ref = fs.Arg(0)
	}
	if *ref == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	out, err := a.withInstance(func() (output, error) {
		sc, err := resolveScope(ctx, a.store, *ref)
		if err != nil {
			return output{}, err
		}
		if !resume && sc.Status == domain.ScopeHeld {
			return output{}, errs.Newf(errs.CodeHeld,
				"scope %s is held; pause cannot replace a stronger safety hold", sc.ID)
		}
		target := domain.ScopePaused
		if resume {
			target = domain.ScopeActive
		}
		if sc.Status == domain.ScopeHeld && resume {
			// "Held" used to have exactly one explanation in this message - a
			// root that no longer proves itself - and that explanation was
			// wrong for the other way a scope gets here: an older build could
			// finish a compensation and stop between closing the object and
			// releasing the scope, leaving a scope held over nothing. Telling
			// that operator to re-register sends them to change a registration
			// that is fine.
			//
			// The two cases are told apart by the fact rather than by a stored
			// reason, because the schema has no field for one: is anything in
			// this scope still unresolved? A held object, a parked one, an open
			// intent - any of those means the hold still has a subject.
			unresolved, uerr := a.store.ScopeHasUnresolved(ctx, sc.ID)
			if uerr != nil {
				return output{}, uerr
			}
			if unresolved {
				return output{}, errs.Newf(errs.CodeHeld,
					"scope %s is held and still has an unresolved object; finish it or restore it before resuming (`dustkeeper quarantine list` shows what is left). If doctor reports that the root no longer matches its registration, re-register the scope instead", sc.ID)
			}
			if !*acceptStale {
				return output{}, errs.Newf(errs.CodeHeld,
					"scope %s is held but nothing in it is unresolved, so the hold no longer describes anything: this is what an older build left behind when a compensation finished and the scope was not released with it. Accepting it changes nothing about the tree, and the root is checked again the next time the scope is opened; pass --accept-stale-hold to resume", sc.ID)
			}
		}
		if err := a.store.SetScopeStatus(ctx, sc.ID, target); err != nil {
			return output{}, err
		}
		res := ok()
		res.ScopeID = string(sc.ID)
		res.Data = map[string]any{"previous": sc.Status, "status": target}
		res.Human = []string{fmt.Sprintf("scope %s: %s -> %s", sc.ID, sc.Status, target)}
		if !resume {
			res.Human = append(res.Human, "an operation already in flight finishes at a safe boundary")
		}
		return res, nil
	})
	if err != nil {
		return out, err
	}
	if !resume {
		out.Message = "scope paused; no new plan will be created for it while the background run continues elsewhere"
	}
	return out, nil
}

// scopeRebind is the two-step, history-preserving recovery of a scope whose
// runtime volume label was renumbered across a boot.
//
// With no --digest it is strictly read-only: it proves the tree, the vault and
// every isolated object, and prints a digest over those exact facts. Confirming
// re-verifies the same facts and refuses a stale digest. `scope resume
// --accept-stale-hold` cannot do this: it only changes a status column and
// leaves the vault binding and the old generation untouched.
func scopeRebind(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("scope rebind", io.Discard)
	ref := fs.String("id", "", "scope ID")
	digest := fs.String("digest", "", "digest printed by the read-only preview")
	confirm := fs.Bool("confirm", false, "confirm this exact rebind; without it the command is a read-only preview")
	backup := fs.String("backup", "", "existing private backup directory outside the scope root")
	scheduleStopped := fs.Bool("schedule-stopped", false, "attest that the launchd job for this instance is stopped")
	note := fs.String("note", "", "why this recovery was performed (recorded with the audit event)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *ref == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	reg := a.registrar()
	scopeID := domain.ScopeID(*ref)

	if *digest == "" {
		p, err := reg.RebindPreviewFor(ctx, scopeID)
		if err != nil {
			return output{}, err
		}
		out := ok()
		out.ScopeID = *ref
		out.Data = map[string]any{"preview": p, "digest": p.Digest, "mode": "preview"}
		out.Message = "read-only preview; no status, identity, event or vault file was changed"
		out.Human = []string{
			fmt.Sprintf("scope %s rebind preview (read-only)", p.ScopeID),
			fmt.Sprintf("  instance: %s   root: %s", p.InstanceID, p.Root),
			fmt.Sprintf("  status: %s   generation: %d -> %d   protection revision: %d -> %d",
				p.Status, p.Generation, p.NewGeneration, p.ProtectionRevision, p.NewProtectionRevision),
			fmt.Sprintf("  identity: %s -> %s   boot changed: %t",
				p.OldRootIdentity.Persistent.VolumeID, p.NewRootIdentity.Persistent.VolumeID, p.BootChanged),
			fmt.Sprintf("  reason: %s", p.Verdict.RuntimeRelabeled),
			fmt.Sprintf("  vault label in owner/bind: %s / %s   objects: %d   pending ops: %d",
				p.OwnerVolumeLabel, p.BindVolumeLabel, p.QuarantinedCount, p.PendingOps),
			fmt.Sprintf("  plans: %d   grants: %d   protections: %d   observations: %d (all preserved)",
				len(p.Plans), len(p.Grants), p.Protections, p.Observations),
			fmt.Sprintf("  global pause: %t   unresolved objects: %d", p.GlobalPaused, len(p.Unresolvable)),
			fmt.Sprintf("  preview digest: %s", p.Digest),
		}
		for _, o := range p.Objects {
			out.Human = append(out.Human, fmt.Sprintf(
				"    %s  state=%s manifest=%t object=%t identity_match=%t restore_chain=%t %s",
				o.ItemID, o.State, o.ManifestPresent, o.ObjectPresent, o.IdentityMatches,
				o.RestoreChainValid, o.UnresolvedReason))
		}
		for _, n := range p.Notes {
			out.Human = append(out.Human, "  - "+n)
		}
		out.Human = append(out.Human,
			"  confirm only after the schedule is stopped, a consistent backup exists and the specific action is authorised",
			"  a rebind never unpauses the instance, never resumes the schedule and never runs a cleanup")
		out.Next = []string{
			fmt.Sprintf("dustkeeper scope rebind --id %s --digest %s --confirm --backup <backup_dir> --schedule-stopped --note \"<reason>\"",
				shellQuote(*ref), shellQuote(p.Digest)),
		}
		return out, nil
	}

	if !*confirm {
		return output{}, errs.New(errs.CodeNeedsInput,
			"a confirmed rebind changes the scope generation; repeat with --confirm (or omit --digest for a read-only preview)")
	}
	if *note == "" {
		return output{}, errs.New(errs.CodeNeedsInput, "--note must record why this recovery was performed")
	}
	return a.withInstance(func() (output, error) {
		res, err := reg.ConfirmRebind(ctx, scopeID, scope.RebindOptions{
			Digest:          *digest,
			BackupPath:      *backup,
			ScheduleStopped: *scheduleStopped,
			Note:            *note,
		})
		if err != nil {
			return output{}, err
		}
		out := ok()
		out.ScopeID = *ref
		out.Data = map[string]any{"rebind": res, "mode": "confirmed"}
		out.Message = "scope rebound on a new generation while paused; no scan, model call, restore, cleanup or schedule change was made"
		out.Human = []string{
			fmt.Sprintf("scope %s: generation %d -> %d, status %s", res.ScopeID, res.OldGeneration, res.NewGeneration, res.Status),
			fmt.Sprintf("  %d isolated object(s) and their manifests/history preserved; audit event %s", res.Objects, res.EventID),
			"  old plans and grants are bound to the old generation and cannot execute; re-preview and re-confirm the applicable rules",
			"  the instance is still globally paused and the schedule is still stopped",
			"  next: verify with doctor / status, review rule previews, and only then decide separately about resuming",
		}
		return out, nil
	})
}
