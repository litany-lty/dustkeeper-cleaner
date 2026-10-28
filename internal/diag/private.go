package diag

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrNotPrivate reports that an object where the log should be does not satisfy
// the private-file contract. It follows the same contract the management
// directories use (see fsops.PrivateExpect): a regular file, one link, owner
// only, no setuid or setgid bit.
var ErrNotPrivate = errors.New("diag: log file is not private")

// openPrivateLog opens the live log file and refuses anything that is not the
// file this program created.
//
// The flags are the contract, not decoration:
//
//   - O_NOFOLLOW refuses a pre-created symlink. Without it an open would follow
//     the link and append records — run identifiers, scope paths, error codes —
//     into a file chosen by someone else.
//   - O_NONBLOCK keeps a FIFO from hanging the process. Opening a FIFO for
//     writing waits for a reader, and that wait happens before any type check
//     could refuse it.
//   - 0o600 applies only when the file is created. An existing file with wider
//     permissions is therefore refused rather than quietly fixed: repairing it
//     would mean the program writes its log into a file it has just decided was
//     not trustworthy, and the operator never learns the log was exposed.
func openPrivateLog(path string) (*os.File, error) {
	fd, err := unix.Open(path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		// Three shapes are refused by the kernel before any stat could describe
		// them. Each is reported as a contract failure, because that is what it
		// is: the name does not hold the file this program created.
		switch {
		case errors.Is(err, unix.ELOOP):
			return nil, fmt.Errorf("%w: %s is a symlink", ErrNotPrivate, path)
		case errors.Is(err, unix.ENXIO):
			// O_NONBLOCK on a FIFO with no reader. Without the flag this open
			// would wait for a writer for ever.
			return nil, fmt.Errorf("%w: %s is a FIFO", ErrNotPrivate, path)
		case errors.Is(err, unix.EISDIR):
			return nil, fmt.Errorf("%w: %s is a directory", ErrNotPrivate, path)
		}
		return nil, fmt.Errorf("diag: open %s: %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("diag: stat %s: %w", path, err)
	}
	if err := verifyPrivate(path, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// verifyPrivate applies the contract to the opened file. The checks are on the
// descriptor, not the name, so they describe the object that will be written to.
func verifyPrivate(path string, st *unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: %s is not a regular file (mode %#o)", ErrNotPrivate, path, st.Mode)
	}
	if wide := st.Mode & 0o7777 &^ 0o600; wide != 0 {
		return fmt.Errorf("%w: %s has mode %04o with bits %04o outside 0600",
			ErrNotPrivate, path, st.Mode&0o7777, wide)
	}
	if st.Nlink != 1 {
		return fmt.Errorf("%w: %s has %d links, want 1", ErrNotPrivate, path, st.Nlink)
	}
	if uid := uint32(os.Geteuid()); st.Uid != uid {
		return fmt.Errorf("%w: %s is owned by uid %d, want %d", ErrNotPrivate, path, st.Uid, uid)
	}
	return nil
}
