package store

// Tests for the two boundaries a state file has to cross before it is a database:
// the pre-flight that decides whether this build may write to it, and the first
// installation that has nothing to decide yet.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/errs"
	"dustkeeper/migrations"
)

// TestAFIFOIsRefusedWithoutBlocking covers D10. A FIFO opens only when a writer
// appears, and that wait happens inside open(2) - so a pre-flight that checks the
// file type *after* opening never gets to the check. The test therefore has a
// deadline: without O_NONBLOCK this call does not return at all, and every
// command that pre-flights the state file hangs with it.
func TestAFIFOIsRefusedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- CheckSupportedState(path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was accepted as a state database")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pre-flight blocked on a FIFO instead of refusing it: it waits for a writer before it can look at the file type")
	}

	// The refusal must not have replaced the entry with a regular file.
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFIFO {
		t.Fatalf("%s is no longer a FIFO (mode %o)", path, st.Mode)
	}

	// The full open path refuses it too, and for the same reason.
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a FIFO as a state database")
	}
}

// TestAnInterruptedFirstInstallationResumes covers D11. The migrations commit one
// at a time, so a first installation that dies after 001 or 002 leaves a file the
// published-name rule would read as "an old schema, keep the previous binary".
// There is no previous binary: this is the installation.
func TestAnInterruptedFirstInstallationResumes(t *testing.T) {
	for _, reached := range []int{0, 1, 2} {
		reached := reached
		t.Run(fmt.Sprintf("stopped_after_%03d", reached), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			staging := path + stagingSuffix
			if reached > 0 {
				buildStaging(t, staging, reached)
			}

			s, err := Open(path)
			if err != nil {
				t.Fatalf("a first installation interrupted after %03d could not be resumed: %v", reached, err)
			}
			defer s.Close()
			if got := schemaVersion(t, s); got != migrations.CurrentVersion {
				t.Fatalf("resumed at schema version %d, want %d", got, migrations.CurrentVersion)
			}
			if _, err := os.Stat(staging); !os.IsNotExist(err) {
				t.Fatalf("the staging name %s survived publication: %v", staging, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("no database was published: %v", err)
			}
		})
	}
}

// TestAnEmptyStateFileIsInitialisedNotRefused covers the other half of the same
// case: an attempt that stopped inside SQLite's own initialisation can leave a
// zero-length state.db, whose version 0 reads as "older than this build". It
// carries nothing, so the honest handling is to initialise it.
func TestAnEmptyStateFileIsInitialisedNotRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create the empty file: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("an empty state file was refused: %v", err)
	}
	defer s.Close()
	if got := schemaVersion(t, s); got != migrations.CurrentVersion {
		t.Fatalf("schema version = %d, want %d", got, migrations.CurrentVersion)
	}
}

// TestARealOlderStateFileIsStillRefused is the guard on the fix: resuming a
// first installation must not become migrating a database somebody was already
// using. Both older versions are checked, byte for byte.
//
// Version 1 is also the file the *previous* build left behind when its first
// installation died after the baseline: it wrote state.db in place. This build
// refuses that file exactly as it did before - the fix is that it no longer
// produces one, not that it learned to read it.
func TestARealOlderStateFileIsStillRefused(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(fmt.Sprintf("version_%03d", version), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			buildStaging(t, path, version)

			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			s, oerr := Open(path)
			if oerr == nil {
				s.Close()
				t.Fatal("an older published database was migrated in place")
			}
			if code := errs.CodeOf(oerr); code != errs.CodeMigrationTooOld {
				t.Fatalf("code = %s, want %s (%v)", code, errs.CodeMigrationTooOld, oerr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the refused database was modified")
			}
			if _, err := os.Stat(path + stagingSuffix); !os.IsNotExist(err) {
				t.Fatal("a refusal created a staging file next to a database it did not touch")
			}
		})
	}
}

// buildStaging creates a database that has committed the steps up to `reached`,
// which is the state an interrupted first installation leaves behind.
func buildStaging(t *testing.T, path string, reached int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=1000&_txlock=immediate")
	if err != nil {
		t.Fatalf("open staging: %v", err)
	}
	defer db.Close()
	for _, step := range migrations.Steps {
		if step.Version > reached {
			break
		}
		if _, err := db.Exec(step.SQL); err != nil {
			t.Fatalf("apply %03d: %v", step.Version, err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.Version)); err != nil {
			t.Fatalf("set user_version %d: %v", step.Version, err)
		}
		// migrate writes this row in the same transaction as the version bump, so
		// a real residue carries it. A fixture that skipped it would describe a
		// state the product never produces - and the ownership check would then
		// pass a file it should have refused, which is worse than the reverse.
		if _, err := db.Exec(
			"INSERT INTO meta(key,value) VALUES('schema_version',?) "+
				"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			fmt.Sprintf("%d", step.Version)); err != nil {
			t.Fatalf("record schema_version %d: %v", step.Version, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close staging: %v", err)
	}
}

// schemaVersion reads the version through SQLite, which is the only trustworthy
// answer once the file has been opened.
func schemaVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

// C04: OpenExisting is what every read-only command uses. A state file that was
// truncated to zero bytes is damage to an instance that still has a vault and a
// tree; rebuilding it here would answer "no scopes, no objects" for files that are
// sitting on disk.
func TestOpenExistingRefusesATruncatedStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("init the state file: %v", err)
	}
	if err := s.MetaSet(context.Background(), "instance_id", "inst_truncated"); err != nil {
		t.Fatalf("set instance id: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var before unix.Stat_t
	if err := unix.Lstat(path, &before); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	got, err := OpenExisting(path)
	if err == nil {
		got.Close()
		t.Fatal("a zero-byte state file was opened as an instance")
	}
	if code := errs.CodeOf(err); code != errs.CodeRecoveryRequired {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeRecoveryRequired, err)
	}

	var after unix.Stat_t
	if err := unix.Lstat(path, &after); err != nil {
		// A refusal that deletes the file is the failure this test exists for.
		t.Fatalf("the state file was removed by a refusal: %v", err)
	}
	if after.Size != 0 {
		t.Fatalf("the refused file was replaced by %d bytes", after.Size)
	}
	if after.Ino != before.Ino {
		t.Fatal("the refused file was replaced: the inode changed")
	}
	// And the staging name was not used to build a new one either.
	if _, err := os.Stat(path + stagingSuffix); !os.IsNotExist(err) {
		t.Fatal("a refusal built a staging database")
	}
}

// The positive half: a complete instance opens normally through OpenExisting.
func TestOpenExistingOpensAnExistingInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := s.MetaSet(context.Background(), "instance_id", "inst_open"); err != nil {
		t.Fatalf("set instance id: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := OpenExisting(path)
	if err != nil {
		t.Fatalf("an existing instance was refused: %v", err)
	}
	defer got.Close()
	if v := schemaVersion(t, got); v != migrations.CurrentVersion {
		t.Fatalf("schema version = %d, want %d", v, migrations.CurrentVersion)
	}
	id, err := got.MetaGet(context.Background(), "instance_id")
	if err != nil || id != "inst_open" {
		t.Fatalf("instance id = %q (%v), want the stored one", id, err)
	}
}

// And OpenExisting does not create anything when there is nothing to open.
func TestOpenExistingRefusesAMissingStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("a missing state file was opened as an instance")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a read-only open created a database: %v", err)
	}
	if _, err := os.Stat(path + stagingSuffix); !os.IsNotExist(err) {
		t.Fatal("a read-only open left a staging database behind")
	}
}

// C03: the staging name is fixed, so it can be occupied by something this build
// never created. SQLite is opened on it - and the first thing the driver is asked
// to do is set the pragmas, journal_mode included. That is a write, and it used to
// happen before anybody checked what the file was.
//
// The target here is a WAL database from another build. Its journal mode byte is
// the evidence: 2 means WAL, 1 means rollback journal, and rewriting it is the
// first thing the old order did.
func TestAStagedSymlinkIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix

	target := filepath.Join(dir, "somebody-elses.db")
	buildStaging(t, target, 0)
	if err := setUserVersionAndWAL(t, target, 99); err != nil {
		t.Fatalf("prepare the foreign database: %v", err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if before[18] != 2 {
		t.Fatalf("the fixture is not a WAL database (write version byte %d)", before[18])
	}
	if err := os.Symlink(target, staging); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, oerr := Open(path)
	if oerr == nil {
		got.Close()
		t.Fatal("a staged symlink was accepted as a first installation")
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the symlink target was modified: its journal mode byte went from %d to %d",
			before[18], after[18])
	}
	if after[18] != 2 {
		t.Fatal("the foreign database is no longer a WAL database")
	}
	// Still a symlink, still pointing at the same file.
	var st unix.Stat_t
	if err := unix.Lstat(staging, &st); err != nil {
		t.Fatalf("lstat staging: %v", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatal("the refusal replaced the staging symlink")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused installation published a state database")
	}
}

// A hard link into the same file is the same write through another name.
func TestAStagedHardLinkIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix

	target := filepath.Join(dir, "shared.db")
	buildStaging(t, target, migrations.CurrentVersion)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if err := os.Link(target, staging); err != nil {
		t.Fatalf("link: %v", err)
	}

	if got, oerr := Open(path); oerr == nil {
		got.Close()
		t.Fatal("a staged hard link was accepted as a first installation")
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the hard-link target was modified")
	}
}

// A file that is not a database at all is not an interrupted installation.
func TestAStagedFileThatIsNotADatabaseIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix
	payload := bytes.Repeat([]byte("not a database "), 16)
	if err := os.WriteFile(staging, payload, 0o600); err != nil {
		t.Fatalf("write staging: %v", err)
	}

	if got, err := Open(path); err == nil {
		got.Close()
		t.Fatal("a non-database staging file was accepted as a first installation")
	}
	after, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(after, payload) {
		t.Fatal("the refusal rewrote the file it refused")
	}
}

// C03, the half that was still open: the file type, the owner, the mode, the link
// count and a SQLite header are all true of somebody else's database. A backup or
// an export parked at the fixed staging name used to be migrated and published as
// this installation - its own rows intact, this build's schema added, `init`
// reporting a fresh start.
//
// The fixture is exactly that case: user_version 0, one table that has nothing to
// do with this product, and a row in it.
func TestAForeignDatabaseAtTheStagingNameIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix

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
	var before unix.Stat_t
	if err := unix.Lstat(staging, &before); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	bytesBefore, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	got, oerr := Open(path)
	if oerr == nil {
		got.Close()
		t.Fatal("a foreign database at the staging name was adopted as this installation")
	}
	if code := errs.CodeOf(oerr); code != errs.CodeRecoveryRequired {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeRecoveryRequired, oerr)
	}
	if !strings.Contains(oerr.Error(), "customer_records") {
		t.Fatalf("the refusal does not say what the file actually is: %v", oerr)
	}
	// The bytes are the point: an unknown database is not ours to migrate, and a
	// refusal that rewrote it while explaining that it would not would be worse
	// than no check at all.
	bytesAfter, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("the refused file is gone: %v", err)
	}
	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatal("the refusal modified the file it refused")
	}
	var after unix.Stat_t
	if err := unix.Lstat(staging, &after); err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	if after.Ino != before.Ino {
		t.Fatal("the refused file was replaced: the inode changed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused installation published a state database")
	}
}

// The positive half of the same boundary: an installation this build finished but
// never published is resumed, not refused. This is the state a process leaves
// behind when it dies between the last migration and the link.
func TestAFinishedButUnpublishedStagingIsResumed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix
	buildStaging(t, staging, migrations.CurrentVersion)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("a finished but unpublished installation was refused: %v", err)
	}
	defer s.Close()
	if got := schemaVersion(t, s); got != migrations.CurrentVersion {
		t.Fatalf("schema version = %d, want %d", got, migrations.CurrentVersion)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("the staging name survived publication: %v", err)
	}
}

// And the reverse of the same coin: a database with this build's schema that has
// already taken data is not an installation that was never published. That is a
// state database somebody put under the wrong name, and publishing it would be a
// migration nobody asked for.
func TestAUsedDatabaseAtTheStagingNameIsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix
	buildStaging(t, staging, migrations.CurrentVersion)

	db, err := sql.Open("sqlite3", "file:"+staging+"?_busy_timeout=1000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO events(event_id, action, phase, occurred_at_ns, contract_json)
VALUES('ev_somebody_elses', 'quarantine', 'info', 1, '{}')`); err != nil {
		t.Fatalf("insert a historical event: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, oerr := Open(path)
	if oerr == nil {
		got.Close()
		t.Fatal("a used database at the staging name was published as a new installation")
	}
	if code := errs.CodeOf(oerr); code != errs.CodeRecoveryRequired {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeRecoveryRequired, oerr)
	}
	if !strings.Contains(oerr.Error(), "belongs at the published name") {
		t.Fatalf("the refusal does not say where the file belongs: %v", oerr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused installation published a state database")
	}
}

// C03, the half that names alone could not see: the same objects, defined
// differently.
//
// A staging file can carry this build's version, this build's object names, this
// build's meta rows and no data at all, and still not be this build's state
// database. ALTER TABLE ... RENAME COLUMN is the narrowest way to get there: the
// set of objects is unchanged, so the earlier check matched on everything it
// looked at; migrate() sees the version it expects and returns without restoring
// the column; and the first grant query fails with "no such column:
// approved_at_ns". The definition is the only part that carries the difference.
func TestAStagingDatabaseWithADifferentDefinitionIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	staging := path + stagingSuffix
	buildStaging(t, staging, migrations.CurrentVersion)

	db, err := sql.Open("sqlite3", "file:"+staging+"?_busy_timeout=1000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE grants RENAME COLUMN approved_at_ns TO approved_at_legacy"); err != nil {
		t.Fatalf("rename the column: %v", err)
	}
	// The premise of the case, checked rather than assumed: the version and the
	// object names are untouched, which is exactly why the file used to pass.
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != migrations.CurrentVersion {
		t.Fatalf("the fixture changed the version to %d, so it is not the case under test", version)
	}
	names, err := schemaNames(db)
	if err != nil {
		t.Fatalf("read object names: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want, err := schemaNamesOfBuild(t, migrations.CurrentVersion)
	if err != nil {
		t.Fatalf("read this build's object names: %v", err)
	}
	if !sameStrings(names, want) {
		t.Fatalf("the fixture changed the object names (%v vs %v), so it is not the case under test", names, want)
	}

	var before unix.Stat_t
	if err := unix.Lstat(staging, &before); err != nil {
		t.Fatalf("lstat: %v", err)
	}
	bytesBefore, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	got, oerr := Open(path)
	if oerr == nil {
		got.Close()
		t.Fatal("a staging database with a renamed column was adopted as this installation")
	}
	if code := errs.CodeOf(oerr); code != errs.CodeRecoveryRequired {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeRecoveryRequired, oerr)
	}
	// Both sides of the difference, so the message says which column moved
	// instead of only that something did.
	for _, fragment := range []string{"table grants", "approved_at_ns", "approved_at_legacy"} {
		if !strings.Contains(oerr.Error(), fragment) {
			t.Fatalf("the refusal does not mention %q: %v", fragment, oerr)
		}
	}
	bytesAfter, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("the refused file is gone: %v", err)
	}
	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatal("the refusal modified the file it refused")
	}
	var after unix.Stat_t
	if err := unix.Lstat(staging, &after); err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	if after.Ino != before.Ino {
		t.Fatal("the refused file was replaced: the inode changed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused installation published a state database")
	}
}

// schemaNames lists a file's objects the way the ownership check names them.
func schemaNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
SELECT type, name FROM sqlite_master
WHERE name NOT LIKE 'sqlite_autoindex%'
ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, err
		}
		out = append(out, kind+" "+name)
	}
	return out, rows.Err()
}

// schemaNamesOfBuild is the same list for the schema this build writes, produced
// by replaying the shipped migrations rather than by a second copy of the answer.
func schemaNamesOfBuild(t *testing.T, version int) ([]string, error) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, step := range migrations.Steps {
		if step.Version > version {
			break
		}
		if _, err := db.Exec(step.SQL); err != nil {
			return nil, err
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", step.Version)); err != nil {
			return nil, err
		}
	}
	return schemaNames(db)
}

// setUserVersionAndWAL turns a database into one from another build: a version
// this build does not implement, and a journal mode of its own.
func setUserVersionAndWAL(t *testing.T, path string, version int) error {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=1000")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		return err
	}
	return db.Close()
}
