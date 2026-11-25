package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/rescue"
	"dustkeeper/internal/review"
	"dustkeeper/internal/run"
)

// TestR14PurgeAndRestoreRemoveManifestsAndSweepOrphanTmp verifies that:
//  1. manifests stay present while objects are quarantined,
//  2. restoring or purging an object removes its dead manifest once committed so
//     `rescue.Inspect` never reports false `manifest_without_object` gaps, and
//  3. stale orphan files inside `.dustkeeper/tmp` are swept while fresh tmp files stay.
func TestR14PurgeAndRestoreRemoveManifestsAndSweepOrphanTmp(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "purge.zip", "purge-payload", 60*24*time.Hour)
	e.writeFile("exports", "restore.zip", "restore-payload", 60*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(7, sc.ID)

	rPurge := e.rule(sc.ID, "rule-purge", []string{"purge.*"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(rPurge, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(rPurge, domain.CapAutoPurge, 10, 1<<30)

	rRestore := e.rule(sc.ID, "rule-restore", []string{"restore.*"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(rRestore, domain.CapAutoQuarantine, 10, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 2 {
		t.Fatalf("expected 2 quarantined, got %+v", rep)
	}

	root := filepath.Join(e.base, "trees", "exports")
	repMid, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("rescue inspect mid: %v", err)
	}
	if repMid.Manifests != 2 || repMid.Complete != 2 || repMid.Missing != 0 {
		t.Fatalf("expected 2 intact manifests while quarantined, got %+v", repMid)
	}

	// Advance past purge.zip retention (8d < restore.zip 30d hold): purge.zip
	// purges -> its manifest is removed.
	repPurge := e.advancePast(8 * 24 * time.Hour)
	if repPurge.Purged != 1 {
		t.Fatalf("expected 1 purged, got %+v", repPurge)
	}
	repAfterPurge, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("rescue inspect after purge: %v", err)
	}
	if repAfterPurge.Manifests != 1 || repAfterPurge.Missing != 0 {
		t.Fatalf("expected 1 manifest and 0 missing after purge, got %+v", repAfterPurge)
	}

	// Place one aged (>24h) orphan and one fresh (1m) file in .dustkeeper/tmp
	// and run a clean pass so SweepOrphanTmp sweeps only stale.tmp.
	tmpDir := filepath.Join(root, domain.VaultTmp)
	staleTmp := filepath.Join(tmpDir, "stale.tmp")
	freshTmp := filepath.Join(tmpDir, "fresh.tmp")
	if err := os.WriteFile(staleTmp, []byte("orphan"), 0o600); err != nil {
		t.Fatalf("write stale.tmp: %v", err)
	}
	if err := os.WriteFile(freshTmp, []byte("recent"), 0o600); err != nil {
		t.Fatalf("write fresh.tmp: %v", err)
	}
	oldStamp := e.clock.Wall().Add(-48 * time.Hour)
	if err := os.Chtimes(staleTmp, oldStamp, oldStamp); err != nil {
		t.Fatalf("chtimes stale.tmp: %v", err)
	}
	freshStamp := e.clock.Wall().Add(-1 * time.Minute)
	if err := os.Chtimes(freshTmp, freshStamp, freshStamp); err != nil {
		t.Fatalf("chtimes fresh.tmp: %v", err)
	}
	_ = e.runOnce()

	// Restore restore.zip -> its manifest must also be removed.
	items := e.quarantinedItems(sc.ID)
	var restoreID domain.ItemID
	for _, it := range items {
		if string(it.RawRelative) == "restore.zip" {
			restoreID = it.ID
			break
		}
	}
	if restoreID == "" {
		t.Fatal("could not find restore.zip item")
	}
	sctx := e.scopeCtx(sc.ID)
	if _, err := e.exec.Restore(context.Background(), sctx, restoreID, execute.RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	e.closeScopeCtxs()

	repAfterRestore, err := rescue.Inspect(root)
	if err != nil {
		t.Fatalf("rescue inspect after restore: %v", err)
	}
	if repAfterRestore.Manifests != 0 || repAfterRestore.Missing != 0 {
		t.Fatalf("expected 0 manifests and 0 missing after restore, got %+v", repAfterRestore)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("stale orphan tmp file was not swept: %v", err)
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Fatalf("fresh tmp file should not have been swept: %v", err)
	}
}

// TestR16ModelOutageStopsAcrossMultipleScopes verifies that when the first active
// scope hits a model outage (CodeModelUnavailable), remaining active scopes in
// the same run are skipped immediately with zero extra review calls.
func TestR16ModelOutageStopsAcrossMultipleScopes(t *testing.T) {
	e := newEnv(t)
	scA := e.addScope("scopea", domain.KindManagedDisposable)
	scB := e.addScope("scopeb", domain.KindManagedDisposable)

	e.writeFile("scopea", "a.zip", "payload-a", 60*24*time.Hour)
	e.writeFile("scopeb", "b.zip", "payload-b", 60*24*time.Hour)
	e.confirmPolicy(14, scA.ID, scB.ID)

	rA := e.rule(scA.ID, "rule-a", []string{"*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(rA, domain.CapAutoQuarantine, 10, 1<<30)
	rB := e.rule(scB.ID, "rule-b", []string{"*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(rB, domain.CapAutoQuarantine, 10, 1<<30)

	var calledScopes []string
	e.exec.Review = func(ctx context.Context, req review.Request) (review.Outcome, error) {
		calledScopes = append(calledScopes, req.ScopeID)
		return outageReview(ctx, req)
	}

	rep := e.runOnce()
	if len(calledScopes) != 1 {
		t.Fatalf("expected circuit breaker to stop after 1 scope outage call, got calls=%v", calledScopes)
	}
	if rep.UntouchedScopes != 1 {
		t.Fatalf("expected UntouchedScopes=1 after model outage circuit break, got %d (scopes=%+v)",
			rep.UntouchedScopes, rep.Scopes)
	}
	if len(rep.Scopes) != 2 || !strings.Contains(rep.Scopes[1].Skipped, "earlier model outage") {
		t.Fatalf("expected second scope to report skip reason, got %+v", rep.Scopes)
	}
}

// TestR17StallAlertIsRateLimitedAndHealthFileUpdates verifies that:
//  1. an actionable stall (clock untrusted) sends 1 alert and records Healthy=false in health.json,
//  2. repeating the same stall within 24h sends 0 duplicate alerts while incrementing ConsecutiveUnhealthyRuns,
//  3. recovering resets health.json to Healthy=true and clears stall alert dedupe state.
func TestR17StallAlertIsRateLimitedAndHealthFileUpdates(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.confirmPolicy(14, sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)

	ctx := context.Background()
	updateHealth := func(rep run.Report) run.HealthSnapshot {
		t.Helper()
		scopes, err := e.st.ListScopes(ctx)
		if err != nil {
			t.Fatalf("list scopes: %v", err)
		}
		snap, err := run.UpdateHealthFile(e.paths.StateDir, rep, scopes, e.clock.Wall().UnixNano())
		if err != nil {
			t.Fatalf("update health file: %v", err)
		}
		return snap
	}

	// Healthy initial baseline run -> 0 alerts, Healthy=true.
	rep0 := e.runOnce()
	snap0 := updateHealth(rep0)
	if !snap0.Healthy || snap0.ConsecutiveUnhealthyRuns != 0 {
		t.Fatalf("expected initial run healthy, got %+v", snap0)
	}
	if got := e.sender.messages(); len(got) != 0 {
		t.Fatalf("expected 0 alerts on healthy run, got %v", got)
	}

	// Jump 72h (>48h default) -> clock untrusted stall: 1 alert, ConsecutiveUnhealthyRuns=1.
	rep1 := e.step(72 * time.Hour)
	snap1 := updateHealth(rep1)
	if snap1.Healthy || snap1.ConsecutiveUnhealthyRuns != 1 {
		t.Fatalf("expected unhealthy run #1, got %+v", snap1)
	}
	if msgs := e.sender.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "时钟校验未通过") {
		t.Fatalf("expected 1 clock stall alert, got %v", msgs)
	}

	// Repeat 1 hour later under same signature -> rate-limited (still 1 alert total), ConsecutiveUnhealthyRuns=2.
	rep2 := e.step(1 * time.Hour)
	snap2 := updateHealth(rep2)
	if snap2.Healthy || snap2.ConsecutiveUnhealthyRuns != 2 {
		t.Fatalf("expected unhealthy run #2, got %+v", snap2)
	}
	if msgs := e.sender.messages(); len(msgs) != 1 {
		t.Fatalf("expected 24h dedupe to suppress repeat stall alert, got %v", msgs)
	}

	// Acknowledge clock -> recovers health.json to Healthy=true and resets streak.
	if _, err := clocktrust.Acknowledge(ctx, e.st, e.clock); err != nil {
		t.Fatalf("acknowledge clock: %v", err)
	}
	rep3 := e.runOnce()
	snap3 := updateHealth(rep3)
	if !snap3.Healthy || snap3.ConsecutiveUnhealthyRuns != 0 {
		t.Fatalf("expected recovery to reset health.json, got %+v", snap3)
	}
	readBack, err := run.ReadHealthFile(e.paths.StateDir)
	if err != nil || readBack == nil || !readBack.Healthy {
		t.Fatalf("unexpected ReadHealthFile result: snap=%+v err=%v", readBack, err)
	}
}

// TestR18CredentialShortCircuitAndCandidateOrderSizeDesc verifies that:
//  1. expanded credential filenames (.keystore, kubeconfig, etc.) are protected
//     both in planning and execution with zero external model calls, and
//  2. candidate_order="size_desc" prioritizes larger eligible files ahead of
//     alphabetically earlier smaller files when MaxFiles=1.
func TestR18CredentialShortCircuitAndCandidateOrderSizeDesc(t *testing.T) {
	e := newEnv(t)
	e.configure(func(c *config.Config) {
		c.Runtime.CandidateOrder = "size_desc"
	})
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "prod.keystore", strings.Repeat("k", 8192), 60*24*time.Hour)
	e.writeFile("exports", "kubeconfig", strings.Repeat("k", 8192), 60*24*time.Hour)
	e.writeFile("exports", "a-small.zip", "tiny", 60*24*time.Hour)
	e.writeFile("exports", "z-large.zip", strings.Repeat("z", 4096), 60*24*time.Hour)
	e.confirmPolicy(14, sc.ID)

	r := e.rule(sc.ID, "rule-all", []string{"*"}, nil, 30*24*time.Hour, 14*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 1, 1<<30)

	var reviewed []string
	e.exec.Review = func(ctx context.Context, req review.Request) (review.Outcome, error) {
		reviewed = append(reviewed, req.RelativePath)
		return factGatedReview(domain.VerdictAllow)(ctx, req)
	}

	rep := e.runOnce()
	if rep.Quarantined != 1 {
		t.Fatalf("expected 1 quarantined, got %+v", rep)
	}
	if len(reviewed) != 1 || reviewed[0] != "z-large.zip" {
		t.Fatalf("expected z-large.zip selected first by size_desc and credentials untouched, reviewed=%v", reviewed)
	}
	if !e.exists("exports", "prod.keystore") || !e.exists("exports", "kubeconfig") {
		t.Fatal("credential files must never be moved")
	}
	if !e.exists("exports", "a-small.zip") || e.exists("exports", "z-large.zip") {
		t.Fatalf("unexpected file state: a-small=%v z-large=%v",
			e.exists("exports", "a-small.zip"), e.exists("exports", "z-large.zip"))
	}
}
