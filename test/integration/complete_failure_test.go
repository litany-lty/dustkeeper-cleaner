package integration

// A09: "the system call succeeded, the record did not".
//
// Every action has a window where the namespace change is already durable and
// the terminal row has not been written: the object is in the vault and the item
// still says q_intent, the file is back and the item still says r_intent, the
// bytes are gone and the item still says p_intent. That is the state a crash
// leaves, and the state a full disk or an I/O error on the state database leaves
// on a process that keeps running.
//
// The window cannot be reached by timing, so the executor exposes one named hook
// that fails the terminal write (internal/execute/hooks.go). These tests use it
// to prove the property the window exists for: reconciliation finishes the
// operation, a second pass changes nothing, and nothing in between claims the
// action was not performed.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// failTheTerminalWriteOf makes one action's terminal write fail, and leaves the
// others alone. The returned function clears the hook early; a cleanup clears it
// in any case, because the hook is process-wide and a test that fails before
// reaching clear() would otherwise leave every later action in the package
// failing its terminal write.
func failTheTerminalWriteOf(t *testing.T, action string) func() {
	t.Helper()
	clear := func() { execute.TestHookBeforeComplete = nil }
	t.Cleanup(clear)
	execute.TestHookBeforeComplete = func(got string) error {
		if got == action {
			return errors.New("sqlite: disk I/O error")
		}
		return nil
	}
	return clear
}

// itemOf reads one item back.
func itemOf(t *testing.T, e *env, id domain.ItemID) store.Item {
	t.Helper()
	it, err := e.st.GetItem(context.Background(), id)
	if err != nil {
		t.Fatalf("get item %s: %v", id, err)
	}
	return it
}

// reconcileTillQuiet runs one pass that must finish the operation, then a second
// that must find nothing left to do. The second pass is the point: a
// reconciliation that keeps "completing" the same operation would mean the first
// pass wrote an outcome the record cannot see.
func reconcileTillQuiet(t *testing.T, e *env) {
	t.Helper()
	first, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if first.Completed != 1 {
		t.Fatalf("the first pass completed %d operations, want 1: %+v", first.Completed, first)
	}
	if first.RecordFailures != 0 {
		t.Fatalf("the first pass could not write %d outcome(s): %+v", first.RecordFailures, first)
	}

	second, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Examined != 0 || second.Completed != 0 {
		t.Fatalf("the second pass examined %d and completed %d operations; the first one did not converge",
			second.Examined, second.Completed)
	}
}

// TestAQuarantineWhoseRecordFailedIsFinishedByReconcile covers the quarantine
// window: the bytes are in the vault, the item still says q_intent.
func TestAQuarantineWhoseRecordFailedIsFinishedByReconcile(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	it := res.Items[0]

	clear := failTheTerminalWriteOf(t, "quarantine")
	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 0 || out.Failed != 1 {
		t.Fatalf("the plan reported %d completed and %d failed: %+v", out.Completed, out.Failed, out.Results)
	}
	if code := out.Results[0].Code; code != string(errs.CodeStateWriteFailed) {
		t.Fatalf("the item ended with %q, want %q", code, errs.CodeStateWriteFailed)
	}
	clear()

	// The action did happen. Reporting failure must not be read as "nothing
	// moved", or an operator would look for the file where it no longer is.
	if e.exists("exports", "a.zip") {
		t.Fatal("the file is still at its original path, so the failure happened before the move")
	}
	if !e.exists("exports", filepath.Join(".dustkeeper", "objects", it.ObjectName)) {
		t.Fatal("the file is not in the vault either")
	}
	got := itemOf(t, e, it.ID)
	if got.State != domain.StateQIntent {
		t.Fatalf("the item is %s, want it left at %s", got.State, domain.StateQIntent)
	}
	if got.PendingOpID == nil {
		t.Fatal("the item carries no pending operation, so nothing would ever finish it")
	}

	reconcileTillQuiet(t, e)

	got = itemOf(t, e, it.ID)
	if got.State != domain.StateQuarantined {
		t.Fatalf("the item is %s after reconciliation, want quarantined", got.State)
	}
	if got.PendingOpID != nil {
		t.Fatalf("the item still carries operation %s", *got.PendingOpID)
	}
	if got.CapturedIdentityJSON == nil || got.ContentHash == nil {
		t.Fatal("reconciliation did not record what it captured")
	}
	if got.PurgeNotBeforeNS == nil {
		t.Fatal("reconciliation did not start a retention window")
	}
	// A completed quarantine is the input a purge needs, so the pass that
	// finishes it must not leave the object unable to reach its next state.
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, *got.PurgeNotBeforeNS, 10)
	if err != nil {
		t.Fatalf("due items: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("the reconciled object is not one the purge queue can see: %d", len(due))
	}
}

// TestARestoreWhoseRecordFailedIsFinishedByReconcile covers the restore window:
// the file is back at its destination, the item still says r_intent.
func TestARestoreWhoseRecordFailedIsFinishedByReconcile(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	it := res.Items[0]

	clear := failTheTerminalWriteOf(t, "restore")
	_, err := e.exec.Restore(context.Background(), e.scopeCtx(sc.ID), it.ID, execute.RestoreOptions{})
	if err == nil {
		t.Fatal("the restore was reported as complete although its record could not be written")
	}
	requireCode(t, err, errs.CodeStateWriteFailed)
	clear()

	if !e.exists("exports", "a.zip") {
		t.Fatal("the file is not back at its original path, so the failure happened before the move")
	}
	if e.exists("exports", filepath.Join(".dustkeeper", "objects", it.ObjectName)) {
		t.Fatal("the object is still in the vault")
	}
	got := itemOf(t, e, it.ID)
	if got.State != domain.StateRIntent {
		t.Fatalf("the item is %s, want it left at %s", got.State, domain.StateRIntent)
	}
	if got.PendingOpID == nil {
		t.Fatal("the item carries no pending operation, so nothing would ever finish it")
	}

	reconcileTillQuiet(t, e)

	got = itemOf(t, e, it.ID)
	if got.State != domain.StateRestored {
		t.Fatalf("the item is %s after reconciliation, want restored", got.State)
	}
	if got.PendingOpID != nil {
		t.Fatalf("the item still carries operation %s", *got.PendingOpID)
	}
	// The object left the scope's managed set, so it must not be offered as a
	// quarantined object that a later purge could act on.
	items := e.quarantinedItems(sc.ID)
	if len(items) != 0 {
		t.Fatalf("%d object(s) are still in the quarantine queue after the restore was finished", len(items))
	}
}

// TestAPurgeWhoseRecordFailedIsFinishedByReconcile covers the purge window: the
// bytes are gone, the item still says p_intent. It is the one where guessing
// wrongly is expensive in both directions - a reconciliation that says "not
// purged" invites a second deletion of something that is not there, and one that
// says "purged" without evidence is a claim about data nobody can check.
func TestAPurgeWhoseRecordFailedIsFinishedByReconcile(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	it := res.Items[0]

	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due items: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected one due object, got %d", len(due))
	}

	clear := failTheTerminalWriteOf(t, "purge")
	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, due, domain.PlanManualPurge,
		e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatalf("purge plan: %v", err)
	}
	out, err := e.exec.ApplyPurgePlan(context.Background(), e.scopeCtx(sc.ID), p.ID, p.Digest,
		domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
	if err != nil {
		t.Fatalf("apply purge: %v", err)
	}
	if out.Completed != 0 || out.Failed != 1 {
		t.Fatalf("the plan reported %d completed and %d failed: %+v", out.Completed, out.Failed, out.Results)
	}
	if code := out.Results[0].Code; code != string(errs.CodeStateWriteFailed) {
		t.Fatalf("the item ended with %q, want %q", code, errs.CodeStateWriteFailed)
	}
	clear()

	if e.exists("exports", filepath.Join(".dustkeeper", "objects", it.ObjectName)) {
		t.Fatal("the object is still in the vault, so the failure happened before the unlink")
	}
	got := itemOf(t, e, it.ID)
	if got.State != domain.StatePIntent {
		t.Fatalf("the item is %s, want it left at %s", got.State, domain.StatePIntent)
	}
	if got.PendingOpID == nil {
		t.Fatal("the item carries no pending operation, so nothing would ever finish it")
	}

	reconcileTillQuiet(t, e)

	got = itemOf(t, e, it.ID)
	if got.State != domain.StatePurged {
		t.Fatalf("the item is %s after reconciliation, want purged", got.State)
	}
	if got.PendingOpID != nil {
		t.Fatalf("the item still carries operation %s", *got.PendingOpID)
	}
}
