package integration

// Counter-examples for the reliability defects D02-D08 of the final review.
//
// Each test reproduces a state that the build under review accepted and asserts
// what it does now. They are deliberately narrow: one window, one injectable
// fault, one assertion about what did *not* happen. A test that says "the run
// reported success" is not evidence; "the file is still there and nothing new is
// pending" is.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// manifestFile is the path of one object's preparation manifest, built the way
// the product builds it.
func (e *env) manifestFile(scopeDir, objectID string) string {
	return filepath.Join(e.base, "trees", scopeDir, ".dustkeeper", "manifests", objectID+".json")
}

// pendingCount counts the operations that are still open.
func (e *env) pendingCount(t *testing.T) int {
	t.Helper()
	ops, err := e.st.PendingOps(context.Background())
	if err != nil {
		t.Fatalf("pending operations: %v", err)
	}
	return len(ops)
}

// D02, first half: the evidence used for the manifest and the contract is the
// evidence that was compared with the plan. A file touched between the
// comparison and the read is not that file any more.
//
// The touch is injected because the window is between two system calls inside
// one call; no test can hit it by timing. Before the fix the executor wrote a
// manifest and a contract describing a file it had never compared, and moved it.
func TestASourceTouchedInsideTheVerificationWindowIsRefused(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	srcPath := e.treePath("exports", "a.zip")
	touched := false
	execute.TestHookBeforeStableRead = func() {
		// A writer that only touches the file: same bytes, same inode, a new
		// modification time - and a file touched a second ago is no longer old
		// enough for the rule the plan approved.
		now := e.clock.Wall()
		if err := os.Chtimes(srcPath, now, now); err != nil {
			return
		}
		touched = true
	}
	t.Cleanup(func() { execute.TestHookBeforeStableRead = nil })

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if !touched {
		t.Fatalf("the hook never ran, so the window was not reproduced (%+v %v)", out, err)
	}
	if out.Code != string(errs.CodePlanStale) {
		t.Fatalf("a file touched inside the verification window was accepted: %+v (%v)", out, err)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the object was moved although it had changed before it was verified")
	}
	if got := e.pendingOf(t, res.Items[0].ID); got != "" {
		t.Fatalf("the refused item left an operation open: %q", got)
	}
	// The refusal happens before the manifest is written, so nothing durable
	// exists and the next scan starts from a clean tree.
	if _, serr := os.Stat(e.manifestFile("exports", res.Items[0].ObjectName)); !os.IsNotExist(serr) {
		t.Fatalf("the refused attempt left a preparation manifest: %v", serr)
	}
	item, gerr := e.st.GetItem(context.Background(), res.Items[0].ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StatePlanned {
		t.Fatalf("the refused item is %s, not planned", item.State)
	}
}

// D02, second half: the same evidence is checked once more immediately before the
// intent commits, and a failure there withdraws the manifest instead of leaving a
// preparation record that describes a move nobody made.
func TestASourceTouchedBeforeAcceptanceWithdrawsItsManifest(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	srcPath := e.treePath("exports", "a.zip")
	manifest := e.manifestFile("exports", res.Items[0].ObjectName)
	touched := false
	execute.TestHookBeforeAcceptance = func() {
		now := e.clock.Wall()
		if err := os.Chtimes(srcPath, now, now); err != nil {
			return
		}
		// The manifest for this attempt already exists at this point, which is
		// what makes the withdrawal the thing under test.
		if _, err := os.Stat(manifest); err == nil {
			touched = true
		}
	}
	t.Cleanup(func() { execute.TestHookBeforeAcceptance = nil })

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if !touched {
		t.Fatalf("the hook never saw the manifest, so the window was not reproduced (%+v %v)", out, err)
	}
	if out.Code != string(errs.CodePlanStale) {
		t.Fatalf("a file touched before acceptance was accepted: %+v (%v)", out, err)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the object was moved although it had changed before acceptance")
	}
	if _, serr := os.Stat(manifest); !os.IsNotExist(serr) {
		t.Fatalf("the refusal left its preparation manifest behind (%v), so a later attempt of the same path meets EEXIST", serr)
	}
	if got := e.pendingOf(t, res.Items[0].ID); got != "" {
		t.Fatalf("the refused item left an operation open: %q", got)
	}
	item, gerr := e.st.GetItem(context.Background(), res.Items[0].ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StatePlanned {
		t.Fatalf("a refused acceptance committed an intent: the item is %s", item.State)
	}
}

// D03, first part: a manifest that cannot be read is an environment problem, not
// a verdict. The operation stays open with its pending marker, and the pass after
// the fault clears finishes it.
func TestAManifestThatCannotBeReadParksTheOperation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, nil)
	e.closeScopeCtxs()

	manifest := e.manifestFile("exports", f.item.ObjectName)
	if err := os.Remove(manifest); err != nil {
		t.Fatalf("remove the manifest: %v", err)
	}
	// An entry that is not a readable record. Before the fix every manifest read
	// error became RECOVERY_REQUIRED, which is a terminal hold: a temporary fault
	// ended an operation that was making progress.
	if err := os.Symlink(filepath.Join(e.base, "gone.json"), manifest); err != nil {
		t.Fatalf("symlink the manifest: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Parked != 1 || rep.Held != 0 {
		t.Fatalf("an unreadable manifest was treated as a verdict: %+v", rep)
	}
	if got := e.pendingOf(t, f.item.ID); got == "" {
		t.Fatal("the parked operation lost its pending marker")
	}
	// A second pass with the fault still in place must park again, not decide.
	again, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if again.Parked != 1 || again.Held != 0 {
		t.Fatalf("the second pass did not park: %+v", again)
	}

	// The fault clears.
	if err := os.Remove(manifest); err != nil {
		t.Fatalf("clear the fault: %v", err)
	}
	body, merr := json.Marshal(f.manifest)
	if merr != nil {
		t.Fatalf("manifest json: %v", merr)
	}
	if err := os.WriteFile(manifest, body, 0o600); err != nil {
		t.Fatalf("restore the manifest: %v", err)
	}
	final, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if final.Completed != 1 {
		t.Fatalf("the operation did not finish once its manifest was readable: %+v", final)
	}
	if got := e.pendingOf(t, f.item.ID); got != "" {
		t.Fatal("the completed operation is still pending")
	}
}

// D03, second part: an object in the vault that cannot be opened is not a
// decision about the object. The capture is retried, and once it can be read the
// evidence is recorded.
func TestAnUnreadableVaultObjectParksTheCapture(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, nil)
	e.closeScopeCtxs()

	// The vault holds an object that is not the planned one: created elsewhere and
	// renamed in, so the inode is not the one the plan recorded.
	outside := filepath.Join(e.base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	wrong := filepath.Join(outside, "wrong.bin")
	if err := os.WriteFile(wrong, []byte("the wrong object entirely"), 0o644); err != nil {
		t.Fatalf("write the wrong object: %v", err)
	}
	if err := os.Remove(f.vaultRaw); err != nil {
		t.Fatalf("remove the staged object: %v", err)
	}
	if err := os.Rename(wrong, f.vaultRaw); err != nil {
		t.Fatalf("move the wrong object into the vault: %v", err)
	}
	// And right now it cannot be opened as a file at all: the entry is a link to
	// an object parked outside the tree.
	parked := filepath.Join(outside, "parked.bin")
	if err := os.Rename(f.vaultRaw, parked); err != nil {
		t.Fatalf("park the wrong object: %v", err)
	}
	if err := os.Symlink(parked, f.vaultRaw); err != nil {
		t.Fatalf("replace the object with a link: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Parked != 1 || rep.Held != 0 {
		t.Fatalf("an unreadable vault object was turned into a verdict: %+v", rep)
	}
	if got := e.pendingOf(t, f.item.ID); got == "" {
		t.Fatal("the parked capture lost its pending marker")
	}

	// The object becomes readable again: the evidence of what actually moved is
	// now recorded instead of the operation ending without it.
	if err := os.Remove(f.vaultRaw); err != nil {
		t.Fatalf("remove the link: %v", err)
	}
	if err := os.Rename(parked, f.vaultRaw); err != nil {
		t.Fatalf("put the wrong object back: %v", err)
	}
	final, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if final.Held != 1 {
		t.Fatalf("the second pass did not record the verdict: %+v", final)
	}
	got, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if got.CapturedIdentityJSON == nil {
		t.Fatal("the operation ended without recording the object that moved")
	}
	if captured := decodeIdentity(t, *got.CapturedIdentityJSON); !sameObject(captured, e.identityOfPath(t, f.vaultRaw)) {
		t.Fatalf("the recorded capture is not the object in the vault: %+v", captured)
	}
}

// D03, third part: a source whose parent cannot be opened is "not checked", not
// "present but strange". Before the fix the failure was compressed into
// found=true with a zero identity, which fell through to the branch that completes
// a quarantine - a completion decided without looking at the source at all.
func TestASourceThatCannotBeStattedIsNotTreatedAsPresent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantineAt(t, "sub/a.zip", nil)
	e.closeScopeCtxs()

	// The directory still opens - that only needs search permission on its
	// parent - but the entry inside it cannot be examined, because "sub" itself
	// lost its search bit. That is "this was not checked", not "the file is not
	// there"; the old code wrote it down as "present with no identity", which fell
	// through to the branch that completes a quarantine.
	sub := filepath.Join(e.base, "trees", "exports", "sub")
	if err := os.Chmod(sub, 0o600); err != nil {
		t.Fatalf("chmod the source directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Parked != 1 || rep.Completed != 0 {
		t.Fatalf("a source that could not be examined was accepted as evidence: %+v", rep)
	}
	if got := e.pendingOf(t, f.item.ID); got == "" {
		t.Fatal("the parked operation lost its pending marker")
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StateQIntent {
		t.Fatalf("the item left q_intent as %s without an observation", item.State)
	}
	if item.PurgeNotBeforeNS != nil {
		t.Fatal("a retention window was started from a check that never ran")
	}
}

// D04: the reconciliation report is a control input, not a log line. When the
// pass could not write down what it decided, the run stops before it starts any
// new work - including on scopes that are perfectly healthy.
func TestARunThatCannotRecordItsOutcomesStartsNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, nil)
	e.closeScopeCtxs()

	// A second scope with real work waiting for it. Nothing may reach it.
	healthy := e.addScope("downloads", domain.KindManagedDisposable)
	e.writeFile("downloads", "old.iso", "payload", 40*24*time.Hour)
	e.scan(healthy.ID)
	r := e.rule(healthy.ID, "rule-2", []string{"*.iso"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<20)

	// The object is in neither place, so the pass wants to record a verdict - and
	// the event log refuses new rows, so it cannot.
	if err := os.Remove(e.vaultPath("exports", f.item.ObjectName)); err != nil {
		t.Fatalf("remove the vault object: %v", err)
	}
	e.execRaw(t, `CREATE TRIGGER test_refuse_event BEFORE INSERT ON events
BEGIN SELECT RAISE(ABORT, 'injected'); END;`)

	before := e.pendingCount(t)
	rep, err := e.run.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Reconciliation.RecordFailures == 0 {
		t.Fatalf("the injected write failure was not reported: %+v", rep.Reconciliation)
	}
	if rep.Control != domain.ControlStopRun {
		t.Fatalf("control = %q, want %q", rep.Control, domain.ControlStopRun)
	}
	if rep.FailureCode != errs.CodeStateWriteFailed {
		t.Fatalf("failure code = %q, want %q", rep.FailureCode, errs.CodeStateWriteFailed)
	}
	if len(rep.Failures) == 0 {
		t.Fatal("the run stopped without saying why")
	}
	if rep.Quarantined != 0 || rep.Purged != 0 {
		t.Fatalf("a stopped run still moved %d/%d object(s)", rep.Quarantined, rep.Purged)
	}
	for _, sr := range rep.Scopes {
		if sr.Scanned != 0 || sr.Skipped == "" {
			t.Fatalf("scope %s was worked on although the pass could not record its outcomes: %+v", sr.Scope, sr)
		}
	}
	if after := e.pendingCount(t); after != before {
		t.Fatalf("the run began %d new operation(s) after a failed reconciliation", after-before)
	}
	if !e.exists("downloads", "old.iso") {
		t.Fatal("the healthy scope's object was moved by a run that should not have started")
	}
	if got := e.pendingOf(t, f.item.ID); got == "" {
		t.Fatal("the original pending operation was dropped")
	}
}

// D07, first part: a purge whose contract cannot be read must not be recorded as
// a completion. The vault object is gone, so nothing can be observed - which is
// exactly why the record has to be there.
func TestAPurgeWithABrokenContractIsNotRecordedAsComplete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndDue(t)

	removeVaultObject(t, e, f)
	stagePurgeIntent(t, e, f, "{not json")

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 {
		t.Fatalf("a purge with an unreadable contract was completed: %+v", rep)
	}
	if rep.Held != 1 || rep.RecordFailures != 0 {
		t.Fatalf("the pass did not end the operation cleanly: %+v", rep)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State == domain.StatePurged {
		t.Fatal("the item was recorded as purged on no evidence")
	}
	if got := e.pendingOf(t, f.item.ID); got != "" {
		t.Fatalf("a held purge is still pending: %q", got)
	}
}

// D07, second part: a contract that names another item is not this operation's
// record, so it cannot be used to decide this operation.
func TestAPurgeWithAContractBoundToAnotherItemIsHeld(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndDue(t)

	contract := purgeContract(t, e, f)
	contract.ItemID = string(domain.ItemID("item_someone_else"))
	stagePurgeIntent(t, e, f, marshalContract(t, contract))

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 || rep.Held != 1 {
		t.Fatalf("a contract for another item was used to decide this one: %+v", rep)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State == domain.StatePurged {
		t.Fatal("the item was recorded as purged from another operation's contract")
	}
}

// D07, third part: when the contract records no prior state, the item is not
// invented back to "quarantined". It stays visible as held, because the state it
// came from is the thing that decides whether it may ever be purged.
func TestAPurgeWithoutARecordedPriorStateIsNotGuessed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndDue(t)

	contract := purgeContract(t, e, f)
	contract.PriorState = ""
	stagePurgeIntent(t, e, f, marshalContract(t, contract))

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Held != 1 || rep.Aborted != 0 {
		t.Fatalf("a purge with no recorded prior state was converged anyway: %+v", rep)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State == domain.StateQuarantined {
		t.Fatal("the item was guessed back to quarantined; the contract did not say so")
	}
	if item.State != domain.StateHeld {
		t.Fatalf("the item is %s, want held", item.State)
	}
	if !strings.Contains(rep.Results[0].Detail, "records no state to return to") {
		t.Fatalf("the report does not name the reason: %+v", rep.Results[0])
	}
}

// D07, fourth part: the legitimate path still converges. A complete contract and
// an object that is still there returns the item to the state it came from.
func TestAPurgeThatDidNotHappenReturnsToItsRecordedState(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndDue(t)

	stagePurgeIntent(t, e, f, marshalContract(t, purgeContract(t, e, f)))

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Aborted != 1 || rep.Held != 0 {
		t.Fatalf("a purge that did not happen was not aborted: %+v", rep)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StateQuarantined {
		t.Fatalf("the item returned to %s, not the state its contract recorded", item.State)
	}
	if _, serr := os.Stat(f.vaultRaw); serr != nil {
		t.Fatalf("the object is no longer in the vault: %v", serr)
	}
}

// D07, fifth part: a restore that started from a hold and never happened does not
// come back as "quarantined" when its contract predates the prior-state field.
func TestARestoreWithoutARecordedPriorStateIsNotGuessed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, nil)

	// Finish the quarantine first, so the item carries real captured evidence and
	// the vault object is the recorded one.
	if rep, err := e.rec.Run(ctx); err != nil || rep.Completed != 1 {
		t.Fatalf("reconcile the quarantine: %+v %v", rep, err)
	}

	// The item is held - a verdict someone made about it - and an operator starts
	// a restore from there. The contract that restore writes is the legacy shape:
	// it records the destination and says nothing about where the object came
	// from.
	e.execRaw(t, `UPDATE items SET state='held' WHERE item_id=?`, string(f.item.ID))
	legacy := `{"raw_relative_b64":"` + plan.B64(f.item.RawRelative) + `"}`
	if err := e.st.BeginIntent(ctx, store.Intent{
		OpID:         domain.OperationID(domain.NewID("op")),
		ItemID:       f.item.ID,
		Action:       "restore",
		PriorState:   domain.StateHeld,
		NextState:    domain.StateRIntent,
		ContractJSON: legacy,
		OccurredAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("begin the restore intent: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile the restore: %v", err)
	}
	if rep.Aborted != 0 || rep.Held != 1 {
		t.Fatalf("a restore with no recorded prior state was converged: %+v", rep)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State == domain.StateQuarantined || item.State == domain.StateRestored {
		t.Fatalf("the restore was guessed into %s; an object that came from a hold must not become purge-eligible", item.State)
	}
	if item.PurgeNotBeforeNS == nil {
		t.Fatal("the test lost the retention window it was staged with")
	}
	if !strings.Contains(rep.Results[0].Detail, "records no state to return to") {
		t.Fatalf("the report does not name the reason: %+v", rep.Results[0])
	}
}

// D08: a manifest that was installed but could not be flushed is withdrawn again.
// Before the fix it stayed, and every later attempt of the same plan met EEXIST
// with no pending operation to explain it.
func TestAManifestThatCannotBeFlushedIsWithdrawn(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	manifest := e.manifestFile("exports", res.Items[0].ObjectName)
	execute.TestHookManifestDirSync = func() error {
		return errors.New("injected: the manifest directory could not be flushed")
	}
	t.Cleanup(func() { execute.TestHookManifestDirSync = nil })

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if out.Code != string(errs.CodeDurabilityFailed) {
		t.Fatalf("a manifest that could not be flushed was accepted: %+v (%v)", out, err)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the object was moved although its preparation record was not durable")
	}
	if _, serr := os.Stat(manifest); !os.IsNotExist(serr) {
		t.Fatalf("the installed manifest was left behind (%v), so the same plan can never be retried", serr)
	}
	if got := e.pendingOf(t, res.Items[0].ID); got != "" {
		t.Fatalf("the failed attempt left an operation open: %q", got)
	}

	// The fault is gone. The plan object itself is terminal once it has been
	// applied, so the retry is a fresh plan for the same file - which only works
	// because the failed attempt withdrew its manifest instead of leaving one in
	// the way for ever.
	again, perr := e.planForMayFail(sc, r, 100)
	if perr != nil {
		t.Fatalf("the same file could not be planned again after the fault cleared: %v", perr)
	}
	retried, aerr := e.apply(sc, again.Plan, again.Plan.Digest)
	if retried.Code != string(errs.CodeOK) {
		t.Fatalf("the retried plan did not go through: %+v (%v)", retried, aerr)
	}
	if e.exists("exports", "a.zip") {
		t.Fatal("the retried plan did not move the object")
	}
}

// D08, the other half: a manifest that was already there belongs to somebody
// else's record. A failed attempt must never delete it.
func TestAFailedAttemptNeverDeletesAnExistingManifest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	// An existing record for this object, written by an attempt whose operation is
	// already gone. It is not this attempt's to remove.
	sctx := e.scopeCtx(sc.ID)
	if err := sctx.WriteManifest(execute.ManifestV1{
		FormatVersion: 1, InstanceID: "inst_test", ScopeID: string(sc.ID),
		ObjectID: res.Items[0].ObjectName, RawRelativeB64: plan.B64(res.Items[0].RawRelative),
		CreatedAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("write the pre-existing manifest: %v", err)
	}
	manifest := e.manifestFile("exports", res.Items[0].ObjectName)
	before, rerr := os.ReadFile(manifest)
	if rerr != nil {
		t.Fatalf("read the pre-existing manifest: %v", rerr)
	}

	out, _ := e.apply(sc, res.Plan, res.Plan.Digest)
	if out.Code == string(errs.CodeOK) {
		t.Fatal("the attempt installed a manifest over an existing one")
	}
	after, rerr := os.ReadFile(manifest)
	if rerr != nil {
		t.Fatalf("the pre-existing manifest is gone: %v", rerr)
	}
	if string(after) != string(before) {
		t.Fatal("the pre-existing manifest was modified or replaced")
	}
	if e.pendingOf(t, res.Items[0].ID) != "" {
		t.Fatal("a failed install left an operation open")
	}
	_ = ctx
}

// D06: a compensation that succeeded puts the object back, protects its slot and
// closes the operation. The scope must then be able to work again after an
// explicit re-approval - the hold it was given was about that object, and that
// object is no longer an open question.
func TestACompensatedMiscaptureLetsTheScopeWorkAgain(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "the planned content", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	srcPath := e.treePath("exports", "a.zip")
	execute.TestHookPreRename = func() {
		tmp := srcPath + ".swap"
		if err := os.WriteFile(tmp, []byte("the object nobody planned for"), 0o644); err != nil {
			return
		}
		_ = os.Rename(tmp, srcPath)
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	out, _ := e.apply(sc, res.Plan, res.Plan.Digest)
	if out.Code != string(errs.CodeHeld) {
		t.Fatalf("the miscapture was not detected: %+v", out)
	}
	item, err := e.st.GetItem(ctx, res.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != domain.StateRestored {
		t.Fatalf("a compensated object settled as %s, not restored", item.State)
	}
	scopeRow, err := e.st.GetScope(ctx, sc.ID)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scopeRow.Status != domain.ScopeNeedsReapproval {
		t.Fatalf("the scope is %s; a hold here can never be resumed", scopeRow.Status)
	}
	// The verdict is gone: the item is settled, so no gate should still see work
	// to route around.
	if unresolved, uerr := e.st.ScopeHasUnresolved(ctx, sc.ID); uerr != nil {
		t.Fatalf("unresolved: %v", uerr)
	} else if unresolved {
		t.Fatal("a settled compensation still counts as unresolved work")
	}

	// Re-approval is the only thing standing between the scope and new work.
	if err := e.st.SetScopeStatus(ctx, sc.ID, domain.ScopeActive); err != nil {
		t.Fatalf("re-approve the scope: %v", err)
	}
	e.writeFile("exports", "b.zip", "legit rubbish", 40*24*time.Hour)
	e.scan(sc.ID)
	// The compensation bumped the protection revision, so the plan is built from
	// the scope row as it is now, not from the value captured by addScope.
	fresh, serr := e.st.GetScope(ctx, sc.ID)
	if serr != nil {
		t.Fatalf("re-read the scope: %v", serr)
	}
	next, perr := e.planForMayFail(fresh, r, 100)
	if perr != nil {
		t.Fatalf("the scope could not plan new work after a compensation: %v", perr)
	}
	if len(next.Items) != 1 {
		t.Fatalf("the new plan has %d member(s), want the one unprotected file", len(next.Items))
	}
	if string(next.Items[0].RawRelative) != "b.zip" {
		t.Fatalf("the new plan took %q", next.Items[0].RawRelative)
	}
	// The same scope gate the background pass uses, on the manual route: it reads
	// the item rows, so a settled compensation must not be enough to stop it.
	moved, aerr := e.apply(sc, next.Plan, next.Plan.Digest)
	if moved.Code != string(errs.CodeOK) {
		t.Fatalf("a healthy object in a compensated scope was refused: %+v (%v)", moved, aerr)
	}
	if e.exists("exports", "b.zip") {
		t.Fatal("the healthy object was not quarantined")
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the compensated object was moved by a later pass")
	}
	if data, rerr := os.ReadFile(srcPath); rerr != nil {
		t.Fatalf("the compensated object is gone: %v", rerr)
	} else if string(data) != "the object nobody planned for" {
		t.Fatalf("the compensated object has different content: %q", data)
	}

	// A genuinely unresolved item still blocks, so the gate was narrowed rather
	// than opened.
	e.execRaw(t, `UPDATE items SET state='held', pending_op_id=NULL WHERE item_id=?`, string(item.ID))
	if unresolved, uerr := e.st.ScopeHasUnresolved(ctx, sc.ID); uerr != nil {
		t.Fatalf("unresolved: %v", uerr)
	} else if !unresolved {
		t.Fatal("a held item no longer counts as unresolved")
	}
}

// quarantinedAndDue returns a fixture whose object sits in the vault, is due for
// purge, and has no operation open. It is the starting point for the purge rows
// of the reconciliation matrix.
func (e *env) quarantinedAndDue(t *testing.T) *crashFixture {
	t.Helper()
	ctx := context.Background()
	f := e.stageCrashedQuarantine(t, nil)
	if rep, err := e.rec.Run(ctx); err != nil || rep.Completed != 1 {
		t.Fatalf("complete the quarantine: %+v %v", rep, err)
	}
	// The retention window has elapsed, which is the only reason a purge is due.
	e.clock.Advance(31 * 24 * time.Hour)
	return f
}

// removeVaultObject takes the object out of the vault, the state a purge that did
// happen leaves behind.
func removeVaultObject(t *testing.T, e *env, f *crashFixture) {
	t.Helper()
	if err := os.Remove(f.vaultRaw); err != nil {
		t.Fatalf("remove the vault object: %v", err)
	}
}

// purgeContract builds the contract a purge of this object would write.
func purgeContract(t *testing.T, e *env, f *crashFixture) plan.OperationContractV1 {
	t.Helper()
	item, err := e.st.GetItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	var captured domain.FileIdentity
	if item.CapturedIdentityJSON == nil {
		t.Fatal("the quarantined item has no captured identity")
	}
	captured = decodeIdentity(t, *item.CapturedIdentityJSON)
	tok, terr := decodeToken(t, ptrStringOf(item.CapturedTokenJSON))
	if terr != nil {
		t.Fatalf("captured token: %v", terr)
	}
	return plan.OperationContractV1{
		BindingVersion: plan.BindingVersion,
		ItemID:         string(item.ID),
		PriorState:     domain.StateQuarantined,
		NextState:      domain.StatePIntent,
		Purpose:        plan.PurposeNormal,
		ScopeID:        string(item.ScopeID),
		RawRelativeB64: plan.B64(item.RawRelative),
		ObjectName:     item.ObjectName,
		ExpectedIdent:  captured,
		ExpectedToken:  tok,
		MaxFiles:       1,
	}
}

func marshalContract(t *testing.T, c plan.OperationContractV1) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("contract json: %v", err)
	}
	return string(b)
}

// stagePurgeIntent opens a purge operation without running it, which is the state
// a crash between the intent and the unlink leaves.
func stagePurgeIntent(t *testing.T, e *env, f *crashFixture, contract string) {
	t.Helper()
	if err := e.st.BeginIntent(context.Background(), store.Intent{
		OpID:         domain.OperationID(domain.NewID("op")),
		ItemID:       f.item.ID,
		Action:       "purge",
		PriorState:   domain.StateQuarantined,
		NextState:    domain.StatePIntent,
		ContractJSON: contract,
		OccurredAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("begin purge intent: %v", err)
	}
}

func ptrStringOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
