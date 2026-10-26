package rules

import (
	"encoding/json"
	"strings"
	"testing"

	"dustkeeper/internal/errs"
)

// requiredPaths mirrors the field contract: every field the DSL documents is
// mandatory. max_size_bytes is the single exception — it is nullable, and "absent"
// and "null" both mean "no upper bound" because the DSL has no third spelling for
// unlimited.
var requiredPaths = []string{
	"dsl_version",
	"scope_id",
	"action",
	"match",
	"match.min_mtime_age_seconds",
	"match.min_size_bytes",
	"match.relative_dir_prefixes",
	"match.basename_patterns",
	"except",
	"except.relative_dir_prefixes",
	"except.basename_patterns",
	"retention_seconds",
}

// mutation decodes the published test vector so each case can change exactly one
// field, then re-encodes it.
func mutation(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(vectorJSON), &m); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	return m
}

func encode(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// parentAndLeaf resolves a dotted path to the container that holds the leaf.
func parentAndLeaf(t *testing.T, m map[string]any, path string) (map[string]any, string) {
	t.Helper()
	parts := strings.Split(path, ".")
	cur := m
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not an object in the vector", path, p)
		}
		cur = next
	}
	return cur, parts[len(parts)-1]
}

// A rule that quietly drops a field executes a *wider* condition than the one a
// human reviewed: a missing `except` means "no exclusions", a missing
// `basename_patterns` means "every file". Both used to parse.
func TestEveryDocumentedFieldIsRequired(t *testing.T) {
	for _, path := range requiredPaths {
		t.Run("without_"+path, func(t *testing.T) {
			m := mutation(t)
			obj, leaf := parentAndLeaf(t, m, path)
			delete(obj, leaf)

			if _, err := Parse(encode(t, m), scopes()); err == nil {
				t.Fatalf("a rule without %s was accepted", path)
			} else if got := errs.CodeOf(err); got != errs.CodeDSLInvalid {
				t.Fatalf("code = %s (%v), want DSL_INVALID", got, err)
			} else if !strings.Contains(err.Error(), path) {
				t.Fatalf("the error does not name the missing field %s: %v", path, err)
			}
		})
	}
}

// "Field present but null" is not "field provided": binding null into a slice or
// an object yields the same zero value as omitting it.
func TestRequiredFieldsRejectNull(t *testing.T) {
	for _, path := range requiredPaths {
		t.Run("null_"+path, func(t *testing.T) {
			m := mutation(t)
			obj, leaf := parentAndLeaf(t, m, path)
			obj[leaf] = nil

			if _, err := Parse(encode(t, m), scopes()); err == nil {
				t.Fatalf("a null %s was accepted as a value", path)
			} else if !strings.Contains(err.Error(), path) {
				t.Fatalf("the error does not name %s: %v", path, err)
			}
		})
	}
}

// The whole point of the previous two tests: an empty collection is legal, a
// missing collection is not, and the two must not be collapsed into one.
func TestEmptyCollectionsAreNotMissingFields(t *testing.T) {
	m := mutation(t)
	obj, leaf := parentAndLeaf(t, m, "except.basename_patterns")
	obj[leaf] = []any{}
	if _, err := Parse(encode(t, m), scopes()); err != nil {
		t.Fatalf("an explicit empty list was rejected: %v", err)
	}

	m = mutation(t)
	obj, leaf = parentAndLeaf(t, m, "match.relative_dir_prefixes")
	obj[leaf] = []any{}
	if _, err := Parse(encode(t, m), scopes()); err != nil {
		t.Fatalf("an explicit empty prefix list was rejected: %v", err)
	}
}

// max_size_bytes is the documented nullable field: absent, null and a number all
// have distinct meanings that the contract allows.
func TestMaxSizeBytesIsTheOnlyOptionalField(t *testing.T) {
	m := mutation(t)
	obj, leaf := parentAndLeaf(t, m, "match.max_size_bytes")
	delete(obj, leaf)
	if _, err := Parse(encode(t, m), scopes()); err != nil {
		t.Fatalf("an absent max_size_bytes was rejected: %v", err)
	}

	m = mutation(t)
	obj, leaf = parentAndLeaf(t, m, "match.max_size_bytes")
	obj[leaf] = nil
	if _, err := Parse(encode(t, m), scopes()); err != nil {
		t.Fatalf("a null max_size_bytes was rejected: %v", err)
	}

	m = mutation(t)
	obj, leaf = parentAndLeaf(t, m, "match.max_size_bytes")
	obj[leaf] = 1024
	if _, err := Parse(encode(t, m), scopes()); err != nil {
		t.Fatalf("a numeric max_size_bytes was rejected: %v", err)
	}
}

// A scalar where an array is expected binds to nothing useful; the shape has to
// be part of the contract, not just the key name.
func TestFieldsMustHaveTheDocumentedShape(t *testing.T) {
	cases := map[string]any{
		"match.basename_patterns":      "*.zip",
		"except.relative_dir_prefixes": map[string]any{},
		"match":                        []any{},
	}
	for path, value := range cases {
		m := mutation(t)
		if path == "match" {
			m["match"] = value
		} else {
			obj, leaf := parentAndLeaf(t, m, path)
			obj[leaf] = value
		}
		if _, err := Parse(encode(t, m), scopes()); err == nil {
			t.Fatalf("a %T was accepted for %s", value, path)
		}
	}
}

// The model draft and the imported file must go through the same validator: a
// draft that omits `except` used to be normalised into "no exclusions" while the
// same omission in a file was also accepted, for the same reason.
func TestModelDraftsGoThroughTheSameFieldContract(t *testing.T) {
	var env map[string]any
	if err := json.Unmarshal([]byte(validDraft), &env); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	dsl, ok := env["dsl"].(map[string]any)
	if !ok {
		t.Fatal("the fixture has no dsl object")
	}
	delete(dsl, "except")

	if _, err := ParseDraft(encode(t, env), scopes()); err == nil {
		t.Fatal("a model draft without `except` was accepted")
	} else if !strings.Contains(err.Error(), "except") {
		t.Fatalf("the error does not name the missing field: %v", err)
	}
}

// The three variants are distinguished by which fields are *present*, not by
// whether the ones present happen to be empty. A draft carrying an empty
// questions list is still a draft trying to also be a clarification.
func TestDraftVariantsAreJudgedByFieldPresence(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"draft with an empty questions list": {
			body: `{"kind":"draft","questions":[],"dsl":` + dslOf(t, validDraft) + `}`,
			want: "questions",
		},
		"draft with an empty reason": {
			body: `{"kind":"draft","reason":"","dsl":` + dslOf(t, validDraft) + `}`,
			want: "reason",
		},
		"clarification with an empty reason": {
			body: `{"kind":"needs_input","questions":["how old?"],"reason":""}`,
			want: "reason",
		},
		"clarification with a null dsl": {
			body: `{"kind":"needs_input","questions":["how old?"],"dsl":null}`,
			want: "dsl",
		},
		"refusal with an empty questions list": {
			body: `{"kind":"unsupported","reason":"no retention stated","questions":[]}`,
			want: "questions",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDraft([]byte(c.body), scopes())
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the error does not mention %q: %v", c.want, err)
			}
		})
	}
}

// dslOf extracts the dsl object of a fixture so the draft cases above reuse the
// published test vector instead of a second hand-written copy.
func dslOf(t *testing.T, draft string) string {
	t.Helper()
	var env struct {
		DSL json.RawMessage `json:"dsl"`
	}
	if err := json.Unmarshal([]byte(draft), &env); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	return string(env.DSL)
}
