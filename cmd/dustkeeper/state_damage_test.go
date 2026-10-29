package main

// C04: what a read-only command does when the state database is damaged.
//
// The rule is the same for every command that is not `init`: report the damage,
// change nothing. A truncated state file is the case that matters, because the
// vault and the tree still hold every object the file used to name, so "rebuilt as
// an empty instance" reads as "there is nothing to restore".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/store"
)

func TestStatusRefusesATruncatedStateDatabase(t *testing.T) {
	base := newHome(t)
	mustCLI(t, "init", "--json")

	dbPath := filepath.Join(base, "state", "state.db")
	var before unix.Stat_t
	if err := unix.Lstat(dbPath, &before); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if err := os.Truncate(dbPath, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	r := cli(t, "status", "--json")
	if r.code == 0 {
		t.Fatalf("status on a truncated state database exited 0: %s", r.stdout)
	}
	if got := r.str(t, "code"); got != "RECOVERY_REQUIRED" {
		t.Errorf("code = %q, want RECOVERY_REQUIRED; stdout:\n%s", got, r.stdout)
	}
	if msg := r.str(t, "message"); !strings.Contains(msg, "zero bytes") {
		t.Errorf("the message does not say what is wrong: %q", msg)
	}

	var after unix.Stat_t
	if err := unix.Lstat(dbPath, &after); err != nil {
		t.Fatalf("the state database was removed by a read-only command: %v", err)
	}
	if after.Size != 0 || after.Ino != before.Ino {
		t.Fatalf("a read-only command replaced the damaged database (size %d, inode %d -> %d)",
			after.Size, before.Ino, after.Ino)
	}
	if _, err := os.Stat(dbPath + ".init"); !os.IsNotExist(err) {
		t.Fatal("a read-only command started building a replacement database")
	}
	// Second run: the refusal has to be stable, not a one-off that leaves a
	// half-repaired directory behind.
	if again := cli(t, "status", "--json"); again.code == 0 {
		t.Fatal("the second status on the same file exited 0")
	}
}

// The other half of the same rule: a database whose instance identity is gone is
// not an instance a command may act on. Acting on it would attach this process to
// a vault whose owner it cannot name.
func TestStatusRefusesADatabaseWithoutAnInstanceIdentity(t *testing.T) {
	base := newHome(t)
	mustCLI(t, "init", "--json")

	dbPath := filepath.Join(base, "state", "state.db")
	st, err := store.OpenExisting(dbPath)
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	if err := st.MetaSet(t.Context(), "instance_id", ""); err != nil {
		t.Fatalf("clear the instance id: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r := cli(t, "status", "--json")
	if r.code == 0 {
		t.Fatalf("status on a database with no instance identity exited 0: %s", r.stdout)
	}
	if got := r.str(t, "code"); got != "RECOVERY_REQUIRED" {
		t.Errorf("code = %q, want RECOVERY_REQUIRED; stdout:\n%s", got, r.stdout)
	}
	if msg := r.str(t, "message"); !strings.Contains(msg, "instance identity") {
		t.Errorf("the message does not say what is missing: %q", msg)
	}
}
