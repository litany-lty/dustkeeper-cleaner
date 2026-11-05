package rescue

import (
	"os"
	"path/filepath"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

func makeOfflineVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{domain.VaultDirName, domain.VaultObjects, domain.VaultManifests} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	return root
}

// B25: a damaged manifest still exists under the object's name. It is a
// conflict, not evidence that the matching payload has no manifest at all.
func TestB25DamagedManifestDoesNotDuplicateObjectAsOrphan(t *testing.T) {
	root := makeOfflineVault(t)
	const object = "obj_00000000000000000000000000000025"
	if err := os.WriteFile(filepath.Join(root, domain.VaultManifests, object+".json"), []byte(`{"broken"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, domain.VaultObjects, object), []byte("retained payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := Inspect(root)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if rep.Conflicts != 1 || rep.Orphans != 0 || len(rep.Entries) != 1 {
		t.Fatalf("conflicts/orphans/entries = %d/%d/%d, want 1/0/1: %+v",
			rep.Conflicts, rep.Orphans, len(rep.Entries), rep.Entries)
	}
	if got := rep.Entries[0]; got.Status != StatusConflict || got.Object != object {
		t.Fatalf("entry = %+v, want one conflict for %s", got, object)
	}
}

// B26: an existing management entry that cannot be opened safely is not the
// same fact as a missing management directory.
func TestB26UnsafeVaultEntryIsNotReportedAsMissing(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, domain.VaultDirName)); err != nil {
		t.Fatal(err)
	}

	_, err := Inspect(root)
	if err == nil {
		t.Fatal("symlinked vault was accepted")
	}
	if code := errs.CodeOf(err); code != errs.CodeAccessDenied {
		t.Fatalf("code = %s, want ACCESS_DENIED: %v", code, err)
	}
}
