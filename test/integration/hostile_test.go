package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// These cases are the hostile-environment half of the acceptance list: a file
// system that lies about identity, links that point elsewhere, objects the
// process may not read, budgets that run out, a second instance, and a signal in
// the middle of a batch. Every one of them must end with the file still where it
// was, or with an explanation on record.

// A18: a file with more than one hard link never becomes a candidate, because
// moving one name of a shared inode frees nothing and changes another path.
func TestHardLinkedFileIsNotMoved(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("hard", "managed_disposable")
	e.writeFile("hard", "a.zip", "aaaa", 60*24*time.Hour)
	e.writeFile("hard", "b.zip", "bbbb", 60*24*time.Hour)

	// b.zip gets a second name: the same inode, two paths.
	second := filepath.Join(e.base, "trees", "hard", "b-copy.zip")
	if err := os.Link(filepath.Join(e.base, "trees", "hard", "b.zip"), second); err != nil {
		t.Skipf("the filesystem does not support hard links: %v", err)
	}

	e.scan(sc.ID)
	r := e.rule(sc.ID, "hard-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)

	// Three names match the rule, but only the one with a single link is a
	// candidate. The exclusion happens at scan time, before any plan exists.
	if got := len(res.Items); got != 1 {
		t.Fatalf("planned %d items; the hard-linked pair must not be a candidate", got)
	}

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("completed_count = %d; only the single-link file may move", out.Completed)
	}
	if !e.exists("hard", "b.zip") || !e.exists("hard", "b-copy.zip") {
		t.Fatal("a hard-linked file was moved despite the single-link rule")
	}
	if e.exists("hard", "a.zip") {
		t.Fatal("the file without extra links should have moved")
	}
}

// A19: a file this process may not read is reported, never collected.
func TestUnreadableFileIsNotMoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	e := newEnv(t)
	sc := e.addScope("perm", "managed_disposable")
	e.writeFile("perm", "locked.zip", "secret", 60*24*time.Hour)

	locked := filepath.Join(e.base, "trees", "perm", "locked.zip")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })

	e.scan(sc.ID)
	r := e.rule(sc.ID, "perm-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		// A refusal before the batch starts is also a safe outcome.
		if errs.CodeOf(err) != errs.CodeAccessDenied {
			t.Fatalf("unexpected failure: %v", err)
		}
	}
	if _, statErr := os.Stat(locked); statErr != nil {
		t.Fatal("the unreadable file disappeared")
	}
	if out.Completed != 0 {
		t.Fatalf("completed_count = %d; an unreadable file must not be collected", out.Completed)
	}
}

// A15: a symbolic link is a link, not the thing it points at.
func TestSymlinkIsNotFollowed(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("links", "managed_disposable")

	// An outside file that a link inside the scope points at.
	outside := filepath.Join(e.base, "outside.zip")
	if err := os.WriteFile(outside, []byte("outside-payload"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	old := e.clock.Wall().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatalf("chtimes outside: %v", err)
	}

	root := filepath.Join(e.base, "trees", "links")
	if err := os.Symlink(outside, filepath.Join(root, "link.zip")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A linked directory is the dangerous one: following it would collect files
	// that live outside the scope.
	if err := os.Symlink(e.base, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	e.scan(sc.ID)
	r := e.rule(sc.ID, "link-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res, planErr := e.planForMayFail(sc, r, 10)

	// The link matches the name pattern, yet it is not a candidate: a link is not
	// a regular file, and the walk never follows one. "No candidates" is the pass
	// condition here, so an error with that code is expected.
	if planErr != nil {
		if errs.CodeOf(planErr) != errs.CodeNoCandidates {
			t.Fatalf("planning a link-only tree failed unexpectedly: %v", planErr)
		}
	} else {
		if len(res.Items) != 0 {
			t.Fatalf("planned %d items for a tree that holds only links", len(res.Items))
		}
		if out, err := e.apply(sc, res.Plan, res.Plan.Digest); err == nil && out.Completed > 0 {
			t.Fatalf("completed_count = %d; nothing behind a link may be collected", out.Completed)
		}
	}

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the file the link pointed at is gone: %v", err)
	}
	// The link itself must still be there: it was not selected, not deleted.
	if _, err := os.Lstat(filepath.Join(root, "link.zip")); err != nil {
		t.Fatalf("the symbolic link itself was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "escape")); err != nil {
		t.Fatalf("the directory link was removed: %v", err)
	}
}

// A14: protection is decided on kernel identity, so a differently-spelled path
// on a case-insensitive volume protects the same object.
func TestProtectionUsesIdentityNotSpelling(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("alias", "managed_disposable")

	dir := filepath.Join(e.base, "trees", "alias", "Keep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	e.writeFile("alias", "Keep/a.zip", "keep me", 60*24*time.Hour)

	// Is this volume case-insensitive? If it is not, the alias cannot exist and
	// the test would be asserting nothing.
	if _, err := os.Stat(filepath.Join(e.base, "trees", "alias", "keep")); err != nil {
		t.Skipf("the volume is case-sensitive, so there is no alias to test: %v", err)
	}

	// Control: without the protection the file is a candidate.
	r := e.rule(sc.ID, "alias-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.scan(sc.ID)
	before := e.planFor(sc, r, 10)
	if len(before.Items) != 1 {
		t.Fatalf("control planned %d items; the alias test needs exactly one candidate", len(before.Items))
	}
	if _, err := e.apply(sc, before.Plan, before.Plan.Digest); err != nil {
		t.Fatalf("control apply: %v", err)
	}
	// Put the file back so the alias case starts from the same state.
	e.writeFile("alias", "Keep/a.zip", "keep me", 60*24*time.Hour)

	// The operator writes the path in a different case than the directory on disk.
	if _, err := e.st.InsertProtection(context.Background(), store.Protection{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.ID,
		RawRelative:  []byte("keep/a.zip"),
		MatchKind:    domain.MatchExact,
		Source:       domain.ProtectUser,
		EvidenceJSON: `{"source":"user"}`,
		Active:       true,
		CreatedAtNS:  e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("insert protection: %v", err)
	}

	e.scan(sc.ID)
	res, planErr := e.planForMayFail(sc, r, 10)

	// Two safe outcomes: the guard resolves "keep/a.zip" to the object reached via
	// "Keep/a.zip" and the candidate disappears, or resolution fails loudly and
	// nothing is planned. Collecting the file is not among them.
	if planErr != nil {
		if errs.CodeOf(planErr) != errs.CodeNoCandidates {
			t.Fatalf("planning with an alias protection failed unexpectedly: %v", planErr)
		}
	} else {
		if len(res.Items) != 0 {
			t.Fatalf("planned %d items while the object is protected under an alias", len(res.Items))
		}
		if out, err := e.apply(sc, res.Plan, res.Plan.Digest); err == nil && out.Completed > 0 {
			t.Fatalf("the protected file was collected anyway: %+v", out)
		}
	}
	if !e.exists("alias", "Keep/a.zip") {
		t.Fatal("a file protected under a different spelling was moved")
	}
}

// A16 documents why there is no cross-device test here.
func TestCrossDeviceIsRefused(t *testing.T) {
	t.Skip("creating a second device inside a temporary directory needs a mount (root); " +
		"the dev check is covered by fsops' cross-device guard and OpenRegularAt's rootDev comparison")
}

// A21: an exhausted read budget stops collection of the rest of the plan,
// without touching the files it did not reach.
func TestBudgetExhaustionLeavesTheRestAlone(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("budget", "managed_disposable")
	e.writeFile("budget", "a.zip", "0123456789", 60*24*time.Hour)
	e.writeFile("budget", "b.zip", "0123456789", 60*24*time.Hour)

	e.scan(sc.ID)
	r := e.rule(sc.ID, "budget-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	p := res.Plan
	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	// Each file needs two reads of 10 bytes. One file fits; the second does not.
	budget := domain.NewBudget(300, 25, 100, 1<<30, 0)

	out, err := e.exec.ApplyQuarantinePlan(context.Background(), sctx, p.ID, res.Plan.Digest, budget)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("completed_count = %d; the budget allowed exactly one file", out.Completed)
	}
	if out.Failed != 1 {
		t.Fatalf("failed_count = %d; the second file should have been refused", out.Failed)
	}
	if out.Code != string(errs.CodePartial) {
		t.Fatalf("code = %s; a partly finished plan reports PARTIAL", out.Code)
	}
	// The refused file is still there, and no intent was left behind for it.
	remaining := e.exists("budget", "a.zip") != e.exists("budget", "b.zip")
	if !remaining {
		t.Fatal("exactly one file should remain")
	}
	pending, err := e.st.PendingOps(context.Background())
	if err != nil {
		t.Fatalf("pending ops: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d pending operations after a budget stop; a refusal happens before any intent", len(pending))
	}
}

// A22: a second instance on the same state directory is refused, not queued.
func TestSecondInstanceIsRefused(t *testing.T) {
	e := newEnv(t)

	first, err := scope.InstanceLock(e.paths.StateDir)
	if err != nil {
		t.Fatalf("first instance lock: %v", err)
	}
	if _, err := scope.InstanceLock(e.paths.StateDir); err == nil {
		t.Fatal("a second instance took the same lock")
	} else {
		requireCode(t, err, errs.CodeInstanceBusy)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := scope.InstanceLock(e.paths.StateDir)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again.Release()
}

// A23: an interrupted batch reports cancellation and leaves the world unchanged.
func TestCancellationMovesNothing(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("cancel", "managed_disposable")
	e.writeFile("cancel", "a.zip", "aaaa", 60*24*time.Hour)
	e.writeFile("cancel", "b.zip", "bbbb", 60*24*time.Hour)

	e.scan(sc.ID)
	r := e.rule(sc.ID, "cancel-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	p := res.Plan
	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := e.exec.ApplyQuarantinePlan(ctx, sctx, p.ID, res.Plan.Digest, domain.NewBudget(300, 1<<30, 100, 1<<30, 0))

	// Either the store refused the cancelled context or the loop reported it. The
	// CLI converts a wrapped context.Canceled into exit 130, so the error must
	// stay discoverable through errors.Is; a completed item is not acceptable.
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancellation was lost: %v", err)
		}
		if code := errs.CodeOf(err); code != errs.CodeCancelled && code != errs.CodeDBError {
			t.Fatalf("unexpected code %s for a cancelled batch", code)
		}
	} else if out.Code != string(errs.CodeCancelled) {
		t.Fatalf("code = %s; an interrupted batch reports CANCELLED", out.Code)
	}
	if out.Completed != 0 {
		t.Fatalf("completed_count = %d after cancellation", out.Completed)
	}
	if !e.exists("cancel", "a.zip") || !e.exists("cancel", "b.zip") {
		t.Fatal("a cancelled batch moved a file")
	}
}
