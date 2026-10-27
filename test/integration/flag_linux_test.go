//go:build linux

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
)

// fsNoAtimeFlag is FS_NOATIME_FL from linux/fs.h. x/sys wraps the ioctl numbers
// but not the flag bits, so the one value this fixture needs is spelled out.
const fsNoAtimeFlag = 0x00000080

// setInodeFlag turns on one benign inode flag and reports whether the kernel
// allowed it.
//
// Every ext4 flag needs CAP_LINUX_IMMUTABLE, so a host without it cannot build
// this fixture at all: the test then skips rather than pretending the case was
// exercised. build/test-linux.sh runs privileged, which is where this matters.
func setInodeFlag(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	fd := int(f.Fd())
	// FS_IOC_SETFLAGS takes a pointer to the *whole* flag word, not a bit to add:
	// passing the value itself fails with EFAULT in get_user, and passing only the
	// wanted bit asks the kernel to clear every other flag, which ext4 refuses for
	// the extent bit it always sets. Read, then write back with the bit added, the
	// way chattr does.
	current, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Logf("cannot read the inode flags of %s (%v)", path, err)
		return false
	}
	if err := unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, current|fsNoAtimeFlag); err != nil {
		t.Logf("cannot set an inode flag on %s (%v); the fixture needs CAP_LINUX_IMMUTABLE", path, err)
		return false
	}
	got, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil || got&fsNoAtimeFlag == 0 {
		t.Logf("the inode flag did not stick on %s (flags=%#x, %v)", path, got, err)
		return false
	}
	return true
}

// inodeFlags reads the flags the kernel reports for a file.
func inodeFlags(t *testing.T, path string) uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Fatalf("get flags %s: %v", path, err)
	}
	return uint64(uint32(flags))
}

// TestANonZeroInodeFlagDoesNotBlockThePurge is the Linux half of R09 B, kept for
// the lifecycle pipeline that this build still runs.
//
// Planning used to build the metadata digest from the change token, which
// carries no inode flags at all, while the pre-purge re-check read the real flags
// through the descriptor. An untouched quarantined file with any flag set then
// looked modified and could never be emptied. Both sides now come from the same
// fd evidence; this test proves that an unchanged object with a real, non-zero
// flag set still reaches a normal purge.
func TestANonZeroInodeFlagDoesNotBlockThePurge(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.bin", "payload", 40*24*time.Hour)

	target := filepath.Join(e.base, "trees", "exports", "old.bin")
	if !setInodeFlag(t, target) {
		t.Skip("this host cannot set an inode flag, so the non-zero flags case cannot be built")
	}
	if flags := inodeFlags(t, target); flags == 0 {
		t.Fatal("the file reports no flags; the fixture did not take")
	}

	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.bin"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if len(res.Items) != 1 {
		t.Fatalf("expected one planned item, got %d (%v)", len(res.Items), res.Reasons)
	}
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	quarantinedPath := filepath.Join(e.base, "trees", "exports", ".dustkeeper", "objects", res.Items[0].ObjectName)
	if flags := inodeFlags(t, quarantinedPath); flags == 0 {
		t.Fatalf("the rename lost the flags: %#x", flags)
	}

	e.clock.Advance(8 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected one due item, got %d", len(due))
	}
	item := due[0]

	if item.MetadataHash == nil {
		t.Fatal("the quarantined object carries no metadata digest")
	}
	t.Logf("inode flags %#x; recorded metadata digest %s", inodeFlags(t, quarantinedPath), *item.MetadataHash)

	if err := e.tryPurge(sc, item); err != nil {
		t.Fatalf("an unchanged object with non-zero flags blocked the purge: %v", err)
	}
	if e.vaultObjectExists(sc, item) {
		t.Fatal("the payload is still in the vault")
	}
	if action := e.lastIntentAction(item.ID); action != "purge" {
		t.Fatalf("expected a purge intent, got %q", action)
	}
}
