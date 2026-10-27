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

// TestANonZeroFlagKeeperIsPurgedNormally is the Linux half of R09 B.
//
// The planning side used to build the metadata digest from the change token,
// which carries no inode flags at all, while the re-check read the real flags
// through the descriptor. An untouched file with any flag set therefore looked
// modified, and the object could never be emptied. The fix reads both sides from
// the same fd evidence; this test proves that an unchanged file with a real,
// non-zero flag set still reaches a normal purge.
func TestANonZeroFlagKeeperIsPurgedNormally(t *testing.T) {
	e := newEnv(t)
	src := e.addScope("dups", domain.KindAssisted).ID
	keeperScope := e.addScope("originals", domain.KindReference).ID

	e.writeFile("dups", "one.bin", "same-bytes", 0)
	e.writeFile("dups", "two.bin", "same-bytes", 0)
	e.writeFile("originals", "keep.bin", "same-bytes", 0)

	// Both copies must carry the same flags: the duplicate policy already refuses
	// to pair files whose metadata differs, and that rule is not being relaxed
	// here. What is being proved is that equal, non-zero flags survive a purge.
	for _, rel := range []string{"dups/one.bin", "dups/two.bin", "originals/keep.bin"} {
		if !setInodeFlag(t, filepath.Join(e.base, "trees", rel)) {
			t.Skip("this host cannot set an inode flag, so the non-zero flags case cannot be built")
		}
	}
	for _, rel := range []string{"dups/one.bin", "dups/two.bin", "originals/keep.bin"} {
		if flags := inodeFlags(t, filepath.Join(e.base, "trees", rel)); flags == 0 {
			t.Fatalf("%s reports no flags; the fixture did not take", rel)
		}
	}

	e.scan(src)
	e.scan(keeperScope)

	sctx := e.scopeCtx(src)
	ks := keeperScope
	res, err := e.det.Detect(context.Background(), sctx, &ks)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		t.Fatalf("expected one plannable group despite equal non-zero flags, got %d (%v)",
			res.PlannableGroups, res.Notes)
	}
	p, items, err := e.det.CreatePlan(context.Background(), sctx, res.Groups)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected two copies planned, got %d", len(items))
	}
	sc, err := e.st.GetScope(context.Background(), src)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if _, err := e.apply(sc, p, p.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), src, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) == 0 {
		t.Fatal("no duplicate became due")
	}
	item := due[0]

	if item.MetadataHash == nil {
		t.Fatal("the quarantined object carries no metadata digest")
	}
	t.Logf("inode flags %#x; recorded metadata digest %s", inodeFlags(t, e.keeperPath()), *item.MetadataHash)

	if err := e.tryPurge(sc, item); err != nil {
		t.Fatalf("an unchanged keeper with non-zero flags blocked the purge: %v", err)
	}
	if e.vaultObjectExists(sc, item) {
		t.Fatal("the payload is still in the vault")
	}
	if action := e.lastIntentAction(item.ID); action != "purge" {
		t.Fatalf("expected a purge intent, got %q", action)
	}
	if flags := inodeFlags(t, e.keeperPath()); flags == 0 {
		t.Fatalf("the keeper lost its flags: %#x", flags)
	}
}
