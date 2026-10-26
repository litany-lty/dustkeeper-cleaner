// Package errs defines the stable error codes and exit statuses from section
// 5.5. Callers branch on the code, never on message text.
package errs

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error identifier.
type Code string

// Codes used across the CLI. The exit status of each is fixed in exitCodes.
const (
	CodeOK = Code("OK")

	// 2: arguments, DSL or semantics.
	CodeInvalidArgument = Code("INVALID_ARGUMENT")
	CodeUnknownField    = Code("UNKNOWN_FIELD")
	CodeDuplicateKey    = Code("DUPLICATE_KEY")
	CodeRuleAmbiguous   = Code("RULE_AMBIGUOUS")
	CodeRuleUnsupported = Code("RULE_UNSUPPORTED")
	CodeDSLInvalid      = Code("DSL_INVALID")
	CodeNeedsInput      = Code("NEEDS_INPUT")

	// 3: another task holds the instance lock.
	CodeInstanceBusy = Code("INSTANCE_BUSY")

	// 4: plan, scope or approval changed under us.
	CodePlanStale      = Code("PLAN_STALE")
	CodePlanExpired    = Code("PLAN_EXPIRED")
	CodeScopeChanged   = Code("SCOPE_CHANGED")
	CodeDigestMismatch = Code("DIGEST_MISMATCH")
	CodeScopeNotFound  = Code("SCOPE_NOT_FOUND")
	CodePlanNotFound   = Code("PLAN_NOT_FOUND")
	CodeItemNotFound   = Code("ITEM_NOT_FOUND")
	CodeRuleNotFound   = Code("RULE_NOT_FOUND")
	CodeGrantRequired  = Code("GRANT_REQUIRED")
	CodePlanNotReady   = Code("PLAN_NOT_READY")

	// 5: capability, permission or filesystem support.
	CodeUnsupportedFS       = Code("UNSUPPORTED_FS")
	CodeAccessDenied        = Code("ACCESS_DENIED")
	CodeUnsupportedMetadata = Code("UNSUPPORTED_METADATA")
	CodeScopeRefused        = Code("SCOPE_REFUSED")
	CodeScopeOverlap        = Code("SCOPE_OVERLAP")
	CodeUnsupportedSnapshot = Code("UNSUPPORTED_SNAPSHOT")

	// 6: needs reconciliation, conflict or safety hold.
	CodeRecoveryRequired          = Code("RECOVERY_REQUIRED")
	CodeRestoreConflict           = Code("RESTORE_CONFLICT")
	CodeHeld                      = Code("HELD")
	CodeClockUntrusted            = Code("CLOCK_UNTRUSTED")
	CodeProtectionAliasUnresolved = Code("PROTECTION_ALIAS_UNRESOLVED")
	CodePaused                    = Code("PAUSED")
	CodePauseRequested            = Code("PAUSE_REQUESTED")
	CodeKeeperMissing             = Code("KEEPER_MISSING")

	// 7: storage or durability.
	CodeDurabilityFailed = Code("DURABILITY_FAILED")
	CodeStateWriteFailed = Code("STATE_WRITE_FAILED")
	CodeIOError          = Code("IO_ERROR")
	CodeDBError          = Code("DB_ERROR")
	CodeMigrationTooNew  = Code("MIGRATION_TOO_NEW")
	CodeDiskPressure     = Code("DISK_PRESSURE")

	// 8: model unavailable; existing rules are unaffected.
	CodeModelUnavailable = Code("MODEL_UNAVAILABLE")

	// 9: a plan completed only partly.
	CodePartial = Code("PARTIAL")

	// 130: cancelled.
	CodeCancelled = Code("CANCELLED")

	// Non-blocking informational outcomes.
	CodeNoCandidates     = Code("NO_CANDIDATES")
	CodeSkippedProtected = Code("SKIPPED_PROTECTED")
	CodeDeepScanRequired = Code("DEEP_SCAN_REQUIRED")
	CodeBudgetExhausted  = Code("BUDGET_EXHAUSTED")
)

// Status is the coarse outcome reported in JSON output.
type Status string

// Statuses allowed by the output contract.
const (
	StatusOK         = Status("ok")
	StatusNeedsInput = Status("needs_input")
	StatusBlocked    = Status("blocked")
	StatusPartial    = Status("partial")
	StatusError      = Status("error")
)

var exitCodes = map[Code]int{
	CodeOK: 0,

	CodeInvalidArgument: 2,
	CodeUnknownField:    2,
	CodeDuplicateKey:    2,
	CodeRuleAmbiguous:   2,
	CodeRuleUnsupported: 2,
	CodeDSLInvalid:      2,
	CodeNeedsInput:      2,

	CodeInstanceBusy: 3,

	CodePlanStale:      4,
	CodePlanExpired:    4,
	CodeScopeChanged:   4,
	CodeDigestMismatch: 4,
	CodeScopeNotFound:  4,
	CodePlanNotFound:   4,
	CodeItemNotFound:   4,
	CodeRuleNotFound:   4,
	CodeGrantRequired:  4,
	CodePlanNotReady:   4,

	CodeUnsupportedFS:       5,
	CodeAccessDenied:        5,
	CodeUnsupportedMetadata: 5,
	CodeScopeRefused:        5,
	CodeScopeOverlap:        5,
	CodeUnsupportedSnapshot: 5,

	CodeRecoveryRequired:          6,
	CodeRestoreConflict:           6,
	CodeHeld:                      6,
	CodeClockUntrusted:            6,
	CodeProtectionAliasUnresolved: 6,
	CodePaused:                    6,
	CodePauseRequested:            6,
	CodeKeeperMissing:             6,

	CodeDurabilityFailed: 7,
	CodeStateWriteFailed: 7,
	CodeIOError:          7,
	CodeDBError:          7,
	CodeMigrationTooNew:  7,
	CodeDiskPressure:     7,

	CodeModelUnavailable: 8,

	CodePartial: 9,

	CodeCancelled: 130,

	CodeNoCandidates:     0,
	CodeSkippedProtected: 0,
	CodeDeepScanRequired: 0,
	CodeBudgetExhausted:  0,
}

var retryable = map[Code]bool{
	CodeInstanceBusy:     true,
	CodeModelUnavailable: true,
	CodeDBError:          true,
	CodeIOError:          true,
	CodeBudgetExhausted:  true,
	CodePauseRequested:   true,
}

var statuses = map[Code]Status{
	CodeOK:               StatusOK,
	CodeNoCandidates:     StatusOK,
	CodeSkippedProtected: StatusOK,
	CodeDeepScanRequired: StatusOK,
	CodeBudgetExhausted:  StatusOK,

	CodeNeedsInput:    StatusNeedsInput,
	CodeRuleAmbiguous: StatusNeedsInput,

	CodePartial: StatusPartial,

	CodePauseRequested: StatusBlocked,
	CodePaused:         StatusBlocked,
}

// Error is a coded, user-facing failure.
type Error struct {
	Code      Code
	Message   string
	Retryable bool
	Cause     error
}

// New builds an error with a fixed message.
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg, Retryable: retryable[code]}
}

// Newf builds an error with a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Retryable: retryable[code]}
}

// Wrap attaches a code to an underlying error, returning an untyped nil when
// err is nil.
//
// The return type is error on purpose: returning *Error would turn a nil
// pointer into a non-nil interface value at every `return errs.Wrap(...)` site,
// which is how a successful call gets reported as a failure.
func Wrap(code Code, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: err.Error(), Retryable: retryable[code], Cause: err}
}

// Wrapf attaches a code and a cause to a formatted message. The cause is kept
// so callers can still use errors.Is/As on the result; Newf deliberately does
// not accept a cause, and must never be given a %w directive.
func Wrapf(code Code, cause error, format string, args ...any) error {
	return &Error{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Retryable: retryable[code],
		Cause:     cause,
	}
}

func (e *Error) Error() string {	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap exposes the cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Cause }

// Exit returns the process exit status for a code.
func Exit(code Code) int {
	if v, ok := exitCodes[code]; ok {
		return v
	}
	return 1
}

// RetryableCode reports whether retrying the same request may succeed.
func RetryableCode(code Code) bool { return retryable[code] }

// StatusOf maps a code to the JSON status string.
func StatusOf(code Code) Status {
	if s, ok := statuses[code]; ok {
		return s
	}
	if code == CodeOK {
		return StatusOK
	}
	return StatusError
}

// CodeOf extracts a code from any error, defaulting to a generic failure.
func CodeOf(err error) Code {
	if err == nil {
		return CodeOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeIOError
}

// ExitOf returns the exit status for an arbitrary error.
func ExitOf(err error) int {
	if err == nil {
		return 0
	}
	return Exit(CodeOf(err))
}
