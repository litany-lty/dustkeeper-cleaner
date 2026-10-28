package diag

// Tests for the boundary around the log *directory*.
//
// The leaf contract in private_test.go covers the four ways the log file itself
// can point somewhere else. It cannot cover a "logs" that is a link, because
// O_NOFOLLOW stops at the last path component - so these cases are about the
// directory, and about what happens to create/rotate after the path it was
// opened through has been replaced.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestASymlinkedLogDirectoryIsRefusedAndItsTargetUnchanged covers D12. The
// directory the link points at holds a perfectly ordinary private log file, so
// every leaf check passes; the refusal has to come from the directory itself.
func TestASymlinkedLogDirectoryIsRefusedAndItsTargetUnchanged(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "somewhere-else")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, FileName)
	if err := os.WriteFile(target, []byte("records of another program\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "logs")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	_, err := Open(link)
	requireNotPrivate(t, err, "a symlinked log directory")

	body, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("read the link target: %v", rerr)
	}
	if string(body) != "records of another program\n" {
		t.Fatalf("the log was written through the directory link: %q", body)
	}
	info, serr := os.Stat(target)
	if serr != nil {
		t.Fatalf("stat the link target: %v", serr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the link target's permissions were changed to %04o", perm)
	}
}

// TestARotatedNameStaysInsideTheVerifiedDirectory covers the second half of D12:
// once the log is open, the path it was opened through can be replaced. Rotation
// re-opens the live file, and doing that by path writes into whatever the name
// points at now.
func TestARotatedNameStaysInsideTheVerifiedDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "logs")
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	l.Log("before", "phase", "open")

	// The name is taken over: the directory that was verified is moved aside and a
	// fresh one appears under the same path.
	moved := filepath.Join(base, "logs-before-replacement")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("move the verified directory aside: %v", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create the replacement directory: %v", err)
	}

	// Enough records to force at least one rotation, which is where the live file
	// is re-created.
	payload := strings.Repeat("y", 200)
	for i := 0; i < (MaxFileBytes/200)*2; i++ {
		l.Log("filler", "i", i, "detail", payload)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The rotation happened inside the directory that was verified.
	if _, serr := os.Stat(filepath.Join(moved, FileName+".1")); serr != nil {
		t.Fatalf("no rotated file inside the verified directory: %v", serr)
	}
	// And nothing was created in the directory that took over the name.
	if _, serr := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(serr) {
		t.Fatalf("a rotation wrote into the directory that replaced the verified one: %v", serr)
	}
}
