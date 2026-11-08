package scope

// A scope whose runtime volume label was renumbered across a boot is still the
// registered tree, but its stored identity, vault digests and isolated-object
// records all name the old label. `scope resume --accept-stale-hold` only
// changes a status column: it cannot rebuild any of those bindings, and opening
// the vault still refuses. This file is the explicit, preview-then-confirm
// recovery that keeps every historical record and every isolated object.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/store"
)

const rebindPreviewPurpose = "dustkeeper/scope-rebind-preview/v1"

// RebindObjectEvidence is the per-object proof that a record still points at
// the original isolated object. Nothing here grants an action.
type RebindObjectEvidence struct {
	ItemID            string `json:"item_id"`
	ObjectID          string `json:"object_id"`
	State             string `json:"state"`
	ManifestPresent   bool   `json:"manifest_present"`
	ObjectPresent     bool   `json:"object_present"`
	IdentityMatches   bool   `json:"identity_matches"`
	RecordedLabel     string `json:"recorded_volume_label"`
	ManifestLabel     string `json:"manifest_volume_label"`
	ContentHash       string `json:"content_hash"`
	RestoreChainValid bool   `json:"restore_chain_valid"`
	UnresolvedReason  string `json:"unresolved_reason,omitempty"`
}

// RebindPlanRef is one historical plan bound to the old generation.
type RebindPlanRef struct {
	PlanID string `json:"plan_id"`
	Status string `json:"status"`
}

// RebindGrantRef is one authorization bound to the old generation.
type RebindGrantRef struct {
	GrantID    string `json:"grant_id"`
	Capability string `json:"capability"`
	Status     string `json:"status"`
}

// RebindPreview is the complete, read-only evidence set behind one rebind.
// The digest covers every field except Digest itself.
type RebindPreview struct {
	FormatVersion int    `json:"format_version"`
	ScopeID       string `json:"scope_id"`
	InstanceID    string `json:"instance_id"`
	Root          string `json:"root"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`

	Generation            int64 `json:"generation"`
	NewGeneration         int64 `json:"new_generation"`
	ProtectionRevision    int64 `json:"protection_revision"`
	NewProtectionRevision int64 `json:"new_protection_revision"`

	OldRootIdentity domain.RootIdentity `json:"old_root_identity"`
	NewRootIdentity domain.RootIdentity `json:"new_root_identity"`
	Verdict         IdentityVerdict     `json:"verdict"`
	BootChanged     bool                `json:"boot_changed"`

	OldMountBinding domain.RuntimeMountIdentity `json:"old_mount_binding"`
	NewMountBinding domain.RuntimeMountIdentity `json:"new_mount_binding"`

	OwnerVolumeLabel   string `json:"owner_volume_label"`
	BindVolumeLabel    string `json:"bind_volume_label"`
	VaultObjectsNonce  string `json:"vault_objects_nonce"`
	OldVaultIdentity   string `json:"old_vault_identity"`
	NewVaultIdentity   string `json:"new_vault_identity"`
	OldObjectsIdentity string `json:"old_objects_identity"`
	NewObjectsIdentity string `json:"new_objects_identity"`
	OldAnchorIdentity  string `json:"old_anchor_identity"`
	NewAnchorIdentity  string `json:"new_anchor_identity"`

	QuarantinedCount int                    `json:"quarantined_count"`
	Objects          []RebindObjectEvidence `json:"objects"`
	Plans            []RebindPlanRef        `json:"plans"`
	Grants           []RebindGrantRef       `json:"grants"`
	Protections      int                    `json:"protections"`
	PendingOps       int64                  `json:"pending_ops"`
	Observations     int64                  `json:"observations"`
	GlobalPaused     bool                   `json:"global_paused"`
	Unresolvable     []string               `json:"unresolvable"`
	Notes            []string               `json:"notes"`
	Digest           string                 `json:"digest"`
}

// rebindFacts is the internal result of reading a vault without opening it for
// mutation.
type rebindFacts struct {
	root      *fsops.Root
	vault     *fsops.Dir
	objects   *fsops.Dir
	manifests *fsops.Dir
	anchor    *fsops.File
	expect    VaultExpect
	marker    OwnerMarker
	bind      VaultBindV1
	nonce     string
	tag       string
	anchorID  domain.FileIdentity
}

func (f *rebindFacts) close() {
	if f.anchor != nil {
		f.anchor.Close()
	}
	for _, d := range []*fsops.Dir{f.manifests, f.objects, f.vault} {
		if d != nil {
			d.Close()
		}
	}
	if f.root != nil {
		f.root.Close()
	}
}

// RebindPreviewFor reads and proves every fact a rebind would change. It writes
// nothing: no status change, no mount binding, no event, no vault file.
func (r *Registrar) RebindPreviewFor(ctx context.Context, id domain.ScopeID) (RebindPreview, error) {
	p, _, err := r.rebindPreview(ctx, id)
	return p, err
}

func (r *Registrar) rebindPreview(ctx context.Context, id domain.ScopeID) (RebindPreview, *rebindFacts, error) {
	var empty RebindPreview
	sc, err := r.Store.GetScope(ctx, id)
	if err != nil {
		return empty, nil, err
	}
	var old domain.RootIdentity
	if err := json.Unmarshal([]byte(sc.RootIdentityJSON), &old); err != nil {
		return empty, nil, errs.Newf(errs.CodeStateWriteFailed,
			"scope %s has an unreadable stored identity: %v", id, err)
	}

	p := RebindPreview{
		FormatVersion:         1,
		ScopeID:               string(sc.ID),
		InstanceID:            r.InstanceID,
		Root:                  string(sc.RawRoot),
		Kind:                  sc.Kind,
		Status:                sc.Status,
		Generation:            sc.Generation,
		NewGeneration:         sc.Generation + 1,
		ProtectionRevision:    sc.ProtectionRevision,
		NewProtectionRevision: sc.ProtectionRevision + 1,
		OldRootIdentity:       old,
	}
	if sc.VaultIdentityJSON != nil {
		p.OldVaultIdentity = *sc.VaultIdentityJSON
	}
	p.OldMountBinding, err = r.storedMountBinding(ctx, id)
	if err != nil {
		return empty, nil, err
	}
	p.GlobalPaused, err = (PauseGate{StateDir: r.Paths.StateDir}).Paused()
	if err != nil {
		return empty, nil, err
	}
	p.PendingOps, err = r.Store.ScopePendingOps(ctx, id)
	if err != nil {
		return empty, nil, err
	}
	p.Observations, err = r.Store.CountObservations(ctx, id)
	if err != nil {
		return empty, nil, err
	}
	protections, err := r.Store.ListProtections(ctx, id, true)
	if err != nil {
		return empty, nil, err
	}
	p.Protections = len(protections)
	plans, err := r.Store.ListPlans(ctx, "", 500)
	if err != nil {
		return empty, nil, err
	}
	for _, pl := range plans {
		p.Plans = append(p.Plans, RebindPlanRef{PlanID: string(pl.ID), Status: pl.Status})
	}
	grants, err := r.Store.ListGrants(ctx, id)
	if err != nil {
		return empty, nil, err
	}
	for _, g := range grants {
		p.Grants = append(p.Grants, RebindGrantRef{
			GrantID: string(g.ID), Capability: g.Capability, Status: g.Status,
		})
	}

	// The path must still be the registered one, not a symlink swapped in
	// afterwards. Only the original path may be rebound.
	resolved, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(p.Root) {
		return empty, nil, errs.Newf(errs.CodeScopeChanged,
			"scope %s now resolves through %q, not its registered path %q; rebind refuses a new path",
			id, resolved, p.Root)
	}

	root, err := fsops.OpenRoot(p.Root)
	if err != nil {
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	f := &rebindFacts{root: root}
	current, err := root.Identity()
	if err != nil {
		f.close()
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	p.NewRootIdentity = current
	p.NewMountBinding = current.Runtime
	p.BootChanged = RootBootChanged(old, current)
	verdict := CompareRootIdentity(old, current)
	p.Verdict = verdict
	switch {
	case verdict.Changed != "":
		f.close()
		return empty, nil, errs.Newf(errs.CodeScopeChanged,
			"scope %s is not the registered object (%s); only an explicitly proven runtime relabel can be rebound",
			id, verdict.Changed)
	case verdict.Weak != "":
		f.close()
		return empty, nil, errs.Newf(errs.CodeScopeChanged,
			"scope %s has no creation-time or generation evidence (%s); rebind refuses", id, verdict.Weak)
	case verdict.RuntimeRelabeled == "":
		f.close()
		return empty, nil, errs.New(errs.CodePlanStale,
			"scope identity has no runtime relabel to recover; nothing to rebind")
	case !p.BootChanged:
		f.close()
		return empty, nil, errs.Newf(errs.CodeScopeChanged,
			"the volume label changed inside one boot (%s); that is a real mount replacement, not a relabel",
			verdict.RuntimeRelabeled)
	case !LegacyVolumeLabel(old.Persistent.VolumeID) || !LegacyVolumeLabel(current.Persistent.VolumeID):
		f.close()
		return empty, nil, errs.New(errs.CodeScopeChanged,
			"only legacy runtime `dev-*` labels can be rebound; a stable volume id change needs a new registration")
	}

	f.expect = VaultExpect{
		OwnerUID:   CurrentOwnerUID(),
		RootDev:    current.Runtime.Device,
		InstanceID: r.InstanceID,
		ScopeID:    string(id),
		RootInode:  current.Persistent.Inode,
		VolumeID:   current.Persistent.VolumeID,
		ScopeKind:  sc.Kind,
	}
	for _, pair := range []struct {
		name string
		dst  **fsops.Dir
	}{
		{domain.VaultDirName, &f.vault},
		{domain.VaultObjects, &f.objects},
		{domain.VaultManifests, &f.manifests},
	} {
		d, err := root.OpenDir([]byte(pair.name))
		if err != nil {
			f.close()
			return empty, nil, errs.Newf(errs.CodeScopeChanged, "%s is missing: %v", pair.name, err)
		}
		*pair.dst = d
		if err := d.VerifyPrivate(f.expect.For(true)); err != nil {
			f.close()
			return empty, nil, refuse(pair.name, err)
		}
	}
	// The volume label in these two files is the OLD one, so the comparison is
	// made against the old identity, not the current one.
	oldExpect := f.expect
	oldExpect.VolumeID = old.Persistent.VolumeID
	f.marker, err = readMarker(f.vault, oldExpect)
	if err != nil {
		f.close()
		return empty, nil, err
	}
	f.bind, err = readBind(f.vault, oldExpect)
	if err != nil {
		f.close()
		return empty, nil, err
	}
	p.OwnerVolumeLabel = f.marker.VolumeID
	p.BindVolumeLabel = f.bind.VolumeID

	nonce, err := f.objects.ReadFileAt(domain.VaultNonceName, maxNonceBytes)
	if err != nil {
		f.close()
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	p.VaultObjectsNonce = string(nonce)
	f.nonce = string(nonce)
	if p.VaultObjectsNonce != f.bind.ObjectsNonce {
		f.close()
		return empty, nil, errs.New(errs.CodeScopeChanged,
			"the objects directory was not created by this vault")
	}
	f.anchor, err = root.OpenFileRW([]byte(domain.VaultAnchor))
	if err != nil {
		f.close()
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if err := f.anchor.VerifyPrivate("durability anchor", f.expect.For(false)); err != nil {
		f.close()
		return empty, nil, refuse("durability anchor", err)
	}
	f.anchorID, _, err = f.anchor.Stat()
	if err != nil {
		f.close()
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	tag, err := fsops.ReadTag(f.anchor.FD(), anchorTagOffset, maxNonceBytes)
	if err != nil {
		f.close()
		return empty, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	f.tag = string(tag)
	if f.tag != f.bind.AnchorTag {
		f.close()
		return empty, nil, errs.New(errs.CodeScopeChanged,
			"the durability anchor is not the one this vault was registered with")
	}

	oldDevice, ok := LegacyDevice(old.Persistent.VolumeID)
	if !ok {
		f.close()
		return empty, nil, errs.New(errs.CodeScopeChanged, "the stored volume label is not a legacy device label")
	}
	p.OldObjectsIdentity, err = VaultIdentityUnder(f.objects, old.Persistent.VolumeID, oldDevice)
	if err != nil {
		f.close()
		return empty, nil, err
	}
	p.OldAnchorIdentity = AnchorIdentityUnder(f.anchorID, f.tag, old.Persistent.VolumeID, oldDevice)
	p.NewObjectsIdentity, err = VaultIdentity(f.objects, f.nonce)
	if err != nil {
		f.close()
		return empty, nil, err
	}
	p.NewAnchorIdentity = AnchorIdentity(f.anchorID, f.tag)
	p.NewVaultIdentity = p.NewObjectsIdentity
	if p.OldObjectsIdentity != f.bind.ObjectsIdentity || p.OldAnchorIdentity != f.bind.AnchorIdentity ||
		p.OldVaultIdentity != p.OldObjectsIdentity {
		f.close()
		return empty, nil, errs.New(errs.CodeScopeChanged,
			"the recorded vault identities are not reproducible from the current objects directory and anchor under the old label; refusing to rebind a different vault")
	}

	// Per-object proof. The vault object must still exist, still be the object
	// the item recorded (under the old label), and still have its manifest.
	items, err := r.isolatedItems(ctx, id)
	if err != nil {
		f.close()
		return empty, nil, err
	}
	p.QuarantinedCount = len(items)
	for _, it := range items {
		p.Objects = append(p.Objects, objectEvidence(f, it, old, current))
	}
	for _, o := range p.Objects {
		if !o.RestoreChainValid {
			p.Unresolvable = append(p.Unresolvable,
				fmt.Sprintf("%s: %s", o.ItemID, o.UnresolvedReason))
		}
	}

	p.Notes = []string{
		"the scope ID, all events, plans, protections, manifests and isolated payloads are preserved byte-for-byte",
		"only the runtime volume label/device inside structured identity JSON is remapped; no bulk text replacement",
		"generation and protection revision increase, so old plans and grants cannot execute against the new identity",
		"active grants are narrowed to needs_reapproval; re-preview and re-confirm the applicable rules",
		"historical observations keep the old label and are not used as authority; the next scan refreshes them",
		"the scope lands on paused, never active; the global pause marker is not touched",
	}
	p.Digest, err = rebindDigest(p)
	if err != nil {
		f.close()
		return empty, nil, err
	}
	return p, f, nil
}

func (r *Registrar) isolatedItems(ctx context.Context, id domain.ScopeID) ([]store.Item, error) {
	var all []store.Item
	for _, state := range []string{domain.StateQuarantined, domain.StateHeld} {
		items, err := r.Store.ListItemsByState(ctx, id, state, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	return all, nil
}

type rebindManifest struct {
	ObjectID         string              `json:"object_id"`
	ScopeID          string              `json:"scope_id"`
	InstanceID       string              `json:"instance_id"`
	ContentHash      string              `json:"content_hash"`
	ParentIdentity   domain.FileIdentity `json:"parent_identity"`
	ExpectedIdentity domain.FileIdentity `json:"expected_identity"`
}

func objectEvidence(f *rebindFacts, it store.Item, old, current domain.RootIdentity) RebindObjectEvidence {
	e := RebindObjectEvidence{
		ItemID:        string(it.ID),
		ObjectID:      it.ObjectName,
		State:         it.State,
		RecordedLabel: old.Persistent.VolumeID,
	}
	if it.CapturedIdentityJSON == nil {
		e.UnresolvedReason = "no recorded post-move identity"
		return e
	}
	var captured domain.FileIdentity
	if err := json.Unmarshal([]byte(*it.CapturedIdentityJSON), &captured); err != nil {
		e.UnresolvedReason = "the recorded captured identity is unreadable"
		return e
	}
	e.RecordedLabel = captured.VolumeID
	objID, _, err := f.objects.StatEntry(it.ObjectName)
	if err != nil {
		e.UnresolvedReason = fmt.Sprintf("the isolated object is missing: %v", err)
		return e
	}
	e.ObjectPresent = true
	e.IdentityMatches = objID.Equal(captured) ||
		(captured.VolumeID == old.Persistent.VolumeID && objID.VolumeID == current.Persistent.VolumeID &&
			objID.Inode == captured.Inode && objID.Kind == captured.Kind &&
			equalPtr(objID.BirthTimeNS, captured.BirthTimeNS) && equalPtr(objID.Generation, captured.Generation))
	body, err := f.manifests.ReadFileAt(it.ObjectName+".json", maxMarkerBytes)
	if err != nil {
		e.UnresolvedReason = fmt.Sprintf("the manifest is missing: %v", err)
		return e
	}
	e.ManifestPresent = true
	var m rebindManifest
	if err := json.Unmarshal(body, &m); err != nil {
		e.UnresolvedReason = "the manifest is unreadable"
		return e
	}
	e.ContentHash = m.ContentHash
	e.ManifestLabel = m.ParentIdentity.VolumeID
	if m.ObjectID != it.ObjectName || m.ScopeID != string(it.ScopeID) || m.InstanceID != f.expect.InstanceID ||
		m.ParentIdentity.VolumeID != old.Persistent.VolumeID ||
		m.ExpectedIdentity.VolumeID != old.Persistent.VolumeID {
		e.UnresolvedReason = "the manifest does not describe this object under the recorded label"
		return e
	}
	if !e.IdentityMatches {
		e.UnresolvedReason = "the isolated object is not the object the item recorded"
		return e
	}
	// Structured verification of every identity field we are about to remap.
	for _, raw := range []string{it.SourceParentIdentityJSON, it.PreIdentityJSON, *it.CapturedIdentityJSON} {
		if _, err := RemapRuntimeLabel(raw, old.Persistent.VolumeID, current.Persistent.VolumeID); err != nil {
			e.UnresolvedReason = "an identity field cannot be structurally remapped: " + err.Error()
			return e
		}
	}
	e.RestoreChainValid = true
	return e
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// RemapRuntimeLabel rewrites ONLY the legacy runtime volume label and device of
// one structured identity JSON object. Any other shape, or a label that does
// not match the one proven in the preview, is refused instead of being guessed.
// This is deliberately not a text replacement over the database.
func RemapRuntimeLabel(raw, oldLabel, newLabel string) (string, error) {
	if raw == "" {
		return "", errs.New(errs.CodeStateWriteFailed, "an identity field is empty")
	}
	var id domain.FileIdentity
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return "", errs.Newf(errs.CodeStateWriteFailed, "identity is unreadable: %v", err)
	}
	oldDev, oldOK := LegacyDevice(oldLabel)
	newDev, newOK := LegacyDevice(newLabel)
	if !oldOK || !newOK {
		return "", errs.New(errs.CodeScopeChanged, "a rebind needs two legacy runtime volume labels")
	}
	if id.VolumeID != oldLabel || id.Device != oldDev {
		return "", errs.Newf(errs.CodeScopeChanged,
			"identity names %s/%d, not the proven old label %s/%d", id.VolumeID, id.Device, oldLabel, oldDev)
	}
	id.VolumeID = newLabel
	id.Device = newDev
	out, err := json.Marshal(id)
	if err != nil {
		return "", errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return string(out), nil
}

func rebindDigest(p RebindPreview) (string, error) {
	copy := p
	copy.Digest = ""
	body, err := json.Marshal(copy)
	if err != nil {
		return "", errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return string(domain.DigestOf(rebindPreviewPurpose, body)), nil
}

// RebindOptions is the operator's explicit, object-specific authorization.
type RebindOptions struct {
	Digest          string
	BackupPath      string
	ScheduleStopped bool
	Note            string
}

// RebindResult is what a confirmed rebind changed.
type RebindResult struct {
	ScopeID       string `json:"scope_id"`
	OldGeneration int64  `json:"old_generation"`
	NewGeneration int64  `json:"new_generation"`
	Status        string `json:"status"`
	EventID       string `json:"event_id"`
	Objects       int    `json:"objects"`
	Digest        string `json:"digest"`
}

// ConfirmRebind re-verifies the exact preview, then commits the new generation
// in one transaction. It never clears the global pause, never starts a scan,
// never calls a model, never moves a file and never resumes automatic runs.
func (r *Registrar) ConfirmRebind(ctx context.Context, id domain.ScopeID, opt RebindOptions) (RebindResult, error) {
	var empty RebindResult
	if opt.Digest == "" {
		return empty, errs.New(errs.CodeNeedsInput, "--digest from a fresh `scope rebind` preview is required")
	}
	if !opt.ScheduleStopped {
		return empty, errs.New(errs.CodeNeedsInput,
			"--schedule-stopped is required: the launchd job must be stopped before a rebind")
	}
	if strings.TrimSpace(opt.BackupPath) == "" {
		return empty, errs.New(errs.CodeNeedsInput,
			"--backup <dir> is required: a consistent private backup must exist before a rebind")
	}
	backup, err := filepath.Abs(opt.BackupPath)
	if err != nil {
		return empty, errs.Wrap(errs.CodeInvalidArgument, err)
	}
	fi, err := os.Stat(backup)
	if err != nil || !fi.IsDir() {
		return empty, errs.New(errs.CodeInvalidArgument, "--backup must name an existing directory")
	}

	p, f, err := r.rebindPreview(ctx, id)
	if f != nil {
		defer f.close()
	}
	if err != nil {
		return empty, err
	}
	if p.Digest != opt.Digest {
		return empty, errs.New(errs.CodePlanStale,
			"the rebind evidence changed since the preview; re-run `scope rebind` and use the new digest")
	}
	if len(p.Unresolvable) > 0 {
		return empty, errs.Newf(errs.CodeHeld,
			"%d isolated object(s) cannot be proven still restorable; refusing to rebind: %s",
			len(p.Unresolvable), strings.Join(p.Unresolvable, "; "))
	}
	if !p.GlobalPaused {
		return empty, errs.New(errs.CodePaused,
			"the global pause marker is absent; a rebind must not run while automatic cleanup is enabled")
	}
	if p.PendingOps > 0 {
		return empty, errs.Newf(errs.CodeHeld,
			"%d file operation(s) are still in flight; finish or reconcile them first", p.PendingOps)
	}
	if p.Status == domain.ScopeActive {
		return empty, errs.New(errs.CodeHeld,
			"scope is active; pause it before rebinding so no run can start between the writes")
	}
	if isAncestor(p.Root, backup) {
		return empty, errs.New(errs.CodeInvalidArgument,
			"the backup directory must be outside the scope root")
	}

	// The same in-tree mutex every mutating command takes. It makes a rebind
	// mutually exclusive with a run over this tree even from a second state dir.
	release, err := acquireScopeLock(f.vault)
	if err != nil {
		return empty, err
	}
	defer release()

	items, err := r.isolatedItems(ctx, id)
	if err != nil {
		return empty, err
	}
	return r.commitRebind(ctx, p, items, backup, opt.Note)
}

func (r *Registrar) commitRebind(ctx context.Context, p RebindPreview, items []store.Item,
	backup, note string) (RebindResult, error) {
	var empty RebindResult
	oldLabel := p.OldRootIdentity.Persistent.VolumeID
	newLabel := p.NewRootIdentity.Persistent.VolumeID
	rootJSON, err := json.Marshal(p.NewRootIdentity)
	if err != nil {
		return empty, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	mountJSON, err := json.Marshal(p.NewMountBinding)
	if err != nil {
		return empty, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	writes := make([]store.ScopeRebindItem, 0, len(items))
	oldEvidence := make([]map[string]any, 0, len(items))
	for _, it := range items {
		var w store.ScopeRebindItem
		w.ItemID = it.ID
		ev := map[string]any{"item_id": string(it.ID), "state": it.State}
		if w.SourceParentIdentityJSON, err = RemapRuntimeLabel(it.SourceParentIdentityJSON, oldLabel, newLabel); err != nil {
			return empty, err
		}
		ev["source_parent_identity_json"] = it.SourceParentIdentityJSON
		if w.PreIdentityJSON, err = RemapRuntimeLabel(it.PreIdentityJSON, oldLabel, newLabel); err != nil {
			return empty, err
		}
		ev["pre_identity_json"] = it.PreIdentityJSON
		if it.CapturedIdentityJSON != nil {
			ev["captured_identity_json"] = *it.CapturedIdentityJSON
			mapped, merr := RemapRuntimeLabel(*it.CapturedIdentityJSON, oldLabel, newLabel)
			if merr != nil {
				return empty, merr
			}
			w.CapturedIdentityJSON = &mapped
		}
		writes = append(writes, w)
		oldEvidence = append(oldEvidence, ev)
	}
	evidence := map[string]any{
		"format_version":         1,
		"scope_id":               p.ScopeID,
		"instance_id":            p.InstanceID,
		"old_generation":         p.Generation,
		"new_generation":         p.NewGeneration,
		"old_root_identity":      p.OldRootIdentity,
		"new_root_identity":      p.NewRootIdentity,
		"old_mount_binding":      p.OldMountBinding,
		"new_mount_binding":      p.NewMountBinding,
		"old_vault_identity":     p.OldVaultIdentity,
		"new_vault_identity":     p.NewVaultIdentity,
		"old_objects_identity":   p.OldObjectsIdentity,
		"new_objects_identity":   p.NewObjectsIdentity,
		"old_anchor_identity":    p.OldAnchorIdentity,
		"new_anchor_identity":    p.NewAnchorIdentity,
		"owner_volume_label":     p.OwnerVolumeLabel,
		"bind_volume_label":      p.BindVolumeLabel,
		"mapping_source":         "structured per-object JSON field rewrite; old label " + oldLabel + " -> " + newLabel,
		"manifests_preserved":    true,
		"preview_digest":         p.Digest,
		"backup_dir":             backup,
		"operator_note":          note,
		"isolated_objects":       oldEvidence,
		"quarantined_count":      p.QuarantinedCount,
		"plans_affected":         p.Plans,
		"grants_affected":        p.Grants,
		"observations_preserved": p.Observations,
	}
	resultJSON, err := json.Marshal(evidence)
	if err != nil {
		return empty, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	contractJSON, err := json.Marshal(map[string]any{
		"scope_id": p.ScopeID, "root": p.Root, "preview_digest": p.Digest,
		"old_generation": p.Generation, "new_generation": p.NewGeneration,
	})
	if err != nil {
		return empty, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	eventID := domain.NewID("ev")
	body := string(resultJSON)
	if err := r.Store.ApplyScopeRebind(ctx, store.ScopeRebindWrite{
		ScopeID:              domain.ScopeID(p.ScopeID),
		ExpectedGeneration:   p.Generation,
		NewRootIdentityJSON:  string(rootJSON),
		NewVaultIdentityJSON: p.NewVaultIdentity,
		MountBindingKey:      mountBindingKey(domain.ScopeID(p.ScopeID)),
		NewMountBindingJSON:  string(mountJSON),
		Items:                writes,
		Event: store.Event{
			EventID:      eventID,
			Action:       "scope_rebind",
			Phase:        domain.PhaseInfo,
			OccurredAtNS: r.Now(),
			ContractJSON: string(contractJSON),
			ResultJSON:   &body,
		},
	}); err != nil {
		return empty, err
	}
	return RebindResult{
		ScopeID:       p.ScopeID,
		OldGeneration: p.Generation,
		NewGeneration: p.NewGeneration,
		Status:        domain.ScopePaused,
		EventID:       eventID,
		Objects:       p.QuarantinedCount,
		Digest:        p.Digest,
	}, nil
}
