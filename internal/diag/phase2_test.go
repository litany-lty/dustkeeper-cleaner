package diag

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloseReportsRotationFailure(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.rot.max = 128
	logger.Log("first", "detail", strings.Repeat("x", 64))
	if err := os.Mkdir(filepath.Join(dir, FileName+".3"), 0o700); err != nil {
		t.Fatal(err)
	}

	logger.Log("forces rotation", "detail", strings.Repeat("y", 128))
	if err := logger.Close(); err == nil {
		t.Fatal("rotation failure was swallowed; Close reported success")
	}
}

func TestErrorValueIsCappedLikeAString(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Log("failure", "error", errors.New(strings.Repeat("e", MaxFileBytes+1024)))
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > MaxFieldBytes+1024 {
		t.Fatalf("error value bypassed field cap: log size=%d", info.Size())
	}
	records := readLines(t, filepath.Join(dir, FileName))
	value, _ := records[0]["error"].(string)
	if !strings.HasSuffix(value, truncated) {
		t.Fatalf("capped error lacks truncation marker: len=%d", len(value))
	}
}
