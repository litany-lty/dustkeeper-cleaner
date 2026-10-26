package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/run"
	"dustkeeper/internal/store"
)

// The tests in this file are about the gates in front of an irreversible action:
// how much a grant approves, when the clock stops being good enough, what disk
// pressure may and may not stop, and which members a plan applies to.

// configure changes the runtime configuration of a running environment.
func (e *env) configure(f func(*config.Config)) {
	e.t.Helper()
	f(&e.cfg)
	e.exec.Config = executeConfig(e.cfg)
	e.run.Config = e.cfg
}

// runOnce performs one background run and returns its report.
func (e *env) runOnce() run.Report {
	e.t.Helper()
	rep, err := e.run.Run(context.Background())
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return rep
}

// step advances the clock by d and runs once. Callers keep each step below the
// 48 hour jump threshold, which is what a scheduled installation does: a single
// larger jump is a suspect clock, not a busy morning.
func (e *env) step(d time.Duration) run.Report {
	e.t.Helper()
	e.clock.Advance(d)
	return e.runOnce()
}

// quarantinedItems returns the objects that are waiting out their retention
// window, whether or not they are due yet.
func (e *env) quarantinedItems(id domain.ScopeID) []store.Item {
	e.t.Helper()
	// The deadline argument is only a query bound; passing a distant instant
	// asks "everything that will ever be due", which is what this helper wants.
	future := e.clock.Wall().AddDate(1, 0, 0).UnixNano()
	items, err := e.st.PurgeQueuePage(context.Background(), id, future, 0, "", 100)
	if err != nil {
		e.t.Fatalf("purge queue: %v", err)
	}
	return items
}

// TestAutomaticPurgeHonoursTheGrantCeiling is the case the review describes: the
// configuration allows ten objects this run, the user approved one for this
// grant, and two are due. The approval is the smaller ceiling and it wins.
func TestAutomaticPurgeHonoursTheGrantCeiling(t *testing.T) {
	e := newEnv(t)
	e.configure(func(c *config.Config) { c.Runtime.MaxAutoPurgeFiles = 10 })

	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)

	const retention = 7 * 24 * time.Hour
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, retention)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	// This is the number the approval prompt showed: one object.
	e.grant(r, domain.CapAutoPurge, 1, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 2 {
		t.Fatalf("expected both objects to be quarantined, got %d", rep.Quarantined)
	}
	if got := len(e.quarantinedItems(sc.ID)); got != 2 {
		t.Fatalf("expected two objects in the queue, got %d", got)
	}

	// Walk the clock towards the deadline in steps a scheduler would use.
	e.step(47 * time.Hour)
	e.step(47 * time.Hour)
	e.step(47 * time.Hour)
	rep := e.step(28 * time.Hour) // past the seventh day

	if !rep.ClockTrusted {
		t.Fatalf("the clock was not trusted at the deadline: %s", rep.ClockNote)
	}
	if rep.Purged != 1 {
		t.Fatalf("the grant approved one object but %d were purged", rep.Purged)
	}
	if got := len(e.quarantinedItems(sc.ID)); got != 1 {
		t.Fatalf("expected one object to survive the granted ceiling, %d remain", got)
	}

	// The survivor is not wedged: the next run approves a new plan against the
	// same ceiling and takes the next object.
	if rep := e.step(30 * time.Minute); rep.Purged != 1 {
		t.Fatalf("the remaining object was not reachable in the next run: purged %d", rep.Purged)
	}
	if got := len(e.quarantinedItems(sc.ID)); got != 0 {
		t.Fatalf("expected an empty queue, %d objects remain", got)
	}
}

// TestAnUntrustedRunLeavesTheTrustedBaselineWhereItWas covers the laundering
// path: a run that cannot prove the clock must not become the yardstick for the
// next one, and returning the wall clock to a normal value must not clear the
// doubt that run recorded.
func TestAnUntrustedRunLeavesTheTrustedBaselineWhereItWas(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 10, 1<<30)

	if rep := e.runOnce(); !rep.ClockTrusted {
		t.Fatalf("the first run did not trust a coherent clock: %s", rep.ClockNote)
	}
	baseline, err := e.st.MetaGet(ctx, clocktrust.MetaLastTrustedRun)
	if err != nil || baseline == "" {
		t.Fatalf("no trusted baseline was recorded: %q %v", baseline, err)
	}

	// The machine was off for months.
	e.clock.Advance(300 * 24 * time.Hour)
	rep := e.runOnce()
	if rep.ClockTrusted {
		t.Fatal("a 300 day gap was treated as a trustworthy clock")
	}
	if rep.Purged != 0 {
		t.Fatalf("an object was purged on an untrusted clock: %d", rep.Purged)
	}
	after, err := e.st.MetaGet(ctx, clocktrust.MetaLastTrustedRun)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if after != baseline {
		t.Fatalf("the untrusted run moved the trusted baseline from %s to %s", baseline, after)
	}
	if at, _, _ := e.st.ClockLatch(ctx); at == 0 {
		t.Fatal("the untrusted run did not latch the clock")
	}

	// Putting the wall clock back to a sensible value must not launder the
	// anomaly: the latch is the only way out, and only an operator clears it.
	e.clock.SetWall(fixedNow.Add(time.Hour))
	rep = e.runOnce()
	if rep.ClockTrusted {
		t.Fatal("moving the wall clock back cleared a latched anomaly")
	}
	if rep.Purged != 0 {
		t.Fatalf("something was purged while the clock was latched: %d", rep.Purged)
	}
}

// TestAcknowledgingTheClockRestartsUnprovableWindows covers the second half of
// the acknowledgement contract: after a long outage, saying "I accept this
// clock" must not make every object that was never observed during the outage
// deletable on the spot. Each of them gets a full new window.
func TestAcknowledgingTheClockRestartsUnprovableWindows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)

	const retention = 7 * 24 * time.Hour
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, retention)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 10, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("expected one object to be quarantined, got %d", rep.Quarantined)
	}
	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("expected one queued object, got %d", len(items))
	}
	oldDue := *items[0].PurgeNotBeforeNS

	// Two months pass without a run, so nothing observed the clock.
	e.clock.Advance(60 * 24 * time.Hour)
	if rep := e.runOnce(); rep.ClockTrusted {
		t.Fatal("a sixty day gap was treated as a trustworthy clock")
	}

	ackNS := e.clock.Wall().UnixNano()
	ack, err := clocktrust.Acknowledge(ctx, e.st, e.clock)
	if err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if ack.ExtendedObjects != 1 {
		t.Fatalf("expected one object to receive a new window, got %d", ack.ExtendedObjects)
	}

	got, err := e.st.GetItem(ctx, items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.PurgeNotBeforeNS == nil {
		t.Fatal("the object lost its purge deadline")
	}
	if *got.PurgeNotBeforeNS <= oldDue {
		t.Fatalf("the acknowledgement moved a deadline earlier: %d then %d", oldDue, *got.PurgeNotBeforeNS)
	}
	want := ackNS + int64(retention/time.Second)*int64(time.Second)
	if *got.PurgeNotBeforeNS < want {
		t.Fatalf("the new window starts before the acknowledgement: due %d, want at least %d",
			*got.PurgeNotBeforeNS, want)
	}

	// The clock is usable again, and the freshly extended object is not due, so
	// the acknowledgement delays a deletion instead of causing one.
	rep := e.runOnce()
	if !rep.ClockTrusted {
		t.Fatalf("the acknowledged clock is still suspended: %s", rep.ClockNote)
	}
	if rep.Purged != 0 {
		t.Fatalf("an object was purged right after an acknowledgement: %d", rep.Purged)
	}
}

// TestDiskPressurePausesQuarantineButNotADuePurge separates the two phases. A
// purge frees space; quarantining copies bytes onto a volume that is already
// below its floor. One shared gate made the pressure stop the cure.
func TestDiskPressurePausesQuarantineButNotADuePurge(t *testing.T) {
	e := newEnv(t)

	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)

	const retention = 7 * 24 * time.Hour
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, retention)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 10, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("expected one object to be quarantined, got %d", rep.Quarantined)
	}

	e.step(47 * time.Hour)
	e.step(47 * time.Hour)
	e.step(47 * time.Hour)

	// The volume is now considered full, and a new candidate appears.
	e.configure(func(c *config.Config) { c.Runtime.MinFreePercent = 100 })
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)

	rep := e.step(28 * time.Hour)
	if rep.Purged != 1 {
		t.Fatalf("disk pressure stopped a purge that was due and authorised: purged %d", rep.Purged)
	}
	if rep.Quarantined != 0 {
		t.Fatalf("a new object was quarantined onto a full volume: %d", rep.Quarantined)
	}
	if len(rep.Scopes) != 1 || !strings.Contains(rep.Scopes[0].QuarantineSkipped, "disk pressure") {
		t.Fatalf("the report does not say why quarantine was skipped: %+v", rep.Scopes)
	}
	if !e.exists("exports", "b.zip") {
		t.Fatal("the new object was moved onto a full volume")
	}
}

// TestAPurgePlanCanBeLoadedForApproval covers the second entry point: both
// `plan apply` and `quarantine apply` read the members of a plan before running
// it, and a purge plan keeps its members in a different table than a quarantine
// plan. Reading the wrong table made every purge plan look empty.
func TestAPurgePlanCanBeLoadedForApproval(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 10, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("expected one object to be quarantined, got %d", rep.Quarantined)
	}
	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("expected one queued object, got %d", len(items))
	}

	for _, kind := range []string{domain.PlanManualPurge, domain.PlanScheduledPurge} {
		p, err := plan.CreatePurgePlan(ctx, e.st, sc, items, kind, e.clock.Wall().UnixNano(),
			int64(plan.DefaultTTL))
		if err != nil {
			t.Fatalf("create %s plan: %v", kind, err)
		}
		members, err := plan.LoadMembers(ctx, e.st, p, p.Digest)
		if err != nil {
			t.Fatalf("%s plan members: %v", kind, err)
		}
		if len(members) != 1 || members[0].ID != items[0].ID {
			t.Fatalf("%s plan members are wrong: %+v", kind, members)
		}
		// A plan whose members cannot be rebuilt cannot be verified, and an
		// unverifiable plan must not be executed.
		if err := plan.VerifyPlanDigest(p, members, p.Digest); err != nil {
			t.Fatalf("%s plan digest did not verify: %v", kind, err)
		}
	}
}
