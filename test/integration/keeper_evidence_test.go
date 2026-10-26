package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// duplicateFixture is one isolated duplicate plus a reachable keeper, with the
// retention window already elapsed so that a purge is attempted.
type duplicateFixture struct {
	scope     store.Scope
	item      store.Item
	keeperRel string
	keeperRaw string
}

// stageDuplicateDetectsAndIsolates runs the real duplicate pipeline end to end.
func (e *env) stageDuplicateDetectsAndIsolates(t *testing.T) duplicateFixture {
	t.Helper()
	sc := e.addScope("dups", domain.KindAssisted)
	e.writeFile("dups", "keeper.bin", "identical-payload", 40*24*time.Hour)
	e.writeFile("dups", "copy.bin", "identical-payload", 40*24*time.Hour)
	e.scan(sc.ID)

	res, err := e.det.Detect(context.Background(), e.scopeCtx(sc.ID), nil)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		t.Fatalf("expected one plannable group, got %d (%v)", res.PlannableGroups, res.Notes)
	}
	if res.Groups[0].Keeper.MetaHash == "" {
		t.Fatal("the plan records no keeper metadata hash, so equivalence can never be re-proven")
	}
	p, items, err := e.det.CreatePlan(context.Background(), e.scopeCtx(sc.ID), res.Groups)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := e.apply(sc, p, p.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one isolated copy, got %d", len(items))
	}
	it, err := e.st.GetItem(context.Background(), items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if it.State != domain.StateQuarantined {
		t.Fatalf("the copy is %s, not quarantined", it.State)
	}
	if it.KeeperJSON == nil {
		t.Fatal("the quarantined duplicate has no keeper evidence")
	}

	keeperRel := "keeper.bin"
	if !e.exists("dups", keeperRel) {
		// The detector picks the older mtime as keeper; both were written with the
		// same stamp, so the lexical order decides.
		keeperRel = "copy.bin"
	}
	return duplicateFixture{
		scope:     sc,
		item:      it,
		keeperRel: keeperRel,
		keeperRaw: filepath.Join(e.base, "trees", "dups", keeperRel),
	}
}

// purgeAttempt creates a purge plan for the due object and applies it.
//
// A refused member is reported in the plan result, not only as an error, so the
// caller gets both signals.
func (e *env) purgeAttempt(t *testing.T, f duplicateFixture) (execute.ApplyResult, error) {
	t.Helper()
	due, err := e.st.ItemsDueForPurge(context.Background(), f.scope.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected one due object, got %d", len(due))
	}
	p, err := plan.CreatePurgePlan(context.Background(), e.st, f.scope, due, domain.PlanManualPurge,
		e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatalf("purge plan: %v", err)
	}
	return e.exec.ApplyPurgePlan(context.Background(), e.scopeCtx(f.scope.ID), p.ID, p.Digest,
		domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
}

// requirePurgeBlocked asserts that the purge did not happen and said why.
func requirePurgeBlocked(t *testing.T, out execute.ApplyResult, err error) {
	t.Helper()
	if err != nil {
		requireCode(t, err, errs.CodeHeld)
		return
	}
	if out.Completed != 0 {
		t.Fatalf("the purge completed despite an invalid keeper: %+v", out)
	}
	if string(out.Code) != string(errs.CodeHeld) {
		t.Fatalf("purge result code = %s, want %s (%+v)", out.Code, errs.CodeHeld, out)
	}
}

// V14/V15: the positive case. An untouched keeper lets the duplicate be purged.
func TestDuplicateIsPurgedWhenTheKeeperIsIntact(t *testing.T) {
	e := newEnv(t)
	f := e.stageDuplicateDetectsAndIsolates(t)
	e.clock.Advance(31 * 24 * time.Hour)
	out, err := e.purgeAttempt(t, f)
	if err != nil {
		t.Fatalf("an intact keeper blocked the purge: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("the duplicate was not purged: %+v", out)
	}
}

// A18: an untouched keeper carrying a non-zero inode flag must still be purged.
//
// The scanned token is produced from a directory entry; the verified token is
// produced from the open descriptor. If the two are not the same fact, a file
// nobody touched looks modified and the duplicate is held forever.
func TestKeeperWithAnInodeFlagIsStillPurged(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("dups", domain.KindAssisted)
	e.writeFile("dups", "keeper.bin", "identical-payload", 40*24*time.Hour)
	e.writeFile("dups", "copy.bin", "identical-payload", 40*24*time.Hour)

	keeper := filepath.Join(e.base, "trees", "dups", "keeper.bin")
	copyPath := filepath.Join(e.base, "trees", "dups", "copy.bin")
	flags := markInodeFlag(t, keeper)
	t.Logf("inode flags on the keeper: %#x", flags)
	markInodeFlag(t, copyPath)

	e.scan(sc.ID)
	res, err := e.det.Detect(context.Background(), e.scopeCtx(sc.ID), nil)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		t.Fatalf("the flagged pair was not planned as one group: %+v", res.Notes)
	}
	p, items, err := e.det.CreatePlan(context.Background(), e.scopeCtx(sc.ID), res.Groups)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := e.apply(sc, p, p.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one isolated copy, got %d", len(items))
	}
	it, err := e.st.GetItem(context.Background(), items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)

	out, err := e.purgeAttempt(t, duplicateFixture{
		scope:     sc,
		item:      it,
		keeperRel: "keeper.bin",
		keeperRaw: keeper,
	})
	if err != nil {
		t.Fatalf("a flagged but untouched keeper blocked the purge: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("the duplicate was not purged: %+v", out)
	}
}

// V15: the keeper's permissions changed. The bytes are identical, so a hash-only
// check would have let the purge through; equivalence is not only about bytes.
func TestDuplicateIsHeldWhenTheKeeperPermissionsChange(t *testing.T) {
	e := newEnv(t)
	f := e.stageDuplicateDetectsAndIsolates(t)
	if err := os.Chmod(f.keeperRaw, 0o640); err != nil {
		t.Fatalf("chmod keeper: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)

	out, err := e.purgeAttempt(t, f)
	requirePurgeBlocked(t, out, err)
}

// V15: an extended attribute was added to the keeper without touching its bytes
// or its mtime.
func TestDuplicateIsHeldWhenTheKeeperMetadataChanges(t *testing.T) {
	e := newEnv(t)
	f := e.stageDuplicateDetectsAndIsolates(t)
	if err := unix.Setxattr(f.keeperRaw, "user.dustkeeper.test", []byte("1"), 0); err != nil {
		t.Skipf("extended attributes are not writable here: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)

	out, err := e.purgeAttempt(t, f)
	requirePurgeBlocked(t, out, err)
}

// V15: a second hard link to the keeper means another name can outlive the copy,
// so "the keeper survives" is no longer a statement about one file.
func TestDuplicateIsHeldWhenTheKeeperGainsALink(t *testing.T) {
	e := newEnv(t)
	f := e.stageDuplicateDetectsAndIsolates(t)
	extra := filepath.Join(e.base, "trees", "dups", "another-name.bin")
	if err := os.Link(f.keeperRaw, extra); err != nil {
		t.Fatalf("link keeper: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)

	out, err := e.purgeAttempt(t, f)
	requirePurgeBlocked(t, out, err)
}

// V15: the keeper's bytes changed but its size and mtime were put back, so the
// cheap checks pass and only a real byte comparison notices.
func TestDuplicateIsHeldWhenTheKeeperBytesDiffer(t *testing.T) {
	e := newEnv(t)
	f := e.stageDuplicateDetectsAndIsolates(t)

	st, err := os.Stat(f.keeperRaw)
	if err != nil {
		t.Fatalf("stat keeper: %v", err)
	}
	body := []byte("identical-payload")
	body[0] = 'X'
	if err := os.WriteFile(f.keeperRaw, body, 0o600); err != nil {
		t.Fatalf("rewrite keeper: %v", err)
	}
	// Restore the modification time so the timestamp check cannot be the reason.
	if err := os.Chtimes(f.keeperRaw, st.ModTime(), st.ModTime()); err != nil {
		t.Fatalf("chtimes keeper: %v", err)
	}
	e.clock.Advance(31 * 24 * time.Hour)

	out, err := e.purgeAttempt(t, f)
	requirePurgeBlocked(t, out, err)
}

// V16: one copy in the source scope and one in the reference scope is a
// duplicate. Only the source copy becomes a candidate and the reference copy is
// kept as the keeper.
func TestOneToOneCrossScopeDuplicateIsFound(t *testing.T) {
	e := newEnv(t)
	src := e.addScope("inbox", domain.KindManagedDisposable)
	ref := e.addScope("archive", domain.KindReference)
	e.writeFile("inbox", "report.pdf", "cross-scope-payload", 40*24*time.Hour)
	e.writeFile("archive", "report.pdf", "cross-scope-payload", 40*24*time.Hour)
	e.scan(src.ID)
	e.scan(ref.ID)

	res, err := e.det.Detect(context.Background(), e.scopeCtx(src.ID), &ref.ID)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		t.Fatalf("a one-to-one cross-scope duplicate was not found (%d groups, notes %v)",
			res.PlannableGroups, res.Notes)
	}
	g := res.Groups[0]
	if g.Keeper.ScopeID != ref.ID {
		t.Fatalf("the keeper is in scope %s, want the reference scope %s", g.Keeper.ScopeID, ref.ID)
	}
	if len(g.Others) != 1 || g.Others[0].ScopeID != src.ID {
		t.Fatalf("expected exactly the source copy as the candidate, got %+v", g.Others)
	}

	_, items, err := e.det.CreatePlan(context.Background(), e.scopeCtx(src.ID), res.Groups)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one item for the source copy, got %d", len(items))
	}
	if !e.exists("archive", "report.pdf") {
		t.Fatal("the reference copy was touched")
	}
}
