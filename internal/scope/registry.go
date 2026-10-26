package scope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// vaultDirName is the private management directory created inside a mutable
// scope. observe and reference scopes never get one.
const vaultDirName = ".dustkeeper"

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
		vaultID, err := r.createVault(root, resolved, sc, identity)
		if err != nil {
			return store.Scope{}, err
		}
		sc.VaultIdentityJSON = &vaultID

		// The capability probe runs before the scope is usable. A host that
		// cannot refuse an overwriting rename must stay read-only.
		caps := fsops.Probe(filepath.Join(resolved, vaultDirName, "tmp"))
		if !caps.Capable() {
			return store.Scope{}, errs.Newf(errs.CodeUnsupportedFS,
				"this filesystem cannot support safe file changes: %s", strings.Join(caps.Notes, "; "))
		}
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
		marker := filepath.Join(dir, vaultDirName, "owner.json")
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
		if filepath.Base(p) == vaultDirName {
			return fs.SkipDir
		}
		marker := filepath.Join(p, vaultDirName, "owner.json")
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

// createVault builds the private directory, owner marker and durability anchor,
// then returns the vault identity digest.
func (r *Registrar) createVault(root *fsops.Root, resolved string, sc store.Scope, identity domain.RootIdentity) (string, error) {
	if err := root.MkdirAll([]byte(vaultDirName), 0o700); err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	for _, sub := range []string{"objects", "manifests", "tmp"} {
		if err := root.MkdirAll([]byte(vaultDirName+"/"+sub), 0o700); err != nil {
			return "", errs.Wrap(errs.CodeAccessDenied, err)
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
	markerPath := filepath.Join(resolved, vaultDirName, "owner.json")
	if existing, err := os.ReadFile(markerPath); err == nil {
		var prior OwnerMarker
		if json.Unmarshal(existing, &prior) == nil && prior.InstanceID != r.InstanceID {
			return "", errs.Newf(errs.CodeScopeOverlap,
				"%q is already managed by another Dustkeeper instance", resolved)
		}
		// Same instance: keep the original marker.
	} else if err := writeFileDurable(markerPath, body, 0o600); err != nil {
		return "", err
	}

	anchorPath := filepath.Join(resolved, vaultDirName, "durability.anchor")
	if _, err := os.Stat(anchorPath); os.IsNotExist(err) {
		if err := writeFileDurable(anchorPath, nil, 0o600); err != nil {
			return "", err
		}
	}

	vault, err := root.OpenDir([]byte(vaultDirName + "/objects"))
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer vault.Close()
	vaultIdentity, _, err := vault.Stat()
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", vaultIdentity.VolumeID, vaultIdentity.Device, vaultIdentity.Inode)))
	return "vault:" + hex.EncodeToString(digest[:]), nil
}

func writeFileDurable(path string, body []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	if len(body) > 0 {
		if _, err := f.Write(body); err != nil {
			f.Close()
			os.Remove(tmp)
			return errs.Wrap(errs.CodeStateWriteFailed, err)
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return syncDir(filepath.Dir(path))
}

// VerifiedScope is a scope whose root was just proven to be the same object
// that was registered.
type VerifiedScope struct {
	Scope  store.Scope
	Root   *fsops.Root
	RootID domain.RootIdentity
}

// OpenVerified opens the scope root and compares identity facts.
//
// Normal restarts compare the persistent half only; a mount ID that happens to
// match proves nothing across a reboot. When the persistent identity cannot be
// matched, the scope is paused rather than silently refreshed.
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

	if stored.Persistent.Inode != current.Persistent.Inode ||
		stored.Persistent.VolumeID != current.Persistent.VolumeID ||
		(stored.Persistent.OwnerUID != 0 && stored.Persistent.OwnerUID != current.Persistent.OwnerUID) {
		root.Close()
		_ = r.Store.SetScopeStatus(ctx, id, domain.ScopeHeld)
		return VerifiedScope{}, errs.Newf(errs.CodeScopeChanged,
			"scope %s root identity changed (inode %d -> %d); re-register after checking the path",
			id, stored.Persistent.Inode, current.Persistent.Inode)
	}
	return VerifiedScope{Scope: sc, Root: root, RootID: current}, nil
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
