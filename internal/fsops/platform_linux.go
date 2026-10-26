//go:build linux

package fsops

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
)

// renameNoReplaceFlag is RENAME_NOREPLACE from linux/fs.h. It is spelled here
// because the syscall is issued directly; x/sys does not wrap renameat2.
const renameNoReplaceFlag = 1

// openDirChain resolves the whole parent path in one openat2 call, so that
// RESOLVE_BENEATH, RESOLVE_NO_SYMLINKS, RESOLVE_NO_MAGICLINKS and
// RESOLVE_NO_XDEV are enforced by the kernel during resolution rather than by
// our own walk.
func openDirChain(rootFD int, parts []string, rootDev uint64) (int, error) {
	if len(parts) == 0 {
		fd, err := unix.Dup(rootFD)
		if err != nil {
			return -1, fmt.Errorf("fsops: dup: %w", err)
		}
		return fd, nil
	}
	rel := strings.Join(parts, "/")
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	fd, err := unix.Openat2(rootFD, rel, how)
	if err != nil {
		if refined := classifyDirOpenError(rootFD, parts[0], err); refined != nil && errors.Is(err, unix.ELOOP) {
			return -1, refined
		}
		switch {
		case errors.Is(err, unix.ENOENT):
			return -1, fmt.Errorf("fsops: open %q: %w", rel, err)
		case errors.Is(err, unix.ELOOP):
			return -1, fmt.Errorf("%w: %q", ErrSymlink, rel)
		case errors.Is(err, unix.EXDEV):
			return -1, fmt.Errorf("%w: %q", ErrCrossMount, rel)
		case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.E2BIG):
			return -1, fmt.Errorf("%w: openat2 unavailable (%v)", ErrNoReplaceUnsupport, err)
		default:
			return -1, fmt.Errorf("fsops: open %q: %w", rel, err)
		}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("fsops: stat %q: %w", rel, err)
	}
	if devOf(&st) != rootDev {
		unix.Close(fd)
		return -1, fmt.Errorf("%w: %q is on dev %d, root on %d", ErrCrossMount, rel, devOf(&st), rootDev)
	}
	return fd, nil
}

// renameNoReplace issues renameat2 with RENAME_NOREPLACE. EINVAL/EOPNOTSUPP
// means the filesystem cannot express "do not replace"; that is a hard stop,
// never a reason to fall back to an overwriting rename.
func renameNoReplace(srcFD int, srcName string, dstFD int, dstName string) error {
	from, err := unix.BytePtrFromString(srcName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	to, err := unix.BytePtrFromString(dstName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_RENAMEAT2,
		uintptr(srcFD), uintptr(unsafe.Pointer(from)),
		uintptr(dstFD), uintptr(unsafe.Pointer(to)),
		uintptr(renameNoReplaceFlag), 0,
	)
	if errno != 0 {
		switch errno {
		case unix.EEXIST:
			return ErrExists
		case unix.EINVAL, unix.EOPNOTSUPP, unix.ENOSYS:
			return fmt.Errorf("%w: renameat2 returned %v", ErrNoReplaceUnsupport, errno)
		default:
			return fmt.Errorf("fsops: renameat2(%q -> %q): %w", srcName, dstName, errno)
		}
	}
	return nil
}

// barrier writes the marker and fsyncs the anchor. Linux has no F_FULLFSYNC;
// fsync of a file is the strongest portable guarantee available here.
func barrier(anchorFD int, marker []byte) error {
	if _, err := unix.Pwrite(anchorFD, marker, 0); err != nil {
		return fmt.Errorf("fsops: write barrier marker: %w", err)
	}
	if err := unix.Fsync(anchorFD); err != nil {
		return fmt.Errorf("fsops: fsync anchor: %w", err)
	}
	return nil
}

func devOf(st *unix.Stat_t) uint64 { return st.Dev }

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
	return domain.FileIdentity{
		VolumeID: fmt.Sprintf("dev-%d", dev),
		Device:   dev,
		Inode:    st.Ino,
		Kind:     kindOf(st),
	}
}

func tokenOf(st *unix.Stat_t, metadataHash domain.Digest) domain.ChangeToken {
	return domain.ChangeToken{
		Size:         st.Size,
		MTimeNS:      st.Mtim.Sec*int64(1e9) + int64(st.Mtim.Nsec),
		CTimeNS:      st.Ctim.Sec*int64(1e9) + int64(st.Ctim.Nsec),
		NLink:        uint64(st.Nlink),
		Mode:         st.Mode,
		UID:          st.Uid,
		GID:          st.Gid,
		Flags:        0,
		MetadataHash: metadataHash,
	}
}

// birthTimeNS is unavailable on the Linux stat contract used here. Returning
// false keeps the identity honest instead of inventing a zero birth time.
func birthTimeNS(*unix.Stat_t) (int64, bool) { return 0, false }

// generationOf is unavailable on Linux.
func generationOf(*unix.Stat_t) (uint64, bool) { return 0, false }

// filesystemName maps the statfs magic to a short name. Unknown magic returns
// "unknown", which the capability check treats as unsupported for mutation.
func filesystemName(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	switch int64(st.Type) {
	case 0xEF53:
		return "ext4", nil
	case 0x58465342:
		return "xfs", nil
	case 0x9123683E:
		return "btrfs", nil
	case 0x01021994:
		return "tmpfs", nil
	case 0x6969:
		return "nfs", nil
	case 0x65735546:
		return "fuse", nil
	default:
		return "unknown", nil
	}
}

// bootOrRunID reads the kernel boot identifier.
func bootOrRunID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// mountIDFor finds the mount ID whose mount point is the longest prefix of
// path. Linux mount IDs are only meaningful within one boot; the caller keeps
// them in the runtime half of the identity for exactly that reason.
func mountIDFor(p string) (*uint64, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var bestID uint64
	bestLen := -1
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		var id uint64
		if _, err := fmt.Sscanf(fields[0], "%d", &id); err != nil {
			continue
		}
		mp := fields[4]
		if mp == p || (strings.HasPrefix(p, mp) && (mp == "/" || p[len(mp)] == '/')) {
			if len(mp) > bestLen {
				bestLen = len(mp)
				bestID = id
			}
		}
	}
	if bestLen < 0 {
		return nil, fmt.Errorf("fsops: no mount point covers %q", p)
	}
	return &bestID, nil
}
