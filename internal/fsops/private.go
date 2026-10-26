package fsops

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
)

// ErrNotPrivate reports that an existing management object does not satisfy the
// private-object contract and must not be used for business actions.
var ErrNotPrivate = errors.New("fsops: management object is not private")

// OwnerAny is the explicit "any owner" value. A zero OwnerUID means root.
const OwnerAny = ^uint32(0)

// PrivateExpect is the contract every object inside a private management
// directory has to satisfy before it may carry business meaning.
//
// R01 is deliberately not about defending against an attacker who keeps
// replacing files with the same uid. It is about refusing to act when the
// management directory is already wrong: left over from an aborted migration,
// restored from a backup, or - the two concrete cases in the review - holding a
// pre-created temporary symlink or a durability anchor that is a hard link to a
// business file. Those are all detectable before any business change, and the
// answer is to refuse rather than to "repair" silently.
type PrivateExpect struct {
	// OwnerUID is the only uid allowed to own the object, compared exactly.
	//
	// Zero is not "unset": it is root. Treating it as unset meant that a
	// management object chowned to another uid was accepted whenever Dustkeeper
	// ran as root, which is the configuration where it matters most. Callers that
	// legitimately have no opinion pass OwnerAny instead of zero.
	OwnerUID uint32
	// MaxPerm is the widest permission accepted. Any bit outside it means the
	// object is reachable by someone other than its owner.
	MaxPerm uint32
	// Links is the required link count. 1 refuses every hard link, which is the
	// anchor-hardlink case: a barrier on a hard link issues F_FULLFSYNC and a
	// Pwrite against the business file's own bytes.
	Links uint64
	// RootDev, when non-zero, requires the object to live on that device.
	RootDev uint64
}

// VerifyToken applies the contract to stat facts already in hand. It is exported
// so that recovery can re-check a manifest without re-opening anything.
func (e PrivateExpect) VerifyToken(what string, tok domain.ChangeToken, dev uint64) error {
	if e.RootDev != 0 && dev != e.RootDev {
		return fmt.Errorf("%w: %s is on device %d, want %d", ErrNotPrivate, what, dev, e.RootDev)
	}
	if e.OwnerUID != OwnerAny && tok.UID != e.OwnerUID {
		return fmt.Errorf("%w: %s is owned by uid %d, want %d", ErrNotPrivate, what, tok.UID, e.OwnerUID)
	}
	// Mode carries the file-type bits on both platforms; only the permission
	// half, including setuid/setgid/sticky, is part of the contract.
	if e.MaxPerm != 0 {
		if wider := tok.Mode & 0o7777 &^ e.MaxPerm; wider != 0 {
			return fmt.Errorf("%w: %s has mode %04o with bits %04o outside %04o",
				ErrNotPrivate, what, tok.Mode&0o7777, wider, e.MaxPerm)
		}
	}
	if e.Links != 0 && tok.NLink != e.Links {
		return fmt.Errorf("%w: %s has %d links, want %d", ErrNotPrivate, what, tok.NLink, e.Links)
	}
	return nil
}

// VerifyPrivate checks an already-open directory against the contract.
//
// It is used for the vault directory itself and for objects, manifests and tmp:
// a management directory that is group-writable, owned by another uid, or
// reachable through a second name is not a place where an intent record can be
// trusted.
func (d *Dir) VerifyPrivate(expect PrivateExpect) error {
	id, tok, err := d.Stat()
	if err != nil {
		return err
	}
	if id.Kind != "directory" {
		return fmt.Errorf("%w: %s is a %s, want a directory", ErrNotPrivate, d.rel, id.Kind)
	}
	return expect.VerifyToken(d.rel, tok, id.Device)
}

// VerifyPrivate checks an already-open regular file against the contract.
//
// OpenFileRW only promises "regular and no symlink". That is not enough for the
// durability anchor: a hard link to a business file satisfies both, and the
// barrier would then write into that business file.
func (f *File) VerifyPrivate(name string, expect PrivateExpect) error {
	id, tok, err := f.Stat()
	if err != nil {
		return err
	}
	if id.Kind != "regular" {
		return fmt.Errorf("%w: %s is a %s, want a regular file", ErrNotPrivate, name, id.Kind)
	}
	return expect.VerifyToken(name, tok, id.Device)
}

// StatEntryPrivate returns the identity and token of one entry and applies the
// contract without opening it.
func (d *Dir) StatEntryPrivate(name string, expect PrivateExpect) (domain.FileIdentity, domain.ChangeToken, error) {
	id, tok, err := d.StatEntry(name)
	if err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, err
	}
	if err := expect.VerifyToken(name, tok, id.Device); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, err
	}
	return id, tok, nil
}

// InstallPrivate creates dst/name with an unpredictable temporary name inside
// tmp and a no-replace rename.
//
// The temporary file name is random because a fixed "<target>.tmp" is a
// predictable target: the review's first R01 scenario is exactly a pre-placed
// owner.json.tmp symlink that an O_CREAT|O_TRUNC open would follow and truncate.
// CreateExclusive uses O_EXCL|O_NOFOLLOW, and the no-replace rename means an
// existing target is reported as ErrExists instead of being overwritten.
//
// created reports whether the target was installed; when it is false the caller
// still owns the decision about the object that is already there.
func InstallPrivate(tmp DirFD, dst DirFD, name string, body []byte, perm uint32) (created bool, err error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	tmpName := ".dk-" + domain.NewID("install")

	f, err := CreateExclusive(tmp, tmpName, perm)
	if err != nil {
		return false, err
	}
	discard := func() {
		f.Close()
		_ = unix.Unlinkat(tmp.DirFD(), tmpName, 0)
	}
	if len(body) > 0 {
		if _, werr := f.Write(body); werr != nil {
			discard()
			return false, fmt.Errorf("fsops: write %q: %w", name, werr)
		}
	}
	if serr := DurableFile(f.FD()); serr != nil {
		discard()
		return false, serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = unix.Unlinkat(tmp.DirFD(), tmpName, 0)
		return false, cerr
	}

	if rerr := RenameNoReplace(tmp, tmpName, dst, name); rerr != nil {
		_ = unix.Unlinkat(tmp.DirFD(), tmpName, 0)
		return false, rerr
	}
	// The entry is in place. A failed directory sync is reported, and the caller
	// must not treat the install as durable.
	if derr := DurableDir(dst); derr != nil {
		return true, derr
	}
	return true, nil
}

// LockFile opens (creating if absent) a lock file inside d and takes an
// exclusive non-blocking flock on it. It never truncates, never follows a link,
// and never leaves the descriptor behind: closing the File releases the lock.
//
// The lock lives in the private management directory rather than in a state
// directory because that is what makes it span state directories. R02 found
// ScopeLock defined but never called, so two installations with copies of the
// same database could both act on one tree; a lock inside the tree is the only
// one that can see both of them.
func (d *Dir) LockFile(name string) (*File, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(d.fd, name,
		unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %q", ErrSymlink, name)
		}
		return nil, fmt.Errorf("fsops: open lock %q: %w", name, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrExists
		}
		return nil, fmt.Errorf("fsops: flock %q: %w", name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fsops: stat %q: %w", name, err)
	}
	if kindOf(&st) != "regular" {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %q is %s", ErrNotRegular, name, kindOf(&st))
	}
	return &File{f: os.NewFile(uintptr(fd), name), fd: fd, Identity: identityOf(&st), Token: tokenOf(&st, "")}, nil
}

// DeviceOf returns the device of an open file handle.
func (f *File) Device() (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(f.fd, &st); err != nil {
		return 0, fmt.Errorf("fsops: stat file: %w", err)
	}
	return devOf(&st), nil
}

// SameDeviceAs reports whether the file and the directory live on the same
// device. The durability anchor is only meaningful when it shares the volume of
// the objects it is supposed to flush.
func (f *File) SameDeviceAs(dir DirFD) (bool, error) {
	fd, err := f.Device()
	if err != nil {
		return false, err
	}
	dd, err := deviceOfFD(dir.DirFD())
	if err != nil {
		return false, err
	}
	return fd == dd, nil
}

func deviceOfFD(fd int) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, fmt.Errorf("fsops: stat descriptor: %w", err)
	}
	return devOf(&st), nil
}

// WriteTag writes a small identifying value at a fixed offset inside an open
// private file.
//
// The durability anchor cannot carry its identity at offset zero: every barrier
// overwrites that area with a fresh marker. A tag further in survives, and a
// replaced anchor - the case R01 accepts today, because a recreated file can be
// handed the same inode number - has nothing at that offset.
func WriteTag(fd int, offset int64, tag []byte) error {
	if _, err := unix.Pwrite(fd, tag, offset); err != nil {
		return fmt.Errorf("fsops: write tag at %d: %w", offset, err)
	}
	return nil
}

// ReadTag reads a bounded tag from a fixed offset inside an open private file.
func ReadTag(fd int, offset int64, limit int) ([]byte, error) {
	buf := make([]byte, limit)
	n, err := unix.Pread(fd, buf, offset)
	if err != nil {
		return nil, fmt.Errorf("fsops: read tag at %d: %w", offset, err)
	}
	return buf[:n], nil
}

// ReadFileAt reads one small regular file inside d through a descriptor opened
// with O_NOFOLLOW.
//
// It exists because reading a management object by path would re-resolve the
// name after it was validated, which is the window R01 is about: the file that
// passed the contract and the file that gets read would not be the same object.
func (d *Dir) ReadFileAt(name string, limit int64) ([]byte, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	// O_NONBLOCK is what keeps a mis-typed management object from hanging the
	// process: opening a FIFO for reading waits for a writer, and the wait happens
	// before any type check could refuse it. Non-blocking open returns at once,
	// and for a regular file the flag has no effect on reads.
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %q", ErrSymlink, name)
		}
		return nil, fmt.Errorf("fsops: open %q: %w", name, err)
	}
	f := &File{f: os.NewFile(uintptr(fd), name), fd: fd}
	defer f.Close()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("fsops: stat %q: %w", name, err)
	}
	if kindOf(&st) != "regular" {
		return nil, fmt.Errorf("%w: %q is %s", ErrNotRegular, name, kindOf(&st))
	}
	if st.Size > limit {
		return nil, fmt.Errorf("fsops: %q is %d bytes, over the %d byte limit", name, st.Size, limit)
	}
	buf := make([]byte, st.Size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("fsops: read %q: %w", name, err)
	}
	return buf, nil
}
