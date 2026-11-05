package fsops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestA21OpenRootRejectsFinalSymlink(t *testing.T) {
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "scope-link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}

	root, err := OpenRoot(link)
	if err == nil {
		root.Close()
		t.Fatal("OpenRoot followed the final symlink")
	}
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("OpenRoot error = %v, want ErrSymlink", err)
	}
}

func TestA22ReadLinkAtRejectsPathTraversal(t *testing.T) {
	base := t.TempDir()
	scope := filepath.Join(base, "scope")
	if err := os.Mkdir(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("outside-target", filepath.Join(base, "outside-link")); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	buf := make([]byte, 256)
	_, err = ReadLinkAt(root, "../outside-link", buf)
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("ReadLinkAt traversal error = %v, want ErrInvalidPath", err)
	}
}

func TestA23ClosedRootCannotUseRecycledDescriptor(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	closedFD := root.DirFD()
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	replacementFD, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(replacementFD)
	if replacementFD != closedFD {
		t.Fatalf("descriptor was not recycled: got %d, want %d", replacementFD, closedFD)
	}

	if _, _, err := root.Stat(); err == nil {
		t.Fatal("closed Root.Stat succeeded through a recycled descriptor")
	}
}

func TestA24OwnedDuplicateIsCloseOnExec(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, err := root.OpenParent([]byte("entry"))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	flags, err := unix.FcntlInt(uintptr(parent.DirFD()), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("owned duplicate can leak into an executed child process")
	}
}
