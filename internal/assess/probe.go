package assess

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
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
// The exit status is deliberately not part of that decision. lsof's own
// documentation makes 0 mean "files were listed" and 1 mean "none were", but
// the macOS build in use returns 1 even when it lists files, so treating 1 as
// "nothing is open" would answer "not in use" for a file that was just printed,
// and treating it as a failure would answer "unknown" for every tree - which,
// since the program itself holds the vault directory open, would be every tree
// it ever asks about. What separates a finished run from a broken one is the
// error's kind, not its number: a process that ran and exited produced an
// answer, while a timeout, a truncated read or a missing binary did not.
func ProbeOpenUnder(ctx context.Context, root string, run CommandRunner) OpenObservation {
	out := OpenObservation{Paths: map[string]bool{}, Coverage: "unknown"}
	if run == nil {
		run = RunBoundedCommand
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := run(c, "lsof", "-nP", "-w", "-F0n", "+D", root)
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
	case err == nil, exited(err):
		out.Coverage = "visible_processes_only"
	case len(out.Paths) > 0:
		out.Coverage = "partial_visible_processes"
	}
	return out
}

// exited reports whether err is a process exit status, which means the command
// ran to completion and its output is the whole of what it had to say.
func exited(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
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
