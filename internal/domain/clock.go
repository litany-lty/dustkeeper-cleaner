package domain

import (
	"sync"
	"time"
)

// Clock supplies business time. File age and retention are computed from Wall;
// in-run budget is measured from Mono, which never jumps.
//
// Splitting the two is what makes the section 11.2 checks possible: a wall clock
// that moved 48 hours while Mono advanced two seconds is a clock problem, not a
// reason to empty the vault.
type Clock interface {
	Wall() time.Time
	Mono() time.Duration
}

// SystemClock is the production clock.
type SystemClock struct {
	start time.Time
}

// NewSystemClock returns a clock whose Mono starts at zero now.
func NewSystemClock() *SystemClock {
	return &SystemClock{start: time.Now()}
}

// Wall returns the current wall clock reading.
func (c *SystemClock) Wall() time.Time { return time.Now() }

// Mono returns monotonic elapsed time since the clock was created.
func (c *SystemClock) Mono() time.Duration { return time.Since(c.start) }

// FakeClock is a test clock. Advancing it changes Wall only, so a test can jump
// days forward without giving the run a larger I/O budget or a longer timeout.
type FakeClock struct {
	mu      sync.Mutex
	base    time.Time
	offset  time.Duration
	started time.Time
}

// NewFakeClock returns a test clock pinned to wall.
func NewFakeClock(wall time.Time) *FakeClock {
	return &FakeClock{base: wall, started: time.Now()}
}

// Wall returns base plus accumulated offset.
func (c *FakeClock) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base.Add(c.offset)
}

// Advance moves the fake wall clock forward (or backward, with a negative
// duration). Mono is unaffected.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// SetWall jumps the fake wall clock to an absolute instant.
func (c *FakeClock) SetWall(t time.Time) {
	c.mu.Lock()
	c.offset = t.Sub(c.base)
	c.mu.Unlock()
}

// Mono reports real elapsed time so budgets behave normally in tests.
func (c *FakeClock) Mono() time.Duration { return time.Since(c.started) }

// SecondsToNS converts whole seconds to nanoseconds with overflow checking.
func SecondsToNS(sec int64) (int64, bool) {
	const maxSeconds = int64(1) << 33 // ~272 years, far outside every domain bound
	if sec < 0 || sec > maxSeconds {
		return 0, false
	}
	return sec * int64(time.Second), true
}

// AddSecondsNS returns base + sec*1e9, reporting overflow instead of wrapping.
func AddSecondsNS(base, sec int64) (int64, bool) {
	delta, ok := SecondsToNS(sec)
	if !ok {
		return 0, false
	}
	sum := base + delta
	if (delta > 0 && sum < base) || (delta < 0 && sum > base) {
		return 0, false
	}
	return sum, true
}
