package execute

import (
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

// ErrUnstableObject means the object changed between the start and the end of a
// content read. It is not a reason to retry blindly: whatever the caller was
// about to do with those bytes is based on a file that no longer exists in that
// form.
var ErrUnstableObject = errors.New("execute: object changed while it was being read")

// ObjectEvidence is the proof collected for one object in one pass.
//
// Section 9.4 asks for four things and this struct is the only place they are
// assembled. Collecting them separately in each caller is how a purge ended up
// without a post-read check: the quarantine path had it, the purge path did not.
type ObjectEvidence struct {
	Identity domain.FileIdentity
	Token    domain.ChangeToken
	Content  domain.Digest
	Metadata domain.Digest
}

// sameStableObject compares the facts that must hold for an object that has not
// been renamed by us: identity, size, mtime, ctime, link count, mode and
// ownership.
//
// ctime is in this comparison and deliberately out of sameIdentityAndToken. The
// case it closes is a writer that rewrites a file and puts the modification time
// back: mtime and size then look untouched, while the kernel's ctime - the record
// of the last inode change - cannot be restored by an unprivileged writer. The
// other half is why it cannot simply be added to the shared comparison: our own
// rename bumps ctime, so a restore or a purge of an object we moved would fail
// against the token recorded before the move.
//
// A token that carries no ctime (a record written by an older build) is compared
// on the other fields only; "not recorded" is not evidence of a change.
func sameStableObject(now domain.FileIdentity, tok domain.ChangeToken,
	wantID domain.FileIdentity, wantTok domain.ChangeToken) (bool, string) {

	if ok, why := sameIdentityAndToken(now, tok, wantID, wantTok); !ok {
		return false, why
	}
	if wantTok.CTimeNS != 0 && tok.CTimeNS != wantTok.CTimeNS {
		return false, "change time changed"
	}
	return true, ""
}

// ReadStable reads an object and proves it did not change underneath the read.
//
// The order is load-bearing:
//
//	stat before -> metadata before -> hash -> stat after -> metadata after
//
// A hash alone is not evidence about the file. A writer that modifies a block
// after the reader already consumed it produces a digest that matches the old
// content while the file on disk no longer does; the trailing stat and metadata
// comparison is what notices.
func ReadStable(f *fsops.File, opt VerifyOptions) (ObjectEvidence, error) {
	var ev ObjectEvidence

	before, tokBefore, err := f.Stat()
	if err != nil {
		return ev, errs.Wrap(errs.CodeIOError, err)
	}
	if before.Kind != "regular" {
		return ev, errs.Newf(errs.CodeHeld, "object is %s, not a regular file", before.Kind)
	}
	metaBefore, err := MetadataOf(f)
	if err != nil {
		return ev, err
	}
	if metaBefore.Unsupported != "" {
		return ev, errs.Newf(errs.CodeUnsupportedMetadata,
			"metadata cannot be established safely: %s", metaBefore.Unsupported)
	}

	hash, _, err := HashFile(f, opt)
	if err != nil {
		return ev, err
	}

	after, tokAfter, err := f.Stat()
	if err != nil {
		return ev, errs.Wrap(errs.CodeIOError, err)
	}
	if ok, why := sameStableObject(after, tokAfter, before, tokBefore); !ok {
		return ev, errs.Wrapf(errs.CodeHeld, ErrUnstableObject, "%s", why)
	}
	metaAfter, err := MetadataOf(f)
	if err != nil {
		return ev, err
	}
	if metaAfter.Unsupported != "" {
		return ev, errs.Newf(errs.CodeUnsupportedMetadata,
			"metadata cannot be re-established safely: %s", metaAfter.Unsupported)
	}
	if MetadataDigest(metaBefore, tokBefore) != MetadataDigest(metaAfter, tokAfter) {
		return ev, errs.Wrapf(errs.CodeHeld, ErrUnstableObject, "extended metadata changed")
	}

	return ObjectEvidence{
		Identity: after,
		Token:    tokAfter,
		Content:  hash,
		Metadata: MetadataDigest(metaAfter, tokAfter),
	}, nil
}

// ConfirmUnchanged re-reads identity right before the operation commits.
//
// It narrows the window between "we verified this object" and "we acted on this
// object". It cannot close that window — rename and unlink have no "only if
// still inode X" parameter — and the design says so rather than pretending
// otherwise.
func ConfirmUnchanged(f *fsops.File, want ObjectEvidence) error {
	id, tok, err := f.Stat()
	if err != nil {
		return errs.Wrap(errs.CodeIOError, err)
	}
	// The object has not been renamed between the read and this call, so the
	// stable comparison applies: a writer that restored mtime still moved ctime.
	if ok, why := sameStableObject(id, tok, want.Identity, want.Token); !ok {
		return errs.Newf(errs.CodeHeld, "the object changed after verification: %s", why)
	}
	return nil
}

// EvidenceMatches compares collected evidence with what was recorded earlier.
//
// wantContent and wantMetadata may be empty for a lifecycle object whose
// metadata hash was not recorded; an empty recorded value is not a reason to
// skip the check on the freshly computed one.
func EvidenceMatches(ev ObjectEvidence, wantIdentity domain.FileIdentity, wantToken domain.ChangeToken,
	wantContent, wantMetadata string) error {

	if !wantIdentity.IsZero() {
		if !ev.Identity.Equal(wantIdentity) {
			return errs.New(errs.CodeHeld, "the object is not the recorded object")
		}
	}
	if ok, why := sameIdentityAndToken(ev.Identity, ev.Token, wantIdentity, wantToken); !ok && !wantIdentity.IsZero() {
		return errs.Newf(errs.CodeHeld, "the object no longer matches its recorded state: %s", why)
	}
	if wantContent != "" && string(ev.Content) != wantContent {
		return errs.New(errs.CodeHeld, "the object's content no longer matches its recorded hash")
	}
	if wantMetadata != "" && string(ev.Metadata) != wantMetadata {
		return errs.New(errs.CodeHeld, "the object's metadata no longer matches its recorded state")
	}
	return nil
}
