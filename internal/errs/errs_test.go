package errs

import (
	"errors"
	"fmt"
	"testing"
)

// allCodes is the complete list from the const block. Keeping it explicit is the
// point: adding a code without an exit status or a status mapping fails here
// rather than at the moment an operator sees an empty field in the JSON.
var allCodes = []Code{
	CodeOK,
	CodeInvalidArgument, CodeUnknownField, CodeDuplicateKey, CodeRuleAmbiguous,
	CodeRuleUnsupported, CodeDSLInvalid, CodeNeedsInput,
	CodeInstanceBusy,
	CodePlanStale, CodePlanExpired, CodeScopeChanged, CodeDigestMismatch,
	CodeScopeNotFound, CodePlanNotFound, CodeItemNotFound, CodeRuleNotFound,
	CodeGrantRequired, CodePlanNotReady,
	CodeUnsupportedFS, CodeAccessDenied, CodeUnsupportedMetadata,
	CodeScopeRefused, CodeScopeOverlap, CodeUnsupportedSnapshot,
	CodeRecoveryRequired, CodeRestoreConflict, CodeHeld, CodeClockUntrusted,
	CodeProtectionAliasUnresolved, CodePaused, CodePauseRequested, CodeKeeperMissing,
	CodeDurabilityFailed, CodeStateWriteFailed, CodeIOError, CodeDBError,
	CodeMigrationTooNew, CodeMigrationTooOld, CodeDiskPressure,
	CodeModelUnavailable,
	CodePartial,
	CodeCancelled,
	CodeNoCandidates, CodeSkippedProtected, CodeDeepScanRequired, CodeBudgetExhausted,
}

// documented maps each code to the exit status published in README and helpText.
var documented = map[Code]int{
	CodeOK: 0,

	CodeInvalidArgument: 2, CodeUnknownField: 2, CodeDuplicateKey: 2,
	CodeRuleAmbiguous: 2, CodeRuleUnsupported: 2, CodeDSLInvalid: 2, CodeNeedsInput: 2,

	CodeInstanceBusy: 3,

	CodePlanStale: 4, CodePlanExpired: 4, CodeScopeChanged: 4, CodeDigestMismatch: 4,
	CodeScopeNotFound: 4, CodePlanNotFound: 4, CodeItemNotFound: 4,
	CodeRuleNotFound: 4, CodeGrantRequired: 4, CodePlanNotReady: 4,

	CodeUnsupportedFS: 5, CodeAccessDenied: 5, CodeUnsupportedMetadata: 5,
	CodeScopeRefused: 5, CodeScopeOverlap: 5, CodeUnsupportedSnapshot: 5,

	CodeRecoveryRequired: 6, CodeRestoreConflict: 6, CodeHeld: 6,
	CodeClockUntrusted: 6, CodeProtectionAliasUnresolved: 6, CodePaused: 6,
	CodePauseRequested: 6, CodeKeeperMissing: 6,

	CodeDurabilityFailed: 7, CodeStateWriteFailed: 7, CodeIOError: 7,
	CodeDBError: 7, CodeMigrationTooNew: 7, CodeMigrationTooOld: 7, CodeDiskPressure: 7,

	CodeModelUnavailable: 8,
	CodePartial:          9,
	CodeCancelled:        130,

	CodeNoCandidates: 0, CodeSkippedProtected: 0, CodeDeepScanRequired: 0,
	CodeBudgetExhausted: 0,
}

func TestEveryCodeHasAnExitStatus(t *testing.T) {
	for _, c := range allCodes {
		if _, ok := exitCodes[c]; !ok {
			t.Errorf("%s has no exit status", c)
		}
		if _, ok := documented[c]; !ok {
			t.Errorf("%s is not in the documented table", c)
		}
		if got, want := Exit(c), documented[c]; got != want {
			t.Errorf("Exit(%s) = %d, want %d", c, got, want)
		}
	}
	if len(exitCodes) != len(allCodes) {
		t.Errorf("exitCodes has %d entries, the code list has %d", len(exitCodes), len(allCodes))
	}
}

func TestEveryCodeHasAStatus(t *testing.T) {
	valid := map[Status]bool{
		StatusOK: true, StatusNeedsInput: true, StatusBlocked: true,
		StatusPartial: true, StatusError: true,
	}
	for _, c := range allCodes {
		s := StatusOf(c)
		if !valid[s] {
			t.Errorf("StatusOf(%s) = %q, which is not one of the five statuses", c, s)
		}
	}
	// A clarification is not an error, and a pause is not a failure.
	if StatusOf(CodeNeedsInput) != StatusNeedsInput {
		t.Errorf("NEEDS_INPUT must report status needs_input")
	}
	if StatusOf(CodeRuleAmbiguous) != StatusNeedsInput {
		t.Errorf("RULE_AMBIGUOUS must report status needs_input")
	}
	if StatusOf(CodePaused) != StatusBlocked || StatusOf(CodePauseRequested) != StatusBlocked {
		t.Errorf("pause conditions must report status blocked")
	}
	if StatusOf(CodePartial) != StatusPartial {
		t.Errorf("PARTIAL must report status partial")
	}
	// Informational outcomes are successes, not failures.
	for _, c := range []Code{CodeNoCandidates, CodeSkippedProtected, CodeDeepScanRequired, CodeBudgetExhausted} {
		if StatusOf(c) != StatusOK || Exit(c) != 0 {
			t.Errorf("%s is informational and must not fail the run", c)
		}
	}
}

func TestUnknownCodeIsNotSilentlyMapped(t *testing.T) {
	bogus := Code("NOT_A_REAL_CODE")
	if Exit(bogus) != 1 {
		t.Errorf("Exit of an unknown code = %d, want 1", Exit(bogus))
	}
	if StatusOf(bogus) != StatusError {
		t.Errorf("StatusOf of an unknown code = %q, want error", StatusOf(bogus))
	}
}

func TestRetryableIsDeliberate(t *testing.T) {
	// Transient conditions are retryable; anything that needs a human to change
	// something is not.
	for _, c := range []Code{CodeInstanceBusy, CodeModelUnavailable, CodeDBError, CodeIOError, CodeBudgetExhausted, CodePauseRequested} {
		if !RetryableCode(c) {
			t.Errorf("%s should be retryable", c)
		}
	}
	for _, c := range []Code{CodeInvalidArgument, CodePlanStale, CodeHeld, CodeRestoreConflict, CodeAccessDenied, CodeScopeChanged} {
		if RetryableCode(c) {
			t.Errorf("%s must not claim that retrying helps", c)
		}
	}
}

func TestCodeOfExtractsTheCode(t *testing.T) {
	if CodeOf(nil) != CodeOK {
		t.Error("CodeOf(nil) must be OK")
	}
	if CodeOf(errors.New("plain")) != CodeIOError {
		t.Error("an uncoded error must be reported as IO_ERROR, never as OK")
	}
	wrapped := fmt.Errorf("outer: %w", New(CodePlanStale, "the plan moved on"))
	if CodeOf(wrapped) != CodePlanStale {
		t.Errorf("CodeOf through a wrap = %s", CodeOf(wrapped))
	}
	if ExitOf(wrapped) != 4 {
		t.Errorf("ExitOf through a wrap = %d", ExitOf(wrapped))
	}
	if ExitOf(nil) != 0 {
		t.Error("ExitOf(nil) must be 0")
	}
}

// TestWrapOfNilIsNil is the typed-nil trap: returning *Error from a function
// typed error turns nil into a non-nil interface, which is how a success gets
// reported as a failure.
func TestWrapOfNilIsNil(t *testing.T) {
	var err error = Wrap(CodeIOError, nil)
	if err != nil {
		t.Fatalf("Wrap(nil) = %v; a wrapped nil must stay nil", err)
	}
}

func TestErrorMessageIsReadable(t *testing.T) {
	e := New(CodeHeld, "the object appears twice")
	if got, want := e.Error(), "HELD: the object appears twice"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// A nil *Error must not panic when formatted through a nil interface check.
	var nilErr *Error
	if s := nilErr.Error(); s != "" {
		t.Errorf("nil Error() = %q", s)
	}
	// An empty message falls back to the code, never to an empty string.
	if got := New(CodePaused, "").Error(); got != "PAUSED" {
		t.Errorf("empty message Error() = %q", got)
	}
}

func TestMessageOfSurvivesWrapping(t *testing.T) {
	cause := errors.New("disk went away")
	e := Wrap(CodeDurabilityFailed, cause)
	if !errors.Is(e, cause) {
		t.Error("the cause must stay reachable through errors.Is")
	}
	var target *Error
	if !errors.As(e, &target) || target.Code != CodeDurabilityFailed {
		t.Error("errors.As must find the coded error")
	}
}
