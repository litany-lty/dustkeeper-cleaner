package rules

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The corpus in test/fixtures/rules/zh_corpus.json is the regression harness for
// the draft contract. Each entry pairs a real Chinese description with the
// answer a correct adapter must produce, and the test proves that answer is
// accepted by the same parser the CLI uses.
//
// What this test can and cannot prove: it proves the corpus is expressible in
// the DSL, that the three-variant discriminator accepts exactly one variant per
// entry, and that canonicalisation is stable. It does not run a model, so it is
// not evidence that "没用过" or "保留发票" was understood.
//
// The semantic half lives in test/eval, which sends the same corpus to a real
// model and records the provider, the model, the prompt digest, the raw answer
// and the host's decision. It skips unless DUSTKEEPER_EVAL_BASE_URL is set, so a
// network call is never a precondition for `go test ./...`.

type corpusFixture struct {
	ID     string          `json:"id"`
	Group  string          `json:"group"`
	Expect string          `json:"expect"`
	Input  string          `json:"input"`
	Note   string          `json:"note"`
	Answer json.RawMessage `json:"answer"`
}

// allowedScopes is the fixed set every fixture may reference. It is not derived
// from the corpus, so a fixture that invents a scope cannot widen its own
// permission.
var allowedScopes = map[string]bool{
	"exports":   true,
	"downloads": true,
	"originals": true,
	"media":     true,
	"tmp_cache": true,
}

func loadCorpus(t *testing.T) []corpusFixture {
	t.Helper()
	raw, err := os.ReadFile("../../test/fixtures/rules/zh_corpus.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var list []corpusFixture
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("the corpus is empty")
	}
	return list
}

func TestCorpusSizeAndGroups(t *testing.T) {
	list := loadCorpus(t)

	// Section 15 asks for at least forty Chinese descriptions and calls out four
	// ambiguity classes that must land on needs_input.
	if len(list) < 40 {
		t.Fatalf("the corpus has %d entries; at least 40 are required", len(list))
	}

	byGroup := map[string]int{}
	ids := map[string]bool{}
	for _, f := range list {
		if f.ID == "" {
			t.Fatal("a fixture has no id")
		}
		if ids[f.ID] {
			t.Fatalf("duplicate fixture id %q", f.ID)
		}
		ids[f.ID] = true
		byGroup[f.Group]++
	}

	for _, g := range []string{"unused", "negation", "exception", "multi_threshold"} {
		if byGroup[g] < 6 {
			t.Errorf("group %q has %d entries; at least 6 are required", g, byGroup[g])
		}
	}
	if byGroup["draft"] < 8 {
		t.Errorf("group draft has %d entries; at least 8 are required", byGroup["draft"])
	}
	if len(byGroup) < 6 {
		t.Errorf("only %d groups are represented; expected 6", len(byGroup))
	}
}

func TestCorpusAnswersMatchTheirDeclaredVariant(t *testing.T) {
	for _, f := range loadCorpus(t) {
		t.Run(f.ID, func(t *testing.T) {
			if strings.TrimSpace(f.Input) == "" {
				t.Fatal("the description is empty")
			}

			d, err := ParseDraft(f.Answer, allowedScopes)
			if err != nil {
				t.Fatalf("the declared answer was rejected: %v", err)
			}
			if d.Kind != f.Expect {
				t.Fatalf("kind = %q, want %q", d.Kind, f.Expect)
			}

			switch f.Group {
			case "draft":
				if f.Expect != DraftKind {
					t.Fatalf("group draft must expect kind draft, got %q", f.Expect)
				}
			case "unused", "negation", "exception", "multi_threshold":
				if f.Expect != DraftNeedsInputKind {
					t.Fatalf("group %q must expect needs_input, got %q", f.Group, f.Expect)
				}
			case "unsupported":
				if f.Expect != DraftUnsupported {
					t.Fatalf("group unsupported must expect kind unsupported, got %q", f.Expect)
				}
			default:
				t.Fatalf("unknown group %q", f.Group)
			}
		})
	}
}

func TestDraftFixturesAreCanonicalAndBounded(t *testing.T) {
	for _, f := range loadCorpus(t) {
		if f.Expect != DraftKind {
			continue
		}
		t.Run(f.ID, func(t *testing.T) {
			d, err := ParseDraft(f.Answer, allowedScopes)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			// Re-parse the rule body independently and canonicalise it. The two
			// strings must agree byte for byte: ParseDraft must not be the only
			// path that produces the bytes a digest covers.
			var env struct {
				DSL json.RawMessage `json:"dsl"`
			}
			if err := json.Unmarshal(f.Answer, &env); err != nil {
				t.Fatalf("decode dsl: %v", err)
			}
			r, err := Parse(env.DSL, allowedScopes)
			if err != nil {
				t.Fatalf("independent parse: %v", err)
			}
			canon, err := r.CanonicalJSON()
			if err != nil {
				t.Fatalf("canonicalise: %v", err)
			}
			if string(canon) != d.DSL {
				t.Fatalf("canonical forms differ:\n  ParseDraft: %s\n  canonical:  %s", d.DSL, canon)
			}

			if r.Action != "quarantine" {
				t.Errorf("action = %q; a rule may only request quarantine", r.Action)
			}
			if r.RetentionSeconds < 604800 || r.RetentionSeconds%86400 != 0 {
				t.Errorf("retention_seconds = %d; must be at least 7 whole days", r.RetentionSeconds)
			}
			// A draft fixture exists to show the user's words were sufficient.
			if !strings.Contains(f.Input, "保留") && !strings.Contains(f.Input, "留") {
				t.Errorf("the description states no retention period, so it cannot be a complete draft")
			}
			if strings.TrimSpace(f.Note) == "" {
				t.Errorf("a draft fixture must explain which mapping it exercises")
			}
		})
	}
}

func TestNeedsInputFixturesAskAnswerableQuestions(t *testing.T) {
	for _, f := range loadCorpus(t) {
		if f.Expect != DraftNeedsInputKind {
			continue
		}
		t.Run(f.ID, func(t *testing.T) {
			d, err := ParseDraft(f.Answer, allowedScopes)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !d.NeedsInput {
				t.Fatal("NeedsInput is false for a clarification request")
			}
			if n := len(d.Questions); n < 1 || n > MaxQuestions {
				t.Fatalf("%d questions; the contract allows 1..%d", n, MaxQuestions)
			}
			for i, q := range d.Questions {
				if strings.TrimSpace(q) == "" {
					t.Fatalf("question %d is empty", i)
				}
				// A clarification has to actually ask something. Trailing guidance
				// after the question mark is fine; asking nothing is not.
				if !strings.Contains(q, "?") && !strings.Contains(q, "？") {
					t.Errorf("question %d asks nothing: %q", i, q)
				}
			}
			if d.DSL != "" {
				t.Error("a clarification request must not carry a rule body")
			}
			// The note is the human-readable reason this fixture needs input.
			if strings.TrimSpace(f.Note) == "" {
				t.Error("every needs_input fixture must explain which field is unstated")
			}
		})
	}
}

func TestUnsupportedFixturesGiveAReason(t *testing.T) {
	for _, f := range loadCorpus(t) {
		if f.Expect != DraftUnsupported {
			continue
		}
		t.Run(f.ID, func(t *testing.T) {
			d, err := ParseDraft(f.Answer, allowedScopes)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if strings.TrimSpace(d.Reason) == "" {
				t.Fatal("an unsupported answer must not be silent")
			}
			if d.DSL != "" || len(d.Questions) > 0 {
				t.Fatal("an unsupported answer must carry neither a rule body nor questions")
			}
		})
	}
}

// TestCorpusNeverSmugglesAuthority guards the whole file, not one entry: no
// fixture may reference an absolute path or an unregistered scope, because a
// corpus that contains such an answer teaches the adapter to produce one.
func TestCorpusNeverSmugglesAuthority(t *testing.T) {
	for _, f := range loadCorpus(t) {
		text := f.Input + "\n" + string(f.Answer)
		for _, bad := range []string{"/Users/", "/home/", "/srv/", "/Volumes/", "C:\\\\"} {
			if f.Expect == DraftUnsupported {
				// A refusal may quote the path it is refusing.
				continue
			}
			if strings.Contains(text, bad) {
				t.Errorf("%s: the answer references an absolute path (%s)", f.ID, bad)
			}
		}
	}
}
