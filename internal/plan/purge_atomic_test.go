package plan

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

// B29: a purge plan is one frozen document. If any member cannot be stored,
// its header must not remain visible as an empty ready plan.
func TestB29PurgePlanHeaderAndMembersCommitTogether(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sc := testScope()
	sc.InstanceID = "instance-test"
	sc.CreatedAtNS = 1
	if err := st.InsertScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	it := validPlanItem(t)
	it.BoundRetentionSeconds = 30 * 24 * 60 * 60
	if err := st.InsertPlan(ctx, store.Plan{
		ID: "source-plan", Kind: domain.PlanScheduledRule, HeaderJSON: `{}`,
		Digest: "sha256:source", Status: domain.PlanReady, CreatedAtNS: 1, ExpiresAtNS: 1 << 60,
	}, []store.Item{it}); err != nil {
		t.Fatal(err)
	}

	// The second copy hits purge_members' UNIQUE(plan_id,item_id) constraint
	// after the first member was accepted, forcing the second half to fail.
	_, err = CreatePurgePlan(ctx, st, sc, []store.Item{it, it},
		domain.PlanManualPurge, time.Now().UnixNano(), int64(DefaultTTL))
	if err == nil {
		t.Fatal("duplicate purge member unexpectedly succeeded")
	}
	plans, listErr := st.ListPlans(ctx, "", 10)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, p := range plans {
		if p.Kind == domain.PlanManualPurge || p.Kind == domain.PlanScheduledPurge {
			t.Fatalf("failed purge member insert left ready plan %s behind", p.ID)
		}
	}
}
