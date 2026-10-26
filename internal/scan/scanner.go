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
	Interrupted    bool  `json:"interrupted"`
	Incomplete     bool  `json:"incomplete"`
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

// dirState is the outcome for one directory.
type dirState int

const (
	dirDone dirState = iota
	dirInterrupted
	dirFailed
)

// Run processes directories until the queue is empty or the run budget is spent.
//
// The caller starts the epoch and, after a completed Run with no pending work,
// retires unseen entries. Calling that retire step from a partial scan would
// turn "not scanned yet" into "deleted".
func (s *Scanner) Run(ctx context.Context, vs scope.VerifiedScope, epoch int64, run *domain.RunLedger) (Stats, error) {
	var stats Stats
	rootDev := vs.RootID.Runtime.Device

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

		for _, rel := range dirs {
			state, err := s.scanDir(ctx, vs, epoch, rootDev, rel, run, &stats)
			if err != nil {
				return stats, err
			}
			switch state {
			case dirDone:
				if err := s.Store.MarkDirDone(ctx, vs.Scope.ID, epoch, rel); err != nil {
					return stats, err
				}
				stats.Dirs++
			case dirFailed:
				// A directory that cannot be read is not "empty" and not
				// "done": it is counted, the epoch cannot call itself complete,
				// and the next epoch will walk it again from the root. Marking
				// it done here is what lets the queue advance instead of
				// spinning on the same entry for ever.
				if err := s.Store.MarkDirDone(ctx, vs.Scope.ID, epoch, rel); err != nil {
					return stats, err
				}
				stats.FailedDirs++
				stats.Incomplete = true
			case dirInterrupted:
				// Leave it claimed. The next run requeues active directories and
				// resumes this one instead of starting a new epoch that would
				// never reach the tail.
				stats.Interrupted = true
				stats.Incomplete = true
				return stats, nil
			}
			if run.TimeUp(s.Clock.Mono()) {
				stats.Interrupted = true
				return stats, nil
			}
		}
	}
}

func (s *Scanner) scanDir(ctx context.Context, vs scope.VerifiedScope, epoch int64, rootDev uint64,
	rel []byte, run *domain.RunLedger, stats *Stats) (dirState, error) {

	dir, err := vs.Root.OpenDir(rel)
	if err != nil {
		// A directory that vanished, became a symlink or crossed a mount is
		// skipped with a count. Treating it as "empty" would be a lie; treating
		// it as fatal would stop the whole scope for one bad entry.
		if errors.Is(err, unix.ENOENT) || errors.Is(err, fsops.ErrSymlink) || errors.Is(err, fsops.ErrCrossMount) {
			stats.Unreadable++
			return dirDone, nil
		}
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			stats.Unreadable++
			return dirFailed, nil
		}
		return dirFailed, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer dir.Close()

	dirID, _, err := dir.Stat()
	if err != nil {
		stats.Unreadable++
		return dirFailed, nil
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
		return dirFailed, err
	}

	var (
		obsBatch []store.Observation
		dirBatch [][]byte
		nowNS    = s.Clock.Wall().UnixNano()
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
				return dirFailed, ferr
			}
			stats.Unreadable++
			return dirFailed, nil
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

			id, token, err := dir.StatEntry(name)
			if err != nil {
				stats.Unreadable++
				continue
			}

			childRel := name
			if len(rel) > 0 {
				childRel = string(rel) + "/" + name
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
				return dirFailed, err
			}
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
		return dirFailed, err
	}
	return dirDone, nil
}

// InsertFailures counts unreadable entries per run; kept for reporting only.
func (s Stats) InsertFailures() int { return s.Unreadable }

// SanitizeDisplay is used by reports; kept here so the scanner owns the rule
// that display strings never round-trip into syscalls.
func SanitizeDisplay(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	return strings.ToValidUTF8(p, "\uFFFD")
}
