package execute

import "dustkeeper/internal/errs"

// recordOr reports the outcome of an operation together with the possibility
// that the outcome could not be written down.
//
// Every action here ends by recording what happened. When that write fails, the
// database still says "this operation is pending", and reconciliation will meet
// it again. Returning the original error alone would describe a decision that
// only exists in memory; returning the storage failure alone would hide why the
// attempt stopped. This keeps both, and the storage code is the one the caller
// acts on, because a batch that cannot record its work must stop.
func recordOr(recordErr, cause error) error {
	switch {
	case recordErr == nil:
		return cause
	case cause == nil:
		return errs.Newf(errs.CodeStateWriteFailed,
			"the outcome of the operation could not be recorded: %v; it stays pending for `recover reconcile`", recordErr)
	default:
		return errs.Newf(errs.CodeStateWriteFailed,
			"%v; the outcome could not be recorded: %v; it stays pending for `recover reconcile`", cause, recordErr)
	}
}
