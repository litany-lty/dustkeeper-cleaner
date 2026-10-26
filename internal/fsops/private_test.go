package fsops

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"dustkeeper/internal/domain"
)

// A02: zero is root, not "no opinion".
//
// The old contract read a zero OwnerUID as "skip the owner check", which meant
// that when Dustkeeper itself ran as root - the configuration with the most to
// lose - a management object chowned to anyone at all still satisfied it.
func TestManagerObjectOwnedByZeroIsComparedExactly(t *testing.T) {
	owned := domain.ChangeToken{UID: 0, Mode: 0o100600, NLink: 1}

	if err := (PrivateExpect{OwnerUID: 0, MaxPerm: 0o600, Links: 1}).VerifyToken("anchor", owned, 1); err != nil {
		t.Fatalf("an object owned by root must satisfy a root expectation: %v", err)
	}

	intruder := domain.ChangeToken{UID: 1000, Mode: 0o100600, NLink: 1}
	err := (PrivateExpect{OwnerUID: 0, MaxPerm: 0o600, Links: 1}).VerifyToken("anchor", intruder, 1)
	if err == nil {
		t.Fatal("an object owned by another uid satisfied a root expectation")
	}
	if !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("error = %v, want ErrNotPrivate", err)
	}

	// The explicit "any owner" value still exists, so a caller that genuinely has
	// no opinion says so instead of relying on a magic zero.
	if err := (PrivateExpect{OwnerUID: OwnerAny, MaxPerm: 0o600, Links: 1}).VerifyToken("anchor", intruder, 1); err != nil {
		t.Fatalf("OwnerAny must accept any owner: %v", err)
	}
}

// A02: a FIFO where a management object is expected must be refused quickly.
//
// Opening a FIFO for reading waits for a writer, and the wait happens before any
// type check can run. A vault whose owner.json is a FIFO therefore used to hang
// the process while holding the scope lock.
func TestFIFOInTheVaultIsRefusedWithoutWaiting(t *testing.T) {
	r, dir := newRoot(t)
	vault := filepath.Join(dir, "vault")
	if err := os.Mkdir(vault, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fifo := filepath.Join(vault, "owner.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("this filesystem cannot create a FIFO: %v", err)
	}
	d, err := r.OpenDir([]byte("vault"))
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer d.Close()

	done := make(chan error, 1)
	go func() {
		_, rerr := d.ReadFileAt("owner.json", 1024)
		done <- rerr
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read as if it were a regular file")
		}
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("error = %v, want ErrNotRegular", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading a FIFO blocked; the type check runs after the open, which already waits")
	}
}
