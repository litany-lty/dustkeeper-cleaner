package execute

import (
	"context"
	"path/filepath"
	"sync"
	"time"

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

// OpenFiles is the probe used for a candidate's open-file fact. A nil field
// means the production probe; a test that has its own deterministic answer sets
// it instead, which is the same seam the clock and the review function use.

// openFilesTTL is how long one probe answer is reused.
//
// The probe is a snapshot of the process table at one instant, not a live
// subscription, and it costs about a second. A run of fifty candidates used to
// mean fifty snapshots; one per minute says the same thing about the tree
// without that cost.
//
// The window is a plain limitation and is stated as one: a file that a process
// opens after the snapshot is not in it. The identity, size and content checks
// at the moment of the action catch a file that is *changed*, and they do not
// catch a file that was merely opened for reading - opening does not alter any
// of the three. So the answer this cache carries is "no visible process held
// anything in this tree open at the moment of the snapshot", and it should not
// be read as a guarantee about the minute that follows.
const openFilesTTL = 60 * time.Second

type openFilesEntry struct {
	at  time.Time
	obs assess.OpenObservation
	err error
}

var (
	openFilesMu    sync.Mutex
	openFilesCache = map[string]openFilesEntry{}
)

// probeOpen answers "is anything under this tree open", using the injected
// probe when there is one and a cached snapshot otherwise.
func (e *Executor) probeOpen(ctx context.Context, sc *ScopeCtx) (assess.OpenObservation, error) {
	root := sc.Root.Path()
	if e.OpenFiles != nil {
		return e.OpenFiles(ctx, root)
	}
	openFilesMu.Lock()
	if entry, ok := openFilesCache[root]; ok && time.Since(entry.at) < openFilesTTL {
		openFilesMu.Unlock()
		return entry.obs, entry.err
	}
	openFilesMu.Unlock()

	obs := assess.ProbeOpenUnder(ctx, root, nil)
	var err error

	openFilesMu.Lock()
	if len(openFilesCache) > 64 {
		// The cache is keyed by tree and lives as long as the process; a
		// scheduled installation starts a new one every run, but a long-lived
		// test binary does not, so it is bounded rather than trusted to stay
		// small.
		openFilesCache = map[string]openFilesEntry{}
	}
	openFilesCache[root] = openFilesEntry{at: time.Now(), obs: obs, err: err}
	openFilesMu.Unlock()
	return obs, err
}

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
// A partial listing is not an answer to "this file is not open": the file may be
// in the part that was truncated. Only a complete listing, or a listing that
// names the file, decides the question.
func (e *Executor) pathOpen(ctx context.Context, sc *ScopeCtx, it store.Item) (open, measured bool) {
	obs, err := e.probeOpen(ctx, sc)
	if err != nil {
		return false, false
	}
	key := filepath.Clean(filepath.Join(sc.Root.Path(), string(it.RawRelative)))
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
