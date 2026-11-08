package scope

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

func int64p(v int64) *int64    { return &v }
func uint64p(v uint64) *uint64 { return &v }

// baseRoot is a stored identity with every field filled in, so each test can
// change exactly one fact.
func baseRoot() domain.RootIdentity {
	return domain.RootIdentity{
		Persistent: domain.PersistentRootIdentity{
			VolumeID:    "dev-16777229",
			Inode:       8611,
			BirthTimeNS: int64p(1_700_000_000_000_000_000),
			Generation:  uint64p(7),
			OwnerUID:    501,
			FileSystem:  "apfs",
		},
		Runtime: domain.RuntimeMountIdentity{
			BootOrRunID: "boot-a",
			Device:      16777229,
			MountID:     uint64p(42),
		},
	}
}

// A normal restart compares every field and finds no change. The positive case
// matters as much as the refusals: R02 must not cost a legitimate authorisation.
func TestIdenticalIdentityIsAccepted(t *testing.T) {
	if v := CompareRootIdentity(baseRoot(), baseRoot()); !v.OK() {
		t.Fatalf("identical identity refused: %+v", v)
	}
}

// A new boot changes the runtime half only. Mount IDs are meaningless across a
// reboot, so this must not be treated as a change.
func TestNewBootIsNotAChange(t *testing.T) {
	stored := baseRoot()
	current := baseRoot()
	current.Runtime.BootOrRunID = "boot-b"
	current.Runtime.MountID = uint64p(9999)
	if v := CompareRootIdentity(stored, current); !v.OK() {
		t.Fatalf("a reboot was treated as a change: %+v", v)
	}
	if v := CompareMountBinding(stored.Runtime, current.Runtime); !v.OK() {
		t.Fatalf("a reboot was treated as a remount: %+v", v)
	}
}

// The same path reached through a different mount inside one boot is a change.
func TestRemountWithinOneBootIsAChange(t *testing.T) {
	stored := baseRoot()
	current := baseRoot()
	current.Runtime.MountID = uint64p(43)
	if v := CompareMountBinding(stored.Runtime, current.Runtime); v.Changed == "" {
		t.Fatal("a different mount within one boot was accepted")
	}
}

// The four R02 scenarios: same dev/inode, but the object behind it is not the
// registered one.
func TestEveryStrongFieldCanRefuseTheObject(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.RootIdentity)
	}{
		{"inode reused", func(r *domain.RootIdentity) { r.Persistent.Inode++ }},
		{"volume swapped", func(r *domain.RootIdentity) {
			r.Persistent.VolumeID = "dev-16777230"
			r.Runtime.Device = 16777230
		}},
		{"creation time differs", func(r *domain.RootIdentity) {
			r.Persistent.BirthTimeNS = int64p(1_700_000_000_000_000_001)
		}},
		{"generation differs", func(r *domain.RootIdentity) { r.Persistent.Generation = uint64p(8) }},
		{"owner differs", func(r *domain.RootIdentity) { r.Persistent.OwnerUID = 0 }},
		{"filesystem differs", func(r *domain.RootIdentity) { r.Persistent.FileSystem = "ext4" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := baseRoot()
			tc.mutate(&current)
			if v := CompareRootIdentity(baseRoot(), current); v.Changed == "" {
				t.Fatalf("%s was accepted as the registered root: %+v", tc.name, v)
			}
		})
	}
}

// A stored owner of uid 0 is a real value, not "unset". The old comparison skipped
// the check in exactly that case, which is the bug R02 names.
func TestStoredRootOwnerZeroIsCompared(t *testing.T) {
	stored := baseRoot()
	stored.Persistent.OwnerUID = 0
	current := baseRoot()
	current.Persistent.OwnerUID = 501
	if v := CompareRootIdentity(stored, current); v.Changed == "" {
		t.Fatal("a directory re-chowned away from uid 0 was accepted")
	}
}

// When nothing distinguishes one object from another that reuses the inode, the
// answer is "weak", not "fine": observing and planning continue, automatic
// permanent deletion stops.
func TestEvidenceWithoutBirthTimeOrGenerationIsWeak(t *testing.T) {
	stored := baseRoot()
	stored.Persistent.BirthTimeNS = nil
	stored.Persistent.Generation = nil
	current := stored
	current.Runtime = baseRoot().Runtime

	v := CompareRootIdentity(stored, current)
	if v.Changed != "" {
		t.Fatalf("thin evidence was reported as a change: %+v", v)
	}
	if v.Weak == "" {
		t.Fatal("an identity with no creation time and no generation was treated as strong")
	}
}

// TestLockHolderHelper is the child half of the cross-task test below. It takes
// the scope lock with plain flock and then waits for stdin to close.
//
// A real second process is required here and not just for realism: BSD flock is
// process-scoped, so a second flock from this process on a different descriptor
// succeeds and proves nothing. The exclusion that matters is between two
// installations, and only another process can express that.
func TestLockHolderHelper(t *testing.T) {
	path := os.Getenv("DUSTKEEPER_TEST_HOLD_LOCK")
	if path == "" {
		t.Skip("helper process only")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock %s: %v", path, err)
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// Holding the scope lock in another process is enough to stop this one before any
// business action: the V02 case, across processes.
func TestHeldScopeLockBlocksTheNextTask(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	lockPath := filepath.Join(dir, domain.VaultDirName, domain.VaultScopeLock)
	cmd := exec.Command(os.Args[0], "-test.run=TestLockHolderHelper", "-test.v=false")
	cmd.Env = append(os.Environ(), "DUSTKEEPER_TEST_HOLD_LOCK="+lockPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()

	// Wait for the child to report that it holds the lock.
	ready := make([]byte, 0, 64)
	buf := make([]byte, 16)
	for !strings.Contains(string(ready), "locked") {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			ready = append(ready, buf[:n]...)
		}
		if rerr != nil {
			t.Fatalf("the helper never took the lock: %v (%q)", rerr, ready)
		}
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("a second task opened the vault while another process held the scope lock")
	} else if code := errs.CodeOf(err); code != errs.CodeInstanceBusy {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeInstanceBusy, err)
	}

	// Release the other process and the scope must become usable again.
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
	if _, err := openVaultFor(t, reg, "docs"); err != nil {
		t.Fatalf("the scope did not become usable after the lock was released: %v", err)
	}
}

// Opening the same scope twice inside one process must succeed: reconcile opens a
// scope once per pending operation. It shares one flock, and the last handle to
// close releases it.
func TestSameProcessReopensShareTheLock(t *testing.T) {
	resetScopeLocks(t)
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	first, err := openVaultFor(t, reg, "docs")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := openVaultFor(t, reg, "docs")
	if err != nil {
		t.Fatalf("second open in the same process: %v", err)
	}
	if got := heldLockRefs(t); got != 2 {
		t.Fatalf("held scope lock references = %d, want 2", got)
	}

	second.Close()
	if got := heldLockRefs(t); got != 1 {
		t.Fatalf("held scope lock references = %d after one close, want 1", got)
	}

	first.Close()
	if got := heldLockRefs(t); got != 0 {
		t.Fatalf("held scope lock references = %d after the last close, want 0", got)
	}
	// And the lock file is actually free again for another process.
	f, err := os.OpenFile(filepath.Join(dir, domain.VaultDirName, domain.VaultScopeLock),
		os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("reopen lock file: %v", err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("the scope lock was not released by the last handle: %v", err)
	}
}

// heldLockRefs reports how many references this process holds across all scopes.
func heldLockRefs(t *testing.T) int {
	t.Helper()
	scopeLocksMu.Lock()
	defer scopeLocksMu.Unlock()
	total := 0
	for _, held := range scopeLocks {
		total += held.refs
	}
	return total
}

// resetScopeLocks clears this process's registry so a lock assertion only sees
// the handles the current test opened.
func resetScopeLocks(t *testing.T) {
	t.Helper()
	clear := func() {
		scopeLocksMu.Lock()
		defer scopeLocksMu.Unlock()
		for key, held := range scopeLocks {
			held.file.Close()
			delete(scopeLocks, key)
		}
	}
	clear()
	t.Cleanup(clear)
}

// The probe must not leave the management directory dirty, and fsops must not
// report ErrNotPrivate for the objects registration itself created.
func TestManagementObjectsSatisfyTheContract(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	vs, err := reg.OpenVerified(context.Background(), "docs")
	if err != nil {
		t.Fatalf("open verified: %v", err)
	}
	defer vs.Root.Close()

	h, err := OpenVault(vs.Root, VaultExpect{
		OwnerUID:   CurrentOwnerUID(),
		RootDev:    vs.Root.Dev(),
		InstanceID: reg.InstanceID,
		Creating:   true,
	})
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	defer h.Close()

	expect := VaultExpect{OwnerUID: CurrentOwnerUID(), RootDev: vs.Root.Dev()}
	for _, d := range []*fsops.Dir{h.Vault, h.Objects, h.Manifests, h.Tmp} {
		if err := d.VerifyPrivate(expect.For(true)); err != nil {
			t.Fatalf("%s: %v", d.Rel(), err)
		}
	}
}

// A boot can renumber the APFS data volume's runtime device without replacing
// the directory. The old build treated `dev-<st_dev>` as a persistent volume
// UUID and held the scope; the fix keeps every persistent fact strict and only
// relaxes the two runtime facts when the boot actually changed.
func TestBootRelabelIsReportedNotHeld(t *testing.T) {
	stored := baseRoot()
	current := baseRoot()
	current.Runtime.BootOrRunID = "boot-b"
	current.Persistent.VolumeID = "dev-16777231"
	current.Runtime.Device = 16777231
	v := CompareRootIdentity(stored, current)
	if v.Changed != "" || v.Weak != "" || v.RuntimeRelabeled == "" {
		t.Fatalf("a proven boot relabel was mishandled: %+v", v)
	}
	if !RootBootChanged(stored, current) {
		t.Fatal("the fixture did not actually change boot")
	}
}

// The relaxation must NOT hide a real replacement: a different inode, creation
// time, owner or filesystem still refuses, even across boots and even when both
// labels have the legacy shape.
func TestBootRelabelStillChecksEveryPersistentFact(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.RootIdentity)
	}{
		{"inode reused", func(r *domain.RootIdentity) { r.Persistent.Inode++ }},
		{"creation time differs", func(r *domain.RootIdentity) {
			r.Persistent.BirthTimeNS = int64p(1_700_000_000_000_000_001)
		}},
		{"generation differs", func(r *domain.RootIdentity) { r.Persistent.Generation = uint64p(8) }},
		{"owner differs", func(r *domain.RootIdentity) { r.Persistent.OwnerUID = 0 }},
		{"filesystem differs", func(r *domain.RootIdentity) { r.Persistent.FileSystem = "ext4" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := baseRoot()
			current := baseRoot()
			current.Runtime.BootOrRunID = "boot-b"
			current.Persistent.VolumeID = "dev-16777231"
			current.Runtime.Device = 16777231
			tc.mutate(&current)
			if v := CompareRootIdentity(stored, current); v.Changed == "" {
				t.Fatalf("%s was accepted during a boot relabel: %+v", tc.name, v)
			}
		})
	}
}

// A device change inside ONE boot is a mount replacement, not a relabel. The
// root-cause fix must not open that door.
func TestSameBootDeviceChangeIsStillRefused(t *testing.T) {
	stored := baseRoot()
	current := baseRoot()
	current.Persistent.VolumeID = "dev-16777231"
	current.Runtime.Device = 16777231
	if v := CompareRootIdentity(stored, current); v.Changed == "" || v.RuntimeRelabeled != "" {
		t.Fatalf("a same-boot device change was accepted: %+v", v)
	}
}

// A non-legacy (stable) volume id remains comparable across boots. Only the
// `dev-*` shape is runtime-scoped.
func TestStableVolumeIDIsComparedAcrossBoots(t *testing.T) {
	stored := baseRoot()
	stored.Persistent.VolumeID = "08317A4F-48DB-4A31-86DC-9FCA86B9B58B"
	current := baseRoot()
	current.Persistent.VolumeID = "AAAAAAAA-48DB-4A31-86DC-9FCA86B9B58B"
	current.Runtime.BootOrRunID = "boot-b"
	if v := CompareRootIdentity(stored, current); v.Changed == "" {
		t.Fatalf("a stable volume id change was ignored: %+v", v)
	}
	if !LegacyVolumeLabel("dev-16777231") || LegacyVolumeLabel("08317A4F-48DB-4A31") {
		t.Fatal("legacy label detection does not match the format the platform writes")
	}
	if n, ok := LegacyDevice("dev-16777231"); !ok || n != 16777231 {
		t.Fatalf("legacy device decoding failed: %d %v", n, ok)
	}
}
