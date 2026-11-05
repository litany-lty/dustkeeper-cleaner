package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/report"
	"dustkeeper/internal/review"
	"dustkeeper/internal/run"
	"dustkeeper/internal/store"
)

// This file is the regression suite for the defects a review of commit 7cb849e
// found: the ones that were reproduced by running the program, not the ones read
// off the source.
//
// Every test here states a promise the product makes in one sentence, and fails
// when the program stops keeping it. They are deliberately about the action
// itself - which object is removed, whether an allowance was spent, which
// directory may be touched, what a user is told - because that is the level at
// which those defects were invisible.

// TestDirectDeleteNeverRemovesTheFileThatReplacedIt is the R01 reproduction.
//
// The reviewed file is renamed away and an unreviewed file takes its place
// inside the last window of the deletion. Before the fix the program unlinked
// the name, which now held the replacement: the reviewed file survived under a
// different name and a file nobody approved was destroyed. The fix captures the
// intended object into the vault first and deletes it there, so the name at the
// original path is never what gets unlinked.
func TestDirectDeleteNeverRemovesTheFileThatReplacedIt(t *testing.T) {
	e, sc, res := scheduledDeleteSetup(t)
	e.savePolicy(directDeletePolicy(sc.ID))
	e.exec.Review = agreeWithTheQuestion

	path := e.treePath("scratch", "discard.zip")
	movedTo := e.treePath("scratch", "reviewed.zip")
	const replacement = "this file was never reviewed, and its bytes must survive"
	execute.TestHookPreRename = func() {
		if err := os.Rename(path, movedTo); err != nil {
			t.Errorf("move the reviewed file away: %v", err)
			return
		}
		if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
			t.Errorf("write the replacement: %v", err)
		}
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	applied, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Failed != 1 {
		t.Fatalf("the batch did not report a failure: %+v", applied)
	}
	if got := applied.Results[0].Code; got != string(errs.CodeHeld) {
		t.Fatalf("code = %s, want %s", got, errs.CodeHeld)
	}

	// The file nobody approved is still there, byte for byte.
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the replacement was removed: %v", rerr)
	}
	if string(got) != replacement {
		t.Fatalf("the replacement was rewritten: %q", got)
	}
	// And the reviewed file is still readable under the name the test moved it to.
	reviewed, rerr := os.ReadFile(movedTo)
	if rerr != nil {
		t.Fatalf("the reviewed file was destroyed: %v", rerr)
	}
	if string(reviewed) != "discard-payload" {
		t.Fatalf("the reviewed file no longer holds its own bytes: %q", reviewed)
	}

	item, ierr := e.st.GetItem(context.Background(), res.Items[0].ID)
	if ierr != nil {
		t.Fatalf("get item: %v", ierr)
	}
	if item.State == domain.StatePurged {
		t.Fatal("a deletion was recorded although nothing approved was deleted")
	}
	// An intent was written, so a delete *event* exists; what may not exist is a
	// completion claiming the deletion happened.
	for _, ev := range e.events(t, res.Items[0].ID) {
		if ev.Phase == domain.PhaseComplete {
			t.Fatalf("a completion was written for a deletion that never happened: %+v", ev)
		}
	}
}

// TestPolicyCeilingBoundsEveryActionInOneRun is R02.
//
// The confirmed policy is the user's "how much may one run do" answer. It was
// read at the start of a run and then never applied to the actions themselves,
// so a policy of one could move ten files. The allowance is shared by every
// action class, which is what makes it a ceiling for the run rather than per
// class.
func TestPolicyCeilingBoundsEveryActionInOneRun(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)

	pol := cleanup.Default()
	pol.Scopes = []string{string(sc.ID)}
	pol.MaxFilesPerRun = 1
	pol.Notify = cleanup.NotifyNone
	e.savePolicy(pol)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.runOnce()
	if rep.Quarantined != 1 {
		t.Fatalf("the run moved %d file(s); the confirmed policy allows one: %+v", rep.Quarantined, rep)
	}
	left := 0
	for _, name := range []string{"a.zip", "b.zip"} {
		if e.exists("exports", name) {
			left++
		}
	}
	if left != 1 {
		t.Fatalf("%d file(s) still at their place, want exactly one", left)
	}
}

// TestScopeOutsideTheConfirmationTakesNoNewAction is R03.
//
// Removing a directory from the confirmed list is how a user withdraws
// permission. The scope keeps its rules, its grants and the objects it isolated
// before, and none of that is authority for one more move.
func TestScopeOutsideTheConfirmationTakesNoNewAction(t *testing.T) {
	e := newEnv(t)
	kept := e.addScope("exports", domain.KindManagedDisposable)
	other := e.addScope("elsewhere", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(kept.ID)

	// The confirmation names the other directory only.
	e.confirmPolicy(14, other.ID)

	r := e.rule(kept.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.runOnce()
	if rep.Quarantined != 0 {
		t.Fatalf("a scope outside the confirmation was cleaned: %+v", rep)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("a file was moved out of a directory the user no longer manages")
	}
	row, ok := scopeRow(rep, kept.ID)
	if !ok {
		t.Fatalf("the run did not report the scope it skipped: %+v", rep.Scopes)
	}
	if !strings.Contains(row.QuarantineSkipped, "not inside the confirmed cleanup scope") {
		t.Fatalf("the skip has no reason a user can act on: %+v", row)
	}
}

// scopeRow returns one scope's row from a run report.
func scopeRow(rep run.Report, id domain.ScopeID) (run.ScopeRun, bool) {
	for _, sr := range rep.Scopes {
		if sr.Scope == id {
			return sr, true
		}
	}
	return run.ScopeRun{}, false
}

// TestUnknownProcessFactDoesNotBecomeAPermissionToMove is R04.
//
// "No visible process holds this file open" is measured, not assumed. When the
// listing cannot be taken - no lsof, a timeout, a truncated answer - the fact is
// unknown, and unknown is not a licence to move the file somewhere else.
func TestUnknownProcessFactDoesNotBecomeAPermissionToMove(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)
	e.exec.Review = factGatedReview(domain.VerdictAllow)
	// The probe could not answer: this is the state an unattended machine with a
	// missing or hanging lsof is in.
	e.exec.OpenFiles = func(context.Context, string) (assess.OpenObservation, error) {
		return assess.OpenObservation{Coverage: "unknown", Paths: map[string]bool{}}, nil
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.runOnce()
	if rep.Quarantined != 0 {
		t.Fatalf("the file was moved although nothing could rule out that it is in use: %+v", rep)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file left its place on an unknown fact")
	}
	item := e.itemsForScanned(t, sc.ID, "a.zip")
	if item.State != domain.StatePlanned {
		t.Fatalf("state = %s, want the item left in its plan", item.State)
	}
}

// TestPurgeAndMoveAreCountedAsWhatTheyAre is R05.
//
// A permanent deletion is not a file in the observation area, and a message that
// counts them together tells the user a file can be recovered when it cannot.
// The date in the message is the object's own: the rule promised thirty days
// while the policy observes fourteen, and only one of those is true.
func TestPurgeAndMoveAreCountedAsWhatTheyAre(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "payload-old", 40*24*time.Hour)
	e.writeFile("exports", "fresh.zip", "payload-fresh", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)

	// The first object is isolated with a seven-day promise.
	old := e.rule(sc.ID, "rule-old", []string{"old.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(old, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(old, domain.CapAutoPurge, 10, 1<<30)
	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("fixture: expected one object observed, got %+v", rep)
	}

	// Just short of the fourteen days the policy promises every new object. The
	// second rule appears only now, so both events land in the same run: one
	// object clears and one is observed.
	e.advancePast(13 * 24 * time.Hour)
	fresh := e.rule(sc.ID, "rule-fresh", []string{"fresh.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(fresh, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.advancePast(30 * time.Hour)
	if rep.Purged != 1 || rep.Quarantined != 1 {
		t.Fatalf("purged=%d quarantined=%d, want one of each: %+v", rep.Purged, rep.Quarantined, rep)
	}
	messages := e.sender.messages()
	if len(messages) == 0 {
		t.Fatal("the run said nothing")
	}
	last := messages[len(messages)-1]
	if !strings.Contains(last, "本次删除 1 个文件") {
		t.Fatalf("the message does not count the deletion as a deletion: %q", last)
	}
	if !strings.Contains(last, "1 个文件进入待删除区") {
		t.Fatalf("the message does not count the move as a move: %q", last)
	}
	// The moved object's own promise, not the policy's fourteen days.
	if !strings.Contains(last, "30 天") {
		t.Fatalf("the message does not state when the new object clears: %q", last)
	}
}

// TestAKeepSuppressesTheSameQuestionUntilTheRetryWindowEnds is R09.
//
// Asking a model about a file it has already refused, on the next run, is a paid
// call whose answer cannot have changed: the bytes and the rule are the same
// ones the answer was about. KeepRetryDays is the configuration that says so, and
// it has to reach the code that asks.
func TestAKeepSuppressesTheSameQuestionUntilTheRetryWindowEnds(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "keep.zip", "payload-keep", 40*24*time.Hour)
	e.scan(sc.ID)

	pol := cleanup.Default()
	pol.Scopes = []string{string(sc.ID)}
	pol.KeepRetryDays = 7
	pol.Notify = cleanup.NotifyNone
	e.savePolicy(pol)

	asked := 0
	e.exec.Review = func(ctx context.Context, r review.Request) (review.Outcome, error) {
		asked++
		return keepReview("model_says_keep")(ctx, r)
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 0 {
		t.Fatalf("fixture: expected the file to be kept, got %+v", rep)
	}
	if asked != 1 {
		t.Fatalf("the first run asked %d time(s), want 1", asked)
	}

	// A day later the same file is still the same file.
	e.step(20 * time.Hour)
	if asked != 1 {
		t.Fatalf("the same file was sent to the models again inside the retry window (%d calls)", asked)
	}
	if !e.exists("exports", "keep.zip") {
		t.Fatal("the kept file was touched")
	}

	// Past the window, the question is asked again: a file that was kept once is
	// not kept for ever.
	e.advancePast(8 * 24 * time.Hour)
	if asked != 2 {
		t.Fatalf("the file was not reviewed again after the retry window (%d calls)", asked)
	}
}

// TestPurgeRechecksTheOriginalPathAfterTheReview is R11.
//
// The expiry pre-check is cheap and runs before the model call, so it can be a
// minute stale by the time the deletion happens. A file at the original path is
// the user working there again, and the object in the vault is then still
// wanted: the purge itself has to ask, not only the pre-flight.
func TestPurgeRechecksTheOriginalPathAfterTheReview(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "payload-old", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 10, 1<<30)
	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("fixture: expected one object observed, got %+v", rep)
	}
	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("fixture: %d object(s) in the observation area", len(items))
	}
	item := items[0]

	// The pre-check has already passed when the reviewer is called; the file
	// reappearing here is the window the review describes.
	path := e.treePath("exports", "old.zip")
	e.exec.Review = func(ctx context.Context, r review.Request) (review.Outcome, error) {
		if r.Action == domain.ActionPurge {
			if err := os.WriteFile(path, []byte("the user is working here again"), 0o644); err != nil {
				t.Errorf("recreate the original path: %v", err)
			}
		}
		return agreeWithTheQuestion(ctx, r)
	}

	// Past the fourteen days the policy promises, which is longer than the seven
	// days the rule asked for.
	rep := e.advancePast(15 * 24 * time.Hour)
	if rep.Purged != 0 {
		t.Fatalf("the object was cleared although its original place is in use again: %+v", rep)
	}

	// The vault object is still there, and the reason is on the record.
	if !e.vaultObjectExists(sc, item) {
		t.Fatal("the isolated object was removed")
	}
	got, err := e.st.GetItem(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.State != domain.StateQuarantined {
		t.Fatalf("state = %s, want the object still in the observation area", got.State)
	}
	row, ok := scopeRow(rep, sc.ID)
	if !ok {
		t.Fatalf("the run did not report the scope: %+v", rep.Scopes)
	}
	if !strings.Contains(row.Message, "original path") {
		t.Fatalf("the run does not say why nothing was cleared: %+v", row)
	}
}

// TestTheCommandOnThePageRestoresTheObjectItNames is R06, through the product's
// own path: run, read the page, copy the command, and use it.
//
// The page used to print the *operation* id where the restore command takes an
// *object* id. The command looked plausible and failed on use, which is the worst
// kind of instruction: the user has no way to tell a wrong id from a wrong
// object.
func TestTheCommandOnThePageRestoresTheObjectItNames(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	rep := e.runOnce()
	if rep.Quarantined != 1 {
		t.Fatalf("fixture: expected one object observed, got %+v", rep)
	}
	if e.exists("exports", "a.zip") {
		t.Fatal("fixture: the file was not moved")
	}

	page, err := report.Collect(context.Background(), e.st, report.Window{
		StartNS: e.clock.Wall().Add(-time.Hour).UnixNano(),
		EndNS:   e.clock.Wall().Add(time.Hour).UnixNano(),
		RunID:   rep.RunID,
	})
	if err != nil {
		t.Fatalf("collect the page: %v", err)
	}
	md := report.Render(page)
	object := objectIDOnPage(t, md)

	// The command the page tells the user to run, resolved the way the page means
	// it: by object id.
	sctx := e.scopeCtx(sc.ID)
	if _, err := e.exec.Restore(context.Background(), sctx, domain.ItemID(object), execute.RestoreOptions{}); err != nil {
		t.Fatalf("the command on the page does not work: %v", err)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the object named by the page was not restored")
	}
	got, err := os.ReadFile(e.treePath("exports", "a.zip"))
	if err != nil {
		t.Fatalf("read the restored file: %v", err)
	}
	if string(got) != "payload-a" {
		t.Fatalf("the wrong object came back: %q", got)
	}
}

// objectIDOnPage returns the id the restore column tells the user to pass.
func objectIDOnPage(t *testing.T, markdown string) string {
	t.Helper()
	const flag = "--object "
	for _, line := range strings.Split(markdown, "\n") {
		i := strings.Index(line, flag)
		if i < 0 {
			continue
		}
		rest := line[i+len(flag):]
		end := strings.IndexAny(rest, " \t`|,；;")
		if end < 0 {
			end = len(rest)
		}
		if id := strings.TrimSpace(rest[:end]); id != "" && id != "unknown" {
			return id
		}
	}
	t.Fatalf("the page carries no usable restore command:\n%s", markdown)
	return ""
}

// advancePast moves the clock forward the way a scheduled installation does:
// several normal runs rather than one large jump. A single jump of more than
// forty-eight hours is a suspect clock, so a test that made one would be
// measuring the clock gate instead of the behaviour it came for.
func (e *env) advancePast(total time.Duration) run.Report {
	e.t.Helper()
	const step = 40 * time.Hour
	var last run.Report
	for left := total; left > 0; {
		d := step
		if left < d {
			d = left
		}
		last = e.step(d)
		left -= d
	}
	return last
}

// itemsForScanned returns the one stored item for a path, and fails loudly when// the scan produced a different number of them.
func (e *env) itemsForScanned(t *testing.T, id domain.ScopeID, rel string) store.Item {
	t.Helper()
	items, err := e.st.ListItemsByState(context.Background(), id, domain.StatePlanned, 100)
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	var found []store.Item
	for _, it := range items {
		if string(it.RawRelative) == rel {
			found = append(found, it)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d item(s) for %q, want 1", len(found), rel)
	}
	return found[0]
}
