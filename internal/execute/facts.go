package execute

import (
	"context"
	"path/filepath"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// Local facts: what this program measured about one candidate, and what it is
// therefore allowed to conclude.
//
// The decision needs two statements that only the executor can make:
//
//	complete  every fact the decision depends on was actually measured
//	movable   reversible isolation is safe for this object, on evidence
//
// Both used to be computed from "the item reached this code path", which is a
// statement about the program and not about the file. "Has a size and an inode"
// says nothing about whether anything is using it, and being about to move a
// file says nothing about whether moving it is safe.

// OpenFilesFunc reports which files under one tree a visible process holds open.
type OpenFilesFunc func(ctx context.Context, root string) (assess.OpenObservation, error)

// candidateFacts is the measured half of the decision for one item.
type candidateFacts struct {
	Checks review.Checks
	// Complete reports that every fact the disposition depends on was measured.
	Complete bool
	// Movable reports that isolation is safe here, on those facts.
	Movable bool
	// Why names the fact that is missing, so "kept" can explain itself.
	Why string
}

// measureFacts establishes what is known about one candidate.
//
// The facts it needs for the disposition are: not protected, a single link, not
// the keeper of an isolated duplicate, an unchanged parent, and no visible
// process holding it open. The first four are measured here or immediately
// before; the fifth needs the process probe, and a probe that did not run is
// reported as unknown rather than as "nothing has it open".
//
// It returns no error on purpose: a fact that could not be measured is not a
// failure of the item, it is the reason the item is left alone.
func (e *Executor) measureFacts(ctx context.Context, sc *ScopeCtx, it store.Item,
	preID domain.FileIdentity, preTok domain.ChangeToken) candidateFacts {

	facts := candidateFacts{
		Checks: review.Checks{
			// Protection was answered just above; a protected item never reaches
			// a review. Keeper use was answered by the store query in the same
			// way, and in_use_by_keeper keeps that exact meaning here.
			Protected:  review.Bool(false),
			SingleLink: review.Bool(preTok.NLink <= 1),
			InUse:      review.Bool(false),
		},
	}

	parentStable, parentMeasured := e.parentUnchanged(sc, it)
	facts.Checks.ParentStable = review.Bool(parentStable)

	open, openMeasured := e.pathOpen(ctx, sc, it)
	if openMeasured {
		facts.Checks.OpenByProcess = review.Bool(open)
	}

	regular := preID.Kind == "" || preID.Kind == "regular"
	facts.Complete = parentMeasured && openMeasured && regular
	facts.Movable = facts.Complete && parentStable && !open && preTok.NLink <= 1

	switch {
	case !regular:
		facts.Why = "the object is not a regular file"
	case !parentMeasured:
		facts.Why = "the directory holding it could not be inspected"
	case !openMeasured:
		facts.Why = "no process listing was available, so no visible process can be ruled out"
	}
	return facts
}

// parentUnchanged reports whether the directory the item was planned under is
// still that directory.
func (e *Executor) parentUnchanged(sc *ScopeCtx, it store.Item) (bool, bool) {
	stored, err := decodeID(it.SourceParentIdentityJSON)
	if err != nil {
		return false, false
	}
	parent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		return false, false
	}
	defer parent.Close()
	now, _, err := parent.Stat()
	if err != nil {
		return false, false
	}
	return now.Equal(stored), true
}

// pathOpen reports whether a visible process holds this exact path open, and
// whether that could be established at all.
//
// When a test injects OpenFiles, that tree-scoped seam is consulted as before.
// In production each candidate is probed directly via assess.ProbeOpenPath so
// large directory trees never hit `lsof +D`'s tree-walk timeout or 256 KiB cap.
func (e *Executor) pathOpen(ctx context.Context, sc *ScopeCtx, it store.Item) (open, measured bool) {
	key := filepath.Clean(filepath.Join(sc.Root.Path(), string(it.RawRelative)))
	var (
		obs assess.OpenObservation
		err error
	)
	if e.OpenFiles != nil {
		obs, err = e.OpenFiles(ctx, sc.Root.Path())
		if err != nil {
			return false, false
		}
	} else {
		obs = assess.ProbeOpenPath(ctx, key, nil)
	}
	if obs.Paths[key] {
		return true, true
	}
	switch obs.Coverage {
	case "visible_processes_only":
		return false, true
	case "partial_visible_processes":
		// Something was listed, but not everything. The file's absence from
		// that list proves nothing.
		return false, false
	}
	return false, false
}
