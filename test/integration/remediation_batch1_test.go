package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/outcome"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// TestR01PendingDispatchBlocksReplannedScheduledItem verifies that an unsettled
// pending dispatch left by a crashed scheduled run still suppresses re-asking
// on the next run even though CreateRulePlan assigns a brand-new item_id.
func TestR01PendingDispatchBlocksReplannedScheduledItem(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)

	obsA, err := e.st.GetObservation(context.Background(), sc.ID, []byte("a.zip"))
	if err != nil {
		t.Fatalf("get observation a.zip: %v", err)
	}
	// Simulate an unsettled dispatch from a crashed prior run with an older item_id.
	if err := e.st.PrepareReviewDispatch(context.Background(), store.ReviewDispatch{
		DispatchID:   "disp_crashed_old",
		ReviewID:     "rev_crashed_old",
		RunID:        "run_crashed_old",
		ItemID:       "obj_from_crashed_run",
		ScopeID:      string(sc.ID),
		Action:       domain.DispositionQuarantine,
		IdentityJSON: obsA.IdentityJSON,
		TokenJSON:    obsA.TokenJSON,
		BudgetJSON:   "{}",
		CreatedNS:    e.clock.Wall().UnixNano(),
		State:        "pending",
		CostStatus:   review.CostNotSent,
	}); err != nil {
		t.Fatalf("seed pending dispatch: %v", err)
	}

	var asked []string
	e.exec.Review = func(ctx context.Context, r review.Request) (review.Outcome, error) {
		asked = append(asked, r.RelativePath)
		return factGatedReview(domain.VerdictAllow)(ctx, r)
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 1, 1<<30)

	rep := e.runOnce()
	// W1 + R01: a.zip is skipped as frozen by its pending dispatch, and b.zip
	// takes the single slot in the same run without re-asking a.zip.
	if rep.Quarantined != 1 {
		t.Fatalf("expected b.zip to be quarantined while a.zip stays frozen, got %+v", rep)
	}
	if len(asked) != 1 || asked[0] != "b.zip" {
		t.Fatalf("reviewed paths = %v, want [b.zip] only (a.zip must never be re-asked)", asked)
	}
	if !e.exists("exports", "a.zip") || e.exists("exports", "b.zip") {
		t.Fatalf("unexpected file presence: a=%v b=%v", e.exists("exports", "a.zip"), e.exists("exports", "b.zip"))
	}
}

// TestR02PurgeReusesKeepAndDoesNotReaskDaily verifies that a due quarantined
// object kept by an expiry review is skipped during KeepRetryDays and lets the
// next due object proceed without wasting model calls.
func TestR02PurgeReusesKeepAndDoesNotReaskDaily(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)

	pol := cleanup.Default()
	pol.Scopes = []string{string(sc.ID)}
	pol.ObservationDays = 7
	pol.KeepRetryDays = 14
	pol.Notify = cleanup.NotifyNone
	e.savePolicy(pol)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 1, 1<<30)

	// Quarantine a.zip first, then advance 1 hour before quarantining b.zip so
	// a.zip deterministically precedes b.zip in purge_not_before_ns order.
	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("fixture: expected a.zip quarantined, got %+v", rep)
	}
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	if rep := e.step(time.Hour); rep.Quarantined != 1 {
		t.Fatalf("fixture: expected b.zip quarantined, got %+v", rep)
	}

	purgeAsked := map[string]int{}
	e.exec.Review = func(ctx context.Context, req review.Request) (review.Outcome, error) {
		if req.Action == domain.ActionPurge {
			purgeAsked[req.RelativePath]++
			if req.RelativePath == "a.zip" {
				return keepReview(outcome.ReasonModelSaysKeep)(ctx, req)
			}
		}
		return factGatedReview(domain.VerdictAllow)(ctx, req)
	}

	// Move past the 7-day retention window: round 1 asks a.zip (keep).
	rep1 := e.advancePast(8 * 24 * time.Hour)
	if rep1.Purged != 0 || purgeAsked["a.zip"] != 1 {
		t.Fatalf("round 1: purged=%d asked=%v", rep1.Purged, purgeAsked)
	}

	// Next day (still within KeepRetryDays): a.zip must NOT be re-asked and
	// b.zip must take the 1-file purge slot and get purged.
	rep2 := e.step(24 * time.Hour)
	if rep2.Purged != 1 {
		t.Fatalf("round 2: expected b.zip purged while a.zip is suppressed, got %+v", rep2)
	}
	if purgeAsked["a.zip"] != 1 || purgeAsked["b.zip"] != 1 {
		t.Fatalf("unexpected purge review counts: %v (want a=1, b=1)", purgeAsked)
	}
}

// TestR03KeptItemDoesNotStarveNextCandidateAndSurvives500Events verifies W1:
// with MaxFiles=1, a kept file A does not starve B, even after >500 unrelated
// keep events, and a file exceeding 2x read budget does not block smaller files.
func TestR03KeptItemDoesNotStarveNextCandidateAndSurvives500Events(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b-huge.zip", strings.Repeat("x", 2048), 40*24*time.Hour)
	e.writeFile("exports", "c.zip", "payload-c", 40*24*time.Hour)
	e.scan(sc.ID)

	pol := cleanup.Default()
	pol.Scopes = []string{string(sc.ID)}
	pol.KeepRetryDays = 14
	pol.Notify = cleanup.NotifyNone
	e.savePolicy(pol)

	// Set read budget to 3000 bytes so b-huge.zip (2048 * 2 = 4096 > 3000) is
	// skipped in planning while c.zip (9 * 2 = 18 <= 3000) proceeds.
	e.configure(func(c *config.Config) { c.Runtime.RunReadBudgetBytes = 3000 })

	asked := map[string]int{}
	e.exec.Review = func(ctx context.Context, req review.Request) (review.Outcome, error) {
		asked[req.RelativePath]++
		if req.RelativePath == "a.zip" {
			return keepReview(outcome.ReasonModelSaysKeep)(ctx, req)
		}
		return factGatedReview(domain.VerdictAllow)(ctx, req)
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 1, 1<<30)

	// Round 1: a.zip gets reviewed and kept.
	if rep := e.runOnce(); rep.Quarantined != 0 || asked["a.zip"] != 1 {
		t.Fatalf("round 1: quarantined=%d asked=%v", rep.Quarantined, asked)
	}

	// Inject 510 unrelated keep events (exceeding the old 500-row cliff).
	nowNS := e.clock.Wall().UnixNano()
	resJSON := `{"source":"decision","disposition":"keep","reason":"model_says_keep","identity":"{}","token":"{}"}`
	for i := 0; i < 510; i++ {
		if err := e.st.AppendEvent(context.Background(), store.Event{
			EventID:      fmt.Sprintf("ev_noise_%04d", i),
			Action:       "keep",
			Phase:        domain.PhaseInfo,
			OccurredAtNS: nowNS + int64(i+1),
			ContractJSON: `{"scope_id":"other","raw_relative_b64":"eA=="}`,
			ResultJSON:   &resJSON,
		}); err != nil {
			t.Fatalf("append noise keep %d: %v", i, err)
		}
	}

	// Round 2: a.zip must still be skipped (despite >500 newer keeps),
	// b-huge.zip must be skipped (exceeds 2x read budget), and c.zip must be quarantined.
	rep2 := e.step(time.Hour)
	if rep2.Quarantined != 1 {
		t.Fatalf("round 2: expected c.zip quarantined, got %+v", rep2)
	}
	if asked["a.zip"] != 1 || asked["b-huge.zip"] != 0 || asked["c.zip"] != 1 {
		t.Fatalf("unexpected review calls: %v (want a=1, b-huge=0, c=1)", asked)
	}
	if !e.exists("exports", "a.zip") || !e.exists("exports", "b-huge.zip") || e.exists("exports", "c.zip") {
		t.Fatalf("unexpected files remaining: a=%v b-huge=%v c=%v",
			e.exists("exports", "a.zip"), e.exists("exports", "b-huge.zip"), e.exists("exports", "c.zip"))
	}
}

// TestR04LocalFactsFailureShortCircuitsBeforePaidReview verifies that when
// local facts fail (e.g. open-file probe unavailable or file is open), no model
// call is made and the transient local refusal does not poison KeepRetryDays.
func TestR04LocalFactsFailureShortCircuitsBeforePaidReview(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "busy.zip", "payload-busy", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(14, sc.ID)

	calls := 0
	e.exec.Review = func(ctx context.Context, r review.Request) (review.Outcome, error) {
		calls++
		return factGatedReview(domain.VerdictAllow)(ctx, r)
	}
	e.exec.OpenFiles = func(context.Context, string) (assess.OpenObservation, error) {
		return assess.OpenObservation{Coverage: "unknown", Paths: map[string]bool{}}, nil
	}

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 5, 1<<30)

	rep1 := e.runOnce()
	if rep1.Quarantined != 0 {
		t.Fatalf("moved file despite unknown open-file fact: %+v", rep1)
	}
	if calls != 0 {
		t.Fatalf("paid model review was called %d time(s) even though local facts already forbade action", calls)
	}

	// Once the probe succeeds on the next run, the file must NOT be stuck in
	// KeepRetryDays from the transient local probe failure.
	e.exec.OpenFiles = func(context.Context, string) (assess.OpenObservation, error) {
		return assess.OpenObservation{Coverage: "visible_processes_only", Paths: map[string]bool{}}, nil
	}
	rep2 := e.step(time.Hour)
	if rep2.Quarantined != 1 || calls != 1 {
		t.Fatalf("expected quarantine once probe recovered: quarantined=%d calls=%d rep=%+v", rep2.Quarantined, calls, rep2)
	}
}

// TestR05ReminderFailureReleasesPurgeGrantQuota verifies that when the
// pre-purge reminder fails, the purge grant quota claimed during collection is
// released so direct-delete under the same grant in the same run is not starved.
func TestR05ReminderFailureReleasesPurgeGrantQuota(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "due.zip", "payload-due", 40*24*time.Hour)
	e.scan(sc.ID)

	// Confirm policy with macOS notifications enabled (so reminder runs) and
	// archive_unknown_contents approved for direct deletion.
	pol := cleanup.Default()
	pol.Scopes = []string{string(sc.ID)}
	pol.ObservationDays = 7
	pol.DirectDeleteCategories = []string{}
	pol.Notify = cleanup.NotifyNone
	e.savePolicy(pol)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 5, 1<<30)
	e.grant(r, domain.CapAutoPurge, 1, 1<<30)

	// Quarantine due.zip first.
	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("fixture: expected due.zip quarantined, got %+v", rep)
	}

	// Advance to just past due.zip's 7-day retention window first, then drop
	// fresh.zip so both due.zip (expired purge) and fresh.zip (direct delete)
	// compete for the SAME 1-file purge grant on the next run.
	e.step(47 * time.Hour)
	e.step(47 * time.Hour)
	e.step(47 * time.Hour)
	e.writeFile("exports", "fresh.zip", "payload-fresh", 40*24*time.Hour)
	pol.DirectDeleteCategories = []string{"archive_unknown_contents"}
	pol.Notify = cleanup.NotifyMacOS
	e.savePolicy(pol)
	e.sender.fail = errors.New("no GUI session")

	rep := e.step(30 * time.Hour)
	// due.zip was NOT purged because reminder failed, so the 1-file purge grant
	// must have been released and fresh.zip must be directly deleted!
	if rep.Purged != 1 {
		t.Fatalf("expected grant quota released after reminder failure so fresh.zip could be directly deleted, got %+v", rep)
	}
	if e.exists("exports", "fresh.zip") {
		t.Fatal("fresh.zip was starved because failed reminder leaked the purge grant quota")
	}
}
