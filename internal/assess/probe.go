package assess

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type CommandRunner func(context.Context, string, ...string) ([]byte, error)
type OpenObservation struct {
	Paths          map[string]bool `json:"-"`
	Coverage       string          `json:"coverage"`
	VisibleMatches int             `json:"visible_matches"`
}
type UsageObservation struct {
	At     time.Time `json:"at,omitempty"`
	Status string    `json:"status"`
	Source string    `json:"source"`
}

func ProbeOpenFiles(ctx context.Context, root string, run CommandRunner) OpenObservation {
	out := OpenObservation{Paths: map[string]bool{}, Coverage: "unknown"}
	if run == nil {
		run = RunBoundedCommand
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := run(c, "lsof", "-nP", "-F0n")
	for _, field := range bytes.Split(data, []byte{0}) {
		field = bytes.TrimLeft(field, "\n")
		if len(field) < 2 || field[0] != 'n' {
			continue
		}
		p := string(field[1:])
		rel, relErr := filepath.Rel(root, p)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out.Paths[filepath.Clean(p)] = true
		}
	}
	out.VisibleMatches = len(out.Paths)
	if err == nil {
		out.Coverage = "visible_processes_only"
	} else if len(out.Paths) > 0 {
		out.Coverage = "partial_visible_processes"
	}
	return out
}

// ProbeOpenUnder reports which files inside one tree a visible process holds
// open.
//
// It is separate from ProbeOpenFiles because the two answer different
// questions. ProbeOpenFiles lists every open file on the machine and keeps the
// ones under a root; on a busy host that listing is tens of megabytes and the
// bounded reader truncates it, so a file that is open can simply be missing
// from the answer. ProbeOpenUnder asks lsof to walk one tree instead, which is
// what a per-file decision needs: the answer is either "this tree" or nothing.
//
// Coverage is the statement a caller may rely on:
//
//	visible_processes_only  lsof finished; its listing is the whole answer
//	partial_visible_processes  the run broke off after listing some files
//	unknown                 nothing usable was learned
//
// Coverage is decided by whether the listing actually ran to the end. A command
// that was canceled, killed by a signal, cut off by the output limit or never
// started produced no answer, and its silence is not "nothing is open": the
// facts built on that silence would authorise a move on evidence nobody
// collected. lsof's own documented statuses are 0 (files were listed) and 1
// (none matched); the macOS build in use returns 1 even when it lists files, so
// both count as finished. Every other status is a broken run.
//
// Anything a broken run did print is kept as a partial answer: seeing that a
// file is open is positive knowledge, and it must survive the failure that came
// after it.
func ProbeOpenUnder(ctx context.Context, root string, run CommandRunner) OpenObservation {
	out := OpenObservation{Paths: map[string]bool{}, Coverage: "unknown"}
	if run == nil {
		run = RunBoundedCommand
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := run(c, processListingPath(), "-nP", "-w", "-F0n", "+D", root)
	for _, field := range bytes.Split(data, []byte{0}) {
		field = bytes.TrimLeft(field, "\n")
		if len(field) < 2 || field[0] != 'n' {
			continue
		}
		p := string(field[1:])
		rel, relErr := filepath.Rel(root, p)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out.Paths[filepath.Clean(p)] = true
		}
	}
	out.VisibleMatches = len(out.Paths)
	switch {
	case c.Err() == nil && listingCompleted(err):
		out.Coverage = "visible_processes_only"
	case len(out.Paths) > 0:
		out.Coverage = "partial_visible_processes"
	}
	return out
}

// listingCompleted reports whether the listing command ran to the end, which is
// the only thing that makes its output the whole of what it had to say.
//
// Being an *exec.ExitError is not that question. The type also covers a command
// killed by a signal, a helper that failed at its own work, and a status the
// tool uses for "I could not look" - none of which said anything about which
// files are open. lsof documents 0 as "files listed" and 1 as "none matched",
// and the macOS build returns 1 while listing files, so those two are accepted
// and nothing else is.
func listingCompleted(err error) bool {
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		// A truncated read (the bounded writer refused more output), a missing
		// binary, or a canceled context: no answer was produced.
		return false
	}
	if !ee.Exited() {
		// Terminated by a signal - the timeout we imposed, or somebody else's
		// kill. The listing stopped where it stopped.
		return false
	}
	switch ee.ExitCode() {
	case 0, 1:
		return true
	}
	return false
}

// processListingCandidates is where the process listing lives on the systems
// this build targets.
//
// The tool is looked for at these paths before PATH is consulted, because the
// listing is the fact a move depends on and a scheduled run is not a shell: a
// launchd job and a minimal container both start with a PATH that may not
// include sbin. Falling back to PATH keeps the behaviour on a system that
// installs the tool somewhere else.
var processListingCandidates = []string{
	"/usr/sbin/lsof", "/usr/bin/lsof", "/sbin/lsof", "/bin/lsof",
}

var (
	processListingOnce sync.Once
	processListingBin  string
)

func processListingPath() string {
	processListingOnce.Do(func() {
		for _, p := range processListingCandidates {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				processListingBin = p
				return
			}
		}
		processListingBin = "lsof"
	})
	return processListingBin
}

func ProbeLastUsed(ctx context.Context, path string, run CommandRunner) UsageObservation {
	out := UsageObservation{Status: "unknown"}
	if run == nil {
		run = RunBoundedCommand
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := run(c, "mdls", "-raw", "-name", "kMDItemLastUsedDate", path)
	if err != nil {
		return out
	}
	stamp := strings.TrimSpace(string(data))
	at, err := time.Parse("2006-01-02 15:04:05 -0700", stamp)
	if err != nil {
		return out
	}
	out.At = at
	out.Status = "observed"
	out.Source = "spotlight_last_used"
	return out
}

type boundedOutput struct {
	bytes.Buffer
	max int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > w.max {
		left := w.max - w.Len()
		if left > 0 {
			w.Buffer.Write(p[:left])
		}
		return left, errors.New("command output limit reached")
	}
	return w.Buffer.Write(p)
}
func RunBoundedCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Bound waiting for inherited pipes as well as the direct child. A helper's
	// descendant may keep stdout open after CommandContext kills the helper.
	cmd.WaitDelay = 50 * time.Millisecond
	out := &boundedOutput{max: 256 << 10}
	cmd.Stdout = out
	err := cmd.Run()
	return out.Bytes(), err
}
