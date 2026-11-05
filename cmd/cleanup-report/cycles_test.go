package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCycle23UnknownUseHasNoInventedTimestampOrSource(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "old.tmp")
	os.WriteFile(file, []byte("data"), 0600)
	old := time.Now().Add(-90 * 24 * time.Hour)
	os.Chtimes(file, old, old)
	tools := t.TempDir()
	os.WriteFile(filepath.Join(tools, "mdls"), []byte("#!/bin/sh\nprintf '(null)\\n'\n"), 0700)
	t.Setenv("PATH", tools)
	var out, errout bytes.Buffer
	code := run([]string{"--root", root, "--spotlight-limit", "1"}, &out, &errout)
	if code != 0 {
		t.Fatalf("%d %s", code, errout.String())
	}
	var doc struct {
		Files []struct {
			Facts map[string]any `json:"facts"`
		} `json:"files"`
	}
	json.Unmarshal(out.Bytes(), &doc)
	f := doc.Files[0].Facts
	if _, ok := f["last_used"]; ok {
		t.Fatal("unknown usage has a timestamp")
	}
	if _, ok := f["last_used_source"]; ok {
		t.Fatal("failed probe claims observed source")
	}
	// The status has to say "we do not know", but the word for it depends on the
	// platform: Spotlight exists on macOS, so a probe was asked for and came back
	// unknown; elsewhere there is nothing to ask, and the report says the question
	// was never put. Both are honest; "observed" and a timestamp are not.
	wantStatus := "not_requested"
	if runtime.GOOS == "darwin" {
		wantStatus = "unknown"
	}
	if f["last_used_status"] != wantStatus || f["process_visibility"] != "not_requested" {
		t.Fatalf("missing unknown provenance: %+v", f)
	}
}
func TestCycle26CLIHonorsExplicitKnownReference(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "old.tmp")
	os.WriteFile(p, []byte("data"), 0600)
	old := time.Now().Add(-90 * 24 * time.Hour)
	os.Chtimes(p, old, old)
	var out, stderr bytes.Buffer
	if code := run([]string{"--root", root, "--reference", "old.tmp"}, &out, &stderr); code != 0 {
		t.Fatalf("known reference unavailable: %d %s", code, stderr.String())
	}
	var doc struct {
		Files []struct {
			Facts struct {
				Known bool `json:"known_reference"`
			} `json:"facts"`
			Assessment struct {
				Disposition string `json:"disposition"`
			} `json:"assessment"`
		} `json:"files"`
	}
	json.Unmarshal(out.Bytes(), &doc)
	if len(doc.Files) != 1 || !doc.Files[0].Facts.Known || doc.Files[0].Assessment.Disposition != "keep" {
		t.Fatal("declared referenced file not preserved")
	}
}
func TestCycle28ReportCannotWriteIntoObservedScope(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "report.json")
	var out, stderr bytes.Buffer
	if code := run([]string{"--root", root, "--output", p}, &out, &stderr); code == 0 {
		t.Fatal("read-only report wrote inside observed scope")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("scope mutated by report output")
	}
}
func TestCycle29FailedWriteDoesNotPublishPartialReport(t *testing.T) {
	p := filepath.Join(t.TempDir(), "report.json")
	err := writeReportFile(p, func(w io.Writer) error { w.Write([]byte(`{"incomplete":`)); return errors.New("disk write failure") })
	if err == nil {
		t.Fatal("write failure was ignored")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("partial report left at final path")
	}
}

func TestB03CountsAlwaysExposeKnownDispositionKeys(t *testing.T) {
	root := t.TempDir()
	var out, stderr bytes.Buffer
	if code := run([]string{"--root", root}, &out, &stderr); code != 0 {
		t.Fatalf("run=%d stderr=%s", code, stderr.String())
	}
	var doc struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"keep", "needs_review"} {
		if _, ok := doc.Counts[key]; !ok {
			t.Fatalf("counts omitted %q: %#v", key, doc.Counts)
		}
	}
}
