//go:build darwin

package fsops

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
)

// openDirChain walks parts from rootFD one component at a time with O_NOFOLLOW.
// Every intermediate directory is checked against rootDev, so a mount or a
// symlink cannot silently redirect the walk to another volume.
func openDirChain(rootFD int, parts []string, rootDev uint64) (int, error) {
	cur, err := unix.Dup(rootFD)
	if err != nil {
		return -1, fmt.Errorf("fsops: dup: %w", err)
	}
	for _, name := range parts {
		fd, err := unix.Openat(cur, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if refined := classifyDirOpenError(cur, name, err); refined != nil {
				unix.Close(cur)
				return -1, refined
			}
			unix.Close(cur)
			switch {
			case errors.Is(err, unix.ENOENT):
				return -1, fmt.Errorf("fsops: open component %q: %w", name, err)
			case errors.Is(err, unix.ELOOP):
				return -1, fmt.Errorf("%w: component %q", ErrSymlink, name)
			default:
				return -1, fmt.Errorf("fsops: open component %q: %w", name, err)
			}
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			unix.Close(cur)
			return -1, fmt.Errorf("fsops: stat component %q: %w", name, err)
		}
		if devOf(&st) != rootDev {
			unix.Close(fd)
			unix.Close(cur)
			return -1, fmt.Errorf("%w: component %q is on dev %d, root on %d", ErrCrossMount, name, devOf(&st), rootDev)
		}
		unix.Close(cur)
		cur = fd
	}
	return cur, nil
}

// renameNoReplace uses renameatx_np with RENAME_EXCL. On APFS an existing
// target is never replaced; there is no fallback to plain renameat.
func renameNoReplace(srcFD int, srcName string, dstFD int, dstName string) error {
	err := unix.RenameatxNp(srcFD, srcName, dstFD, dstName, unix.RENAME_EXCL)
	if err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			return ErrExists
		}
		return fmt.Errorf("fsops: renameatx_np(%q -> %q): %w", srcName, dstName, err)
	}
	return nil
}

// barrier asks the storage device to flush everything it holds. A plain fsync
// on macOS does not promise that; F_FULLFSYNC does, per the platform contract.
func barrier(anchorFD int, marker []byte) error {
	if _, err := unix.Pwrite(anchorFD, marker, 0); err != nil {
		return fmt.Errorf("fsops: write barrier marker: %w", err)
	}
	if _, err := unix.FcntlInt(uintptr(anchorFD), unix.F_FULLFSYNC, 0); err != nil {
		return fmt.Errorf("fsops: F_FULLFSYNC: %w", err)
	}
	return nil
}

func devOf(st *unix.Stat_t) uint64 { return uint64(uint32(st.Dev)) }

func kindOf(st *unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "regular"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFLNK:
		return "symlink"
	case unix.S_IFBLK:
		return "block_device"
	case unix.S_IFCHR:
		return "char_device"
	case unix.S_IFIFO:
		return "fifo"
	case unix.S_IFSOCK:
		return "socket"
	default:
		return "unknown"
	}
}

func identityOf(st *unix.Stat_t) domain.FileIdentity {
	dev := devOf(st)
	id := domain.FileIdentity{
		VolumeID: fmt.Sprintf("dev-%d", dev),
		Device:   dev,
		Inode:    st.Ino,
		Kind:     kindOf(st),
	}
	if st.Btim.Sec != 0 {
		v := st.Btim.Sec*int64(1e9) + int64(st.Btim.Nsec)
		id.BirthTimeNS = &v
	}
	if st.Gen != 0 {
		v := uint64(st.Gen)
		id.Generation = &v
	}
	return id
}

func tokenOf(st *unix.Stat_t, metadataHash domain.Digest) domain.ChangeToken {
	return domain.ChangeToken{
		Size:         st.Size,
		MTimeNS:      st.Mtim.Sec*int64(1e9) + int64(st.Mtim.Nsec),
		CTimeNS:      st.Ctim.Sec*int64(1e9) + int64(st.Ctim.Nsec),
		NLink:        uint64(st.Nlink),
		Mode:         uint32(st.Mode),
		UID:          st.Uid,
		GID:          st.Gid,
		Flags:        uint64(st.Flags),
		MetadataHash: metadataHash,
	}
}

func birthTimeNS(st *unix.Stat_t) (int64, bool) {
	if st.Btim.Sec == 0 {
		return 0, false
	}
	return st.Btim.Sec*int64(1e9) + int64(st.Btim.Nsec), true
}

func generationOf(st *unix.Stat_t) (uint64, bool) {
	if st.Gen == 0 {
		return 0, false
	}
	return uint64(st.Gen), true
}

// filesystemName returns the APFS/HFS type name reported by statfs.
func filesystemName(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b), nil
}

// bootOrRunID binds a runtime identity to the current boot session. macOS
// mount data has no stable mount ID, so the boot session UUID is the anchor.
func bootOrRunID() (string, error) {
	return unix.Sysctl("kern.bootsessionuuid")
}

// mountIDFor is unavailable on macOS; nil means "not available", not zero.
func mountIDFor(string) (*uint64, error) { return nil, nil }

// birthTimeOfFD re-reads the creation time of an open descriptor. macOS reports
// it in the ordinary stat structure, so no extra syscall is needed.
func birthTimeOfFD(fd int) (int64, bool) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, false
	}
	if st.Btim.Sec == 0 {
		return 0, false
	}
	return st.Btim.Sec*int64(1e9) + int64(st.Btim.Nsec), true
}

// mountIDOfFD has no macOS equivalent. Returning nil is honest: the runtime half
// of the identity keeps only the boot session UUID, and the comparison must not
// read "no mount id" as "same mount".
func mountIDOfFD(int, string) *uint64 { return nil }

// fileFlags reads the BSD file flags (uchg, hidden, compressed, ...). macOS carries
// them in the ordinary stat structure, so no extra syscall is needed.
func fileFlags(fd int) (uint64, string) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, "flags_unreadable"
	}
	return uint64(st.Flags), ""
}

// tokenFlags is the flags half of a change token for an open regular file.
func tokenFlags(fd int, st *unix.Stat_t) uint64 { return uint64(st.Flags) }

// syscallName keeps error text readable in reports without leaking numbers.
func syscallName(err error) string {
	if err == nil {
		return ""
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}
