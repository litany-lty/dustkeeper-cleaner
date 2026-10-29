package main

// C02: a held object with complete captured evidence has to be restorable through
// the command the product tells the user to run.
//
// The three places that answer "can this come back" disagreed: the list called
// such an object restorable, the executor restored it, and this CLI refused it
// before the executor was ever called. The protocol tests called the executor
// directly, which is exactly why the gap survived them - so this test goes through
// the real dispatch path and checks the filesystem afterwards.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

// heldObjectFixture quarantines one file and leaves the item held with the
// evidence a miscapture records: the object is in the vault, nothing is pending,
// and the recorded identity is the object that moved.
func heldObjectFixture(t *testing.T) (base string, itemID string) {
	t.Helper()
	base = newHome(t)
	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	victim := filepath.Join(tree, "old.zip")
	if err := os.WriteFile(victim, []byte("payload the operator wants back"), 0o644); err != nil {
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
	if !ok || len(objects) != 1 {
		t.Fatalf("the vault does not hold exactly one object: %v", dataOf(t, listed)["objects"])
	}
	first, _ := objects[0].(map[string]any)
	itemID, _ = first["item_id"].(string)
	if itemID == "" {
		t.Fatalf("the list entry has no item id: %v", first)
	}

	// What a miscapture leaves behind: a verdict about the object, its evidence
	// recorded, and no operation open.
	st, err := store.OpenExisting(filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	if err := st.MarkItemState(t.Context(), domain.ItemID(itemID), domain.StateHeld, "MISCAPTURE"); err != nil {
		st.Close()
		t.Fatalf("hold the item: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return base, itemID
}

func TestQuarantineRestoreTakesAHeldObjectOutOfTheVault(t *testing.T) {
	base, itemID := heldObjectFixture(t)

	// The list has to agree with the command, or one of the two is lying.
	listed := mustCLI(t, "quarantine", "list", "--json")
	objects, _ := dataOf(t, listed)["objects"].([]any)
	first, _ := objects[0].(map[string]any)
	if restorable, _ := first["restorable"].(bool); !restorable {
		t.Fatalf("the list does not offer the held object back: %v", first)
	}
	if blocked, _ := first["blocked"].(string); blocked != "" {
		t.Fatalf("the list blocks a restorable object: %v", first)
	}

	// The original slot is occupied by another file, so the operator picks an
	// empty destination - the case the list is telling them is possible.
	if err := os.MkdirAll(filepath.Join(base, "tree", "recovered"), 0o755); err != nil {
		t.Fatalf("mkdir destination: %v", err)
	}
	replacement := filepath.Join(base, "tree", "old.zip")
	if err := os.WriteFile(replacement, []byte("a newer file at the old path"), 0o644); err != nil {
		t.Fatalf("write the replacement: %v", err)
	}

	res := mustCLI(t, "quarantine", "restore", "--object", itemID,
		"--destination-relative", "recovered/old.zip", "--json")
	if got := dataString(t, res, "restored"); got != "recovered/old.zip" {
		t.Fatalf("restored = %q", got)
	}

	body, err := os.ReadFile(filepath.Join(base, "tree", "recovered", "old.zip"))
	if err != nil {
		t.Fatalf("read the restored file: %v", err)
	}
	if string(body) != "payload the operator wants back" {
		t.Fatalf("the restored file holds %q", body)
	}
	kept, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatalf("read the replacement: %v", err)
	}
	if string(kept) != "a newer file at the old path" {
		t.Fatalf("the file at the original path was overwritten: %q", kept)
	}
	after := mustCLI(t, "quarantine", "list", "--json")
	if n, _ := dataOf(t, after)["count"].(float64); n != 0 {
		t.Fatalf("%v object(s) are still listed after a restore", dataOf(t, after)["count"])
	}
}

// The safety checks around that exit stay in place: an occupied destination is
// still refused, and nothing is overwritten.
func TestQuarantineRestoreStillRefusesAnOccupiedDestination(t *testing.T) {
	base, itemID := heldObjectFixture(t)

	if err := os.MkdirAll(filepath.Join(base, "tree", "recovered"), 0o755); err != nil {
		t.Fatalf("mkdir destination: %v", err)
	}
	occupied := filepath.Join(base, "tree", "recovered", "old.zip")
	if err := os.WriteFile(occupied, []byte("already here"), 0o644); err != nil {
		t.Fatalf("write the occupying file: %v", err)
	}

	r := cli(t, "quarantine", "restore", "--object", itemID,
		"--destination-relative", "recovered/old.zip", "--json")
	if r.code == 0 {
		t.Fatalf("a restore onto an existing file exited 0: %s", r.stdout)
	}
	if code := r.str(t, "code"); code != "RESTORE_CONFLICT" {
		t.Fatalf("code = %q, want RESTORE_CONFLICT; stdout:\n%s", code, r.stdout)
	}
	body, err := os.ReadFile(occupied)
	if err != nil {
		t.Fatalf("read the occupying file: %v", err)
	}
	if string(body) != "already here" {
		t.Fatalf("the occupying file was overwritten: %q", body)
	}
	listed := mustCLI(t, "quarantine", "list", "--json")
	if n, _ := dataOf(t, listed)["count"].(float64); n != 1 {
		t.Fatalf("the object left the vault after a refused restore: %v", dataOf(t, listed))
	}
}

// And a state the executor does not accept is still refused here, with a message
// that says which state the object is in.
func TestQuarantineRestoreRefusesAnObjectThatIsMidOperation(t *testing.T) {
	base, itemID := heldObjectFixture(t)

	st, err := store.OpenExisting(filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	if err := st.MarkItemState(t.Context(), domain.ItemID(itemID), domain.StateRestored, ""); err != nil {
		st.Close()
		t.Fatalf("move the item out of the vault states: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r := cli(t, "quarantine", "restore", "--object", itemID, "--json")
	if r.code == 0 {
		t.Fatalf("restoring a released object exited 0: %s", r.stdout)
	}
	if msg := r.str(t, "message"); !strings.Contains(msg, "not quarantined or held") {
		t.Fatalf("the message does not name the accepted states: %q", msg)
	}
}
