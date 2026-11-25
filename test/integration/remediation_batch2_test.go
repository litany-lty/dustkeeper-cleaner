package integration

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/run"
	"dustkeeper/internal/scan"
)

// TestR06FailedSubdirDoesNotFreezeHealthySubdirOrNewFileDiscovery verifies that
// a permanently unreadable subdirectory:
//  1. keeps its own epoch open and never retires its own observations (R11),
//  2. does NOT stop healthy subtrees from discovering newly added files on
//     subsequent runs, and
//  3. does NOT permanently freeze cleanup of healthy subtrees while excluding
//     the unreadable subtree until it recovers.
func TestR06FailedSubdirDoesNotFreezeHealthySubdirOrNewFileDiscovery(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "good/old.zip", "good-old-payload", 60*24*time.Hour)
	e.writeFile("exports", "bad/locked.zip", "bad-locked-payload", 60*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	// Make `bad` unreadable.
	scan.TestHookDirRead = func(rel string) error {
		if rel == "bad" {
			return syscall.EACCES
		}
		return nil
	}
	defer func() { scan.TestHookDirRead = nil }()

	// Round 1: healthy subdir `good/old.zip` cleans normally while `bad` is
	// excluded and reported as unreadable.
	rep1 := e.runOnce()
	if rep1.Quarantined != 1 {
		t.Fatalf("round 1 expected healthy good/old.zip to be quarantined, got %d (scopes=%+v)", rep1.Quarantined, rep1.Scopes)
	}
	if e.exists("exports", "good/old.zip") {
		t.Fatal("good/old.zip should have been quarantined")
	}
	if !e.exists("exports", "bad/locked.zip") {
		t.Fatal("bad/locked.zip inside failed directory must not be touched")
	}
	if len(rep1.Scopes) != 1 || rep1.Scopes[0].ScanComplete {
		t.Fatalf("expected incomplete scan status while bad/ fails: %+v", rep1.Scopes)
	}
	if !strings.Contains(rep1.Scopes[0].Message, "bad") {
		t.Fatalf("expected failed dir name in message, got %q", rep1.Scopes[0].Message)
	}
	scAfter1, err := e.st.GetScope(context.Background(), sc.ID)
	if err != nil {
		t.Fatalf("get scope after round 1: %v", err)
	}
	epoch1 := scAfter1.ScanEpoch

	// Drop a brand-new old file into `good/` while `bad/` is STILL unreadable.
	e.writeFile("exports", "good/new.zip", "good-new-payload", 60*24*time.Hour)

	// Round 2: readable dirs refresh inside the same epoch, discovering and
	// cleaning `good/new.zip`, while `bad/locked.zip` stays un-retired.
	rep2 := e.runOnce()
	scAfter2, err := e.st.GetScope(context.Background(), sc.ID)
	if err != nil {
		t.Fatalf("get scope after round 2: %v", err)
	}
	if scAfter2.ScanEpoch != epoch1 {
		t.Fatalf("epoch should stay open at %d while bad/ fails, got %d", epoch1, scAfter2.ScanEpoch)
	}
	if rep2.Quarantined != 1 {
		t.Fatalf("round 2 expected newly dropped good/new.zip to be discovered and quarantined, got %d (scopes=%+v)", rep2.Quarantined, rep2.Scopes)
	}
	if e.exists("exports", "good/new.zip") {
		t.Fatal("good/new.zip should have been discovered and quarantined")
	}
	if _, status := e.observationOf(t, sc.ID, "bad/locked.zip"); status != domain.ObsSeen {
		t.Fatalf("bad/locked.zip observation must not be retired while bad/ fails, got status %q", status)
	}

	// Lift the fault on `bad/`: epoch completes and `bad/locked.zip` cleans.
	scan.TestHookDirRead = nil
	rep3 := e.runOnce()
	if len(rep3.Scopes) != 1 || !rep3.Scopes[0].ScanComplete {
		t.Fatalf("expected epoch to complete after bad/ recovered: %+v", rep3.Scopes)
	}
	if rep3.Quarantined != 1 || e.exists("exports", "bad/locked.zip") {
		t.Fatalf("expected bad/locked.zip to be quarantined once readable, got %d", rep3.Quarantined)
	}
}

// TestR08ClockMaxGapHoursConfigAllowsOfflineWindow verifies that setting
// runtime.clock_max_gap_hours allows unattended runs to resume cleanly after a
// multi-day offline/sleep gap within the configured window, while still
// suspending runs that exceed the window.
func TestR08ClockMaxGapHoursConfigAllowsOfflineWindow(t *testing.T) {
	e := newEnv(t)
	e.configure(func(c *config.Config) {
		c.Runtime.ClockMaxGapHours = 168 // 7 days
	})
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.confirmPolicy(14, sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	// Baseline trusted run.
	if rep := e.runOnce(); !rep.ClockTrusted {
		t.Fatalf("initial run should be trusted: %+v", rep)
	}

	// Sleep 100 hours (> 48h default, < 168h configured) and drop a candidate.
	e.writeFile("exports", "after-sleep.zip", "payload", 60*24*time.Hour)
	rep := e.step(100 * time.Hour)
	if !rep.ClockTrusted {
		t.Fatalf("100h offline gap should stay trusted under 168h config: %+v", rep)
	}
	if rep.Quarantined != 1 {
		t.Fatalf("expected after-sleep.zip to be quarantined, got %d", rep.Quarantined)
	}

	// Sleep 200 hours (> 168h configured): must suspend until acknowledged.
	e.writeFile("exports", "too-late.zip", "payload", 60*24*time.Hour)
	repLate := e.step(200 * time.Hour)
	if repLate.ClockTrusted {
		t.Fatalf("200h gap should not be trusted under 168h config: %+v", repLate)
	}
	if repLate.Quarantined != 0 {
		t.Fatalf("expected 0 quarantined when clock untrusted, got %d", repLate.Quarantined)
	}
}

// TestR09DiskPressurePerScopeAndAllowsDirectDelete verifies that:
//  1. disk pressure on one scope's volume does NOT block quarantine on a second
//     scope whose volume has sufficient free space, and
//  2. on the full scope itself, reversible quarantine moves stay paused while
//     confirmed direct-delete categories can still run to free space.
func TestR09DiskPressurePerScopeAndAllowsDirectDelete(t *testing.T) {
	e := newEnv(t)
	fullSc := e.addScope("fullvol", domain.KindManagedDisposable)
	healthySc := e.addScope("healthyvol", domain.KindManagedDisposable)

	e.writeFile("fullvol", "old.dmg", "installer-bytes", 60*24*time.Hour)
	e.writeFile("fullvol", "keepme.zip", "archive-bytes", 60*24*time.Hour)
	e.writeFile("healthyvol", "ok.zip", "healthy-bytes", 60*24*time.Hour)

	// Confirm cleanup policy covering both scopes with `installer` allowed for
	// direct deletion (while `archive_unknown_contents` stays quarantine-only).
	if err := cleanup.Save(context.Background(), e.st, cleanup.Policy{
		Version:                1,
		Scopes:                 []string{string(fullSc.ID), string(healthySc.ID)},
		DirectDeleteCategories: []string{cleanup.CategoryOf("old.dmg")},
		ObservationDays:        14,
		KeepRetryDays:          7,
		MaxFilesPerRun:         100,
		MaxBytesPerRun:         1 << 30,
		Notify:                 cleanup.NotifyNone,
		ConfirmedAtNS:          e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("save policy: %v", err)
	}

	rFull := e.rule(fullSc.ID, "full-rule", []string{"*.dmg", "*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(rFull, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(rFull, domain.CapAutoPurge, 10, 1<<30)

	rHealthy := e.rule(healthySc.ID, "healthy-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(rHealthy, domain.CapAutoQuarantine, 10, 1<<30)

	e.configure(func(c *config.Config) {
		c.Runtime.MinFreePercent = 10
		c.Runtime.MinFreeBytes = 1 << 20
	})

	// Simulate fullvol at 2% free and healthyvol at 80% free.
	diskFn := func(path string) (uint64, uint64, error) {
		if path == string(fullSc.RawRoot) {
			return 20 << 20, 1000 << 20, nil // 2% free (< 10%)
		}
		return 800 << 20, 1000 << 20, nil // 80% free
	}
	run.TestHookDiskStatus = diskFn
	execute.TestHookDiskStatus = diskFn
	defer func() {
		run.TestHookDiskStatus = nil
		execute.TestHookDiskStatus = nil
	}()

	rep := e.runOnce()

	// Healthy scope must quarantine normally despite fullvol being under pressure.
	if e.exists("healthyvol", "ok.zip") {
		t.Fatalf("healthyvol/ok.zip was blocked by fullvol's disk pressure: %+v", rep.Scopes)
	}
	// Full scope must permanently delete the confirmed installer category to
	// free space, while keeping quarantine-only keepme.zip untouched.
	if e.exists("fullvol", "old.dmg") {
		t.Fatalf("fullvol/old.dmg should have been direct-deleted under disk pressure: %+v", rep.Scopes)
	}
	if !e.exists("fullvol", "keepme.zip") {
		t.Fatal("fullvol/keepme.zip should NOT be quarantined onto a full volume")
	}
	if rep.Purged != 1 || rep.Quarantined != 1 {
		t.Fatalf("expected purged=1 (fullvol dmg) and quarantined=1 (healthyvol zip), got purged=%d quarantined=%d scopes=%+v",
			rep.Purged, rep.Quarantined, rep.Scopes)
	}
}
