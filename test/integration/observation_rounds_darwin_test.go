//go:build darwin

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
)

func TestO03ScannerSkipsImmutableEntries(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("o03box", domain.KindManagedDisposable)
	e.writeFile("o03box", "locked.tmp", "locked", 60*24*time.Hour)
	e.writeFile("o03box", "normal.tmp", "normal", 60*24*time.Hour)

	lockedPath := filepath.Join(e.base, "trees", "o03box", "locked.tmp")
	if err := unix.Chflags(lockedPath, unix.UF_IMMUTABLE); err != nil {
		t.Skipf("unable to set UF_IMMUTABLE: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Chflags(lockedPath, 0)
	})

	e.scan(sc.ID)
	obs, err := e.st.CountObservations(context.Background(), sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if obs != 1 {
		t.Fatalf("expected only normal.tmp observed (1), got %d (locked.tmp should be skipped)", obs)
	}
	if _, err := os.Stat(lockedPath); err != nil {
		t.Fatalf("locked.tmp missing: %v", err)
	}
}
