package rules

import (
	"context"
	"strings"
	"testing"

	"dustkeeper/internal/errs"
)

const validDraft = `{"kind":"draft","dsl":{
  "dsl_version":1,"scope_id":"exports","action":"quarantine",
  "match":{"min_mtime_age_seconds":2592000,"min_size_bytes":0,"max_size_bytes":null,
           "relative_dir_prefixes":[],"basename_patterns":["*.zip"]},
  "except":{"relative_dir_prefixes":["keep/"],"basename_patterns":[]},
  "retention_seconds":2592000}}`

func scopes() map[string]bool { return map[string]bool{"exports": true} }

func TestParseDraftAcceptsTheThreeVariants(t *testing.T) {
	d, err := ParseDraft([]byte(validDraft), scopes())
	if err != nil {
		t.Fatalf("valid draft rejected: %v", err)
	}
	if d.Kind != DraftKind || d.DSL == "" || d.NeedsInput {
		t.Fatalf("wrong draft: %+v", d)
	}

	d, err = ParseDraft([]byte(`{"kind":"needs_input","questions":["how old?","which names?"]}`), scopes())
	if err != nil {
		t.Fatalf("clarification rejected: %v", err)
	}
	if !d.NeedsInput || len(d.Questions) != 2 {
		t.Fatalf("wrong clarification: %+v", d)
	}

	d, err = ParseDraft([]byte(`{"kind":"unsupported","reason":"no retention stated"}`), scopes())
	if err != nil {
		t.Fatalf("refusal rejected: %v", err)
	}
	if d.Kind != DraftUnsupported || d.Reason == "" {
		t.Fatalf("wrong refusal: %+v", d)
	}
}

func TestParseDraftRejectsMixedOrWrappedAnswers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"draft with questions", `{"kind":"draft","dsl":{"dsl_version":1,"scope_id":"exports","action":"quarantine","match":{"min_mtime_age_seconds":2592000},"except":{},"retention_seconds":2592000},"questions":["and?"]}`},
		{"draft with a reason", `{"kind":"draft","reason":"because"}`},
		{"fenced JSON", "```json\n" + validDraft + "\n```"},
		{"prose around JSON", "Here is the rule: " + validDraft},
		{"clarification with a body", `{"kind":"needs_input","dsl":{"dsl_version":1},"questions":["a"]}`},
		{"too many questions", `{"kind":"needs_input","questions":["1","2","3","4"]}`},
		{"empty question", `{"kind":"needs_input","questions":["  "]}`},
		{"refusal without reason", `{"kind":"unsupported"}`},
		{"unknown kind", `{"kind":"delete_everything"}`},
		{"unknown field", `{"kind":"unsupported","reason":"x","auto_purge":true}`},
		{"not JSON", `ok`},
		{"empty", ``},
	}
	for _, c := range cases {
		if _, err := ParseDraft([]byte(c.raw), scopes()); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestParseDraftRejectsAnUnregisteredScope(t *testing.T) {
	raw := strings.Replace(validDraft, `"scope_id":"exports"`, `"scope_id":"somewhere_else"`, 1)
	if _, err := ParseDraft([]byte(raw), scopes()); err == nil {
		t.Fatal("a rule for an unregistered scope was accepted")
	}
}

func TestParseDraftRejectsADraftThatViolatesTheFieldContract(t *testing.T) {
	raw := strings.Replace(validDraft, `"min_mtime_age_seconds":2592000`, `"min_mtime_age_seconds":10`, 1)
	if _, err := ParseDraft([]byte(raw), scopes()); err == nil {
		t.Fatal("an age below the documented minimum was accepted")
	}
}

// stubProvider returns a fixed answer, so Compile can be tested without a
// network or a model.
type stubProvider struct {
	answer string
	err    error
	seen   DraftRequest
}

func (s *stubProvider) Draft(_ context.Context, req DraftRequest) ([]byte, error) {
	s.seen = req
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.answer), nil
}

func TestCompilePassesOnlyTheUserTextAndScopeIDs(t *testing.T) {
	p := &stubProvider{answer: validDraft}
	d, err := Compile(context.Background(), p, "30 days old zips in exports", scopes())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if d.Kind != DraftKind {
		t.Fatalf("wrong draft kind %q", d.Kind)
	}
	if len(p.seen.Scopes) != 1 || p.seen.Scopes[0] != "exports" {
		t.Fatalf("the scope list was not passed: %v", p.seen.Scopes)
	}
	if strings.Contains(p.seen.System, "{{") {
		t.Error("the system prompt was not rendered")
	}
}

func TestCompileBoundsTheInput(t *testing.T) {
	p := &stubProvider{answer: validDraft}
	long := strings.Repeat("字", MaxInputRunes+1)
	_, err := Compile(context.Background(), p, long, scopes())
	if err == nil {
		t.Fatal("an over-long description was accepted")
	}
	if errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Fatalf("unexpected code %s", errs.CodeOf(err))
	}
	if p.seen.Text != "" {
		t.Fatal("the provider was called with an over-long description")
	}
}

func TestCompileWithoutAProvider(t *testing.T) {
	_, err := Compile(context.Background(), nil, "anything", scopes())
	if err == nil {
		t.Fatal("compiling without a provider succeeded")
	}
	if errs.CodeOf(err) != errs.CodeModelUnavailable {
		t.Fatalf("unexpected code %s", errs.CodeOf(err))
	}
}

func TestCompilePropagatesProviderFailure(t *testing.T) {
	p := &stubProvider{err: errs.New(errs.CodeModelUnavailable, "offline")}
	_, err := Compile(context.Background(), p, "text", scopes())
	if errs.CodeOf(err) != errs.CodeModelUnavailable {
		t.Fatalf("unexpected code %s", errs.CodeOf(err))
	}
}
