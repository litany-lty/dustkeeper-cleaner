package execute

import (
	"os"
	"path/filepath"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

func openExecuteTestFile(t *testing.T, root *fsops.Root, name string) *fsops.File {
	t.Helper()
	parent, err := root.OpenParent([]byte(name))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	file, err := fsops.OpenRegularAt(parent, root.Dev())
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCompareFilesReportsReadErrorBeforeLengthDifference(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("abcdefgh"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	a := openExecuteTestFile(t, root, "a.bin")
	b := openExecuteTestFile(t, root, "b.bin")
	defer a.Close()
	defer b.Close()
	TestHookDuringCompare = func() { _ = a.Close() }
	t.Cleanup(func() { TestHookDuringCompare = nil })
	budget := domain.NewBudget(60, 1<<20, 1, 1<<20, 0)

	_, err = CompareFiles(a, b, VerifyOptions{Buffer: make([]byte, 4), Budget: budget})
	if errs.CodeOf(err) != errs.CodeIOError {
		t.Fatalf("read failure was downgraded to a content difference: %v", err)
	}
	if budget.Run.Read != 12 {
		t.Fatalf("read failure hid bytes already read: charged=%d want=12", budget.Run.Read)
	}
}

func TestCompareFilesChargesReadBudgetBeforeContentMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	a := openExecuteTestFile(t, root, "a.bin")
	b := openExecuteTestFile(t, root, "b.bin")
	defer a.Close()
	defer b.Close()
	budget := domain.NewBudget(60, 4, 1, 1<<20, 0)

	_, err = CompareFiles(a, b, VerifyOptions{Buffer: make([]byte, 4), Budget: budget})
	if errs.CodeOf(err) != errs.CodeBudgetExhausted {
		t.Fatalf("mismatching bytes bypassed read budget: read=%d err=%v", budget.Run.Read, err)
	}
}

func TestCompareFilesChargesReadBudgetBeforeLengthMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("bbbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	a := openExecuteTestFile(t, root, "a.bin")
	b := openExecuteTestFile(t, root, "b.bin")
	defer a.Close()
	defer b.Close()
	budget := domain.NewBudget(60, 1, 1, 1<<20, 0)

	_, err = CompareFiles(a, b, VerifyOptions{Buffer: make([]byte, 8), Budget: budget})
	if errs.CodeOf(err) != errs.CodeBudgetExhausted {
		t.Fatalf("different read lengths bypassed read budget: read=%d err=%v", budget.Run.Read, err)
	}
}
