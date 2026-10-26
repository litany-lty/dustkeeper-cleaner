// Package eval runs the Chinese corpus against a real model.
//
// This is deliberately separate from internal/rules/corpus_test.go, which only
// checks that the hand-written answers parse. That test can pass while the model
// understands nothing: it never calls one. This one records what the model
// actually returned, together with the provider, the model name, the prompt
// version and the host's decision, so a human can review the difference.
//
// Nothing here runs during a normal `go test ./...`. Set DUSTKEEPER_EVAL_BASE_URL
// (and usually DUSTKEEPER_EVAL_MODEL / DUSTKEEPER_EVAL_API_KEY) to run it:
//
//	DUSTKEEPER_EVAL_BASE_URL=https://host/v1/chat/completions \
//	DUSTKEEPER_EVAL_MODEL=some-model \
//	DUSTKEEPER_EVAL_API_KEY=... \
//	DUSTKEEPER_EVAL_OUT=/tmp/dustkeeper-eval.jsonl \
//	go test ./test/eval -v -count=1
//
// The output file is the evidence: one JSON object per corpus entry, including
// the entries the model got wrong.
package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/rules"
)

// corpusEntry mirrors the fixture shape used by the parse-only test.
type corpusEntry struct {
	ID     string          `json:"id"`
	Group  string          `json:"group"`
	Expect string          `json:"expect"`
	Input  string          `json:"input"`
	Note   string          `json:"note"`
	Answer json.RawMessage `json:"answer"`
}

// record is one line of the report.
type record struct {
	CorpusID      string `json:"corpus_id"`
	PromptVersion string `json:"prompt_version"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Input         string `json:"input"`
	Answer        string `json:"answer,omitempty"`
	Error         string `json:"error,omitempty"`
	HostKind      string `json:"host_decision"`
	Expected      string `json:"expected"`
	Agrees        bool   `json:"agrees"`
	Reviewed      bool   `json:"reviewed"` // a human sets this after reading it
}

func TestCorpusAgainstARealModel(t *testing.T) {
	baseURL := os.Getenv("DUSTKEEPER_EVAL_BASE_URL")
	if baseURL == "" {
		t.Skip("set DUSTKEEPER_EVAL_BASE_URL to run the live model evaluation")
	}
	modelName := os.Getenv("DUSTKEEPER_EVAL_MODEL")
	if modelName == "" {
		t.Skip("set DUSTKEEPER_EVAL_MODEL to name the model being evaluated")
	}

	m := *config.Default().Model
	m.Enabled = true
	m.BaseURL = baseURL
	m.Model = modelName
	// The key travels in the environment, never in the report. An empty name means
	// "this endpoint needs no credential", which is how a local model is run.
	if os.Getenv("DUSTKEEPER_EVAL_API_KEY") != "" {
		m.APIKeyEnv = "DUSTKEEPER_EVAL_API_KEY"
	} else {
		m.APIKeyEnv = ""
	}

	provider, err := rules.NewHTTPProvider(m)
	if err != nil {
		t.Fatalf("build the adapter: %v", err)
	}

	entries := loadCorpus(t)
	scopeList := scopeSet(entries)
	scopes := make(map[string]bool, len(scopeList))
	for _, id := range scopeList {
		scopes[id] = true
	}
	promptVersion := promptDigest()

	out := os.Getenv("DUSTKEEPER_EVAL_OUT")
	if out == "" {
		out = filepath.Join(t.TempDir(), "eval.jsonl")
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create report %s: %v", out, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	agree, disagree := 0, 0
	for _, e := range entries {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		raw, callErr := provider.Draft(ctx, rules.DraftRequest{
			Text:   e.Input,
			Scopes: scopeList,
			System: rules.DraftSystemPrompt(),
		})
		cancel()

		rec := record{
			CorpusID:      e.ID,
			PromptVersion: promptVersion,
			Provider:      m.Provider,
			Model:         m.Model,
			Input:         e.Input,
			Answer:        string(raw),
			Expected:      e.Expect,
		}
		if callErr != nil {
			rec.Error = callErr.Error()
			rec.HostKind = "error"
		} else if draft, parseErr := rules.ParseDraft(raw, scopes); parseErr != nil {
			// A rejection is a host decision too, and the most important one to
			// record: "the model said something the host refused" is not a pass.
			rec.Error = parseErr.Error()
			rec.HostKind = "rejected"
		} else {
			rec.HostKind = draft.Kind
		}
		rec.Agrees = rec.HostKind == e.Expect
		if rec.Agrees {
			agree++
		} else {
			disagree++
			t.Logf("%s: expected %s, host recorded %s (model: %s)", e.ID, e.Expect, rec.HostKind, rec.Answer)
		}
		if err := enc.Encode(rec); err != nil {
			t.Fatalf("write report: %v", err)
		}
	}

	t.Logf("model=%s prompt=%s entries=%d agree=%d disagree=%d report=%s",
		m.Model, promptVersion, len(entries), agree, disagree, out)
	if disagree > 0 {
		t.Errorf("%d of %d corpus entries were not understood as expected; the report lists them", disagree, len(entries))
	}
}

// loadCorpus reads the same fixture the parse-only test uses.
func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	path := filepath.Join("..", "fixtures", "rules", "zh_corpus.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var entries []corpusEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the corpus is empty")
	}
	return entries
}

// scopeSet is the union of the scope IDs the corpus refers to, so the model is
// asked to choose from a set that actually contains the answer.
func scopeSet(entries []corpusEntry) []string {
	seen := map[string]bool{}
	for _, e := range entries {
		var env struct {
			DSL struct {
				ScopeID string `json:"scope_id"`
			} `json:"dsl"`
		}
		if err := json.Unmarshal(e.Answer, &env); err != nil {
			continue
		}
		if env.DSL.ScopeID != "" {
			seen[env.DSL.ScopeID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// promptDigest pins the exact instructions used for a run. A prompt change makes
// old reports incomparable, so the version travels with every record.
func promptDigest() string {
	sum := sha256.Sum256([]byte(rules.DraftSystemPrompt()))
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:])[:12])
}
