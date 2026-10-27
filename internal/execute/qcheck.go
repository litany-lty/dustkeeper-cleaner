package execute

import (
	"context"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// requireQuarantineTrust evaluates the clock assertion for one new quarantine.
//
// The run and the manual apply both pass the same clocktrust guard, so the two
// entry points answer the question with the same code. A nil function is not
// "trusted": it is the absence of an assertion, and an object quarantined under a
// clock nobody checked starts a retention window that cannot be defended.
func (e *Executor) requireQuarantineTrust(ctx context.Context) error {
	if e.QuarantineTrust == nil {
		return errs.New(errs.CodeClockUntrusted,
			"no clock assertion was supplied; a new quarantine requires one on every entry point")
	}
	return e.QuarantineTrust(ctx)
}

// checkQuarantineSpace refuses to move bytes onto a volume that has reached the
// configured floor.
//
// The runner already refuses to start a quarantine during disk pressure, but that
// check runs once, before the scan, and the volumes it inspects are the scope
// roots. A quarantine writes into the vault, so the question is asked here, of
// the objects directory's own descriptor: same volume by construction, and every
// entry point asks it - a manual apply never goes through the runner.
func (e *Executor) checkQuarantineSpace(sc *ScopeCtx) error {
	if e.Config.MinFreeBytes <= 0 && e.Config.MinFreePercent <= 0 {
		return nil
	}
	if sc.Objects == nil {
		return errs.Newf(errs.CodeScopeChanged,
			"scope %s has no object directory, so the space available cannot be established", sc.Scope.ID)
	}
	free, total, err := statfsFD(sc.Objects.DirFD())
	if err != nil {
		return errs.Wrap(errs.CodeIOError, err)
	}
	if e.Config.MinFreeBytes > 0 && free < uint64(e.Config.MinFreeBytes) {
		return errs.Newf(errs.CodeDiskPressure,
			"free space on the volume holding scope %s is %d bytes, below the configured floor of %d; nothing was moved",
			sc.Scope.ID, free, e.Config.MinFreeBytes)
	}
	if e.Config.MinFreePercent > 0 && total > 0 && free*100/total < uint64(e.Config.MinFreePercent) {
		return errs.Newf(errs.CodeDiskPressure,
			"free space on the volume holding scope %s is %d%%, below the configured floor of %d%%; nothing was moved",
			sc.Scope.ID, free*100/total, e.Config.MinFreePercent)
	}
	return nil
}

// abandonPreparation undoes the only durable thing a refused quarantine wrote:
// its own manifest. The payload was never touched, so there is nothing else to
// undo, and the next attempt is not blocked by a leftover preparation record.
//
// A removal that fails is reported together with the original reason rather than
// swallowed: the operator needs to know that a manifest is sitting in the way, and
// a silent ignore would turn "cannot retry" into "retries and does nothing".
func (e *Executor) abandonPreparation(sc *ScopeCtx, it store.Item, cause error) error {
	if err := sc.RemoveManifest(string(it.ID)); err != nil {
		if cause != nil {
			return errs.Newf(errs.CodeStateWriteFailed,
				"%v; the preparation manifest for %s could not be removed (%v), so it was left in place",
				cause, it.ID, err)
		}
		return errs.Newf(errs.CodeStateWriteFailed,
			"the preparation manifest for %s could not be removed: %v; it was left in place", it.ID, err)
	}
	return cause
}

// statfsFD reports free and total bytes for the volume behind an open descriptor.
func statfsFD(fd int) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}
