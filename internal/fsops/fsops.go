// Package fsops is the only place in Dustkeeper that is allowed to change the
// namespace: rename, unlink, mkdir, fsync.
//
// Everything here works on file descriptors obtained by walking the path one
// component at a time with O_NOFOLLOW, starting from a root that was itself
// verified by identity. Strings that a caller supplies are either a validated
// single component or a validated relative path; no shell, no glob, and no
// re-encoding of a display path ever reaches a syscall.
//
// The package cannot make rename atomic with respect to a check of the source
// inode: renameat2 and renameatx_np have no "only if the source is still inode
// X" parameter. What it can do is refuse to overwrite, and let the executor
// verify what actually moved afterwards. That boundary is documented in
// section 2.4 and is enforced by the callers in internal/execute.
package fsops

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
)

// Sentinel errors. Callers branch on these rather than on errno text.
var (
	ErrInvalidPath        = errors.New("fsops: invalid relative path")
	ErrCrossMount         = errors.New("fsops: path crosses a mount boundary")
	ErrNotRegular         = errors.New("fsops: object is not a regular file")
	ErrSymlink            = errors.New("fsops: symbolic link encountered")
	ErrHandleClosed       = errors.New("fsops: handle already closed")
	ErrNoReplaceUnsupport = errors.New("fsops: exclusive rename is not supported here")
	ErrExists             = errors.New("fsops: target already exists")
	ErrUnsupportedObject  = errors.New("fsops: unsupported object type")
)

// Limits for path validation. They are deliberately generous but bounded.
const (
	maxPathBytes  = 4096
	maxComponents = 128
)

// SplitRelative validates a raw relative path and returns its components.
//
// Rejected: absolute paths, empty components, "." and "..", NUL bytes, trailing
// separators and over-long inputs. Each rejection exists because the component
// walk below would otherwise have to interpret something.
func SplitRelative(rel []byte) ([]string, error) {
	if len(rel) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if len(rel) > maxPathBytes {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrInvalidPath, maxPathBytes)
	}
	if bytes.IndexByte(rel, 0) >= 0 {
		return nil, fmt.Errorf("%w: contains NUL", ErrInvalidPath)
	}
	if rel[0] == '/' {
		return nil, fmt.Errorf("%w: absolute", ErrInvalidPath)
	}
	if rel[len(rel)-1] == '/' {
		return nil, fmt.Errorf("%w: trailing separator", ErrInvalidPath)
	}
	parts := strings.Split(string(rel), "/")
	if len(parts) > maxComponents {
		return nil, fmt.Errorf("%w: more than %d components", ErrInvalidPath, maxComponents)
	}
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("%w: empty component", ErrInvalidPath)
		}
		if p == "." || p == ".." {
			return nil, fmt.Errorf("%w: relative component %q", ErrInvalidPath, p)
		}
		// The kernel would reject this at openat time with ENAMETOOLONG, which
		// surfaces as an I/O error long after the caller believed the path was
		// valid. Reject it here, where the answer is still "bad argument".
		if len(p) > 255 {
			return nil, fmt.Errorf("%w: component longer than 255 bytes", ErrInvalidPath)
		}
	}
	return parts, nil
}

// ValidateName checks a single directory entry name.
func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: bare name %q", ErrInvalidPath, name)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: name contains a separator or NUL", ErrInvalidPath)
	}
	if len(name) > 255 {
		return fmt.Errorf("%w: name longer than 255 bytes", ErrInvalidPath)
	}
	return nil
}

// DirFD is implemented by every handle that can act as a directory operand.
type DirFD interface {
	DirFD() int
}

// classifyDirOpenError refines an O_DIRECTORY|O_NOFOLLOW failure.
//
// macOS reports ENOTDIR, not ELOOP, when the component is a symlink to a
// directory. Reporting that as a generic error would hide the fact that a link
// was present, which is exactly what the caller must know.
func classifyDirOpenError(dirFD int, name string, err error) error {
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: component %q", ErrSymlink, name)
	}
	if errors.Is(err, unix.ENOTDIR) {
		var st unix.Stat_t
		if serr := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil {
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFLNK:
				return fmt.Errorf("%w: component %q is a symlink to a directory", ErrSymlink, name)
			case unix.S_IFREG:
				return fmt.Errorf("fsops: component %q is a regular file, not a directory", name)
			}
		}
	}
	return nil
}

// Root is an open, verified scope root.
type Root struct {
	f      *os.File
	fd     int
	path   string
	dev    uint64
	mu     sync.Mutex
	closed bool
}

// OpenRoot opens path as a directory without following a symlink on the final
// component, then records the device number used for cross-mount checks.
func OpenRoot(p string) (*Root, error) {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("fsops: open root %q: %w", p, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fsops: stat root: %w", err)
	}
	if err := unix.SetNonblock(fd, false); err == nil {
		// directories are never in non-blocking mode; ignore
	}
	return &Root{
		f:    os.NewFile(uintptr(fd), p),
		fd:   fd,
		path: p,
		dev:  devOf(&st),
	}, nil
}

// Path returns the path this root was opened with.
func (r *Root) Path() string { return r.path }

// Dev returns the device number of the root.
func (r *Root) Dev() uint64 { return r.dev }

// DirFD implements DirFD.
func (r *Root) DirFD() int { return r.fd }

// Close releases the root descriptor exactly once.
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.f.Close()
}

// Stat returns the root's identity and change token.
func (r *Root) Stat() (domain.FileIdentity, domain.ChangeToken, error) {
	var st unix.Stat_t
	if err := unix.Fstat(r.fd, &st); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, fmt.Errorf("fsops: stat root: %w", err)
	}
	return identityOf(&st), tokenOf(&st, ""), nil
}

// Identity builds the two-layer root identity.
func (r *Root) Identity() (domain.RootIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(r.fd, &st); err != nil {
		return domain.RootIdentity{}, fmt.Errorf("fsops: stat root: %w", err)
	}
	fsName, err := filesystemName(r.path)
	if err != nil {
		fsName = "unknown"
	}
	boot, err := bootOrRunID()
	if err != nil {
		boot = "unavailable"
	}
	mountID, _ := mountIDFor(r.path)
	var birth *int64
	if b, ok := birthTimeNS(&st); ok {
		birth = &b
	}
	var gen *uint64
	if g, ok := generationOf(&st); ok {
		gen = &g
	}
	return domain.RootIdentity{
		Persistent: domain.PersistentRootIdentity{
			VolumeID:    fmt.Sprintf("dev-%d", devOf(&st)),
			Inode:       st.Ino,
			BirthTimeNS: birth,
			Generation:  gen,
			OwnerUID:    st.Uid,
			FileSystem:  fsName,
		},
		Runtime: domain.RuntimeMountIdentity{
			BootOrRunID: boot,
			Device:      devOf(&st),
			MountID:     mountID,
		},
	}, nil
}

// Dir is an open directory inside a root.
type Dir struct {
	f      *os.File
	fd     int
	rel    string
	mu     sync.Mutex
	closed bool
}

// DirFD implements DirFD.
func (d *Dir) DirFD() int { return d.fd }

// Rel returns the path of the directory relative to its root.
func (d *Dir) Rel() string { return d.rel }

// Close releases the descriptor exactly once.
func (d *Dir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.f.Close()
}

// Stat returns the directory's identity and change token.
func (d *Dir) Stat() (domain.FileIdentity, domain.ChangeToken, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, fmt.Errorf("fsops: stat dir: %w", err)
	}
	return identityOf(&st), tokenOf(&st, ""), nil
}

// Readdir enumerates up to n entries. The caller must treat a directory listing
// as a hint, not as proof about a file: the entry is re-checked when it is
// opened.
func (d *Dir) Readdir(n int) ([]os.DirEntry, error) {
	return d.f.ReadDir(n)
}

// OpenDir opens rel inside the root without following symlinks and without
// crossing a mount boundary. An empty rel returns a duplicate of the root.
func (r *Root) OpenDir(rel []byte) (*Dir, error) {
	if len(rel) == 0 {
		dup, err := unix.Dup(r.fd)
		if err != nil {
			return nil, fmt.Errorf("fsops: dup root: %w", err)
		}
		return &Dir{f: os.NewFile(uintptr(dup), r.path), fd: dup, rel: "."}, nil
	}
	parts, err := SplitRelative(rel)
	if err != nil {
		return nil, err
	}
	child, err := openDirChain(r.fd, parts, r.dev)
	if err != nil {
		return nil, err
	}
	return &Dir{f: os.NewFile(uintptr(child), path.Join(r.path, string(rel))), fd: child, rel: string(rel)}, nil
}

// Parent is a verified parent directory plus one validated entry name.
type Parent struct {
	f    *os.File
	fd   int
	name string
	rel  string
	mu   sync.Mutex
	done bool
}

// DirFD implements DirFD.
func (p *Parent) DirFD() int { return p.fd }

// Name returns the entry name inside the parent.
func (p *Parent) Name() string { return p.name }

// Rel returns the full path of the entry relative to the root.
func (p *Parent) Rel() string { return p.rel }

// Close releases the descriptor exactly once.
func (p *Parent) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return nil
	}
	p.done = true
	return p.f.Close()
}

// Stat returns the parent directory's identity.
func (p *Parent) Stat() (domain.FileIdentity, domain.ChangeToken, error) {
	var st unix.Stat_t
	if err := unix.Fstat(p.fd, &st); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, fmt.Errorf("fsops: stat parent: %w", err)
	}
	return identityOf(&st), tokenOf(&st, ""), nil
}

// OpenParent resolves rel down to its parent directory and returns the last
// component as a validated name.
func (r *Root) OpenParent(rel []byte) (*Parent, error) {
	parts, err := SplitRelative(rel)
	if err != nil {
		return nil, err
	}
	name := parts[len(parts)-1]
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	var fd int
	if len(parts) == 1 {
		fd, err = unix.Dup(r.fd)
		if err != nil {
			return nil, fmt.Errorf("fsops: dup root: %w", err)
		}
	} else {
		fd, err = openDirChain(r.fd, parts[:len(parts)-1], r.dev)
		if err != nil {
			return nil, err
		}
	}
	return &Parent{f: os.NewFile(uintptr(fd), r.path), fd: fd, name: name, rel: string(rel)}, nil
}

// File is an open regular file plus its identity at open time.
type File struct {
	f        *os.File
	fd       int
	Identity domain.FileIdentity
	Token    domain.ChangeToken
	mu       sync.Mutex
	closed   bool
}

// DirFD is not applicable; File intentionally does not implement DirFD.
func (f *File) FD() int { return f.fd }

// Stat re-reads identity and token from the open descriptor.
func (f *File) Stat() (domain.FileIdentity, domain.ChangeToken, error) {
	var st unix.Stat_t
	if err := unix.Fstat(f.fd, &st); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, fmt.Errorf("fsops: stat file: %w", err)
	}
	return identityOf(&st), tokenOf(&st, ""), nil
}

// Read reads from the descriptor.
func (f *File) Read(p []byte) (int, error) { return f.f.Read(p) }

// ReadAt reads at an offset.
func (f *File) ReadAt(p []byte, off int64) (int, error) { return f.f.ReadAt(p, off) }

// Close releases the descriptor exactly once.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.f.Close()
}

// OpenRegularAt opens an existing regular file inside p without following a
// symlink, and rejects anything that is not a plain file on the root's device.
func OpenRegularAt(p *Parent, rootDev uint64) (*File, error) {
	if err := ValidateName(p.name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(p.fd, p.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil, err
		}
		if err == unix.ELOOP || err == unix.EMLINK {
			return nil, fmt.Errorf("%w: %q", ErrSymlink, p.name)
		}
		return nil, fmt.Errorf("fsops: open %q: %w", p.name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fsops: stat %q: %w", p.name, err)
	}
	if devOf(&st) != rootDev {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %q is on dev %d", ErrCrossMount, p.name, devOf(&st))
	}
	if kindOf(&st) != "regular" {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %q is %s", ErrNotRegular, p.name, kindOf(&st))
	}
	return &File{
		f:        os.NewFile(uintptr(fd), p.name),
		fd:       fd,
		Identity: identityOf(&st),
		Token:    tokenOf(&st, ""),
	}, nil
}

// StatAtNoFollow returns the identity of an entry without following symlinks.
// A missing entry is reported with ENOENT, never as a zero identity.
func (p *Parent) StatAtNoFollow() (domain.FileIdentity, domain.ChangeToken, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(p.fd, p.name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, err
	}
	return identityOf(&st), tokenOf(&st, ""), nil
}

// MkdirAll creates each missing component under the root with perm, refusing to
// follow symlinks and refusing to cross a mount. It is only used to create the
// private vault at registration time.
func (r *Root) MkdirAll(rel []byte, perm uint32) error {
	parts, err := SplitRelative(rel)
	if err != nil {
		return err
	}
	cur, err := unix.Dup(r.fd)
	if err != nil {
		return fmt.Errorf("fsops: dup root: %w", err)
	}
	defer unix.Close(cur)

	for _, name := range parts {
		next, err := openDirChain(cur, []string{name}, r.dev)
		if err == nil {
			unix.Close(cur)
			cur = next
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		if err := unix.Mkdirat(cur, name, perm); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("fsops: mkdir %q: %w", name, err)
		}
		next, err = openDirChain(cur, []string{name}, r.dev)
		if err != nil {
			return err
		}
		unix.Close(cur)
		cur = next
	}
	return SyncDir(cur)
}

// CreateExclusive creates a new file inside dir with O_CREAT|O_EXCL and returns
// an open handle. It never truncates an existing file.
func CreateExclusive(dir DirFD, name string, perm uint32) (*File, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dir.DirFD(), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		if err == unix.EEXIST {
			return nil, ErrExists
		}
		return nil, fmt.Errorf("fsops: create %q: %w", name, err)
	}
	return &File{f: os.NewFile(uintptr(fd), name), fd: fd}, nil
}

// Exists reports whether an entry exists, distinguishing "not there" (ENOENT)
// from "cannot tell" (permission, I/O). Callers must not treat the latter as
// absence.
func (p *Parent) Exists() (bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(p.fd, p.name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	default:
		return false, fmt.Errorf("fsops: stat %q: %w", p.name, err)
	}
}

// RenameNoReplace moves an entry without ever replacing an existing target.
// There is no fallback to an overwriting rename and no fallback to copy+unlink:
// if the platform cannot do this, the answer is an error, not a riskier path.
func RenameNoReplace(src DirFD, srcName string, dst DirFD, dstName string) error {
	if err := ValidateName(srcName); err != nil {
		return err
	}
	if err := ValidateName(dstName); err != nil {
		return err
	}
	return renameNoReplace(src.DirFD(), srcName, dst.DirFD(), dstName)
}

// UnlinkAt removes a single entry by name. The executor only ever passes a
// generated object name inside the private vault.
func UnlinkAt(dir DirFD, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := unix.Unlinkat(dir.DirFD(), name, 0); err != nil {
		return fmt.Errorf("fsops: unlink %q: %w", name, err)
	}
	return nil
}

// ReadLinkAt reads a symlink target, used only for reporting.
func ReadLinkAt(dir DirFD, name string, buf []byte) (string, error) {
	n, err := unix.Readlinkat(dir.DirFD(), name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// Chmod adjusts permissions on an open directory.
func Chmod(dir DirFD, perm uint32) error {
	return unix.Fchmod(dir.DirFD(), perm)
}

// MetadataEvidence is what the exact-duplicate policy needs to know about a
// file's extended metadata. Unsupported is non-empty when the evidence cannot
// be established, in which case the file must be kept.
type MetadataEvidence struct {
	XattrDigest     string
	XattrCount      int
	HasACL          bool
	HasResourceFork bool
	Flags           uint64
	Unsupported     string
}

// ReadMetadata collects xattr digests, ACL presence and file flags. Limits from
// section 9.4 are enforced: one value at most 1 MiB, 4 MiB in total.
func ReadMetadata(fd int) (MetadataEvidence, error) {
	return readMetadata(fd)
}

// DurableFile flushes file data. Renaming an entry does not make its data
// durable, so this runs before the parent directories are synced.
func DurableFile(fd int) error {
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("fsops: fsync file: %w", err)
	}
	return nil
}

// DurableDir flushes a directory entry. Syncing the file alone does not prove
// the directory entry is durable.
func DurableDir(dir DirFD) error {
	return SyncDir(dir.DirFD())
}

// SyncDir fsyncs an open directory descriptor.
func SyncDir(fd int) error {
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("fsops: fsync dir: %w", err)
	}
	return nil
}

// Barrier requests a full device flush using the registered same-volume anchor.
// On macOS this is F_FULLFSYNC; on Linux it is an ordinary fsync of the anchor.
// The marker is diagnostic only and never a source of truth about an action.
func Barrier(anchorFD int, marker []byte) error {
	return barrier(anchorFD, marker)
}

// StatEntry returns the identity of one entry inside d without following a
// symlink. The entry name is validated first; a name with a separator can never
// reach the syscall.
func (d *Dir) StatEntry(name string) (domain.FileIdentity, domain.ChangeToken, error) {
	if err := ValidateName(name); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return domain.FileIdentity{}, domain.ChangeToken{}, err
	}
	return identityOf(&st), tokenOf(&st, ""), nil
}

// Dev returns the device of the directory, used for the cross-mount check.
func (d *Dir) Dev() (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return 0, err
	}
	return devOf(&st), nil
}

// OpenFileRW opens an existing regular file read-write without following a
// symlink. It is used for the durability anchor, which must accept a barrier
// request.
func (r *Root) OpenFileRW(rel []byte) (*File, error) {
	p, err := r.OpenParent(rel)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	fd, err := unix.Openat(p.fd, p.name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("fsops: open %q read-write: %w", p.name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fsops: stat %q: %w", p.name, err)
	}
	if kindOf(&st) != "regular" {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %q is %s", ErrNotRegular, p.name, kindOf(&st))
	}
	return &File{f: os.NewFile(uintptr(fd), p.name), fd: fd, Identity: identityOf(&st), Token: tokenOf(&st, "")}, nil
}

// Write writes at the current offset.
func (f *File) Write(p []byte) (int, error) { return f.f.Write(p) }

// Seek repositions the read offset.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	return f.f.Seek(offset, whence)
}

// Sync flushes this file's data.
func (f *File) Sync() error { return unix.Fsync(f.fd) }

// ParentOf returns a Parent handle for one entry inside d. The directory
// descriptor is duplicated so the handle owns its own lifetime.
func (d *Dir) ParentOf(name string) (*Parent, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Dup(d.fd)
	if err != nil {
		return nil, fmt.Errorf("fsops: dup dir: %w", err)
	}
	return &Parent{f: os.NewFile(uintptr(fd), name), fd: fd, name: name, rel: path.Join(d.rel, name)}, nil
}
