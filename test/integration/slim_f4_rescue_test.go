package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/rescue"
)

// The rescue inventory is the only thing an operator has when the database is
// gone, so the one sentence it must never write is "your file is gone" on
// evidence that only says "I did not look". These cases cover the two shapes of
// that mistake: an entry that exists but could not be described, and a directory
// that stopped answering halfway through.

// quarantineAndLocate runs one object through the pipeline and returns the scope
// root and the object file path inside the vault.
func quarantineAndLocate(t *testing.T, e *env) (root, objectPath string) {
	t.Helper()
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "recoverable-payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	items := e.itemsFor(res.Plan)
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	root = filepath.Join(e.base, "trees", "exports")
	return root, filepath.Join(root, domain.VaultObjects, string(items[0].ObjectName))
}

// An entry that is listed and cannot be described is not an entry that is
// absent. A directory with read but not search permission produces exactly that:
// the names come back, the stats do not.
func TestSlimRescueEntryLevelUnverified(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	e := newEnv(t)
	root, _ := quarantineAndLocate(t, e)
	objects := filepath.Join(root, domain.VaultObjects)
	if err := os.Chmod(objects, 0o400); err != nil {
		t.Fatalf("chmod objects: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(objects, 0o700) })

	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if rep.ObjectState != rescue.ObjectStateRead {
		t.Fatalf("object_state = %q, want %q: the names were listed", rep.ObjectState, rescue.ObjectStateRead)
	}
	if rep.Missing != 0 {
		t.Fatalf("manifest_without_object = %d; an entry that could not be described is not absent (%+v)",
			rep.Missing, rep.Entries)
	}
	if rep.Unverified != 1 || len(rep.Entries) != 1 {
		t.Fatalf("unverified = %d with %d entries, want 1/1 (%+v)", rep.Unverified, len(rep.Entries), rep.Entries)
	}
	entry := rep.Entries[0]
	if entry.Status != rescue.StatusUnverified {
		t.Fatalf("entry status = %q, want %q", entry.Status, rescue.StatusUnverified)
	}
	if entry.RawRelative != "old.zip" {
		t.Fatalf("the manifest facts were lost: %+v", entry)
	}
	if !contains(strings.Join(entry.Notes, " "), "could not be examined") {
		t.Fatalf("the entry does not say why it is unverified: %+v", entry.Notes)
	}
}

// A listing that stopped halfway cannot support "this object is gone", even for
// an object that really is gone: the same inventory run to completion does make
// that claim, which is what proves the difference is the completeness of the
// listing and not a different code path.
func TestSlimRescueDirectoryLevelUnverified(t *testing.T) {
	e := newEnv(t)
	root, objectPath := quarantineAndLocate(t, e)
	// The object really is gone. Only the listing state differs between the two
	// inventories below.
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove object: %v", err)
	}

	rescue.TestHookListAfter = func(rel string, seen int) error {
		if strings.HasSuffix(rel, domain.VaultObjects) && seen > 0 {
			return rescue.ErrListingStopped
		}
		return nil
	}
	t.Cleanup(func() { rescue.TestHookListAfter = nil })

	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if rep.ObjectState != rescue.ObjectStatePartial {
		t.Fatalf("object_state = %q, want %q", rep.ObjectState, rescue.ObjectStatePartial)
	}
	if rep.Missing != 0 {
		t.Fatalf("manifest_without_object = %d; a partial listing does not establish absence (%+v)",
			rep.Missing, rep.Entries)
	}
	if rep.Unverified != 1 {
		t.Fatalf("unverified = %d, want 1 (%+v)", rep.Unverified, rep.Entries)
	}
	entry := rep.Entries[0]
	if entry.Status != rescue.StatusUnverified {
		t.Fatalf("entry status = %q, want %q", entry.Status, rescue.StatusUnverified)
	}
	if !contains(strings.Join(entry.Notes, " "), "could not be read to the end") {
		t.Fatalf("the entry does not say why it is unverified: %+v", entry.Notes)
	}

	// The same tree, read to the end, does report the loss. Without this the case
	// above would also pass in an inventory that never reports a missing object.
	rescue.TestHookListAfter = nil
	rep, err = rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if rep.ObjectState != rescue.ObjectStateRead {
		t.Fatalf("object_state = %q, want %q", rep.ObjectState, rescue.ObjectStateRead)
	}
	if rep.Missing != 1 {
		t.Fatalf("manifest_without_object = %d, want 1 (%+v)", rep.Missing, rep.Entries)
	}
	if got := rep.Entries[0].Status; got != rescue.StatusMissingObject {
		t.Fatalf("entry status = %q, want %q", got, rescue.StatusMissingObject)
	}
}

// The manifests directory has the same rule in the other direction: an object
// whose manifest was not listed is not an object without a manifest.
func TestSlimRescuePartialManifestsDoNotBecomeOrphans(t *testing.T) {
	e := newEnv(t)
	root, _ := quarantineAndLocate(t, e)
	// A payload that never had a manifest, which is what an orphan is.
	stray := filepath.Join(root, domain.VaultObjects, "obj_ffffffffffffffffffffffffffffffff")
	if err := os.WriteFile(stray, []byte("unknown bytes"), 0o600); err != nil {
		t.Fatalf("write stray object: %v", err)
	}

	rescue.TestHookListAfter = func(rel string, seen int) error {
		if strings.HasSuffix(rel, domain.VaultManifests) && seen > 0 {
			return rescue.ErrListingStopped
		}
		return nil
	}
	t.Cleanup(func() { rescue.TestHookListAfter = nil })

	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if rep.ManifestState != rescue.ManifestStatePartial {
		t.Fatalf("manifest_state = %q, want %q", rep.ManifestState, rescue.ManifestStatePartial)
	}
	if rep.Orphans != 0 {
		t.Fatalf("orphan_objects = %d; an unread manifests directory does not prove a manifest is absent (%+v)",
			rep.Orphans, rep.Entries)
	}
	if rep.Unverified != 1 {
		t.Fatalf("unverified = %d, want 1 (%+v)", rep.Unverified, rep.Entries)
	}
	var unverified *rescue.Entry
	for i := range rep.Entries {
		if rep.Entries[i].Status == rescue.StatusUnverified {
			unverified = &rep.Entries[i]
		}
	}
	if unverified == nil {
		t.Fatalf("no entry is unverified: %+v", rep.Entries)
	}
	if !contains(strings.Join(unverified.Notes, " "), "may have a manifest that was not listed") {
		t.Fatalf("the entry does not say why it is unverified: %+v", unverified.Notes)
	}

	// Read to the end, the same tree does call it an orphan.
	rescue.TestHookListAfter = nil
	rep, err = rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("orphan_objects = %d, want 1 (%+v)", rep.Orphans, rep.Entries)
	}
}
