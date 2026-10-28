package scope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/store"
)

// scopeIDPattern is the only shape a scope ID may take, because it is also used
// in file names and in the DSL.
var scopeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// OwnerMarker identifies which instance manages a tree.
type OwnerMarker struct {
	FormatVersion int      `json:"format_version"`
	InstanceID    string   `json:"instance_id"`
	ScopeID       string   `json:"scope_id"`
	VolumeID      string   `json:"volume_id"`
	RootInode     uint64   `json:"root_inode"`
	Kind          string   `json:"kind"`
	CreatedAtNS   int64    `json:"created_at_ns"`
	Notes         []string `json:"notes,omitempty"`
}

// Registrar performs scope registration and verification.
type Registrar struct {
	Store      *store.Store
	Paths      config.Paths
	InstanceID string
	Now        func() int64
}

// AddOptions describes a registration request.
type AddOptions struct {
	Path string
	Kind string
	Name string
}

// Add registers a directory as a scope.
//
// The kind is supplied by the user, never by a model: a directory called temp
// or downloads is not disposable until someone says it is.
func (r *Registrar) Add(ctx context.Context, opt AddOptions) (store.Scope, error) {
	if !validKind(opt.Kind) {
		return store.Scope{}, errs.Newf(errs.CodeInvalidArgument,
			"scope kind must be one of observe, assisted, managed_disposable, reference; got %q", opt.Kind)
	}
	// A new reference scope is refused: its only use was to hold the surviving
	// copy for content comparison, and that pipeline no longer exists. The refusal
	// happens before a directory, a vault or a row is created, so a rejected
	// request leaves the tree exactly as it was. Existing reference scopes keep
	// loading and stay read-only; they are not reclassified.
	if opt.Kind == domain.KindReference {
		return store.Scope{}, errs.New(errs.CodeInvalidArgument,
			"reference scopes are not created by this build; register the directory with --kind observe to inspect it")
	}

	resolved, err := resolveExistingDir(opt.Path)
	if err != nil {
		return store.Scope{}, err
	}
	if err := r.refusePath(resolved); err != nil {
		return store.Scope{}, err
	}

	home, _ := os.UserHomeDir()

	// One fixed user-level lock makes "two state directories, one tree"
	// impossible during registration.
	regLock, err := RegistrationLock(home)
	if err != nil {
		return store.Scope{}, err
	}
	defer regLock.Release()

	root, err := fsops.OpenRoot(resolved)
	if err != nil {
		return store.Scope{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer root.Close()

	identity, err := root.Identity()
	if err != nil {
		return store.Scope{}, errs.Wrap(errs.CodeAccessDenied, err)
	}

	if domain.MutableKind(opt.Kind) {
		if err := requireMutableFilesystem(identity.Persistent.FileSystem); err != nil {
			return store.Scope{}, err
		}
		// The owner-marker probe walks the whole subtree, so it only runs where
		// mutation is possible.
		if err := checkOwnerMarkers(resolved, r.InstanceID); err != nil {
			return store.Scope{}, err
		}
	}
	// Overlap is refused for every kind, not only for mutable ones. Two scopes
	// over one tree produce two candidate sets for the same files, and a stale
	// candidate from the read-only copy is indistinguishable from a real one in
	// reports. Refusing is cheaper than explaining that later.
	if err := r.checkExistingScopes(ctx, resolved, identity); err != nil {
		return store.Scope{}, err
	}

	id := opt.Name
	if id == "" {
		id = sanitizeID(filepath.Base(resolved))
	}
	if !scopeIDPattern.MatchString(id) {
		return store.Scope{}, errs.Newf(errs.CodeInvalidArgument,
			"scope name %q must match %s", id, scopeIDPattern.String())
	}
	if exists, err := r.Store.ScopeExists(ctx, domain.ScopeID(id)); err != nil {
		return store.Scope{}, err
	} else if exists {
		return store.Scope{}, errs.Newf(errs.CodeInvalidArgument, "scope %q already exists", id)
	}

	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return store.Scope{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	sc := store.Scope{
		ID:                 domain.ScopeID(id),
		RawRoot:            []byte(resolved),
		RootIdentityJSON:   string(identityJSON),
		InstanceID:         r.InstanceID,
		Kind:               opt.Kind,
		Generation:         1,
		ProtectionRevision: 1,
		Status:             domain.ScopeActive,
		ScanEpoch:          0,
		CreatedAtNS:        r.Now(),
	}

	if domain.MutableKind(opt.Kind) {
		vaultID, err := r.createVault(root, sc, identity)
		if err != nil {
			return store.Scope{}, err
		}
		sc.VaultIdentityJSON = &vaultID
	}

	if err := r.Store.InsertScope(ctx, sc); err != nil {
		return store.Scope{}, err
	}
	return sc, nil
}

func validKind(k string) bool {
	switch k {
	case domain.KindObserve, domain.KindAssisted, domain.KindManagedDisposable, domain.KindReference:
		return true
	}
	return false
}

func resolveExistingDir(p string) (string, error) {
	if p == "" {
		return "", errs.New(errs.CodeInvalidArgument, "path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	if !fi.IsDir() {
		return "", errs.Newf(errs.CodeInvalidArgument, "%q is not a directory", abs)
	}
	// Record the resolved target: later stages must not follow a path that can
	// change, so registration pins the real directory and shows it to the user.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	return filepath.Clean(real), nil
}

// refusePath rejects roots whose scope would be meaningless or dangerous.
func (r *Registrar) refusePath(resolved string) error {
	// resolved has already been through EvalSymlinks. Compare it against paths
	// that have been resolved the same way, otherwise /var vs /private/var on
	// macOS makes every containment test silently false.
	home, _ := os.UserHomeDir()
	home, stateDir := canonical(home), canonical(r.Paths.StateDir)

	if resolved == string(filepath.Separator) {
		return errs.New(errs.CodeScopeRefused, "the filesystem root cannot be a scope")
	}
	if home != "" {
		if resolved == home {
			return errs.New(errs.CodeScopeRefused, "the whole home directory cannot be a scope")
		}
		if isAncestor(resolved, home) {
			return errs.Newf(errs.CodeScopeRefused,
				"%q contains the home directory and cannot be a scope", resolved)
		}
	}
	for _, bad := range systemPaths() {
		if resolved == bad {
			return errs.Newf(errs.CodeScopeRefused, "%q is a system directory", resolved)
		}
	}
	// The state directory must never live inside a scope: the program would then
	// scan and potentially move its own database.
	if stateDir != "" && isAncestor(resolved, stateDir) {
		return errs.Newf(errs.CodeScopeRefused,
			"the state directory %q is inside this path; register a narrower directory", r.Paths.StateDir)
	}
	return nil
}

// canonical resolves symlinks when it can and falls back to a cleaned path. The
// fallback matters: the state directory may not exist yet, and refusing to
// compare then would silently disable every containment check.
func canonical(p string) string {
	if p == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(p)
}

func systemPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/System", "/Library", "/usr", "/bin", "/sbin", "/etc", "/private",
			"/Applications", "/Volumes", "/Users", "/cores", "/dev", "/tmp", "/var"}
	}
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/var", "/boot",
		"/dev", "/proc", "/sys", "/root", "/home", "/tmp", "/run"}
}

// requireMutableFilesystem limits mutation to filesystems that were actually
// validated. Everything else stays read-only, with an explicit reason.
func requireMutableFilesystem(fsName string) error {
	switch fsName {
	case "apfs", "ext4", "xfs":
		return nil
	case "":
		return errs.New(errs.CodeUnsupportedFS, "the filesystem type could not be determined")
	default:
		return errs.Newf(errs.CodeUnsupportedFS,
			"filesystem %q is not in the validated mutation set (apfs, ext4, xfs)", fsName)
	}
}

// checkExistingScopes refuses aliases, parent/child overlap and duplicates.
func (r *Registrar) checkExistingScopes(ctx context.Context, resolved string, identity domain.RootIdentity) error {
	scopes, err := r.Store.ListScopes(ctx)
	if err != nil {
		return err
	}
	for _, sc := range scopes {
		other := string(sc.RawRoot)
		if other == resolved {
			return errs.Newf(errs.CodeScopeOverlap, "%q is already registered as %s", resolved, sc.ID)
		}
		if isAncestor(other, resolved) || isAncestor(resolved, other) {
			return errs.Newf(errs.CodeScopeOverlap,
				"%q overlaps the already registered scope %s (%q)", resolved, sc.ID, other)
		}
		var existing domain.RootIdentity
		if err := json.Unmarshal([]byte(sc.RootIdentityJSON), &existing); err == nil {
			if existing.Persistent.Inode == identity.Persistent.Inode &&
				existing.Persistent.VolumeID == identity.Persistent.VolumeID {
				return errs.Newf(errs.CodeScopeOverlap,
					"%q is the same physical directory as scope %s", resolved, sc.ID)
			}
		}
	}
	return nil
}

// checkOwnerMarkers looks for management markers in the ancestors and in the
// whole subtree. A different instance owning any part of it is refused: the
// locks of two instances would not interlock.
func checkOwnerMarkers(resolved, instanceID string) error {
	// Ancestors.
	dir := filepath.Dir(resolved)
	for {
		marker := filepath.Join(dir, domain.VaultDirName, "owner.json")
		if data, err := os.ReadFile(marker); err == nil {
			var m OwnerMarker
			if json.Unmarshal(data, &m) == nil && m.InstanceID != instanceID {
				return errs.Newf(errs.CodeScopeOverlap,
					"an ancestor %q is managed by another Dustkeeper instance", dir)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Subtree probe. It is bounded; an incomplete probe is a refusal, because a
	// missed marker would mean two managers over one tree.
	const maxEntries = 200000
	seen := 0
	var probeErr error
	walkErr := filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			probeErr = fmt.Errorf("cannot inspect %q: %w", p, err)
			return fs.SkipAll
		}
		if !d.IsDir() {
			return nil
		}
		seen++
		if seen > maxEntries {
			probeErr = fmt.Errorf("more than %d directories; register a narrower scope", maxEntries)
			return fs.SkipAll
		}
		if filepath.Base(p) == domain.VaultDirName {
			return fs.SkipDir
		}
		marker := filepath.Join(p, domain.VaultDirName, "owner.json")
		if data, err := os.ReadFile(marker); err == nil {
			var m OwnerMarker
			if json.Unmarshal(data, &m) == nil {
				return fmt.Errorf("nested scope marker at %q belongs to instance %s", p, m.InstanceID)
			}
		}
		return nil
	})
	if walkErr != nil {
		return errs.Newf(errs.CodeScopeOverlap, "%v", walkErr)
	}
	if probeErr != nil {
		return errs.Newf(errs.CodeScopeRefused, "ownership probe incomplete: %v", probeErr)
	}
	return nil
}

// createVault builds the private directory, owner marker and durability anchor.
//
// Order matters and is the R01 fix: the capability probe runs before anything is
// created, every object is created or opened through the verified root
// descriptor, an existing object is verified rather than trusted, and the whole
// directory is re-opened through OpenVault at the end so that what registration
// just wrote is checked by exactly the code that will check it on every later
// run.
func (r *Registrar) createVault(root *fsops.Root, sc store.Scope, identity domain.RootIdentity) (string, error) {
	// The probe runs against the business root, which is the volume the anchor
	// and the objects will live on, and it runs before the private directory
	// exists. A host that cannot finish a barrier must stay read-only instead of
	// moving a file and discovering afterwards that it cannot close the action.
	caps := fsops.Probe(root.Path())
	if !caps.Capable() {
		return "", errs.Newf(errs.CodeUnsupportedFS,
			"this filesystem cannot support safe file changes: missing %s",
			strings.Join(caps.Missing(), "; "))
	}

	if err := root.MkdirAll([]byte(domain.VaultDirName), 0o700); err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	for _, sub := range []string{domain.VaultObjects, domain.VaultManifests, domain.VaultTmp} {
		if err := root.MkdirAll([]byte(sub), 0o700); err != nil {
			return "", errs.Wrap(errs.CodeAccessDenied, err)
		}
	}

	vault, err := root.OpenDir([]byte(domain.VaultDirName))
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer vault.Close()

	tmp, err := root.OpenDir([]byte(domain.VaultTmp))
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer tmp.Close()

	expect := VaultExpect{
		OwnerUID:   CurrentOwnerUID(),
		RootDev:    root.Dev(),
		InstanceID: r.InstanceID,
		ScopeID:    string(sc.ID),
		RootInode:  identity.Persistent.Inode,
		VolumeID:   identity.Persistent.VolumeID,
		ScopeKind:  sc.Kind,
	}
	for _, d := range []struct {
		dir  *fsops.Dir
		name string
	}{{vault, "management directory"}, {tmp, "temporary directory"}} {
		if err := d.dir.VerifyPrivate(expect.For(true)); err != nil {
			return "", refuse(d.name, err)
		}
	}

	marker := OwnerMarker{
		FormatVersion: 1,
		InstanceID:    r.InstanceID,
		ScopeID:       string(sc.ID),
		VolumeID:      identity.Persistent.VolumeID,
		RootInode:     identity.Persistent.Inode,
		Kind:          sc.Kind,
		CreatedAtNS:   sc.CreatedAtNS,
	}
	body, err := json.Marshal(marker)
	if err != nil {
		return "", errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	// InstallPrivate never truncates and never replaces. The review's first
	// scenario is a directory that already holds owner.json.tmp as a symlink to
	// a file outside the scope: a fixed temporary name plus O_CREAT|O_TRUNC would
	// truncate that file before any check could run.
	created, installErr := fsops.InstallPrivate(tmp, vault, "owner.json", body, 0o600)
	switch {
	case installErr == nil && created:
		// Fresh marker written and synced.
	case errors.Is(installErr, fsops.ErrExists):
		// A marker is already present. It is verified, not trusted: a stale or
		// tampered marker is exactly the case R01 refuses to act on.
		if err := verifyMarker(vault, expect); err != nil {
			return "", err
		}
	case installErr != nil && created:
		// The entry is in place but the directory sync failed, so its durability
		// is unknown. Continuing would build intents on top of an unsynced marker.
		return "", errs.Wrap(errs.CodeDurabilityFailed, installErr)
	default:
		return "", errs.Wrap(errs.CodeStateWriteFailed, installErr)
	}

	// The objects directory carries a random value for its whole life. It is the
	// one fact a replacement cannot reproduce: deleting and recreating the
	// directory loses it, and an inode number handed to the new directory by the
	// filesystem says nothing about who created it.
	objects, err := root.OpenDir([]byte(domain.VaultObjects))
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer objects.Close()
	if err := objects.VerifyPrivate(expect.For(true)); err != nil {
		return "", refuse("objects directory", err)
	}
	if created, ierr := fsops.InstallPrivate(tmp, objects, domain.VaultNonceName,
		[]byte(domain.NewID("vault")), 0o600); ierr != nil {
		if !errors.Is(ierr, fsops.ErrExists) {
			if created {
				return "", errs.Wrap(errs.CodeDurabilityFailed, ierr)
			}
			return "", errs.Wrap(errs.CodeStateWriteFailed, ierr)
		}
		// Already there: re-registration of the same tree reuses its evidence
		// instead of minting a second one.
	}
	nonce, err := objects.ReadFileAt(domain.VaultNonceName, maxNonceBytes)
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}

	// The anchor gets the same treatment: exclusive create, no link, private
	// mode. A pre-placed hard link to a business file would turn every later
	// barrier into a write into that file.
	anchorCreated, anchorErr := fsops.InstallPrivate(tmp, vault, "durability.anchor", nil, 0o600)
	if anchorErr != nil && !errors.Is(anchorErr, fsops.ErrExists) {
		if anchorCreated {
			return "", errs.Wrap(errs.CodeDurabilityFailed, anchorErr)
		}
		return "", errs.Wrap(errs.CodeStateWriteFailed, anchorErr)
	}
	anchor, err := root.OpenFileRW([]byte(domain.VaultAnchor))
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	// The contract is checked before anything is written. A pre-placed hard link
	// to a business file satisfies "regular file, no symlink", so a tag written
	// first would land inside that file.
	if err := anchor.VerifyPrivate("durability anchor", expect.For(false)); err != nil {
		anchor.Close()
		return "", refuse("durability anchor", err)
	}
	anchorID, _, err := anchor.Stat()
	if err != nil {
		anchor.Close()
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	// The tag identifies the anchor without occupying the area the barrier
	// writes. It is written once, at creation.
	anchorTag := ""
	if tag, terr := fsops.ReadTag(anchor.FD(), anchorTagOffset, maxNonceBytes); terr == nil && len(tag) > 0 {
		anchorTag = string(tag)
	} else {
		anchorTag = domain.NewID("anchor")
		if werr := fsops.WriteTag(anchor.FD(), anchorTagOffset, []byte(anchorTag)); werr != nil {
			anchor.Close()
			return "", errs.Wrap(errs.CodeDurabilityFailed, werr)
		}
		if serr := fsops.DurableFile(anchor.FD()); serr != nil {
			anchor.Close()
			return "", errs.Wrap(errs.CodeDurabilityFailed, serr)
		}
	}
	anchor.Close()

	vaultID, err := VaultIdentity(objects, string(nonce))
	if err != nil {
		return "", err
	}
	bindBody, err := json.Marshal(VaultBindV1{
		FormatVersion:   1,
		ScopeID:         string(sc.ID),
		RootInode:       identity.Persistent.Inode,
		VolumeID:        identity.Persistent.VolumeID,
		ScopeKind:       sc.Kind,
		ObjectsNonce:    string(nonce),
		ObjectsIdentity: vaultID,
		AnchorTag:       anchorTag,
		AnchorIdentity:  AnchorIdentity(anchorID, anchorTag),
	})
	if err != nil {
		return "", errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if created, ierr := fsops.InstallPrivate(tmp, vault, "vault-bind.json", bindBody, 0o600); ierr != nil {
		if !errors.Is(ierr, fsops.ErrExists) {
			if created {
				return "", errs.Wrap(errs.CodeDurabilityFailed, ierr)
			}
			return "", errs.Wrap(errs.CodeStateWriteFailed, ierr)
		}
	}

	expect.Creating = true
	expect.Identity = vaultID
	expect.Lock = true
	handles, err := OpenVault(root, expect)
	if err != nil {
		return "", err
	}
	defer handles.Close()
	return handles.Identity, nil
}

// VerifiedScope is a scope whose root was just proven to be the same object
// that was registered.
type VerifiedScope struct {
	Scope  store.Scope
	Root   *fsops.Root
	RootID domain.RootIdentity
	// Weak is non-empty when nothing proved a change but the surviving evidence
	// cannot rule one out. Callers keep the scope usable for observing and
	// planning and refuse automatic permanent deletion on it.
	Weak string
}

// OpenVerified opens the scope root and compares identity facts.
//
// The comparison uses every persistent field the platform can supply, including
// the birth time and generation, and it compares the owner exactly instead of
// treating a stored uid 0 as "unset". When the persistent evidence cannot
// distinguish the object, the scope is marked weak instead of being silently
// trusted: thin evidence is reported, not ignored.
//
// The runtime half is re-established on every open, because mount IDs only mean
// something within one boot. A different mount inside the same boot is treated as
// a change; a new boot is not.
func (r *Registrar) OpenVerified(ctx context.Context, id domain.ScopeID) (VerifiedScope, error) {
	sc, err := r.Store.GetScope(ctx, id)
	if err != nil {
		return VerifiedScope{}, err
	}
	root, err := fsops.OpenRoot(string(sc.RawRoot))
	if err != nil {
		return VerifiedScope{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	current, err := root.Identity()
	if err != nil {
		root.Close()
		return VerifiedScope{}, errs.Wrap(errs.CodeAccessDenied, err)
	}

	var stored domain.RootIdentity
	if err := json.Unmarshal([]byte(sc.RootIdentityJSON), &stored); err != nil {
		root.Close()
		return VerifiedScope{}, errs.Newf(errs.CodeStateWriteFailed,
			"scope %s has an unreadable stored identity: %v", id, err)
	}

	verdict := CompareRootIdentity(stored, current)
	if verdict.Changed == "" {
		if storedBinding, berr := r.storedMountBinding(ctx, id); berr != nil {
			root.Close()
			return VerifiedScope{}, berr
		} else {
			verdict = mergeVerdicts(verdict, CompareMountBinding(storedBinding, current.Runtime))
		}
	}
	if verdict.Changed != "" {
		root.Close()
		// Marking the scope held is what tells every other entry point that this
		// scope is not usable. If that write fails, this caller is still refused,
		// but the operator has to know the durable answer is missing rather than
		// assume the scope was frozen for them.
		if serr := r.Store.SetScopeStatus(ctx, id, domain.ScopeHeld); serr != nil {
			return VerifiedScope{}, errs.Newf(errs.CodeScopeChanged,
				"scope %s no longer matches its registration: %s; re-register after checking the path (and the scope could not be marked held: %v)",
				id, verdict.Changed, serr)
		}
		return VerifiedScope{}, errs.Newf(errs.CodeScopeChanged,
			"scope %s no longer matches its registration: %s; re-register after checking the path",
			id, verdict.Changed)
	}

	// Record the binding this run will compare against, so a remount later in the
	// same boot is still detected. A failure here is reported: acting on a scope
	// whose binding could not be written would leave the next run without the
	// evidence this one claims to have established.
	if err := r.recordMountBinding(ctx, id, current.Runtime); err != nil {
		root.Close()
		return VerifiedScope{}, err
	}
	return VerifiedScope{Scope: sc, Root: root, RootID: current, Weak: verdict.Weak}, nil
}

func mergeVerdicts(a, b IdentityVerdict) IdentityVerdict {
	if a.Changed == "" {
		a.Changed = b.Changed
	}
	if a.Weak == "" {
		a.Weak = b.Weak
	}
	return a
}

// mountBindingKey is the meta key holding one scope's recorded runtime binding.
func mountBindingKey(id domain.ScopeID) string { return "scope_mount_binding:" + string(id) }

func (r *Registrar) storedMountBinding(ctx context.Context, id domain.ScopeID) (domain.RuntimeMountIdentity, error) {
	raw, err := r.Store.MetaGet(ctx, mountBindingKey(id))
	if err != nil {
		return domain.RuntimeMountIdentity{}, err
	}
	if raw == "" {
		return domain.RuntimeMountIdentity{}, nil
	}
	var b domain.RuntimeMountIdentity
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		// An unreadable binding is missing evidence, not a match. Reporting it
		// keeps the caller from believing a comparison that never happened.
		return domain.RuntimeMountIdentity{}, errs.Newf(errs.CodeStateWriteFailed,
			"the recorded mount binding for scope %s is unreadable: %v", id, err)
	}
	return b, nil
}

func (r *Registrar) recordMountBinding(ctx context.Context, id domain.ScopeID, b domain.RuntimeMountIdentity) error {
	body, err := json.Marshal(b)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := r.Store.MetaSet(ctx, mountBindingKey(id), string(body)); err != nil {
		return err
	}
	return nil
}

// isAncestor reports whether parent contains child (strictly).
func isAncestor(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sanitizeID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := b.String()
	if out == "" {
		out = "scope"
	}
	if out[0] < 'a' || out[0] > 'z' {
		out = "s" + out
	}
	return out
}
