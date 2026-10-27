// Package execute performs every business file change.
//
// This package and recovery are the only callers of fsops rename/unlink. Every
// action follows the same shape: verify, persist the intent, change the
// namespace, verify the result, persist the outcome. Nothing is deleted on the
// strength of an index, a hash or a model statement.
package execute

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Vault locations inside a mutable scope. The names come from internal/domain
// so that registration and execution cannot disagree about them.
const (
	objectsDir      = domain.VaultObjects
	manifestsDir    = domain.VaultManifests
	tmpDir          = domain.VaultTmp
	anchorFile      = domain.VaultAnchor
	ownerMarkerFile = domain.VaultMarker
)

// ScopeCtx is one verified scope with its private directories open.
type ScopeCtx struct {
	Scope     store.Scope
	Root      *fsops.Root
	Objects   *fsops.Dir
	Manifests *fsops.Dir
	Tmp       *fsops.Dir
	vault     *scope.VaultHandles
	RootDev   uint64
	// IdentityWeak is non-empty when the root could not be told apart from a
	// different object that reuses the same inode and device. The scope stays
	// usable for observing and planning; automatic permanent deletion is refused.
	IdentityWeak string
}

// OpenScopeCtx verifies the root and opens the private directories.
//
// R01 is about what happens here. Previously the vault was opened "by name" and
// nothing checked that the directory being used was the one that had been
// registered: a restored backup, a swapped objects inode or a widened permission
// all passed. Now the whole directory is validated once, by the same code that
// registration runs, and the vault identity is compared against the scope row.
func OpenScopeCtx(ctx context.Context, reg *scope.Registrar, id domain.ScopeID) (*ScopeCtx, error) {
	vs, err := reg.OpenVerified(ctx, id)
	if err != nil {
		return nil, err
	}
	c := &ScopeCtx{Scope: vs.Scope, Root: vs.Root, RootDev: vs.RootID.Runtime.Device, IdentityWeak: vs.Weak}

	if !domain.MutableKind(c.Scope.Kind) {
		// observe and reference scopes have no vault and never will; opening one
		// is not required for scanning or for keeper verification.
		return c, nil
	}

	want := ""
	if c.Scope.VaultIdentityJSON != nil {
		want = *c.Scope.VaultIdentityJSON
	}
	if want == "" {
		c.Root.Close()
		return nil, errs.Newf(errs.CodeScopeChanged,
			"scope %s has no recorded object directory identity; re-register it before any file change", id)
	}

	handles, err := scope.OpenVault(c.Root, scope.VaultExpect{
		OwnerUID:   scope.CurrentOwnerUID(),
		RootDev:    c.RootDev,
		InstanceID: reg.InstanceID,
		Identity:   want,
		Lock:       true,
		ScopeID:    string(id),
		RootInode:  vs.RootID.Persistent.Inode,
		VolumeID:   vs.RootID.Persistent.VolumeID,
		ScopeKind:  c.Scope.Kind,
	})
	if err != nil {
		c.Root.Close()
		return nil, err
	}
	c.vault = handles
	c.Objects = handles.Objects
	c.Manifests = handles.Manifests
	c.Tmp = handles.Tmp
	return c, nil
}

// AnchorFD returns the durability anchor descriptor, or -1 when the scope has
// no vault.
func (c *ScopeCtx) AnchorFD() int {
	if c == nil || c.vault == nil || c.vault.Anchor == nil {
		return -1
	}
	return c.vault.Anchor.FD()
}

// VaultIdentity returns the digest of the object directory in use, for reports.
func (c *ScopeCtx) VaultIdentity() string {
	if c == nil || c.vault == nil {
		return ""
	}
	return c.vault.Identity
}

// Close releases every handle.
func (c *ScopeCtx) Close() {
	if c == nil {
		return
	}
	if c.vault != nil {
		c.vault.Close()
		c.vault = nil
		c.Objects, c.Manifests, c.Tmp = nil, nil, nil
	}
	if c.Root != nil {
		c.Root.Close()
	}
}

// ManifestV1 is the private, independent recovery fact for one object.
//
// It is written and made durable before the file moves, so that a crash between
// rename and database commit still leaves enough information to explain where
// the bytes went. It grants nothing: a manifest alone can never authorise a
// purge.
type ManifestV1 struct {
	FormatVersion    int                 `json:"format_version"`
	InstanceID       string              `json:"instance_id"`
	ScopeID          string              `json:"scope_id"`
	ObjectID         string              `json:"object_id"`
	RawRelativeB64   string              `json:"raw_relative_b64"`
	ParentIdentity   domain.FileIdentity `json:"parent_identity"`
	ExpectedIdentity domain.FileIdentity `json:"expected_identity"`
	ContentHash      string              `json:"content_hash"`
	MetadataHash     string              `json:"metadata_hash"`
	RuleDigest       *string             `json:"rule_digest"`
	PlanDigest       string              `json:"plan_digest"`
	CreatedAtNS      int64               `json:"created_at_ns"`
}

// WriteManifest persists a manifest: write to a private temp file, fsync it,
// install it without replacing anything, then fsync the directory.
func (c *ScopeCtx) WriteManifest(m ManifestV1) error {
	body, err := json.Marshal(m)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	name := m.ObjectID + ".json"

	tmpName := domain.NewID("manifest") + ".tmp"
	f, err := fsops.CreateExclusive(c.Tmp, tmpName, 0o600)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := fsops.DurableFile(f.FD()); err != nil {
		f.Close()
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	if err := f.Close(); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}

	if err := fsops.RenameNoReplace(c.Tmp, tmpName, c.Manifests, name); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := fsops.DurableDir(c.Manifests); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	return nil
}

// manifestPath is used by reporting; it never drives an action.
func manifestPath(root string, objectID string) string {
	return filepath.Join(root, domain.VaultManifests, objectID+".json")
}

// RemoveManifest deletes one object's manifest and flushes the directory.
//
// It exists for exactly one situation: the executor wrote its own preparation
// record and then refused the action, so the file never moved. Undoing that
// preparation is what makes the refusal clean - the next attempt must not find a
// manifest that describes a move which never happened. It never touches payload:
// the argument is the manifest name derived from the item ID, not a path.
func (c *ScopeCtx) RemoveManifest(objectID string) error {
	if c == nil || c.Manifests == nil {
		return errs.New(errs.CodeStateWriteFailed,
			"no manifest directory is open, so the preparation record cannot be removed")
	}
	name := objectID + ".json"
	if err := fsops.UnlinkAt(c.Manifests, name); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := fsops.DurableDir(c.Manifests); err != nil {
		return errs.Wrap(errs.CodeDurabilityFailed, err)
	}
	return nil
}

// PlanBindingOf decodes the stored item binding.
func PlanBindingOf(it store.Item) (plan.ItemBindingV1, error) {
	var b plan.ItemBindingV1
	if err := json.Unmarshal([]byte(it.BindingJSON), &b); err != nil {
		return b, errs.Newf(errs.CodeStateWriteFailed, "item %s binding is unreadable: %v", it.ID, err)
	}
	return b, nil
}

// NowNS reads the clock.
func NowNS(c domain.Clock) int64 { return c.Wall().UnixNano() }

// sleepUntilRate paces content reads so a run cannot saturate the disk.
func sleepUntilRate(bytesRead int64, started time.Time, ratePerSec int64) {
	if ratePerSec <= 0 {
		return
	}
	expected := time.Duration(float64(bytesRead) / float64(ratePerSec) * float64(time.Second))
	spent := time.Since(started)
	if expected > spent {
		time.Sleep(expected - spent)
	}
}
