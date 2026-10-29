package integration

// C05, the half that was still open: the compensation's terminal transaction.
//
// The transaction itself was fixed - the item's verdict, the scope's release and
// the closing event commit together - but the caller still turned its failure into
// a string and then into HELD. HELD is a verdict about one object and stops one
// scope; a database that refused a write has to stop the run, or the pass goes on
// to the next scope and the report describes a storage failure as a decision.
//
// The failure is injected where the report suggested: a trigger on the scope
// update inside that transaction, created in the test's own state database and
// dropped before the following reconciliation. Nothing in the product schema knows
// about it, and the item the trigger protects is the one whose release is the
// point of the transaction - a scope that still holds work must not be released.

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
)

func TestACompensationWhoseRecordCannotBeClosedStopsTheRun(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "the planned content", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "the second object", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if len(res.Items) != 2 {
		t.Fatalf("the plan has %d members, want 2", len(res.Items))
	}
	first, second := res.Items[0], res.Items[1]

	// The object that actually sits at the planned path when the rename happens is
	// not the one the plan measured: that is a miscapture, and its compensation is
	// the code under test.
	srcPath := e.treePath("exports", string(first.RawRelative))
	const unplanned = "the object nobody planned for"
	execute.TestHookPreRename = func() {
		tmp := srcPath + ".swap"
		if err := os.WriteFile(tmp, []byte(unplanned), 0o644); err != nil {
			return
		}
		_ = os.Rename(tmp, srcPath)
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	// The release of a held scope is a write like any other, and this one fails
	// once. A trigger is used rather than a hook because the write is inside the
	// store's transaction: the failure has to come from the database, after the
	// item's own update has already run, or the atomicity being tested is not.
	e.execRaw(t, `CREATE TRIGGER refuse_scope_release BEFORE UPDATE ON scopes
	WHEN OLD.status='held' AND NEW.status='needs_reapproval'
	BEGIN SELECT RAISE(ABORT, 'the scope update was refused'); END;`)

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Code != string(errs.CodeStateWriteFailed) {
		t.Fatalf("the batch ended with %q, not the storage failure: %+v", out.Code, out.Results)
	}
	if out.Control != domain.ControlStopRun {
		t.Fatalf("the batch stopped with control %q, and a database that refused a write stops the run", out.Control)
	}

	// The item that failed names the same code and the same stop range. Anything
	// less and the row says "held" while the batch says "run stopped", which is the
	// inconsistency the review found.
	failed := resultFor(t, out, first.ID)
	if failed.Code != string(errs.CodeStateWriteFailed) {
		t.Fatalf("the item result is %q, not the storage failure: %+v", failed.Code, failed)
	}
	if failed.Control != domain.ControlStopRun {
		t.Fatalf("the item result stops %q, not the run: %+v", failed.Control, failed)
	}
	if failed.State != domain.StateRIntent {
		t.Fatalf("the item result reports state %q, but its compensation intent is still open", failed.State)
	}

	// Nothing was claimed and nothing was lost: the object is back where it came
	// from, the vault no longer holds it, and the operation is still pending for
	// reconciliation - which is the only thing that can finish it now.
	if got, rerr := os.ReadFile(srcPath); rerr != nil || string(got) != unplanned {
		t.Fatalf("the compensated object is not back at its path: %q (%v)", got, rerr)
	}
	stored := itemOf(t, e, first.ID)
	if stored.State != domain.StateRIntent {
		t.Fatalf("the item is recorded as %q; the compensation intent did not close", stored.State)
	}
	if e.pendingOf(t, first.ID) == "" {
		t.Fatal("the failed transaction cleared the pending marker; nothing would retry it")
	}
	scopeRow, serr := e.st.GetScope(ctx, sc.ID)
	if serr != nil {
		t.Fatalf("get scope: %v", serr)
	}
	if scopeRow.Status != domain.ScopeHeld {
		t.Fatalf("the scope is %q; the release rolled back, so it must still be held", scopeRow.Status)
	}

	// And the run stopped: the second object was never touched.
	if secondResult := resultFor(t, out, second.ID); secondResult.Code != string(errs.CodePaused) {
		t.Fatalf("the second member was %q, so the batch went on after a storage failure", secondResult.Code)
	}
	if !e.exists("exports", string(second.RawRelative)) {
		t.Fatal("the second object was moved out of the tree by a batch that had already stopped")
	}

	// With the fault gone, one pass finishes the operation and the next finds
	// nothing: the pending marker written by the failure is enough to repair it.
	e.execRaw(t, "DROP TRIGGER refuse_scope_release")
	reconcileTillQuiet(t, e)

	settled := itemOf(t, e, first.ID)
	if settled.State != domain.StateRestored {
		t.Fatalf("reconciliation left the item %q, not restored", settled.State)
	}
	if p := e.pendingOf(t, first.ID); p != "" {
		t.Fatalf("reconciliation left the operation open: %s", p)
	}
	after, serr := e.st.GetScope(ctx, sc.ID)
	if serr != nil {
		t.Fatalf("get scope after reconcile: %v", serr)
	}
	if after.Status != domain.ScopeNeedsReapproval {
		t.Fatalf("the scope is %q; a repaired hold asks for re-approval, it does not clear itself", after.Status)
	}
}

// countItems counts the object rows of one scope, straight from the state file:
// "the scope was never started" is a fact about rows, and no store accessor
// answers "how many are there".
func countItems(t *testing.T, e *env, scopeID domain.ScopeID) int {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+e.paths.DBFile+"?_busy_timeout=2000&_txlock=immediate")
	if err != nil {
		t.Fatalf("open the state file: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM items WHERE scope_id=?", string(scopeID)).Scan(&n); err != nil {
		t.Fatalf("count the objects of %s: %v", scopeID, err)
	}
	return n
}

// resultFor returns the result row for one item.
func resultFor(t *testing.T, out execute.ApplyResult, id domain.ItemID) domain.OperationResult {
	t.Helper()
	for _, r := range out.Results {
		if r.Item == id {
			return r
		}
	}
	t.Fatalf("the batch has no result for item %s: %+v", id, out.Results)
	return domain.OperationResult{}
}

// The same failure, seen from the round instead of from the batch.
//
// HELD stops one scope and the round moves on; a database that refused a write
// stops the round. Which of the two happens is decided by the control value the
// failed item carries, so this test follows it all the way out: the report ends
// with the run-level code, and the scope behind the failing one is never started.
func TestACompensationStorageFailureStopsTheWholeRound(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	failing := e.addScope("alpha", domain.KindManagedDisposable)
	behind := e.addScope("beta", domain.KindManagedDisposable)
	e.writeFile("alpha", "a.zip", "the planned content", 40*24*time.Hour)
	e.writeFile("beta", "c.zip", "another tree entirely", 40*24*time.Hour)
	e.scan(failing.ID)
	e.scan(behind.ID)
	rFailing := e.rule(failing.ID, "rule-a", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	rBehind := e.rule(behind.ID, "rule-b", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(rFailing, domain.CapAutoQuarantine, 5, 1<<20)
	e.grant(rBehind, domain.CapAutoQuarantine, 5, 1<<20)

	// The round is made to start at the scope that will fail, so that "the scope
	// behind it never started" is a statement about the stop and not about the
	// order the scopes happened to be listed in.
	if err := e.st.MetaSet(ctx, "rotation_cursor", string(failing.ID)); err != nil {
		t.Fatalf("point the round at the failing scope: %v", err)
	}

	srcPath := e.treePath("alpha", "a.zip")
	const unplanned = "the object nobody planned for"
	swapped := false
	execute.TestHookPreRename = func() {
		// Only the first tree is disturbed. The hook fires for every rename in the
		// round, and a second miscapture in the scope behind would say nothing about
		// the stop this test is measuring.
		if swapped {
			return
		}
		swapped = true
		tmp := srcPath + ".swap"
		if err := os.WriteFile(tmp, []byte(unplanned), 0o644); err != nil {
			return
		}
		_ = os.Rename(tmp, srcPath)
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	e.execRaw(t, `CREATE TRIGGER refuse_scope_release BEFORE UPDATE ON scopes
	WHEN OLD.status='held' AND NEW.status='needs_reapproval'
	BEGIN SELECT RAISE(ABORT, 'the scope update was refused'); END;`)

	rep := e.runOnce()
	if rep.FailureCode != errs.CodeStateWriteFailed {
		t.Fatalf("the round ended with %q, not the storage failure: %+v / %+v",
			rep.FailureCode, rep.Scopes, rep.Failures)
	}
	if rep.Control != domain.ControlStopRun {
		t.Fatalf("the round stopped with control %q; a write that failed stops the round", rep.Control)
	}
	if len(rep.Scopes) != 1 || rep.Scopes[0].Scope != failing.ID {
		t.Fatalf("the round went on past the failing scope: %+v", rep.Scopes)
	}

	// The tree it never reached is exactly where it was, and the scope has no
	// record of work that never started.
	if !e.exists("beta", "c.zip") {
		t.Fatal("the scope behind the failure had a file moved out of it")
	}
	if n := countItems(t, e, behind.ID); n != 0 {
		t.Fatalf("the untouched scope has %d object row(s); the round started it after a storage failure", n)
	}

	// The failing scope kept its pending marker, so the pass that repairs it is the
	// one that ran into the failure - not a new plan on top of an unknown state.
	e.execRaw(t, "DROP TRIGGER refuse_scope_release")
	reconcileTillQuiet(t, e)
	settled, serr := e.st.GetScope(ctx, failing.ID)
	if serr != nil {
		t.Fatalf("get scope: %v", serr)
	}
	if settled.Status != domain.ScopeNeedsReapproval {
		t.Fatalf("the repaired scope is %q, want needs_reapproval", settled.Status)
	}
	if got, rerr := os.ReadFile(srcPath); rerr != nil || string(got) != unplanned {
		t.Fatalf("the compensated object is not in its old place: %q (%v)", got, rerr)
	}
}
