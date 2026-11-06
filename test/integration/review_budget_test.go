package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/review"
)

// The tests here pin the three facts the incident of 2026-11-06 was missing.
//
// A run that had already deleted ten files reported "model unavailable" and the
// operator read that as "nothing happened". A request that timed out was asked
// again by the next run. And a batch that ran out of its own allowance looked
// exactly like a provider outage.
//
// Each test drives the real service and the real executor; only the judge is a
// fixture, because a paid endpoint cannot be part of a test.

// scriptedJudge answers the first `allow` questions with agreement and the rest
// with a transport timeout. It counts the calls it actually received, which is
// the only honest source for "was a request sent".
type scriptedJudge struct {
	mu      sync.Mutex
	calls   int
	allow   int
	timeout error
}

func (j *scriptedJudge) Review(_ context.Context, _ review.Facts) (review.Decision, error) {
	j.mu.Lock()
	j.calls++
	n := j.calls
	j.mu.Unlock()
	if n <= j.allow {
		return review.Decision{
			Verdict: domain.VerdictAllow, Reason: "dual_review_agrees",
			JEVModel: "jev-fixture", LLMModel: "minimax-fixture", LLMConsulted: true,
			JEV: &review.JEVDiagnostic{Choice: "allow_review", Confidence: 0.95,
				Probabilities: map[string]float64{"allow_review": 0.95, "keep": 0.03, "insufficient": 0.02},
				MinConfidence: 0.9, MinAllowProbability: 0.9, GatePassed: true},
			LLMDecision: "allow", LLMReason: "disposable",
		}, nil
	}
	return review.Decision{
		Verdict:      domain.VerdictKeep,
		ModelError:   "MiniMax review failed: model request timed out after 60s",
		LLMConsulted: true,
		LLMDecision:  "",
	}, errors.New("MiniMax review failed: model request timed out after 60s")
}

func (j *scriptedJudge) Stats() review.Attempts {
	j.mu.Lock()
	defer j.mu.Unlock()
	return review.Attempts{JEV: j.calls, MiniMax: j.calls, Attempts: j.calls * 2}
}

func (j *scriptedJudge) Calls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

// wireReview installs a real review service (with the fixture judge) and its
// batch allowance on the environment's executor.
func (e *env) wireReview(judge review.Judge) *review.Service {
	e.t.Helper()
	svc := &review.Service{Judge: judge, Now: e.clock.Wall, MaxCandidates: 50,
		Defaults: review.Limits{Candidates: 5, Attempts: 10, Seconds: 180,
			Reserve: 10 * time.Second, PerCandidate: 60 * time.Second}}
	e.exec.Review = func(ctx context.Context, r review.Request) (review.Outcome, error) {
		return svc.Triage(ctx, r)
	}
	e.exec.ReviewBudget = serviceBudget{svc: svc, run: e.run.Config.Runtime.RunSoftSeconds}
	e.t.Cleanup(svc.EndBatch)
	return svc
}

// serviceBudget adapts the review service to the executor's allowance interface
// the same way the CLI does.
type serviceBudget struct {
	svc *review.Service
	run int
}

func (b serviceBudget) BeginBatch(remaining func() time.Duration) {
	if remaining == nil {
		b.svc.BeginBatch(review.Limits{})
		return
	}
	b.svc.BeginBatch(review.Limits{RunRemaining: remaining})
}

func (b serviceBudget) CheckStart() string { return b.svc.CheckStart() }
func (b serviceBudget) EndBatch()          { b.svc.EndBatch() }

// TestReviewTimeoutKeepsCompletedWorkAndStopsTheBatch is V1.
//
// Ten files were deleted before the eleventh request timed out. Those ten are
// results, not a prelude to "nothing happened", and the eleventh file must still
// be where it was. The cost of the unanswered request is unknown, so the exit
// status is non-zero and no count claims zero.
func TestReviewTimeoutKeepsCompletedWorkAndStopsTheBatch(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("timeout", domain.KindManagedDisposable)
	names := []string{"a.zip", "b.zip", "c.zip", "d.zip", "e.zip", "f.zip", "g.zip", "h.zip", "i.zip", "j.zip", "k.zip"}
	for _, name := range names {
		e.writeFile("timeout", name, "discard-payload", 40*24*time.Hour)
	}
	e.scan(sc.ID)
	r := e.rule(sc.ID, "timeout-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 50, 1<<30)
	e.grant(r, domain.CapAutoPurge, 50, 1<<30)
	e.savePolicy(directDeletePolicy(sc.ID))
	judge := &scriptedJudge{allow: 10, timeout: errors.New("timed out")}
	svc := e.wireReview(judge)
	// One batch large enough to reach the timeout: the point of this test is the
	// interruption, not the batch size. The fixture reports two HTTP attempts per
	// candidate, which is what a dual review really costs.
	svc.Defaults.Candidates = 20
	svc.Defaults.Attempts = 40

	rep := e.runOnce()

	if rep.Purged != 10 {
		t.Fatalf("purged = %d; the ten deletions that completed before the timeout are results", rep.Purged)
	}
	if rep.FailureCode != errs.CodeModelUnavailable {
		t.Fatalf("failure_code = %q; an unanswered request is not a clean run", rep.FailureCode)
	}
	if rep.ReviewUnknownHeld == 0 {
		t.Fatal("the unanswered request was not counted anywhere in the report")
	}
	// The eleventh file is untouched.
	left := 0
	for _, name := range names {
		if e.exists("timeout", name) {
			left++
		}
	}
	if left != 1 {
		t.Fatalf("%d files remain, want exactly the one whose review never answered", left)
	}

	// The unanswered request is recorded, not just counted.
	open, err := e.st.ListReviewUnknown(context.Background(), false, 10)
	if err != nil {
		t.Fatalf("list unresolved: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("unresolved requests = %d, want 1", len(open))
	}
	if open[0].Attempts < 1 || open[0].CostStatus != review.CostUnknown {
		t.Fatalf("the record does not say a request was sent with unknown cost: %+v", open[0])
	}
}

// TestAnUnansweredRequestIsNotAskedAgain is V3.
//
// The same state directory, a second run. The valid-keep cache deliberately
// holds only decisions, so before this change the unresolved request was asked
// again and paid for again. The second run must make no new request for it, and
// the fact must survive the run that does nothing about it.
func TestAnUnansweredRequestIsNotAskedAgain(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("retry", domain.KindManagedDisposable)
	e.writeFile("retry", "a.zip", "discard-payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "retry-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 5, 1<<30)
	e.grant(r, domain.CapAutoPurge, 5, 1<<30)
	e.savePolicy(directDeletePolicy(sc.ID))
	judge := &scriptedJudge{allow: 0}
	e.wireReview(judge)

	first := e.runOnce()
	if first.FailureCode != errs.CodeModelUnavailable {
		t.Fatalf("first run failure_code = %q", first.FailureCode)
	}
	before := judge.Calls()
	if before == 0 {
		t.Fatal("the fixture was never asked; the test proves nothing")
	}

	second := e.runOnce()
	if got := judge.Calls(); got != before {
		t.Fatalf("the second run made %d new request(s); an unresolved request must not be paid for twice", got-before)
	}
	if second.ReviewUnknownHeld == 0 {
		t.Fatal("the second run did not report that it held back an unresolved request")
	}
	if !e.exists("retry", "a.zip") {
		t.Fatal("the file was moved while its review was unresolved")
	}
	open, err := e.st.ListReviewUnknown(context.Background(), false, 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("unresolved requests after the second run = %d (%v); the fact must survive", len(open), err)
	}

	// A person resolves it; only then may one more request be made.
	if err := e.st.ResolveReviewUnknown(context.Background(), open[0].ItemID, open[0].Action, "checked the provider dashboard", e.clock.Wall().UnixNano()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	third := e.runOnce()
	if judge.Calls() == before {
		t.Fatal("after an explicit resolution the candidate was still never asked; the hold is permanent")
	}
	if third.ReviewUnknownHeld != 0 && judge.Calls() == before {
		t.Fatal("the run still held the object after it was resolved")
	}
}

// TestBatchBudgetStopsBeforeSending is V2.
//
// A batch whose allowance is gone must not dispatch a request at all, and the
// scope that follows must not report the shortfall as a provider outage.
func TestBatchBudgetStopsBeforeSending(t *testing.T) {
	e := newEnv(t)
	first := e.addScope("budget-a", domain.KindManagedDisposable)
	second := e.addScope("budget-b", domain.KindManagedDisposable)
	for _, sc := range []struct {
		name string
		id   domain.ScopeID
	}{{"budget-a", first.ID}, {"budget-b", second.ID}} {
		e.writeFile(sc.name, "old.zip", "discard-payload", 40*24*time.Hour)
		e.scan(sc.id)
		r := e.rule(sc.id, sc.name+"-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
		e.grant(r, domain.CapAutoQuarantine, 5, 1<<30)
		e.grant(r, domain.CapAutoPurge, 5, 1<<30)
		e.savePolicy(scopePolicy(first.ID, second.ID))
	}
	judge := &scriptedJudge{allow: 1}
	svc := e.wireReview(judge)
	// One candidate may be reviewed for the whole run. The second scope must be
	// stopped by that allowance, not by a provider condition.
	svc.MaxCandidates = 1

	rep := e.runOnce()

	if judge.Calls() != 1 {
		t.Fatalf("HTTP attempts = %d, want exactly 1: the second scope must not be sent", judge.Calls())
	}
	if rep.FailureCode == errs.CodeModelUnavailable {
		t.Fatal("an exhausted allowance was reported as a provider outage")
	}
	// The scope that found the allowance gone stops the run rather than letting
	// the next one ask again, and it does not report a provider condition.
	if rep.Control != domain.ControlStopRun {
		t.Fatalf("control = %q; an exhausted task allowance must stop the run", rep.Control)
	}
	for _, s := range rep.Scopes {
		if s.FailureCode == errs.CodeModelUnavailable {
			t.Fatalf("scope %s reports MODEL_UNAVAILABLE although no request was sent", s.Scope)
		}
		if s.FailureCode == errs.CodeBudgetExhausted && s.NotStarted != 1 {
			t.Fatalf("scope %s stopped on a budget without recording the review that never started", s.Scope)
		}
	}
}

// scopePolicy confirms several scopes for direct deletion.
func scopePolicy(ids ...domain.ScopeID) cleanup.Policy {
	pol := directDeletePolicy(ids[0])
	pol.Scopes = nil
	for _, id := range ids {
		pol.Scopes = append(pol.Scopes, string(id))
	}
	return pol
}
