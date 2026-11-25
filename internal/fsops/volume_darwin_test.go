//go:build darwin

package fsops

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"dustkeeper/internal/domain"
)

var uuidShape = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)

func TestR07DarwinVolumeUUIDIsPersistentAndConsistent(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	rid, err := root.Identity()
	if err != nil {
		t.Fatalf("root identity: %v", err)
	}
	vol := rid.Persistent.VolumeID
	if !domain.HasPersistentVolumeID(vol) || !uuidShape.MatchString(vol) {
		t.Fatalf("expected persistent uppercase UUID on macOS, got %q", vol)
	}

	rootStat, _, err := root.Stat()
	if err != nil {
		t.Fatalf("root stat: %v", err)
	}
	if rootStat.VolumeID != vol {
		t.Fatalf("root stat VolumeID %q != root identity %q", rootStat.VolumeID, vol)
	}

	d, err := root.OpenDir([]byte("sub"))
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer d.Close()

	dirStat, _, err := d.Stat()
	if err != nil {
		t.Fatalf("dir stat: %v", err)
	}
	if dirStat.VolumeID != vol {
		t.Fatalf("dir stat VolumeID %q != root %q", dirStat.VolumeID, vol)
	}

	entryID, _, err := d.StatEntry("a.txt")
	if err != nil {
		t.Fatalf("stat entry: %v", err)
	}
	if entryID.VolumeID != vol {
		t.Fatalf("entry VolumeID %q != root %q", entryID.VolumeID, vol)
	}

	p, err := root.OpenParent([]byte("sub/a.txt"))
	if err != nil {
		t.Fatalf("open parent: %v", err)
	}
	defer p.Close()

	f, err := OpenRegularAt(p, root.Dev())
	if err != nil {
		t.Fatalf("open regular: %v", err)
	}
	defer f.Close()
	if f.Identity.VolumeID != vol {
		t.Fatalf("file VolumeID %q != root %q", f.Identity.VolumeID, vol)
	}
}
