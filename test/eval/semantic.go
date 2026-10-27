package eval

import (
	"encoding/json"
	"fmt"

	"dustkeeper/internal/rules"
)

// This file is the part of the evaluation that decides whether an answer *means*
// what the corpus says it should mean.
//
// The previous version compared one string: kind == expected kind. A draft that
// turned "90 days" into "30 days", dropped "except the invoices", or picked a
// different but still legal scope agreed with the corpus, because it was still a
// draft. That is not a gate; it is a counter.

// judgment is what the host concluded from the model's answer.
type judgment struct {
	// Kind is the host decision: draft, needs_input, unsupported, rejected, error.
	Kind string
	// Rule is the normalised DSL, present only for a draft.
	Rule *rules.Rule
}

// expectedAnswer is the corpus's own answer, decoded the same way.
type expectedAnswer struct {
	Kind      string
	Rule      *rules.Rule
	Questions []string
	Reason    string
}

// decodeExpected parses the answer recorded in the corpus.
func decodeExpected(raw json.RawMessage) (expectedAnswer, error) {
	var env struct {
		Kind      string          `json:"kind"`
		DSL       json.RawMessage `json:"dsl"`
		Questions []string        `json:"questions"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return expectedAnswer{}, err
	}
	out := expectedAnswer{Kind: env.Kind, Questions: env.Questions, Reason: env.Reason}
	if len(env.DSL) > 0 {
		rule, err := rules.Parse(env.DSL, nil)
		if err != nil {
			return expectedAnswer{}, fmt.Errorf("corpus answer does not parse: %w", err)
		}
		out.Rule = rule
	}
	return out, nil
}

// compare reports whether the host's judgement matches the corpus, and why not.
//
// A draft is compared on the normalised DSL, field by field: the scope, the age
// threshold, both size bounds, the match conditions, the exclusions and the
// retention period. A clarification or a refusal is compared on the decision, not
// on its wording: the corpus cannot say which sentence a model should use, but it
// can say that the model must not guess.
func compare(want expectedAnswer, got judgment) (bool, []string) {
	if want.Kind != got.Kind {
		return false, []string{fmt.Sprintf("host decided %q, corpus expects %q", got.Kind, want.Kind)}
	}
	switch want.Kind {
	case "draft":
		if want.Rule == nil || got.Rule == nil {
			return false, []string{"a draft without a parsed rule cannot be compared"}
		}
		diffs := diffRule(*want.Rule, *got.Rule)
		return len(diffs) == 0, diffs
	case "needs_input":
		// The host already required at least one question to accept this variant,
		// so agreeing on the kind is the whole check. Wording is reviewed by hand.
		return true, nil
	case "unsupported":
		return true, nil
	default:
		return false, []string{fmt.Sprintf("the corpus expects an unknown kind %q", want.Kind)}
	}
}

// diffRule lists the semantic differences between two normalised rules.
func diffRule(want, got rules.Rule) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	if want.DSLVersion != got.DSLVersion {
		add("dsl_version: want %d, got %d", want.DSLVersion, got.DSLVersion)
	}
	if want.ScopeID != got.ScopeID {
		add("scope_id: want %q, got %q", want.ScopeID, got.ScopeID)
	}
	if want.Action != got.Action {
		add("action: want %q, got %q", want.Action, got.Action)
	}
	if want.Match.MinMTimeAgeSeconds != got.Match.MinMTimeAgeSeconds {
		add("match.min_mtime_age_seconds: want %s, got %s",
			days(want.Match.MinMTimeAgeSeconds), days(got.Match.MinMTimeAgeSeconds))
	}
	if want.Match.MinSizeBytes != got.Match.MinSizeBytes {
		add("match.min_size_bytes: want %d, got %d", want.Match.MinSizeBytes, got.Match.MinSizeBytes)
	}
	// A missing ceiling and an unlimited ceiling are different answers, so the
	// comparison has to see the difference between "null" and "5 MiB".
	if !sameSizeBound(want.Match.MaxSizeBytes, got.Match.MaxSizeBytes) {
		add("match.max_size_bytes: want %s, got %s",
			sizeBound(want.Match.MaxSizeBytes), sizeBound(got.Match.MaxSizeBytes))
	}
	if !sameSet(want.Match.RelativeDirPrefixes, got.Match.RelativeDirPrefixes) {
		add("match.relative_dir_prefixes: want %v, got %v",
			want.Match.RelativeDirPrefixes, got.Match.RelativeDirPrefixes)
	}
	if !sameSet(want.Match.BasenamePatterns, got.Match.BasenamePatterns) {
		add("match.basename_patterns: want %v, got %v",
			want.Match.BasenamePatterns, got.Match.BasenamePatterns)
	}
	// Exclusions are the field a model drops most easily, and losing them widens
	// the rule: "everything except the invoices" becomes "the invoices too".
	if !sameSet(want.Except.RelativeDirPrefixes, got.Except.RelativeDirPrefixes) {
		add("except.relative_dir_prefixes: want %v, got %v",
			want.Except.RelativeDirPrefixes, got.Except.RelativeDirPrefixes)
	}
	if !sameSet(want.Except.BasenamePatterns, got.Except.BasenamePatterns) {
		add("except.basename_patterns: want %v, got %v",
			want.Except.BasenamePatterns, got.Except.BasenamePatterns)
	}
	if want.RetentionSeconds != got.RetentionSeconds {
		add("retention_seconds: want %s, got %s",
			days(want.RetentionSeconds), days(got.RetentionSeconds))
	}
	return out
}

func sameSizeBound(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sizeBound(v *int64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *v)
}

// sameSet compares two string lists as sets: the DSL does not promise an order,
// and an order difference is not a semantic difference.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// days renders a duration in seconds as days when it divides evenly, so the diff
// reads "90d vs 30d" instead of "7776000 vs 2592000".
func days(seconds int64) string {
	const perDay = 86400
	if seconds%perDay == 0 {
		return fmt.Sprintf("%dd", seconds/perDay)
	}
	return fmt.Sprintf("%ds", seconds)
}
