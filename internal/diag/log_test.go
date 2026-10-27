package diag

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
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
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// A record has to carry the identifiers a person needs at 3am: which run, which
// command, which decision, which code. Prose alone is what stdout already had.
func TestARecordCarriesTheDiagnosticFields(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Log("command", "phase", "begin", "command", "run", "run_id", "run_abc", "instance", "inst_1")
	l.Log("result", "phase", "result", "command", "run", "code", "PARTIAL", "control", "stop_scope", "exit", 8)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	recs := readLines(t, filepath.Join(dir, FileName))
	if len(recs) != 2 {
		t.Fatalf("expected two records, got %d", len(recs))
	}
	first := recs[0]
	for _, key := range []string{"time", "msg", "phase", "command", "run_id", "instance"} {
		if _, ok := first[key]; !ok {
			t.Errorf("the first record has no %q: %v", key, first)
		}
	}
	second := recs[1]
	for _, key := range []string{"code", "control", "exit"} {
		if _, ok := second[key]; !ok {
			t.Errorf("the result record has no %q: %v", key, second)
		}
	}
	// The timestamp is UTC with a zone, so two machines' logs can be compared.
	if ts, _ := first["time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("timestamp is not UTC: %q", ts)
	}
}

// The bound is the point: a scheduled task left alone for a year must not fill a disk
// that the program is meant to free.
func TestTheLogRotatesAndStaysBounded(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Each record is roughly 200 bytes; enough records to cross MaxFileBytes twice.
	payload := strings.Repeat("x", 120)
	for i := 0; i < (MaxFileBytes/80)*3; i++ {
		l.Log("filler", "i", i, "detail", payload)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > KeepFiles+1 {
		t.Fatalf("rotation kept %d files, want at most %d", len(entries), KeepFiles+1)
	}
	if len(entries) < 2 {
		t.Fatalf("the log never rotated; %d file(s)", len(entries))
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > MaxFileBytes+4096 {
			t.Fatalf("%s is %d bytes, past the bound %d", e.Name(), info.Size(), MaxFileBytes)
		}
	}
	// The most recent records survive: rotating must not throw away the end.
	live := readLines(t, filepath.Join(dir, FileName))
	if len(live) == 0 {
		t.Fatal("the live file is empty after rotation")
	}
	last := live[len(live)-1]
	if got, _ := last["i"].(float64); int(got) == 0 {
		t.Fatalf("the live file does not hold the newest records: %v", last)
	}
}

// A record that would be enormous is truncated, not dropped: dropping it would lose
// exactly the event that explains a failure, and keeping it whole would let one error
// string grow the log without bound.
func TestAnOversizedFieldIsTruncatedNotDropped(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Log("huge", "detail", strings.Repeat("y", MaxFileBytes+1024), "i", 7)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readLines(t, filepath.Join(dir, FileName))
	if len(recs) != 1 {
		t.Fatalf("expected the record to be kept, got %d records", len(recs))
	}
	detail, _ := recs[0]["detail"].(string)
	if !strings.HasSuffix(detail, truncated) {
		t.Fatalf("the oversized value was not marked as truncated (len %d)", len(detail))
	}
	if len(detail) > MaxFieldBytes+len(truncated) {
		t.Fatalf("the value is still %d bytes", len(detail))
	}
	if got, _ := recs[0]["i"].(float64); int(got) != 7 {
		t.Fatalf("the rest of the record was lost: %v", recs[0])
	}
}

// A nil logger must be inert rather than a panic: the log is optional, the command is
// not.
func TestANilLoggerIsSafe(t *testing.T) {
	var l *Logger
	l.Log("nothing", "k", "v")
	if err := l.Close(); err != nil {
		t.Fatalf("closing a nil logger: %v", err)
	}
	if l.Path() != "" {
		t.Fatalf("a nil logger reports a path: %q", l.Path())
	}
}

// The file must be private: it records which scopes exist and what happened to them.
func TestTheLogFileIsNotWorldReadable(t *testing.T) {
	if _, err := exec.LookPath("stat"); err != nil {
		t.Skip("no stat tool")
	}
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Log("x", "k", "v")
	l.Close()

	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the log is readable by others: %o", perm)
	}
}
