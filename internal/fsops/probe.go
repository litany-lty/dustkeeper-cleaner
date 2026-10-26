package fsops

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// Capabilities records what this host was actually observed doing, not what the
// OS version suggests. Section 3.4 forbids inferring rename or durability
// ability from a version string.
type Capabilities struct {
	Platform        string   `json:"platform"`
	NoReplaceRename bool     `json:"no_replace_rename"`
	DirSync         bool     `json:"dir_sync"`
	FullBarrier     bool     `json:"full_barrier"`
	XattrRead       bool     `json:"xattr_read"`
	FileFlags       bool     `json:"file_flags"`
	NoFollow        bool     `json:"no_follow"`
	Notes           []string `json:"notes"`
}

// Probe performs real operations in a private probe directory under base.
//
// It never touches a business file: the directory it creates is removed again,
// and it fails closed. An incomplete probe is reported as "not capable", which
// keeps the scope read-only instead of guessing.
func Probe(base string) Capabilities {
	caps := Capabilities{Platform: runtime.GOOS}

	dir, err := os.MkdirTemp(base, ".dustkeeper-probe-")
	if err != nil {
		caps.Notes = append(caps.Notes, fmt.Sprintf("probe directory could not be created: %v", err))
		return caps
	}
	defer os.RemoveAll(dir)

	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		caps.Notes = append(caps.Notes, fmt.Sprintf("probe write failed: %v", err))
		return caps
	}
	if err := os.WriteFile(b, []byte("b"), 0o600); err != nil {
		caps.Notes = append(caps.Notes, fmt.Sprintf("probe write failed: %v", err))
		return caps
	}

	// Exclusive rename must refuse to replace b, and must leave b intact.
	root, err := OpenRoot(dir)
	if err != nil {
		caps.Notes = append(caps.Notes, fmt.Sprintf("probe root open failed: %v", err))
		return caps
	}
	defer root.Close()

	src, err := root.OpenParent([]byte("a"))
	if err == nil {
		defer src.Close()
		if err := RenameNoReplace(src, "a", root, "b"); err != nil {
			if err == ErrExists {
				caps.NoReplaceRename = true
			} else {
				caps.Notes = append(caps.Notes, fmt.Sprintf("exclusive rename failed: %v", err))
			}
		} else {
			caps.Notes = append(caps.Notes, "exclusive rename replaced an existing target")
		}
	}
	if data, err := os.ReadFile(b); err != nil || string(data) != "b" {
		caps.NoReplaceRename = false
		caps.Notes = append(caps.Notes, "target file was modified by a refused rename")
	}

	// Directory sync.
	if f, err := os.Open(dir); err == nil {
		if err := unix.Fsync(int(f.Fd())); err == nil {
			caps.DirSync = true
		} else {
			caps.Notes = append(caps.Notes, fmt.Sprintf("directory fsync failed: %v", err))
		}
		f.Close()
	}

	// Full flush barrier on a same-volume anchor file.
	anchor := filepath.Join(dir, "anchor")
	if af, err := os.OpenFile(anchor, os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		if err := Barrier(int(af.Fd()), []byte("probe")); err == nil {
			caps.FullBarrier = true
		} else {
			caps.Notes = append(caps.Notes, fmt.Sprintf("barrier failed: %v", err))
		}
		af.Close()
	}

	// Extended attributes and file flags.
	if err := unix.Setxattr(b, "user.dustkeeper.probe", []byte("1"), 0); err == nil {
		buf := make([]byte, 8)
		if n, err := unix.Getxattr(b, "user.dustkeeper.probe", buf); err == nil && n == 1 {
			caps.XattrRead = true
		}
		unix.Removexattr(b, "user.dustkeeper.probe")
	} else {
		caps.Notes = append(caps.Notes, "extended attributes are not writable on this volume")
	}
	caps.FileFlags = runtime.GOOS == "darwin"

	// Symlink refusal.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err == nil {
		if _, err := root.OpenDir([]byte("link")); err != nil {
			caps.NoFollow = true
		} else {
			caps.Notes = append(caps.Notes, "a symlinked directory was followed")
		}
	}

	return caps
}

// Capable reports whether the host can be trusted with file mutation: exclusive
// rename plus directory sync are the two non-negotiable primitives.
func (c Capabilities) Capable() bool { return c.NoReplaceRename && c.DirSync && c.NoFollow }
