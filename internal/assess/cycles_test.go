package assess

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCycle01RootProtectionIncludesAllDescendants(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "obsolete.tmp"), []byte("data"), 0600)
	r, err := Scan(context.Background(), root, Options{MaxEntries: 10, Days: 30, Now: time.Now(), ProtectedPrefixes: []string{"."}})
	if err != nil || len(r.Files) != 1 || !r.Files[0].Facts.Protected {
		t.Fatalf("explicit root protection ignored: %+v %v", r, err)
	}
}
func TestCycle02InvalidProtectionCannotSilentlyScan(t *testing.T) {
	_, err := Scan(context.Background(), t.TempDir(), Options{MaxEntries: 10, Days: 30, ProtectedPatterns: []string{"["}})
	if err == nil {
		t.Fatal("invalid protection was silently ignored")
	}
}
func TestCycle07ReferencedDirectoryProtectsItsFiles(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "data"), 0700)
	os.WriteFile(filepath.Join(root, "data", "unique.txt"), []byte("record"), 0600)
	os.Symlink("data", filepath.Join(root, "alias"))
	r, err := Scan(context.Background(), root, Options{MaxEntries: 20, Days: 30, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Files {
		if f.Facts.Path == "data/unique.txt" && !f.Facts.KnownReference {
			t.Fatal("directory reference did not protect descendant")
		}
	}
}
func TestCycle08TempOriginDoesNotEraseInstallerEvidenceRequirement(t *testing.T) {
	r := Assess(Facts{Path: "tmp/tool.dmg", Regular: true, NLink: 1, TemporaryOrigin: "operator_declared", AllocatedBytes: 4096}, 30, time.Now())
	if r.Category != "installer_candidate" || !slices.Contains(r.Missing, "verified_retrieval_source") {
		t.Fatalf("installer evidence erased: %+v", r)
	}
}
func TestCycle09CredentialFilesNeverBecomeSpaceCandidates(t *testing.T) {
	for _, path := range []string{"id_rsa", ".env", ".env.production", "wallet.dat", "vault.kdbx", "client.p12", "client.pfx", "backup.gpg"} {
		r := Assess(Facts{Path: path, Regular: true, NLink: 1, AllocatedBytes: 4096}, 30, time.Now())
		if r.Disposition != "keep" {
			t.Fatalf("credential candidate: %s %+v", path, r)
		}
	}
}
func TestCycle10SupportedSourceFilesStayPreserved(t *testing.T) {
	for _, path := range []string{"build.sh", "main.c", "lib.cpp", "lib.rs", "App.swift", "view.vue"} {
		r := Assess(Facts{Path: path, Regular: true, NLink: 1}, 30, time.Now())
		if r.Disposition != "keep" {
			t.Fatalf("source mistaken for unused data: %s", path)
		}
	}
}
func TestCycle11DocumentFormatsCarryDocumentDisposalRequirement(t *testing.T) {
	for _, path := range []string{"report.pages", "sheet.numbers", "report.docm", "sheet.xlsm", "slides.pptm", "export.sql"} {
		r := Assess(Facts{Path: path, Regular: true, NLink: 1}, 30, time.Now())
		if r.Category != "document_or_backup_candidate" || !slices.Contains(r.Missing, "document_disposal_basis") {
			t.Fatalf("document-specific evidence omitted: %s %+v", path, r)
		}
	}
}
func TestCycle25BasenamePatternCannotSilentlyContainPath(t *testing.T) {
	_, err := Scan(context.Background(), t.TempDir(), Options{MaxEntries: 10, Days: 30, ProtectedPatterns: []string{"private/*.txt"}})
	if err == nil {
		t.Fatal("path pattern accepted but would only match basenames")
	}
}
