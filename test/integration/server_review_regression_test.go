package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
)

// This file closes the three defects a review of 25dcee2 reproduced by running
// the program, plus the two wiring gaps it confirmed by reading it. Each test
// states one promise in its own name, and fails when the program stops keeping
// it.
//
// They are separate from review_root_regression_test.go because they are the
// follow-up round: the earlier file pins the first review's findings, this one
// pins what was still open afterwards.

// TestServerReviewDirectDeleteHonoursPurgeGrant is S01.
//
// The purge approval is the user's "how many may be permanently deleted" answer.
// It was applied to the expiry queue and to the run ledger, but not to the
// direct-deletion branch, so an approval for one file produced three deletions:
// the number the user approved never reached the action that spent it.
func TestServerReviewDirectDeleteHonoursPurgeGrant(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("scratch", domain.KindManagedDisposable)
	for _, name := range []string{"a.zip", "b.zip", "c.zip"} {
		e.writeFile("scratch", name, "discard-payload", 40*24*time.Hour)
	}
	e.scan(sc.ID)
	r := e.rule(sc.ID, "delete-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	// The run and the policy both allow more than one; the approval is the
	// smaller ceiling and it is the one that must win.
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	pg := e.grant(r, domain.CapAutoPurge, 1, 1<<30)
	e.savePolicy(directDeletePolicy(sc.ID))
	e.exec.Review = agreeWithTheQuestion

	rep := e.runOnce()

	// Counted from the completion events, not from the source paths that are
	// gone: a file can leave its path for several reasons, and only one of them
	// is a deletion this run performed.
	completed := 0
	events, err := e.st.ListEvents(context.Background(), 200, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.Action == domain.ActionDelete && ev.Phase == domain.PhaseComplete {
			completed++
		}
	}
	if completed > 1 {
		t.Fatalf("a one-file purge approval was exceeded: grant=%+v delete_completions=%d purged=%d quarantined=%d",
			pg, completed, rep.Purged, rep.Quarantined)
	}
	if completed != 1 {
		t.Fatalf("the fixture did not exercise direct deletion: events=%d report=%+v", completed, rep)
	}
	// Two of the three files are still there, and they are not in the vault.
	inTree := 0
	for _, name := range []string{"a.zip", "b.zip", "c.zip"} {
		if e.exists("scratch", name) {
			inTree++
		}
	}
	if inTree != 2 {
		t.Fatalf("%d file(s) remain in the tree, want 2", inTree)
	}
}

// TestServerReviewOneApprovalCoversBothClearingAndDirectDeletion is the second
// half of S01, and the half that a "min(pg) at the deletion site" patch would
// still get wrong.
//
// The expiry clearance and the direct deletion spend the same approval in the
// same run. Two counters - one per phase - each see "one file left" and each
// spend it, so an approval for one file produces one cleared object and one
// destroyed candidate.
func TestServerReviewOneApprovalCoversBothClearingAndDirectDeletion(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "payload-old", 40*24*time.Hour)
	e.scan(sc.ID)
	// First the object is observed; a policy that allows direct deletion would
	// have destroyed it instead of putting it in the vault.
	e.confirmPolicy(14, sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 1, 1<<30)
	e.exec.Review = agreeWithTheQuestion

	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("fixture: expected one object observed, got %+v", rep)
	}
	if len(e.quarantinedItems(sc.ID)) != 1 {
		t.Fatalf("fixture: the object is not in the observation area")
	}

	// Now the same installation also allows direct deletion of archives. Thirteen
	// days of ordinary runs go by with nothing new to do; then a candidate appears
	// in the tree, and the next run is the one where both claims meet: an object
	// whose retention window has ended and a file the rule may destroy outright.
	pol := directDeletePolicy(sc.ID)
	e.savePolicy(pol)
	e.advancePast(13 * 24 * time.Hour)
	e.writeFile("exports", "new.zip", "payload-new", 40*24*time.Hour)

	// One step past the fourteen-day retention window.
	rep := e.step(47 * time.Hour)

	if rep.Purged != 1 {
		t.Fatalf("the due object was not cleared exactly once: %+v", rep)
	}
	if !e.exists("exports", "new.zip") {
		events, _ := e.st.ListEvents(context.Background(), 200, 0)
		var seen []string
		for _, ev := range events {
			id := "<none>"
			if ev.ItemID != nil {
				id = string(*ev.ItemID)
			}
			seen = append(seen, ev.Action+"/"+ev.Phase+"/"+id)
		}
		t.Fatalf("a one-file approval was spent twice: the direct-deletion candidate was destroyed after the expiry clearance used the same approval; rep=%+v events=%v", rep, seen)
	}
	got, err := os.ReadFile(e.treePath("exports", "new.zip"))
	if err != nil || string(got) != "payload-new" {
		t.Fatalf("the surviving file was changed: %q %v", got, err)
	}
	if left := e.quarantinedItems(sc.ID); len(left) != 0 {
		t.Fatalf("%d object(s) are still due after a successful clearance", len(left))
	}
}

// TestServerReviewChangedCaptureCanBeRestored is S03.
//
// The file changes between the review and the capture. The program captures it
// anyway, notices the bytes are not the reviewed bytes, and stops - which is
// right. What was wrong is the record it left behind: the object sat in the
// vault without the identity that the restore path requires, so the product's
// own recovery instruction could not be followed, and the next reconcile had no
// pending row left to work on.
//
// The bytes were never lost. The way to get them back was.
func TestServerReviewChangedCaptureCanBeRestored(t *testing.T) {
	e, sc, planned := scheduledDeleteSetup(t)
	e.savePolicy(directDeletePolicy(sc.ID))
	e.exec.Review = agreeWithTheQuestion

	path := e.treePath("scratch", "discard.zip")
	const changed = "file changed while the deletion was being prepared"
	execute.TestHookPreRename = func() {
		execute.TestHookPreRename = nil
		if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
			t.Errorf("rewrite the file inside the capture window: %v", err)
		}
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	applied, err := e.apply(sc, planned.Plan, planned.Plan.Digest)
	if err == nil && applied.Failed == 0 {
		t.Fatalf("a deletion whose captured bytes differ from the reviewed bytes was reported as successful: %+v", applied)
	}
	if got := applied.Results[0].Code; got != string(errs.CodeHeld) {
		t.Fatalf("code = %s, want %s", got, errs.CodeHeld)
	}
	if e.exists("scratch", "discard.zip") {
		t.Fatal("the changed file was left at its path instead of being captured; the fixture did not reach the branch under test")
	}

	item, err := e.st.GetItem(context.Background(), planned.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !e.vaultObjectExists(sc, item) {
		t.Fatalf("the captured bytes are gone: %+v", item)
	}

	// The hold is written by reconciliation, so this is also the pass that has
	// to record what it found.
	if _, err := e.rec.Run(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	item, err = e.st.GetItem(context.Background(), planned.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != domain.StateHeld {
		t.Fatalf("state = %s, want the object held", item.State)
	}
	if item.CapturedIdentityJSON == nil || item.CapturedTokenJSON == nil {
		t.Fatalf("the held object has no post-move identity, so restore cannot find it: %+v", item)
	}

	// A second pass must not touch a case it already closed.
	second, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second.Examined != 0 {
		t.Fatalf("the second pass examined %d operation(s) that were already closed", second.Examined)
	}

	sctx := e.scopeCtx(sc.ID)
	if _, err := e.exec.Restore(context.Background(), sctx, item.ID, execute.RestoreOptions{}); err != nil {
		t.Fatalf("the captured changed file cannot be restored: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the restored file: %v", err)
	}
	if string(got) != changed {
		t.Fatalf("restored bytes = %q, want the bytes that were captured", got)
	}
}

// TestServerReviewUnchangedCaptureCanBeRestored is the positive control for S03.
//
// The same interruption, with the reviewed bytes intact. It passed before the
// fix and has to keep passing after it: the change is about what is recorded for
// an object that is *not* the reviewed one, not about the ordinary case.
func TestServerReviewUnchangedCaptureCanBeRestored(t *testing.T) {
	e, sc, planned := scheduledDeleteSetup(t)
	e.savePolicy(directDeletePolicy(sc.ID))
	e.exec.Review = agreeWithTheQuestion

	const stopped = "stop after the capture, before the unlink"
	reached := false
	execute.TestHookPreUnlink = func() {
		execute.TestHookPreUnlink = nil
		panic(stopped)
	}
	t.Cleanup(func() { execute.TestHookPreUnlink = nil })
	func() {
		defer func() {
			if v := recover(); v != nil {
				if v != stopped {
					panic(v)
				}
				reached = true
			}
		}()
		_, _ = e.apply(sc, planned.Plan, planned.Plan.Digest)
	}()
	if !reached {
		t.Fatal("the fixture did not reach the moment between capture and unlink")
	}

	if _, err := e.rec.Run(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	item, err := e.st.GetItem(context.Background(), planned.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	sctx := e.scopeCtx(sc.ID)
	if _, err := e.exec.Restore(context.Background(), sctx, item.ID, execute.RestoreOptions{}); err != nil {
		t.Fatalf("an interrupted capture of unchanged bytes was not restorable: %v", err)
	}
	got, err := os.ReadFile(e.treePath("scratch", "discard.zip"))
	if err != nil || string(got) != "discard-payload" {
		t.Fatalf("restored bytes = %q (%v), want the reviewed payload", got, err)
	}
}
