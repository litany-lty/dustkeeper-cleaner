package rules

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/config"
)

// captureServer records the request body and answers with one fixed draft.
type captureServer struct {
	*httptest.Server
	body []byte
}

func newCaptureServer(t *testing.T, answer string) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		cs.body = body
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": answer}}},
		})
		_, _ = w.Write(payload)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *captureServer) provider(t *testing.T) *HTTPProvider {
	t.Helper()
	p, err := NewHTTPProvider(config.Model{
		Enabled:               true,
		Provider:              ProviderOpenAICompatible,
		BaseURL:               cs.URL,
		Model:                 "test-model",
		RequestTimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	return p
}

// The prompt tells the model to pick one of the allowed scope IDs. Sending the
// list is therefore not decoration: without it the model can only guess, and a
// guess that happens to match a registered scope is indistinguishable from a
// reading of the user's description.
func TestDraftRequestCarriesTheAllowedScopes(t *testing.T) {
	cs := newCaptureServer(t, `{"kind":"unsupported","reason":"no retention stated"}`)
	p := cs.provider(t)

	_, err := p.Draft(context.Background(), DraftRequest{
		Text:   "把 exports 里 30 天没改过的 zip 收起来",
		Scopes: []string{"exports", "logs"},
		System: DraftSystemPrompt(),
	})
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	body := string(cs.body)
	for _, id := range []string{"exports", "logs"} {
		if !strings.Contains(body, id) {
			t.Fatalf("the request does not mention the allowed scope %q:\n%s", id, body)
		}
	}
	if !strings.Contains(body, "Allowed scope IDs") {
		t.Fatalf("the request does not label the scope list:\n%s", body)
	}
}

// A bounded list of logical IDs is the only inventory the model may see. This
// test fails if file names, hashes or paths ever reach the request body.
func TestDraftRequestCarriesNoInventory(t *testing.T) {
	cs := newCaptureServer(t, `{"kind":"unsupported","reason":"no retention stated"}`)
	p := cs.provider(t)

	text := "把 exports 里 30 天没改过的 zip 收起来"
	if _, err := p.Draft(context.Background(), DraftRequest{
		Text:   text,
		Scopes: []string{"exports"},
		System: DraftSystemPrompt(),
	}); err != nil {
		t.Fatalf("Draft: %v", err)
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(cs.body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("request has %d messages, want the system prompt and the user text", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("unexpected roles: %+v", req.Messages)
	}
	if req.Messages[1].Content != text {
		t.Fatalf("the user message is not the user's text: %q", req.Messages[1].Content)
	}
	for _, forbidden := range []string{"/srv/", "invoice-2024.zip", "sha256:"} {
		if strings.Contains(string(cs.body), forbidden) {
			t.Fatalf("the request leaks %q:\n%s", forbidden, cs.body)
		}
	}
}

// Snapshot of the request body. A change here is a change to what leaves the
// machine, so it has to be reviewed rather than discovered in a packet capture.
func TestDraftRequestBodyMatchesTheSnapshot(t *testing.T) {
	cs := newCaptureServer(t, `{"kind":"unsupported","reason":"no retention stated"}`)
	p := cs.provider(t)

	if _, err := p.Draft(context.Background(), DraftRequest{
		// Two scopes in the wrong order: the provider is responsible for a
		// deterministic body, not for inheriting the caller's map iteration.
		Scopes: []string{"logs", "exports"},
		Text:   "keep everything older than 30 days",
		System: DraftSystemPrompt(),
	}); err != nil {
		t.Fatalf("Draft: %v", err)
	}

	got := normalizeJSON(t, cs.body)
	golden := filepath.Join("testdata", "draft_request.golden")
	want, err := os.ReadFile(golden)
	if err != nil {
		// The snapshot is created on first run and compared afterwards; it is
		// committed, so a later change shows up as a diff.
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		t.Fatalf("snapshot %s did not exist and was written; re-run to compare", golden)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(got) {
		t.Fatalf("the request body changed\n got:\n%s\nwant:\n%s", got, want)
	}
}

// normalizeJSON re-encodes with sorted keys and two-space indentation so the
// snapshot diff is readable.
func normalizeJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// json.MarshalIndent sorts map keys, so the diff is stable.
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("re-encode body: %v", err)
	}
	return string(out) + "\n"
}

// The loader validates one provider name and the adapter accepted a different
// one, so no configuration the loader approved could ever construct a provider:
// `rule draft` was unreachable, not merely misconfigured. One constant, used by
// both sides, is the fix.
func TestTheDefaultConfigCanConstructAProvider(t *testing.T) {
	m := *config.Default().Model
	m.Enabled = true
	m.BaseURL = "https://model.example.invalid/v1/chat/completions"
	t.Setenv(m.APIKeyEnv, "test-key")

	if _, err := NewHTTPProvider(m); err != nil {
		t.Fatalf("the documented default provider is refused by the adapter: %v", err)
	}

	m.Provider = config.ProviderChatCompletionsJSON
	if _, err := NewHTTPProvider(m); err != nil {
		t.Fatalf("the validated provider name is refused by the adapter: %v", err)
	}
}
