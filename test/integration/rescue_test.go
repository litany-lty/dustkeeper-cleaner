package integration

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/rescue"
)

// treeSnapshot lists every path under dir with its size, so a test can prove an
// inventory changed nothing.
func treeSnapshot(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		out[rel] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func diffSnapshots(before, after map[string]int64) []string {
	var diff []string
	for k, v := range before {
		if got, ok := after[k]; !ok {
			diff = append(diff, "removed: "+k)
		} else if got != v {
			diff = append(diff, "resized: "+k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			diff = append(diff, "added: "+k)
		}
	}
	sort.Strings(diff)
	return diff
}

// V23: with the database destroyed, the manifests inside the scope still describe
// what was moved, and inventorying them changes nothing.
func TestManifestInventoryWorksWithoutTheDatabase(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "recoverable-payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	root := filepath.Join(e.base, "trees", "exports")
	before := treeSnapshot(t, root)

	// Destroy the database, which is the situation the independent record exists
	// for.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(e.paths.DBFile + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove database%s: %v", suffix, err)
		}
	}

	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory without a database: %v", err)
	}
	if len(rep.Entries) != 1 {
		t.Fatalf("expected one inventory entry, got %d (%+v)", len(rep.Entries), rep.Entries)
	}
	entry := rep.Entries[0]
	if entry.Status != rescue.StatusComplete {
		t.Fatalf("the object is %q, want %q (%+v)", entry.Status, rescue.StatusComplete, entry)
	}
	if entry.RawRelative != "old.zip" {
		t.Fatalf("the inventory recovered path %q, want old.zip", entry.RawRelative)
	}
	if entry.ContentHash == "" || entry.MetadataHash == "" {
		t.Fatalf("the inventory lost the recorded evidence: %+v", entry)
	}
	if entry.ObjectSize == 0 {
		t.Fatal("the inventory did not record the object size")
	}
	if rep.InstanceID != "inst_test" {
		t.Fatalf("owner instance = %q, want inst_test", rep.InstanceID)
	}

	if diff := diffSnapshots(before, treeSnapshot(t, root)); len(diff) != 0 {
		t.Fatalf("the inventory changed the scope: %v", diff)
	}
}

// V23: an object with no manifest, and a non-file entry inside the objects
// directory, are listed and left alone. Neither is deleted and neither is adopted
// as an authorised object.
func TestManifestInventoryListsUnexplainedEntries(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	root := filepath.Join(e.base, "trees", "exports")
	objects := filepath.Join(root, ".dustkeeper", "objects")
	// An object that never had a manifest: nothing here says which file it was.
	orphan := filepath.Join(objects, "obj_00000000000000000000000000000000")
	if err := os.WriteFile(orphan, []byte("unknown bytes"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	// A payload that is not a regular file at all.
	if err := os.Mkdir(filepath.Join(objects, "not-an-object"), 0o700); err != nil {
		t.Fatalf("mkdir unknown payload: %v", err)
	}

	before := treeSnapshot(t, root)
	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if rep.Orphans != 1 {
		t.Fatalf("orphan objects = %d, want 1 (%+v)", rep.Orphans, rep.Entries)
	}
	if rep.Unknown != 1 {
		t.Fatalf("unknown payloads = %d, want 1 (%+v)", rep.Unknown, rep.Entries)
	}
	if rep.Complete != 1 {
		t.Fatalf("complete objects = %d, want 1", rep.Complete)
	}

	for _, entry := range rep.Entries {
		switch entry.Status {
		case rescue.StatusOrphanObject:
			if entry.RawRelative != "" {
				t.Fatal("an orphan object was given a reconstructed path")
			}
		case rescue.StatusUnknownPayload:
			if entry.Object != "not-an-object" {
				t.Fatalf("unknown payload name = %q", entry.Object)
			}
		case rescue.StatusComplete:
			// The one real object, already checked above.
		default:
			t.Fatalf("unexpected inventory status %q", entry.Status)
		}
	}
	if diff := diffSnapshots(before, treeSnapshot(t, root)); len(diff) != 0 {
		t.Fatalf("the inventory changed the scope: %v", diff)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("the orphan object was removed: %v", err)
	}
}

// A23: an objects directory that cannot be opened proves only that the check did
// not happen. Reporting manifest_without_object would turn "I could not look"
// into "your file is gone", which is the one conclusion a rescue inventory must
// never invent.
func TestAnUnreadableObjectsDirectoryDoesNotClaimTheObjectsAreGone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "recoverable-payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	root := filepath.Join(e.base, "trees", "exports")
	before := treeSnapshot(t, root)
	objects := filepath.Join(root, domain.VaultObjects)
	if err := os.Chmod(objects, 0o000); err != nil {
		t.Fatalf("chmod objects: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(objects, 0o700) })

	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory with an unreadable objects directory: %v", err)
	}
	if rep.ObjectState != rescue.ObjectStateUnreadable {
		t.Fatalf("object_state = %q, want %q", rep.ObjectState, rescue.ObjectStateUnreadable)
	}
	if rep.Missing != 0 {
		t.Fatalf("manifest_without_object = %d; an unreadable directory does not prove absence (%+v)",
			rep.Missing, rep.Entries)
	}
	if rep.Complete != 0 {
		t.Fatalf("complete = %d; the inventory could not look", rep.Complete)
	}
	if rep.Unverified != 1 || len(rep.Entries) != 1 {
		t.Fatalf("unverified = %d with %d entries, want 1/1", rep.Unverified, len(rep.Entries))
	}
	entry := rep.Entries[0]
	if entry.Status != rescue.StatusUnverified {
		t.Fatalf("entry status = %q, want %q (%+v)", entry.Status, rescue.StatusUnverified, entry)
	}
	// The path and the evidence still come from the manifest: the operator keeps
	// what the scope recorded even when the objects cannot be examined.
	if entry.RawRelative != "old.zip" || entry.ContentHash == "" {
		t.Fatalf("the manifest facts were lost: %+v", entry)
	}

	if err := os.Chmod(objects, 0o700); err != nil {
		t.Fatalf("restore mode: %v", err)
	}
	if diff := diffSnapshots(before, treeSnapshot(t, root)); len(diff) != 0 {
		t.Fatalf("the inventory changed the scope: %v", diff)
	}
}

// A23: a whole objects directory that is absent is a different fact from one
// that cannot be read, and it does establish that the objects are not there.
func TestAMissingObjectsDirectoryIsReportedAsMissingNotUnverified(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}

	root := filepath.Join(e.base, "trees", "exports")
	if err := os.RemoveAll(filepath.Join(root, domain.VaultObjects)); err != nil {
		t.Fatalf("remove objects: %v", err)
	}

	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory with a missing objects directory: %v", err)
	}
	if rep.ObjectState != rescue.ObjectStateMissing {
		t.Fatalf("object_state = %q, want %q", rep.ObjectState, rescue.ObjectStateMissing)
	}
	if rep.Unverified != 0 {
		t.Fatalf("unverified = %d; an absent directory is established, not unknown", rep.Unverified)
	}
	if rep.Missing != 1 || len(rep.Entries) != 1 {
		t.Fatalf("manifest_without_object = %d with %d entries, want 1/1 (%+v)",
			rep.Missing, len(rep.Entries), rep.Entries)
	}
	if got := rep.Entries[0].Status; got != rescue.StatusMissingObject {
		t.Fatalf("entry status = %q, want %q", got, rescue.StatusMissingObject)
	}
	// The manifests still name the file: the independent record outlives the
	// payload, which is what makes the loss explainable.
	if rep.Entries[0].RawRelative != "old.zip" {
		t.Fatalf("the manifest facts were lost: %+v", rep.Entries[0])
	}
}

// A directory that is not a managed scope is reported as such, not as an empty
// vault. R13 asks for a rescue path that fails closed.
func TestManifestInventoryRefusesAnUnmanagedDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := rescue.Inspect(dir); err == nil {
		t.Fatal("a directory with no management directory was inventoried as an empty vault")
	}
}

// The value of a rescue path is that it does not depend on the installation being
// healthy: no state directory, no lock, no database.
func TestManifestInventoryNeedsNoStateDirectory(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "old.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	root := filepath.Join(e.base, "trees", "exports")

	// Remove the whole state directory, then inventory the scope.
	if err := os.RemoveAll(e.paths.StateDir); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	if _, err := os.Stat(e.paths.StateDir); !os.IsNotExist(err) {
		t.Fatalf("the state directory still exists: %v", err)
	}
	rep, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("inventory after the state directory was removed: %v", err)
	}
	if rep.Complete != 1 {
		t.Fatalf("complete objects = %d, want 1", rep.Complete)
	}
}
