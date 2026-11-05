// cleanup-report only reads file metadata. It never opens Dustkeeper's database,
// calls a model, creates a deletion plan, or moves/removes files.
package main

import (
	"context"
	"dustkeeper/internal/assess"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type listFlag []string

func (f *listFlag) String() string { return strings.Join(*f, ",") }
func (f *listFlag) Set(v string) error {
	if v == "" || filepath.IsAbs(v) || filepath.Clean(v) == ".." || strings.HasPrefix(filepath.Clean(v), ".."+string(filepath.Separator)) {
		return fmt.Errorf("relative scoped value required")
	}
	*f = append(*f, v)
	return nil
}
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func run(args []string, out, errout io.Writer) int {
	flags := flag.NewFlagSet("cleanup-report", flag.ContinueOnError)
	flags.SetOutput(errout)
	root := flags.String("root", "", "explicit read-only scope")
	max := flags.Int("max-entries", 2000, "maximum directory entries; limit produces incomplete report")
	days := flags.Int("days", 30, "recent activity window; absence is not proof of nonuse")
	open := flags.Bool("open-files", false, "inspect visible processes once; incomplete visibility remains unknown")
	spotlight := flags.Int("spotlight-limit", 0, "Mac last-used metadata probes, 0..30")
	output := flags.String("output", "", "new private JSON file; omitted writes stdout")
	var protectPrefixes, protectPatterns, tempPrefixes, references listFlag
	flags.Var(&references, "reference", "known referenced relative file/directory; repeatable")
	flags.Var(&protectPrefixes, "protect-prefix", "relative protected subtree, repeatable")
	flags.Var(&protectPatterns, "protect-pattern", "protected basename pattern, repeatable")
	flags.Var(&tempPrefixes, "temporary-prefix", "explicitly declared temporary origin, not deletion permission")
	if flags.Parse(args) != nil {
		return 2
	}
	if *root == "" || flags.NArg() != 0 || *spotlight < 0 || *spotlight > 30 {
		fmt.Fprintln(errout, "root required; spotlight-limit must be 0..30")
		return 2
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return 2
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		fmt.Fprintln(errout, "root unavailable")
		return 2
	}
	if *output != "" {
		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(*output))
		if err != nil {
			fmt.Fprintln(errout, "output parent is unavailable")
			return 2
		}
		resolvedParent, err = filepath.Abs(resolvedParent)
		if err != nil {
			return 2
		}
		rootInfo, err := os.Stat(canonical)
		if err != nil {
			return 2
		}
		for parent := resolvedParent; ; parent = filepath.Dir(parent) {
			info, err := os.Stat(parent)
			if err != nil {
				return 2
			}
			if os.SameFile(rootInfo, info) {
				fmt.Fprintln(errout, "report output must be outside the scanned scope")
				return 2
			}
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	referencePaths := map[string]bool{}
	for _, ref := range references {
		referencePaths[filepath.Join(canonical, ref)] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	now := time.Now()
	process := assess.OpenObservation{Coverage: "not_requested", Paths: map[string]bool{}}
	if *open {
		process = assess.ProbeOpenFiles(ctx, canonical, nil)
	}
	rep, err := assess.Scan(ctx, abs, assess.Options{MaxEntries: *max, Days: *days, Now: now, ReferencePaths: referencePaths, OpenPaths: process.Paths, ProcessCoverage: process.Coverage, ProtectedPrefixes: protectPrefixes, ProtectedPatterns: protectPatterns, TemporaryPrefixes: tempPrefixes})
	if err != nil {
		fmt.Fprintln(errout, "scan refused:", err)
		return 2
	}
	sort.SliceStable(rep.Files, func(i, j int) bool {
		return rep.Files[i].Assessment.PurgeReclaimUpperBound > rep.Files[j].Assessment.PurgeReclaimUpperBound
	})
	probes := 0
	unknownProbes := 0
	if runtime.GOOS == "darwin" {
		for i := range rep.Files {
			if probes >= *spotlight || ctx.Err() != nil {
				break
			}
			f := &rep.Files[i]
			if f.Assessment.Disposition == "keep" {
				continue
			}
			p := assess.ProbeLastUsed(ctx, filepath.Join(rep.Root, f.Facts.Path), nil)
			probes++
			if p.Status != "observed" {
				unknownProbes++
			}
			f.Facts.LastUsed = p.At
			f.Facts.LastUsedStatus = p.Status
			if p.Status == "observed" {
				f.Facts.LastUsedSource = p.Source
			}
			f.Assessment = assess.Assess(f.Facts, *days, now)
		}
	}
	if ctx.Err() != nil {
		rep.Complete = false
		rep.Issues = append(rep.Issues, "time_limit_reached")
	}
	sort.SliceStable(rep.Files, func(i, j int) bool {
		return rep.Files[i].Assessment.PurgeReclaimUpperBound > rep.Files[j].Assessment.PurgeReclaimUpperBound
	})
	counts := map[string]int{"keep": 0, "needs_review": 0}
	var upper int64
	seen := map[string]bool{}
	for _, f := range rep.Files {
		counts[f.Assessment.Disposition]++
		id := fmt.Sprintf("%d:%d", f.Facts.Device, f.Facts.Inode)
		if !seen[id] {
			upper += f.Assessment.PurgeReclaimUpperBound
			seen[id] = true
		}
	}
	report := struct {
		assess.Report
		DeleteAuthorized              bool                   `json:"delete_authorized"`
		Counts                        map[string]int         `json:"counts"`
		ProcessObservation            assess.OpenObservation `json:"process_observation"`
		SpotlightProbes               int                    `json:"spotlight_probes"`
		UnknownSpotlightProbes        int                    `json:"unknown_spotlight_probes"`
		CandidateAllocationUpperBound int64                  `json:"candidate_allocation_upper_bound"`
		MinimumReclaimGuaranteed      int64                  `json:"minimum_reclaim_guaranteed"`
		Notice                        string                 `json:"notice"`
	}{Report: rep, Counts: counts, ProcessObservation: process, SpotlightProbes: probes, UnknownSpotlightProbes: unknownProbes, CandidateAllocationUpperBound: upper, Notice: "No deletion permission. Allocated bytes are an upper bound, not a promise; snapshots/clones may retain blocks. Missing usage/references remain unknown."}
	encode := func(w io.Writer) error { enc := json.NewEncoder(w); enc.SetIndent("", "  "); return enc.Encode(report) }
	if *output != "" {
		if err := writeReportFile(*output, encode); err != nil {
			fmt.Fprintln(errout, "report output could not be written")
			return 2
		}
	} else if encode(out) != nil {
		return 2
	}
	if !rep.Complete {
		return 9
	}
	return 0
}
