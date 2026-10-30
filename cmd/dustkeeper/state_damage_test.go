package main

// C04: what a read-only command does when the state database is damaged.
//
// The rule is the same for every command that is not `init`: report the damage,
// change nothing. A truncated state file is the case that matters, because the
// vault and the tree still hold every object the file used to name, so "rebuilt as
// an empty instance" reads as "there is nothing to restore".

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"

	"dustkeeper/internal/store"
	"dustkeeper/migrations"
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

// C03 through the command an operator actually runs.
//
// `init` is the one entry point that is allowed to create a state database, and
// the staging name next to it is fixed. A database somebody else put there - a
// backup, an export, another product's file - satisfied every check there was:
// right owner, 0600, one link, a valid SQLite header, a version number in range.
// It was then migrated and published as this installation, with its own rows still
// inside. The ownership question has to be answered before the first pragma, and
// the answer for a file this build cannot account for is "no".
func TestInitRefusesAForeignDatabaseAtTheStagingName(t *testing.T) {
	base := newHome(t)
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	staging := filepath.Join(stateDir, "state.db.init")

	db, err := sql.Open("sqlite3", "file:"+staging+"?_busy_timeout=1000")
	if err != nil {
		t.Fatalf("create the foreign database: %v", err)
	}
	for _, statement := range []string{
		"CREATE TABLE customer_records (id INTEGER PRIMARY KEY, name TEXT)",
		"INSERT INTO customer_records(name) VALUES('a row that is not ours')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	beforeBytes, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	var before unix.Stat_t
	if err := unix.Lstat(staging, &before); err != nil {
		t.Fatalf("lstat: %v", err)
	}

	r := cli(t, "init", "--json")
	if r.code == 0 {
		t.Fatalf("init adopted a foreign database at the staging name: %s", r.stdout)
	}
	if got := r.str(t, "code"); got != "RECOVERY_REQUIRED" {
		t.Errorf("code = %q, want RECOVERY_REQUIRED; stdout:\n%s", got, r.stdout)
	}
	if msg := r.str(t, "message"); !strings.Contains(msg, "customer_records") {
		t.Errorf("the refusal does not say what the file is: %q", msg)
	}

	afterBytes, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("the file was removed by the refusal: %v", err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatal("the refusal modified the file it refused")
	}
	var after unix.Stat_t
	if err := unix.Lstat(staging, &after); err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	if after.Ino != before.Ino {
		t.Fatal("the refused file was replaced: the inode changed")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.db")); !os.IsNotExist(err) {
		t.Fatal("a refused init published a state database")
	}

	// And it keeps saying so: a refusal that only works once is a file that gets
	// adopted on the second attempt.
	if again := cli(t, "init", "--json"); again.code == 0 {
		t.Fatalf("the second init adopted the same file: %s", again.stdout)
	}
}

// C03 through the same command, for the file that agrees about everything a name
// can show.
//
// This staging file is built from this build's own migrations: the right version,
// the right objects, the right meta rows, no data. Only one column of `grants` has
// been renamed, so `migrate` sees the version it expects, does nothing, and the
// first grant query dies with "no such column: approved_at_ns". The definition is
// the only evidence that separates it from an interrupted installation.
func TestInitRefusesAStagingDatabaseWhoseDefinitionDiffers(t *testing.T) {
	base := newHome(t)
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	staging := filepath.Join(stateDir, "state.db.init")

	db, err := sql.Open("sqlite3", "file:"+staging+"?_busy_timeout=1000&_txlock=immediate")
	if err != nil {
		t.Fatalf("create the staging database: %v", err)
	}
	for _, step := range migrations.Steps {
		if _, err := db.Exec(step.SQL); err != nil {
			t.Fatalf("apply %03d: %v", step.Version, err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.Version)); err != nil {
			t.Fatalf("set user_version %d: %v", step.Version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO meta(key,value) VALUES('schema_version',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			fmt.Sprintf("%d", step.Version)); err != nil {
			t.Fatalf("record schema_version %d: %v", step.Version, err)
		}
	}
	if _, err := db.Exec("ALTER TABLE grants RENAME COLUMN approved_at_ns TO approved_at_legacy"); err != nil {
		t.Fatalf("rename the column: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	beforeBytes, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	var before unix.Stat_t
	if err := unix.Lstat(staging, &before); err != nil {
		t.Fatalf("lstat: %v", err)
	}

	r := cli(t, "init", "--json")
	if r.code == 0 {
		t.Fatalf("init adopted a staging database whose definition differs: %s", r.stdout)
	}
	if got := r.str(t, "code"); got != "RECOVERY_REQUIRED" {
		t.Errorf("code = %q, want RECOVERY_REQUIRED; stdout:\n%s", got, r.stdout)
	}
	msg := r.str(t, "message")
	for _, fragment := range []string{"table grants", "approved_at_ns", "approved_at_legacy"} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("the refusal does not mention %q: %q", fragment, msg)
		}
	}

	afterBytes, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("the file was removed by the refusal: %v", err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatal("the refusal modified the file it refused")
	}
	var after unix.Stat_t
	if err := unix.Lstat(staging, &after); err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	if after.Ino != before.Ino {
		t.Fatal("the refused file was replaced: the inode changed")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.db")); !os.IsNotExist(err) {
		t.Fatal("a refused init published a state database")
	}
}
