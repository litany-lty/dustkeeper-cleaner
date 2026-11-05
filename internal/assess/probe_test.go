package assess

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestRound6UnavailableUsageAndProcessProbeRemainUnknown(t *testing.T) {
	bad := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("denied") }
	p := ProbeOpenFiles(context.Background(), "/scoped", bad)
	if p.Coverage == "complete" || len(p.Paths) != 0 {
		t.Fatal("failed process listing claimed complete")
	}
	u := ProbeLastUsed(context.Background(), "/scoped/a", func(context.Context, string, ...string) ([]byte, error) { return []byte("(null)"), nil })
	if !u.At.IsZero() || u.Status != "unknown" {
		t.Fatal("missing last-use became unused")
	}
	u = ProbeLastUsed(context.Background(), "/scoped/a", func(context.Context, string, ...string) ([]byte, error) {
		return []byte("2026-10-30 12:00:00 +0000"), nil
	})
	if u.At.IsZero() {
		t.Fatal("known timestamp lost")
	}
	r := Assess(Facts{Path: "a.tmp", Regular: true, NLink: 1, LastUsed: u.At}, 30, time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC))
	if r.Disposition != "keep" {
		t.Fatal("known recent use ignored")
	}
}

// TestOneTreeProbeSeparatesAFinishedRunFromABrokenOne pins the statement the
// per-file decision depends on.
//
// The exit status of lsof is not that statement. The macOS build returns 1 both
// when it lists nothing and when it lists files, so a caller that read 1 as "no
// match" would call an open file unused, and a caller that read it as an error
// would call every tree unknown - including every tree this program is inside,
// because it holds its own vault directory open while it works.
func TestOneTreeProbeSeparatesAFinishedRunFromABrokenOne(t *testing.T) {
	root := "/scoped"
	listed := []byte("p42\x00f3\x00n/scoped/a.zip\x00")
	exitOne := &exec.ExitError{ProcessState: exitedWith(1)}

	obs := ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) { return listed, exitOne })
	if obs.Coverage != "visible_processes_only" || !obs.Paths[root+"/a.zip"] {
		t.Fatalf("a finished listing was not trusted: %+v", obs)
	}

	obs = ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) { return nil, exitOne })
	if obs.Coverage != "visible_processes_only" || len(obs.Paths) != 0 {
		t.Fatalf("an empty finished listing was not trusted: %+v", obs)
	}

	// Truncated output is not a complete answer: the file could be in the part
	// that was cut off.
	obs = ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) {
			return listed, errors.New("command output limit reached")
		})
	if obs.Coverage != "partial_visible_processes" {
		t.Fatalf("truncated output claimed to be complete: %+v", obs)
	}

	obs = ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, context.DeadlineExceeded
		})
	if obs.Coverage != "unknown" {
		t.Fatalf("a timeout was reported as an answer: %+v", obs)
	}
}

// TestAProcessListingThatDidNotFinishIsNotAnEmptyAnswer pins the other half of
// the same statement: a failed run said nothing, and nothing is not "unused".
//
// An *exec.ExitError is not proof that the command ran. It is also what a
// crashed helper, a refused permission and a tool that could not look at all
// return, and treating those as "the listing is complete and the file was not
// in it" turns a failure into the positive fact a move is authorised on.
func TestAProcessListingThatDidNotFinishIsNotAnEmptyAnswer(t *testing.T) {
	root := "/scoped"
	listed := []byte("p42\x00f3\x00n/scoped/a.zip\x00")

	obs := ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, &exec.ExitError{ProcessState: exitedWith(2)}
		})
	if obs.Coverage == "visible_processes_only" {
		t.Fatalf("a failed listing became a complete empty answer: %+v", obs)
	}

	// What the broken run did see is still knowledge: the file was listed open,
	// and a later failure does not unsee it.
	obs = ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) {
			return listed, &exec.ExitError{ProcessState: exitedWith(2)}
		})
	if obs.Coverage != "partial_visible_processes" || !obs.Paths[root+"/a.zip"] {
		t.Fatalf("a match seen before the failure was dropped: %+v", obs)
	}

	// The positive case, unchanged: a run that finished with nothing to list is
	// a complete answer, and the fact built on it stays available.
	obs = ProbeOpenUnder(context.Background(), root,
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, &exec.ExitError{ProcessState: exitedWith(1)}
		})
	if obs.Coverage != "visible_processes_only" || len(obs.Paths) != 0 {
		t.Fatalf("an empty finished listing stopped being trusted: %+v", obs)
	}
}

// TestAProbeKilledByItsOwnDeadlineIsNotAnAnswer uses the real command runner and
// a real deadline, not a hand-made error value: the shape of "the process was
// killed" is the thing under test, and only the operating system produces it.
func TestAProbeKilledByItsOwnDeadlineIsNotAnAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	obs := ProbeOpenUnder(ctx, t.TempDir(), func(c context.Context, _ string, _ ...string) ([]byte, error) {
		return RunBoundedCommand(c, "/bin/sleep", "1")
	})
	if ctx.Err() == nil {
		t.Fatal("fixture did not reach its deadline")
	}
	if obs.Coverage == "visible_processes_only" {
		t.Fatalf("a killed listing became a complete empty answer: %+v ctx=%v", obs, ctx.Err())
	}
}

// exitedWith builds a process state that exited with the given status.
func exitedWith(code int) *os.ProcessState {
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	_ = cmd.Run()
	return cmd.ProcessState
}
