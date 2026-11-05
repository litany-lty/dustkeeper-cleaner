package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRound7ReadOnlyCLIProducesReviewListNotDeletePlan(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "old.tmp"), []byte("payload"), 0600)
	var out, errout bytes.Buffer
	if code := run([]string{"--root", root, "--max-entries", "20"}, &out, &errout); code != 0 {
		t.Fatalf("%d %s", code, errout.String())
	}
	var v struct {
		ActualFreedBytes int64 `json:"actual_freed_bytes"`
		DeleteAuthorized bool  `json:"delete_authorized"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.ActualFreedBytes != 0 || v.DeleteAuthorized {
		t.Fatal("report claimed deletion")
	}
	b, _ := os.ReadFile(filepath.Join(root, "old.tmp"))
	if string(b) != "payload" {
		t.Fatal("source changed")
	}
}
