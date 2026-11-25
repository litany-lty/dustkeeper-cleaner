package integration

import (
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/policy"
)

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
