package review

import (
	"context"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/outcome"
)

// Triage is the single entry point the executor uses.
//
// It runs the review and then maps the models' answer together with the local
// facts onto exactly one of three dispositions. Doing the mapping here, and
// only here, means a caller cannot obtain an approval for an action the
// disposition does not permit: when the outcome is keep, Outcome.Approval is
// nil, so "the model said yes but nobody authorised it" cannot be turned into a
// file action by a caller that only looks at the approval.
func (s *Service) Triage(ctx context.Context, r Request) (Outcome, error) {
	d, ap, err := s.Review(ctx, r)
	if err != nil {
		return Outcome{}, err
	}
	disposition, reason := outcome.Decide(outcome.Inputs{
		Action:        r.Action,
		Verdict:       d.Verdict,
		ModelFailed:   d.ModelError != "",
		FactsComplete: r.FactsComplete,
		Movable:       r.Movable,
		Authorized:    r.Authorized,
	})
	out := Outcome{Decision: d, Disposition: disposition, Reason: reason}
	if disposition != domain.DispositionKeep {
		out.Approval = ap
	}
	// A caller asks for an action; if the disposition permits a different one
	// (delete asked, quarantine concluded), the approval must already be the
	// permission for that other action. Review's permitFor guarantees it; this
	// is the check that keeps the guarantee honest if it ever stops holding.
	if out.Approval != nil && out.Approval.Disposition() != disposition {
		return Outcome{}, errs.Newf(errs.CodeReviewRequired,
			"the review approved %q while the disposition is %q; nothing was done",
			out.Approval.Disposition(), disposition)
	}
	return out, nil
}
