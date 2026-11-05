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
