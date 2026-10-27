package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/policy"
)

// projectMarkers are the entries that mark a directory tree as a project.
//
// They are recognised at any ancestor, which is what makes "repo/src/main.go"
// safe: the file itself has no marker, and the metadata-directory rule can only
// protect ".git/...", not its siblings.
var projectMarkers = []string{".git", ".hg", ".svn"}

// projectProtected reports whether rel lives inside a project tree.
//
// A `.git` file (a worktree pointer) counts as a marker exactly like a `.git`
// directory, because the tree it describes is just as much someone's work.
//
// This walks the ancestor chain with verified descriptors instead of the display
// path, so a renamed or symlinked ancestor cannot hide a project.
func (e *Executor) projectProtected(sc *ScopeCtx, rel []byte) (bool, string, error) {
	parts, err := fsops.SplitRelative(rel)
	if err != nil {
		return false, "", nil
	}
	// Root (i=0) through the immediate parent (i=len(parts)-1). The file itself
	// is not a directory and cannot carry a marker that matters here.
	for i := 0; i <= len(parts)-1; i++ {
		var prefix []byte
		if i > 0 {
			prefix = []byte(strings.Join(parts[:i], "/"))
		}
		d, oerr := sc.Root.OpenDir(prefix)
		if oerr != nil {
			if errors.Is(oerr, unix.ENOENT) {
				continue
			}
			return false, "", errs.Wrap(errs.CodeAccessDenied, oerr)
		}
		var found string
		for _, m := range projectMarkers {
			if _, _, serr := d.StatEntry(m); serr == nil {
				found = m
				break
			}
		}
		d.Close()
		if found != "" {
			where := "the scope root"
			if i > 0 {
				where = fmt.Sprintf("%q", strings.Join(parts[:i], "/"))
			}
			return true, fmt.Sprintf("%s is a %s project tree", where, found), nil
		}
	}
	return false, "", nil
}

// protected reports whether this object must not be touched, using the one
// shared answer plus the two checks that need file descriptors.
func (e *Executor) protected(ctx context.Context, sc *ScopeCtx, rel []byte, id domain.FileIdentity) (bool, string, error) {
	if ok, why := policy.GlobalProtected(string(rel)); ok {
		return true, "global protection: " + why, nil
	}
	ancestors, err := ancestorIdentities(sc, rel)
	if err != nil {
		// A protected subtree whose ancestor chain cannot be walked is not evidence
		// that the object is unprotected. Saying so would turn a permission problem
		// into a quarantine.
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the directories above %q cannot all be resolved, so subtree protections cannot be compared by identity: %v", rel, err)
	}
	if ok, why := e.Guard.CheckFile(string(rel), id, ancestors...); ok {
		return true, why, nil
	}
	return e.projectProtected(sc, rel)
}

// ancestorIdentities returns the identity of every directory above rel, nearest
// first, read through verified descriptors.
//
// Subtree protections are recorded as paths, and a path is a spelling: on a
// filesystem that treats "Folder" and "folder" as one directory, comparing
// strings lets a protected subtree through. Comparing the identity of each
// ancestor closes that, and it needs no case folding of its own - the kernel
// already decided that "Folder/a.zip" lives inside "folder".
func ancestorIdentities(sc *ScopeCtx, rel []byte) ([]domain.FileIdentity, error) {
	parts, err := fsops.SplitRelative(rel)
	if err != nil {
		return nil, nil
	}
	out := make([]domain.FileIdentity, 0, len(parts))
	for k := len(parts) - 1; k >= 0; k-- {
		d, oerr := sc.Root.OpenDir([]byte(strings.Join(parts[:k], "/")))
		if oerr != nil {
			if errors.Is(oerr, unix.ENOENT) {
				continue
			}
			return nil, oerr
		}
		id, _, serr := d.Stat()
		d.Close()
		if serr != nil {
			return nil, serr
		}
		out = append(out, id)
	}
	return out, nil
}

// scopeGate refuses a new business operation on a scope that is no longer active.
//
// A pause that only changes a status column is not a pause. The check reads the
// status again for every item, because the plan may have been created before the
// pause and the batch may be long.
//
// Restore deliberately does not call this: recovering a file is not a new
// business action, and freezing the product's own repair path would be a way to
// lose data rather than protect it.
func (e *Executor) scopeGate(ctx context.Context, sc *ScopeCtx) error {
	cur, err := e.Store.GetScope(ctx, sc.Scope.ID)
	if err != nil {
		return err
	}
	switch cur.Status {
	case domain.ScopeActive:
		return nil
	case domain.ScopePaused:
		return errs.Newf(errs.CodePaused,
			"scope %s is paused; no new operation starts until it is resumed", sc.Scope.ID)
	case domain.ScopeHeld:
		return errs.Newf(errs.CodeHeld,
			"scope %s is held; resolve the held objects before new operations", sc.Scope.ID)
	default:
		return errs.Newf(errs.CodePlanStale, "scope %s is %s", sc.Scope.ID, cur.Status)
	}
}

// controlOf maps a stop condition onto the control axis so the caller does not
// have to infer "may I continue" from a nil error.
func controlOf(code string) string {
	switch code {
	case string(errs.CodePaused), string(errs.CodeHeld), string(errs.CodeScopeChanged),
		string(errs.CodeProtectionAliasUnresolved):
		return domain.ControlStopScope
	case string(errs.CodePlanStale), string(errs.CodePlanExpired), string(errs.CodeDigestMismatch):
		return domain.ControlStopPlan
	case string(errs.CodeDurabilityFailed), string(errs.CodeIOError),
		string(errs.CodeStateWriteFailed), string(errs.CodeDBError),
		string(errs.CodeClockUntrusted), string(errs.CodeInstanceBusy):
		return domain.ControlStopRun
	}
	return domain.ControlContinue
}
