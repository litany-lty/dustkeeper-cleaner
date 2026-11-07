package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/outcome"
)

// These tests pin the budget hierarchy and the request facts. They use a judge
// that is the only fixture: everything else is the real service.

// blockingJudge records attempts and waits, so a test can spend a batch's time
// without a network.
type blockingJudge struct {
	attempts int
	wait     time.Duration
	err      error
	decision Decision
}

func (j *blockingJudge) Review(ctx context.Context, _ Facts) (Decision, error) {
	j.attempts++
	if j.wait > 0 {
		select {
		case <-time.After(j.wait):
		case <-ctx.Done():
			return Decision{Verdict: domain.VerdictKeep, ModelError: ctx.Err().Error()}, ctx.Err()
		}
	}
	if j.err != nil {
		return Decision{Verdict: domain.VerdictKeep, ModelError: j.err.Error()}, j.err
	}
	return j.decision, nil
}

func (j *blockingJudge) Stats() Attempts {
	return Attempts{JEV: j.attempts, MiniMax: j.attempts, Attempts: j.attempts * 2}
}

func testRequest() Request {
	return Request{Action: domain.DispositionDelete, ScopeID: "s", ItemID: "i", RelativePath: "a.zip",
		Category: "archive", SizeBytes: 10, FactsComplete: true, Movable: true, Authorized: true}
}

// TestABatchThatCannotFitTheNextCandidateDoesNotStartIt is V5.
//
// The candidate that does not fit is not started, and the one that did fit is
// recorded as answered. A batch that started work it could not finish would
// leave a request it must abandon after paying for it.
func TestABatchThatCannotFitTheNextCandidateDoesNotStartIt(t *testing.T) {
	judge := &blockingJudge{wait: 150 * time.Millisecond,
		decision: Decision{Verdict: domain.VerdictKeep, Reason: "model_says_keep"}}
	// One second of batch, 100 ms of reserve, and an 800 ms worst-case path: the
	// first candidate fits, and after it has taken 150 ms a second full path no
	// longer does.
	svc := &Service{Judge: judge, MaxCandidates: 10,
		Defaults: Limits{Candidates: 10, Attempts: 20, Seconds: 1,
			Reserve: 100 * time.Millisecond, PerCandidate: 800 * time.Millisecond}}
	svc.BeginBatch(Limits{})

	if d, _, err := svc.Review(context.Background(), testRequest()); err != nil || d.Failure != "" {
		t.Fatalf("the first candidate did not fit its own batch: %+v %v", d, err)
	}
	d, _, err := svc.Review(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("a batch that stops is not an error: %v", err)
	}
	if d.Failure != FailureBatchBudget {
		t.Fatalf("failure = %q, want %q; the second candidate does not fit the remaining batch", d.Failure, FailureBatchBudget)
	}
	if d.Attempts != 0 {
		t.Fatalf("attempts = %d for a candidate that was never sent", d.Attempts)
	}
	if d.CostStatus != CostNotSent {
		t.Fatalf("cost = %q for a candidate that was never sent", d.CostStatus)
	}
	if got := judge.attempts; got != 1 {
		t.Fatalf("the judge was asked %d time(s), want 1", got)
	}
}

// TestARunThatExhaustedItsTaskAllowanceStopsSending is V2's task half.
func TestARunThatExhaustedItsTaskAllowanceStopsSending(t *testing.T) {
	judge := &blockingJudge{decision: Decision{Verdict: domain.VerdictKeep}}
	svc := &Service{Judge: judge, MaxCandidates: 1,
		Defaults: Limits{Candidates: 10, Attempts: 20, Seconds: 60,
			Reserve: time.Second, PerCandidate: 40 * time.Second}}
	svc.BeginBatch(Limits{RunRemaining: func() time.Duration { return 300 * time.Second }})

	if d, _, _ := svc.Review(context.Background(), testRequest()); d.Failure != "" {
		t.Fatalf("first review failed: %+v", d)
	}
	d, _, err := svc.Review(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("an exhausted task allowance is not an error: %v", err)
	}
	if d.Failure != FailureRunBudget {
		t.Fatalf("failure = %q, want %q", d.Failure, FailureRunBudget)
	}
	if judge.attempts != 1 {
		t.Fatalf("the judge was asked %d time(s); the second candidate must not be sent", judge.attempts)
	}
}

// TestAShortTaskSliceStopsBeforeSendingAndSaysSo is the consistency half: a task
// with less time left than one review path cannot start another candidate.
func TestAShortTaskSliceStopsBeforeSendingAndSaysSo(t *testing.T) {
	judge := &blockingJudge{decision: Decision{Verdict: domain.VerdictKeep}}
	svc := &Service{Judge: judge, MaxCandidates: 10,
		Defaults: Limits{Candidates: 5, Attempts: 10, Seconds: 180,
			Reserve: 10 * time.Second, PerCandidate: 60 * time.Second}}
	svc.BeginBatch(Limits{RunRemaining: func() time.Duration { return 30 * time.Second }})

	d, _, _ := svc.Review(context.Background(), testRequest())
	if d.Failure != FailureRunBudget {
		t.Fatalf("failure = %q; 30 seconds cannot contain a 60-second path plus reserve", d.Failure)
	}
	if judge.attempts != 0 {
		t.Fatalf("the judge was asked %d time(s); nothing fits, so nothing may be sent", judge.attempts)
	}
	if _, reason := outcome.Decide(outcome.Inputs{Failure: d.Failure, Attempted: d.Attempted(),
		ModelFailed: true, FactsComplete: true, Movable: true, Authorized: true,
		Action: domain.DispositionDelete}); reason != outcome.ReasonNotStartedBudget {
		t.Fatalf("reason = %q, want %q", reason, outcome.ReasonNotStartedBudget)
	}
}

// TestAnUnansweredRequestKeepsItsAttemptsAndUnknownCost is V4.
//
// The request reached the provider and no answer came back. The decision has to
// carry the attempt and say the cost is unknown; the file is kept and nothing
// claims the request was free.
func TestAnUnansweredRequestKeepsItsAttemptsAndUnknownCost(t *testing.T) {
	judge := &blockingJudge{err: context.DeadlineExceeded}
	svc := &Service{Judge: judge, MaxCandidates: 10,
		Defaults: Limits{Candidates: 5, Attempts: 10, Seconds: 180,
			Reserve: 10 * time.Second, PerCandidate: 60 * time.Second}}
	svc.BeginBatch(Limits{})

	d, ap, err := svc.Review(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if ap != nil {
		t.Fatal("an unanswered review authorised an action")
	}
	if d.Attempts != 2 || !d.JEVSent || !d.LLMSent {
		t.Fatalf("request facts lost: attempts=%d jev=%v minimax=%v", d.Attempts, d.JEVSent, d.LLMSent)
	}
	if d.Failure != FailureTimeout {
		t.Fatalf("failure = %q, want %q", d.Failure, FailureTimeout)
	}
	if d.CostStatus != CostUnknown {
		t.Fatalf("cost = %q; a sent request without usage is unknown, never free", d.CostStatus)
	}
	if d.Verdict != domain.VerdictKeep {
		t.Fatalf("verdict = %q; a failed review is never an approval", d.Verdict)
	}
	if _, reason := outcome.Decide(outcome.Inputs{Failure: d.Failure, Attempted: d.Attempted(),
		ModelFailed: true, FactsComplete: true, Movable: true, Authorized: true,
		Action: domain.DispositionDelete}); reason != outcome.ReasonReviewUnknown {
		t.Fatalf("reason = %q, want %q", reason, outcome.ReasonReviewUnknown)
	}
}

// TestACandidateNeedsTwoAttemptsBeforeItStarts pins the "reserve the whole
// path" rule. A batch configured with one attempt is legal, but it must not
// send the first of two calls: the second is the one that would be abandoned
// after the first had already been paid for.
func TestACandidateNeedsTwoAttemptsBeforeItStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  *Service
		want string
	}{
		{
			name: "batch with one attempt",
			svc: &Service{Judge: &blockingJudge{decision: Decision{Verdict: domain.VerdictKeep}},
				MaxCandidates: 5,
				Defaults: Limits{Candidates: 5, Attempts: 1, Seconds: 180,
					Reserve: time.Second, PerCandidate: 2 * time.Second}},
			want: FailureBatchBudget,
		},
		{
			name: "task with one attempt",
			svc: &Service{Judge: &blockingJudge{decision: Decision{Verdict: domain.VerdictKeep}},
				MaxCandidates: 5, MaxAttempts: 1,
				Defaults: Limits{Candidates: 5, Attempts: 10, Seconds: 180,
					Reserve: time.Second, PerCandidate: 2 * time.Second}},
			want: FailureRunBudget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			judge := tc.svc.Judge.(*blockingJudge)
			tc.svc.BeginBatch(Limits{})
			d, _, err := tc.svc.Review(context.Background(), testRequest())
			if err != nil {
				t.Fatalf("a budget stop is not an error: %v", err)
			}
			if d.Failure != tc.want {
				t.Fatalf("failure = %q, want %q", d.Failure, tc.want)
			}
			if d.Attempts != 0 || d.CostStatus != CostNotSent || judge.attempts != 0 {
				t.Fatalf("a candidate that cannot fit sent a request: attempts=%d cost=%s judge=%d",
					d.Attempts, d.CostStatus, judge.attempts)
			}
		})
	}

	// Two attempts are enough for exactly one candidate, and the next one is
	// refused without another call.
	judge := &blockingJudge{decision: Decision{Verdict: domain.VerdictKeep, Reason: "model_says_keep"}}
	svc := &Service{Judge: judge, MaxCandidates: 5,
		Defaults: Limits{Candidates: 5, Attempts: 2, Seconds: 180,
			Reserve: time.Second, PerCandidate: 2 * time.Second}}
	svc.BeginBatch(Limits{})
	if d, _, _ := svc.Review(context.Background(), testRequest()); d.Failure != "" || d.Attempts != 2 {
		t.Fatalf("one full path did not fit in two attempts: %+v", d)
	}
	d, _, _ := svc.Review(context.Background(), testRequest())
	if d.Failure != FailureBatchBudget || d.Attempts != 0 || judge.attempts != 1 {
		t.Fatalf("the next candidate started with no attempt left: %+v (judge %d)", d, judge.attempts)
	}
}

// TestFailureKindsMatchTheOutcomePackage keeps the duplicated spellings equal.
// outcome imports only domain on purpose, so the two packages name the same
// failure; a silent drift would map a budget stop to a provider outage.
func TestFailureKindsMatchTheOutcomePackage(t *testing.T) {
	if outcome.ReviewFailureRunBudget != FailureRunBudget {
		t.Fatalf("run budget: %q vs %q", outcome.ReviewFailureRunBudget, FailureRunBudget)
	}
	if outcome.ReviewFailureBatchBudget != FailureBatchBudget {
		t.Fatalf("batch budget: %q vs %q", outcome.ReviewFailureBatchBudget, FailureBatchBudget)
	}
}

// TestBatchSettingsMustContainOneReviewPath is the configuration half of V5: a
// batch window that cannot hold the path it authorises is refused, not clamped.
func TestBatchSettingsMustContainOneReviewPath(t *testing.T) {
	timeout := 60
	tooShort := 120 // 2*60 + 10 = 130
	s := &Settings{MaxCandidates: 5, JEVURL: "https://127.0.0.1:9/jev", JEVModel: "jev-1.14.0",
		MiniMaxURL: "https://127.0.0.1:9/mm", MiniMaxModel: "MiniMax-M3.1-Flash",
		TimeoutSeconds: &timeout, BatchSeconds: &tooShort}
	if err := s.Validate(); err == nil {
		t.Fatal("a 120-second batch around a 60+60-second double call was accepted")
	}
	wide := 180
	s.BatchSeconds = &wide
	if err := s.Validate(); err != nil {
		t.Fatalf("a consistent batch was refused: %v", err)
	}
	if got := s.Limits().Seconds; got != 180 {
		t.Fatalf("limits seconds = %d, want the configured 180", got)
	}
}

// TestAnUnsetBatchWindowGrowsToFitThePath is the default half: the default is a
// starting point, and it is raised rather than left too small for the timeout a
// caller configured.
func TestAnUnsetBatchWindowGrowsToFitThePath(t *testing.T) {
	timeout := 120
	s := &Settings{MaxCandidates: 5, JEVURL: "https://127.0.0.1:9/jev", JEVModel: "jev-1.14.0",
		MiniMaxURL: "https://127.0.0.1:9/mm", MiniMaxModel: "MiniMax-M3.1-Flash",
		TimeoutSeconds: &timeout}
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := s.Limits().Seconds; got < 250 {
		t.Fatalf("default batch window = %d; two 120-second calls plus reserve do not fit", got)
	}
}

// TestAnErrorBeforeAnyRequestIsNotACostUnknown keeps the two failure shapes
// apart: nothing was sent, so nothing can have been billed.
func TestAnErrorBeforeAnyRequestIsNotACostUnknown(t *testing.T) {
	judge := &blockingJudge{err: errors.New("credentials rejected")}
	svc := &Service{Judge: judge, MaxCandidates: 5}
	d, _, _ := svc.Review(context.Background(), testRequest())
	// This fixture increments its counter even on failure, which is what a real
	// transport attempt does; a configuration error that never reaches the wire
	// reports zero and is covered by the classification below.
	if d.CostStatus == CostUnknown && d.Attempts == 0 {
		t.Fatalf("cost unknown with no attempt: %+v", d)
	}
	if got := classify(errors.New("no send"), 0); got != FailureTransport {
		t.Fatalf("classify(no send) = %q", got)
	}
}
