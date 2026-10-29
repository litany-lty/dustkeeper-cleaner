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
			"scope needs a subcommand: add, list, show, pause, resume")
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
		"  root: " + string(sc.RawRoot),
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
		out.Human = append(out.Human, fmt.Sprintf("%-18s %-20s %-8s %s",
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

	obs, _ := a.store.CountObservations(ctx, sc.ID)
	planned, _ := a.store.CountItemsByState(ctx, sc.ID, domain.StatePlanned)
	quarantined, _ := a.store.CountItemsByState(ctx, sc.ID, domain.StateQuarantined)
	held, _ := a.store.CountItemsByState(ctx, sc.ID, domain.StateHeld)
	unknown, _ := a.store.CountItemsByState(ctx, sc.ID, domain.StateUnknown)
	protections, _ := a.store.ListProtections(ctx, sc.ID, true)
	rules, _ := a.store.ListRules(ctx, sc.ID)
	grants, _ := a.store.ListGrants(ctx, sc.ID)
	due, _ := a.store.ItemsDueForPurge(ctx, sc.ID, a.clock.Wall().UnixNano(), 1000)

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"scope":        sc,
		"observations": obs,
		"items": map[string]int64{
			"planned": planned, "quarantined": quarantined, "held": held, "unknown": unknown,
		},
		"due_for_purge": dueCount(due),
		"protections":   protections,
		"rules":         rules,
		"grants":        grants,
	}
	out.Human = []string{
		fmt.Sprintf("scope %s (%s) %s", sc.ID, sc.Kind, sc.Status),
		"  root:        " + string(sc.RawRoot),
		fmt.Sprintf("  generation:  %d   protection revision: %d", sc.Generation, sc.ProtectionRevision),
		fmt.Sprintf("  observed:    %d file(s)", obs),
		fmt.Sprintf("  quarantine:  %d object(s), %d due for purge", quarantined, dueCount(due)),
		fmt.Sprintf("  awaiting:    %d planned, %d held, %d unknown", planned, held, unknown),
		fmt.Sprintf("  rules:       %d revision(s), grants: %d", len(rules), len(grants)),
		"  created:     " + time.Unix(0, sc.CreatedAtNS).Format("2006-01-02 15:04:05"),
	}
	if sc.Status != domain.ScopeActive {
		out.Human = append(out.Human, "  this scope will not be processed until it is resumed")
	}
	return out, nil
}

func dueCount(items []store.Item) int { return len(items) }

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
