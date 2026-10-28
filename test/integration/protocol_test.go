package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// These tests are about the second recovery, not the first. Every scenario here
// starts from a state where the filesystem already changed and the record did
// not, then asks the same three questions the review asks: is the item still
// findable, does a retry succeed, and is there an exit for the states that can no
// longer be retried.

// quarantinedItem produces a genuinely quarantined object: staged crash, then
// completed through reconciliation, so the captured evidence is the real thing.
func (e *env) quarantinedItem(t *testing.T) (store.Scope, store.Item) {
	t.Helper()
	f := e.stageCrashedQuarantine(t, nil)
	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("complete the crashed quarantine: %v", err)
	}
	if rep.Completed != 1 {
		t.Fatalf("the staged crash was not completed: %+v", rep)
	}
	got, err := e.st.GetItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.State != domain.StateQuarantined {
		t.Fatalf("the item is %s, not quarantined", got.State)
	}
	// The scope contexts this environment opened hold the in-tree lock. Releasing
	// them is what makes the lock contention scenarios below possible.
	e.closeScopeCtxs()
	return f.scope, got
}

// holdScopeLock takes the in-tree scope lock from outside the product, the way a
// second installation would. The reconciler must treat this as "not observable
// right now", never as an observation about the object.
func (e *env) holdScopeLock(t *testing.T, scopeDir string) func() {
	t.Helper()
	path := filepath.Join(e.base, "trees", scopeDir, domain.VaultDirName, domain.VaultScopeLock)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open scope lock: %v", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("take scope lock: %v", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}
}

// pendingOf returns the item's pending operation, or "" when it has none.
func (e *env) pendingOf(t *testing.T, id domain.ItemID) string {
	t.Helper()
	got, err := e.st.GetItem(context.Background(), id)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.PendingOpID == nil {
		return ""
	}
	return string(*got.PendingOpID)
}

// vaultPath returns the path of one object inside a vault.
func (e *env) vaultPath(scopeDir, object string) string {
	return filepath.Join(e.base, "trees", scopeDir, domain.VaultDirName, "objects", object)
}

// treePath returns the path of one file inside a scope tree.
func (e *env) treePath(scopeDir, rel string) string {
	return filepath.Join(e.base, "trees", scopeDir, rel)
}

// TestReconcileRetriesEveryActionUntilTheScopeIsFree covers the regression the
// review names: a scope that could not be locked was written down as an
// unobservable object, which cleared its pending marker and made the second pass
// find nothing. "Somebody else is working" is a reason to come back later.
func TestReconcileRetriesEveryActionUntilTheScopeIsFree(t *testing.T) {
	ctx := context.Background()

	t.Run("quarantine", func(t *testing.T) {
		e := newEnv(t)
		f := e.stageCrashedQuarantine(t, nil)
		e.closeScopeCtxs()
		opID := e.pendingOf(t, f.item.ID)
		if opID == "" {
			t.Fatal("the staged crash has no pending operation")
		}

		release := e.holdScopeLock(t, "exports")
		// Two consecutive rounds, because "the marker survives one failure" is
		// not the property that matters: a scheduled installation meets the same
		// blocked scope every morning until somebody fixes it.
		for round := 1; round <= 2; round++ {
			rep, err := e.rec.Run(ctx)
			if err != nil {
				t.Fatalf("reconcile while busy (round %d): %v", round, err)
			}
			if rep.Busy != 1 || rep.Completed != 0 || rep.Held != 0 {
				t.Fatalf("round %d: a busy scope was not reported as busy: %+v", round, rep)
			}
			if got := e.pendingOf(t, f.item.ID); got != opID {
				t.Fatalf("round %d: the pending operation was dropped while the scope was busy: %q -> %q",
					round, opID, got)
			}
			if it, _ := e.st.GetItem(ctx, f.item.ID); it.State != domain.StateQIntent {
				t.Fatalf("round %d: a busy scope changed the item state to %s", round, it.State)
			}
		}

		release()
		rep, err := e.rec.Run(ctx)
		if err != nil {
			t.Fatalf("second reconcile: %v", err)
		}
		if rep.Completed != 1 {
			t.Fatalf("the second pass did not finish the operation: %+v", rep)
		}
		if got := e.pendingOf(t, f.item.ID); got != "" {
			t.Fatalf("the completed item still has a pending operation: %q", got)
		}
	})

	t.Run("restore", func(t *testing.T) {
		e := newEnv(t)
		sc, it := e.quarantinedItem(t)

		// The restore moved the object and then the process died.
		captured := decodeIdentity(t, *it.CapturedIdentityJSON)
		destParent := e.parentIdentityOf(t, sc.ID, []byte("a.zip"))
		e.closeScopeCtxs()
		contract := plan.OperationContractV1{
			BindingVersion:     plan.BindingVersion,
			ItemID:             string(it.ID),
			PriorState:         domain.StateQuarantined,
			NextState:          domain.StateRIntent,
			Purpose:            plan.PurposeNormal,
			ScopeID:            string(sc.ID),
			RawRelativeB64:     plan.B64(it.RawRelative),
			ObjectName:         it.ObjectName,
			DestParentIdentity: &destParent,
			ExpectedIdent:      captured,
			MaxFiles:           1,
			MaxLogicalBytes:    1 << 20,
		}
		body, _ := json.Marshal(contract)
		opID := domain.OperationID(domain.NewID("op"))
		if err := e.st.BeginIntent(ctx, store.Intent{
			OpID: opID, ItemID: it.ID, Action: "restore",
			PriorState: domain.StateQuarantined, NextState: domain.StateRIntent,
			ContractJSON: string(body), OccurredAtNS: e.clock.Wall().UnixNano(),
		}); err != nil {
			t.Fatalf("begin restore intent: %v", err)
		}
		if err := os.Rename(e.vaultPath("exports", it.ObjectName), e.treePath("exports", "a.zip")); err != nil {
			t.Fatalf("move the object back: %v", err)
		}

		release := e.holdScopeLock(t, "exports")
		// Two rounds for the same reason as the quarantine case: a scheduled
		// installation meets the same blocked scope every morning until somebody
		// fixes it, so the pending marker has to survive more than one attempt.
		for round := 1; round <= 2; round++ {
			rep, err := e.rec.Run(ctx)
			if err != nil {
				t.Fatalf("reconcile while busy (round %d): %v", round, err)
			}
			if rep.Busy != 1 {
				t.Fatalf("round %d: a busy scope was not reported as busy: %+v", round, rep)
			}
			if got := e.pendingOf(t, it.ID); got != string(opID) {
				t.Fatalf("round %d: the pending restore was dropped while the scope was busy: %q", round, got)
			}
			if got, _ := e.st.GetItem(ctx, it.ID); got.State != domain.StateRIntent {
				t.Fatalf("round %d: a busy scope changed the item state to %s", round, got.State)
			}
		}

		release()
		rep, err := e.rec.Run(ctx)
		if err != nil {
			t.Fatalf("second reconcile: %v", err)
		}
		if rep.Completed != 1 {
			t.Fatalf("the second pass did not finish the restore: %+v", rep)
		}
		got, _ := e.st.GetItem(ctx, it.ID)
		if got.State != domain.StateRestored {
			t.Fatalf("the item is %s, not restored", got.State)
		}
	})

	t.Run("purge", func(t *testing.T) {
		e := newEnv(t)
		_, it := e.quarantinedItem(t)
		e.clock.Advance(40 * 24 * time.Hour)

		opID := domain.OperationID(domain.NewID("op"))
		body, _ := json.Marshal(plan.OperationContractV1{
			BindingVersion: plan.BindingVersion,
			ItemID:         string(it.ID),
			PriorState:     domain.StateQuarantined,
			NextState:      domain.StatePIntent,
			Purpose:        plan.PurposeNormal,
			ScopeID:        string(it.ScopeID),
			RawRelativeB64: plan.B64(it.RawRelative),
			ObjectName:     it.ObjectName,
			ExpectedIdent:  decodeIdentity(t, *it.CapturedIdentityJSON),
		})
		if err := e.st.BeginIntent(ctx, store.Intent{
			OpID: opID, ItemID: it.ID, Action: "purge",
			PriorState: domain.StateQuarantined, NextState: domain.StatePIntent,
			ContractJSON: string(body), OccurredAtNS: e.clock.Wall().UnixNano(),
		}); err != nil {
			t.Fatalf("begin purge intent: %v", err)
		}
		if err := os.Remove(e.vaultPath("exports", it.ObjectName)); err != nil {
			t.Fatalf("remove the object: %v", err)
		}

		release := e.holdScopeLock(t, "exports")
		for round := 1; round <= 2; round++ {
			rep, err := e.rec.Run(ctx)
			if err != nil {
				t.Fatalf("reconcile while busy (round %d): %v", round, err)
			}
			if rep.Busy != 1 {
				t.Fatalf("round %d: a busy scope was not reported as busy: %+v", round, rep)
			}
			if got := e.pendingOf(t, it.ID); got != string(opID) {
				t.Fatalf("round %d: the pending purge was dropped while the scope was busy: %q", round, got)
			}
			if got, _ := e.st.GetItem(ctx, it.ID); got.State != domain.StatePIntent {
				t.Fatalf("round %d: a busy scope changed the item state to %s", round, got.State)
			}
		}

		release()
		rep, err := e.rec.Run(ctx)
		if err != nil {
			t.Fatalf("second reconcile: %v", err)
		}
		if rep.Completed != 1 {
			t.Fatalf("the second pass did not finish the purge: %+v", rep)
		}
		got, _ := e.st.GetItem(ctx, it.ID)
		if got.State != domain.StatePurged {
			t.Fatalf("the item is %s, not purged", got.State)
		}
	})
}

// identityOfPath reads the device and inode of one path. A test can only compare
// these two fields against a recorded identity: the rest of the identity (birth
// time, generation) is established by the product from a descriptor, and a test
// that invented those values would be comparing its own fiction.
func (e *env) identityOfPath(t *testing.T, path string) domain.FileIdentity {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return domain.FileIdentity{
		Device: uint64(st.Dev),
		Inode:  uint64(st.Ino),
	}
}

// sameObject compares the two fields a test can establish independently.
func sameObject(a, b domain.FileIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode
}

// parentIdentityOf asks the product for a directory's identity, which is the only
// way to obtain a value comparable with what the product records.
func (e *env) parentIdentityOf(t *testing.T, id domain.ScopeID, rel []byte) domain.FileIdentity {
	t.Helper()
	sctx := e.scopeCtx(id)
	parent, err := sctx.Root.OpenParent(rel)
	if err != nil {
		t.Fatalf("open parent of %q: %v", rel, err)
	}
	defer parent.Close()
	identity, _, err := parent.Stat()
	if err != nil {
		t.Fatalf("stat parent of %q: %v", rel, err)
	}
	return identity
}

// stageLegacyHold reproduces what an older build left behind: a terminal hold
// with the pending marker cleared, and (optionally) the evidence of the object
// that actually moved.
func (e *env) stageLegacyHold(t *testing.T, withCapture bool) *crashFixture {
	t.Helper()
	ctx := context.Background()
	f := e.stageCrashedQuarantine(t, nil)

	pending, err := e.st.PendingOps(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending operations: %v %v", pending, err)
	}
	in := intentOfForTest(pending[0])
	if !withCapture {
		if err := e.st.HoldIntent(ctx, in, "UNEXPECTED_VAULT_OBJECT",
			`{"source":"reconciled","result":"old build: vault object is not the recorded object"}`); err != nil {
			t.Fatalf("hold: %v", err)
		}
		return f
	}

	// The evidence an older build would have written is the evidence the product
	// produces: identity and token read from the descriptor, not from a path.
	identity, token := e.vaultObject(t, f.sctx, f.item.ObjectName)
	idJSON, _ := json.Marshal(identity)
	tokJSON, _ := json.Marshal(token)
	payload := `{"hold_code":"MISCAPTURE","source":"observed","captured_identity":` +
		string(mustJSON(t, string(idJSON))) + `}`
	if err := e.st.HoldIntentWithCapture(ctx, in, "MISCAPTURE", payload,
		string(idJSON), string(tokJSON)); err != nil {
		t.Fatalf("hold with capture: %v", err)
	}
	return f
}

// vaultObject reads one vault object's identity and token through the product's
// own descriptor path, so a fixture writes exactly the evidence the product would
// have written.
func (e *env) vaultObject(t *testing.T, sctx *execute.ScopeCtx, object string) (domain.FileIdentity, domain.ChangeToken) {
	t.Helper()
	parent, err := sctx.Objects.ParentOf(object)
	if err != nil {
		t.Fatalf("open objects directory: %v", err)
	}
	defer parent.Close()
	obj, err := fsops.OpenRegularAt(parent, sctx.RootDev)
	if err != nil {
		t.Fatalf("open vault object %s: %v", object, err)
	}
	defer obj.Close()
	return obj.Identity, obj.Token
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	return b
}

func intentOfForTest(po store.PendingOp) store.Intent {
	return store.Intent{
		OpID:         po.OpID,
		ItemID:       po.Item.ID,
		Action:       po.Action,
		ContractJSON: po.ContractJSON,
		OccurredAtNS: po.IntentAtNS,
	}
}

func hasProtection(list []store.Protection, rel string) bool {
	for _, p := range list {
		if string(p.RawRelative) == rel {
			return true
		}
	}
	return false
}

// TestCrashDiscoveredMiscaptureRecordsTheActualObject covers the first row of the
// wrong-capture table: the process died after the wrong object was moved and
// before its evidence was written. Reconciliation must record what is actually
// there, otherwise the file is preserved but unreachable.
func TestCrashDiscoveredMiscaptureRecordsTheActualObject(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	f := e.stageCrashedQuarantine(t, nil)

	// The vault holds an object that is not the planned one. It is created in
	// another directory and then renamed into place: on a filesystem that reuses
	// a freed inode immediately, writing over the old name can produce an entry
	// that is indistinguishable from the original, which is a property of the
	// fixture rather than of the product.
	outside := filepath.Join(e.base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	wrong := filepath.Join(outside, "wrong.bin")
	if err := os.WriteFile(wrong, []byte("the wrong object entirely"), 0o644); err != nil {
		t.Fatalf("write the wrong object: %v", err)
	}
	if err := os.Remove(f.vaultRaw); err != nil {
		t.Fatalf("remove staged object: %v", err)
	}
	if err := os.Rename(wrong, f.vaultRaw); err != nil {
		t.Fatalf("move the wrong object into the vault: %v", err)
	}
	actual := e.identityOfPath(t, f.vaultRaw)

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Held != 1 {
		t.Fatalf("a wrong object in the vault was not held: %+v", rep)
	}
	got, err := e.st.GetItem(ctx, f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.CapturedIdentityJSON == nil {
		t.Fatal("the wrong object was held without recording what it is")
	}
	if captured := decodeIdentity(t, *got.CapturedIdentityJSON); !sameObject(captured, actual) {
		t.Fatalf("the recorded capture is not the object in the vault: %+v vs %+v", captured, actual)
	}

	// The original path is taken by a new file, so the only safe restore is to an
	// explicit empty destination. This is also the exit the simplified build keeps
	// for a held object: adoption is gone, an explicit restore is not.
	if err := os.WriteFile(f.srcPath, []byte("a new file at the old path"), 0o644); err != nil {
		t.Fatalf("occupy the original path: %v", err)
	}
	sc := e.scopeCtx(f.item.ScopeID)
	e.exec.Guard = e.guard(sc)
	if _, err := e.exec.Restore(ctx, sc, f.item.ID, execute.RestoreOptions{}); errs.CodeOf(err) != errs.CodeRestoreConflict {
		t.Fatalf("restoring onto an occupied path returned %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(e.treePath("exports", "recovered/wrong-object.bin")), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := e.exec.Restore(ctx, sc, f.item.ID,
		execute.RestoreOptions{DestinationRelative: []byte("recovered/wrong-object.bin")}); err != nil {
		t.Fatalf("restore the held object to an explicit destination: %v", err)
	}
	if data, err := os.ReadFile(e.treePath("exports", "recovered/wrong-object.bin")); err != nil {
		t.Fatalf("the recovered object is not there: %v", err)
	} else if string(data) != "the wrong object entirely" {
		t.Fatalf("the recovered object has different content: %q", data)
	}
	if data, err := os.ReadFile(f.srcPath); err != nil {
		t.Fatalf("the new file at the original path is gone: %v", err)
	} else if string(data) != "a new file at the old path" {
		t.Fatalf("the new file at the original path was overwritten: %q", data)
	}
}

// TestWrongCaptureIsPutBackWithProtections drives the executor into the
// wrong-capture branch and checks the compensation reuses the restore protocol:
// the object is put back and the path is protected in the same transaction as the
// intent, so the next scan cannot quarantine it again.
func TestWrongCaptureIsPutBackWithProtections(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "the planned content", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)

	srcPath := e.treePath("exports", "a.zip")
	swapped := false
	execute.TestHookPreRename = func() {
		// A concurrent writer replaces the verified file with another one in the
		// window the last identity check cannot close.
		tmp := srcPath + ".swap"
		if err := os.WriteFile(tmp, []byte("the object nobody planned for"), 0o644); err != nil {
			return
		}
		if err := os.Rename(tmp, srcPath); err != nil {
			return
		}
		swapped = true
	}
	t.Cleanup(func() { execute.TestHookPreRename = nil })

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if !swapped {
		t.Fatalf("the swap hook did not run; the scenario was never reproduced (%+v %v)", out, err)
	}
	if out.Code != string(errs.CodeHeld) {
		t.Fatalf("the wrong capture was not refused: %+v %v", out, err)
	}

	got, gerr := e.st.GetItem(ctx, res.Items[0].ID)
	if gerr != nil {
		t.Fatalf("get item: %v", gerr)
	}
	// The compensation is a convergence, not a verdict: the object is back, the
	// slot is protected, nothing is pending, so the item is restored. Leaving it
	// held made it permanent - reconciliation only looks at pending rows, restore
	// has no vault object left to work with, and the scope gate read the
	// historical hold as unfinished work for ever.
	if got.State != domain.StateRestored {
		t.Fatalf("a compensated wrong capture left the item %s", got.State)
	}
	// The compensation ran, so the wrong object is back where it came from.
	if data, rerr := os.ReadFile(srcPath); rerr != nil {
		t.Fatalf("the wrong object was not put back: %v", rerr)
	} else if string(data) != "the object nobody planned for" {
		t.Fatalf("the object at the original path is %q", data)
	}
	// And the compensation is protected in the record, not only on disk.
	protections, perr := e.st.ListProtections(ctx, sc.ID, true)
	if perr != nil {
		t.Fatalf("protections: %v", perr)
	}
	if !hasProtection(protections, string(got.RawRelative)) {
		t.Fatalf("a compensated path is unprotected: %+v", protections)
	}

	// Clearing the scope hold and scanning again must not produce a candidate.
	if err := e.st.SetScopeStatus(ctx, sc.ID, domain.ScopeActive); err != nil {
		t.Fatalf("clear scope status: %v", err)
	}
	e.scan(sc.ID)
	if _, err := e.planForMayFail(sc, r, 100); errs.CodeOf(err) != errs.CodeNoCandidates {
		t.Fatalf("a compensated file was planned again: %v", err)
	}
}

// TestCompensationCrashReturnsToTheHold covers the third row of the table: the
// compensation committed its intent, then the process died before the rename. The
// observed state is "the vault still holds it, the destination is empty", and the
// item must go back to held - not to quarantined, which would start it towards
// purge eligibility.
func TestCompensationCrashReturnsToTheHold(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

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
	var crashed bool
	execute.TestHookPreCompensateRename = func() {
		// The compensation intent is durable and the object has not moved yet.
		crashed = true
		panic("simulated crash between the compensation intent and its rename")
	}
	t.Cleanup(func() {
		execute.TestHookPreRename = nil
		execute.TestHookPreCompensateRename = nil
	})

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the crash hook did not fire")
			}
		}()
		_, _ = e.apply(sc, res.Plan, res.Plan.Digest)
	}()
	if !crashed {
		t.Fatal("the compensation never started")
	}

	itemID := res.Items[0].ID
	opID := e.pendingOf(t, itemID)
	if opID == "" {
		t.Fatal("the crashed compensation left no pending operation")
	}

	rep, err := e.rec.Run(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Aborted != 1 {
		t.Fatalf("the crashed compensation was not aborted: %+v", rep)
	}
	got, err := e.st.GetItem(ctx, itemID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.State != domain.StateHeld {
		t.Fatalf("an aborted compensation returned the item to %s, not held", got.State)
	}
	if got.PendingOpID != nil {
		t.Fatal("the aborted compensation left the operation open")
	}
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Fatal("the object was moved even though the rename never ran")
	}
}
