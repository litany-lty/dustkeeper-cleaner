package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// The tests in this file are about one kind of protection that does not come
// from a rule or from a registered subtree: the marker that says "this directory
// is somebody's working copy". It is checked in two places that decide different
// things - the scan, which chooses what to observe, and the executor, which
// chooses what to move - and a plain folder can become a repository between the
// two.

// TestAGitRepositoryAtTheScopeRootIsNotScanned: when the scope root itself is a
// repository, nothing underneath it is a candidate. This is deliberately blunt:
// a managed_disposable scope pointing at a checkout is a configuration mistake,
// and the safe reading of that mistake is "protect all of it", not "take the
// files that look old".
func TestAGitRepositoryAtTheScopeRootIsNotScanned(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("repo", domain.KindManagedDisposable)
	if err := os.MkdirAll(filepath.Join(e.base, "trees", "repo", ".git", "objects"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	e.writeFile("repo", "build.zip", "payload", 40*24*time.Hour)
	e.writeFile("repo", "src", "not a directory, just a file", 40*24*time.Hour)

	e.scan(sc.ID)

	ctx := context.Background()
	n, err := e.st.CountObservations(ctx, sc.ID)
	if err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d object(s) inside a repository were observed; the scan descended into it", n)
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	if _, perr := e.planForMayFail(sc, r, 10); errs.CodeOf(perr) != errs.CodeNoCandidates {
		t.Fatalf("a plan was offered for a repository tree: %v", perr)
	}

	if rep := e.runOnce(); rep.Quarantined != 0 {
		t.Fatalf("the background run took %d object(s) out of a repository", rep.Quarantined)
	}
	if !e.exists("repo", "build.zip") {
		t.Fatal("a file inside a repository was moved")
	}
}

// TestAGitWorktreePointerFileProtectsTheTreeToo: in a linked worktree, ".git" is
// a one-line file pointing at the real metadata directory, not a directory. A
// check that only looked for a directory would leave every worktree exposed -
// which is exactly the tree a developer is most likely to have in a scratch
// directory.
func TestAGitWorktreePointerFileProtectsTheTreeToo(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("wt", domain.KindManagedDisposable)
	root := filepath.Join(e.base, "trees", "wt")
	pointer := []byte("gitdir: /elsewhere/.git/worktrees/wt\n")
	if err := os.WriteFile(filepath.Join(root, ".git"), pointer, 0o644); err != nil {
		t.Fatalf("write the worktree pointer: %v", err)
	}
	e.writeFile("wt", "build.zip", "payload", 40*24*time.Hour)

	e.scan(sc.ID)

	ctx := context.Background()
	n, err := e.st.CountObservations(ctx, sc.ID)
	if err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d object(s) inside a linked worktree were observed", n)
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	if rep := e.runOnce(); rep.Quarantined != 0 {
		t.Fatalf("the background run took %d object(s) out of a linked worktree", rep.Quarantined)
	}
	if !e.exists("wt", "build.zip") {
		t.Fatal("a file inside a linked worktree was moved")
	}
	// The pointer file is still a file with its contents intact: refusing to act
	// must not mean rewriting what marks the tree.
	got, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatalf("read the pointer back: %v", err)
	}
	if string(got) != string(pointer) {
		t.Fatalf("the worktree pointer was rewritten: %q", got)
	}
}

// TestAProjectMarkerThatAppearsAfterThePlanStopsTheMove is the case the two
// checks exist for. The plan was approved while the directory was an ordinary
// folder, so the plan is not wrong; by the time it runs, a repository has been
// created in it. Nothing moves, and the item does not become "quarantined".
func TestAProjectMarkerThatAppearsAfterThePlanStopsTheMove(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("work", domain.KindManagedDisposable)
	e.writeFile("work", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	res := e.planFor(sc, r, 10)
	if len(res.Items) != 1 {
		t.Fatalf("the plan offered %d objects, want 1", len(res.Items))
	}

	// `git init` between the approval and the run.
	if err := os.MkdirAll(filepath.Join(e.base, "trees", "work", ".git", "refs"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply returned an error instead of a per-item verdict: %v", err)
	}
	if out.Completed != 0 {
		t.Fatalf("%d object(s) moved out of a tree that had become a repository", out.Completed)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected one result, got %+v", out.Results)
	}
	if got := out.Results[0].Code; got != string(errs.CodeSkippedProtected) {
		t.Fatalf("the object was reported as %q, want %q (%s)",
			got, errs.CodeSkippedProtected, out.Results[0].Message)
	}
	if !e.exists("work", "a.zip") {
		t.Fatal("the file was moved even though the tree is a repository now")
	}
	items := e.itemsFor(res.Plan)
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	if items[0].State != domain.StatePlanned {
		t.Fatalf("the item was settled as %q by a move that never happened", items[0].State)
	}
}

// TestAProtectedRepositoryDoesNotFreezeItsSiblings is the other direction: a
// protection that leaked out of the marked subtree would make one checkout in a
// shared directory enough to stop the whole scope from ever being cleaned.
func TestAProtectedRepositoryDoesNotFreezeItsSiblings(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("shared", domain.KindManagedDisposable)
	if err := os.MkdirAll(filepath.Join(e.base, "trees", "shared", "checkout", ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	e.writeFile("shared", "checkout/README", "the checkout's own file", 40*24*time.Hour)
	e.writeFile("shared", "loose.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	res := e.planFor(sc, r, 10)
	if len(res.Items) != 1 {
		t.Fatalf("the plan offered %d objects, want the one outside the checkout", len(res.Items))
	}

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("the object outside the checkout was not quarantined: %+v", out)
	}
	if !e.exists("shared", "checkout/README") {
		t.Fatal("the checkout lost a file")
	}
	if e.exists("shared", "loose.zip") {
		t.Fatal("the loose file is still there")
	}
}
