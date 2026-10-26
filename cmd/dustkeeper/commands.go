package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/recovery"
	"dustkeeper/internal/run"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// cmdInit creates the state directory, the default configuration and the
// database. It is idempotent and never overwrites an existing configuration.
func cmdInit(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("init", io.Discard)
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}

	for _, dir := range []string{a.paths.ConfigDir, a.paths.StateDir, a.paths.LogDir} {
		if err := ensureDir(dir, 0o700); err != nil {
			return output{}, err
		}
	}

	createdConfig := false
	if _, err := os.Stat(a.paths.ConfigFile); os.IsNotExist(err) {
		body, merr := config.Marshal(a.cfg)
		if merr != nil {
			return output{}, errs.Wrap(errs.CodeStateWriteFailed, merr)
		}
		if werr := os.WriteFile(a.paths.ConfigFile, body, 0o600); werr != nil {
			return output{}, errs.Wrap(errs.CodeStateWriteFailed, werr)
		}
		createdConfig = true
	} else if err != nil {
		return output{}, errs.Wrap(errs.CodeAccessDenied, err)
	}

	st, err := store.Open(a.paths.DBFile)
	if err != nil {
		return output{}, err
	}
	a.store = st

	instID, err := st.MetaGet(ctx, "instance_id")
	if err != nil || instID == "" {
		instID = domain.NewID("inst")
		if err := st.MetaSet(ctx, "instance_id", instID); err != nil {
			return output{}, err
		}
	}
	a.instance = instID

	if err := writeInstanceFile(a.paths.InstanceJSON, instanceFile{
		FormatVersion: 1,
		InstanceID:    instID,
		CreatedAtNS:   a.clock.Wall().UnixNano(),
		StateDir:      a.paths.StateDir,
		DBFile:        a.paths.DBFile,
	}); err != nil {
		return output{}, err
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.Data = map[string]any{
		"config_dir":     a.paths.ConfigDir,
		"config_file":    a.paths.ConfigFile,
		"state_dir":      a.paths.StateDir,
		"db_file":        a.paths.DBFile,
		"log_dir":        a.paths.LogDir,
		"instance_id":    instID,
		"config_created": createdConfig,
		"stats":          stats,
	}
	out.Human = []string{
		"initialised " + a.paths.StateDir,
		"  database: " + a.paths.DBFile,
		"  config:   " + a.paths.ConfigFile,
		"  instance: " + instID,
		"",
		"Nothing is monitored yet. Register a directory you own:",
		"  dustkeeper scope add --path <dir> --kind observe --name <id>",
	}
	out.Next = []string{"dustkeeper scope add --path <dir> --kind observe --name <id>"}
	return out, nil
}

// check is one doctor observation.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// cmdDoctor reports what the host can actually do. It creates only its own
// probe directory, and only after the caller asked for a check.
func cmdDoctor(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("doctor", io.Discard)
	scopeRef := fs.String("scope", "", "scope ID or directory name")
	ackClock := fs.Bool("acknowledge-clock", false, "record that the current wall clock is trusted")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	var checks []check
	add := func(name string, ok bool, code errs.Code, detail string) {
		c := check{Name: name, OK: ok, Detail: detail}
		if !ok {
			c.Code = string(code)
		}
		checks = append(checks, c)
	}

	// 1. Directory layout.
	layoutOK := true
	for _, dir := range []string{a.paths.ConfigDir, a.paths.StateDir, a.paths.LogDir} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			layoutOK = false
			break
		}
	}
	add("state_dir", layoutOK, errs.CodeStateWriteFailed,
		"expected a private state directory at "+a.paths.StateDir)

	// 2. Database round-trip. Reading a pragma back proves the connection is
	// configured the way the store claims, not just that a file opened.
	stats, err := a.store.Stats(ctx)
	if err != nil {
		add("database", false, errs.CodeDBError, err.Error())
	} else {
		add("database", true, errs.CodeOK,
			fmt.Sprintf("%d scopes, %d rules, %d items", stats.Scopes, stats.Rules, stats.Items))
	}

	// 3. Filesystem capabilities, measured in the state directory and then in
	// every mutable scope: a scope on another volume may not share them.
	caps := fsops.Probe(a.paths.StateDir)
	add("capabilities_state_dir", caps.Capable(), errs.CodeUnsupportedFS,
		describeCaps(caps))

	// 4. Pause marker and pending work.
	gate := scope.PauseGate{StateDir: a.paths.StateDir}
	paused, perr := gate.Paused()
	if perr != nil {
		add("pause_marker", false, errs.CodeStateWriteFailed, perr.Error())
	} else {
		add("pause_marker", true, errs.CodeOK, map[bool]string{true: "paused", false: "running"}[paused])
	}

	pending, err := a.store.PendingOps(ctx)
	if err != nil {
		add("pending_operations", false, errs.CodeDBError, err.Error())
	} else {
		add("pending_operations", len(pending) == 0, errs.CodeRecoveryRequired,
			fmt.Sprintf("%d operation(s) need reconciliation", len(pending)))
	}

	// 5. Clock sanity against the last recorded run.
	nowNS := a.clock.Wall().UnixNano()
	lastStr, _ := a.store.MetaGet(ctx, "last_run_ns")
	clockOK := true
	clockDetail := "no previous run recorded"
	if lastStr != "" {
		if last, perr := strconv.ParseInt(lastStr, 10, 64); perr == nil {
			delta := nowNS - last
			clockDetail = "last run " + humanDuration(time.Duration(delta))
			if delta < -int64(5*time.Minute) {
				clockOK = false
				clockDetail = "the wall clock is more than 5 minutes behind the last run"
			} else if delta > int64(48*time.Hour) {
				ackStr, _ := a.store.MetaGet(ctx, "clock_ack_ns")
				ack, _ := strconv.ParseInt(ackStr, 10, 64)
				if ack <= last {
					clockOK = false
					clockDetail = "more than 48 hours passed between runs; purging stays suspended until you acknowledge the clock"
				}
			}
		}
	}
	if *ackClock {
		if err := a.store.MetaSet(ctx, "clock_ack_ns", strconv.FormatInt(nowNS, 10)); err != nil {
			return output{}, err
		}
		clockOK = true
		clockDetail = "clock acknowledged now; future purges may proceed until the next anomaly"
	}
	add("clock", clockOK, errs.CodeClockUntrusted, clockDetail)

	// 6. Scope verification.
	targets := []store.Scope{}
	if *scopeRef != "" {
		sc, err := resolveScope(ctx, a.store, *scopeRef)
		if err != nil {
			return output{}, err
		}
		targets = append(targets, sc)
	} else {
		all, err := a.store.ListScopes(ctx)
		if err != nil {
			return output{}, err
		}
		targets = all
	}

	reg := a.registrar()
	var firstFail errs.Code
	var firstDetail string
	keep := func(c check) {
		if !c.OK && firstFail == "" {
			firstFail = errs.Code(c.Code)
			firstDetail = c.Name + ": " + c.Detail
		}
	}
	for _, c := range checks {
		keep(c)
	}

	scopeReport := make([]map[string]any, 0, len(targets))
	for _, sc := range targets {
		entry := map[string]any{
			"scope_id": string(sc.ID),
			"kind":     sc.Kind,
			"status":   sc.Status,
			"root":     string(sc.RawRoot),
		}
		vs, err := reg.OpenVerified(ctx, sc.ID)
		if err != nil {
			code := errs.CodeOf(err)
			entry["ok"] = false
			entry["code"] = string(code)
			entry["detail"] = messageOf(err)
			if firstFail == "" {
				firstFail, firstDetail = code, string(sc.ID)+": "+messageOf(err)
			}
		} else {
			caps := fsops.Probe(string(sc.RawRoot))
			entry["ok"] = true
			entry["capabilities"] = caps
			vs.Root.Close()
			if !caps.Capable() {
				entry["ok"] = false
				entry["code"] = string(errs.CodeUnsupportedFS)
				entry["detail"] = describeCaps(caps)
				if firstFail == "" {
					firstFail, firstDetail = errs.CodeUnsupportedFS, string(sc.ID)+": "+describeCaps(caps)
				}
			}
		}
		scopeReport = append(scopeReport, entry)
	}

	out := ok()
	if firstFail != "" && firstFail != errs.CodeOK {
		out = fail(errs.New(firstFail, firstDetail))
	}
	out.Data = map[string]any{
		"checks": checks,
		"scopes": scopeReport,
		"capabilities": map[string]any{
			"state_dir": caps,
		},
	}
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		out.Human = append(out.Human, fmt.Sprintf("%s %-24s %s", mark, c.Name, c.Detail))
	}
	for _, e := range scopeReport {
		mark := "ok  "
		if e["ok"] == false {
			mark = "FAIL"
		}
		out.Human = append(out.Human, fmt.Sprintf("%s scope %-18v %v", mark, e["scope_id"], e["status"]))
	}
	if out.Code == errs.CodeOK {
		out.Next = []string{"dustkeeper scan --scope <id>"}
	} else {
		out.Next = []string{"fix the failing check, then re-run `dustkeeper doctor`"}
	}
	return out, nil
}

func describeCaps(c fsops.Capabilities) string {
	missing := ""
	if !c.NoReplaceRename {
		missing += " no-replace rename;"
	}
	if !c.DirSync {
		missing += " directory fsync;"
	}
	if !c.NoFollow {
		missing += " no-follow traversal;"
	}
	if missing == "" {
		return "exclusive rename, directory sync and no-follow traversal verified"
	}
	return "missing:" + missing
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		return "-" + humanDuration(-d)
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// cmdStatus is a read-only summary. It never drives a decision.
func cmdStatus(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("status", io.Discard)
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	stats, err := a.store.Stats(ctx)
	if err != nil {
		return output{}, err
	}
	scopes, err := a.store.ListScopes(ctx)
	if err != nil {
		return output{}, err
	}
	pending, err := a.store.PendingOps(ctx)
	if err != nil {
		return output{}, err
	}
	gate := scope.PauseGate{StateDir: a.paths.StateDir}
	paused, err := gate.Paused()
	if err != nil {
		return output{}, err
	}
	lastRun, _ := a.store.MetaGet(ctx, "last_run_ns")

	type scopeStatus struct {
		ScopeID     string `json:"scope_id"`
		Kind        string `json:"kind"`
		Status      string `json:"status"`
		Observation int64  `json:"observation_count"`
		Quarantined int64  `json:"quarantined_count"`
		Due         int64  `json:"due_for_purge_count"`
	}
	var rows []scopeStatus
	for _, sc := range scopes {
		obs, _ := a.store.CountObservations(ctx, sc.ID)
		q, _ := a.store.CountItemsByState(ctx, sc.ID, domain.StateQuarantined)
		due, _ := a.store.ItemsDueForPurge(ctx, sc.ID, a.clock.Wall().UnixNano(), 1)
		rows = append(rows, scopeStatus{
			ScopeID:     string(sc.ID),
			Kind:        sc.Kind,
			Status:      sc.Status,
			Observation: obs,
			Quarantined: q,
			Due:         int64(len(due)),
		})
	}

	out := ok()
	out.Data = map[string]any{
		"paused":      paused,
		"last_run_ns": lastRun,
		"pending_ops": len(pending),
		"stats":       stats,
		"scopes":      rows,
		"state_dir":   a.paths.StateDir,
	}
	state := "running"
	if paused {
		state = "paused (global marker present)"
	}
	out.Human = append(out.Human,
		"state:    "+state,
		fmt.Sprintf("scopes:   %d   rules: %d   items: %d   events: %d",
			stats.Scopes, stats.Rules, stats.Items, stats.Events),
		fmt.Sprintf("pending:  %d operation(s) awaiting reconciliation", len(pending)),
	)
	for _, r := range rows {
		out.Human = append(out.Human, fmt.Sprintf("  %-18s %-20s %-8s quarantined=%d due=%d",
			r.ScopeID, r.Kind, r.Status, r.Quarantined, r.Due))
	}
	if len(pending) > 0 {
		out.Next = []string{"dustkeeper recover inspect"}
	}
	return out, nil
}

// cmdEvents prints the audit trail. The events table is authoritative; this is
// a derived view.
func cmdEvents(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("events", io.Discard)
	limit := fs.Int("limit", 50, "maximum number of events")
	afterSeq := fs.Int64("after-seq", 0, "only events after this sequence")
	item := fs.String("item", "", "only events for this item")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *limit <= 0 || *limit > 5000 {
		return output{}, errs.New(errs.CodeInvalidArgument, "--limit must be between 1 and 5000")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	var events []store.Event
	var err error
	if *item != "" {
		events, err = a.store.EventsForItem(ctx, domain.ItemID(*item))
	} else {
		events, err = a.store.ListEvents(ctx, *limit, *afterSeq)
	}
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.Data = map[string]any{"events": events, "count": len(events)}
	for _, ev := range events {
		item := ""
		if ev.ItemID != nil {
			item = " " + string(*ev.ItemID)
		}
		out.Human = append(out.Human, fmt.Sprintf("%6d %s %-8s %-7s%s",
			ev.Sequence, time.Unix(0, ev.OccurredAtNS).Format("2006-01-02T15:04:05"), ev.Action, ev.Phase, item))
	}
	if len(events) == 0 {
		out.Human = append(out.Human, "no events recorded")
	}
	return out, nil
}

// cmdPause writes the global pause marker.
//
// "Requested" is not "stopped": an operation that already committed its intent
// finishes at a safe boundary. The output says so instead of implying that
// files stopped moving instantly.
func cmdPause(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("pause", io.Discard)
	reason := fs.String("reason", "operator requested", "recorded reason")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	gate := scope.PauseGate{StateDir: a.paths.StateDir}
	created, err := gate.Request(*reason)
	if err != nil {
		return output{}, err
	}
	out := output{
		Status:  string(errs.StatusBlocked),
		Code:    errs.CodePauseRequested,
		Message: "pause requested; no new intent will be committed, and an operation already in flight finishes at its safe boundary",
		Data:    map[string]any{"newly_written": created, "marker": a.paths.PauseRequest},
		Human: []string{
			"pause marker written to " + a.paths.PauseRequest,
			"in-flight work continues to a safe boundary; this is not an instant stop",
			"resume with an explicit `dustkeeper resume`",
		},
	}
	return out, nil
}

// cmdResume clears only the global marker. It never reactivates a scope.
func cmdResume(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("resume", io.Discard)
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	gate := scope.PauseGate{StateDir: a.paths.StateDir}
	// Section 12.5: resume must hold the execution lock, so that clearing the
	// marker cannot race a run that is mid-batch. Lock order is instance lock,
	// then control lock — the same order the executor uses.
	return a.withInstance(func() (output, error) {
		removed, err := gate.Clear()
		if err != nil {
			return output{}, err
		}
		out := ok()
		out.Data = map[string]any{"removed": removed}
		out.Human = []string{
			"global pause marker cleared",
			"this does not change any scope status; use `dustkeeper scope resume --id <id>` for that",
		}
		if !removed {
			out.Human = append(out.Human, "there was no pause marker to clear")
		}
		return out, nil
	})
}

// cmdRun performs one bounded background pass.
func cmdRun(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("run", io.Discard)
	dryRun := fs.Bool("dry-run", false, "reconcile and report without scanning or applying")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	if *dryRun {
		rec, err := a.reconciler().Inspect(ctx)
		if err != nil {
			return output{}, err
		}
		out := ok()
		out.Data = map[string]any{"pending_ops": rec}
		out.Human = []string{fmt.Sprintf("%d pending operation(s); nothing was changed", len(rec))}
		return out, nil
	}

	var rep run.Report
	out, err := a.withInstance(func() (output, error) {
		r := &run.Runner{
			Store:      a.store,
			Clock:      a.clock,
			Config:     a.cfg,
			Registrar:  a.registrar(),
			Exec:       a.executor(nil),
			Reconciler: a.reconciler(),
		}
		var rerr error
		rep, rerr = r.Run(ctx)
		if rerr != nil {
			return output{}, rerr
		}
		res := ok()
		res.Data = rep
		res.Human = []string{
			fmt.Sprintf("run %s", rep.RunID),
			fmt.Sprintf("  reconciled %d operation(s): %d completed, %d aborted, %d held, %d unknown",
				rep.Reconciliation.Examined, rep.Reconciliation.Completed,
				rep.Reconciliation.Aborted, rep.Reconciliation.Held, rep.Reconciliation.Unknown),
			fmt.Sprintf("  quarantined %d object(s), purged %d object(s), read %d bytes",
				rep.Quarantined, rep.Purged, rep.ReadBytes),
		}
		if !rep.ClockTrusted {
			res.Human = append(res.Human, "  clock: "+rep.ClockNote)
			res.Message = rep.ClockNote
		}
		for _, s := range rep.Scopes {
			res.Human = append(res.Human, fmt.Sprintf("  %-18s %s scanned=%d complete=%v quarantined=%d purged=%d %s",
				s.Scope, "", s.Scanned, s.ScanComplete, s.Quarantined, s.Purged, s.Skipped))
		}
		return res, nil
	})
	if err != nil {
		return out, err
	}
	if !rep.ClockTrusted {
		out.Code = errs.CodeClockUntrusted
		out.Status = string(errs.StatusOf(errs.CodeClockUntrusted))
		out.Next = []string{"dustkeeper doctor --acknowledge-clock"}
	}
	return out, nil
}

// cmdScan indexes metadata for one scope. It reads no file content.
func cmdScan(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("scan", io.Discard)
	scopeRef := fs.String("scope", "", "scope ID or directory name")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *scopeRef == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--scope is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	return a.withInstance(func() (output, error) {
		sc, err := resolveScope(ctx, a.store, *scopeRef)
		if err != nil {
			return output{}, err
		}
		sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), sc.ID)
		if err != nil {
			return output{}, err
		}
		defer sctx.Close()

		guard, err := run.BuildGuard(ctx, a.store, sctx)
		if err != nil {
			return output{}, err
		}
		budget := domain.NewBudget(a.cfg.Runtime.RunSoftSeconds, a.cfg.Runtime.RunReadBudgetBytes,
			0, 0, a.cfg.Runtime.ReadRateBytesPerSecond)
		st, done, err := run.ScanScope(ctx, a.store, a.clock, a.cfg.Runtime.ScanBatchSize, sctx, guard, budget)
		if err != nil {
			return output{}, err
		}
		res := ok()
		res.ScopeID = string(sc.ID)
		res.Data = map[string]any{"scan": st, "epoch_complete": done}
		res.Human = []string{
			fmt.Sprintf("scanned %d directories, %d files (%d bytes)", st.Dirs, st.Files, st.Bytes),
			fmt.Sprintf("  protected directories skipped: %d, entries skipped: %d, unreadable: %d",
				st.ProtectedDirs, st.SkippedEntries, st.Unreadable),
		}
		if done {
			res.Human = append(res.Human, "  scan epoch complete; entries not seen were retired")
		} else {
			res.Human = append(res.Human, "  budget reached before the queue drained; nothing was retired")
		}
		res.Next = []string{"dustkeeper rule preview --id <rule> --revision 1"}
		return res, nil
	})
}

// ---- shared constructors --------------------------------------------------

func (a *app) registrar() *scope.Registrar {
	if a.instance == "" {
		a.instance = "unidentified"
	}
	return &scope.Registrar{
		Store:      a.store,
		Paths:      a.paths,
		InstanceID: a.instance,
		Now:        func() int64 { return a.clock.Wall().UnixNano() },
	}
}

func (a *app) executor(keeper execute.KeeperVerifier) *execute.Executor {
	return &execute.Executor{
		Store: a.store,
		Clock: a.clock,
		Config: execute.ConfigView{
			BufferBytes:    a.cfg.Runtime.IOBufferBytes,
			ReadRatePerSec: a.cfg.Runtime.ReadRateBytesPerSecond,
			MaxFiles:       a.cfg.Runtime.MaxAutoQuarantineFiles,
			MaxBytes:       a.cfg.Runtime.MaxAutoQuarantineBytes,
			MinFreeBytes:   a.cfg.Runtime.MinFreeBytes,
			MinFreePercent: a.cfg.Runtime.MinFreePercent,
		},
		Gate:   scope.PauseGate{StateDir: a.paths.StateDir},
		Keeper: keeper,
	}
}

func (a *app) reconciler() *recovery.Reconciler {
	return &recovery.Reconciler{
		Store:     a.store,
		Clock:     a.clock,
		Registrar: a.registrar(),
		Exec:      a.executor(nil),
	}
}

// resolveScope accepts a scope ID, or a unique directory base name.
func resolveScope(ctx context.Context, st *store.Store, ref string) (store.Scope, error) {
	if ref == "" {
		return store.Scope{}, errs.New(errs.CodeInvalidArgument, "a scope ID is required")
	}
	if sc, err := st.GetScope(ctx, domain.ScopeID(ref)); err == nil {
		return sc, nil
	}
	all, err := st.ListScopes(ctx)
	if err != nil {
		return store.Scope{}, err
	}
	var matches []store.Scope
	for _, sc := range all {
		if filepath.Base(string(sc.RawRoot)) == ref || string(sc.ID) == ref {
			matches = append(matches, sc)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return store.Scope{}, errs.Newf(errs.CodeInvalidArgument,
			"%q matches %d scopes; use the scope ID", ref, len(matches))
	}
	return store.Scope{}, errs.Newf(errs.CodeScopeNotFound, "no scope %q is registered", ref)
}

// guardFor builds the resolved protection set for one scope context.
func guardFor(ctx context.Context, st *store.Store, sctx *execute.ScopeCtx) (*policy.Guard, error) {
	return run.BuildGuard(ctx, st, sctx)
}
