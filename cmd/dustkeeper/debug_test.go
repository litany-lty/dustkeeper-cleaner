package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
)

func TestDebugPlanDoesNotOpenStateOrCallModels(t *testing.T) {
	base := newHome(t)
	root := filepath.Join(base, "candidates")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "obsolete-notes.txt")
	if err := os.WriteFile(file, []byte("old generated notes"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	r := cli(t, "debug", "--root", root, "--scope-id", "debug-fixture", "--max-candidates", "1", "--json")
	if r.code != 0 {
		t.Fatalf("debug plan failed: %s", r.stdout)
	}
	d := dataOf(t, r)
	if d["mode"] != "debug" || d["model_calls"] != float64(0) || d["actual_deleted_files"] != float64(0) || d["actual_freed_bytes"] != float64(0) {
		t.Fatalf("not a read-only simulation: %+v", d)
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.DBFile); !os.IsNotExist(err) {
		t.Fatalf("debug created/opened state: %v", err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "old generated notes" {
		t.Fatalf("source changed: %q %v", b, err)
	}
}

func TestDebugUsesRealReviewerAndLeavesFileAndStateUnchanged(t *testing.T) {
	clk := domain.NewFakeClock(time.Now())
	base, tree := scopeWithRule(t, clk)
	installClockReviewFixture(t, base, tree, clk)
	file := filepath.Join(tree, "old.zip")
	old := clk.Wall().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	dbBefore, err := os.ReadFile(paths.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	r := cliOn(t, clk, "debug", "--root", tree, "--scope-id", "exports", "--max-candidates", "1", "--execute", "--json")
	if r.code != 0 {
		t.Fatalf("debug run failed: %s", r.stdout)
	}
	d := dataOf(t, r)
	if d["model_calls"] != float64(2) || d["simulated_cleaned_files"] != float64(1) || d["actual_deleted_files"] != float64(0) {
		t.Fatalf("missing live review or fake deletion leaked: %+v", d)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "payload" {
		t.Fatalf("source changed: %q %v", b, err)
	}
	dbAfter, err := os.ReadFile(paths.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(dbAfter) != string(dbBefore) {
		t.Fatal("debug changed real state database")
	}
}
