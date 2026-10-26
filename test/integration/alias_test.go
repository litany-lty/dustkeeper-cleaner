package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// A22: a protection that cannot be compared by identity must freeze the purge
// instead of being ignored. The object is gone from its directory and the
// directory itself is gone too, so no kernel resolution can prove the
// protection is about some other slot.
func TestPurgeFreezesWhenAliasCannotBeResolved(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("frozen", domain.KindManagedDisposable)
	e.writeFile("frozen", "sub/a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "frozen-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// A user protection exists for a neighbouring slot in the same directory.
	if _, err := e.st.InsertProtection(context.Background(), store.Protection{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.ID,
		RawRelative:  []byte("sub/c.zip"),
		MatchKind:    domain.MatchExact,
		Source:       domain.ProtectUser,
		EvidenceJSON: `{"source":"user"}`,
		Active:       true,
		CreatedAtNS:  e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("insert protection: %v", err)
	}

	// The directory that held the object disappears, so the original slot can no
	// longer be reached through the kernel.
	if err := os.Remove(filepath.Join(e.base, "trees", "frozen", "sub")); err != nil {
		t.Fatalf("remove the original directory: %v", err)
	}

	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected one due object, got %d", len(due))
	}

	sctx := e.scopeCtx(sc.ID)
	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, due, domain.PlanManualPurge,
		e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatalf("purge plan: %v", err)
	}
	out, err := e.exec.Purge(context.Background(), sctx, due[0], execute.PurgeOptions{
		Plan:         &p,
		Budget:       domain.NewBudget(300, 1<<30, 10, 1<<30, 0),
		ClockTrusted: true,
	})
	if err == nil {
		t.Fatalf("purge ran while a protection could not be resolved: %+v", out)
	}
	if code := errs.CodeOf(err); code != errs.CodeProtectionAliasUnresolved {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeProtectionAliasUnresolved, err)
	}
	object := filepath.Join(e.base, "trees", "frozen", ".dustkeeper", "objects", due[0].ObjectName)
	if _, err := os.Stat(object); err != nil {
		t.Fatalf("the quarantined object was removed despite the freeze: %v", err)
	}
}
