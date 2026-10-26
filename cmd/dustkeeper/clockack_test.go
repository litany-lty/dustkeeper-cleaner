package main

import (
	"context"
	"testing"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/config"
	"dustkeeper/internal/store"
)

// checkByName returns one entry of the doctor's check list.
func checkByName(t *testing.T, r result, name string) map[string]any {
	t.Helper()
	if r.env == nil {
		t.Fatalf("the command produced no JSON envelope; stdout was:\n%s", r.stdout)
	}
	data, _ := r.env["data"].(map[string]any)
	list, _ := data["checks"].([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("doctor reported no %q check; got %v", name, list)
	return nil
}

// openStateStore opens the installation's database without going through the
// CLI, so a test can put the state into a condition only a bad day produces.
func openStateStore(t *testing.T) *store.Store {
	t.Helper()
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	st, err := store.Open(paths.DBFile)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// TestDoctorAcknowledgingTheClockClearsTheLatch drives the real entry point.
//
// The command used to write one timestamp and report success. The latch stayed,
// so every purge stayed suspended while the operator was told the clock was
// fine. The acknowledgement is a transaction: it clears the latch and restarts
// the windows that could not be proven.
func TestDoctorAcknowledgingTheClockClearsTheLatch(t *testing.T) {
	newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init exited %d: %s%s", r.code, r.stdout, r.stderr)
	}

	// A run that could not prove the clock writes the doubt down.
	st := openStateStore(t)
	if err := st.LatchClock(context.Background(), time.Now().UnixNano(),
		"the wall clock drifted away from the monotonic clock during this run"); err != nil {
		st.Close()
		t.Fatalf("latch: %v", err)
	}
	st.Close()

	// Reporting the latch is part of the contract: an operator running doctor
	// before an acknowledgement must be told purging is suspended.
	before := cli(t, "doctor", "--json")
	if before.code == 0 {
		t.Fatalf("doctor reported success while purging was suspended:\n%s", before.stdout)
	}
	if c := checkByName(t, before, "clock"); c["ok"] != false {
		t.Fatalf("the clock check passed with a latch in place: %v", c)
	}

	after := cli(t, "doctor", "--acknowledge-clock", "--json")
	if after.code != 0 {
		t.Fatalf("acknowledging exited %d: %s%s", after.code, after.stdout, after.stderr)
	}
	c := checkByName(t, after, "clock")
	if c["ok"] != true {
		t.Fatalf("the clock check did not pass after an acknowledgement: %v", c)
	}
	detail, _ := c["detail"].(string)
	if detail == "" {
		t.Fatal("the acknowledgement reported nothing about what it did")
	}

	st = openStateStore(t)
	defer st.Close()
	if at, reason, err := st.ClockLatch(context.Background()); err != nil || at != 0 {
		t.Fatalf("the acknowledgement left the latch in place: at=%d reason=%q err=%v", at, reason, err)
	}
	ack, err := st.MetaGet(context.Background(), clocktrust.MetaAck)
	if err != nil {
		t.Fatalf("read acknowledgement: %v", err)
	}
	if ack == "" {
		t.Fatal("no acknowledgement instant was recorded")
	}

	// The state must now survive the question the purge path actually asks.
	v := clocktrust.Start(st, testClock{time.Now()})
	verdict, err := v.Assess(context.Background())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if !verdict.Trusted {
		t.Fatalf("the acknowledged clock is still refused: %s", verdict.Reason)
	}
}

// testClock is the smallest clock a verdict needs.
type testClock struct{ at time.Time }

func (c testClock) Wall() time.Time     { return c.at }
func (c testClock) Mono() time.Duration { return 0 }
