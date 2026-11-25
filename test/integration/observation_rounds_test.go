package integration

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/policy"
)

func TestO03ScannerSkipsImmutableEntries(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS BSD UF_IMMUTABLE flag test runs on darwin")
	}
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

func TestO05ZeroActionFunnelReportsOverByteLimitAndSkipBreakdown(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("o05box", domain.KindManagedDisposable)
	e.writeFile("o05box", "huge.zip", strings.Repeat("Z", 4096), 60*24*time.Hour)
	e.writeFile("o05box", "recent.zip", "new", 1*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(7, sc.ID)

	r := e.rule(sc.ID, "r-o05", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1024)

	rep := e.runOnce()
	if rep.Quarantined != 0 || rep.Purged != 0 || len(rep.Scopes) != 1 {
		t.Fatalf("unexpected run report: %+v", rep)
	}
	sr := rep.Scopes[0]
	if sr.CandidateCounts[policy.ReasonOverRunByteLimit] != 1 {
		t.Fatalf("expected OVER_RUN_BYTE_LIMIT=1 in CandidateCounts, got %+v", sr.CandidateCounts)
	}
	if sr.CandidateCounts[policy.ReasonTooNew] != 1 {
		t.Fatalf("expected MODIFIED_TOO_RECENTLY=1 in CandidateCounts, got %+v", sr.CandidateCounts)
	}
	if !strings.Contains(sr.QuarantineSkipped, "exceed per-run byte limit") || !strings.Contains(sr.QuarantineSkipped, "modified too recently") {
		t.Fatalf("expected structured skip breakdown in QuarantineSkipped, got %q", sr.QuarantineSkipped)
	}
}
