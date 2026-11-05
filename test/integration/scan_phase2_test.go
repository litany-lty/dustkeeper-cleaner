package integration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/scan"
)

func TestProjectMarkerInspectionFailureKeepsScanIncomplete(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(e.base, "trees", "project")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := e.addScope("project", "observe")
	sctx := e.scopeCtx(sc.ID)
	scan.TestHookEntryStat = func(rel string) error {
		if rel == ".git" {
			return unix.EIO
		}
		return nil
	}
	t.Cleanup(func() { scan.TestHookEntryStat = nil })

	stats, state := e.scanRound(t, sctx)
	if state.Complete || !state.Incomplete || state.FailedDirs != 1 {
		t.Fatalf("marker inspection failure was not retained: state=%+v stats=%+v", state, stats)
	}
	if !errors.Is(scan.TestHookEntryStat(".git"), unix.EIO) {
		t.Fatal("test hook did not construct the intended marker failure")
	}
	if count, err := e.st.CountObservations(t.Context(), sc.ID); err != nil || count != 0 {
		t.Fatalf("project contents were observed despite unknown marker state: count=%d err=%v", count, err)
	}
}

func TestReadErrorAlongsideEntriesKeepsScanIncomplete(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(e.base, "trees", "partial-read")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sc := e.addScope("partial-read", "observe")
	sctx := e.scopeCtx(sc.ID)
	called := false
	scan.TestHookReadDirError = func(rel string, entries int) error {
		if !called && rel == "" && entries > 0 {
			called = true
			return unix.EIO
		}
		return nil
	}
	t.Cleanup(func() { scan.TestHookReadDirError = nil })

	stats, state := e.scanRound(t, sctx)
	if !called {
		t.Fatal("read-error hook was not exercised")
	}
	if state.Complete || !state.Incomplete || state.FailedDirs != 1 {
		t.Fatalf("partial read error was reported complete: state=%+v stats=%+v", state, stats)
	}
}
