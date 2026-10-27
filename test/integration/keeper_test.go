package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
)

// prepareDuplicatePair scans a scope with two identical copies and a reference
// scope holding a third, plans the duplicate pair and applies it. The returned
// item is the quarantined copy that still names the reference copy as keeper.
func (e *env) prepareDuplicatePair() (src, keeperScope domain.ScopeID, got domain.ItemID) {
	e.t.Helper()
	src = e.addScope("dups", domain.KindAssisted).ID
	keeperScope = e.addScope("originals", domain.KindReference).ID

	e.writeFile("dups", "one.bin", "same-bytes", 0)
	e.writeFile("dups", "two.bin", "same-bytes", 0)
	e.writeFile("originals", "keep.bin", "same-bytes", 0)

	e.scan(src)
	e.scan(keeperScope)

	sctx := e.scopeCtx(src)
	ks := keeperScope
	res, err := e.det.Detect(context.Background(), sctx, &ks)
	if err != nil {
		e.t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		e.t.Fatalf("expected one plannable group, got %d (%v)", res.PlannableGroups, res.Notes)
	}
	p, items, err := e.det.CreatePlan(context.Background(), sctx, res.Groups)
	if err != nil {
		e.t.Fatalf("create plan: %v", err)
	}
	if len(items) != 2 {
		e.t.Fatalf("expected two copies planned, got %d", len(items))
	}
	srcScope, err := e.st.GetScope(context.Background(), src)
	if err != nil {
		e.t.Fatalf("get scope: %v", err)
	}
	if _, err := e.apply(srcScope, p, p.Digest); err != nil {
		e.t.Fatalf("apply: %v", err)
	}

	due, err := e.st.ItemsDueForPurge(context.Background(), src, e.clock.Wall().AddDate(0, 0, 31).UnixNano(), 10)
	if err != nil {
		e.t.Fatalf("due: %v", err)
	}
	if len(due) == 0 {
		e.t.Fatal("no duplicate became due")
	}
	return src, keeperScope, due[0].ID
}

// keeperPath is where the reference copy lives.
func (e *env) keeperPath() string {
	return filepath.Join(e.base, "trees", "originals", "keep.bin")
}

// TestAKeeperChangedAfterTheFinalComparisonBlocksThePurge is the window R09 A
// describes. The comparison has already read the keeper's bytes and found them
// equal; a write then changes a region it has already passed, so the comparison
// still returns "same". Nothing used to look at the keeper again, and the payload
// was deleted.
//
// The fixture is the smallest one that makes the window deterministic: one block,
// written after the block was compared.
func TestAKeeperChangedAfterTheFinalComparisonBlocksThePurge(t *testing.T) {
	e := newEnv(t)
	src, _, itemID := e.prepareDuplicatePair()
	e.clock.Advance(31 * 24 * time.Hour)

	item, err := e.st.GetItem(context.Background(), itemID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.Reason != domain.ReasonExactDuplicate {
		t.Fatalf("expected an exact duplicate, got %q", item.Reason)
	}

	path := e.keeperPath()
	execute.TestHookDuringCompare = func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open keeper: %v", err)
			return
		}
		// Same length, so the size stays the same: the change is only visible in
		// the mtime and the metadata digest, which is exactly what the last check
		// has to read.
		if _, err := f.WriteAt([]byte("X"), 0); err != nil {
			t.Errorf("modify keeper: %v", err)
		}
		f.Close()
	}
	t.Cleanup(func() { execute.TestHookDuringCompare = nil })

	sc, err := e.st.GetScope(context.Background(), src)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	err = e.tryPurge(sc, item)
	requireCode(t, err, errs.CodeHeld)
	if !e.vaultObjectExists(sc, item) {
		t.Fatal("the payload was deleted although the keeper changed after the comparison")
	}
	if action := e.lastIntentAction(item.ID); action != "quarantine" {
		t.Fatalf("a purge intent was recorded for a keeper that moved: %q", action)
	}
}

// TestAnIntactDuplicateIsStillPurged is the positive control: the last check must
// not turn every duplicate into a hold.
func TestAnIntactDuplicateIsStillPurged(t *testing.T) {
	e := newEnv(t)
	src, _, itemID := e.prepareDuplicatePair()
	e.clock.Advance(31 * 24 * time.Hour)

	item, err := e.st.GetItem(context.Background(), itemID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	sc, err := e.st.GetScope(context.Background(), src)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if err := e.tryPurge(sc, item); err != nil {
		t.Fatalf("an intact duplicate was not purged: %v", err)
	}
	if e.vaultObjectExists(sc, item) {
		t.Fatal("the payload is still in the vault")
	}
	if !e.exists("originals", "keep.bin") {
		t.Fatal("the keeper was removed")
	}
	if action := e.lastIntentAction(item.ID); action != "purge" {
		t.Fatalf("expected a purge intent, got %q", action)
	}
}
