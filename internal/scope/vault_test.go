package scope

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

// The tests in this file are the R01 acceptance cases. Each one describes a
// management directory that is already wrong before Dustkeeper touches it, and
// every assertion is "the business tree is byte-identical afterwards".

// sentinel plants a file outside the scope whose bytes must never change.
func sentinel(t *testing.T, base, name string) (string, []byte) {
	t.Helper()
	dir := filepath.Join(base, "outside")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	p := filepath.Join(dir, name)
	body := []byte("BUSINESS DATA - must never be truncated or written")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	return p, body
}

func readAll(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

// a pre-placed owner.json.tmp symlink must not be followed and truncated. The
// old writer opened "<marker>.tmp" with O_CREAT|O_TRUNC, which is exactly the
// first R01 scenario: the truncation happened before any check could run.
func TestRegistrationNeverFollowsAPlacedTempSymlink(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	sentinelPath, want := sentinel(t, base, "precious.txt")

	vault := filepath.Join(dir, domain.VaultDirName)
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	// The name the old implementation used for its staging file.
	if err := os.Symlink(sentinelPath, filepath.Join(vault, "owner.json.tmp")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "managed_disposable", Name: "docs"}); err != nil {
		t.Fatalf("registration should succeed without touching the placed temp entry: %v", err)
	}
	if got := readAll(t, sentinelPath); string(got) != string(want) {
		t.Fatalf("the placed temporary symlink was followed: sentinel is now %q", got)
	}
}

// a durability anchor that is a hard link to a business file must be refused.
// A barrier on such an anchor issues F_FULLFSYNC (and a Pwrite) against the
// business file's own bytes.
func TestRegistrationRefusesAnAnchorHardLink(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	business := filepath.Join(dir, "invoice.bin")
	want := []byte("INVOICE BYTES")
	if err := os.WriteFile(business, want, 0o600); err != nil {
		t.Fatalf("write business file: %v", err)
	}

	vault := filepath.Join(dir, domain.VaultDirName)
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}
	if err := os.Link(business, filepath.Join(vault, "durability.anchor")); err != nil {
		t.Fatalf("hard link: %v", err)
	}

	_, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "managed_disposable", Name: "docs"})
	if err == nil {
		t.Fatal("registration accepted an anchor that is a hard link to a business file")
	}
	if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
	if got := readAll(t, business); string(got) != string(want) {
		t.Fatalf("the business file was modified through the anchor link: %q", got)
	}
	if exists, xerr := reg.Store.ScopeExists(context.Background(), "docs"); xerr != nil || exists {
		t.Fatalf("a refused registration must not leave a scope row (exists=%v, err=%v)", exists, xerr)
	}
}

// openVaultFor re-opens a registered scope's vault the way the executor does.
func openVaultFor(t *testing.T, reg *Registrar, id domain.ScopeID) (*VaultHandles, error) {
	t.Helper()
	vs, err := reg.OpenVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("open verified: %v", err)
	}
	t.Cleanup(func() { vs.Root.Close() })

	sc, err := reg.Store.GetScope(context.Background(), id)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	want := ""
	if sc.VaultIdentityJSON != nil {
		want = *sc.VaultIdentityJSON
	}
	// The helper carries the same contract the executor passes. A helper that
	// passed fewer facts would let a test pass while the real entry point
	// refused, which is the drift the review keeps finding.
	return OpenVault(vs.Root, VaultExpect{
		OwnerUID:   CurrentOwnerUID(),
		RootDev:    vs.Root.Dev(),
		InstanceID: reg.InstanceID,
		Identity:   want,
		Lock:       true,
		ScopeID:    string(id),
		RootInode:  vs.RootID.Persistent.Inode,
		VolumeID:   vs.RootID.Persistent.VolumeID,
		ScopeKind:  sc.Kind,
	})
}

func mustRegister(t *testing.T, reg *Registrar, dir, name string) {
	t.Helper()
	if _, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "managed_disposable", Name: name}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

// A normal registration re-opens cleanly: the fix must not make the positive
// path fail. This is the counterweight to every refusal test below.
func TestRegisteredVaultReopensCleanly(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	h, err := openVaultFor(t, reg, "docs")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer h.Close()
	if h.Identity == "" || h.Objects == nil || h.Anchor == nil {
		t.Fatalf("incomplete vault handles: %+v", h)
	}
}

// Replacing the objects directory changes the vault identity, so the scope row no
// longer describes the directory that would be used.
func TestReplacedObjectsDirectoryIsRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	// A replacement is the whole directory, including the creation evidence it
	// carries. Removing only part of it would leave the test asserting against a
	// directory nobody would actually end up with.
	objects := filepath.Join(dir, domain.VaultDirName, "objects")
	if err := os.RemoveAll(objects); err != nil {
		t.Fatalf("remove objects: %v", err)
	}
	if err := os.Mkdir(objects, 0o700); err != nil {
		t.Fatalf("recreate objects: %v", err)
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("a replaced objects directory was accepted")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
}

// A01: the directory is the same inode and device, but the creation evidence
// inside it is not the value this vault minted. Inode reuse after a delete and
// recreate is exactly this shape, and it is the reason the nonce exists at all.
func TestVaultWithADifferentCreationNonceIsRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	nonce := filepath.Join(dir, domain.VaultDirName, "objects", domain.VaultNonceName)
	if err := os.WriteFile(nonce, []byte("vault_someone_elses_creation"), 0o600); err != nil {
		t.Fatalf("rewrite nonce: %v", err)
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("a different creation nonce was accepted")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
}

// A01: a marker copied from another scope has the right shape and the wrong
// subject. The scope, root and kind it names are part of the contract.
func TestVaultWhoseMarkerNamesAnotherScopeIsRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	marker := filepath.Join(dir, domain.VaultDirName, "owner.json")
	var m OwnerMarker
	body := readAll(t, marker)
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	m.ScopeID = "somewhere-else"
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode marker: %v", err)
	}
	if err := os.WriteFile(marker, out, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("a marker naming a different scope was accepted")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
}

// A01: the anchor is rewritten by every barrier, so its bytes cannot identify
// it. A replaced anchor is a different file, and a barrier on it would flush
// something other than the objects it is supposed to make durable.
func TestVaultWhoseAnchorWasReplacedIsRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	anchor := filepath.Join(dir, domain.VaultAnchor)
	if err := os.Remove(anchor); err != nil {
		t.Fatalf("remove anchor: %v", err)
	}
	if err := os.WriteFile(anchor, nil, 0o600); err != nil {
		t.Fatalf("recreate anchor: %v", err)
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("a replaced durability anchor was accepted")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
}

// A marker that is no longer a marker must stop the scope before any action.
func TestCorruptOwnerMarkerIsRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	marker := filepath.Join(dir, domain.VaultDirName, "owner.json")
	if err := os.WriteFile(marker, []byte("not a marker"), 0o600); err != nil {
		t.Fatalf("corrupt marker: %v", err)
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("an unreadable owner marker was accepted")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
}

// A management directory that other users can reach is not a private boundary.
func TestWidenedVaultPermissionsAreRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	vault := filepath.Join(dir, domain.VaultDirName)
	if err := os.Chmod(vault, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := openVaultFor(t, reg, "docs"); err == nil {
		t.Fatal("a world-writable management directory was accepted")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}
}

// A scope whose row records no vault identity cannot be opened for mutation: the
// identity is what ties the row to the directory. An empty string must not be a
// silent "skip the comparison", so the creation path has to say so explicitly.
func TestVaultWithoutRecordedIdentityIsRefused(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "docs")
	mustRegister(t, reg, dir, "docs")

	vs, err := reg.OpenVerified(context.Background(), "docs")
	if err != nil {
		t.Fatalf("open verified: %v", err)
	}
	defer vs.Root.Close()

	expect := VaultExpect{OwnerUID: CurrentOwnerUID(), RootDev: vs.Root.Dev()}
	if _, err := OpenVault(vs.Root, expect); err == nil {
		t.Fatal("a vault was opened without comparing any identity")
	} else if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Fatalf("code = %s, want %s (%v)", code, errs.CodeScopeChanged, err)
	}

	expect.Creating = true
	h, err := OpenVault(vs.Root, expect)
	if err != nil {
		t.Fatalf("the explicit creation path must still open: %v", err)
	}
	h.Close()
}

// Unsupported runs the probe and reports the same Missing list the CLI shows.
func TestProbeReportsRequiredCapabilities(t *testing.T) {
	dir := t.TempDir()
	caps := fsops.Probe(dir)
	if got := caps.Missing(); caps.Capable() != (len(got) == 0) {
		t.Fatalf("Capable and Missing disagree: capable=%v missing=%v", caps.Capable(), got)
	}
	if !caps.Capable() {
		t.Fatalf("this host cannot run the protocol at all: %v", caps.Missing())
	}
}
