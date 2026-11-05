package assess

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestB01RootOpenProbeMatchesAbsoluteChildren(t *testing.T) {
	probe := ProbeOpenFiles(context.Background(), "/", func(context.Context, string, ...string) ([]byte, error) {
		return []byte("n/etc/hosts\x00"), nil
	})
	if !probe.Paths["/etc/hosts"] || probe.VisibleMatches != 1 {
		t.Fatalf("root probe lost child path: %+v", probe)
	}
}

func TestB02FactsJSONUsesStableSnakeCaseNames(t *testing.T) {
	b, err := json.Marshal(Facts{Device: 1, Inode: 2, NLink: 1, Regular: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"device", "inode", "nlink", "regular"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %q in %s", key, b)
		}
	}
	for _, key := range []string{"Device", "Inode", "NLink", "Regular"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("unstable exported field %q in %s", key, b)
		}
	}
}

func TestB04ConfigurationFormatsStayPreserved(t *testing.T) {
	for _, path := range []string{"config.toml", "app.ini", "service.conf", "Info.plist", "gradle.properties"} {
		assertKeptRecord(t, path)
	}
}

func TestB04GenericXMLRemainsReviewable(t *testing.T) {
	r := Assess(oldRegular("旧发票.xml"), 30, time.Now())
	if r.Disposition != "needs_review" || r.Category != "document_or_backup_candidate" || !slices.Contains(r.Missing, "document_disposal_basis") || r.CanDelete {
		t.Fatalf("generic document XML must require disposal evidence without an extension exemption: %+v", r)
	}
}

func TestB05UserRecordFormatsStayPreserved(t *testing.T) {
	for _, path := range []string{"voice.m4a", "recording.wav", "mail.mbox", "message.eml", "data.sqlite3", "archive.pst"} {
		assertKeptRecord(t, path)
	}
}

func TestB06OversizedObservationWindowFailsClosed(t *testing.T) {
	r := Assess(Facts{Path: "old.tmp", Regular: true, NLink: 1, MTime: time.Now().Add(-10 * 365 * 24 * time.Hour), AllocatedBytes: 4096}, 3651, time.Now())
	if r.Disposition != "keep" || !slices.Contains(r.Reasons, "invalid_observation_window") {
		t.Fatalf("oversized observation window produced candidate: %+v", r)
	}
}

func TestB07WellKnownProjectFilesStayPreserved(t *testing.T) {
	for _, path := range []string{"Makefile", "Dockerfile", "CMakeLists.txt", "Cargo.lock", "go.mod", "go.sum", "Gemfile"} {
		assertKeptRecord(t, path)
	}
}

func TestB08ArchiveFormatsRemainUnknownContents(t *testing.T) {
	for _, path := range []string{"bundle.tgz", "bundle.tbz2", "bundle.xz", "bundle.rar", "bundle.zst"} {
		r := Assess(oldRegular(path), 30, time.Now())
		if r.Category != "archive_unknown_contents" || r.CanDelete {
			t.Fatalf("archive classified as ordinary stale file: %s %+v", path, r)
		}
	}
}

func TestB09InstallerFormatsRequireRetrievalEvidence(t *testing.T) {
	for _, path := range []string{"tool.appimage", "tool.deb", "tool.rpm", "tool.apk"} {
		r := Assess(oldRegular(path), 30, time.Now())
		if r.Category != "installer_candidate" || !slices.Contains(r.Missing, "verified_retrieval_source") {
			t.Fatalf("installer evidence omitted: %s %+v", path, r)
		}
	}
}

func TestB10BackupSuffixesCarryDisposalRequirement(t *testing.T) {
	for _, path := range []string{"records.bak", "records.old", "records.orig"} {
		r := Assess(oldRegular(path), 30, time.Now())
		if r.Category != "document_or_backup_candidate" || !slices.Contains(r.Missing, "document_disposal_basis") {
			t.Fatalf("backup-specific evidence omitted: %s %+v", path, r)
		}
	}
}

func assertKeptRecord(t *testing.T, path string) {
	t.Helper()
	r := Assess(oldRegular(path), 30, time.Now())
	if r.Disposition != "keep" || r.PurgeReclaimUpperBound != 0 {
		t.Fatalf("user record became space candidate: %s %+v", path, r)
	}
}

func oldRegular(path string) Facts {
	now := time.Now()
	return Facts{Path: path, Regular: true, NLink: 1, MTime: now.Add(-90 * 24 * time.Hour), ATime: now.Add(-90 * 24 * time.Hour), AllocatedBytes: 4096}
}

func TestB11RelativeReferencePathsAreScopedToRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "record.txt"), []byte("record"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Scan(context.Background(), root, Options{MaxEntries: 10, Days: 30, Now: time.Now(), ReferencePaths: map[string]bool{"data": true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range rep.Files {
		if file.Facts.Path == "data/record.txt" && !file.Facts.KnownReference {
			t.Fatal("relative reference path was not scoped to the scan root")
		}
	}
}

func TestB12TemporaryOriginDoesNotEraseArchiveUncertainty(t *testing.T) {
	r := Assess(Facts{Path: "tmp/bundle.zip", Regular: true, NLink: 1, TemporaryOrigin: "operator_declared", AllocatedBytes: 4096}, 30, time.Now())
	if r.Category != "archive_unknown_contents" {
		t.Fatalf("temporary origin erased unknown archive contents: %+v", r)
	}
}

func TestB13TerminalKeepDoesNotClaimPurposeReviewIsPending(t *testing.T) {
	r := Assess(oldRegular("backup.json"), 30, time.Now())
	if r.Disposition != "keep" {
		t.Fatalf("JSON record was not preserved: %+v", r)
	}
	if slices.Contains(r.Reasons, "important_name_requires_purpose_review") {
		t.Fatalf("terminal keep carries contradictory review reason: %+v", r)
	}
}

func TestB14SensitiveCredentialDirectoriesAreNotTraversed(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".ssh", ".gnupg", ".aws"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "secret"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Scan(context.Background(), root, Options{MaxEntries: 20, Days: 30, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 0 || rep.SkippedDirectories != 3 {
		t.Fatalf("sensitive directory contents were inventoried: %+v", rep)
	}
}

func TestB15MacOSBundlesAreSkippedLikeApplications(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"Kit.framework", "Tool.bundle", "Addon.plugin"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "binary"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Scan(context.Background(), root, Options{MaxEntries: 20, Days: 30, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 0 || rep.SkippedDirectories != 3 {
		t.Fatalf("bundle contents were inventoried as loose files: %+v", rep)
	}
}

func TestB16SkippedDirectoriesAreAuditableByPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	rep, err := Scan(context.Background(), root, Options{MaxEntries: 10, Days: 30, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SkippedPaths []string `json:"skipped_paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if rep.SkippedDirectories != 1 || !slices.Contains(doc.SkippedPaths, ".git") {
		t.Fatalf("skipped scope cannot be audited: %+v", rep)
	}
}

func TestB17UnknownLastUsedDoesNotClaimADataSource(t *testing.T) {
	obs := ProbeLastUsed(context.Background(), "/scope/file", func(context.Context, string, ...string) ([]byte, error) {
		return []byte("(null)"), nil
	})
	if obs.Status != "unknown" || obs.Source != "" || !obs.At.IsZero() {
		t.Fatalf("unknown observation claimed evidence: %+v", obs)
	}
}

func TestB18PartialDownloadNamesNeedOriginEvidence(t *testing.T) {
	for _, path := range []string{"movie.part", "archive.crdownload", "package.download"} {
		r := Assess(oldRegular(path), 30, time.Now())
		if r.Category != "temporary_name_only" || !slices.Contains(r.Missing, "temporary_origin") {
			t.Fatalf("partial download was not identified as a name-only signal: %s %+v", path, r)
		}
	}
}
