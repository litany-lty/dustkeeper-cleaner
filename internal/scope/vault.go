package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

// maxMarkerBytes bounds the owner marker read. The marker is a small JSON
// object; anything larger is not one, and reading it would only waste memory.
const maxMarkerBytes = 64 << 10

// maxNonceBytes bounds the creation-evidence read. The value is an ID, not a
// document.
const maxNonceBytes = 256

// VaultExpect says what the private management directory of one scope must be.
//
// The same struct is used when the directory is created and on every later open,
// so that "what registration wrote" and "what the executor accepts" cannot drift
// apart. R01 lists three separate symptoms of that drift: a reused directory, an
// anchor that was never checked, and a marker nobody re-read.
type VaultExpect struct {
	// OwnerUID is the uid every management object must be owned by.
	OwnerUID uint32
	// RootDev is the device of the scope root; management objects must be on it.
	RootDev uint64
	// InstanceID is the instance owner.json must name. Empty skips the check,
	// which is only correct while creating the marker.
	InstanceID string
	// Identity is the vault identity digest stored on the scope row.
	Identity string
	// Creating says the caller is verifying a directory it has just built, so
	// there is no recorded identity to compare against yet. It exists so that
	// "no identity recorded" cannot quietly mean "skip the comparison": R01 is a
	// list of checks that were present but not enforced, and an empty string is
	// the easiest way to reintroduce one.
	Creating bool
	// Lock asks OpenVault to take the in-tree scope mutex before validating
	// anything else. It is set by every caller that could change a business file,
	// which is what makes two state directories mutually exclusive over one tree.
	Lock bool
	// ScopeID, RootInode, VolumeID and ScopeKind are what the vault binding must
	// name. They are empty only while the vault is being created, and the binding
	// is compared against them on every later open: a management directory that
	// belongs to a different scope row is not a directory this run may use.
	ScopeID   string
	RootInode uint64
	VolumeID  string
	ScopeKind string
}

// For returns the fsops contract for one object inside the vault.
func (v VaultExpect) For(isDir bool) fsops.PrivateExpect {
	e := fsops.PrivateExpect{OwnerUID: v.OwnerUID, MaxPerm: 0o600, Links: 1, RootDev: v.RootDev}
	if isDir {
		// A directory's link count is structural (2 plus one per subdirectory),
		// so it carries no evidence here; the permission and owner bits do.
		e.MaxPerm = 0o700
		e.Links = 0
	}
	return e
}

// VaultBindV1 is the record that ties one objects directory to one scope row.
//
// It is a separate file from the owner marker on purpose. The marker answers
// "which instance owns this tree" and is read by registration's ancestor probe;
// the binding answers "which scope row describes this directory", which is the
// question a swapped or restored directory fails. Keeping them apart means the
// ancestor probe never has to understand a scope row it cannot see.
type VaultBindV1 struct {
	FormatVersion   int    `json:"format_version"`
	ScopeID         string `json:"scope_id"`
	RootInode       uint64 `json:"root_inode"`
	VolumeID        string `json:"volume_id"`
	ScopeKind       string `json:"kind"`
	ObjectsNonce    string `json:"objects_nonce"`
	ObjectsIdentity string `json:"objects_identity"`
	AnchorTag       string `json:"anchor_tag"`
	AnchorIdentity  string `json:"anchor_identity"`
}

// anchorTagOffset is where the anchor's identifying value lives. Offset zero is
// where every barrier writes its marker, so the tag sits past it.
const anchorTagOffset = 4096

// VaultHandles are the opened and validated private objects of a mutable scope.
// Close releases them in the order they were opened.
type VaultHandles struct {
	Vault     *fsops.Dir
	Objects   *fsops.Dir
	Manifests *fsops.Dir
	Tmp       *fsops.Dir
	Anchor    *fsops.File
	// releaseLock drops this handle's reference to the in-tree scope mutex.
	releaseLock func()
	Identity    string
}

// Close releases every handle; nil handles are ignored. The directories are
// closed before the lock so the lock is held for the whole lifetime of the
// handles it protects.
func (h *VaultHandles) Close() {
	if h == nil {
		return
	}
	for _, d := range []*fsops.Dir{h.Objects, h.Manifests, h.Tmp, h.Vault} {
		if d != nil {
			d.Close()
		}
	}
	if h.Anchor != nil {
		h.Anchor.Close()
	}
	if h.releaseLock != nil {
		h.releaseLock()
		h.releaseLock = nil
	}
}

// OpenVault opens the private directory of a mutable scope and proves it is the
// directory the scope row describes.
//
// Everything is opened from the already-verified root descriptor, so no check
// here is separated from the object it validates by a path lookup. The order is
// deliberate: directories and their permissions first, then the owner marker,
// then the anchor's link count, and only then the vault identity comparison -
// because a wrong anchor is the case where a later barrier would write into
// business data.
func OpenVault(root *fsops.Root, expect VaultExpect) (*VaultHandles, error) {
	if expect.RootDev == 0 {
		// RootDev is the device of the verified root. A zero value means the
		// caller did not pass a verified root, and every same-volume check below
		// would silently pass.
		return nil, errs.New(errs.CodeAccessDenied, "vault contract needs a verified root device")
	}
	if expect.Identity == "" && !expect.Creating {
		// Nothing ties this directory to a scope row, so every retained object
		// would be addressed by an identity nobody recorded.
		return nil, errs.New(errs.CodeScopeChanged, "vault contract needs the recorded identity of the scope")
	}

	h := &VaultHandles{}
	fail := func(err error) (*VaultHandles, error) {
		h.Close()
		return nil, err
	}

	var err error
	if h.Vault, err = root.OpenDir([]byte(domain.VaultDirName)); err != nil {
		return nil, errs.Newf(errs.CodeScopeChanged, "management directory is missing: %v", err)
	}
	if err := h.Vault.VerifyPrivate(expect.For(true)); err != nil {
		return fail(refuse("management directory", err))
	}

	// The lock is taken before anything else is validated and held until Close.
	// Order is instance lock -> scope lock -> control lock; inverting it would let
	// two runs hold one scope while both wait for the other's instance lock.
	if expect.Lock {
		release, lerr := acquireScopeLock(h.Vault)
		if lerr != nil {
			return fail(lerr)
		}
		h.releaseLock = release
	}

	for _, sub := range []struct {
		rel  string
		dst  **fsops.Dir
		name string
	}{
		{domain.VaultObjects, &h.Objects, "objects"},
		{domain.VaultManifests, &h.Manifests, "manifests"},
		{domain.VaultTmp, &h.Tmp, "tmp"},
	} {
		d, err := root.OpenDir([]byte(sub.rel))
		if err != nil {
			return fail(errs.Newf(errs.CodeScopeChanged, "%s directory is missing: %v", sub.name, err))
		}
		*sub.dst = d
		if err := d.VerifyPrivate(expect.For(true)); err != nil {
			return fail(refuse(sub.name+" directory", err))
		}
	}

	if err := verifyMarker(h.Vault, expect); err != nil {
		return fail(err)
	}

	anchor, err := root.OpenFileRW([]byte(domain.VaultAnchor))
	if err != nil {
		return fail(errs.Newf(errs.CodeScopeChanged, "durability anchor is missing: %v", err))
	}
	h.Anchor = anchor
	if err := anchor.VerifyPrivate("durability anchor", expect.For(false)); err != nil {
		return fail(refuse("durability anchor", err))
	}
	// A barrier issued on the anchor is only meaningful when the anchor shares
	// the volume with the objects it flushes. A cross-device anchor would flush
	// a different device and report success.
	same, err := h.Anchor.SameDeviceAs(h.Objects)
	if err != nil {
		return fail(errs.Wrap(errs.CodeAccessDenied, err))
	}
	if !same {
		return fail(errs.New(errs.CodeScopeChanged,
			"the durability anchor is on a different device than the objects directory"))
	}

	bind, err := readBind(h.Vault, expect)
	if err != nil {
		return fail(err)
	}

	// The nonce lives inside the objects directory, so it is the one fact that a
	// replacement cannot carry over: deleting and recreating the directory, or
	// restoring it from a snapshot taken before the nonce existed, loses it.
	nonce, err := h.Objects.ReadFileAt(domain.VaultNonceName, maxNonceBytes)
	if err != nil {
		return fail(errs.Newf(errs.CodeScopeChanged,
			"the objects directory carries no creation evidence: %v", err))
	}
	if string(nonce) != bind.ObjectsNonce {
		return fail(errs.New(errs.CodeScopeChanged,
			"the objects directory was not created by this vault"))
	}

	anchorID, _, err := h.Anchor.Stat()
	if err != nil {
		return fail(errs.Wrap(errs.CodeAccessDenied, err))
	}
	tag, err := fsops.ReadTag(h.Anchor.FD(), anchorTagOffset, maxNonceBytes)
	if err != nil {
		return fail(errs.Wrap(errs.CodeAccessDenied, err))
	}
	if string(tag) != bind.AnchorTag || AnchorIdentity(anchorID, string(tag)) != bind.AnchorIdentity {
		return fail(errs.New(errs.CodeScopeChanged,
			"the durability anchor is not the one this vault was registered with"))
	}

	identity, err := VaultIdentity(h.Objects, string(nonce))
	if err != nil {
		return fail(err)
	}
	if identity != bind.ObjectsIdentity {
		return fail(errs.New(errs.CodeScopeChanged,
			"the objects directory is not the one this vault was bound to"))
	}
	if expect.Identity != "" && expect.Identity != identity {
		// The scope row names a different vault. Re-registering is the only way to
		// accept the new object directory, because every retained object is
		// addressed by the old one.
		return fail(errs.Newf(errs.CodeScopeChanged,
			"the objects directory belongs to a different vault (%s, want %s)", identity, expect.Identity))
	}
	h.Identity = identity
	return h, nil
}

func refuse(what string, err error) error {
	return errs.Newf(errs.CodeScopeChanged, "%s is not a private management object: %v", what, err)
}

// verifyMarker proves the owner marker exists, is a private file, and names this
// instance. The content is read through the validated descriptor, not by path.
func verifyMarker(vault *fsops.Dir, expect VaultExpect) error {
	const name = "owner.json"
	if _, _, err := vault.StatEntryPrivate(name, expect.For(false)); err != nil {
		return refuse("owner marker", err)
	}
	body, err := vault.ReadFileAt(name, maxMarkerBytes)
	if err != nil {
		return refuse("owner marker", err)
	}
	var m OwnerMarker
	if err := json.Unmarshal(body, &m); err != nil {
		return errs.Newf(errs.CodeScopeChanged, "the owner marker is unreadable: %v", err)
	}
	if m.FormatVersion != 1 {
		return errs.Newf(errs.CodeScopeChanged,
			"the owner marker has format version %d, this build understands 1", m.FormatVersion)
	}
	if expect.InstanceID != "" && m.InstanceID != expect.InstanceID {
		return errs.Newf(errs.CodeScopeOverlap,
			"this tree is managed by another Dustkeeper instance (%s)", m.InstanceID)
	}
	// The marker names the scope, root and kind it was written for. Comparing
	// them is what turns "a marker exists" into "this marker describes this
	// tree": a marker copied from another scope has the right shape and the
	// wrong subject.
	switch {
	case expect.ScopeID != "" && m.ScopeID != expect.ScopeID:
		return errs.Newf(errs.CodeScopeChanged,
			"the owner marker names scope %q, this scope is %q", m.ScopeID, expect.ScopeID)
	case expect.VolumeID != "" && m.VolumeID != expect.VolumeID:
		return errs.Newf(errs.CodeScopeChanged,
			"the owner marker was written on volume %q, this volume is %q", m.VolumeID, expect.VolumeID)
	case expect.RootInode != 0 && m.RootInode != expect.RootInode:
		return errs.New(errs.CodeScopeChanged,
			"the owner marker was written for a different root directory")
	case expect.ScopeKind != "" && m.Kind != expect.ScopeKind:
		return errs.Newf(errs.CodeScopeChanged,
			"the owner marker was written for a %q scope, this scope is %q", m.Kind, expect.ScopeKind)
	}
	return nil
}

// readBind loads and checks the vault binding against the scope contract.
func readBind(vault *fsops.Dir, expect VaultExpect) (VaultBindV1, error) {
	var b VaultBindV1
	if _, _, err := vault.StatEntryPrivate("vault-bind.json", expect.For(false)); err != nil {
		return b, refuse("vault binding", err)
	}
	body, err := vault.ReadFileAt("vault-bind.json", maxMarkerBytes)
	if err != nil {
		return b, refuse("vault binding", err)
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return b, errs.Newf(errs.CodeScopeChanged, "the vault binding is unreadable: %v", err)
	}
	if b.FormatVersion != 1 {
		return b, errs.Newf(errs.CodeScopeChanged,
			"the vault binding has format version %d, this build understands 1", b.FormatVersion)
	}
	if b.ObjectsNonce == "" || b.ObjectsIdentity == "" || b.AnchorIdentity == "" {
		return b, errs.New(errs.CodeScopeChanged, "the vault binding records no identity")
	}
	switch {
	case expect.ScopeID != "" && b.ScopeID != expect.ScopeID:
		return b, errs.Newf(errs.CodeScopeChanged,
			"this management directory belongs to scope %q, not %q", b.ScopeID, expect.ScopeID)
	case expect.VolumeID != "" && b.VolumeID != expect.VolumeID:
		return b, errs.Newf(errs.CodeScopeChanged,
			"this management directory was created on volume %q, not %q", b.VolumeID, expect.VolumeID)
	case expect.RootInode != 0 && b.RootInode != expect.RootInode:
		return b, errs.New(errs.CodeScopeChanged,
			"this management directory was created inside a different root directory")
	case expect.ScopeKind != "" && b.ScopeKind != expect.ScopeKind:
		return b, errs.Newf(errs.CodeScopeChanged,
			"this management directory was created for a %q scope, not %q", b.ScopeKind, expect.ScopeKind)
	}
	return b, nil
}

// VaultIdentity derives the digest that identifies one objects directory.
//
// It is a function rather than a copy of the expression at each call site so
// that registration and every later open hash exactly the same facts.
func VaultIdentity(objects *fsops.Dir, nonce string) (string, error) {
	id, _, err := objects.Stat()
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	birth, birthOK, generation, generationOK := fsops.CreationEvidence(objects.DirFD())
	payload := fmt.Sprintf("%s|%d|%d|%s|%s", id.VolumeID, id.Device, id.Inode,
		optionalInt(birth, birthOK), optionalUint(generation, generationOK))
	sum := sha256.Sum256([]byte(payload))
	return "vault:" + hex.EncodeToString(sum[:]), nil
}

// FormatVaultIdentity digests the facts that identify one management object.
//
// The device and inode alone cannot distinguish a replacement: a filesystem may
// hand the same inode number to the directory that replaced the deleted one.
// The nonce carried inside the objects directory closes that, and VaultIdentity
// folds in the birth time and generation as well when the platform has them.
func FormatVaultIdentity(id domain.FileIdentity, nonce string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s", id.VolumeID, id.Device, id.Inode, nonce)))
	return "vault:" + hex.EncodeToString(sum[:])
}

// AnchorIdentity names the durability anchor.
//
// Device and inode are not enough on their own: a deleted and recreated anchor
// can be handed the same inode number, and then a barrier would be issued on a
// file this vault never installed. The tag written past the barrier area is what
// closes that, because a replacement does not have it.
func AnchorIdentity(id domain.FileIdentity, tag string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("anchor|%s|%d|%d|%s", id.VolumeID, id.Device, id.Inode, tag)))
	return "anchor:" + hex.EncodeToString(sum[:])
}

func optionalInt(v int64, ok bool) string {
	if !ok {
		return "none"
	}
	return fmt.Sprintf("%d", v)
}

func optionalUint(v uint64, ok bool) string {
	if !ok {
		return "none"
	}
	return fmt.Sprintf("%d", v)
}

// CurrentOwnerUID is the uid that must own every management object.
func CurrentOwnerUID() uint32 { return uint32(os.Geteuid()) }

// IsNotPrivate reports whether err says a management object failed the contract.
func IsNotPrivate(err error) bool { return errors.Is(err, fsops.ErrNotPrivate) }
