package execute

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

// VerifyOptions carries the I/O policy for one item.
type VerifyOptions struct {
	Buffer     []byte
	Budget     *domain.Budget
	RatePerSec int64
}

// HashFile streams a file through SHA-256.
//
// The whole file is read: sampling is a negative filter for duplicate
// detection, never proof of equality. Reading twice costs budget twice, which is
// why the caller reserves before starting.
func HashFile(f *fsops.File, opt VerifyOptions) (domain.Digest, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, errs.Wrap(errs.CodeIOError, err)
	}
	buf := opt.Buffer
	if len(buf) == 0 {
		buf = make([]byte, 1<<20)
	}
	h := sha256.New()
	var total int64
	started := time.Now()

	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			if opt.Budget != nil {
				opt.Budget.AddRead(int64(n))
				if opt.Budget.ReadUp() {
					return "", total, errs.New(errs.CodeBudgetExhausted,
						"the read budget was spent before the file could be verified")
				}
			}
			sleepUntilRate(total, started, opt.RatePerSec)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", total, errs.Wrap(errs.CodeIOError, err)
		}
		if n == 0 {
			break
		}
	}
	return domain.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), total, nil
}

// CompareFiles streams two files and reports whether their bytes are identical.
//
// A matching hash is not enough on its own: a hash collision would otherwise be
// allowed to delete a distinct file. This is the step that makes "same content"
// a fact rather than a probability.
func CompareFiles(a, b *fsops.File, opt VerifyOptions) (bool, error) {
	if _, err := a.Seek(0, io.SeekStart); err != nil {
		return false, errs.Wrap(errs.CodeIOError, err)
	}
	if _, err := b.Seek(0, io.SeekStart); err != nil {
		return false, errs.Wrap(errs.CodeIOError, err)
	}
	buf := opt.Buffer
	if len(buf) == 0 {
		buf = make([]byte, 1<<20)
	}
	bufB := make([]byte, len(buf))
	started := time.Now()
	var total int64

	for {
		na, erra := a.Read(buf)
		nb, errb := b.Read(bufB)
		if na != nb {
			return false, nil
		}
		if na > 0 {
			for i := 0; i < na; i++ {
				if buf[i] != bufB[i] {
					return false, nil
				}
			}
			total += int64(na) * 2
			if opt.Budget != nil {
				opt.Budget.AddRead(int64(na) * 2)
				if opt.Budget.ReadUp() {
					return false, errs.New(errs.CodeBudgetExhausted,
						"the read budget was spent before the comparison finished")
				}
			}
			sleepUntilRate(total, started, opt.RatePerSec)
		}
		endA := errors.Is(erra, io.EOF)
		endB := errors.Is(errb, io.EOF)
		if endA && endB {
			return true, nil
		}
		if erra != nil && !endA {
			return false, errs.Wrap(errs.CodeIOError, erra)
		}
		if errb != nil && !endB {
			return false, errs.Wrap(errs.CodeIOError, errb)
		}
		if endA != endB {
			return false, nil
		}
	}
}

// MetadataOf reads the extended-metadata evidence for an open file.
func MetadataOf(f *fsops.File) (fsops.MetadataEvidence, error) {
	ev, err := fsops.ReadMetadata(f.FD())
	if err != nil {
		return ev, errs.Wrap(errs.CodeIOError, err)
	}
	return ev, nil
}

// MetadataDigest hashes the evidence that must match between two copies.
func MetadataDigest(ev fsops.MetadataEvidence, token domain.ChangeToken) domain.Digest {
	payload := fmt.Sprintf("xattr=%s;count=%d;flags=%d;mode=%d;uid=%d;gid=%d;mtime=%d",
		ev.XattrDigest, ev.XattrCount, token.Flags, token.Mode, token.UID, token.GID, token.MTimeNS)
	return domain.DigestOf("dustkeeper/metadata/v1", []byte(payload))
}

// sameIdentityAndToken verifies that the object behind an open handle is still
// the one the plan recorded.
//
// ctime is deliberately not compared: a rename changes it. atime is not
// compared at all: reading the file changes it.
func sameIdentityAndToken(now domain.FileIdentity, tok domain.ChangeToken,
	wantID domain.FileIdentity, wantTok domain.ChangeToken) (bool, string) {

	if !now.Equal(wantID) {
		return false, "object identity changed"
	}
	if now.Kind != "regular" {
		return false, "object is no longer a regular file"
	}
	if tok.Size != wantTok.Size {
		return false, "size changed"
	}
	if tok.MTimeNS != wantTok.MTimeNS {
		return false, "modification time changed"
	}
	if tok.NLink != wantTok.NLink {
		return false, "link count changed"
	}
	if tok.Mode != wantTok.Mode {
		return false, "permissions changed"
	}
	if tok.UID != wantTok.UID || tok.GID != wantTok.GID {
		return false, "ownership changed"
	}
	return true, ""
}

// checkNoWriterChange re-reads identity after content work. Content reads
// advance atime only, which is why it is not part of the comparison.
func checkNoWriterChange(f *fsops.File, wantID domain.FileIdentity, wantTok domain.ChangeToken) error {
	nowID, nowTok, err := f.Stat()
	if err != nil {
		return errs.Wrap(errs.CodeIOError, err)
	}
	if ok, why := sameIdentityAndToken(nowID, nowTok, wantID, wantTok); !ok {
		return errs.Newf(errs.CodePlanStale, "the file changed while it was being read: %s", why)
	}
	return nil
}

// SyncAfterMove makes the namespace change durable: the object's data, then both
// parent directories, then a full-device barrier on macOS.
func SyncAfterMove(filefd int, srcDir, dstDir fsops.DirFD, anchorFD int, marker string) error {
	if err := fsops.DurableFile(filefd); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := fsops.DurableDir(srcDir); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := fsops.DurableDir(dstDir); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if anchorFD >= 0 {
		if err := fsops.Barrier(anchorFD, []byte(marker)); err != nil {
			return errs.Wrap(errs.CodeDurabilityFailed, err)
		}
	}
	return nil
}
