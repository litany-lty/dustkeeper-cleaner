package main

import (
	"context"

	"dustkeeper/internal/execute"
	"dustkeeper/internal/review"
)

// One service per executor bounds calls/reads for the whole command. Loading
// credentials is deferred so status/restore/reconcile work without any model.
//
// The wrapper is deliberately thin: everything the reviewers are told is
// measured by the executor before this is called, so there is no second place
// where a candidate could acquire a fact it does not have.
func (a *app) reviewer() execute.ReviewFunc {
	var svc *review.Service
	var initErr error
	initialized := false
	return func(ctx context.Context, req review.Request) (review.Outcome, error) {
		if !initialized {
			initialized = true
			svc, initErr = a.cfg.Review.NewService()
			if svc != nil {
				svc.Now = a.clock.Wall
			}
		}
		if initErr != nil {
			return review.Outcome{}, initErr
		}
		return svc.Triage(ctx, req)
	}
}
