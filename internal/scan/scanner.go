// Package scan enumerates a scope with bounded resources.
//
// Three properties matter more than speed:
//
//   - it never follows a symlink and never crosses a mount;
//   - it never reads file content;
//   - a partial scan never implies that an unseen file was deleted.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// vaultName is the private management directory. It is never scanned: the
// program must not build candidate lists out of its own quarantine objects.
const vaultName = ".dustkeeper"

// Scanner walks the directory queue.
type Scanner struct {
	Store        *store.Store
	Clock        domain.Clock
	Guard        *policy.Guard
	Batch        int
	DirsPerClaim int
	ReadDirBatch int
}

// Stats reports what one scan pass did.
//
// Interrupted and Incomplete are separate facts and neither means success:
//
//	Interrupted  the batch stopped at a safe boundary because its budget ran out
//	Incomplete   a directory could not be finished (read error, vanished root)
//
// A caller may retire unseen observations only when the queue is empty and
// neither flag is set. The previous version returned nil from a budget stop and
// then declared the epoch complete, which turned "not scanned yet" into
// "deleted" for the tail of every large directory.
type Stats struct {
	Dirs           int   `json:"dirs"`
	Files          int   `json:"files"`
	Bytes          int64 `json:"bytes"`
	ProtectedDirs  int   `json:"protected_dirs"`
	SkippedEntries int   `json:"skipped_entries"`
	Unreadable     int   `json:"unreadable"`
	FailedDirs     int   `json:"failed_dirs"`
	// Vanished counts entries that were listed and then could not be stat'ed
	// because they were gone. It is the one entry-level error that means "this
	// path really is not there any more", so it is counted apart from Unreadable:
	// "the file ended" and "the filesystem would not answer" lead to opposite
	// conclusions.
	Vanished    int  `json:"vanished_entries"`
	Interrupted bool `json:"interrupted"`
	Incomplete  bool `json:"incomplete"`
}

// New returns a scanner with conservative defaults.
func New(st *store.Store, clock domain.Clock, guard *policy.Guard, batch int) *Scanner {
	if batch <= 0 {
		batch = 500
	}
	return &Scanner{
		Store:        st,
		Clock:        clock,
		Guard:        guard,
		Batch:        batch,
		DirsPerClaim: 8,
		ReadDirBatch: 128,
	}
}

// dirState is the outcome for one directory, with the reason a failure carries
// into the queue so a later run and a report can both name it.
type dirState struct {
	kind   int
	reason string
}

const (
	stateDone = iota
	stateInterrupted
	stateFailed
)

var (
	dirDone        = dirState{kind: stateDone}
	dirInterrupted = dirState{kind: stateInterrupted}
)

// dirFailedWith records why a directory could not be read. The reason is
// written to the queue row so the next round, and any report built from the
// database, can say which directory failed and why instead of only counting it
// in one process's memory.
func dirFailedWith(reason string) dirState { return dirState{kind: stateFailed, reason: reason} }

// Run processes directories until the queue is empty or the run budget is spent.
//
// The caller starts the epoch and, after a completed Run with no pending work,
// retires unseen entries. Calling that retire step from a partial scan would
// turn "not scanned yet" into "deleted".
func (s *Scanner) Run(ctx context.Context, vs scope.VerifiedScope, epoch int64, run *domain.RunLedger) (Stats, error) {
	var stats Stats
	rootDev := vs.RootID.Runtime.Device
	// failedHere remembers the directories this pass could not read. The claim
	// loop offers every row it has not finished, and a failed row is one of those,
	// so without this the pass would claim it, fail it, claim it again and fail it
	// again until the budget expired — rewriting the same row the whole time.
	failedHere := make(map[string]string)

	for {
		if err := ctx.Err(); err != nil {
			return stats, errs.New(errs.CodeCancelled, "scan cancelled")
		}
		if run.TimeUp(s.Clock.Mono()) || run.ReadUp() {
			stats.Interrupted = true
			return stats, nil
		}

		dirs, err := s.Store.ClaimDirs(ctx, vs.Scope.ID, epoch, s.DirsPerClaim)
		if err != nil {
			return stats, err
		}
		if len(dirs) == 0 {
			return stats, nil
		}

		progressed := false
		for i, rel := range dirs {
			if reason, again := failedHere[string(rel)]; again {
				if err := s.Store.MarkDirFailed(ctx, vs.Scope.ID, epoch, rel, reason); err != nil {
					return stats, err
				}
				continue
			}
			state, err := s.scanDir(ctx, vs, epoch, rootDev, rel, run, &stats)
			if err != nil {
				return stats, err
			}
			switch state.kind {
			case stateDone:
				if err := s.Store.MarkDirDone(ctx, vs.Scope.ID, epoch, rel); err != nil {
					return stats, err
				}
				stats.Dirs++
				progressed = true
			case stateFailed:
				// A directory that cannot be read is not "empty" and not "done".
				// It used to be marked done so the queue could advance, which
				// kept the incompleteness in this run's memory only: the next
				// round saw an empty pending set, called the epoch complete, and
				// retired every observation inside that directory as stale.
				//
				// Recording it as failed keeps the fact in the queue. The epoch
				// stays open, the directory is claimed again in a later round
				// (least-recently-claimed first, so it cannot starve the rest),
				// and only a successful pass over it lets the epoch complete.
				if err := s.Store.MarkDirFailed(ctx, vs.Scope.ID, epoch, rel, state.reason); err != nil {
					return stats, err
				}
				failedHere[string(rel)] = state.reason
				stats.FailedDirs++
				stats.Incomplete = true
				progressed = true
			case stateInterrupted:
				// Leave this one claimed: the next pass requeues it and resumes
				// here instead of starting a new epoch that would never reach the
				// tail. The rest of the batch was claimed but never opened, so it
				// goes back to pending without a claim sequence and will be picked
				// before this directory next time.
				if err := s.Store.ReleaseClaimed(ctx, vs.Scope.ID, epoch, dirs[i+1:]); err != nil {
					return stats, err
				}
				stats.Interrupted = true
				stats.Incomplete = true
				return stats, nil
			}
			if run.TimeUp(s.Clock.Mono()) {
				if err := s.Store.ReleaseClaimed(ctx, vs.Scope.ID, epoch, dirs[i+1:]); err != nil {
					return stats, err
				}
				stats.Interrupted = true
				return stats, nil
			}
		}
		if !progressed {
			// Everything this batch offered had already been tried in this pass.
			// The failures stay in the queue for the next pass; stopping here is
			// what keeps this one from spinning on them.
			return stats, nil
		}
	}
}

func (s *Scanner) scanDir(ctx context.Context, vs scope.VerifiedScope, epoch int64, rootDev uint64,
	rel []byte, run *domain.RunLedger, stats *Stats) (dirState, error) {

	if f := TestHookDirRead; f != nil {
		if herr := f(string(rel)); herr != nil {
			stats.Unreadable++
			return dirFailedWith(herr.Error()), nil
		}
	}

	dir, err := vs.Root.OpenDir(rel)
	if err != nil {
		// A directory that vanished, became a symlink or crossed a mount is
		// done with: nothing inside it can be enumerated, and treating it as
		// "empty" is not a lie because there is no reachable content left to
		// list. Its own observations are retired when the epoch completes,
		// which is correct — the path no longer holds a directory we manage.
		if errors.Is(err, unix.ENOENT) || errors.Is(err, fsops.ErrSymlink) || errors.Is(err, fsops.ErrCrossMount) {
			stats.Unreadable++
			return dirDone, nil
		}
		// A permission error is different: the directory is still there and
		// still holds files we have observations for. It is failed, not done.
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			stats.Unreadable++
			return dirFailedWith("permission denied"), nil
		}
		// A root that a path now resolves to a different inode for is not a
		// directory this scan may report on at all: every identity underneath it
		// was recorded against another object.
		if errors.Is(err, fsops.ErrRootChanged) {
			return dirFailedWith("root replaced"), errs.Wrap(errs.CodeScopeChanged, err)
		}
		return dirFailedWith("open failed"), errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer dir.Close()

	dirID, _, err := dir.Stat()
	if err != nil {
		stats.Unreadable++
		return dirFailedWith("stat failed"), nil
	}
	if ok, _ := s.Guard.CheckSubtree(string(rel), dirID); ok {
		stats.ProtectedDirs++
		return dirDone, nil
	}
	// A directory containing a project marker is a project tree. Protecting only
	// the marker's own subtree leaves "repo/src/main.go" exposed, because no
	// segment of that path is ".git".
	for _, marker := range []string{".git", ".hg", ".svn"} {
		if _, _, merr := dir.StatEntry(marker); merr == nil {
			stats.ProtectedDirs++
			return dirDone, nil
		}
	}

	parentIdentityJSON, err := jsonIdentity(dirID)
	if err != nil {
		return dirFailedWith("identity unavailable"), err
	}

	var (
		obsBatch []store.Observation
		dirBatch [][]byte
		nowNS    = s.Clock.Wall().UnixNano()
		// Entries this directory listed but could not describe. They are not
		// reported one by one: the queue row carries one reason, and the queue is
		// what survives this process.
		unverifiedCount int
		unverifiedFirst string
	)

	flush := func() error {
		if len(obsBatch) > 0 {
			if err := s.Store.UpsertObservations(ctx, obsBatch); err != nil {
				return err
			}
			obsBatch = obsBatch[:0]
		}
		if len(dirBatch) > 0 {
			if err := s.Store.EnqueueDirs(ctx, vs.Scope.ID, epoch, dirBatch); err != nil {
				return err
			}
			dirBatch = dirBatch[:0]
		}
		return nil
	}

	for {
		entries, err := dir.Readdir(s.ReadDirBatch)
		if err != nil && len(entries) == 0 {
			if errors.Is(err, io.EOF) {
				break
			}
			// A non-EOF read error is not the end of the directory. Flushing what
			// was seen and marking the directory finished would record a partial
			// listing as complete.
			if ferr := flush(); ferr != nil {
				return dirFailedWith("read error"), ferr
			}
			stats.Unreadable++
			return dirFailedWith("read error"), nil
		}
		if len(entries) == 0 {
			break
		}
		for _, ent := range entries {
			name := ent.Name()
			if name == vaultName {
				continue
			}
			if !utf8.ValidString(name) {
				// A name we cannot represent losslessly is reported, never
				// planned: the executor would have to re-encode a display
				// string into a path, which this design forbids.
				stats.SkippedEntries++
				continue
			}

			childRel := name
			if len(rel) > 0 {
				childRel = string(rel) + "/" + name
			}

			// One entry the filesystem would not describe. "I could not look" is
			// not "it is not there": the directory is unfinished, the epoch stays
			// open, and the old observation of this path survives. Only ENOENT is
			// an answer about the file itself.
			if herr := entryStatError(childRel); herr != nil {
				if errors.Is(herr, unix.ENOENT) {
					stats.Vanished++
					continue
				}
				stats.Unreadable++
				unverifiedCount++
				if unverifiedFirst == "" {
					unverifiedFirst = fmt.Sprintf("%q: %v", name, herr)
				}
				continue
			}

			id, token, err := dir.StatEntry(name)
			if err != nil {
				if errors.Is(err, unix.ENOENT) {
					// It was in the listing and is gone by the time we asked. The
					// epoch's retirement sweep is what withdraws its observation,
					// and it may only run if the epoch completes.
					stats.Vanished++
					continue
				}
				stats.Unreadable++
				unverifiedCount++
				if unverifiedFirst == "" {
					unverifiedFirst = fmt.Sprintf("%q: %v", name, err)
				}
				continue
			}

			switch id.Kind {
			case "directory":
				if protected, _ := policy.GlobalProtected(childRel + "/x"); protected {
					stats.ProtectedDirs++
					continue
				}
				if ok, _ := s.Guard.CheckSubtree(childRel, id); ok {
					stats.ProtectedDirs++
					continue
				}
				dirBatch = append(dirBatch, []byte(childRel))
			case "regular":
				if token.NLink > 1 {
					// A hard-linked file is not an independent copy and moving it
					// would change another path's behaviour.
					stats.SkippedEntries++
					continue
				}
				obsBatch = append(obsBatch, store.Observation{
					ScopeID:      vs.Scope.ID,
					RawRelative:  []byte(childRel),
					IdentityJSON: mustJSONIdentity(id),
					TokenJSON:    mustJSONToken(token),
					ParentJSON:   parentIdentityJSON,
					SizeBytes:    token.Size,
					MTimeNS:      token.MTimeNS,
					FirstSeenNS:  nowNS,
					LastSeenNS:   nowNS,
					ScanEpoch:    epoch,
					Status:       domain.ObsSeen,
				})
				stats.Files++
				stats.Bytes += token.Size
			default:
				// Symlinks, devices, sockets and FIFOs are not objects this
				// program moves.
				stats.SkippedEntries++
			}
		}

		if len(obsBatch) >= s.Batch || len(dirBatch) >= s.Batch {
			if err := flush(); err != nil {
				return dirFailedWith("flush failed"), err
			}
		}
		if unverifiedCount > 0 {
			// The entries that could be read are written first: a directory that
			// is only partly describable still has facts worth updating. What it
			// does not have is an ending - the epoch stays open, so the
			// observations this pass could not confirm are not retired as stale.
			if err := flush(); err != nil {
				return dirFailedWith("flush failed"), err
			}
			return dirFailedWith(fmt.Sprintf("%d entries could not be inspected (first: %s)",
				unverifiedCount, unverifiedFirst)), nil
		}
		if run.TimeUp(s.Clock.Mono()) {
			// Flush what we have, then report the directory as unfinished. Its
			// already-recorded observations stay; the rest is picked up by the
			// next run, which resumes this epoch.
			if err := flush(); err != nil {
				return dirInterrupted, err
			}
			return dirInterrupted, nil
		}
	}

	if err := flush(); err != nil {
		return dirFailedWith("flush failed"), err
	}
	return dirDone, nil
}

// InsertFailures counts unreadable entries per run; kept for reporting only.
func (s Stats) InsertFailures() int { return s.Unreadable }

// entryStatError consults the test seam. It is nil in production, so the whole
// cost is one branch per entry.
func entryStatError(rel string) error {
	if f := TestHookEntryStat; f != nil {
		return f(rel)
	}
	return nil
}

// SanitizeDisplay is used by reports; kept here so the scanner owns the rule
// that display strings never round-trip into syscalls.
func SanitizeDisplay(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	return strings.ToValidUTF8(p, "\uFFFD")
}
