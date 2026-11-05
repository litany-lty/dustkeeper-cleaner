package review

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"golang.org/x/sys/unix"
)

func refused(reason string) error { return errs.New(errs.CodeReviewRequired, "retained: "+reason) }

// ReadPrivateFile reads a configuration or credential file that must be a
// private, owner-only regular file.
//
// The walk opens every component with O_NOFOLLOW: checking only the final name
// would let a symlinked directory turn a "private" path into something else by
// the time it is read.
func ReadPrivateFile(p string, limit int64) ([]byte, error) {
	f, err := openNoFollow(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 ||
		st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		return nil, fmt.Errorf("private regular owner-only file required")
	}
	metadata, err := fsops.ReadMetadata(int(f.Fd()))
	if err != nil || metadata.HasACL || metadata.Unsupported != "" {
		return nil, fmt.Errorf("private file ACL or metadata cannot be established")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, fmt.Errorf("private file unreadable or too large")
	}
	return b, nil
}

func openNoFollow(p string) (*os.File, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return nil, fmt.Errorf("absolute normalized path required")
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if p == "/" {
		return os.NewFile(uintptr(fd), p), nil
	}
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, er := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if er != nil {
			return nil, er
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), p), nil
}
