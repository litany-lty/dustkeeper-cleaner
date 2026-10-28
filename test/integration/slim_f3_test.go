package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
)

// The tests in this file are about the last few metres of a file operation: what
// is checked immediately before the intent, what makes two reads "the same
// object", when a stopped batch is reported as stopped, and what a reconciliation
// pass does with a failure that may clear on its own.

// countingClock reports a monotonic reading that advances by a fixed step on every
// call, so a test can decide which checkpoint inside one action crosses the run's
// deadline. Wall time comes from the environment's fake clock unchanged.
type countingClock struct {
	wall  domain.Clock
	step  time.Duration
	mu    sync.Mutex
	calls int
}

func (c *countingClock) Wall() time.Time { return c.wall.Wall() }

func (c *countingClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return time.Duration(c.calls) * c.step
}

// manifestsIn lists the preparation manifests a scope has written.
func (e *env) manifestsIn(scopeDir string) []string {
	e.t.Helper()
	dir := filepath.Join(e.base, "trees", scopeDir, ".dustkeeper", "manifests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		e.t.Fatalf("read manifests: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, en := range entries {
		out = append(out, en.Name())
	}
	return out
}

// TestSlimQuarantineFinalGate is A07's resource half. The last checkpoint before a
// quarantine is accepted is not decoration: the file has not moved yet, the clock,
// the allowance and the protection set can all have changed during the read that
// produced the manifest, and nothing durable may be left behind when one of them
// says stop.
func TestSlimQuarantineFinalGate(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	it := res.Items[0]

	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)

	// Quarantine asks the clock for its monotonic reading three times before the
	// intent: before the read, after the read, and immediately before the intent.
	// A step of 10s against a 25s slice is still allowed at the first two and
	// over the line at the third, which is exactly the "the budget ran out after
	// the manifest was already durable" case.
	normal := e.exec.Clock
	e.exec.Clock = &countingClock{wall: normal, step: 10 * time.Second}
	t.Cleanup(func() { e.exec.Clock = normal })

	budget := domain.NewBudget(25, 1<<30, 10, 1<<30, 0)
	_, err := e.exec.Quarantine(context.Background(), sctx, res.Plan, it, budget)
	requireCode(t, err, errs.CodeBudgetExhausted)
	if !contains(err.Error(), "before this file could be accepted") {
		t.Fatalf("the refusal did not come from the last checkpoint: %v", err)
	}

	// Nothing durable is left behind, and nothing moved.
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file moved although the budget ran out before the intent")
	}
	if got := e.pendingOf(t, it.ID); got != "" {
		t.Fatalf("a pending operation was recorded: %q", got)
	}
	item, gerr := e.st.GetItem(context.Background(), it.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StatePlanned {
		t.Fatalf("item state = %s, want %s", item.State, domain.StatePlanned)
	}
	if left := e.manifestsIn("exports"); len(left) != 0 {
		t.Fatalf("the abandoned preparation was left behind: %v", left)
	}

	// And the next attempt is not blocked by any of it.
	e.exec.Clock = normal
	fresh := domain.NewBudget(300, 1<<30, 10, 1<<30, 0)
	out, err := e.exec.Quarantine(context.Background(), sctx, res.Plan, it, fresh)
	if err != nil {
		t.Fatalf("the retry after an abandoned preparation failed: %v", err)
	}
	if out.State != domain.StateQuarantined {
		t.Fatalf("the retry left the item in %s", out.State)
	}
	if e.exists("exports", "a.zip") {
		t.Fatal("the retry reported success but the file is still in the tree")
	}
}

// TestSlimReadStability is A07's evidence half. A file whose bytes were replaced
// and whose modification time was put back is not the file that was planned: only
// the change time can tell, which is why it is part of the comparison. A normal
// rename must still be accepted, or the check would forbid the product's own
// move.
func TestSlimReadStability(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "original", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)

	// Same size, new bytes, and the modification time put back where it was: the
	// only surviving difference is the change time.
	path := e.treePath("exports", "a.zip")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mtime := info.ModTime()
	if err := os.WriteFile(path, []byte("replaced"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}

	// The refusal is reported on the batch: a per-item stop code is not a Go
	// error, and a caller that only checked err would read this as success.
	out, _ := e.apply(sc, res.Plan, res.Plan.Digest)
	if out.Code != string(errs.CodePlanStale) {
		t.Fatalf("batch code = %q, want %q (message %q)", out.Code, errs.CodePlanStale, out.Message)
	}
	if out.Completed != 0 {
		t.Fatalf("completed_count = %d for a changed file", out.Completed)
	}
	// And the per-item record says which item and why.
	if len(out.Results) != 1 || out.Results[0].Code != string(errs.CodePlanStale) {
		t.Fatalf("the per-item record does not carry the reason: %+v", out.Results)
	}
	if !contains(out.Results[0].Message, "changed after planning") &&
		!contains(out.Results[0].Message, "change time") {
		t.Fatalf("the refusal does not name the instability: %q", out.Results[0].Message)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("a file that changed under the plan was moved anyway")
	}

	// The positive half: the same code path accepts a file that really is the
	// planned object, and the rename it performs is not mistaken for a change.
	e2 := newEnv(t)
	sc2 := e2.addScope("exports", domain.KindManagedDisposable)
	e2.writeFile("exports", "b.zip", "payload", 40*24*time.Hour)
	e2.scan(sc2.ID)
	r2 := e2.rule(sc2.ID, "rule-2", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res2 := e2.planFor(sc2, r2, 10)
	applied, err := e2.apply(sc2, res2.Plan, res2.Plan.Digest)
	if err != nil {
		t.Fatalf("a normal quarantine was refused: %v", err)
	}
	if applied.Completed != 1 {
		t.Fatalf("completed_count = %d for one matching file", applied.Completed)
	}
	if e2.exists("exports", "b.zip") {
		t.Fatal("the file was not moved")
	}

	// And the restore that follows moves it back without being told it changed.
	item := e2.itemsFor(res2.Plan)[0]
	item, err = e2.st.GetItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if _, err := e2.exec.Restore(context.Background(), e2.scopeCtx(sc2.ID), item.ID,
		execute.RestoreOptions{}); err != nil {
		t.Fatalf("a normal restore was refused: %v", err)
	}
	if !e2.exists("exports", "b.zip") {
		t.Fatal("the object was not restored")
	}
}

// TestSlimUnresolvedObjectBlocksBothEntrances is A08's control half: an object
// whose fate is not settled stops new work on its scope, whether the next action
// is asked for by a person or started by the background run.
func TestSlimUnresolvedObjectBlocksBothEntrances(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 100, 1<<30)

	first := e.planFor(sc, r, 10)
	if _, err := e.apply(sc, first.Plan, first.Plan.Digest); err != nil {
		t.Fatalf("first quarantine: %v", err)
	}
	quarantined := e.itemsFor(first.Plan)[0]
	// An object whose outcome is unresolved: the state says so even though no
	// operation is pending any more.
	e.execRaw(t, "UPDATE items SET state='held' WHERE item_id=?", string(quarantined.ID))

	// A second file appears. It is planned and applied by hand below.
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)
	second := e.planFor(sc, r, 10)
	if len(second.Items) != 1 {
		t.Fatalf("the second plan holds %d item(s), want only the new file", len(second.Items))
	}

	applied, _ := e.apply(sc, second.Plan, second.Plan.Digest)
	if applied.Code != string(errs.CodeHeld) {
		t.Fatalf("batch code = %q, want %q (message %q)", applied.Code, errs.CodeHeld, applied.Message)
	}
	if applied.Completed != 0 {
		t.Fatalf("a manual apply moved %d object(s) while an object is unresolved", applied.Completed)
	}
	if !e.exists("exports", "b.zip") {
		t.Fatal("the manual route moved a file while the scope had an unresolved object")
	}
	if got := e.pendingOf(t, second.Items[0].ID); got != "" {
		t.Fatalf("the refused manual attempt left a pending operation: %q", got)
	}

	// The automatic route asks the same question and gets the same answer. A new
	// file is used because the planner does not re-plan an object that is already
	// sitting in an open plan.
	e.writeFile("exports", "c.zip", "payload-c", 40*24*time.Hour)
	e.scan(sc.ID)
	rep := e.runOnce()
	if rep.Quarantined != 0 {
		t.Fatalf("the run moved %d object(s) while an object is unresolved", rep.Quarantined)
	}
	if rep.FailureCode != errs.CodeHeld {
		t.Fatalf("run failure code = %q, want %q", rep.FailureCode, errs.CodeHeld)
	}
	if !e.exists("exports", "c.zip") {
		t.Fatal("the automatic route moved a file while the scope had an unresolved object")
	}
}

// TestAQuarantineOnlyApprovalNeverBecomesADeletion is A06's last clause. Time
// passing is not an approval: an object quarantined under a quarantine-only
// approval stays in the vault when its window elapses.
func TestAQuarantineOnlyApprovalNeverBecomesADeletion(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 100, 1<<30)

	// One background run does the quarantine, exactly as a scheduled install does.
	rep := e.runOnce()
	if rep.Quarantined != 1 {
		t.Fatalf("the run quarantined %d object(s), want 1", rep.Quarantined)
	}
	if e.exists("exports", "a.zip") {
		t.Fatal("the object was not moved into the vault")
	}

	// Its window elapses with no purge approval anywhere.
	for i := 0; i < 8; i++ {
		e.step(24 * time.Hour)
	}
	remaining := e.quarantinedItems(sc.ID)
	if len(remaining) != 1 {
		t.Fatalf("%d object(s) remain in the vault, want the quarantined one", len(remaining))
	}
	rep = e.runOnce()
	if rep.Purged != 0 {
		t.Fatalf("the run purged %d object(s) without a purge approval", rep.Purged)
	}
	if !e.vaultObjectExists(sc, remaining[0]) {
		t.Fatal("the object is no longer in the vault")
	}
}

// contains is a local helper so the assertions read as sentences.
func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestAFailedRecordIsReportedNotSwallowed is the other half of A09's honesty
// requirement: when the outcome of a pass cannot be written down, the pass says
// so. The object is untouched either way, so the difference is entirely in the
// report - which is exactly what used to be missing.
func TestAFailedRecordIsReportedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, nil)
	e.closeScopeCtxs()

	// The object is in neither place: no branch of the matrix explains it, so the
	// pass will try to record a verdict.
	if err := os.Remove(e.vaultPath("exports", f.item.ObjectName)); err != nil {
		t.Fatalf("remove the vault object: %v", err)
	}
	// And the event log refuses new rows, so the verdict has nowhere to go.
	e.execRaw(t, `CREATE TRIGGER test_refuse_event BEFORE INSERT ON events
BEGIN SELECT RAISE(ABORT, 'injected'); END;`)

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.RecordFailures != 1 {
		t.Fatalf("record_failures = %d, want 1 (report %+v)", rep.RecordFailures, rep)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("%d result row(s)", len(rep.Results))
	}
	res := rep.Results[0]
	if res.RecordError == "" {
		t.Fatalf("the result does not name the failed write: %+v", res)
	}
	if res.Outcome == "held" {
		t.Fatal("the pass reported a verdict it could not record")
	}
	// The pending marker is the thing that keeps the object visible and retryable.
	if got := e.pendingOf(t, f.item.ID); got == "" {
		t.Fatal("the pending operation was dropped although nothing was recorded")
	}
}
