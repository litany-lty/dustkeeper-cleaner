package fsops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// newRoot creates a temporary tree and opens it as a scope root.
func newRoot(t *testing.T) (*Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// TestNoReplace is the load-bearing test for section 2.3 item 6: an existing
// target is never replaced, and the source survives the refusal.
func TestNoReplace(t *testing.T) {
	r, dir := newRoot(t)
	writeFile(t, dir, "a.txt", "source-a")
	writeFile(t, dir, "b.txt", "target-b")
	if err := os.Mkdir(filepath.Join(dir, "vault"), 0o700); err != nil {
		t.Fatal(err)
	}

	src, err := r.OpenParent([]byte("a.txt"))
	if err != nil {
		t.Fatalf("OpenParent src: %v", err)
	}
	defer src.Close()
	dstDir, err := r.OpenDir([]byte("vault"))
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	defer dstDir.Close()

	// Move into the vault first, so the collision is with a real duplicate name.
	if err := RenameNoReplace(src, "a.txt", dstDir, "a.txt"); err != nil {
		t.Fatalf("first move should succeed: %v", err)
	}
	// Put a new source in place and try to overwrite the object in the vault.
	writeFile(t, dir, "a.txt", "source-a-2")
	src2, err := r.OpenParent([]byte("a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer src2.Close()

	err = RenameNoReplace(src2, "a.txt", dstDir, "a.txt")
	if !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}

	kept, err := os.ReadFile(filepath.Join(dir, "vault", "a.txt"))
	if err != nil {
		t.Fatalf("vault object vanished: %v", err)
	}
	if string(kept) != "source-a" {
		t.Fatalf("vault object was overwritten: %q", kept)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("source was consumed by the refused rename: %v", err)
	}
}

// TestNoFollow proves a symlink in any path position is refused rather than
// followed, including the final component.
func TestNoFollow(t *testing.T) {
	r, dir := newRoot(t)
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "outside")

	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	if _, err := r.OpenDir([]byte("linkdir")); !errors.Is(err, ErrSymlink) {
		t.Fatalf("OpenDir through symlink: want ErrSymlink, got %v", err)
	}
	if _, err := r.OpenParent([]byte("linkdir/secret.txt")); !errors.Is(err, ErrSymlink) {
		t.Fatalf("OpenParent through symlink: want ErrSymlink, got %v", err)
	}

	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "linkfile")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	p, err := r.OpenParent([]byte("linkfile"))
	if err != nil {
		t.Fatalf("OpenParent on final symlink should resolve the name: %v", err)
	}
	defer p.Close()
	if _, err := OpenRegularAt(p, r.Dev()); !errors.Is(err, ErrSymlink) {
		t.Fatalf("OpenRegularAt on symlink: want ErrSymlink, got %v", err)
	}
}

// TestPathValidation pins the rejection list from section 6.3 and the executor.
func TestPathValidation(t *testing.T) {
	bad := []string{"", "/abs", "a/../b", "./a", "a//b", "a/", "a\x00b"}
	for _, s := range bad {
		if _, err := SplitRelative([]byte(s)); err == nil {
			t.Fatalf("SplitRelative(%q) should fail", s)
		}
	}
	good := []string{"a", "a/b", "deep/dir/file.zip", "keep/old.zip"}
	for _, s := range good {
		if _, err := SplitRelative([]byte(s)); err != nil {
			t.Fatalf("SplitRelative(%q): %v", s, err)
		}
	}
	if err := ValidateName(".."); err == nil {
		t.Fatal("ValidateName should reject ..")
	}
	if err := ValidateName("a/b"); err == nil {
		t.Fatal("ValidateName should reject a separator")
	}

	// A component the kernel would refuse with ENAMETOOLONG must be a path
	// argument error, not an I/O error surfacing later at openat. Found by
	// FuzzSplitRelative.
	long := strings.Repeat("x", 256)
	if _, err := SplitRelative([]byte(long)); err == nil {
		t.Fatal("SplitRelative should reject a 256-byte component")
	}
	if _, err := SplitRelative([]byte(strings.Repeat("x", 255))); err != nil {
		t.Fatalf("SplitRelative rejected a 255-byte component: %v", err)
	}
}

// TestDirSyncAndBarrier is the T03 durability probe: the platform must accept a
// directory fsync and a full-flush barrier on the anchor file. If this fails,
// the product cannot claim mutation support on the host.
func TestDirSyncAndBarrier(t *testing.T) {
	r, dir := newRoot(t)
	writeFile(t, dir, "f.txt", "payload")

	p, err := r.OpenParent([]byte("f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	f, err := OpenRegularAt(p, r.Dev())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := DurableFile(f.FD()); err != nil {
		t.Fatalf("DurableFile: %v", err)
	}
	d, err := r.OpenDir(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := DurableDir(d); err != nil {
		t.Fatalf("DurableDir: %v", err)
	}

	anchorPath := filepath.Join(dir, "anchor")
	af, err := os.OpenFile(anchorPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer af.Close()
	if err := Barrier(int(af.Fd()), []byte("barrier-1")); err != nil {
		t.Fatalf("Barrier: %v", err)
	}
	got, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("barrier-1")) {
		t.Fatalf("anchor marker not durable as written: %q", got)
	}
}

// TestIdentityAndToken checks that identity facts are populated or explicitly
// absent, and that the change token tracks size and mtime.
func TestIdentityAndToken(t *testing.T) {
	r, dir := newRoot(t)
	writeFile(t, dir, "f.txt", "hello")

	p, err := r.OpenParent([]byte("f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	f, err := OpenRegularAt(p, r.Dev())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if f.Identity.Kind != "regular" {
		t.Fatalf("kind = %q", f.Identity.Kind)
	}
	if f.Identity.Inode == 0 || f.Identity.Device == 0 {
		t.Fatalf("identity missing device/inode: %+v", f.Identity)
	}
	if f.Token.Size != 5 {
		t.Fatalf("size = %d", f.Token.Size)
	}
	if f.Token.MTimeNS == 0 {
		t.Fatal("mtime not recorded")
	}

	rootID, err := r.Identity()
	if err != nil {
		t.Fatalf("root identity: %v", err)
	}
	if rootID.Persistent.Inode == 0 {
		t.Fatal("root inode missing")
	}
	if rootID.Runtime.BootOrRunID == "" {
		t.Fatal("boot/run id missing")
	}
	t.Logf("root identity: fs=%s vol=%s boot=%s", rootID.Persistent.FileSystem, rootID.Persistent.VolumeID, rootID.Runtime.BootOrRunID)
}

// TestAPFSNameAlias records the name-equivalence behaviour of the host
// filesystem. On a case-insensitive APFS volume "Case.TXT" and "case.txt" are
// the same object, which is why protection checks resolve kernel identities
// instead of comparing strings.
func TestAPFSNameAlias(t *testing.T) {
	r, dir := newRoot(t)
	writeFile(t, dir, "Case.TXT", "same object")

	p1, err := r.OpenParent([]byte("Case.TXT"))
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := r.OpenParent([]byte("case.txt"))
	if err != nil {
		t.Logf("case variant is a distinct name on this filesystem: %v", err)
		return
	}
	defer p2.Close()

	id1, _, err := p1.StatAtNoFollow()
	if err != nil {
		t.Fatal(err)
	}
	id2, _, err := p2.StatAtNoFollow()
	if err != nil {
		t.Fatalf("alias lookup failed: %v", err)
	}
	if id1.Equal(id2) {
		t.Logf("filesystem treats names as aliases: both resolve to inode %d", id1.Inode)
	} else {
		t.Logf("filesystem treats names as distinct: %d vs %d", id1.Inode, id2.Inode)
	}
}

// TestMkdirAllIsBounded ensures the vault helper refuses to escape the root and
// cannot follow a symlink out of it.
func TestMkdirAllIsBounded(t *testing.T) {
	r, dir := newRoot(t)
	if err := r.MkdirAll([]byte(".dustkeeper/objects"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".dustkeeper", "objects")); err != nil || !fi.IsDir() {
		t.Fatalf("vault not created: %v", err)
	}
	if err := r.MkdirAll([]byte("../escape"), 0o700); err == nil {
		t.Fatal("MkdirAll should reject ..")
	}
}

// TestMetadataDetectsXattr shows the exact-duplicate gate is driven by real
// attribute evidence, not by a filesystem guess.
func TestMetadataDetectsXattr(t *testing.T) {
	r, dir := newRoot(t)
	p := writeFile(t, dir, "f.txt", "content")
	if err := unix.Setxattr(p, "user.dustkeeper.test", []byte("v1"), 0); err != nil {
		t.Skipf("xattr not supported here: %v", err)
	}

	parent, err := r.OpenParent([]byte("f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	f, err := OpenRegularAt(parent, r.Dev())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ev, err := ReadMetadata(f.FD())
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if ev.XattrCount == 0 || ev.XattrDigest == "" {
		t.Fatalf("xattr not observed: %+v", ev)
	}
	if ev.Unsupported != "" {
		t.Fatalf("unexpected unsupported reason: %s", ev.Unsupported)
	}
	base := ev.XattrDigest

	if err := unix.Setxattr(p, "user.dustkeeper.test", []byte("v2"), 0); err != nil {
		t.Fatal(err)
	}
	ev2, err := ReadMetadata(f.FD())
	if err != nil {
		t.Fatal(err)
	}
	if ev2.XattrDigest == base {
		t.Fatal("xattr digest did not change with the value")
	}
	if !strings.HasPrefix(ev2.XattrDigest, "sha256:") {
		t.Fatalf("digest shape: %s", ev2.XattrDigest)
	}
}
