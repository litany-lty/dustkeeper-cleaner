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
	"path/filepath"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Vault locations inside a mutable scope.
const (
	vaultPrefix     = ".dustkeeper"
	objectsDir      = vaultPrefix + "/objects"
	manifestsDir    = vaultPrefix + "/manifests"
	tmpDir          = vaultPrefix + "/tmp"
	anchorFile      = vaultPrefix + "/durability.anchor"
	ownerMarkerFile = vaultPrefix + "/owner.json"
)

// ScopeCtx is one verified scope with its private directories open.
type ScopeCtx struct {
	Scope     store.Scope
	Root      *fsops.Root
	Objects   *fsops.Dir
	Manifests *fsops.Dir
	Tmp       *fsops.Dir
	anchor    *fsops.File
	RootDev   uint64
}

// OpenScopeCtx verifies the root and opens the private directories.
func OpenScopeCtx(ctx context.Context, reg *scope.Registrar, id domain.ScopeID) (*ScopeCtx, error) {
	vs, err := reg.OpenVerified(ctx, id)
	if err != nil {
		return nil, err
	}
	c := &ScopeCtx{Scope: vs.Scope, Root: vs.Root, RootDev: vs.RootID.Runtime.Device}

	closeAll := func() {
		if c.Objects != nil {
			c.Objects.Close()
		}
		if c.Manifests != nil {
			c.Manifests.Close()
		}
		if c.Tmp != nil {
			c.Tmp.Close()
		}
		if c.anchor != nil {
			c.anchor.Close()
		}
		c.Root.Close()
	}

	if !domain.MutableKind(c.Scope.Kind) {
		// observe and reference scopes have no vault and never will; opening one
		// is not required for scanning or for keeper verification.
		return c, nil
	}

	if c.Objects, err = c.Root.OpenDir([]byte(objectsDir)); err != nil {
		closeAll()
		return nil, errs.Newf(errs.CodeScopeChanged, "vault is missing: %v", err)
	}
	if c.Manifests, err = c.Root.OpenDir([]byte(manifestsDir)); err != nil {
		closeAll()
		return nil, errs.Newf(errs.CodeScopeChanged, "manifest directory is missing: %v", err)
	}
	if c.Tmp, err = c.Root.OpenDir([]byte(tmpDir)); err != nil {
		closeAll()
		return nil, errs.Newf(errs.CodeScopeChanged, "temporary directory is missing: %v", err)
	}
	anchor, err := c.Root.OpenFileRW([]byte(anchorFile))
	if err != nil {
		closeAll()
		return nil, errs.Newf(errs.CodeScopeChanged, "durability anchor is missing: %v", err)
	}
	// The scope keeps the fsops handle itself. Wrapping the descriptor in a
	// second *os.File would mean two owners of one descriptor: when the
	// original became unreachable its finalizer closed the number, and the
	// next barrier failed with EBADF.
	c.anchor = anchor
	return c, nil
}

// AnchorFD returns the durability anchor descriptor, or -1 when the scope has
// no vault.
func (c *ScopeCtx) AnchorFD() int {
	if c == nil || c.anchor == nil {
		return -1
	}
	return c.anchor.FD()
}

// Close releases every handle.
func (c *ScopeCtx) Close() {
	if c == nil {
		return
	}
	if c.Objects != nil {
		c.Objects.Close()
	}
	if c.Manifests != nil {
		c.Manifests.Close()
	}
	if c.Tmp != nil {
		c.Tmp.Close()
	}
	if c.anchor != nil {
		c.anchor.Close()
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
	return filepath.Join(root, vaultPrefix, "manifests", objectID+".json")
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
