// Package inventory provides bounded, resumable, read-only directory space summaries.
// It never creates a cleanup scope, calls external models, or mutates user files.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/policy"
)

const (
	DefaultMaxEntries = 25000
	MaxAllowedEntries = 500000
	DefaultMaxSeconds = 30
	MaxAllowedSeconds = 300
	DefaultTopN       = 15
	MaxAllowedTopN    = 100
	maxIssueSamples   = 50
	cursorVersion     = 1
)

type Options struct {
	Root         string
	StateFile    string
	Reset        bool
	MaxEntries   int
	MaxSeconds   int
	TopN         int
	SkipPrefixes []string
}

type DirEntrySummary struct {
	Path                 string `json:"path"`
	IncludesChildren     bool   `json:"includes_children"`
	DirectAllocatedBytes int64  `json:"direct_allocated_bytes"`
	AllocatedBytes       int64  `json:"allocated_bytes"`
	LogicalBytes         int64  `json:"logical_bytes"`
	Files                int    `json:"files"`
	Dirs                 int    `json:"dirs"`
}

type FileSummary struct {
	Path           string `json:"path"`
	Category       string `json:"category"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	LogicalBytes   int64  `json:"logical_bytes"`
	NLink          uint64 `json:"nlink"`
	Sparse         bool   `json:"sparse,omitempty"`
	CloudDataless  bool   `json:"cloud_dataless,omitempty"`
}

type CategorySummary struct {
	Category       string `json:"category"`
	Files          int    `json:"files"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	LogicalBytes   int64  `json:"logical_bytes"`
}

type Report struct {
	Root                string            `json:"root"`
	VolumeID            string            `json:"volume_id"`
	FileSystem          string            `json:"filesystem"`
	Device              uint64            `json:"device"`
	RootInode           uint64            `json:"root_inode"`
	VolumeTotalBytes    int64             `json:"volume_total_bytes"`
	VolumeAvailBytes    int64             `json:"volume_avail_bytes"`
	Complete            bool              `json:"complete"`
	StopReason          string            `json:"stop_reason,omitempty"`
	ThisRunEntries      int               `json:"this_run_entries"`
	CumulativeEntries   int               `json:"cumulative_entries"`
	VisitedDirs         int               `json:"visited_dirs"`
	VisitedFiles        int               `json:"visited_files"`
	UniqueInodes        int               `json:"unique_inodes"`
	HardlinkExtraLinks  int               `json:"hardlink_extra_links"`
	SymlinksSeen        int               `json:"symlinks_seen"`
	SparseFiles         int               `json:"sparse_files"`
	CloudDatalessFiles  int               `json:"cloud_dataless_files"`
	PendingDirs         int               `json:"pending_dirs"`
	TotalAllocatedBytes int64             `json:"total_allocated_bytes"`
	TotalLogicalBytes   int64             `json:"total_logical_bytes"`
	TopDirs             []DirEntrySummary `json:"top_dirs"`
	TopFiles            []FileSummary     `json:"top_files"`
	ByCategory          []CategorySummary `json:"by_category"`
	UnreadableDirs      []string          `json:"unreadable_dirs"`
	UnreadableCount     int               `json:"unreadable_count"`
	PolicySkippedDirs   []string          `json:"policy_skipped_dirs"`
	PolicySkippedCount  int               `json:"policy_skipped_count"`
	CrossMountDirs      []string          `json:"cross_mount_dirs"`
	CrossMountCount     int               `json:"cross_mount_count"`
	StateFile           string            `json:"state_file,omitempty"`
	SharedBlocksNote    string            `json:"shared_blocks_note"`
}

type queueItem struct {
	Rel       string `json:"rel"`
	AfterName string `json:"after_name,omitempty"`
}

type cursorState struct {
	Version             int                         `json:"version"`
	Root                string                      `json:"root"`
	VolumeID            string                      `json:"volume_id"`
	FileSystem          string                      `json:"filesystem"`
	Device              uint64                      `json:"device"`
	RootInode           uint64                      `json:"root_inode"`
	Complete            bool                        `json:"complete"`
	StopReason          string                      `json:"stop_reason,omitempty"`
	Queue               []queueItem                 `json:"queue"`
	CumulativeEntries   int                         `json:"cumulative_entries"`
	VisitedDirs         int                         `json:"visited_dirs"`
	VisitedFiles        int                         `json:"visited_files"`
	HardlinkExtraLinks  int                         `json:"hardlink_extra_links"`
	SymlinksSeen        int                         `json:"symlinks_seen"`
	SparseFiles         int                         `json:"sparse_files"`
	CloudDatalessFiles  int                         `json:"cloud_dataless_files"`
	TotalAllocatedBytes int64                       `json:"total_allocated_bytes"`
	TotalLogicalBytes   int64                       `json:"total_logical_bytes"`
	SeenInodes          []uint64                    `json:"seen_inodes"`
	Buckets             map[string]*DirEntrySummary `json:"buckets"`
	Categories          map[string]*CategorySummary `json:"categories"`
	TopFiles            []FileSummary               `json:"top_files"`
	UnreadableDirs      []string                    `json:"unreadable_dirs"`
	UnreadableCount     int                         `json:"unreadable_count"`
	PolicySkippedDirs   []string                    `json:"policy_skipped_dirs"`
	PolicySkippedCount  int                         `json:"policy_skipped_count"`
	CrossMountDirs      []string                    `json:"cross_mount_dirs"`
	CrossMountCount     int                         `json:"cross_mount_count"`
}

func normalizeOptions(opt *Options) error {
	if strings.TrimSpace(opt.Root) == "" {
		return errs.New(errs.CodeInvalidArgument, "inventory --root is required")
	}
	if opt.MaxEntries == 0 {
		opt.MaxEntries = DefaultMaxEntries
	}
	if opt.MaxEntries < 1 || opt.MaxEntries > MaxAllowedEntries {
		return errs.Newf(errs.CodeInvalidArgument, "max-entries must be 1..%d, got %d", MaxAllowedEntries, opt.MaxEntries)
	}
	if opt.MaxSeconds == 0 {
		opt.MaxSeconds = DefaultMaxSeconds
	}
	if opt.MaxSeconds < 1 || opt.MaxSeconds > MaxAllowedSeconds {
		return errs.Newf(errs.CodeInvalidArgument, "max-seconds must be 1..%d, got %d", MaxAllowedSeconds, opt.MaxSeconds)
	}
	if opt.TopN == 0 {
		opt.TopN = DefaultTopN
	}
	if opt.TopN < 1 || opt.TopN > MaxAllowedTopN {
		return errs.Newf(errs.CodeInvalidArgument, "top must be 1..%d, got %d", MaxAllowedTopN, opt.TopN)
	}
	cleanSkips := make([]string, 0, len(opt.SkipPrefixes))
	for _, s := range opt.SkipPrefixes {
		s = strings.TrimSpace(filepath.ToSlash(s))
		s = strings.TrimSuffix(s, "/")
		if s == "" || s == "." || s == ".." || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "../") {
			return errs.Newf(errs.CodeInvalidArgument, "skip-prefix %q must be a relative path inside root", s)
		}
		cleanSkips = append(cleanSkips, s)
	}
	opt.SkipPrefixes = cleanSkips
	return nil
}

func Run(ctx context.Context, opt Options) (Report, error) {
	if err := normalizeOptions(&opt); err != nil {
		return Report{}, err
	}
	absRoot, err := filepath.Abs(opt.Root)
	if err != nil {
		return Report{}, errs.Wrap(errs.CodeInvalidArgument, err)
	}
	// Reject a symlink on the root itself before canonicalising parent components.
	var lst unix.Stat_t
	if err := unix.Lstat(absRoot, &lst); err != nil {
		return Report{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if lst.Mode&unix.S_IFMT == unix.S_IFLNK {
		return Report{}, errs.Newf(errs.CodeScopeRefused, "inventory root %q must not be a symbolic link", absRoot)
	}
	if lst.Mode&unix.S_IFMT != unix.S_IFDIR {
		return Report{}, errs.Newf(errs.CodeInvalidArgument, "inventory root %q is not a directory", absRoot)
	}
	canonRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return Report{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	rootHandle, err := fsops.OpenRoot(canonRoot)
	if err != nil {
		return Report{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer rootHandle.Close()

	rootID, err := rootHandle.Identity()
	if err != nil {
		return Report{}, errs.Wrap(errs.CodeIOError, err)
	}

	var statePath string
	if opt.StateFile != "" {
		sp, err := validateStateFileOutsideRoot(opt.StateFile, canonRoot)
		if err != nil {
			return Report{}, err
		}
		statePath = sp
	}

	cur, seenInodes, err := loadOrInitCursor(statePath, opt.Reset, canonRoot, rootID.Persistent.VolumeID, rootID.Persistent.FileSystem, rootHandle.Dev(), rootID.Persistent.Inode)
	if err != nil {
		return Report{}, err
	}
	if cur.Complete {
		return buildReport(cur, seenInodes, 0, statePath, rootHandle.DirFD(), opt.TopN), nil
	}

	started := time.Now()
	maxDur := time.Duration(opt.MaxSeconds) * time.Second
	thisRun := 0
	stopReason := ""

	for len(cur.Queue) > 0 {
		if ctx.Err() != nil {
			stopReason = "cancelled"
			break
		}
		if thisRun >= opt.MaxEntries {
			stopReason = "entry_limit_reached"
			break
		}
		if thisRun > 0 && time.Since(started) >= maxDur {
			stopReason = "time_limit_reached"
			break
		}

		item := cur.Queue[0]
		if item.Rel != "" && isSkippedPrefix(item.Rel, opt.SkipPrefixes) {
			cur.PolicySkippedCount++
			cur.PolicySkippedDirs = appendSample(cur.PolicySkippedDirs, item.Rel)
			cur.Queue = cur.Queue[1:]
			continue
		}

		var relBytes []byte
		if item.Rel != "" {
			relBytes = []byte(item.Rel)
		}
		dirHandle, err := rootHandle.OpenDir(relBytes)
		if err != nil {
			switch {
			case errors.Is(err, fsops.ErrCrossMount):
				cur.CrossMountCount++
				cur.CrossMountDirs = appendSample(cur.CrossMountDirs, displayRel(item.Rel))
			case errors.Is(err, fsops.ErrSymlink):
				cur.SymlinksSeen++
			case errors.Is(err, fsops.ErrRootChanged):
				return Report{}, errs.New(errs.CodeScopeChanged, "inventory root directory was replaced during scan")
			default:
				cur.UnreadableCount++
				cur.UnreadableDirs = appendSample(cur.UnreadableDirs, displayRel(item.Rel))
			}
			cur.Queue = cur.Queue[1:]
			continue
		}

		entries, rerr := dirHandle.Readdir(-1)
		if rerr != nil {
			dirHandle.Close()
			cur.UnreadableCount++
			cur.UnreadableDirs = appendSample(cur.UnreadableDirs, displayRel(item.Rel))
			cur.Queue = cur.Queue[1:]
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		if item.AfterName == "" {
			cur.VisitedDirs++
			if item.Rel != "" {
				b := ensureBucket(cur.Buckets, topBucketKey(item.Rel))
				b.Dirs++
			}
		}

		pausedMidDir := false
		lastName := item.AfterName
		var discoveredDirs []queueItem

		for _, entry := range entries {
			name := entry.Name()
			if item.AfterName != "" && name <= item.AfterName {
				continue
			}
			if ctx.Err() != nil {
				stopReason = "cancelled"
				pausedMidDir = true
				break
			}
			if thisRun >= opt.MaxEntries {
				stopReason = "entry_limit_reached"
				pausedMidDir = true
				break
			}
			if thisRun > 0 && time.Since(started) >= maxDur {
				stopReason = "time_limit_reached"
				pausedMidDir = true
				break
			}

			thisRun++
			cur.CumulativeEntries++
			lastName = name

			var st unix.Stat_t
			if serr := unix.Fstatat(dirHandle.DirFD(), name, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
				cur.UnreadableCount++
				cur.UnreadableDirs = appendSample(cur.UnreadableDirs, joinRel(item.Rel, name))
				continue
			}

			switch st.Mode & unix.S_IFMT {
			case unix.S_IFLNK:
				cur.SymlinksSeen++
			case unix.S_IFDIR:
				childRel := joinRel(item.Rel, name)
				if uint64(st.Dev) != rootHandle.Dev() {
					cur.CrossMountCount++
					cur.CrossMountDirs = appendSample(cur.CrossMountDirs, childRel)
					continue
				}
				if isSkippedPrefix(childRel, opt.SkipPrefixes) {
					cur.PolicySkippedCount++
					cur.PolicySkippedDirs = appendSample(cur.PolicySkippedDirs, childRel)
					continue
				}
				discoveredDirs = append(discoveredDirs, queueItem{Rel: childRel})
			default:
				cur.VisitedFiles++
				logical := st.Size
				if logical < 0 {
					logical = 0
				}
				var alloc int64
				if st.Blocks > 0 && st.Blocks <= math.MaxInt64/512 {
					alloc = st.Blocks * 512
				}
				dataless := isCloudDataless(&st)
				regular := st.Mode&unix.S_IFMT == unix.S_IFREG
				sparse := regular && !dataless && logical > 0 && alloc < logical

				if uint64(st.Dev) != rootHandle.Dev() {
					continue
				}
				if _, seen := seenInodes[uint64(st.Ino)]; seen {
					cur.HardlinkExtraLinks++
					continue
				}
				seenInodes[uint64(st.Ino)] = struct{}{}

				cur.TotalAllocatedBytes += alloc
				cur.TotalLogicalBytes += logical
				if dataless {
					cur.CloudDatalessFiles++
				}
				if sparse {
					cur.SparseFiles++
				}

				bKey := topBucketKeyForFile(item.Rel)
				b := ensureBucket(cur.Buckets, bKey)
				b.AllocatedBytes += alloc
				b.LogicalBytes += logical
				b.Files++
				if item.Rel == "" || item.Rel == bKey {
					b.DirectAllocatedBytes += alloc
				}

				cls := assess.ClassifyName(name)
				cs := cur.Categories[cls.Category]
				if cs == nil {
					cs = &CategorySummary{Category: cls.Category}
					cur.Categories[cls.Category] = cs
				}
				cs.Files++
				cs.AllocatedBytes += alloc
				cs.LogicalBytes += logical

				cur.TopFiles = insertTopFile(cur.TopFiles, FileSummary{
					Path:           joinRel(item.Rel, name),
					Category:       cls.Category,
					AllocatedBytes: alloc,
					LogicalBytes:   logical,
					NLink:          uint64(st.Nlink),
					Sparse:         sparse,
					CloudDataless:  dataless,
				}, MaxAllowedTopN)
			}
		}
		dirHandle.Close()

		if pausedMidDir {
			cur.Queue[0].AfterName = lastName
			cur.Queue = append(cur.Queue, discoveredDirs...)
			break
		}
		cur.Queue = append(cur.Queue[1:], discoveredDirs...)
	}

	if len(cur.Queue) == 0 && stopReason == "" {
		cur.Complete = true
		cur.StopReason = ""
	} else {
		cur.Complete = false
		cur.StopReason = stopReason
	}

	if statePath != "" {
		if err := saveCursor(statePath, cur, seenInodes); err != nil {
			return Report{}, err
		}
	}
	if stopReason == "cancelled" {
		return buildReport(cur, seenInodes, thisRun, statePath, rootHandle.DirFD(), opt.TopN),
			errs.New(errs.CodeCancelled, "inventory interrupted; cursor saved")
	}
	return buildReport(cur, seenInodes, thisRun, statePath, rootHandle.DirFD(), opt.TopN), nil
}

func validateStateFileOutsideRoot(rawStateFile, canonRoot string) (string, error) {
	absState, err := filepath.Abs(rawStateFile)
	if err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err)
	}
	parentDir := filepath.Dir(absState)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return "", errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	canonParent, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	rootInfo, err := os.Stat(canonRoot)
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	for cur := canonParent; ; cur = filepath.Dir(cur) {
		info, serr := os.Stat(cur)
		if serr == nil && os.SameFile(rootInfo, info) {
			return "", errs.New(errs.CodeInvalidArgument, "inventory state file must be outside the scanned root")
		}
		next := filepath.Dir(cur)
		if next == cur {
			break
		}
	}
	return filepath.Join(canonParent, filepath.Base(absState)), nil
}

func loadOrInitCursor(statePath string, reset bool, root, volID, fsName string, dev, ino uint64) (*cursorState, map[uint64]struct{}, error) {
	fresh := func() (*cursorState, map[uint64]struct{}) {
		return &cursorState{
			Version:           cursorVersion,
			Root:              root,
			VolumeID:          volID,
			FileSystem:        fsName,
			Device:            dev,
			RootInode:         ino,
			Queue:             []queueItem{{Rel: ""}},
			Buckets:           map[string]*DirEntrySummary{},
			Categories:        map[string]*CategorySummary{},
			UnreadableDirs:    []string{},
			PolicySkippedDirs: []string{},
			CrossMountDirs:    []string{},
		}, map[uint64]struct{}{}
	}
	if statePath == "" || reset {
		c, m := fresh()
		return c, m, nil
	}
	raw, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		c, m := fresh()
		return c, m, nil
	}
	if err != nil {
		return nil, nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	var cur cursorState
	if err := json.Unmarshal(raw, &cur); err != nil || cur.Version != cursorVersion {
		return nil, nil, errs.New(errs.CodeStateWriteFailed, "inventory cursor file is unreadable or incompatible; pass --reset")
	}
	if cur.RootInode != ino || (cur.VolumeID != "" && volID != "" && cur.VolumeID != volID) {
		return nil, nil, errs.New(errs.CodeScopeChanged, "inventory root identity changed since cursor was saved; pass --reset to rescan")
	}
	cur.Device = dev
	if cur.Buckets == nil {
		cur.Buckets = map[string]*DirEntrySummary{}
	}
	if cur.Categories == nil {
		cur.Categories = map[string]*CategorySummary{}
	}
	seen := make(map[uint64]struct{}, len(cur.SeenInodes))
	for _, id := range cur.SeenInodes {
		seen[id] = struct{}{}
	}
	return &cur, seen, nil
}

func saveCursor(statePath string, cur *cursorState, seen map[uint64]struct{}) error {
	inodes := make([]uint64, 0, len(seen))
	for id := range seen {
		inodes = append(inodes, id)
	}
	sort.Slice(inodes, func(i, j int) bool { return inodes[i] < inodes[j] })
	cur.SeenInodes = inodes

	body, err := json.Marshal(cur)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	dir := filepath.Dir(statePath)
	tmp, err := os.CreateTemp(dir, ".inventory-cursor-*")
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := os.Rename(tmpName, statePath); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if pfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); err == nil {
		_ = unix.Fsync(pfd)
		_ = unix.Close(pfd)
	}
	return nil
}

func buildReport(cur *cursorState, seen map[uint64]struct{}, thisRun int, statePath string, rootFD int, topN int) Report {
	var volTotal, volAvail int64
	var stfs unix.Statfs_t
	if unix.Fstatfs(rootFD, &stfs) == nil && stfs.Bsize > 0 {
		bsize := uint64(stfs.Bsize)
		if stfs.Blocks <= math.MaxInt64/bsize {
			volTotal = int64(stfs.Blocks * bsize)
		}
		if stfs.Bavail <= math.MaxInt64/bsize {
			volAvail = int64(stfs.Bavail * bsize)
		}
	}

	dirs := make([]DirEntrySummary, 0, len(cur.Buckets))
	for _, b := range cur.Buckets {
		dirs = append(dirs, *b)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].AllocatedBytes != dirs[j].AllocatedBytes {
			return dirs[i].AllocatedBytes > dirs[j].AllocatedBytes
		}
		if dirs[i].LogicalBytes != dirs[j].LogicalBytes {
			return dirs[i].LogicalBytes > dirs[j].LogicalBytes
		}
		return dirs[i].Path < dirs[j].Path
	})
	if len(dirs) > topN {
		dirs = dirs[:topN]
	}

	files := append([]FileSummary(nil), cur.TopFiles...)
	if len(files) > topN {
		files = files[:topN]
	}

	cats := make([]CategorySummary, 0, len(cur.Categories))
	for _, c := range cur.Categories {
		cats = append(cats, *c)
	}
	sort.Slice(cats, func(i, j int) bool {
		if cats[i].AllocatedBytes != cats[j].AllocatedBytes {
			return cats[i].AllocatedBytes > cats[j].AllocatedBytes
		}
		return cats[i].Category < cats[j].Category
	})

	return Report{
		Root:                cur.Root,
		VolumeID:            cur.VolumeID,
		FileSystem:          cur.FileSystem,
		Device:              cur.Device,
		RootInode:           cur.RootInode,
		VolumeTotalBytes:    volTotal,
		VolumeAvailBytes:    volAvail,
		Complete:            cur.Complete,
		StopReason:          cur.StopReason,
		ThisRunEntries:      thisRun,
		CumulativeEntries:   cur.CumulativeEntries,
		VisitedDirs:         cur.VisitedDirs,
		VisitedFiles:        cur.VisitedFiles,
		UniqueInodes:        len(seen),
		HardlinkExtraLinks:  cur.HardlinkExtraLinks,
		SymlinksSeen:        cur.SymlinksSeen,
		SparseFiles:         cur.SparseFiles,
		CloudDatalessFiles:  cur.CloudDatalessFiles,
		PendingDirs:         len(cur.Queue),
		TotalAllocatedBytes: cur.TotalAllocatedBytes,
		TotalLogicalBytes:   cur.TotalLogicalBytes,
		TopDirs:             dirs,
		TopFiles:            files,
		ByCategory:          cats,
		UnreadableDirs:      nonNilStrings(cur.UnreadableDirs),
		UnreadableCount:     cur.UnreadableCount,
		PolicySkippedDirs:   nonNilStrings(cur.PolicySkippedDirs),
		PolicySkippedCount:  cur.PolicySkippedCount,
		CrossMountDirs:      nonNilStrings(cur.CrossMountDirs),
		CrossMountCount:     cur.CrossMountCount,
		StateFile:           statePath,
		SharedBlocksNote:    "read-only metadata inventory; hardlinks on this volume are deduplicated by inode, while APFS clones/snapshots and open unlinked files remain unmeasured",
	}
}

func ensureBucket(m map[string]*DirEntrySummary, key string) *DirEntrySummary {
	if b, ok := m[key]; ok {
		return b
	}
	b := &DirEntrySummary{
		Path:             key,
		IncludesChildren: key != ".",
	}
	m[key] = b
	return b
}

func topBucketKey(relDir string) string {
	if i := strings.IndexByte(relDir, '/'); i >= 0 {
		return relDir[:i]
	}
	return relDir
}

func topBucketKeyForFile(parentRel string) string {
	if parentRel == "" {
		return "."
	}
	return topBucketKey(parentRel)
}

func joinRel(dirRel, name string) string {
	if dirRel == "" {
		return name
	}
	return path.Join(dirRel, name)
}

func displayRel(rel string) string {
	if rel == "" {
		return "."
	}
	return fmt.Sprintf("%s", rel)
}

func isSkippedPrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if policy.HasDirPrefix(rel, p) {
			return true
		}
	}
	return false
}

func appendSample(list []string, val string) []string {
	if len(list) >= maxIssueSamples {
		return list
	}
	return append(list, val)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func insertTopFile(list []FileSummary, item FileSummary, limit int) []FileSummary {
	if len(list) == limit {
		last := list[len(list)-1]
		if item.AllocatedBytes < last.AllocatedBytes ||
			(item.AllocatedBytes == last.AllocatedBytes && item.LogicalBytes <= last.LogicalBytes) {
			return list
		}
	}
	list = append(list, item)
	sort.Slice(list, func(i, j int) bool {
		if list[i].AllocatedBytes != list[j].AllocatedBytes {
			return list[i].AllocatedBytes > list[j].AllocatedBytes
		}
		if list[i].LogicalBytes != list[j].LogicalBytes {
			return list[i].LogicalBytes > list[j].LogicalBytes
		}
		return list[i].Path < list[j].Path
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}
