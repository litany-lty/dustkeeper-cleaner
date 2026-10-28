package store

// Tests for the two boundaries a state file has to cross before it is a database:
// the pre-flight that decides whether this build may write to it, and the first
// installation that has nothing to decide yet.

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
