package execute

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/store"
)

// ReviewExpiry is the pre-flight a due object passes before it is allowed to
// spend part of a run's purge allowance.
//
// It answers one question - "is there anything about this object that should stop
// the clearing before it costs anything" - and it answers it cheaply: no content
// is read and no model is asked. The steps are the ones the product promises the
// user before anything irreversible happens to a file that was moved for them:
//
//	still in the observation area, not restored
//	not protected since it was moved
//	no new use signal at the original path
//	the object in the vault is still the object that was moved
//	the evidence recorded at the move is complete
//
// It is not the gate. Purge re-checks all of this immediately before the unlink,
// because a pre-flight that ran a minute ago is not a permission. This exists so
// that an object which cannot be cleared is reported as skipped - with a reason -
// instead of failing an item inside the batch and consuming its allowance.
//
// A non-empty string means "skip, and this is why". An error means the check
// itself could not be performed, which is not a verdict about the object.
func (e *Executor) ReviewExpiry(ctx context.Context, sc *ScopeCtx, it store.Item) (string, error) {
	if it.State != domain.StateQuarantined {
		// Restored, held, or settled elsewhere: the observation area no longer
		// holds this object, so there is nothing to clear.
		return "not_in_observation_area: the object is " + it.State + ", not quarantined", nil
	}
	if it.PurgeNotBeforeNS == nil || it.QuarantinedAtNS == nil || it.BoundRetentionSeconds <= 0 {
		return "retention_unknown: the recorded observation window cannot be read, so it cannot have elapsed", nil
	}
	if it.CapturedIdentityJSON == nil || it.CapturedTokenJSON == nil ||
		it.ContentHash == nil || *it.ContentHash == "" ||
		it.MetadataHash == nil || *it.MetadataHash == "" {
		return "evidence_incomplete: the object has no complete post-move evidence; reconcile it first", nil
	}

	// 1. Protected now. The check is against the recorded original path, not
	// against a path that currently exists: a protected slot is protected even
	// while it holds nothing.
	blocked, why, err := e.purgeBlockedByProtection(ctx, sc, it)
	if err != nil {
		return "", err
	}
	if blocked {
		return "protected: " + why, nil
	}

	// 2. A new use signal: something was put back at the original path. That is
	// the user working in the same place again, and it is exactly the evidence
	// that cannot be obtained any other way.
	reused, err := e.originalPathReused(sc, it)
	if err != nil {
		return "", err
	}
	if reused {
		return "path_reused: a file exists at the original path again, so the place is in use", nil
	}

	// 3. The vault object must still be the object that was moved. A capture whose
	// identity no longer matches cannot be described, and a description is what a
	// permanent deletion has to be able to give.
	captured, err := decodeID(*it.CapturedIdentityJSON)
	if err != nil {
		return "", err
	}
	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return "object_missing: the vault holds no object under this name", nil
		}
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return "object_missing: the vault holds no object under this name", nil
		}
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer obj.Close()
	if !obj.Identity.Equal(captured) {
		return "object_changed: the vault object is not the object that was moved", nil
	}
	if obj.Token.Size < 0 {
		return "object_unreadable: the vault object cannot be described", nil
	}
	return "", nil
}

// originalPathReused reports whether something now occupies the path the object
// was moved away from.
//
// A directory that cannot be inspected is not an answer either way, so the error
// is returned rather than being read as "not reused": the caller turns it into a
// failed check, not into a permission.
func (e *Executor) originalPathReused(sc *ScopeCtx, it store.Item) (bool, error) {
	parent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer parent.Close()
	exists, err := parent.Exists()
	if err != nil {
		return false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	return exists, nil
}
