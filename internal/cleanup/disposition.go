package cleanup

import "dustkeeper/internal/outcome"

// The three-outcome mapping itself lives in internal/outcome, so that the
// review client and the cleanup policy can share one implementation without
// importing each other. Everything below is a re-export: the names a caller
// uses stay where the task described them, and there is exactly one switch
// statement behind them.

// Reason codes for a keep or a move.
const (
	ReasonReviewUnavailable    = outcome.ReasonReviewUnavailable
	ReasonFactsMissing         = outcome.ReasonFactsMissing
	ReasonModelSaysKeep        = outcome.ReasonModelSaysKeep
	ReasonMoveSafetyUnknown    = outcome.ReasonMoveSafetyUnknown
	ReasonNoAuthority          = outcome.ReasonNoAuthority
	ReasonNotEnoughForDeletion = outcome.ReasonNotEnoughForDeletion
	ReasonSafeToObserve        = outcome.ReasonSafeToObserve
	ReasonDualReviewAgrees     = outcome.ReasonDualReviewAgrees
)

// ReasonText renders a reason code for the result page.
func ReasonText(reason string) string { return outcome.ReasonText(reason) }

// Inputs is everything the disposition depends on.
type Inputs = outcome.Inputs

// Decide maps a reviewed state onto exactly one disposition.
func Decide(in Inputs) (disposition, reason string) { return outcome.Decide(in) }
