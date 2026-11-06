package main

import (
	"context"
	"time"

	"dustkeeper/internal/execute"
	"dustkeeper/internal/review"
)

// reviewer owns the one review service a command uses.
//
// Loading credentials is deferred so status/restore/reconcile work without any
// model. The budget half is on the same object because a batch allowance that
// lived somewhere else could be reset by starting a scope: the task's counters
// have to be the same counters the batch draws from.
type reviewer struct {
	app    *app
	svc    *review.Service
	err    error
	loaded bool
}

func (r *reviewer) init() error {
	if r.loaded {
		return r.err
	}
	r.loaded = true
	r.svc, r.err = r.app.cfg.Review.NewService()
	if r.svc != nil {
		r.svc.Now = r.app.clock.Wall
	}
	return r.err
}

// Review is the function the executor calls for each candidate.
//
// Everything the reviewers are told is measured by the executor before this is
// called, so there is no second place where a candidate could acquire a fact it
// does not have.
func (r *reviewer) Review(ctx context.Context, req review.Request) (review.Outcome, error) {
	if err := r.init(); err != nil {
		return review.Outcome{}, err
	}
	return r.svc.Triage(ctx, req)
}

// BeginBatch opens the allowance for one plan. remaining is nil when the caller
// has no task clock, in which case only the batch allowance applies.
func (r *reviewer) BeginBatch(remaining func() time.Duration) {
	if err := r.init(); err != nil || r.svc == nil {
		return
	}
	r.svc.BeginBatch(review.Limits{RunRemaining: remaining})
}

// CheckStart reports which allowance, if any, stops the next candidate.
func (r *reviewer) CheckStart() string {
	if r.svc == nil {
		// Not loaded yet: nothing has been spent, so the first candidate may
		// start. Loading happens in Review.
		return ""
	}
	return r.svc.CheckStart()
}

// EndBatch closes the current allowance.
func (r *reviewer) EndBatch() {
	if r.svc != nil {
		r.svc.EndBatch()
	}
}

// reviewerFunc adapts the object to the executor's function type.
func (r *reviewer) reviewerFunc() execute.ReviewFunc { return r.Review }

// app.reviewer builds the one reviewer a command uses.
func (a *app) reviewer() *reviewer { return &reviewer{app: a} }
