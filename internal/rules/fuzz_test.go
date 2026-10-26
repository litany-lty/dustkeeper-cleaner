package rules

import (
	"encoding/json"
	"testing"
)

// FuzzParseDraft proves the discriminator never returns an ambiguous answer: a
// parsed draft carries a rule body and nothing else, a clarification carries
// questions and nothing else, and a refusal carries a reason and nothing else.
func FuzzParseDraft(f *testing.F) {
	f.Add([]byte(`{"kind":"draft","dsl":{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":2592000,"min_size_bytes":0,"relative_dir_prefixes":[],"basename_patterns":["*.zip"]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":2592000}}`))
	f.Add([]byte(`{"kind":"needs_input","questions":["how many days?"]}`))
	f.Add([]byte(`{"kind":"unsupported","reason":"a rule cannot delete"}`))
	f.Add([]byte(`{"kind":"needs_input","questions":["a"],"dsl":null}`))
	f.Add([]byte(`{"kind":"draft"}`))
	f.Add([]byte(`{"kind":"draft","dsl":{"dsl_version":1},"questions":["x"]}`))
	f.Add([]byte("```json\n{\"kind\":\"unsupported\",\"reason\":\"x\"}\n```"))
	f.Add([]byte(`{"kind":"unsupported","reason":"x","extra":1}`))

	scopes := map[string]bool{"exports": true, "downloads": true}

	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := ParseDraft(raw, scopes)
		if err != nil {
			return
		}
		switch d.Kind {
		case DraftKind:
			if d.DSL == "" {
				t.Fatal("a draft returned no rule body")
			}
			if d.NeedsInput || len(d.Questions) != 0 || d.Reason != "" {
				t.Fatal("a draft carried fields from another variant")
			}
			// The body must be canonical and accepted on its own.
			r, perr := Parse([]byte(d.DSL), scopes)
			if perr != nil {
				t.Fatalf("the returned body does not parse: %v", perr)
			}
			canon, cerr := r.CanonicalJSON()
			if cerr != nil {
				t.Fatalf("canonicalise: %v", cerr)
			}
			if string(canon) != d.DSL {
				t.Fatalf("the returned body is not canonical:\n  %s\n  %s", d.DSL, canon)
			}
		case DraftNeedsInputKind:
			if !d.NeedsInput || len(d.Questions) == 0 || len(d.Questions) > MaxQuestions {
				t.Fatal("a clarification request is not usable")
			}
			if d.DSL != "" || d.Reason != "" {
				t.Fatal("a clarification request carried fields from another variant")
			}
		case DraftUnsupported:
			if d.NeedsInput || d.DSL != "" || len(d.Questions) != 0 || d.Reason == "" {
				t.Fatal("a refusal carried fields from another variant")
			}
		default:
			t.Fatalf("unknown kind %q", d.Kind)
		}
	})
}

// FuzzParseRule checks that the accepted language is closed under
// canonicalisation: re-parsing the canonical form must produce the same bytes
// and the same digest. If this ever fails, a stored rule digest stops matching
// the rule that produced it.
func FuzzParseRule(f *testing.F) {
	f.Add([]byte(`{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":86400,"min_size_bytes":0,"relative_dir_prefixes":[],"basename_patterns":[]},"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`))
	f.Add([]byte(`{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":2592000,"min_size_bytes":0,"max_size_bytes":104857600,"relative_dir_prefixes":["keep/"],"basename_patterns":["*.zip","*.tar.gz"]},"except":{"relative_dir_prefixes":["keep/old/"],"basename_patterns":["*发票*"]},"retention_seconds":2592000}`))
	f.Add([]byte(`{"dsl_version":2}`))
	f.Add([]byte(`{"dsl_version":1,"scope_id":"exports","action":"delete"}`))
	f.Add([]byte(`{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":1},"retention_seconds":1}`))
	f.Add([]byte(`{"dsl_version":1.0,"scope_id":"exports","action":"quarantine"}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		r, err := Parse(raw, nil)
		if err != nil {
			return
		}
		canon, cerr := r.CanonicalJSON()
		if cerr != nil {
			t.Fatalf("canonicalise: %v", cerr)
		}

		again, err := Parse(canon, nil)
		if err != nil {
			t.Fatalf("the canonical form no longer parses: %v", err)
		}
		second, cerr := again.CanonicalJSON()
		if cerr != nil {
			t.Fatalf("canonicalise twice: %v", cerr)
		}
		if string(second) != string(canon) {
			t.Fatalf("canonicalisation is not idempotent:\n  %s\n  %s", canon, second)
		}

		d1, err := r.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		d2, err := again.Digest()
		if err != nil {
			t.Fatalf("digest of the canonical form: %v", err)
		}
		if d1 != d2 {
			t.Fatalf("digest changed after round trip: %s vs %s", d1, d2)
		}

		// The canonical form must be a single JSON object with no trailing bytes.
		var v any
		if err := json.Unmarshal(canon, &v); err != nil {
			t.Fatalf("the canonical form is not valid JSON: %v", err)
		}
	})
}
