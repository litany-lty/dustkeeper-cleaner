package diag

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The log names every scope and every decision, so the file it appends to has to
// be the file this program created. These cases are the four ways the name can
// point somewhere else. Each one asserts two things: the open is refused, and the
// object that was already there is untouched — no truncation, no rewrite, no
// permission repair.

// requireNotPrivate asserts the refusal and that it is the contract that refused,
// not an unrelated errno.
func requireNotPrivate(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted as the log file", what)
	}
	if !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("%s was refused for the wrong reason: %v", what, err)
	}
}

func TestASymlinkedLogIsRefusedAndItsTargetUnchanged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "business.txt")
	if err := os.WriteFile(target, []byte("business bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	_, err := Open(dir)
	requireNotPrivate(t, err, "a symlink")

	body, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("read the link target: %v", rerr)
	}
	if string(body) != "business bytes\n" {
		t.Fatalf("the link target was written through: %q", body)
	}
}

func TestAFifoLogIsRefusedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, FileName), 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	// A blocking open would wait for a reader; the test would hang rather than
	// fail. O_NONBLOCK is what turns this into an ordinary refusal.
	done := make(chan error, 1)
	go func() {
		_, err := Open(dir)
		done <- err
	}()
	select {
	case err := <-done:
		requireNotPrivate(t, err, "a FIFO")
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO as the log blocked instead of being refused")
	}
}

func TestAWidePermissionLogIsRefusedAndLeftAsItIs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(dir)
	requireNotPrivate(t, err, "a group-readable log")

	info, serr := os.Stat(path)
	if serr != nil {
		t.Fatalf("stat: %v", serr)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("the refusal also changed the permissions to %04o", perm)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "existing\n" {
		t.Fatalf("the refusal wrote to the file: %q", body)
	}
}

func TestAHardLinkedLogIsRefusedAndItsOtherNameUnchanged(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "elsewhere.txt")
	if err := os.WriteFile(other, []byte("other bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, filepath.Join(dir, FileName)); err != nil {
		t.Skipf("cannot create a hard link here: %v", err)
	}

	_, err := Open(dir)
	requireNotPrivate(t, err, "a hard-linked log")

	body, _ := os.ReadFile(other)
	if string(body) != "other bytes\n" {
		t.Fatalf("the other name of the log was written through: %q", body)
	}
}

// A directory where the log should be is the unprivileged half of "not a
// regular file": the kernel refuses the open itself, before any stat.
func TestADirectoryAtTheLogPathIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	requireNotPrivate(t, err, "a directory")
	if info, serr := os.Stat(filepath.Join(dir, FileName)); serr != nil || !info.IsDir() {
		t.Fatalf("the refusal changed the directory: %v", serr)
	}
}

// A device node is the other half: the open succeeds, so only the type check in
// the contract can refuse it. Creating one needs privileges, so this case is
// honest about skipping.
func TestANonRegularLogIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mknod(filepath.Join(dir, FileName), unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))); err != nil {
		t.Skipf("cannot create a device node here: %v", err)
	}
	_, err := Open(dir)
	requireNotPrivate(t, err, "a character device")
}

// The refusal must not cost the ordinary case: a fresh log is created private,
// and it rotates.
func TestAPrivateLogIsCreatedAndStillRotates(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("a new log was refused: %v", err)
	}
	payload := strings.Repeat("x", 200)
	for i := 0; i < (MaxFileBytes/200)*2; i++ {
		l.Log("filler", "i", i, "detail", payload)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	live := filepath.Join(dir, FileName)
	for _, path := range []string{live, live + ".1"} {
		info, serr := os.Stat(path)
		if serr != nil {
			t.Fatalf("stat %s: %v", path, serr)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s is readable by others: %04o", path, perm)
		}
	}
	// Reopening an existing private log appends instead of refusing, so a
	// scheduled installation keeps one file across runs.
	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening an existing private log was refused: %v", err)
	}
	again.Log("reopened", "k", "v")
	if err := again.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("the log vanished on reopen: %v", err)
	}
}
