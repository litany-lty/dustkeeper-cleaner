package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// A01: a matching file moves into the vault, its bytes are intact, and nothing
// else is touched.
func TestQuarantineMovesOnlyWhatTheRuleSelected(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "old-payload", 40*24*time.Hour)
	e.writeFile("exports", "keep/old.zip", "kept-payload", 40*24*time.Hour)
	e.writeFile("exports", "fresh.zip", "fresh-payload", time.Hour)
	e.writeFile("exports", "notes.txt", "notes", 40*24*time.Hour)

	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, []string{"keep/"}, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if len(res.Items) != 1 {
		t.Fatalf("expected 1 planned item, got %d (%v)", len(res.Items), res.Reasons)
	}

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 1 || out.Failed != 0 {
		t.Fatalf("expected one completion, got %+v", out)
	}
	if e.exists("exports", "old.zip") {
		t.Fatal("the matched file is still in the tree")
	}
	if !e.exists("exports", "keep/old.zip") || !e.exists("exports", "fresh.zip") || !e.exists("exports", "notes.txt") {
		t.Fatal("a file that should have been left alone moved")
	}

	it := res.Items[0]
	vault := filepath.Join(e.base, "trees", "exports", ".dustkeeper", "objects", it.ObjectName)
	body, err := os.ReadFile(vault)
	if err != nil {
		t.Fatalf("read vault object: %v", err)
	}
	if string(body) != "old-payload" {
		t.Fatalf("vault content is %q", body)
	}
	if it.BoundRetentionSeconds != int64((30 * 24 * time.Hour).Seconds()) {
		t.Fatalf("retention bound is %d", it.BoundRetentionSeconds)
	}
}

// A02: a restore never overwrites, and a conflict leaves both files alone.
func TestRestoreRefusesToOverwrite(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "original", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// A new file takes the old path. It belongs to someone else now.
	e.writeFile("exports", "a.zip", "impostor", 0)

	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	_, err := e.exec.Restore(context.Background(), sctx, res.Items[0].ID, execute.RestoreOptions{})
	if err == nil {
		t.Fatal("restore overwrote an existing file")
	}
	requireCode(t, err, errs.CodeRestoreConflict)

	body, _ := os.ReadFile(filepath.Join(e.base, "trees", "exports", "a.zip"))
	if string(body) != "impostor" {
		t.Fatalf("the existing file was modified: %q", body)
	}
	it, _ := e.st.GetItem(context.Background(), res.Items[0].ID)
	if it.State != domain.StateQuarantined {
		t.Fatalf("object left quarantine on a conflict: %s", it.State)
	}
}

// A03: restoring protects both the original slot and the actual path, and the
// protection is what stops the next plan from taking it again.
func TestRestoreProtectsAndBlocksTheNextPlan(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "original", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	if _, err := e.exec.Restore(context.Background(), sctx, res.Items[0].ID, execute.RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file was not restored")
	}

	protections, err := e.st.ListProtections(context.Background(), sc.ID, true)
	if err != nil {
		t.Fatalf("protections: %v", err)
	}
	if len(protections) == 0 {
		t.Fatal("no protection was recorded")
	}

	// The restored file is old and still matches the rule, but it must not be
	// selected again. A second scan is required first: the original scan is
	// stale for that path.
	e.scan(sc.ID)
	scAfter, _ := e.st.GetScope(context.Background(), sc.ID)
	parsed, _ := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
	_, err = e.plan.CreateRulePlan(context.Background(), plan.RulePlanRequest{
		Scope:    scAfter,
		Rule:     parsed,
		RuleRef:  domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:     domain.PlanManualRule,
		Guard:    e.guard(sctx),
		MaxFiles: 100,
		MaxBytes: 1 << 30,
		Config:   e.cfg,
		NowNS:    e.clock.Wall().UnixNano(),
	})
	if err == nil {
		t.Fatal("a protected path was planned for quarantine again")
	}
	requireCode(t, err, errs.CodeNoCandidates)
}

// A04: an expired plan is refused before anything is touched.
func TestExpiredPlanIsRefused(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	e.clock.Advance(2 * time.Hour) // longer than the 15 minute approval window

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err == nil {
		t.Fatalf("an expired plan was applied: %+v", out)
	}
	requireCode(t, err, errs.CodePlanExpired)
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file moved even though the plan had expired")
	}
}

// A05: a digest that does not match the stored plan is refused.
func TestWrongDigestIsRefused(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	out, err := e.apply(sc, res.Plan, domain.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000"))
	if err == nil {
		t.Fatalf("apply accepted a wrong digest: %+v", out)
	}
	requireCode(t, err, errs.CodeDigestMismatch)
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file moved despite a digest mismatch")
	}
}

// A06: a grant is never retroactive. An object isolated while only quarantine
// was authorised cannot be purged by turning on auto-purge later.
func TestAutoPurgeIsNotRetroactive(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 100, 1<<30)

	// A scheduled plan is what `run` produces: only the quarantine grant exists.
	sctx := e.scopeCtx(sc.ID)
	parsed, _ := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
	qg, err := e.st.ActiveGrant(context.Background(), sc.ID, r.ID, r.Revision, domain.CapAutoQuarantine)
	if err != nil {
		t.Fatalf("quarantine grant: %v", err)
	}
	res, err := e.plan.CreateRulePlan(context.Background(), plan.RulePlanRequest{
		Scope:           sc,
		Rule:            parsed,
		RuleRef:         domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:            domain.PlanScheduledRule,
		Guard:           e.guard(sctx),
		MaxFiles:        100,
		MaxBytes:        1 << 30,
		QuarantineGrant: &qg,
		Config:          e.cfg,
		NowNS:           e.clock.Wall().UnixNano(),
	})
	if err != nil {
		t.Fatalf("scheduled plan: %v", err)
	}
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Now the user enables auto-purge. The existing object must stay out of
	// reach: it was isolated without that authority.
	e.grant(r, domain.CapAutoPurge, 100, 1<<30)
	e.clock.Advance(31 * 24 * time.Hour)

	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due items: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected one due object, got %d", len(due))
	}
	it := due[0]
	if it.AutoPurgeGrantID != nil {
		t.Fatal("the object somehow carries an auto-purge grant that did not exist when it was isolated")
	}

	out2, err := e.exec.Purge(context.Background(), sctx, it, execute.PurgeOptions{
		Automatic:    true,
		Budget:       domain.NewBudget(300, 1<<30, 10, 1<<30, 0),
		ClockTrusted: true,
	})
	if err == nil {
		t.Fatalf("purge accepted an object with no bound grant: %+v", out2)
	}
	requireCode(t, err, errs.CodeGrantRequired)
}

// A07: a manual purge plan deletes exactly the members it froze, and only after
// the retention bound has elapsed.
func TestManualPurgeRemovesOnlyListedObjects(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	sctx := e.scopeCtx(sc.ID)
	// Too early: the retention window is still open.
	early, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("an object was due before its retention elapsed: %d", len(early))
	}
	if _, err := e.exec.Purge(context.Background(), sctx, res.Items[0], execute.PurgeOptions{
		Budget:       domain.NewBudget(300, 1<<30, 10, 1<<30, 0),
		ClockTrusted: true,
	}); err == nil {
		t.Fatal("purge ran before the retention window closed")
	} else if errs.CodeOf(err) == errs.CodeOK {
		t.Fatal("purge returned a successful code before retention")
	}

	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("expected two due objects, got %d", len(due))
	}

	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, due[:1], domain.PlanManualPurge,
		e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatalf("purge plan: %v", err)
	}
	out, err := e.exec.ApplyPurgePlan(context.Background(), sctx, p.ID, p.Digest,
		domain.NewBudget(300, 1<<30, 10, 1<<30, 0), true)
	if err != nil {
		t.Fatalf("apply purge: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("expected one purge, got %+v", out)
	}

	remaining, _ := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if len(remaining) != 1 {
		t.Fatalf("expected one object to survive, got %d", len(remaining))
	}
}

// A08: a duplicate whose keeper disappeared must not be purged.
func TestPurgeRefusesWhenTheKeeperIsGone(t *testing.T) {
	e := newEnv(t)
	src := e.addScope("dups", domain.KindAssisted)
	e.addScope("originals", domain.KindReference)

	e.writeFile("dups", "one.bin", "same-bytes", 0)
	e.writeFile("dups", "two.bin", "same-bytes", 0)
	e.writeFile("originals", "keep.bin", "same-bytes", 0)

	e.scan(src.ID)
	e.scan(domain.ScopeID("originals"))

	sctx := e.scopeCtx(src.ID)
	keeper := domain.ScopeID("originals")
	res, err := e.det.Detect(context.Background(), sctx, &keeper)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		t.Fatalf("expected one plannable group, got %d (%v)", res.PlannableGroups, res.Notes)
	}
	if res.Groups[0].Keeper.ScopeID != keeper {
		t.Fatalf("the keeper should come from the reference scope, got %s", res.Groups[0].Keeper.ScopeID)
	}

	p, items, err := e.det.CreatePlan(context.Background(), sctx, res.Groups)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected two copies planned, got %d", len(items))
	}
	if _, err := e.apply(src, p, p.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The keeper is removed by someone else.
	if err := os.Remove(filepath.Join(e.base, "trees", "originals", "keep.bin")); err != nil {
		t.Fatalf("remove keeper: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)

	due, err := e.st.ItemsDueForPurge(context.Background(), src.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) == 0 {
		t.Fatal("no object became due")
	}
	if err := e.det.VerifyKeeper(context.Background(), due[0]); err == nil {
		t.Fatal("the keeper verification passed after the keeper was deleted")
	}
}

// A09: a zero action limit forbids the action; it never means "unlimited".
func TestZeroBudgetForbidsTheAction(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)

	sctx := e.scopeCtx(sc.ID)
	parsed, _ := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
	_, err := e.plan.CreateRulePlan(context.Background(), plan.RulePlanRequest{
		Scope:    sc,
		Rule:     parsed,
		RuleRef:  domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:     domain.PlanManualRule,
		Guard:    e.guard(sctx),
		MaxFiles: 0, // "do not move anything"
		MaxBytes: 1 << 30,
		Config:   e.cfg,
		NowNS:    e.clock.Wall().UnixNano(),
	})
	if err == nil {
		t.Fatal("a zero file limit produced a plan")
	}
	requireCode(t, err, errs.CodeInvalidArgument)
}

// A10: a clock that jumped backwards suspends purging for the whole run.
func TestClockGoingBackwardsSuspendsPurge(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 100, 1<<30)
	e.grant(r, domain.CapAutoPurge, 100, 1<<30)

	if _, err := e.run.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The next run happens almost a year later: more than the 48h jump limit.
	e.clock.Advance(300 * 24 * time.Hour)
	rep, err := e.run.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep.ClockTrusted {
		t.Fatal("a 300 day jump was treated as a trusted clock")
	}
	if rep.Purged != 0 {
		t.Fatalf("something was purged with an untrusted clock: %d", rep.Purged)
	}
}

// A11: an operation that committed its intent and moved the file but never
// recorded the outcome is finished by reconciliation, not guessed at.
func TestReconcileCompletesACrashedQuarantine(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	it := res.Items[0]

	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)

	// Recreate what a crash leaves behind: a durable manifest, a committed
	// intent, and the file already renamed into the vault.
	srcPath := filepath.Join(e.base, "trees", "exports", "a.zip")
	preID := decodeIdentity(t, it.PreIdentityJSON)
	parentID := decodeIdentity(t, it.SourceParentIdentityJSON)
	contentHash := hashFile(t, srcPath)

	if err := sctx.WriteManifest(execute.ManifestV1{
		FormatVersion: 1, InstanceID: "inst_test", ScopeID: string(sc.ID),
		ObjectID: it.ObjectName, RawRelativeB64: plan.B64(it.RawRelative),
		ParentIdentity: parentID, ExpectedIdentity: preID,
		ContentHash: string(contentHash), MetadataHash: "sha256:meta",
		PlanDigest: string(res.Plan.Digest), CreatedAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	preTok, err := decodeToken(t, it.PreTokenJSON)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	var binding plan.ItemBindingV1
	if err := json.Unmarshal([]byte(it.BindingJSON), &binding); err != nil {
		t.Fatalf("binding: %v", err)
	}
	planIDStr := string(res.Plan.ID)
	contract, err := json.Marshal(plan.OperationContractV1{
		BindingVersion:  plan.BindingVersion,
		ItemID:          string(it.ID),
		ExecutionPlanID: &planIDStr,
		PriorState:      domain.StatePlanned,
		NextState:       domain.StateQIntent,
		Purpose:         plan.PurposeNormal,
		ScopeID:         string(sc.ID),
		RawRelativeB64:  plan.B64(it.RawRelative),
		ObjectName:      it.ObjectName,
		ParentIdentity:  parentID,
		ExpectedIdent:   preID,
		ExpectedToken:   preTok,
		Binding:         binding,
		MaxFiles:        1,
		MaxLogicalBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("contract: %v", err)
	}

	opID := domain.OperationID(domain.NewID("op"))
	if err := e.st.BeginIntent(context.Background(), store.Intent{
		OpID: opID, ItemID: it.ID, PlanID: &res.Plan.ID,
		Action: "quarantine", PriorState: domain.StatePlanned,
		NextState:    domain.StateQIntent,
		ContractJSON: string(contract),
		OccurredAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("begin intent: %v", err)
	}
	if err := os.Rename(srcPath, filepath.Join(e.base, "trees", "exports", ".dustkeeper", "objects", it.ObjectName)); err != nil {
		t.Fatalf("rename: %v", err)
	}

	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 1 {
		t.Fatalf("expected the operation to be completed, got %+v", rep)
	}
	got, err := e.st.GetItem(context.Background(), it.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.State != domain.StateQuarantined {
		t.Fatalf("item state is %s", got.State)
	}
	if got.CapturedIdentityJSON == nil || got.ContentHash == nil {
		t.Fatal("reconciliation did not record the captured evidence")
	}
	if got.PurgeNotBeforeNS == nil || *got.PurgeNotBeforeNS < e.clock.Wall().UnixNano() {
		t.Fatal("reconciliation must start the retention window at completion, not earlier")
	}
}

// A12: the global protection list stops a move even when the rule matches.
func TestGlobalProtectionWinsOverTheRule(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "backup.zip.bak", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.bak"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	_, err := e.plan.CreateRulePlan(context.Background(), plan.RulePlanRequest{
		Scope:    sc,
		Rule:     mustParse(t, r),
		RuleRef:  domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:     domain.PlanManualRule,
		Guard:    e.guard(e.scopeCtx(sc.ID)),
		MaxFiles: 10,
		MaxBytes: 1 << 30,
		Config:   e.cfg,
		NowNS:    e.clock.Wall().UnixNano(),
	})
	if err == nil {
		t.Fatal("a globally protected name was planned")
	}
	requireCode(t, err, errs.CodeNoCandidates)
}

// A13: a paused installation commits no new intent.
func TestPauseStopsNewIntents(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	gate := scope.PauseGate{StateDir: e.paths.StateDir}
	if _, err := gate.Request("test"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply returned an error instead of a blocked result: %v", err)
	}
	if out.Code != string(errs.CodePaused) {
		t.Fatalf("expected the PAUSED stop code, got %q (%s)", out.Code, out.Message)
	}
	if out.Completed != 0 {
		t.Fatalf("something completed while paused: %+v", out)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file moved while paused")
	}
}
