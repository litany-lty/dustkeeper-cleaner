package integration

// Tests for the two promises the restore path makes to a user:
//
//   - an object that was sent to a destination of the operator's choosing can
//     still be reconciled if the process stops before the move is recorded;
//   - an object the product keeps - including one it is holding - can be taken
//     out again through the command the product tells the user to run.
//
// Both were broken by the previous round: the new contract binding compared the
// destination with the item's own path, and the CLI still demanded "quarantined".
// These tests drive the real entry points and assert the filesystem, not the
// tool's opinion of itself.

import (
	"context"
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

// quarantinedAndSettled completes a crashed quarantine and returns the fixture,
// so the item is a normal quarantined object with captured evidence.
func (e *env) quarantinedAndSettled(t *testing.T) *crashFixture {
	t.Helper()
	f := e.stageCrashedQuarantine(t, nil)
	if rep, err := e.rec.Run(context.Background()); err != nil || rep.Completed != 1 {
		t.Fatalf("complete the quarantine: %+v %v", rep, err)
	}
	return f
}

// restoreContractFor builds the contract a restore of this item writes when it is
// sent to dest. The fields mirror internal/execute/restore.go, which is the point:
// reconciliation has to accept what the executor writes.
func (e *env) restoreContractFor(t *testing.T, f *crashFixture, action string, dest []byte,
	destParent domain.FileIdentity) plan.OperationContractV1 {

	t.Helper()
	it, err := e.st.GetItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if it.CapturedIdentityJSON == nil || it.CapturedTokenJSON == nil {
		t.Fatal("the quarantined item has no captured evidence")
	}
	captured := decodeIdentity(t, *it.CapturedIdentityJSON)
	tok, terr := decodeToken(t, *it.CapturedTokenJSON)
	if terr != nil {
		t.Fatalf("captured token: %v", terr)
	}
	var sourceParent domain.FileIdentity
	if it.SourceParentIdentityJSON != "" {
		sourceParent = decodeIdentity(t, it.SourceParentIdentityJSON)
	}
	purpose := plan.PurposeNormal
	if action == "compensate" {
		purpose = plan.PurposeMiscaptureFixup
	}
	return plan.OperationContractV1{
		BindingVersion:     plan.BindingVersion,
		ItemID:             string(it.ID),
		PriorState:         it.State,
		NextState:          domain.StateRIntent,
		Purpose:            purpose,
		ScopeID:            string(it.ScopeID),
		RawRelativeB64:     plan.B64(dest),
		ObjectName:         it.ObjectName,
		ParentIdentity:     sourceParent,
		DestParentIdentity: &destParent,
		ExpectedIdent:      captured,
		ExpectedToken:      tok,
		MaxFiles:           1,
		MaxLogicalBytes:    1 << 20,
	}
}

// openRestore starts the operation without moving anything, which is the state a
// crash between the intent and the rename leaves.
func (e *env) openRestore(t *testing.T, f *crashFixture, action string, c plan.OperationContractV1) {
	t.Helper()
	if err := e.st.BeginIntent(context.Background(), store.Intent{
		OpID:         domain.OperationID(domain.NewID("op")),
		ItemID:       f.item.ID,
		Action:       action,
		PriorState:   c.PriorState,
		NextState:    domain.StateRIntent,
		ContractJSON: marshalContract(t, c),
		OccurredAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("begin the %s intent: %v", action, err)
	}
}

// destinationDir creates a directory inside the scope and returns its identity,
// which is what the contract records as the approved destination.
func (e *env) destinationDir(t *testing.T, f *crashFixture, rel string) domain.FileIdentity {
	t.Helper()
	path := filepath.Join(e.base, "trees", "exports", rel)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	return e.parentIdentityOf(t, f.scope.ID, []byte(rel+"/placeholder"))
}

// C01, first half: a restore to another path that stopped before the rename is
// aborted, not judged. The item returns to the state the contract recorded, the
// object stays in the vault, and the destination stays empty.
func TestACustomDestinationRestoreThatNeverHappenedIsAborted(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndSettled(t)

	dest := []byte("recovered/a.zip")
	parentID := e.destinationDir(t, f, "recovered")
	c := e.restoreContractFor(t, f, "restore", dest, parentID)
	e.openRestore(t, f, "restore", c)

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Aborted != 1 || rep.Held != 0 {
		t.Fatalf("a restore to a chosen destination that never happened was not aborted: %+v", rep.Results)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StateQuarantined {
		t.Fatalf("the item returned to %s, not the state its contract recorded", item.State)
	}
	if item.PendingOpID != nil {
		t.Fatal("the aborted attempt left its pending marker behind")
	}
	if _, serr := os.Stat(f.vaultRaw); serr != nil {
		t.Fatalf("the object is no longer in the vault: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(e.base, "trees", "exports", "recovered", "a.zip")); !os.IsNotExist(serr) {
		t.Fatalf("something was placed at the destination: %v", serr)
	}
}

// C01, second half: the rename happened and the completion did not. The object is
// where the operator asked for it; reconciliation has to finish the record there
// instead of calling the contract a mismatch.
func TestACustomDestinationRestoreThatHappenedIsCompleted(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndSettled(t)

	dest := []byte("recovered/a.zip")
	parentID := e.destinationDir(t, f, "recovered")
	c := e.restoreContractFor(t, f, "restore", dest, parentID)
	e.openRestore(t, f, "restore", c)

	landed := filepath.Join(e.base, "trees", "exports", "recovered", "a.zip")
	if err := os.Rename(f.vaultRaw, landed); err != nil {
		t.Fatalf("move the object to the destination: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 1 || rep.Held != 0 {
		t.Fatalf("a completed move to a chosen destination was not completed: %+v", rep.Results)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StateRestored {
		t.Fatalf("the item is %s, want restored", item.State)
	}
	body, rerr := os.ReadFile(landed)
	if rerr != nil {
		t.Fatalf("read the restored file: %v", rerr)
	}
	if string(body) != "payload" {
		t.Fatalf("the restored file holds %q", body)
	}
	if _, serr := os.Stat(f.vaultRaw); !os.IsNotExist(serr) {
		t.Fatalf("the object is still in the vault: %v", serr)
	}
}

// Accepting the recorded destination must not turn into accepting any
// destination: the directory it names is compared with the one on disk.
func TestACustomDestinationRestoreRejectsAReplacedDirectory(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndSettled(t)

	dest := []byte("recovered/a.zip")
	parentID := e.destinationDir(t, f, "recovered")
	c := e.restoreContractFor(t, f, "restore", dest, parentID)
	e.openRestore(t, f, "restore", c)

	// The directory the operator approved is replaced by another one with the
	// same name before the record is finished. It is renamed aside rather than
	// removed and recreated: removing it frees the inode, and on a filesystem
	// that hands the same number back the replacement would be indistinguishable
	// from the original, which is the one thing this case must not depend on.
	dir := filepath.Join(e.base, "trees", "exports", "recovered")
	if err := os.Rename(dir, dir+".replaced"); err != nil {
		t.Fatalf("move the destination directory aside: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("recreate it: %v", err)
	}
	// The case only exists if the replacement is a different directory. Saying so
	// here keeps a filesystem that reuses inodes from turning into a mysterious
	// "accepted" result further down.
	if now := e.parentIdentityOf(t, f.scope.ID, []byte("recovered/placeholder")); now.Equal(parentID) {
		t.Fatalf("the replacement directory has the identity of the one it replaced (%+v); "+
			"this filesystem cannot express the case", now)
	}
	if err := os.Rename(f.vaultRaw, filepath.Join(dir, "a.zip")); err != nil {
		t.Fatalf("move the object: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 || rep.Held != 1 {
		t.Fatalf("a replaced destination directory was accepted: %+v", rep.Results)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.HoldCode == nil || *item.HoldCode != "DEST_PARENT_REPLACED" {
		t.Fatalf("the hold does not name the replaced directory: %+v", item.HoldCode)
	}
	if !strings.Contains(rep.Results[0].Detail, "not the one the operation recorded") {
		t.Fatalf("the report does not explain the hold: %+v", rep.Results[0])
	}
}

// C06: the quarantine branch reads its contract through the same binder as the
// other actions. A contract that is valid JSON but bound to another path must not
// be consumed - the old code parsed it, saw the source still present and wrote
// "aborted" for an operation it had not understood.
func TestAQuarantineContractBoundToAnotherPathIsNotConsumed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, func(f *crashFixture) {
		f.contract.RawRelativeB64 = plan.B64([]byte("a-different.zip"))
	})
	// The file never left the tree, so the old branch is "source present, vault
	// empty": a plain abort of somebody else's operation.
	if err := os.Rename(f.vaultRaw, f.srcPath); err != nil {
		t.Fatalf("put the file back: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Aborted != 0 || rep.Completed != 0 {
		t.Fatalf("a contract bound to another path was consumed: %+v", rep.Results)
	}
	if rep.Held != 1 {
		t.Fatalf("the mis-bound contract was not held: %+v", rep.Results)
	}
	if !strings.Contains(rep.Results[0].Detail, "INTENT_CONTRACT_UNREADABLE") {
		t.Fatalf("the report does not name the contract: %+v", rep.Results[0])
	}
	if !strings.Contains(rep.Results[0].Detail, "a-different.zip") {
		t.Fatalf("the report does not say which path the contract names: %+v", rep.Results[0])
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.HoldCode == nil || *item.HoldCode == "" {
		t.Fatal("the hold records no reason")
	}
}

// C06: the purge branch checks the prior state before it looks at whether the
// object is still there. "The object is gone" plus "the contract does not say
// what state it came from" is not a completion.
func TestAPurgeWithoutARecordedPriorStateIsNotCompletedWhenTheObjectIsGone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndDue(t)

	c := purgeContract(t, e, f)
	c.PriorState = ""
	stagePurgeIntent(t, e, f, marshalContract(t, c))
	removeVaultObject(t, e, f)

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 {
		t.Fatalf("a purge with no recorded prior state was completed because the object was gone: %+v", rep.Results)
	}
	if rep.Held != 1 || !strings.Contains(rep.Results[0].Detail, "PRIOR_STATE_UNKNOWN") {
		t.Fatalf("the report does not hold the record with a reason: %+v", rep.Results)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State == domain.StatePurged {
		t.Fatal("the item was recorded as purged from a contract that says nothing about its prior state")
	}
}

// The positive half of the same rule: a complete contract with the object gone is
// the case reconciliation exists to finish.
func TestAPurgeWithACompleteContractIsRecordedAsReconciledAbsent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndDue(t)

	stagePurgeIntent(t, e, f, marshalContract(t, purgeContract(t, e, f)))
	removeVaultObject(t, e, f)

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 1 {
		t.Fatalf("a complete purge contract was not completed: %+v", rep.Results)
	}
	if !strings.Contains(rep.Results[0].Detail, "RECONCILED_ABSENT") {
		t.Fatalf("the completion claims more than it observed: %+v", rep.Results[0])
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StatePurged {
		t.Fatalf("the item is %s, want purged", item.State)
	}
}

// C05: the compensation moved the object back and the process died before the
// record was closed. Finishing it has to settle the item *and* the scope the
// miscapture put on hold, or the file is back and the scope is still frozen.
func TestACompensationCompletedByReconcileAlsoReleasesTheScopeHold(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndSettled(t)

	e.execRaw(t, `UPDATE items SET state='held', hold_code='MISCAPTURE' WHERE item_id=?`, string(f.item.ID))
	if err := e.st.SetScopeStatus(ctx, f.scope.ID, domain.ScopeHeld); err != nil {
		t.Fatalf("hold the scope: %v", err)
	}

	dest := f.item.RawRelative
	c := e.restoreContractFor(t, f, "compensate", dest, e.parentIdentityOf(t, f.scope.ID, dest))
	e.openRestore(t, f, "compensate", c)
	if err := os.Rename(f.vaultRaw, f.srcPath); err != nil {
		t.Fatalf("put the object back: %v", err)
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 1 {
		t.Fatalf("the compensation was not completed: %+v", rep.Results)
	}
	item, gerr := e.st.GetItem(ctx, f.item.ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StateRestored {
		t.Fatalf("the item is %s, want restored", item.State)
	}
	got, serr := e.st.GetScope(ctx, f.scope.ID)
	if serr != nil {
		t.Fatalf("get scope: %v", serr)
	}
	if got.Status != domain.ScopeNeedsReapproval {
		t.Fatalf("the scope is %s: the object is back but the scope still refuses `scope resume`", got.Status)
	}
}

// The normal compensation path ends with the same two records settled. This is a
// guard on the path that already worked end to end - the previous build wrote both
// records, one after the other - so it is not the proof for C05; the failure
// window is (the reconcile test above, plus the single transaction the two writes
// now share).
func TestACompensatedMiscaptureSettlesItemAndScopeTogether(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "the planned content", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	srcPath := e.treePath("exports", "a.zip")
	execute.TestHookPreRename = func() {
		// A concurrent writer replaces the verified file in the window the last
		// identity check cannot close.
		tmp := srcPath + ".swap"
		if err := os.WriteFile(tmp, []byte("the object nobody planned for"), 0o644); err != nil {
			return
		}
		_ = os.Rename(tmp, srcPath)
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if out.Code != string(errs.CodeHeld) {
		t.Fatalf("the wrong capture was not refused: %+v %v", out, err)
	}
	item, gerr := e.st.GetItem(ctx, res.Items[0].ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	if item.State != domain.StateRestored {
		t.Fatalf("a compensated wrong capture left the item %s", item.State)
	}
	got, serr := e.st.GetScope(ctx, sc.ID)
	if serr != nil {
		t.Fatalf("get scope: %v", serr)
	}
	if got.Status != domain.ScopeNeedsReapproval {
		t.Fatalf("the item is restored but the scope is %s: the two records were not settled together", got.Status)
	}
}

// And a scope that still has another unresolved object is not handed back by one
// repair. The file that comes back is the only thing that changed.
func TestACompensationDoesNotReleaseAScopeWithAnotherObjectStillHeld(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndSettled(t)

	// A second object in the same scope, quarantined through the normal path.
	e.writeFile("exports", "b.zip", "second payload", 40*24*time.Hour)
	e.scan(f.scope.ID)
	r := e.rule(f.scope.ID, "rule-2", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	other := e.planFor(f.scope, r, 100)
	if len(other.Items) != 1 {
		t.Fatalf("the second plan holds %d items", len(other.Items))
	}
	if out, aerr := e.apply(f.scope, other.Plan, other.Plan.Digest); aerr != nil || out.Completed != 1 {
		t.Fatalf("quarantine the second object: %+v %v", out, aerr)
	}
	e.execRaw(t, `UPDATE items SET state='held', hold_code='UNEXPLAINED' WHERE item_id=?`,
		string(other.Items[0].ID))

	// The first object is held too, and its compensation had already moved it back.
	e.execRaw(t, `UPDATE items SET state='held', hold_code='MISCAPTURE' WHERE item_id=?`, string(f.item.ID))
	if err := e.st.SetScopeStatus(ctx, f.scope.ID, domain.ScopeHeld); err != nil {
		t.Fatalf("hold the scope: %v", err)
	}
	dest := f.item.RawRelative
	c := e.restoreContractFor(t, f, "compensate", dest, e.parentIdentityOf(t, f.scope.ID, dest))
	e.openRestore(t, f, "compensate", c)
	if err := os.Rename(f.vaultRaw, f.srcPath); err != nil {
		t.Fatalf("put the object back: %v", err)
	}

	if rep, rerr := e.rec.Run(ctx); rerr != nil || rep.Completed != 1 {
		t.Fatalf("reconcile: %+v %v", rep, rerr)
	}
	got, serr := e.st.GetScope(ctx, f.scope.ID)
	if serr != nil {
		t.Fatalf("get scope: %v", serr)
	}
	if got.Status != domain.ScopeHeld {
		t.Fatalf("the scope is %s; another object in it is still held, so the hold has not been explained", got.Status)
	}
}

// The normal restore entry point settles the scope too: taking a held object out
// of the vault is the repair the hold was waiting for.
func TestRestoringAHeldObjectReleasesTheScopeHold(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.quarantinedAndSettled(t)

	e.execRaw(t, `UPDATE items SET state='held', hold_code='MISCAPTURE' WHERE item_id=?`, string(f.item.ID))
	if err := e.st.SetScopeStatus(ctx, f.scope.ID, domain.ScopeHeld); err != nil {
		t.Fatalf("hold the scope: %v", err)
	}

	sctx := e.scopeCtx(f.scope.ID)
	res, rerr := e.exec.Restore(ctx, sctx, f.item.ID, execute.RestoreOptions{})
	if rerr != nil {
		t.Fatalf("restore a held object: %v (%+v)", rerr, res)
	}
	got, serr := e.st.GetScope(ctx, f.scope.ID)
	if serr != nil {
		t.Fatalf("get scope: %v", serr)
	}
	if got.Status != domain.ScopeNeedsReapproval {
		t.Fatalf("the scope is %s after its held object was restored", got.Status)
	}
}
