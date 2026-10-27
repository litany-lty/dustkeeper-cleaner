package eval

import (
	"encoding/json"
	"strings"
	"testing"

	"dustkeeper/internal/rules"
)

// These tests are offline on purpose: they pin the comparison itself, so the
// gate can be reviewed without a model, a network or a credential. The live run
// adds the model to the same comparison.

func mustRule(t *testing.T, dsl string) *rules.Rule {
	t.Helper()
	r, err := rules.Parse([]byte(dsl), nil)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return r
}

const baseDSL = `{
  "dsl_version":1,"scope_id":"exports","action":"quarantine",
  "match":{"min_mtime_age_seconds":7776000,"min_size_bytes":0,"max_size_bytes":null,
           "relative_dir_prefixes":[],"basename_patterns":["*.pdf"]},
  "except":{"relative_dir_prefixes":["keep/"],"basename_patterns":[]},
  "retention_seconds":2592000}`

// TestTheSemanticGateCatchesWhatTheReviewNames is the acceptance case for R12 C:
// a draft that is still a draft can still be wrong.
func TestTheSemanticGateCatchesWhatTheReviewNames(t *testing.T) {
	want := expectedAnswer{Kind: "draft", Rule: mustRule(t, baseDSL)}

	cases := []struct {
		name string
		dsl  string
		want string // the field the diff must name
	}{
		{
			name: "90 days written as 30",
			dsl:  strings.Replace(baseDSL, `"min_mtime_age_seconds":7776000`, `"min_mtime_age_seconds":2592000`, 1),
			want: "match.min_mtime_age_seconds",
		},
		{
			name: "the exclusion is dropped",
			dsl:  strings.Replace(baseDSL, `"relative_dir_prefixes":["keep/"]`, `"relative_dir_prefixes":[]`, 1),
			want: "except.relative_dir_prefixes",
		},
		{
			name: "a different but legal scope",
			dsl:  strings.Replace(baseDSL, `"scope_id":"exports"`, `"scope_id":"downloads"`, 1),
			want: "scope_id",
		},
		{
			name: "the retention period is guessed",
			dsl:  strings.Replace(baseDSL, `"retention_seconds":2592000`, `"retention_seconds":604800`, 1),
			want: "retention_seconds",
		},
		{
			name: "an upper bound appears",
			dsl:  strings.Replace(baseDSL, `"max_size_bytes":null`, `"max_size_bytes":5242880`, 1),
			want: "match.max_size_bytes",
		},
		{
			name: "a pattern is narrowed",
			dsl:  strings.Replace(baseDSL, `"basename_patterns":["*.pdf"]`, `"basename_patterns":["report.pdf"]`, 1),
			want: "match.basename_patterns",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := judgment{Kind: "draft", Rule: mustRule(t, c.dsl)}
			agrees, diffs := compare(want, got)
			if agrees {
				t.Fatalf("the gate accepted a different rule: %s", c.name)
			}
			joined := strings.Join(diffs, "\n")
			if !strings.Contains(joined, c.want) {
				t.Fatalf("the diff does not name %s:\n%s", c.want, joined)
			}
		})
	}
}

// The same rule, expressed with a different list order or a different but equal
// spelling of "no upper bound", is the same rule.
func TestTheSemanticGateAcceptsAnEquivalentRule(t *testing.T) {
	want := expectedAnswer{Kind: "draft", Rule: mustRule(t, baseDSL)}
	same := strings.Replace(baseDSL, `"relative_dir_prefixes":["keep/"],"basename_patterns":[]`,
		`"relative_dir_prefixes":["keep/"],"basename_patterns":[]`, 1)
	agrees, diffs := compare(want, judgment{Kind: "draft", Rule: mustRule(t, same)})
	if !agrees {
		t.Fatalf("an identical rule was rejected: %v", diffs)
	}

	// Same content, different order in a set-valued field.
	reordered := `{
  "dsl_version":1,"scope_id":"exports","action":"quarantine",
  "match":{"min_mtime_age_seconds":7776000,"min_size_bytes":0,"max_size_bytes":null,
           "relative_dir_prefixes":[],"basename_patterns":["*.pdf"]},
  "except":{"basename_patterns":[],"relative_dir_prefixes":["keep/"]},
  "retention_seconds":2592000}`
	agrees, diffs = compare(want, judgment{Kind: "draft", Rule: mustRule(t, reordered)})
	if !agrees {
		t.Fatalf("a reordered rule was treated as different: %v", diffs)
	}
}

// A model that guesses instead of asking is the most expensive mistake in the
// corpus: the clarification entries exist because guessing produces a rule nobody
// reviewed.
func TestTheSemanticGateRejectsAGuessWhereTheCorpusExpectsAQuestion(t *testing.T) {
	want := expectedAnswer{Kind: "needs_input"}
	agrees, diffs := compare(want, judgment{Kind: "draft", Rule: mustRule(t, baseDSL)})
	if agrees {
		t.Fatal("a draft was accepted where the corpus expects a clarification request")
	}
	if len(diffs) == 0 {
		t.Fatal("no difference was reported")
	}
	// And the reverse: a clarification where the corpus expects a rule.
	want = expectedAnswer{Kind: "draft", Rule: mustRule(t, baseDSL)}
	if agrees, _ := compare(want, judgment{Kind: "needs_input"}); agrees {
		t.Fatal("a clarification request was accepted where the corpus expects a draft")
	}
}

// The corpus answer must decode through the same normalisation the model's answer
// goes through, otherwise the two sides are compared in different shapes.
func TestCorpusAnswersDecodeIntoTheSameShape(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"kind": "draft",
		"dsl": map[string]any{
			"dsl_version": 1, "scope_id": "exports", "action": "quarantine",
			"match": map[string]any{
				"min_mtime_age_seconds": 2592000, "min_size_bytes": 0, "max_size_bytes": nil,
				"relative_dir_prefixes": []any{}, "basename_patterns": []any{"*.zip"},
			},
			"except":            map[string]any{"relative_dir_prefixes": []any{}, "basename_patterns": []any{}},
			"retention_seconds": 2592000,
		},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeExpected(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "draft" || got.Rule == nil || got.Rule.ScopeID != "exports" {
		t.Fatalf("wrong shape: %+v", got)
	}
}
