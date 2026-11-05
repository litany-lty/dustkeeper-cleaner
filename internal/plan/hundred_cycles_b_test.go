package plan

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/store"
)

func TestB19StoredItemBindingDecodeErrorsAreNotSwallowed(t *testing.T) {
	base := validPlanItem(t)
	tests := []struct {
		name string
		edit func(*store.Item)
	}{
		{"binding", func(it *store.Item) { it.BindingJSON = "{" }},
		{"keeper", func(it *store.Item) { bad := "{"; it.KeeperJSON = &bad }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			it := base
			tc.edit(&it)
			if _, err := itemDocOf(it); err == nil || errs.CodeOf(err) != errs.CodeStateWriteFailed {
				t.Fatalf("malformed %s was ignored: %v", tc.name, err)
			}
		})
	}
}

func TestB20ZeroBytePlanLimitForbidsPlanning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := Planner{Store: st, Clock: domain.NewFakeClock(time.Now())}
	_, err = p.CreateRulePlan(context.Background(), RulePlanRequest{
		Scope: testScope(), Rule: &rules.Rule{}, Kind: domain.PlanManualRule,
		Guard: policy.NewGuard(nil), MaxFiles: 1, MaxBytes: 0, DryRun: true,
		Config: config.Default(),
	})
	if err == nil || errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("zero byte ceiling did not forbid planning: %v", err)
	}
}

func TestB21PurgePlanRejectsItemFromAnotherScopeBeforeWriting(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	it := validPlanItem(t)
	it.ScopeID = "another-scope"
	_, err = CreatePurgePlan(context.Background(), st, testScope(), []store.Item{it},
		domain.PlanManualPurge, time.Now().UnixNano(), int64(DefaultTTL))
	if err == nil || errs.CodeOf(err) != errs.CodeScopeChanged {
		t.Fatalf("cross-scope purge member was not rejected at the boundary: %v", err)
	}
	plans, listErr := st.ListPlans(context.Background(), "", 10)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(plans) != 0 {
		t.Fatalf("cross-scope refusal left a plan behind: %+v", plans)
	}
}

func validPlanItem(t *testing.T) store.Item {
	t.Helper()
	identity, _ := json.Marshal(domain.FileIdentity{Device: 1, Inode: 2})
	token, _ := json.Marshal(domain.ChangeToken{Size: 7})
	binding, _ := json.Marshal(ItemBindingV1{BindingVersion: BindingVersion, ScopeID: "exports", SourceKind: domain.PlanScheduledRule})
	return store.Item{
		ID: "item_b", ScopeID: "exports", RawRelative: []byte("old.zip"), ObjectName: "obj_b",
		Reason: domain.ReasonLifecycle, SourceKind: domain.PlanScheduledRule, BindingJSON: string(binding),
		SourceParentIdentityJSON: string(identity), PreIdentityJSON: string(identity), PreTokenJSON: string(token),
		State: domain.StateQuarantined,
	}
}
