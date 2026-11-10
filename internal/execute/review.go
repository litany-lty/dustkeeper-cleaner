package execute

import (
	"context"
	"encoding/json"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// ReviewFunc is the dual-review entry point the executor calls.
//
// It takes a fully measured Request and returns the outcome: the models' answer
// plus the disposition the whole set of facts permits. The executor never
// receives a bare "allow" flag, because a boolean cannot say whether it was the
// answer to "may this be deleted" or to "may this be moved".
type ReviewFunc func(context.Context, review.Request) (review.Outcome, error)

// ReviewBudget is the allowance one batch of reviews may spend.
//
// It is a separate interface from ReviewFunc because the batch is a different
// object from a single question: the executor opens one batch per plan, and the
// reviewer decides whether the batch, the task, or neither has room. Passing the
// remaining task time as a closure keeps a single monotonic source of truth -
// a batch can spend less than the task allows, never more.
type ReviewBudget interface {
	BeginBatch(remaining func() time.Duration)
	CheckStart() string
	EndBatch()
}

// beginBatch opens the review allowance for one plan. A nil budget means the
// reviewer owns its own limits, which is the shape of the debug commands.
func (e *Executor) beginBatch(b *domain.Budget) func() {
	if e.ReviewBudget == nil {
		return func() {}
	}
	var remaining func() time.Duration
	if b != nil && b.Run != nil {
		ledger := b.Run
		remaining = func() time.Duration { return ledger.Remaining(e.Clock.Mono()) }
	}
	e.ReviewBudget.BeginBatch(remaining)
	return e.ReviewBudget.EndBatch
}

// reviewAllowance reports which allowance, if any, stops the next candidate.
func (e *Executor) reviewAllowance() string {
	if e.ReviewBudget == nil {
		return ""
	}
	return e.ReviewBudget.CheckStart()
}

// reviewItem asks the reviewers about one already-measured candidate.
//
// A keep verdict is not an error: it is a result. Only the conditions that make
// asking impossible at all - a pause request, a missing reviewer - are returned
// as errors, and those carry their own codes so a batch can tell "the scope is
// paused" from "this file stays".
func (e *Executor) reviewItem(ctx context.Context, req review.Request) (review.Outcome, error) {
	paused, err := e.Gate.Paused()
	if err != nil {
		return review.Outcome{}, err
	}
	if paused {
		return review.Outcome{}, errs.New(errs.CodePaused, "a pause was requested; no model review was started")
	}
	if e.Review == nil {
		return review.Outcome{}, errs.New(errs.CodeReviewRequired, "dual review is required; no file action was started")
	}
	out, err := e.Review(ctx, req)
	if err == nil {
		out.Explanation = review.NewExplanation(req, out.Decision, out.Reason)
	}
	return out, err
}

// decodeToken reads the change token a plan bound.
func decodeToken(it store.Item) (domain.ChangeToken, error) {
	var tok domain.ChangeToken
	if err := json.Unmarshal([]byte(it.PreTokenJSON), &tok); err != nil {
		return tok, errs.Newf(errs.CodeStateWriteFailed, "change token is unreadable: %v", err)
	}
	return tok, nil
}

// ageDays is how old the file is by modification time, which is the only age
// the program can measure. It is reported as such and never as "unused since".
func ageDays(tok domain.ChangeToken, now time.Time) int {
	if tok.MTimeNS <= 0 {
		return 0
	}
	d := now.Sub(time.Unix(0, tok.MTimeNS))
	if d < 0 {
		return 0
	}
	return int(d / (24 * time.Hour))
}
