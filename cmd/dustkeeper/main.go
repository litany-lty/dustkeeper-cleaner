// Command dustkeeper is the phase-one bounded file cleaner.
//
// The binary is the only entry point. It never spawns a shell, never accepts a
// path from JSON input as authority, and never keeps a background process
// alive: every command does one bounded thing and exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"dustkeeper/internal/config"
	"dustkeeper/internal/diag"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"

	"dustkeeper/migrations"
)

// schemaVersion is the version of the machine-readable envelope. It changes
// only when the envelope shape changes incompatibly.
const schemaVersion = 1

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// app carries the process-wide state every command shares.
type app struct {
	paths  config.Paths
	cfg    config.Config
	clock  domain.Clock
	runID  string
	cmd    string
	json   bool
	stdout io.Writer
	stderr io.Writer

	store    *store.Store
	instance string
	// log is the bounded operational log. It is nil until a mutating command opens
	// it, and a nil logger drops records rather than panicking.
	log *diag.Logger
}

// output is what a command returns. Every field ends up in the JSON envelope,
// so a machine caller never has to parse prose.
type output struct {
	Status   string    `json:"status"`
	Code     errs.Code `json:"code"`
	Message  string    `json:"message,omitempty"`
	ScopeID  string    `json:"scope_id,omitempty"`
	PlanID   string    `json:"plan_id,omitempty"`
	ObjectID string    `json:"object_id,omitempty"`
	Counts   *counts   `json:"counts,omitempty"`
	Data     any       `json:"data,omitempty"`
	Next     []string  `json:"next_steps,omitempty"`
	Human    []string  `json:"-"`
}

// counts is the plan result shape section 5.5 requires.
type counts struct {
	Planned   int `json:"planned_count"`
	Completed int `json:"completed_count"`
	Skipped   int `json:"skipped_count"`
	Failed    int `json:"failed_count"`
}

func ok() output { return output{Status: string(errs.StatusOK), Code: errs.CodeOK} }

// fail converts an error into an output with the right status and code.
func fail(err error) output {
	code := errs.CodeOf(err)
	return output{
		Status:  string(errs.StatusOf(code)),
		Code:    code,
		Message: messageOf(err),
	}
}

func messageOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// --json is global and may appear anywhere; strip it before the command
	// flag sets see the arguments.
	jsonOut := false
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "--json", "-json":
			jsonOut = true
		default:
			filtered = append(filtered, a)
		}
	}
	args = filtered

	a := &app{json: jsonOut, runID: domain.NewID("run"), stdout: stdout, stderr: stderr, clock: domain.NewSystemClock()}

	paths, err := config.ResolvePaths()
	if err != nil {
		return a.emit(fail(err))
	}
	a.paths = paths

	cfg, _, err := config.Load(paths.ConfigFile)
	if err != nil {
		return a.emit(fail(err))
	}
	a.cfg = cfg

	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	if args[0] == "version" {
		return a.emit(versionOutput())
	}

	fn, rest, found := lookup(args[0], args[1:])
	if !found {
		out := fail(errs.Newf(errs.CodeInvalidArgument, "unknown command %q; run dustkeeper help", args[0]))
		out.Next = []string{"dustkeeper help"}
		return a.emit(out)
	}

	// The command name is the key a person greps for. Two-word commands are kept
	// together ("plan apply", not "plan"), because the pair is what ran.
	a.cmd = args[0]
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		a.cmd = a.cmd + " " + args[1]
	}

	out, err := fn(ctx, a, rest)
	if a.store != nil {
		a.store.Close()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			out = fail(errs.New(errs.CodeCancelled, "interrupted; nothing further was started"))
		} else if out.Code == "" {
			out = fail(err)
		} else {
			// The command reported a partial result and an explanation.
			out.Message = messageOf(err)
		}
	}
	code := a.emit(out)
	a.closeLog()
	return code
}

// closeLog flushes the operational log exactly once.
func (a *app) closeLog() {
	if a.log == nil {
		return
	}
	if err := a.log.Close(); err != nil {
		fmt.Fprintf(a.stderr, "warning: could not close the operational log: %v\n", err)
	}
	a.log = nil
}

// nextStepsFor turns an error code into the next thing to run. It is a fallback:
// a command that knows more about its own failure has already filled Next, and
// that answer wins.
func nextStepsFor(code errs.Code) []string {
	switch code {
	case errs.CodeInvalidArgument, errs.CodeUnknownField, errs.CodeDuplicateKey,
		errs.CodeRuleAmbiguous, errs.CodeRuleUnsupported, errs.CodeDSLInvalid,
		errs.CodeNeedsInput:
		return []string{"dustkeeper help"}
	case errs.CodeScopeNotFound:
		return []string{"dustkeeper scope list"}
	case errs.CodePlanNotFound:
		return []string{"dustkeeper plan create --scope <scope_id> --rule <rule_id>"}
	case errs.CodeItemNotFound:
		return []string{"dustkeeper quarantine list"}
	case errs.CodeRuleNotFound:
		return []string{"dustkeeper rule import --file <rules.json>"}
	case errs.CodeInstanceBusy:
		return []string{"wait for the running task, then retry"}
	case errs.CodeAccessDenied, errs.CodeScopeRefused:
		return []string{"check the path exists and this user may read it, then retry"}
	case errs.CodeRecoveryRequired, errs.CodeRestoreConflict, errs.CodeHeld:
		return []string{"dustkeeper recover inspect"}
	case errs.CodeClockUntrusted:
		return []string{"dustkeeper doctor"}
	case errs.CodeDiskPressure:
		return []string{"free space, then retry"}
	case errs.CodeStateWriteFailed, errs.CodeDBError, errs.CodeIOError, errs.CodeDurabilityFailed:
		return []string{"dustkeeper doctor"}
	case errs.CodeModelUnavailable:
		return []string{"retry later; existing rules are unaffected"}
	case errs.CodePartial, errs.CodeBudgetExhausted:
		return []string{"run again to continue; progress is recorded"}
	case errs.CodeCancelled:
		return []string{"run again when ready"}
	}
	return []string{"dustkeeper doctor"}
}

// emit writes the envelope (or the human rendering) and returns the exit code.
func (a *app) emit(out output) int {
	if out.Status == "" {
		out.Status = string(errs.StatusOK)
	}
	if out.Code == "" {
		out.Code = errs.CodeOK
	}
	exit := 0
	if out.Code != errs.CodeOK {
		exit = errs.Exit(out.Code)
	}
	// Every failure ends with something to try, in the envelope and in the log.
	// A code on its own tells an operator what went wrong but not what to do, and
	// that is exactly the moment they are reading this.
	if len(out.Next) == 0 && out.Code != errs.CodeOK {
		out.Next = nextStepsFor(out.Code)
	}
	// One record per command, with what a person looking at yesterday needs: which
	// run, what was decided, how it ended, and what to do next. The same facts the
	// envelope carries, in a file that outlives the terminal.
	if a.log != nil {
		kv := []any{
			"phase", "result",
			"command", a.cmd,
			"run_id", a.runID,
			"instance", a.instance,
			"code", string(out.Code),
			"status", out.Status,
			"exit", exit,
			"retryable", errs.RetryableCode(out.Code),
			"scope_id", out.ScopeID,
			"plan_id", out.PlanID,
			"object_id", out.ObjectID,
			"next_steps", strings.Join(out.Next, " | "),
			"message", out.Message,
		}
		if out.Counts != nil {
			kv = append(kv, "planned", out.Counts.Planned, "completed", out.Counts.Completed,
				"skipped", out.Counts.Skipped, "failed", out.Counts.Failed)
		}
		a.log.Log("result", kv...)
	}
	if a.json {
		env := map[string]any{
			"schema_version": schemaVersion,
			"status":         out.Status,
			"code":           string(out.Code),
			// Section 5.5 lists message among the keys every document carries.
			// An empty string is information ("no explanation needed"); an
			// absent key forces every consumer to branch on presence.
			"message":   out.Message,
			"retryable": errs.RetryableCode(out.Code),
			"run_id":    a.runID,
		}
		// Plan results carry the four counts as top-level keys, as named in
		// section 5.5, so a script does not have to know about a nested object.
		if out.Counts != nil {
			env["planned_count"] = out.Counts.Planned
			env["completed_count"] = out.Counts.Completed
			env["skipped_count"] = out.Counts.Skipped
			env["failed_count"] = out.Counts.Failed
		}
		for k, v := range map[string]any{
			"scope_id":   out.ScopeID,
			"plan_id":    out.PlanID,
			"object_id":  out.ObjectID,
			"data":       out.Data,
			"next_steps": out.Next,
		} {
			if !isZero(v) {
				env[k] = v
			}
		}
		enc := json.NewEncoder(a.stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(env); err != nil {
			fmt.Fprintf(a.stderr, "write output: %v\n", err)
			return errs.Exit(errs.CodeStateWriteFailed)
		}
	} else {
		for _, line := range out.Human {
			fmt.Fprintln(a.stdout, line)
		}
		if out.Message != "" {
			fmt.Fprintf(a.stdout, "%s: %s\n", out.Code, out.Message)
		}
		for _, n := range out.Next {
			fmt.Fprintf(a.stdout, "next: %s\n", n)
		}
	}
	return exit
}

func isZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case *counts:
		return t == nil
	case []string:
		return len(t) == 0
	}
	return false
}

func versionOutput() output {
	return output{
		Status:  string(errs.StatusOK),
		Code:    errs.CodeOK,
		Message: "",
		Data: map[string]any{
			"version":    version,
			"commit":     commit,
			"build_tags": buildTags,
			"built_at":   buildStamp,
			"go":         goVersion(),
			"policy":     policyVersionText(),
			"platform":   platformName(),
			"sqlite":     sqliteVersion(),
			"driver":     driverVersion(),
			"schema":     migrations.CurrentVersion,
		},
		Human: []string{
			fmt.Sprintf("dustkeeper %s (%s, SQLite %s, commit %s)", version, platformName(), sqliteVersion(), commit),
		},
	}
}

// lookup resolves one command name plus its arguments.
func lookup(name string, args []string) (func(context.Context, *app, []string) (output, error), []string, bool) {
	switch name {
	case "init":
		return cmdInit, args, true
	case "doctor":
		return cmdDoctor, args, true
	case "scope":
		return cmdScope, args, true
	case "scan":
		return cmdScan, args, true
	case "rule":
		return cmdRule, args, true
	case "plan":
		return cmdPlan, args, true
	case "dedup":
		return cmdDedup, args, true
	case "quarantine":
		return cmdQuarantine, args, true
	case "protect":
		return cmdProtect, args, true
	case "purge":
		return cmdPurge, args, true
	case "recover":
		return cmdRecover, args, true
	case "status":
		return cmdStatus, args, true
	case "events":
		return cmdEvents, args, true
	case "run":
		return cmdRun, args, true
	case "pause":
		return cmdPause, args, true
	case "resume":
		return cmdResume, args, true
	case "schedule":
		return cmdSchedule, args, true
	}
	return nil, nil, false
}

// ---- shared helpers -------------------------------------------------------

// openStore opens the state database, creating nothing. Commands that must
// create it call init first, so a typo in the state directory never produces a
// fresh, empty database that looks like "no scopes".
func (a *app) openStore() error {
	if a.store != nil {
		return nil
	}
	if _, err := os.Stat(a.paths.DBFile); err != nil {
		if os.IsNotExist(err) {
			return errs.Newf(errs.CodeInvalidArgument,
				"no state database at %s; run `dustkeeper init` first", a.paths.DBFile)
		}
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	st, err := store.Open(a.paths.DBFile)
	if err != nil {
		return err
	}
	a.store = st
	if id, err := st.MetaGet(context.Background(), "instance_id"); err == nil {
		a.instance = id
	}
	return nil
}

// flagset builds a quiet flag set: usage errors must become structured output,
// not a wall of usage text on stderr.
func flagset(name string, errOut io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {}
	return fs
}

// parseFlags wraps Parse so every caller gets the same INVALID_ARGUMENT code.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return errs.Newf(errs.CodeInvalidArgument, "%s: %v", fs.Name(), err)
	}
	if fs.NArg() > 0 {
		return errs.Newf(errs.CodeInvalidArgument, "unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// withInstance runs fn while holding the instance execution lock. Any command
// that can change a business file or commit an intent takes it first, so two
// runs never interleave over one state directory.
//
// It also opens the operational log, because this is the boundary every mutating
// command crosses: read-only commands (help, status) must not create files, and a
// command that changes state must leave a record that outlives the process.
func (a *app) withInstance(fn func() (output, error)) (output, error) {
	lock, err := lockInstance(a.paths.StateDir)
	if err != nil {
		return output{}, err
	}
	defer lock()
	a.openLog()
	a.log.Log("command", "phase", "begin",
		"command", a.cmd, "run_id", a.runID, "instance", a.instance)
	return fn()
}

// openLog prepares the log once. A failure is reported on stderr and ignored: the
// command's own result matters more than the record of it, and refusing to work
// because a log file cannot be opened would be its own kind of outage.
func (a *app) openLog() {
	if a.log != nil {
		return
	}
	l, err := diag.Open(a.paths.LogDir)
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: the operational log is unavailable: %v\n", err)
		return
	}
	a.log = l
}

var helpText = `dustkeeper — bounded, reversible file cleanup

Usage:
  dustkeeper <command> [subcommand] [flags]
  dustkeeper help | version

Commands:
  init                     initialise the local state directory
  doctor                   check permissions, database, capabilities and clock
  scope add|list|show|pause|resume
                           register and manage scopes
  scan --scope ID          metadata-only scan and candidate index
  rule draft|import|show|validate|preview|activate|authorize|revoke|disable
                           rule lifecycle; activation is not permission
  plan create|show|apply   freeze and execute a quarantine plan
  dedup plan               exact duplicate detection and plan
  quarantine list|restore  inspect and restore isolated objects
  protect add|list|remove  manage path protections
  purge plan|show|apply    separate due-purge plan and execution
  recover inspect|reconcile|adopt
                           finish or explain interrupted operations; adopt
                           converges an item the automatic path held
  status | events          run state and audit trail
  run                      reconcile, scan, apply granted work
  pause | resume           global safety pause marker
  schedule render          print systemd / launchd templates

Global flags:
  --json                   machine-readable output with schema_version

Exit codes: 0 ok, 2 arguments/DSL, 3 busy, 4 stale, 5 unsupported,
            6 recovery needed, 7 storage, 8 model, 9 partial, 130 cancelled
`

func platformName() string { return platform() }

func policyVersionText() string { return fmt.Sprintf("%d", 1) }

// absPath normalises a user-supplied path without touching the filesystem.
func absPath(p string) (string, error) {
	if p == "" {
		return "", errs.New(errs.CodeInvalidArgument, "a path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err)
	}
	return abs, nil
}

// splitList accepts both a repeated flag and a comma-separated value.
func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
