package integration

import (
	"context"
	"syscall"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/scan"
)

// The tests in this file are about telling four outcomes apart after a listing:
//
//	the entry was described         -> an observation for this epoch
//	the entry is gone (ENOENT)      -> the file really ended; it may be retired
//	the entry could not be looked at-> nothing is known; the directory is unfinished
//	the directory could not be read -> nothing inside it is known; the same
//
// Merging the middle two is the failure this covers: "the filesystem would not
// answer" is not "the file is not there", and only one of them permits retiring
// an observation.

// entryStatFault installs a per-entry fault for the duration of a test.
func entryStatFault(t *testing.T, f func(rel string) error) {
	t.Helper()
	scan.TestHookEntryStat = f
	t.Cleanup(func() { scan.TestHookEntryStat = nil })
}

// TestSlimIncompleteScanKeepsTheEpochOpen is A10's first half: an entry that was
// listed and then could not be stat'ed leaves the directory unfinished across
// two rounds, does not stale the observation it could not confirm, and still
// updates the entries it could describe.
func TestSlimIncompleteScanKeepsTheEpochOpen(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("scanentries", "observe")
	for _, rel := range []string{"keep/a.zip", "keep/b.zip", "keep/c.zip"} {
		e.writeFile("scanentries", rel, "payload of "+rel, 60*24*time.Hour)
	}
	sctx := e.scopeCtx(sc.ID)

	_, state := e.scanRound(t, sctx)
	requireComplete(t, state)
	for _, rel := range []string{"keep/a.zip", "keep/b.zip", "keep/c.zip"} {
		if epoch, status := e.observationOf(t, sc.ID, rel); epoch != 1 || status != domain.ObsSeen {
			t.Fatalf("%s should be indexed in epoch 1: epoch %d status %q", rel, epoch, status)
		}
	}

	// One entry in the middle of the listing cannot be described. The two system
	// errors that mean this are EACCES and EIO; neither is ENOENT, and neither is
	// an answer about the file.
	var faultErr error = syscall.EACCES
	entryStatFault(t, func(rel string) error {
		if rel == "keep/b.zip" {
			return faultErr
		}
		return nil
	})

	stats, state := e.scanRound(t, sctx)
	if state.Complete {
		t.Fatalf("an entry that could not be inspected completed the epoch: %+v", state)
	}
	if !state.Incomplete || state.FailedDirs != 1 {
		t.Fatalf("expected one failed directory and an incomplete epoch, got %+v (stats %+v)", state, stats)
	}
	if stats.Unreadable == 0 || stats.Vanished != 0 {
		t.Fatalf("an unreadable entry should count as unreadable and not as vanished: %+v", stats)
	}
	if stats.Files != 2 {
		t.Fatalf("the two entries that could be described should have been recorded, got %d", stats.Files)
	}
	// The entries either side of the fault are re-observed in the new epoch...
	for _, rel := range []string{"keep/a.zip", "keep/c.zip"} {
		if epoch, status := e.observationOf(t, sc.ID, rel); epoch != 2 || status != domain.ObsSeen {
			t.Fatalf("%s should have been re-observed in epoch 2: epoch %d status %q", rel, epoch, status)
		}
	}
	// ...while the one that could not be inspected keeps its old observation,
	// with its old status. It is not stale: nothing was learned about it.
	if epoch, status := e.observationOf(t, sc.ID, "keep/b.zip"); epoch != 1 || status != domain.ObsSeen {
		t.Fatalf("an uninspected entry was treated as evidence of absence: epoch %d status %q", epoch, status)
	}

	// A second round with a different errno must not converge to "complete"
	// either. If it did, the retirement sweep would withdraw keep/b.zip.
	faultErr = syscall.EIO
	_, state = e.scanRound(t, sctx)
	if state.Complete || !state.Incomplete || state.FailedDirs != 1 {
		t.Fatalf("a repeated fault was not carried across rounds: %+v", state)
	}
	if state.Interrupts != 2 {
		t.Fatalf("expected two unfinished rounds, got %d", state.Interrupts)
	}
	if epoch, status := e.observationOf(t, sc.ID, "keep/b.zip"); epoch != 1 || status != domain.ObsSeen {
		t.Fatalf("keep/b.zip was retired while it could not be inspected: epoch %d status %q", epoch, status)
	}
	if got := mustScope(t, e, sc.ID).ScanEpoch; got != 2 {
		t.Fatalf("an unreadable entry started a new epoch (%d): the work it holds would never resume", got)
	}

	// The fault lifts. The epoch finishes, and only now may anything be retired.
	scan.TestHookEntryStat = nil
	_, state = e.scanRound(t, sctx)
	requireComplete(t, state)
	if state.Interrupts != 0 {
		t.Fatalf("a complete epoch should clear the unfinished-round counter, got %d", state.Interrupts)
	}
	if epoch, status := e.observationOf(t, sc.ID, "keep/b.zip"); epoch != 2 || status != domain.ObsSeen {
		t.Fatalf("keep/b.zip should be seen in epoch 2 once it can be inspected: epoch %d status %q", epoch, status)
	}
}

// TestSlimVanishedEntryIsNotAFailure is the other half of the same distinction:
// an entry that was listed and is really gone must not keep the epoch open, and
// the complete pass must retire it. Conflating the two would make a directory
// with any churn permanently unscannable.
func TestSlimVanishedEntryIsNotAFailure(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("scanvanish", "observe")
	e.writeFile("scanvanish", "keep/a.zip", "payload-a", 60*24*time.Hour)
	e.writeFile("scanvanish", "keep/b.zip", "payload-b", 60*24*time.Hour)
	sctx := e.scopeCtx(sc.ID)

	_, state := e.scanRound(t, sctx)
	requireComplete(t, state)

	// "b.zip" is in the listing and answers ENOENT when asked about itself. That
	// is an answer — the file ended — so it is counted apart and the directory
	// still finishes.
	entryStatFault(t, func(rel string) error {
		if rel == "keep/b.zip" {
			return syscall.ENOENT
		}
		return nil
	})
	stats, state := e.scanRound(t, sctx)
	if !state.Complete {
		t.Fatalf("an entry that is genuinely gone blocked the epoch: %+v (stats %+v)", state, stats)
	}
	if stats.Vanished != 1 {
		t.Fatalf("a vanished entry should be counted as vanished, got %+v", stats)
	}
	if stats.Unreadable != 0 {
		t.Fatalf("a vanished entry is not an unreadable one: %+v", stats)
	}
	if epoch, status := e.observationOf(t, sc.ID, "keep/a.zip"); epoch != 2 || status != domain.ObsSeen {
		t.Fatalf("keep/a.zip should be seen in epoch 2: epoch %d status %q", epoch, status)
	}
	if _, status := e.observationOf(t, sc.ID, "keep/b.zip"); status == domain.ObsSeen {
		t.Fatalf("keep/b.zip was not seen in epoch 2 and should have been retired, status %q", status)
	}
}

// TestSlimIncompleteScopeStartsNoAutomaticQuarantine is A10's rule about new
// automatic work: a rule evaluated against a partial picture is evaluated
// against the wrong tree, so the round records why it did not plan instead of
// planning from the part it happened to see. The same scope collects normally
// once the fault lifts, so the gate is a wait and not a permanent stop.
func TestSlimIncompleteScopeStartsNoAutomaticQuarantine(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("scanautq", domain.KindManagedDisposable)
	for _, rel := range []string{"keep/a.zip", "keep/b.zip", "keep/c.zip"} {
		e.writeFile("scanautq", rel, "payload of "+rel, 60*24*time.Hour)
	}
	r := e.rule(sc.ID, "scanautq-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	entryStatFault(t, func(rel string) error {
		if rel == "keep/b.zip" {
			return syscall.EACCES
		}
		return nil
	})
	rep := e.runOnce()
	if rep.Quarantined != 0 {
		t.Fatalf("a run whose scan was incomplete collected %d objects", rep.Quarantined)
	}
	if len(rep.Scopes) != 1 {
		t.Fatalf("expected one scope in the report, got %d", len(rep.Scopes))
	}
	sr := rep.Scopes[0]
	if sr.ScanComplete {
		t.Fatalf("the scope reported a complete scan: %+v", sr)
	}
	if sr.QuarantineSkipped == "" {
		t.Fatal("nothing was collected and the report does not say why")
	}
	if !contains(sr.QuarantineSkipped, "not complete") {
		t.Fatalf("the reported reason should name the incomplete scan, got %q", sr.QuarantineSkipped)
	}
	for _, rel := range []string{"keep/a.zip", "keep/b.zip", "keep/c.zip"} {
		if !e.exists("scanautq", rel) {
			t.Fatalf("%s was moved after an incomplete scan", rel)
		}
	}
	pending, err := e.st.PendingOps(context.Background())
	if err != nil {
		t.Fatalf("pending ops: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d pending operations were created by a run that planned nothing", len(pending))
	}

	// The fault lifts and the same rule collects: the scope was waiting, not
	// broken, and no manual step was needed to unstick it.
	scan.TestHookEntryStat = nil
	rep = e.runOnce()
	if rep.Quarantined != 3 {
		t.Fatalf("the run after the fault lifted collected %d objects", rep.Quarantined)
	}
	for _, rel := range []string{"keep/a.zip", "keep/b.zip", "keep/c.zip"} {
		if e.exists("scanautq", rel) {
			t.Fatalf("%s should have been collected once the scan completed", rel)
		}
	}
}
