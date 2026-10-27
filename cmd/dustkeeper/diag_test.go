package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordKeys is the closed set of keys a record may carry. It is written out
// rather than inferred on purpose: a diagnostic log that starts carrying an
// extra field is a change somebody has to agree to, because the field after that
// one might be a file's contents.
var recordKeys = map[string]bool{
	"time": true, "level": true, "msg": true, "phase": true,
	"command": true, "run_id": true, "instance": true,
	"code": true, "status": true, "exit": true, "retryable": true,
	"scope_id": true, "plan_id": true, "object_id": true,
	"next_steps": true, "message": true,
	"planned": true, "completed": true, "skipped": true, "failed": true,
	"control": true, "failure_code": true,
	"clock_trusted": true, "clock_note": true, "clock_reassessed": true,
	"scopes": true, "scopes_not_reached": true,
	"quarantined": true, "purged": true, "read_bytes": true,
	"disk_pressure": true, "pressure_note": true, "failures": true,
	"pending_examined": true, "pending_completed": true, "pending_aborted": true,
	"pending_held": true, "pending_parked": true, "pending_unknown": true,
	"scanned": true, "scan_complete": true, "scan_interrupted": true,
	"deep_scan_required": true, "due_not_authorised": true,
	"purge_skipped": true, "quarantine_skipped": true,
	"op_id": true, "item_id": true, "action": true, "outcome": true, "detail": true,
}

func readRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "dustkeeper.jsonl"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not one JSON object: %v\n%s", err, line)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return out
}

// R13: the log has to answer "which run, what phase, what code, what next" for a
// command that failed, not only the one that worked.
func TestTheLogRecordsTheRunAndItsConclusion(t *testing.T) {
	base := newHome(t)
	stateDir := filepath.Join(base, "state")

	// A command that fails after opening the installation: no scope has that ID.
	// It goes through the instance boundary, which is where the log opens.
	cli(t, "init", "--json")
	r := cli(t, "dedup", "plan", "--scope", "scp_doesnotexist", "--json")
	if r.code == 0 {
		t.Fatalf("expected a failing command, stdout:\n%s", r.stdout)
	}
	runID := r.str(t, "run_id")

	recs := readRecords(t, filepath.Join(stateDir, "logs"))
	if len(recs) == 0 {
		t.Fatal("the log is empty: the command recorded nothing")
	}
	var begin, result map[string]any
	for _, rec := range recs {
		switch rec["phase"] {
		case "begin":
			begin = rec
		case "result":
			result = rec
		}
	}
	if begin == nil {
		t.Fatalf("no begin record; got %d records", len(recs))
	}
	if result == nil {
		t.Fatalf("no result record; got %d records", len(recs))
	}
	if begin["run_id"] != runID || result["run_id"] != runID {
		t.Fatalf("the log names a different run than the envelope: log %v/%v, envelope %s",
			begin["run_id"], result["run_id"], runID)
	}
	if got := result["exit"]; got == float64(0) {
		t.Fatal("a failed command logged exit 0")
	}
	if got := result["code"]; got == "OK" || got == "" {
		t.Fatalf("the result record carries code %v", got)
	}
	if s, _ := result["next_steps"].(string); s == "" {
		t.Fatal("the result record offers no next step for a failure")
	}
	if s, _ := result["message"].(string); s == "" {
		t.Fatal("the result record carries no explanation")
	}
	if s, _ := result["command"].(string); !strings.HasPrefix(s, "dedup") {
		t.Fatalf("the result record names command %q", s)
	}
}

// R13: a log that leaks a credential or a payload is worse than no log. This is
// checked against the whole log directory, including rotated files, because a
// leak does not stop being one after rotation.
func TestTheLogNeverCarriesTheModelKeyOrFileContents(t *testing.T) {
	base := newHome(t)
	stateDir := filepath.Join(base, "state")
	secret := "sk-live-1f4c2a9b8d7e6f5a4b3c2d1e0f9a8b7c"
	payload := "PAYLOAD-MARKER-9c1e7b3a-should-never-be-logged"
	t.Setenv("DUSTKEEPER_MODEL_API_KEY", secret)

	// Something real to log about, plus a file whose bytes must not travel.
	root := filepath.Join(base, "scope")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "old.zip"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	cli(t, "init", "--json")
	cli(t, "scope", "add", "--path", root, "--kind", "managed_disposable", "--json")
	cli(t, "scan", "--scope", "scp_missing", "--json")
	cli(t, "recover", "inspect", "--root", root, "--json")

	dir := filepath.Join(stateDir, "logs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the log directory is empty")
	}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, bad := range []string{secret, payload} {
			if strings.Contains(string(body), bad) {
				t.Fatalf("%s contains %q; a diagnostic log must never carry a credential or a payload",
					e.Name(), bad)
			}
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s is mode %04o; the log names paths and must not be world readable", e.Name(), perm)
		}
	}
}

// R13: every record stays inside the documented key set. This is the assertion
// that keeps the previous test true after somebody adds a field.
func TestEveryRecordUsesTheDocumentedKeys(t *testing.T) {
	base := newHome(t)
	cli(t, "init", "--json")
	cli(t, "dedup", "plan", "--scope", "scp_doesnotexist", "--json")

	recs := readRecords(t, filepath.Join(base, "state", "logs"))
	if len(recs) == 0 {
		t.Fatal("the log is empty")
	}
	for _, rec := range recs {
		for k := range rec {
			if !recordKeys[k] {
				t.Fatalf("record %v carries undocumented key %q", rec["msg"], k)
			}
		}
	}
}
