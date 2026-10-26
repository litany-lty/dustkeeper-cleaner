// Package policy decides whether one indexed path is a candidate, and protects
// paths the program must never touch.
//
// Evaluate is pure: it takes data, returns a verdict with a stable reason code
// and touches nothing. That separation is what lets the same rule be previewed,
// explained, planned and executed with one implementation of the truth.
package policy

import (
	"strings"

	"dustkeeper/internal/rules"
)

// MatchName reports whether name matches pattern under the phase-one pattern
// language: literals, '*' (any run, including empty) and '?' (exactly one).
// Matching is byte-wise and case-sensitive: "*.zip" does not match "A.ZIP".
func MatchName(pattern, name string) bool {
	// Iterative wildcard match with single-star backtracking; no recursion, so a
	// hostile pattern cannot blow the stack.
	//
	// The star branch must be tested before the literal branch. '*' is a legal
	// byte in a file name, so when both the pattern and the name hold '*' the
	// literal comparison would win and consume the wildcard — which made "a*"
	// fail to match "a*bc".
	var (
		p, n         int
		starP, starN = -1, 0
	)
	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			starP = p
			starN = n
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case starP >= 0:
			p = starP + 1
			starN++
			n = starN
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// MatchAny reports whether name matches any pattern. An empty list means "no
// name restriction", which is the documented meaning of an empty match list.
func MatchAny(patterns []string, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if MatchName(p, name) {
			return true
		}
	}
	return false
}

// MatchAnyExcept reports whether name is excluded. An empty list excludes
// nothing.
func MatchAnyExcept(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchName(p, name) {
			return true
		}
	}
	return false
}

// BaseName returns the last path segment of a relative path.
func BaseName(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// DirOf returns the relative directory of rel, or "" for the scope root.
func DirOf(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// HasDirPrefix compares path segments, never raw strings.
//
// "keep/" matches "keep/a" and "keep/x/y" but not "keeper/a": a prefix is a
// directory boundary, not a character prefix.
func HasDirPrefix(rel string, prefix string) bool {
	if prefix == "" {
		return true
	}
	p := strings.TrimSuffix(prefix, "/")
	if p == "" {
		return true
	}
	return rel == p || strings.HasPrefix(rel, p+"/")
}

// UnderAnyPrefix reports whether rel sits under any of the prefixes. An empty
// list means "no location restriction".
func UnderAnyPrefix(prefixes []string, rel string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if HasDirPrefix(rel, p) {
			return true
		}
	}
	return false
}

// underAnyPrefixExcept reports whether rel is excluded by a location prefix.
func underAnyPrefixExcept(prefixes []string, rel string) bool {
	for _, p := range prefixes {
		if HasDirPrefix(rel, p) {
			return true
		}
	}
	return false
}

// Reason codes. They are stable strings because reports, plans and tests all
// refer to them.
const (
	ReasonCandidate    = "CANDIDATE"
	ReasonProtected    = "PROTECTED"
	ReasonExceptDir    = "EXCEPT_DIR"
	ReasonExceptName   = "EXCEPT_NAME"
	ReasonOutsideDir   = "OUTSIDE_DIR_PREFIX"
	ReasonNameMismatch = "NAME_MISMATCH"
	ReasonTooSmall     = "SIZE_BELOW_MIN"
	ReasonTooLarge     = "SIZE_ABOVE_MAX"
	ReasonMTimeFuture  = "MTIME_IN_FUTURE"
	ReasonTooNew       = "MODIFIED_TOO_RECENTLY"
)

// Input is everything the decision depends on. Nothing here is read from disk.
type Input struct {
	Rel             string
	SizeBytes       int64
	MTimeNS         int64
	NowNS           int64
	Protected       bool
	ProtectedReason string
}

// Verdict is the outcome for one path.
type Verdict struct {
	Candidate  bool
	ReasonCode string
	Reason     string
}

// Decide applies the rule to one path.
//
// Order matters and is fixed: global protection first, then exclusions, then
// positive conditions. Exclusions therefore always win, and a newly added
// protection cannot be outvoted by a broader match list.
func Decide(r *rules.Rule, in Input) Verdict {
	if in.Protected {
		reason := in.ProtectedReason
		if reason == "" {
			reason = "protected path"
		}
		return Verdict{false, ReasonProtected, reason}
	}

	name := BaseName(in.Rel)

	if underAnyPrefixExcept(r.Except.RelativeDirPrefixes, in.Rel) {
		return Verdict{false, ReasonExceptDir, "excluded directory prefix"}
	}
	if MatchAnyExcept(r.Except.BasenamePatterns, name) {
		return Verdict{false, ReasonExceptName, "excluded by name"}
	}
	if !UnderAnyPrefix(r.Match.RelativeDirPrefixes, in.Rel) {
		return Verdict{false, ReasonOutsideDir, "outside the allowed directory prefixes"}
	}
	if !MatchAny(r.Match.BasenamePatterns, name) {
		return Verdict{false, ReasonNameMismatch, "name does not match"}
	}
	if in.SizeBytes < r.Match.MinSizeBytes {
		return Verdict{false, ReasonTooSmall, "smaller than the minimum size"}
	}
	if r.Match.MaxSizeBytes != nil && in.SizeBytes > *r.Match.MaxSizeBytes {
		return Verdict{false, ReasonTooLarge, "larger than the maximum size"}
	}

	// Age comparison is fixed: mtime_ns <= now_ns - age_seconds * 1e9.
	if in.MTimeNS > in.NowNS {
		// A future mtime is not evidence of age, and clock skew makes it
		// unexplainable. Keep the file and say why.
		return Verdict{false, ReasonMTimeFuture, "modification time is in the future"}
	}
	threshold := in.NowNS - r.Match.MinMTimeAgeSeconds*1_000_000_000
	if threshold < 0 {
		threshold = 0
	}
	if in.MTimeNS > threshold {
		return Verdict{false, ReasonTooNew, "modified too recently"}
	}
	return Verdict{true, ReasonCandidate, "matches all conditions"}
}
