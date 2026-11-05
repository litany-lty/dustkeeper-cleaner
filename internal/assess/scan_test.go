package assess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRound5BoundedReadOnlyScanDoesNotFollowLinksOrVault(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(root, "candidate.tmp"), []byte("abc"), 0600)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("data"), 0600)
	os.Mkdir(filepath.Join(root, ".dustkeeper"), 0700)
	os.WriteFile(filepath.Join(root, ".dustkeeper", "object"), []byte("vault"), 0600)
	os.Symlink(outside, filepath.Join(root, "linked"))
	rep, err := Scan(context.Background(), root, Options{MaxEntries: 100, Days: 30, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Files {
		if f.Facts.Path == "linked/secret" || f.Facts.Path == ".dustkeeper/object" {
			t.Fatal("walk escaped or scanned vault")
		}
	}
	if !rep.Complete {
		t.Fatal("unexpected incomplete")
	}
	b, _ := os.ReadFile(filepath.Join(root, "candidate.tmp"))
	if string(b) != "abc" {
		t.Fatal("source changed")
	}
	rep, err = Scan(context.Background(), root, Options{MaxEntries: 1, Days: 30, Now: time.Now()})
	if err != nil || rep.Complete {
		t.Fatal("bounded scan claimed complete")
	}
}
func TestRound10ProtectedSubtreeDoesNotConsumeScanBudget(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "pdf"), 0700)
	for i := 0; i < 20; i++ {
		os.WriteFile(filepath.Join(root, "pdf", fmt.Sprintf("book%d.pdf", i)), []byte("private"), 0600)
	}
	os.WriteFile(filepath.Join(root, "z.tmp"), []byte("candidate"), 0600)
	rep, err := Scan(context.Background(), root, Options{MaxEntries: 3, Days: 30, Now: time.Now(), ProtectedPrefixes: []string{"pdf"}})
	if err != nil || !rep.Complete || rep.SkippedDirectories != 1 || len(rep.Files) != 1 {
		t.Fatalf("protected subtree consumed budget: %+v %v", rep, err)
	}
}
