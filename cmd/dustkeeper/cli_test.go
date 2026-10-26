package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/scope"
)

// These tests drive dispatch() directly. It is the same function main calls, so
// the exit statuses and the JSON envelope are the real ones — no subprocess, no
// build step, and every assertion is about the contract a script depends on.

type result struct {
	code   int
	stdout string
	stderr string
	env    map[string]any
}

func cli(t *testing.T, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := dispatch(context.Background(), args, &out, &errb)
	r := result{code: code, stdout: out.String(), stderr: errb.String()}
	if strings.Contains(out.String(), "{") {
		var env map[string]any
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, out.String())
		}
		r.env = env
	}
	return r
}

// newHome points the process at a private state directory for one test.
func newHome(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("DUSTKEEPER_STATE_DIR", filepath.Join(base, "state"))
	t.Setenv("DUSTKEEPER_CONFIG_DIR", filepath.Join(base, "config"))
	return base
}

func (r result) str(t *testing.T, key string) string {
	t.Helper()
	if r.env == nil {
		t.Fatalf("no JSON envelope; stdout was:\n%s", r.stdout)
	}
	v, ok := r.env[key]
	if !ok {
		t.Fatalf("the envelope has no %q; got keys %v", key, keys(r.env))
	}
	s, _ := v.(string)
	return s
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestHelpAndVersionDoNotNeedState(t *testing.T) {
	newHome(t)

	h := cli(t, "help")
	if h.code != 0 {
		t.Fatalf("help exited %d", h.code)
	}
	for _, want := range []string{"init", "doctor", "rule", "recover", "schedule render"} {
		if !strings.Contains(h.stdout, want) {
			t.Errorf("help does not mention %q", want)
		}
	}

	v := cli(t, "version", "--json")
	if v.code != 0 {
		t.Fatalf("version exited %d: %s", v.code, v.stdout)
	}
	if got := v.env["schema_version"]; got != float64(schemaVersion) {
		t.Errorf("schema_version = %v, want %d", got, schemaVersion)
	}
	if v.str(t, "status") != "ok" {
		t.Errorf("status = %q", v.str(t, "status"))
	}
	if v.env["run_id"] == "" {
		t.Error("the envelope must carry a run_id")
	}
	data, ok := v.env["data"].(map[string]any)
	if !ok {
		t.Fatalf("version has no data object: %v", v.env["data"])
	}
	for _, k := range []string{"version", "platform", "go", "commit", "build_tags", "sqlite", "driver", "schema"} {
		if data[k] == nil || data[k] == "" {
			t.Errorf("version data has no %q", k)
		}
	}
}

func TestJSONFlagIsGlobalAndStripped(t *testing.T) {
	newHome(t)
	// The flag may appear before the command, after it, or between subcommands.
	for _, args := range [][]string{
		{"--json", "version"},
		{"version", "--json"},
	} {
		r := cli(t, args...)
		if r.env == nil {
			t.Fatalf("%v produced no JSON envelope:\n%s", args, r.stdout)
		}
		if r.code != 0 {
			t.Fatalf("%v exited %d", args, r.code)
		}
	}
}

func TestUnknownCommandIsAnArgumentError(t *testing.T) {
	newHome(t)
	r := cli(t, "--json", "frobnicate")
	if r.code != 2 {
		t.Fatalf("exit = %d, want 2", r.code)
	}
	if r.str(t, "code") != "INVALID_ARGUMENT" {
		t.Errorf("code = %q", r.str(t, "code"))
	}
	if r.str(t, "status") != "error" {
		t.Errorf("status = %q", r.str(t, "status"))
	}
	// A machine caller must be told what to do next, not left to guess.
	if _, ok := r.env["next_steps"]; !ok {
		t.Error("an unknown command should point at `dustkeeper help`")
	}
}

func TestMissingArgumentsAreRejectedBeforeTouchingDisk(t *testing.T) {
	newHome(t)
	cases := [][]string{
		{"scan"},
		{"scope"},
		{"scope", "add"},
		{"rule", "import"},
		{"plan", "create"},
		{"quarantine", "restore"},
		{"protect", "add"},
		{"schedule", "render"},
		{"events", "--limit", "0"},
	}
	for _, args := range cases {
		r := cli(t, append([]string{"--json"}, args...)...)
		if r.code != 2 {
			t.Errorf("%v exited %d, want 2", args, r.code)
			continue
		}
		if r.str(t, "code") != "INVALID_ARGUMENT" {
			t.Errorf("%v code = %q", args, r.str(t, "code"))
		}
	}
}

func TestStateCommandsRequireInit(t *testing.T) {
	newHome(t)
	for _, args := range [][]string{{"status"}, {"doctor"}, {"scope", "list"}} {
		r := cli(t, append([]string{"--json"}, args...)...)
		if r.code != 2 {
			t.Errorf("%v before init exited %d, want 2", args, r.code)
			continue
		}
		if r.str(t, "code") != "INVALID_ARGUMENT" {
			t.Errorf("%v code = %q", args, r.str(t, "code"))
		}
		if !strings.Contains(strings.ToLower(r.str(t, "message")), "init") {
			t.Errorf("%v should tell the user to run init; message was %q", args, r.str(t, "message"))
		}
	}
}

func TestInitCreatesTheStateDirectory(t *testing.T) {
	base := newHome(t)
	r := cli(t, "init", "--json")
	if r.code != 0 {
		t.Fatalf("init exited %d: %s", r.code, r.stdout)
	}

	state := filepath.Join(base, "state")
	if fi, err := os.Stat(state); err != nil {
		t.Fatalf("state directory: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("state directory mode = %o, want 700", perm)
	}
	for _, name := range []string{"state.db", "instance.json"} {
		if _, err := os.Stat(filepath.Join(state, name)); err != nil {
			t.Errorf("init did not create %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "config", "config.json")); err != nil {
		t.Errorf("init did not write the default config: %v", err)
	}

	// Running it twice is not an error: it is the documented way to repair a
	// missing directory.
	if again := cli(t, "init", "--json"); again.code != 0 {
		t.Fatalf("second init exited %d: %s", again.code, again.stdout)
	}
}

func TestPauseReportsThatItOnlyRequestedAPause(t *testing.T) {
	newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init: %d %s", r.code, r.stdout)
	}

	p := cli(t, "pause", "--json")
	if p.code != 6 {
		t.Fatalf("pause exited %d, want 6: %s", p.code, p.stdout)
	}
	if p.str(t, "code") != "PAUSE_REQUESTED" {
		t.Errorf("pause code = %q, want PAUSE_REQUESTED", p.str(t, "code"))
	}
	if p.str(t, "status") != "blocked" {
		t.Errorf("pause status = %q, want blocked", p.str(t, "status"))
	}
	// The message must not claim that running work has stopped.
	if msg := p.str(t, "message"); !strings.Contains(msg, "safe boundary") {
		t.Errorf("pause message does not say it waits for a safe boundary: %q", msg)
	}

	// Pausing twice is idempotent and says so.
	if again := cli(t, "pause", "--json"); again.code != 6 {
		t.Errorf("second pause exited %d", again.code)
	}

	res := cli(t, "resume", "--json")
	if res.code != 0 {
		t.Fatalf("resume exited %d: %s", res.code, res.stdout)
	}
	if res.str(t, "code") != "OK" {
		t.Errorf("resume code = %q", res.str(t, "code"))
	}
}

func TestStatusReportsAConsistentShape(t *testing.T) {
	newHome(t)
	cli(t, "init", "--json")

	r := cli(t, "status", "--json")
	if r.code != 0 {
		t.Fatalf("status exited %d: %s", r.code, r.stdout)
	}
	data, ok := r.env["data"].(map[string]any)
	if !ok {
		t.Fatalf("status has no data object")
	}
	// The fields a monitoring script reads must exist even when they are empty.
	for _, k := range []string{"paused", "scopes", "pending_ops", "stats", "state_dir"} {
		if _, ok := data[k]; !ok {
			t.Errorf("status data has no %q", k)
		}
	}
	if n, ok := data["pending_ops"].(float64); !ok || n != 0 {
		t.Errorf("a fresh installation reports %v pending operations", data["pending_ops"])
	}
}

func TestScopeLifecycleThroughTheCLI(t *testing.T) {
	base := newHome(t)
	cli(t, "init", "--json")

	dir := filepath.Join(base, "tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	add := cli(t, "scope", "add", "--path", dir, "--kind", "observe", "--name", "demo", "--json")
	if add.code != 0 {
		t.Fatalf("scope add exited %d: %s", add.code, add.stdout)
	}
	if add.str(t, "scope_id") != "demo" {
		t.Errorf("scope_id = %q", add.str(t, "scope_id"))
	}

	list := cli(t, "scope", "list", "--json")
	if list.code != 0 {
		t.Fatalf("scope list exited %d: %s", list.code, list.stdout)
	}
	if !strings.Contains(list.stdout, "demo") {
		t.Errorf("scope list does not mention the new scope:\n%s", list.stdout)
	}

	// Registering is not authorising: an observe scope can be scanned but never
	// changed, so a rule for it cannot be activated into a mutation.
	r := cli(t, "rule", "preview", "--id", "nope", "--json")
	if r.code != 4 {
		t.Errorf("previewing an unknown rule exited %d, want 4", r.code)
	}
	if r.str(t, "code") != "RULE_NOT_FOUND" {
		t.Errorf("code = %q, want RULE_NOT_FOUND", r.str(t, "code"))
	}
}

func TestRecoverInspectIsReadOnlyAndExplicit(t *testing.T) {
	newHome(t)
	cli(t, "init", "--json")

	r := cli(t, "recover", "inspect", "--json")
	if r.code != 0 {
		t.Fatalf("recover inspect exited %d: %s", r.code, r.stdout)
	}
	data, ok := r.env["data"].(map[string]any)
	if !ok {
		t.Fatal("recover inspect has no data object")
	}
	if n, ok := data["count"].(float64); !ok || n != 0 {
		t.Errorf("a fresh installation has %v pending operations, want 0", data["count"])
	}
}

// Section 12.5: resume must hold the execution lock. Clearing the pause marker
// while a run is mid-batch would widen authority underneath it.
func TestResumeNeedsTheExecutionLock(t *testing.T) {
	base := newHome(t)
	if r := cli(t, "init"); r.code != 0 {
		t.Fatalf("init exited %d:\n%s", r.code, r.stdout)
	}
	if r := cli(t, "pause"); r.code != 6 {
		t.Fatalf("pause exited %d:\n%s", r.code, r.stdout)
	}

	lock, err := scope.InstanceLock(filepath.Join(base, "state"))
	if err != nil {
		t.Fatalf("hold instance lock: %v", err)
	}
	defer lock.Release()

	r := cli(t, "resume", "--json")
	if r.code != 3 {
		t.Fatalf("resume while a run holds the lock exited %d, want 3 (INSTANCE_BUSY):\n%s", r.code, r.stdout)
	}
	if got := r.str(t, "code"); got != string(errs.CodeInstanceBusy) {
		t.Errorf("code = %q, want %q", got, errs.CodeInstanceBusy)
	}
	if _, err := os.Stat(filepath.Join(base, "state", "pause.request")); err != nil {
		t.Errorf("a refused resume must leave the pause marker in place: %v", err)
	}
}

// The candidate list must never contain a path the operator is told to protect,
// and the envelope must stay one object with no extra logging noise.
func TestEnvelopeHasNoNoise(t *testing.T) {
	newHome(t)
	r := cli(t, "version", "--json")
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.More() {
		t.Fatal("stdout carried a second JSON value after the envelope")
	}
	for _, k := range []string{"schema_version", "status", "code", "message", "retryable", "run_id"} {
		if _, ok := first[k]; !ok {
			t.Errorf("the envelope has no %q", k)
		}
	}
}
