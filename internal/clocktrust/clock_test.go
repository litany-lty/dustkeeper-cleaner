package clocktrust_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// movableClock is a clock whose two readings can be moved independently.
//
// The fake clock in internal/domain moves the wall clock only, which is right
// for "the machine was off for a month" scenarios. This one also moves the
// monotonic reading, so a test can describe the other case: the wall clock
// changed while the process was running and the monotonic clock did not.
type movableClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newMovableClock(wall time.Time) *movableClock {
	return &movableClock{wall: wall}
}

func (c *movableClock) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *movableClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// jump moves the wall clock without moving the monotonic clock.
func (c *movableClock) jump(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.mu.Unlock()
}

// elapse moves both readings together, the way a real elapsed interval does.
func (c *movableClock) elapse(d time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(d)
	c.mono += d
	c.mu.Unlock()
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestAJumpInsideOneRunIsCaughtByTheDriftCheck covers the case the start verdict
// cannot see: the clock was coherent when the run began and stopped being
// coherent before it ended.
func TestAJumpInsideOneRunIsCaughtByTheDriftCheck(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newMovableClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	guard := clocktrust.Start(st, c)
	if v, err := guard.Assess(ctx); err != nil || !v.Trusted {
		t.Fatalf("a coherent clock was not trusted: %+v %v", v, err)
	}

	// Ten minutes of wall clock with no monotonic time behind it.
	c.jump(10 * time.Minute)
	v, err := guard.Assess(ctx)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if v.Trusted {
		t.Fatal("a ten minute wall clock jump inside one run was treated as trustworthy")
	}
	if v.Reason == "" {
		t.Fatal("the verdict carries no explanation")
	}
}

// TestASuspectClockIsLatchedBeforeTheRefusalIsReturned is the anti-laundering
// rule: the run that notices the anomaly is the only one that knows about it, so
// it must write the anomaly down. An error on its own leaves the next run free
// to start from a clean baseline.
func TestASuspectClockIsLatchedBeforeTheRefusalIsReturned(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newMovableClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	guard := clocktrust.Start(st, c)
	c.jump(time.Hour)

	err := guard.Assert(ctx)
	if errs.CodeOf(err) != errs.CodeClockUntrusted {
		t.Fatalf("expected CLOCK_UNTRUSTED, got %v", err)
	}
	if at, reason, lerr := st.ClockLatch(ctx); lerr != nil || at == 0 {
		t.Fatalf("the refusal was returned without latching the clock: at=%d reason=%q err=%v", at, reason, lerr)
	}

	// A later run that starts from a coherent wall clock must still see the
	// doubt, because the latch is what carries it forward.
	later := clocktrust.Start(st, c)
	v, verr := later.Assess(ctx)
	if verr != nil {
		t.Fatalf("assess after latch: %v", verr)
	}
	if v.Trusted || !v.Latched {
		t.Fatalf("the latch did not survive into the next run: %+v", v)
	}
}

// TestAnUntrustedRunDoesNotMoveTheTrustedBaseline keeps "a run happened" apart
// from "a run proved the clock". Recording the anomalous instant as trusted is
// what let one bad run become the yardstick for every later run.
func TestAnUntrustedRunDoesNotMoveTheTrustedBaseline(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := newMovableClock(start)

	// A first, coherent run fixes the baseline.
	first := clocktrust.Start(st, c)
	if err := first.RecordRun(ctx, clocktrust.Verdict{Trusted: true}); err != nil {
		t.Fatalf("record trusted run: %v", err)
	}
	baseline, err := st.MetaGet(ctx, clocktrust.MetaLastTrustedRun)
	if err != nil || baseline == "" {
		t.Fatalf("no trusted baseline was recorded: %q %v", baseline, err)
	}

	// Sixty days later the clock cannot prove anything. The run happens, so it
	// is recorded, but it must not become the new yardstick.
	c.elapse(60 * 24 * time.Hour)
	second := clocktrust.Start(st, c)
	v, err := second.Assess(ctx)
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if v.Trusted {
		t.Fatal("a sixty day gap between runs was treated as trustworthy")
	}
	if err := second.RecordRun(ctx, v); err != nil {
		t.Fatalf("record untrusted run: %v", err)
	}
	after, err := st.MetaGet(ctx, clocktrust.MetaLastTrustedRun)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if after != baseline {
		t.Fatalf("the untrusted run moved the trusted baseline from %s to %s", baseline, after)
	}
	if at, _, _ := st.ClockLatch(ctx); at == 0 {
		t.Fatal("the untrusted run did not latch the clock")
	}

	// Only an explicit acknowledgement ends the suspension.
	rep, err := clocktrust.Acknowledge(ctx, st, c)
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if rep.AtNS == 0 || rep.PreviousLatch == "" {
		t.Fatalf("the acknowledgement did not report what it cleared: %+v", rep)
	}
	if at, _, _ := st.ClockLatch(ctx); at != 0 {
		t.Fatal("the latch survived an acknowledgement")
	}
	third := clocktrust.Start(st, c)
	if v, err := third.Assess(ctx); err != nil || !v.Trusted {
		t.Fatalf("the clock is still suspended after an acknowledgement: %+v %v", v, err)
	}
}

// TestARunKeepsTheSameBaselineForItsWholeLife pins down that the baseline is
// captured once. Recomputing "the start" later from wall-minus-mono cancels
// itself out and measures nothing.
func TestARunKeepsTheSameBaselineForItsWholeLife(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := newMovableClock(start)

	guard := clocktrust.Start(st, c)
	wall0, mono0 := guard.Baseline()
	c.elapse(30 * time.Second)
	if w, m := guard.Baseline(); w != wall0 || m != mono0 {
		t.Fatalf("the baseline moved during the run: %d/%d became %d/%d", wall0, mono0, w, m)
	}
	if v, err := guard.Assess(ctx); err != nil || !v.Trusted {
		t.Fatalf("a coherent thirty second step was treated as suspicious: %+v %v", v, err)
	}
}
