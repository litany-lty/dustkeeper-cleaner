package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

func TestB22GrantReadFailureStopsAutomaticPurge(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	clock := domain.NewFakeClock(now)
	sc := store.Scope{ID: "scope_b22", RawRoot: []byte(t.TempDir()), RootIdentityJSON: `{}`, InstanceID: "inst_b22", Kind: domain.KindManagedDisposable, Generation: 1, Status: domain.ScopeActive}
	if err := st.InsertScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	rule := store.Rule{ID: "rule_b22", Revision: 1, ScopeID: sc.ID, DSLJSON: `{}`, Digest: "sha256:rule", ReviewBindingJSON: `{}`, Status: domain.RuleActive, CreatedAtNS: now.UnixNano()}
	if err := st.InsertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	binding, _ := json.Marshal(plan.GrantBindingV1{Capability: domain.CapAutoPurge, RuleID: string(rule.ID), RuleRevision: 1, ApprovedMaxFiles: 2, ApprovedMaxLogicalBytes: 1 << 20})
	grantID := domain.GrantID("grant_b22")
	grant := store.Grant{ID: grantID, ScopeID: sc.ID, RuleID: rule.ID, RuleRevision: 1, Capability: domain.CapAutoPurge, BindingJSON: string(binding), Digest: "sha256:grant", Status: domain.GrantActive, ApprovedAtNS: now.UnixNano()}
	if _, err := st.ReplaceGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	due := now.Add(-time.Hour).UnixNano()
	it := store.Item{ID: "item_b22", ScopeID: sc.ID, RawRelative: []byte("old.bin"), ObjectName: "obj_b22", Reason: domain.ReasonLifecycle, SourceKind: domain.PlanScheduledRule, BindingJSON: `{}`, SourceParentIdentityJSON: `{}`, PreIdentityJSON: `{}`, PreTokenJSON: `{}`, BoundRetentionSeconds: 604800, AutoPurgeGrantID: &grantID, State: domain.StateQuarantined, PurgeNotBeforeNS: &due}
	seedPlan := store.Plan{ID: "plan_b22", Kind: domain.PlanScheduledRule, HeaderJSON: `{}`, Digest: "sha256:plan", Status: domain.PlanComplete, CreatedAtNS: now.UnixNano(), ExpiresAtNS: now.Add(time.Hour).UnixNano()}
	if err := st.InsertPlan(ctx, seedPlan, []store.Item{it}); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE grants`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	cfg := config.Default()
	cfg.Runtime.MaxAutoPurgeFiles = 2
	cfg.Runtime.MaxAutoPurgeBytes = 1 << 20
	runner := Runner{Store: st, Clock: clock, Config: cfg}
	got := runner.purgeDue(ctx, &execute.ScopeCtx{Scope: sc},
		domain.NewRunLedger(60, 1<<20, 0), domain.NewActionLedger(2, 1<<20), nil, nil)
	if got.Err == nil || errs.CodeOf(got.Err) != errs.CodeDBError ||
		got.Control != domain.ControlStopRun || got.Refused != 0 {
		t.Fatalf("grant read failure was treated as ordinary refusal: %+v", got)
	}
}
