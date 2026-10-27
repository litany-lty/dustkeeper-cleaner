package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustCLI runs one command and fails the test when it does not exit 0.
func mustCLI(t *testing.T, args ...string) result {
	t.Helper()
	r := cli(t, args...)
	if r.code != 0 {
		t.Fatalf("%v exited %d\nstdout:\n%s\nstderr:\n%s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

// dataOf returns the data object of an envelope.
func dataOf(t *testing.T, r result) map[string]any {
	t.Helper()
	v, ok := r.env["data"]
	if !ok {
		t.Fatalf("the envelope has no data object; stdout:\n%s", r.stdout)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want an object", v)
	}
	return m
}

func dataString(t *testing.T, r result, key string) string {
	t.Helper()
	v, ok := dataOf(t, r)[key]
	if !ok {
		t.Fatalf("data has no %q; got %v", key, dataOf(t, r))
	}
	s, _ := v.(string)
	if s == "" {
		t.Fatalf("data[%q] is empty", key)
	}
	return s
}

// TestQuarantineListShowsWhatIsInTheVaultAndWhetherItCanComeBack drives the real
// command line: register, import, preview, activate, scan, plan, apply, list.
//
// The list used to show quarantined objects only, so a held or half-reconciled
// object was invisible - and "the list is empty" reads as "the file is gone".
// Every state that can still hold an object is listed now, together with the
// reason a restore cannot proceed.
func TestQuarantineListShowsWhatIsInTheVaultAndWhetherItCanComeBack(t *testing.T) {
	base := newHome(t)
	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	victim := filepath.Join(tree, "old.zip")
	if err := os.WriteFile(victim, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	mustCLI(t, "init", "--json")
	mustCLI(t, "scope", "add", "--path", tree, "--kind", "managed_disposable", "--name", "exports", "--json")

	rulePath := filepath.Join(base, "rule.json")
	dsl := `{"dsl_version":1,"scope_id":"exports","action":"quarantine",` +
		`"match":{"min_mtime_age_seconds":2592000,"min_size_bytes":0,"max_size_bytes":1073741824,` +
		`"relative_dir_prefixes":[],"basename_patterns":["*.zip"]},` +
		`"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`
	if err := os.WriteFile(rulePath, []byte(dsl), 0o600); err != nil {
		t.Fatalf("write rule: %v", err)
	}
	// The scan comes first: a preview reads the candidate index, and an empty
	// index would make the rest of the test a no-op.
	mustCLI(t, "scan", "--scope", "exports", "--json")
	mustCLI(t, "rule", "import", "--file", rulePath, "--id", "r1", "--json")
	prev := mustCLI(t, "rule", "preview", "--id", "r1", "--revision", "1", "--json")
	mustCLI(t, "rule", "activate", "--id", "r1", "--revision", "1",
		"--digest", dataString(t, prev, "review_digest"), "--json")

	created := mustCLI(t, "plan", "create", "--rule", "r1", "--json")
	mustCLI(t, "plan", "apply",
		"--id", dataString(t, created, "plan_id"),
		"--digest", dataString(t, created, "digest"), "--json")

	listed := mustCLI(t, "quarantine", "list", "--json")
	objects, ok := dataOf(t, listed)["objects"].([]any)
	if !ok {
		t.Fatalf("objects is not a list; data: %v", dataOf(t, listed))
	}
	if len(objects) != 1 {
		t.Fatalf("listed %d objects, want the one that was quarantined", len(objects))
	}
	row, ok := objects[0].(map[string]any)
	if !ok {
		t.Fatalf("an object row is %T", objects[0])
	}
	if row["state"] != "quarantined" {
		t.Fatalf("state = %v, want quarantined", row["state"])
	}
	if row["restorable"] != true {
		t.Fatalf("a quarantined object with evidence is not restorable: %v", row)
	}
	if _, ok := row["blocked_reason"]; ok {
		t.Fatalf("a restorable object was given a blocked reason: %v", row["blocked_reason"])
	}
	if row["relative_path"] != "old.zip" {
		t.Fatalf("relative_path = %v", row["relative_path"])
	}

	// And the listed identifier is the one restore accepts.
	itemID, _ := row["item_id"].(string)
	mustCLI(t, "quarantine", "restore", "--object", itemID, "--json")
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the object was not restored to %s: %v", victim, err)
	}
}
