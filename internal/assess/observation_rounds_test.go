package assess

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestO01OpenFileObservationResolvesAliasesAndExitOne(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDir, "open.zip")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	// Parent symlink simulates macOS /tmp -> /private/tmp or user symlinked parent.
	linkParent := filepath.Join(base, "linkparent")
	if err := os.Symlink(realDir, linkParent); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(base, "held-via-link.zip")
	if err := os.Symlink(target, aliasPath); err != nil {
		t.Fatal(err)
	}

	// 1) ProbeOpenFiles must handle symlink-aliased roots and macOS lsof exit-1.
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	obs := ProbeOpenFiles(context.Background(), linkParent, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("p99\x00n" + canonicalTarget + "\x00"), &exec.ExitError{ProcessState: exitedWith(1)}
	})
	if obs.Coverage != "visible_processes_only" || !obs.Paths[canonicalTarget] {
		t.Fatalf("ProbeOpenFiles missed canonical match or misread exit-1: %+v", obs)
	}

	// 2) Scan must resolve symlink aliases in OpenPaths by canonical path/inode.
	rep, err := Scan(context.Background(), realDir, Options{
		MaxEntries:      100,
		Days:            30,
		Now:             time.Now(),
		OpenPaths:       map[string]bool{aliasPath: true},
		ProcessCoverage: "visible_processes_only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 || !rep.Files[0].Facts.OpenByProcess || rep.Files[0].Assessment.Disposition != "keep" {
		t.Fatalf("symlink-aliased open file was not marked open_by_visible_process/keep: %+v", rep.Files)
	}
}

func TestO02ProtectedBundlesAndVCSDoNotExhaustScanBudget(t *testing.T) {
	root := t.TempDir()
	for dir, count := range map[string]int{
		"Vacation.photoslibrary": 15,
		"App.xcodeproj":          10,
		".hg":                    5,
	} {
		sub := filepath.Join(root, dir)
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < count; i++ {
			if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%02d.dat", i)), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	candidate := filepath.Join(root, "z-old.tmp")
	if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(candidate, old, old); err != nil {
		t.Fatal(err)
	}

	rep, err := Scan(context.Background(), root, Options{
		MaxEntries: 10,
		Days:       30,
		Now:        time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Complete || rep.SkippedDirectories != 3 || len(rep.Files) != 1 || rep.Files[0].Facts.Path != "z-old.tmp" {
		t.Fatalf("protected bundle/VCS dirs exhausted scan budget or were traversed: complete=%v skipped=%d files=%+v issues=%v",
			rep.Complete, rep.SkippedDirectories, rep.Files, rep.Issues)
	}
}

func TestO03ImmutableAndCloudDatalessFilesStayKept(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	r1 := Assess(Facts{
		Path:      "locked.tmp",
		LogicalBytes: 12,
		Regular:   true,
		NLink:     1,
		MTime:     old,
		Immutable: true,
	}, 30, time.Now())
	if r1.Disposition != "keep" || r1.PurgeReclaimUpperBound != 0 {
		t.Fatalf("immutable file must be kept with 0 reclaim: %+v", r1)
	}
	r2 := Assess(Facts{
		Path:          "cloud.zip",
		LogicalBytes:  1024,
		Regular:       true,
		NLink:         1,
		MTime:         old,
		CloudDataless: true,
	}, 30, time.Now())
	if r2.Disposition != "keep" || r2.PurgeReclaimUpperBound != 0 {
		t.Fatalf("cloud dataless file must be kept with 0 reclaim: %+v", r2)
	}
}

func TestLiveObsCredentialBasenamesProtected(t *testing.T) {
	for _, name := range []string{"aws_credentials", "gcp-credentials.bak", "db_credential.tmp"} {
		c := ClassifyName(name)
		if c.Category != CatCredential || !c.KeepName {
			t.Fatalf("%q must classify as CatCredential/KeepName, got %+v", name, c)
		}
	}
}
