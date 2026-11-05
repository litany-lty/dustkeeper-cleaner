package assess

import (
	"context"
	"dustkeeper/internal/policy"
	"fmt"
	"golang.org/x/sys/unix"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Options struct {
	MaxEntries, Days                                        int
	ProcessCoverage                                         string
	Now                                                     time.Time
	OpenPaths, ReferencePaths                               map[string]bool
	ProtectedPrefixes, ProtectedPatterns, TemporaryPrefixes []string
}
type FileReport struct {
	Facts      Facts  `json:"facts"`
	Assessment Result `json:"assessment"`
}
type Report struct {
	directories        map[string]directorySnapshot
	Root               string       `json:"root"`
	Complete           bool         `json:"complete"`
	Visited            int          `json:"visited"`
	SkippedDirectories int          `json:"skipped_directories"`
	SkippedPaths       []string     `json:"skipped_paths"`
	Files              []FileReport `json:"files"`
	Issues             []string     `json:"issues"`
	ScopeOfReferences  string       `json:"reference_coverage"`
	ActualFreedBytes   int64        `json:"actual_freed_bytes"`
}

type directorySnapshot struct {
	device, inode uint64
	mtime, ctime  int64
}

func readDirectorySnapshot(path string) (directorySnapshot, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return directorySnapshot{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return directorySnapshot{}, fmt.Errorf("directory replaced")
	}
	return directorySnapshot{uint64(st.Dev), st.Ino, time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UnixNano(), time.Unix(st.Ctim.Sec, st.Ctim.Nsec).UnixNano()}, nil
}

// ReferencesUnchanged verifies the namespace used for bounded link discovery.
// It does not establish absence of references outside the scanned directories.
func (r Report) ReferencesUnchanged() bool {
	for path, before := range r.directories {
		after, err := readDirectorySnapshot(path)
		if err != nil || before != after {
			return false
		}
	}
	return len(r.directories) > 0
}

func Scan(ctx context.Context, root string, opt Options) (Report, error) {
	r := Report{directories: map[string]directorySnapshot{}, Complete: true, Files: []FileReport{}, SkippedPaths: []string{}, Issues: []string{}, ScopeOfReferences: "only explicitly supplied references and symlinks observed inside scan; not proof of no references"}
	if opt.MaxEntries < 1 || opt.MaxEntries > 100000 || opt.Days < 1 || opt.Days > 3650 {
		return r, fmt.Errorf("max entries must be 1..100000 and days 1..3650")
	}
	for _, pattern := range opt.ProtectedPatterns {
		if strings.Contains(pattern, string(filepath.Separator)) {
			return r, fmt.Errorf("protection pattern must match a basename; use protected prefix for subtrees")
		}
		if _, err := filepath.Match(pattern, ""); err != nil {
			return r, fmt.Errorf("invalid protection pattern")
		}
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return r, fmt.Errorf("scan root must be an existing real directory")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return r, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return r, err
	}
	r.Root = root
	if blocked, reason := policy.GlobalProtected(filepath.ToSlash(root) + "/"); blocked {
		return r, fmt.Errorf("scan root is protected: %s", reason)
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	references := map[string]bool{}
	protectedIDs := map[[2]uint64]bool{}
	for _, prefix := range opt.ProtectedPrefixes {
		if !withinPrefix(prefix, prefix) {
			return r, fmt.Errorf("protection prefix must be relative to the scan root")
		}
		var st unix.Stat_t
		if unix.Stat(filepath.Join(root, prefix), &st) == nil {
			protectedIDs[[2]uint64{uint64(st.Dev), st.Ino}] = true
		}
	}
	for p, v := range opt.ReferencePaths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		references[filepath.Clean(p)] = v
	}
	err = filepath.WalkDir(root, func(p string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			r.Complete = false
			r.Issues = append(r.Issues, "cancelled")
			return fs.SkipAll
		}
		if walkErr != nil {
			r.Complete = false
			if len(r.Issues) < 50 {
				rel, _ := filepath.Rel(root, p)
				r.Issues = append(r.Issues, "unreadable:"+rel)
			}
			return nil
		}
		if p == root {
			snapshot, err := readDirectorySnapshot(p)
			if err != nil {
				return err
			}
			r.directories[p] = snapshot
			return nil
		}
		if r.Visited >= opt.MaxEntries {
			r.Complete = false
			r.Issues = append(r.Issues, "entry_limit_reached")
			return fs.SkipAll
		}
		r.Visited++
		rel, _ := filepath.Rel(root, p)
		if entry.IsDir() {
			var st unix.Stat_t
			if unix.Lstat(p, &st) == nil && protectedIDs[[2]uint64{uint64(st.Dev), st.Ino}] {
				r.SkippedDirectories++
				r.SkippedPaths = append(r.SkippedPaths, rel)
				return fs.SkipDir
			}
			for _, prefix := range opt.ProtectedPrefixes {
				if withinPrefix(rel, prefix) {
					r.SkippedDirectories++
					r.SkippedPaths = append(r.SkippedPaths, rel)
					return fs.SkipDir
				}
			}
			name := strings.ToLower(entry.Name())
			if name == ".dustkeeper" || name == ".git" || name == "node_modules" ||
				name == ".ssh" || name == ".gnupg" || name == ".aws" ||
				strings.HasSuffix(name, ".app") || strings.HasSuffix(name, ".framework") ||
				strings.HasSuffix(name, ".bundle") || strings.HasSuffix(name, ".plugin") {
				r.SkippedDirectories++
				r.SkippedPaths = append(r.SkippedPaths, rel)
				return fs.SkipDir
			}
			snapshot, err := readDirectorySnapshot(p)
			if err != nil {
				r.Complete = false
				return fs.SkipDir
			}
			r.directories[p] = snapshot
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			r.Complete = false
			if len(r.Issues) < 50 {
				r.Issues = append(r.Issues, "stat_failed:"+rel)
			}
			return nil
		}
		f := Facts{Path: rel, LogicalBytes: st.Size, Device: uint64(st.Dev), Inode: st.Ino, NLink: uint64(st.Nlink), Regular: st.Mode&unix.S_IFMT == unix.S_IFREG, MTime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec), ATime: time.Unix(st.Atim.Sec, st.Atim.Nsec), OpenByProcess: opt.OpenPaths[p], ProcessVisibility: opt.ProcessCoverage, LastUsedStatus: "not_requested"}
		f.CTimeNS, f.Mode, f.UID, f.GID = time.Unix(st.Ctim.Sec, st.Ctim.Nsec).UnixNano(), uint32(st.Mode), st.Uid, st.Gid
		f.Protected = protectedIDs[[2]uint64{f.Device, f.Inode}]
		if f.ProcessVisibility == "" {
			f.ProcessVisibility = "unknown"
		}
		if st.Blocks >= 0 && st.Blocks <= math.MaxInt64/512 {
			f.AllocatedBytes = st.Blocks * 512
		} else {
			f.AllocatedBytes = -1
			r.Complete = false
			r.Issues = append(r.Issues, "allocation_unknown:"+rel)
		}
		for _, prefix := range opt.ProtectedPrefixes {
			if withinPrefix(rel, prefix) {
				f.Protected = true
			}
		}
		for _, pattern := range opt.ProtectedPatterns {
			if yes, _ := filepath.Match(pattern, filepath.Base(rel)); yes {
				f.Protected = true
			}
		}
		for _, prefix := range opt.TemporaryPrefixes {
			if withinPrefix(rel, prefix) {
				f.TemporaryOrigin = "operator_declared_temporary_directory"
			}
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if target, er := os.Readlink(p); er == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(p), target)
				}
				references[filepath.Clean(target)] = true
			}
		}
		r.Files = append(r.Files, FileReport{Facts: f})
		return nil
	})
	if err != nil {
		return r, err
	}
	// References bind to filesystem objects as well as lexical names. A symlink
	// or hardlink spelling supplied by the operator is still a known reference.
	resolvedReferences := map[string]bool{}
	referencedIDs := map[[2]uint64]bool{}
	for ref, known := range references {
		if !known {
			continue
		}
		resolvedReferences[ref] = true
		if resolved, err := filepath.EvalSymlinks(ref); err == nil {
			resolvedReferences[resolved] = true
		}
		var st unix.Stat_t
		if unix.Stat(ref, &st) == nil {
			referencedIDs[[2]uint64{uint64(st.Dev), st.Ino}] = true
		}
	}
	references = resolvedReferences
	openDirectories := []string{}
	for path, open := range opt.OpenPaths {
		if open {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				if resolved, err := filepath.EvalSymlinks(path); err == nil {
					openDirectories = append(openDirectories, resolved)
				}
			}
		}
	}
	for i := range r.Files {
		f := &r.Files[i].Facts
		for _, dir := range openDirectories {
			if strings.HasPrefix(filepath.Join(root, f.Path), dir+string(filepath.Separator)) {
				f.OpenDirectoryContext = true
				break
			}
		}
		f.KnownReference = references[filepath.Join(root, f.Path)] || referencedIDs[[2]uint64{f.Device, f.Inode}]
		for ref, known := range references {
			if known && strings.HasPrefix(filepath.Join(root, f.Path), ref+string(filepath.Separator)) {
				f.KnownReference = true
				break
			}
		}
		r.Files[i].Assessment = Assess(*f, opt.Days, opt.Now)
	}
	return r, nil
}
func withinPrefix(rel, prefix string) bool {
	if prefix == "." {
		return true
	}
	prefix = strings.TrimSuffix(filepath.Clean(prefix), string(filepath.Separator))
	if prefix == "." || prefix == "" || filepath.IsAbs(prefix) || prefix == ".." || strings.HasPrefix(prefix, ".."+string(filepath.Separator)) {
		return false
	}
	return rel == prefix || strings.HasPrefix(rel, prefix+string(filepath.Separator))
}
