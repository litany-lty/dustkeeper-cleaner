package rescue

import "errors"

// TestHookListAfter is consulted before each batch of a directory listing. It is
// nil in production, so the whole cost is one branch per batch.
//
// The hook exists because "the directory stopped answering halfway" is a real
// filesystem behaviour that cannot be produced on demand inside a test: the
// failure this covers is that a half-read listing was reported as a complete one,
// which turned every object that had not been reached yet into "gone".
var TestHookListAfter func(rel string, seen int) error

// listFault consults the hook, if any.
func listFault(rel string, seen int) error {
	if f := TestHookListAfter; f != nil {
		return f(rel, seen)
	}
	return nil
}

// ErrListingStopped names the injected interruption.
var ErrListingStopped = errors.New("listing stopped by the test hook")
