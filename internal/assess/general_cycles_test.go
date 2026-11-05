package assess

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGeneral25ProtectionAliasProtectsTargetSubtree(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "records")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("records", filepath.Join(root, "protected-alias")); err != nil {
		t.Fatal(err)
	}
	r, err := Scan(context.Background(), root, Options{Days: 30, MaxEntries: 20, ProtectedPrefixes: []string{"protected-alias"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range r.Files {
		if f.Facts.Path == "records/old.txt" && !f.Facts.Protected {
			t.Fatal("protected directory reachable through alias was scanned unprotected")
		}
	}
}

func TestGeneral16AssessmentHonorsGlobalFileProtection(t *testing.T) {
	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, path := range []string{".netrc", "records/activity.log", "archive/data.backup"} {
		r := Assess(Facts{Path: path, Regular: true, NLink: 1, MTime: old, ATime: old}, 30, time.Now())
		if r.Disposition != "keep" {
			t.Errorf("globally protected file offered for review: %s %+v", path, r)
		}
	}
}

func TestGeneral09ProbeDeadlineIncludesPipeDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := RunBoundedCommand(ctx, "sh", "-c", "sleep 0.6 & wait")
	if err == nil || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("probe deadline exceeded while child holds pipe: elapsed=%v err=%v", time.Since(start), err)
	}
}

func TestGeneral08DirectoryContextIsNotFileUse(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "old.tmp")
	if err = os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err = os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	obs := ProbeOpenFiles(context.Background(), root, func(context.Context, string, ...string) ([]byte, error) { return []byte("n" + root + "\x00"), nil })
	if !obs.Paths[root] {
		t.Fatal("open root directory omitted from observation")
	}
	r, err := Scan(context.Background(), root, Options{Days: 30, MaxEntries: 20, OpenPaths: obs.Paths, ProcessCoverage: obs.Coverage})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r.Files[0].Facts)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if fields["open_directory_context"] != true || r.Files[0].Facts.OpenByProcess || r.Files[0].Assessment.Disposition != "needs_review" {
		t.Fatal("directory context must be recorded without claiming the file is open")
	}
}

func TestGeneral07ReferenceAliasProtectsSameObject(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "old-document.txt")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			alias := filepath.Join(base, kind)
			if kind == "symlink" {
				err = os.Symlink(file, alias)
			} else {
				err = os.Link(file, alias)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(alias)
			r, err := Scan(context.Background(), root, Options{Days: 30, MaxEntries: 20, ReferencePaths: map[string]bool{alias: true}})
			if err != nil {
				t.Fatal(err)
			}
			if !r.Files[0].Facts.KnownReference {
				t.Fatalf("reference via %s lost", kind)
			}
		})
	}
}
