package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/run"
	"dustkeeper/internal/scan"
	"dustkeeper/internal/store"
)

// scanRound runs one scanning batch through the production entry point and
// returns what the caller is told, so a test asserts on the reported state
// instead of reaching into the store for the same facts.
func (e *env) scanRound(t *testing.T, sctx *execute.ScopeCtx) (scan.Stats, run.ScanState) {
	t.Helper()
	ledger := domain.NewRunLedger(300, 1<<30, 0)
	stats, state, err := run.ScanScope(context.Background(), e.st, e.clock, 500, sctx, e.guard(sctx), ledger)
	if err != nil {
		e.t.Fatalf("scan round: %v", err)
	}
	return stats, state
}

// observationOf reads one indexed path and reports its epoch and status.
func (e *env) observationOf(t *testing.T, id domain.ScopeID, rel string) (int64, string) {
	t.Helper()
	o, err := e.st.GetObservation(context.Background(), id, []byte(rel))
	if err != nil {
		t.Fatalf("observation %s: %v", rel, err)
	}
	return o.ScanEpoch, o.Status
}

// requireComplete asserts one round closed the epoch.
func requireComplete(t *testing.T, state run.ScanState) {
	t.Helper()
	if !state.Complete {
		t.Fatalf("expected a complete epoch, got %+v", state)
	}
}

// TestAFailedDirectoryCannotCompleteTheEpochOrRetireItsOwnObservations is R11's
// first acceptance case.
//
// The loss this guards is not "a directory was skipped". It is that skipping was
// recorded as success: the failed directory was marked done, the next round saw an
// empty pending set, called the epoch complete, and retired every observation the
// epoch had never actually read. Two complete epochs run first so the failing
// round is epoch 3 — that is what makes a wrongly-completed epoch retire epoch-2
// observations as stale, which is the damage the review describes.
func TestAFailedDirectoryCannotCompleteTheEpochOrRetireItsOwnObservations(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("scanfail", "observe")
	e.writeFile("scanfail", "a/one.txt", "an old file under a", 90*24*time.Hour)
	e.writeFile("scanfail", "b/two.txt", "an old file under b", 90*24*time.Hour)
	sctx := e.scopeCtx(sc.ID)

	_, state := e.scanRound(t, sctx)
	requireComplete(t, state)
	_, state = e.scanRound(t, sctx)
	requireComplete(t, state)
	if got := (mustScope(t, e, sc.ID)).ScanEpoch; got != 2 {
		t.Fatalf("expected two completed epochs, scope is at epoch %d", got)
	}
	for _, rel := range []string{"a/one.txt", "b/two.txt"} {
		if epoch, status := e.observationOf(t, sc.ID, rel); epoch != 2 || status != domain.ObsSeen {
			t.Fatalf("%s should be indexed in epoch 2: epoch %d status %q", rel, epoch, status)
		}
	}

	// Nothing changes on disk from here on: only readability does.
	var failA bool = true
	scan.TestHookDirRead = func(rel string) error {
		if failA && rel == "a" {
			return errors.New("permission denied")
		}
		return nil
	}
	t.Cleanup(func() { scan.TestHookDirRead = nil })

	stats, state := e.scanRound(t, sctx)
	if state.Complete {
		q, _ := e.st.ScanQueueStateOf(context.Background(), sc.ID, mustScope(t, e, sc.ID).ScanEpoch)
		t.Fatalf("an unreadable directory was reported as a complete epoch: %+v (stats %+v queue %+v)", state, stats, q)
	}
	if !state.Incomplete || state.FailedDirs != 1 {
		t.Fatalf("expected one failed directory and an incomplete epoch, got %+v (stats %+v)", state, stats)
	}
	if state.Interrupts != 1 {
		t.Fatalf("expected the unfinished round to be counted, got %d", state.Interrupts)
	}
	if epoch, status := e.observationOf(t, sc.ID, "a/one.txt"); epoch != 2 || status != domain.ObsSeen {
		t.Fatalf("a/one.txt should still be the epoch-2 observation: epoch %d status %q", epoch, status)
	}
	if epoch, _ := e.observationOf(t, sc.ID, "b/two.txt"); epoch != 3 {
		t.Fatalf("b/two.txt should have been re-observed in epoch 3, got %d", epoch)
	}

	// A second round with the directory still unreadable must not converge to
	// "complete" either, and must not retire anything.
	_, state = e.scanRound(t, sctx)
	if state.Complete || !state.Incomplete || state.FailedDirs != 1 {
		t.Fatalf("a repeated failure was not carried across rounds: %+v", state)
	}
	if state.Interrupts != 2 {
		t.Fatalf("expected two unfinished rounds, got %d", state.Interrupts)
	}
	if epoch, status := e.observationOf(t, sc.ID, "a/one.txt"); epoch != 2 || status != domain.ObsSeen {
		t.Fatalf("a/one.txt was retired despite never being read: epoch %d status %q", epoch, status)
	}
	if n, err := e.st.CountObservations(context.Background(), sc.ID); err != nil || n != 2 {
		t.Fatalf("expected both paths still indexed, got %d (%v)", n, err)
	}
	if got := mustScope(t, e, sc.ID).ScanEpoch; got != 3 {
		t.Fatalf("a failed directory started a new epoch (%d): the work it holds would never resume", got)
	}

	// The failure lifts: the epoch finishes, and finishing it is what allows the
	// path to be re-observed rather than retired.
	failA = false
	_, state = e.scanRound(t, sctx)
	requireComplete(t, state)
	if epoch, status := e.observationOf(t, sc.ID, "a/one.txt"); epoch != 3 || status != domain.ObsSeen {
		t.Fatalf("a/one.txt should be seen in epoch 3 after the failure lifted: epoch %d status %q", epoch, status)
	}
	if state.Interrupts != 0 {
		t.Fatalf("a complete epoch should clear the unfinished-round counter, got %d", state.Interrupts)
	}
}

// TestAnInterruptedDirectoryDoesNotStarveTheOnesBehindIt is R11's second
// acceptance case.
//
// Path order alone was enough to starve: a directory that exhausts the budget
// every round was claimed first, interrupted, requeued, and claimed first again,
// so everything after it in path order never ran — for ever, because the epoch
// never advanced either. Claims are now ordered by last attempt time, so a
// directory that was just interrupted goes to the back of the queue.
func TestAnInterruptedDirectoryDoesNotStarveTheOnesBehindIt(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("scanfair", "observe")
	e.writeFile("scanfair", "a/deep/one.txt", "under the big directory", 90*24*time.Hour)
	e.writeFile("scanfair", "b/two.txt", "under b", 90*24*time.Hour)
	e.writeFile("scanfair", "c/three.txt", "under c", 90*24*time.Hour)
	sctx := e.scopeCtx(sc.ID)

	// "a" never finishes: as soon as it is opened the batch is out of budget.
	// That is the shape of a directory too large for one time slice. The ledger
	// belongs to the round, so the hook rewrites the round's own ledger.
	t.Cleanup(func() { scan.TestHookDirRead = nil })

	for round := 1; round <= 3; round++ {
		ledger := domain.NewRunLedger(300, 1<<30, 0)
		scan.TestHookDirRead = func(rel string) error {
			if rel == "a" {
				ledger.SoftTime = time.Nanosecond
			}
			return nil
		}
		stats, state, err := run.ScanScope(context.Background(), e.st, e.clock, 500, sctx, e.guard(sctx), ledger)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if state.Complete {
			t.Fatalf("round %d: a directory that never finished cannot complete the epoch: %+v", round, state)
		}
		if !state.Interrupted {
			t.Fatalf("round %d: expected the budget stop to be reported: %+v (stats %+v)", round, state, stats)
		}
		if state.Interrupts != int64(round) {
			t.Fatalf("round %d: expected %d unfinished rounds, got %d", round, round, state.Interrupts)
		}
	}

	// Rounds 2 and 3 reached the small directories even though "a" is always
	// interrupted first in path order.
	for _, rel := range []string{"b/two.txt", "c/three.txt"} {
		epoch, status := e.observationOf(t, sc.ID, rel)
		if epoch == 0 || status != domain.ObsSeen {
			t.Fatalf("%s was starved: epoch %d status %q", rel, epoch, status)
		}
	}
	// "a" is still owed work, and the queue says so instead of forgetting it.
	q, err := e.st.ScanQueueStateOf(context.Background(), sc.ID, mustScope(t, e, sc.ID).ScanEpoch)
	if err != nil {
		t.Fatalf("queue state: %v", err)
	}
	if q.Pending == 0 {
		t.Fatalf("the interrupted directory left the queue: %+v", q)
	}
}

// mustScope loads a scope or fails the test.
func mustScope(t *testing.T, e *env, id domain.ScopeID) store.Scope {
	t.Helper()
	sc, err := e.st.GetScope(context.Background(), id)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	return sc
}
