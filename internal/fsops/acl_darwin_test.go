//go:build darwin

package fsops

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestANativeACLIsFoundWithoutAnAttribute is the fixture R09 C asks for.
//
// The old reader looked for "com.apple.acl.text" in the attribute list, so a file
// whose ACL lives in the inode — which is how APFS stores it — looked like an
// ordinary file. The test therefore asserts both halves: the attribute list does
// not mention the ACL, and the ACL is still found.
func TestANativeACLIsFoundWithoutAnAttribute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acl.txt")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	before := metadataOf(t, path)
	if before.HasACL {
		t.Fatal("a plain file reports an ACL")
	}
	if before.Unsupported != "" {
		t.Fatalf("a plain file's metadata is unsupported: %s", before.Unsupported)
	}

	// The system chmod is the only portable way to install a real ACL; the acl(3)
	// setters are what the product deliberately does not use.
	if out, err := exec.Command("/bin/chmod", "+a", "everyone deny delete", path).CombinedOutput(); err != nil {
		t.Skipf("this host cannot install an ACL fixture: %v (%s)", err, out)
	}
	// The deny rule blocks the removal of the file itself, so the ACL has to go
	// before the temporary directory is cleaned up. Registered after TempDir, so
	// it runs first.
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-N", path).Run() })

	after := metadataOf(t, path)
	if !after.HasACL {
		t.Fatal("the native ACL was not detected")
	}
	if after.Unsupported != "acl_present" {
		t.Fatalf("an ACL must make the file unsupported, got %q", after.Unsupported)
	}
	// The attribute list is not what answered the question: macOS 14 also writes
	// com.apple.provenance on every new file, and none of the attributes names an
	// ACL. Asserting this is what makes the test about the ACL API rather than
	// about a lucky attribute name.
	for _, name := range attributeNames(t, path) {
		if name == "com.apple.acl.text" {
			t.Fatalf("the fixture carries %q, so it does not prove the native query is used", name)
		}
	}
}

// attributeNames lists one file's extended attribute names.
func attributeNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	fd := int(f.Fd())
	sz, err := unix.Flistxattr(fd, nil)
	if err != nil || sz == 0 {
		return nil
	}
	buf := make([]byte, sz)
	n, err := unix.Flistxattr(fd, buf)
	if err != nil {
		t.Fatalf("list xattrs %s: %v", path, err)
	}
	return splitNUL(buf[:n])
}

// metadataOf reads the metadata evidence for one path.
func metadataOf(t *testing.T, path string) MetadataEvidence {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	ev, err := ReadMetadata(int(f.Fd()))
	if err != nil {
		t.Fatalf("read metadata %s: %v", path, err)
	}
	return ev
}
