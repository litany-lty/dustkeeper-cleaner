// Package scope owns registration, identity verification and the locks that
// keep two runs from acting on one tree.
//
// Lock order is fixed and must never be inverted:
//
//	registration lock -> instance lock -> scope lock (in-tree) -> control lock
//
// The registration lock is the only one that spans a user-level directory, so it
// is taken first and released before any business action. The scope lock lives
// inside the tree (see OpenVault), which is what makes two state directories
// mutually exclusive over one scope instead of only over one database. Scopes are
// visited in ascending scope ID order and a keeper scope is opened after its
// source, so a cross-scope pass always takes its pair in the same direction. The
// scope lock is also acquired non-blocking, so a cycle surfaces as INSTANCE_BUSY
// for the loser rather than as a deadlock: the order keeps failures predictable,
// it is not what makes them finite.
//
// The control lock never spans a content read, a rename, an unlink or a
// post-move sync; it only guards "am I paused" and "commit the intent".
package scope

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

// Lock is a held advisory file lock.
type Lock struct {
	f    *os.File
	path string
}

// TryLock takes an exclusive lock without waiting.
func TryLock(path string) (*Lock, error) {
	return lockFile(path, false, 0)
}

// LockFor waits up to timeout for an exclusive lock.
func LockFor(path string, timeout time.Duration) (*Lock, error) {
	return lockFile(path, true, timeout)
}

func lockFile(path string, wait bool, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &Lock{f: f, path: path}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, errs.Newf(errs.CodeStateWriteFailed, "flock %s: %v", path, err)
		}
		if !wait || (timeout > 0 && time.Now().After(deadline)) {
			f.Close()
			return nil, errs.Newf(errs.CodeInstanceBusy, "lock %s is held by another task", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Release drops the lock. Releasing an already released lock is a no-op.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// Path returns the locked file.
func (l *Lock) Path() string { return l.path }

// InstanceLock takes the per-state-directory execution lock. If another run
// holds it, the caller exits 3 rather than queueing work.
func InstanceLock(stateDir string) (*Lock, error) {
	return TryLock(filepath.Join(stateDir, "instance.lock"))
}

// RegistrationLock takes the fixed user-level lock used while registering a
// mutable scope. It is derived from the home directory so that a different
// state directory cannot produce a second, unrelated lock over the same tree.
func RegistrationLock(home string) (*Lock, error) {
	return LockFor(filepath.Join(home, ".dustkeeper-registry.lock"), 30*time.Second)
}

// ScopeLock lives on the vault descriptor, not on a path.
//
// The in-tree mutex is taken by OpenVault through the already-opened management
// directory, because opening it by path would first have to create
// ".dustkeeper" - which an observe or reference scope must never grow. There is
// deliberately no path-based constructor here: R02 found the previous one
// defined and never called, which is worse than not having it.

// heldScopeLock is one scope lock owned by this process.
type heldScopeLock struct {
	file *fsops.File
	refs int
}

var (
	scopeLocksMu sync.Mutex
	scopeLocks   = map[string]*heldScopeLock{}
)

// acquireScopeLock takes the in-tree scope lock for the directory d.
//
// flock is attached to the open file description, so a second open of the same
// lock file from this process would be refused by a lock this process already
// holds. That is not what "another task" means, and reconcile opens one scope
// several times in a single command. The registry therefore makes the lock
// re-entrant inside one process while keeping it exclusive across processes,
// which is the property that actually matters: two installations must not act on
// one tree.
//
// The key is the identity of the management directory, so two scopes that happen
// to share a path string still get separate locks.
func acquireScopeLock(d *fsops.Dir) (release func(), err error) {
	id, _, err := d.Stat()
	if err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	key := fmt.Sprintf("%d:%d", id.Device, id.Inode)

	scopeLocksMu.Lock()
	defer scopeLocksMu.Unlock()

	if held, ok := scopeLocks[key]; ok {
		held.refs++
		return func() { releaseScopeLock(key) }, nil
	}

	file, err := d.LockFile(domain.VaultScopeLock)
	if err != nil {
		if errors.Is(err, fsops.ErrExists) {
			return nil, errs.Newf(errs.CodeInstanceBusy,
				"another Dustkeeper task is acting on this scope right now")
		}
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	scopeLocks[key] = &heldScopeLock{file: file, refs: 1}
	return func() { releaseScopeLock(key) }, nil
}

// releaseScopeLock drops one reference and closes the descriptor when the last
// holder in this process is done.
func releaseScopeLock(key string) {
	scopeLocksMu.Lock()
	defer scopeLocksMu.Unlock()

	held, ok := scopeLocks[key]
	if !ok {
		return
	}
	held.refs--
	if held.refs > 0 {
		return
	}
	held.file.Close()
	delete(scopeLocks, key)
}

// ControlLock is the tiny gate shared by `pause` and the intent commit.
type ControlLock struct {
	lock *Lock
}

// AcquireControlLock waits briefly for the control lock.
func AcquireControlLock(stateDir string) (*ControlLock, error) {
	l, err := LockFor(filepath.Join(stateDir, "control.lock"), 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &ControlLock{lock: l}, nil
}

// Release drops the control lock.
func (c *ControlLock) Release() {
	if c != nil && c.lock != nil {
		c.lock.Release()
	}
}

// PauseGate implements the pause marker from section 12.5.
//
// The marker only ever narrows authority. Writing it does not stop a filesystem
// operation that has already begun; it prevents the next one from starting.
type PauseGate struct {
	StateDir string
}

func (g PauseGate) markerPath() string { return filepath.Join(g.StateDir, "pause.request") }

// Request persists the pause marker inside the control lock and reports whether
// it was newly written. "Requested" is not "stopped"; callers must say so.
func (g PauseGate) Request(reason string) (bool, error) {
	c, err := AcquireControlLock(g.StateDir)
	if err != nil {
		return false, err
	}
	defer c.Release()

	if _, err := os.Stat(g.markerPath()); err == nil {
		return false, nil
	}
	body := fmt.Sprintf("requested_at_ns=%d\nreason=%s\n", time.Now().UnixNano(), reason)
	tmp := g.markerPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return false, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := syncFile(tmp); err != nil {
		return false, errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := os.Rename(tmp, g.markerPath()); err != nil {
		return false, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := syncDir(g.StateDir); err != nil {
		return false, errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	return true, nil
}

// Clear removes the marker. It is explicit: nothing clears a pause implicitly.
func (g PauseGate) Clear() (bool, error) {
	c, err := AcquireControlLock(g.StateDir)
	if err != nil {
		return false, err
	}
	defer c.Release()

	if err := os.Remove(g.markerPath()); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := syncDir(g.StateDir); err != nil {
		return false, errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	return true, nil
}

// Paused reports whether the marker exists. A read error other than "missing"
// is reported as an error, never as "not paused".
func (g PauseGate) Paused() (bool, error) {
	_, err := os.Stat(g.markerPath())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, errs.Wrap(errs.CodeAccessDenied, err)
}

// CheckAndCommit runs fn while holding the control lock, but only if no pause
// marker exists.
//
// This is the one critical section that makes "pause requests and new intents
// are mutually exclusive" true: if the intent commits first the item converges,
// if the marker lands first no new item is accepted.
func (g PauseGate) CheckAndCommit(fn func() error) error {
	c, err := AcquireControlLock(g.StateDir)
	if err != nil {
		return err
	}
	defer c.Release()

	paused, err := g.Paused()
	if err != nil {
		return err
	}
	if paused {
		return errs.New(errs.CodePaused, "a pause was requested; no new file change may start")
	}
	return fn()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Fsync(int(f.Fd()))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Fsync(int(f.Fd()))
}
