package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/inventory"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// cmdInventory runs a bounded, resumable, read-only directory space summary.
// It never opens the state database for writing, creates no cleanup scope, and
// calls no external model.
func cmdInventory(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("inventory", io.Discard)
	root := fs.String("root", "", "directory to inventory (same volume, no symlinks followed)")
	stateFile := fs.String("state-file", "", "cursor file for resumable scan; empty runs in memory, 'auto' uses state_dir/inventory/")
	maxEntries := fs.Int("max-entries", inventory.DefaultMaxEntries, "maximum directory entries visited in this invocation")
	maxSeconds := fs.Int("max-seconds", inventory.DefaultMaxSeconds, "soft wall-clock time slice in seconds")
	topN := fs.Int("top", inventory.DefaultTopN, "number of top non-overlapping subtrees and top files to list")
	reset := fs.Bool("reset", false, "discard any existing cursor file and start from the root")
	var skipPrefixes stringSliceFlag
	fs.Var(&skipPrefixes, "skip-prefix", "relative subtree inside root to skip by policy (repeatable)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *root == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "inventory requires --root <dir>")
	}
	absRoot, err := absPath(*root)
	if err != nil {
		return output{}, err
	}
	resolvedState := *stateFile
	if resolvedState == "auto" {
		sum := sha256.Sum256([]byte(absRoot))
		resolvedState = filepath.Join(a.paths.StateDir, "inventory", "cursor-"+hex.EncodeToString(sum[:8])+".json")
	}

	rep, err := inventory.Run(ctx, inventory.Options{
		Root:         absRoot,
		StateFile:    resolvedState,
		Reset:        *reset,
		MaxEntries:   *maxEntries,
		MaxSeconds:   *maxSeconds,
		TopN:         *topN,
		SkipPrefixes: skipPrefixes,
	})
	if err != nil {
		return output{}, err
	}

	res := ok()
	res.Data = rep
	res.Human = renderInventoryHuman(rep)
	if !rep.Complete {
		res.Code = errs.CodePartial
		res.Status = string(errs.StatusPartial)
		res.Message = fmt.Sprintf("inventory paused (%s) with %d directories still queued", rep.StopReason, rep.PendingDirs)
		nextCmd := "dustkeeper inventory --root " + shellQuote(rep.Root)
		if rep.StateFile != "" {
			nextCmd += " --state-file " + shellQuote(rep.StateFile)
		} else {
			nextCmd += " --state-file auto"
		}
		res.Next = []string{nextCmd}
	}
	return res, nil
}

func renderInventoryHuman(rep inventory.Report) []string {
	status := "complete"
	if !rep.Complete {
		status = fmt.Sprintf("partial (%s, %d pending dirs)", rep.StopReason, rep.PendingDirs)
	}
	lines := []string{
		fmt.Sprintf("inventory: %s [%s]", rep.Root, status),
		fmt.Sprintf("  volume:    %s (%s, avail %s / total %s)",
			rep.VolumeID, rep.FileSystem, humanBytesOf(rep.VolumeAvailBytes), humanBytesOf(rep.VolumeTotalBytes)),
		fmt.Sprintf("  allocated: %s (logical %s) across %d files (%d unique inodes, %d extra hardlinks, %d sparse, %d cloud-dataless)",
			humanBytesOf(rep.TotalAllocatedBytes), humanBytesOf(rep.TotalLogicalBytes),
			rep.VisitedFiles, rep.UniqueInodes, rep.HardlinkExtraLinks, rep.SparseFiles, rep.CloudDatalessFiles),
		fmt.Sprintf("  coverage:  +%d entries this run (%d cumulative), %d dirs visited, %d unreadable, %d policy-skipped, %d cross-mount, %d symlinks",
			rep.ThisRunEntries, rep.CumulativeEntries, rep.VisitedDirs,
			rep.UnreadableCount, rep.PolicySkippedCount, rep.CrossMountCount, rep.SymlinksSeen),
	}
	if len(rep.TopDirs) > 0 {
		lines = append(lines, "", "top non-overlapping subtrees:")
		for _, d := range rep.TopDirs {
			scopeTag := "direct files"
			if d.IncludesChildren {
				scopeTag = "includes children"
			}
			lines = append(lines, fmt.Sprintf("  %10s  %-36s  (%d files, %s)",
				humanBytesOf(d.AllocatedBytes), d.Path, d.Files, scopeTag))
		}
	}
	if len(rep.TopFiles) > 0 {
		lines = append(lines, "", "top files by allocated blocks:")
		for _, f := range rep.TopFiles {
			lines = append(lines, fmt.Sprintf("  %10s  (logical %s)  [%s]  %s",
				humanBytesOf(f.AllocatedBytes), humanBytesOf(f.LogicalBytes), f.Category, f.Path))
		}
	}
	return lines
}
