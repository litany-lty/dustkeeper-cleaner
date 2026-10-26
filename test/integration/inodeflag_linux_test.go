//go:build linux

package integration

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// markInodeFlag sets a real inode flag that changes neither the bytes nor the
// permissions of the file, and returns the flags the descriptor reports.
//
// Linux keeps these flags outside the portable stat structure, which is why the
// scanner cannot see them from a directory entry and the executor can see them
// from a descriptor. The flag is the difference between those two facts, so it
// is what a test needs in order to tell them apart.
func markInodeFlag(t *testing.T, path string) uint64 {
	t.Helper()
	if err := exec.Command("chattr", "+d", path).Run(); err != nil {
		t.Skipf("chattr +d is unavailable here, so no inode flag can be set: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Skipf("this filesystem does not report inode flags: %v", err)
	}
	if flags == 0 {
		t.Skipf("the filesystem accepted chattr but reports no flag")
	}
	return uint64(uint32(flags))
}
