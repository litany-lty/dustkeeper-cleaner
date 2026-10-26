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
type Stats struct {
	Dirs           int   `json:"dirs"`
	Files          int   `json:"files"`
	Bytes          int64 `json:"bytes"`
	ProtectedDirs  int   `json:"protected_dirs"`
	SkippedEntries int   `json:"skipped_entries"`
	Unreadable     int   `json:"unreadable"`
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

// Run processes directories until the queue is empty or the budget is spent.
//
// The caller starts the epoch and, after a completed Run with no pending work,
// retires unseen entries. Calling that retire step from a partial scan would
// turn "not scanned yet" into "deleted".
func (s *Scanner) Run(ctx context.Context, vs scope.VerifiedScope, epoch int64, budget *domain.Budget) (Stats, error) {
	var stats Stats
	rootDev := vs.RootID.Runtime.Device

	for {
		if err := ctx.Err(); err != nil {
			return stats, errs.New(errs.CodeCancelled, "scan cancelled")
		}
		if budget.TimeUp(s.Clock) || budget.ReadUp() {
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
			if err := s.scanDir(ctx, vs, epoch, rootDev, rel, budget, &stats); err != nil {
				return stats, err
			}
			if err := s.Store.MarkDirDone(ctx, vs.Scope.ID, epoch, rel); err != nil {
				return stats, err
			}
			stats.Dirs++
			if budget.TimeUp(s.Clock) {
				return stats, nil
			}
		}
	}
}

func (s *Scanner) scanDir(ctx context.Context, vs scope.VerifiedScope, epoch int64, rootDev uint64,
	rel []byte, budget *domain.Budget, stats *Stats) error {

	dir, err := vs.Root.OpenDir(rel)
	if err != nil {
		// A directory that vanished, became a symlink or crossed a mount is
		// skipped with a count. Treating it as "empty" would be a lie; treating
		// it as fatal would stop the whole scope for one bad entry.
		if errors.Is(err, unix.ENOENT) || errors.Is(err, fsops.ErrSymlink) || errors.Is(err, fsops.ErrCrossMount) {
			stats.Unreadable++
			return nil
		}
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer dir.Close()

	dirID, _, err := dir.Stat()
	if err != nil {
		stats.Unreadable++
		return nil
	}
	if ok, _ := s.Guard.CheckSubtree(string(rel), dirID); ok {
		stats.ProtectedDirs++
		return nil
	}

	parentIdentityJSON, err := jsonIdentity(dirID)
	if err != nil {
		return err
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
			// End of directory.
			break
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
				return err
			}
		}
		if budget.TimeUp(s.Clock) {
			break
		}
	}

	return flush()
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
