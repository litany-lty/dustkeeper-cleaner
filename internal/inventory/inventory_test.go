package inventory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"dustkeeper/internal/errs"
)

func TestW3ResumableInventoryReachesTailAndDeduplicatesHardlinks(t *testing.T) {
	root := t.TempDir()
	stateDir := t.TempDir()
	stateFile := filepath.Join(stateDir, "cursor.json")

	mustMkdir := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mustWrite := func(rel string, data []byte) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	mustWrite("a_dir/one.txt", bytes.Repeat([]byte("a"), 4096))
	mustWrite("a_dir/two.txt", bytes.Repeat([]byte("b"), 4096))

	// Hardlink pair: 64 KiB payload linked twice under b_dir.
	h1 := mustWrite("b_dir/hard1.bin", bytes.Repeat([]byte("h"), 64*1024))
	h2 := filepath.Join(root, "b_dir/hard2.bin")
	if err := os.Link(h1, h2); err != nil {
		t.Fatal(err)
	}

	// Sparse file: 16 MiB logical size with zero data written.
	sparsePath := filepath.Join(mustMkdir("c_sparse"), "vm.img")
	sf, err := os.Create(sparsePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Truncate(16 * 1024 * 1024); err != nil {
		sf.Close()
		t.Fatal(err)
	}
	sf.Close()

	// Unreadable directory (when not running as root).
	lockedDir := mustMkdir("d_locked")
	mustWrite("d_locked/secret.txt", []byte("hidden"))
	if os.Getuid() != 0 {
		if err := os.Chmod(lockedDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o700) })
	}

	// Policy-skipped directory via --skip-prefix.
	mustWrite("e_skipped/ignored.bin", bytes.Repeat([]byte("i"), 32*1024))

	// Symlink pointing outside root: must never be traversed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "external.bin"), bytes.Repeat([]byte("x"), 128*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "f_external_link")); err != nil {
		t.Fatal(err)
	}

	// Tail large installer file that must be reached on resume.
	tailPayload := bytes.Repeat([]byte("z"), 512*1024)
	mustWrite("z_tail/ArchiveInstaller.dmg", tailPayload)

	// Refusal check: state file inside scanned root must be rejected with INVALID_ARGUMENT.
	if _, err := Run(context.Background(), Options{
		Root:       root,
		StateFile:  filepath.Join(root, "inside.json"),
		MaxEntries: 10,
	}); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("expected INVALID_ARGUMENT for state file inside root, got %v", err)
	}

	// Batch 1: small entry budget forces a partial stop before z_tail.
	rep1, err := Run(context.Background(), Options{
		Root:         root,
		StateFile:    stateFile,
		MaxEntries:   3,
		MaxSeconds:   30,
		TopN:         10,
		SkipPrefixes: []string{"e_skipped"},
	})
	if err != nil {
		t.Fatalf("batch 1 failed: %v", err)
	}
	if rep1.Complete {
		t.Fatalf("expected batch 1 to stop partially, got complete=true")
	}
	if rep1.PendingDirs == 0 {
		t.Fatalf("expected pending dirs > 0 after partial batch")
	}
	for _, f := range rep1.TopFiles {
		if f.Path == "z_tail/ArchiveInstaller.dmg" {
			t.Fatalf("tail file should not have been reached in batch 1 (budget=3)")
		}
	}

	// Resume until complete.
	var final Report
	for i := 0; i < 20; i++ {
		rep, err := Run(context.Background(), Options{
			Root:         root,
			StateFile:    stateFile,
			MaxEntries:   4,
			MaxSeconds:   30,
			TopN:         10,
			SkipPrefixes: []string{"e_skipped"},
		})
		if err != nil {
			t.Fatalf("resume batch %d failed: %v", i+2, err)
		}
		final = rep
		if rep.Complete {
			break
		}
	}
	if !final.Complete {
		t.Fatalf("expected inventory to reach complete=true after resuming, still pending=%d", final.PendingDirs)
	}
	if final.HardlinkExtraLinks != 1 {
		t.Fatalf("expected 1 deduplicated hardlink, got %d", final.HardlinkExtraLinks)
	}
	if final.SymlinksSeen != 1 {
		t.Fatalf("expected 1 symlink seen without traversal, got %d", final.SymlinksSeen)
	}
	if final.PolicySkippedCount != 1 {
		t.Fatalf("expected 1 policy-skipped subtree, got %d (%v)", final.PolicySkippedCount, final.PolicySkippedDirs)
	}
	if os.Getuid() != 0 && final.UnreadableCount != 1 {
		t.Fatalf("expected 1 unreadable directory, got %d (%v)", final.UnreadableCount, final.UnreadableDirs)
	}

	// Tail file must be discovered and ranked first by allocated bytes.
	if len(final.TopFiles) == 0 || final.TopFiles[0].Path != "z_tail/ArchiveInstaller.dmg" {
		t.Fatalf("expected z_tail/ArchiveInstaller.dmg as #1 top file, got %+v", final.TopFiles)
	}
	if final.TopFiles[0].Category != "installer_candidate" {
		t.Fatalf("expected installer_candidate category, got %q", final.TopFiles[0].Category)
	}

	// Sparse file logical bytes (16 MiB) must exceed allocated bytes on APFS/ext4/tmpfs.
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if final.TotalLogicalBytes < 16*1024*1024 {
			t.Fatalf("expected total logical bytes >= 16 MiB, got %d", final.TotalLogicalBytes)
		}
		if final.TotalAllocatedBytes >= final.TotalLogicalBytes {
			t.Fatalf("expected sparse file to keep allocated (%d) < logical (%d)",
				final.TotalAllocatedBytes, final.TotalLogicalBytes)
		}
	}

	// Parent/child non-overlap: sum of all top-level buckets (TopN=10 covers all 4 non-empty dirs) equals total allocated bytes.
	var bucketSum int64
	for _, d := range final.TopDirs {
		bucketSum += d.AllocatedBytes
	}
	if bucketSum != final.TotalAllocatedBytes {
		t.Fatalf("top-level bucket allocated sum (%d) != total allocated (%d)", bucketSum, final.TotalAllocatedBytes)
	}
}
