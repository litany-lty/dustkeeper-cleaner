package review

import (
	"context"
	"dustkeeper/internal/domain"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPJudgeKeepDoesNotCallMiniMax(t *testing.T) {
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}}}`))
	}))
	defer jev.Close()

	var minimaxCalls atomic.Int32
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		minimaxCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer minimax.Close()

	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	decision, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine", ScopeID: "scope-1", RelativePath: "cache/old.tmp", ContentSHA256: "abc", SizeBytes: 12})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if decision.Verdict == domain.VerdictAllow {
		t.Fatal("keep decision unexpectedly allowed the candidate")
	}
	if got := minimaxCalls.Load(); got != 0 {
		t.Fatalf("MiniMax calls = %d, want 0", got)
	}
}

func TestHTTPJudgeAllowsOnlyTwoValidIndependentDecisions(t *testing.T) {
	var minimaxRequest map[string]any
	var jevRequest struct {
		State string `json:"state"`
	}
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer jev-test" {
			t.Errorf("JEV authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&jevRequest); err != nil {
			t.Errorf("decode JEV request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.14.2","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.94,"probabilities":{"allow_review":0.92,"keep":0.03,"insufficient":0.05}}}}`))
	}))
	defer jev.Close()
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer minimax-test" || r.Header.Get("x-api-key") != "minimax-test" {
			t.Error("MiniMax compatibility authentication headers are missing")
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&minimaxRequest); err != nil {
			t.Errorf("decode MiniMax request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1-20261030","content":[{"type":"thinking","thinking":"private analysis","signature":"sig-1"},{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`))
	}))
	defer minimax.Close()

	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-latest", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1-20261030", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	decision, err := judge.Review(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if decision.Verdict != domain.VerdictAllow || decision.JEVModel != "jev-1.14.2" || decision.LLMModel != "MiniMax-M2.1-20261030" {
		t.Fatalf("decision = %+v", decision)
	}
	requestJSON, _ := json.Marshal(minimaxRequest)
	if strings.Contains(string(requestJSON), "jev-1.14.2") || strings.Contains(string(requestJSON), "allow_review") {
		t.Fatalf("MiniMax request received the JEV result: %s", requestJSON)
	}
	messages, _ := minimaxRequest["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(string)
	if jevRequest.State == "" || !strings.Contains(content, jevRequest.State) {
		t.Fatalf("models did not receive the same candidate facts: JEV state=%q MiniMax=%s", jevRequest.State, requestJSON)
	}
	if got := minimaxRequest["max_tokens"]; got != float64(4096) {
		t.Fatalf("max_tokens = %#v", got)
	}
	if got := minimaxRequest["stream"]; got != false {
		t.Fatalf("stream = %#v", got)
	}
	if _, exists := minimaxRequest["thinking"]; exists {
		t.Fatal("unsupported thinking field was sent")
	}
}

func TestHTTPJudgeIgnoresThinkingDecisionAndUsesFinalText(t *testing.T) {
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(allowJEVBody))
	}))
	defer jev.Close()
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M3.1-Flash","content":[{"type":"thinking","thinking":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}","signature":"sig-hostile"},{"type":"text","text":"{\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"}],"stop_reason":"end_turn"}`))
	}))
	defer minimax.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M3.1-Flash", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	decision, err := judge.Review(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if decision.Verdict == domain.VerdictAllow || decision.Reason != "still_needed" {
		t.Fatalf("decision = %+v, want final text denial", decision)
	}
}

func TestHTTPJudgePurgePromptsPermanentAction(t *testing.T) {
	var jevRequest struct {
		State     string         `json:"state"`
		Questions map[string]any `json:"questions"`
	}
	var minimaxRequest struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&jevRequest); err != nil {
			t.Errorf("decode JEV request: %v", err)
		}
		_, _ = w.Write([]byte(allowJEVBody))
	}))
	defer jev.Close()
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&minimaxRequest); err != nil {
			t.Errorf("decode MiniMax request: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`))
	}))
	defer minimax.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	candidate := testCandidate()
	candidate.Action = "purge"
	if _, err := judge.Review(context.Background(), candidate); err != nil {
		t.Fatalf("Review: %v", err)
	}
	questionJSON, _ := json.Marshal(jevRequest.Questions)
	joined := strings.ToLower(string(questionJSON) + " " + minimaxRequest.Messages[0].Content)
	for _, want := range []string{"permanent", "observation", "cannot be restored"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("purge prompts do not mention %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "enter quarantine") || strings.Contains(joined, "safe to isolate") {
		t.Fatalf("purge prompts used quarantine reasoning: %s", joined)
	}
	if !strings.Contains(jevRequest.State, `"action":"purge"`) {
		t.Fatalf("JEV state omitted purge action: %s", jevRequest.State)
	}
}

func TestHTTPJudgeFailuresStayClosed(t *testing.T) {
	tests := []struct {
		name    string
		jevBody string
		llmBody string
		wantLLM int32
	}{
		{
			name:    "invalid probability distribution",
			jevBody: `{"model":"jev-1.13.0","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.99,"probabilities":{"allow_review":0.95,"keep":0.05,"insufficient":0.05}}}}`,
		},
		{
			name:    "duplicate decision key",
			jevBody: allowJEVBody,
			llmBody: `{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"text","text":"{\"decision\":\"allow\",\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"}],"stop_reason":"end_turn"}`,
			wantLLM: 1,
		},
		{
			name:    "truncated response",
			jevBody: allowJEVBody,
			llmBody: `{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"max_tokens"}`,
			wantLLM: 1,
		},
		{
			name:    "tool use block",
			jevBody: allowJEVBody,
			llmBody: `{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"tool_use"},{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`,
			wantLLM: 1,
		},
		{
			name:    "unknown block",
			jevBody: allowJEVBody,
			llmBody: `{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"image"},{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`,
			wantLLM: 1,
		},
		{
			name:    "multiple text blocks",
			jevBody: allowJEVBody,
			llmBody: `{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"text","text":"{\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"},{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`,
			wantLLM: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.jevBody)) }))
			defer jev.Close()
			var calls atomic.Int32
			minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(tc.llmBody))
			}))
			defer minimax.Close()
			judge, err := NewHTTPJudge(
				Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
				Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
			)
			if err != nil {
				t.Fatalf("NewHTTPJudge: %v", err)
			}
			decision, err := judge.Review(context.Background(), testCandidate())
			if err == nil || decision.Verdict == domain.VerdictAllow {
				t.Fatalf("decision=%+v err=%v, want fail closed", decision, err)
			}
			if got := calls.Load(); got != tc.wantLLM {
				t.Fatalf("MiniMax calls = %d, want %d", got, tc.wantLLM)
			}
		})
	}
}

func TestHTTPJudgeDoesNotLeakKeysOrFollowRedirects(t *testing.T) {
	const secret = "secret-key-must-not-leak"
	redirectCalls := atomic.Int32{}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectCalls.Add(1) }))
	defer target.Close()
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"?token="+secret)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer jev.Close()
	minimax := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer minimax.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: secret},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: secret},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	decision, err := judge.Review(context.Background(), testCandidate())
	if err == nil || decision.Verdict == domain.VerdictAllow {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked API key: %v", err)
	}
	if got := redirectCalls.Load(); got != 0 {
		t.Fatalf("redirect target calls = %d", got)
	}

	_, err = NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: secret},
	)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("missing-key error = %v", err)
	}
	_, err = NewHTTPJudge(
		Endpoint{URL: jev.URL + "?access_token=" + secret, Model: "jev-1.13.0", APIKey: secret},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: secret},
	)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("access-bearing URL error = %v", err)
	}
}

func TestHTTPJudgeHonorsCallerTimeout(t *testing.T) {
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(allowJEVBody))
	}))
	defer jev.Close()
	minimax := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer minimax.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	decision, err := judge.Review(ctx, testCandidate())
	if err == nil || decision.Verdict == domain.VerdictAllow {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

const allowJEVBody = `{"model":"jev-1.13.0","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.95,"probabilities":{"allow_review":0.92,"keep":0.03,"insufficient":0.05}}}}`

func testCandidate() Facts {
	return Facts{Category: "temporary_name_only", Action: "quarantine", ScopeID: "scope-1", RelativePath: "cache/old.tmp", ContentSHA256: "abc", SizeBytes: 12}
}

// The production path (ReviewBoth=false) records the requests it actually
// sends. It must never be switched to a debug second opinion just to obtain
// timings, because that would call MiniMax after a final JEV keep.
func TestProductionTimingRecordsOnlyJEVOnKeep(t *testing.T) {
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}}}`))
	}))
	defer jev.Close()
	var minimaxCalls atomic.Int32
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		minimaxCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer minimax.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if judge.ReviewBoth {
		t.Fatal("this test must exercise the production path")
	}
	d, err := judge.Review(context.Background(), testCandidate())
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict == domain.VerdictAllow {
		t.Fatal("a keep verdict unexpectedly allowed the candidate")
	}
	if got := minimaxCalls.Load(); got != 0 {
		t.Fatalf("MiniMax calls = %d, want 0", got)
	}
	if judge.Stats().MiniMax != 0 {
		t.Fatalf("the judge attempted MiniMax: %+v", judge.Stats())
	}
	if len(d.RequestTimings) != 1 {
		t.Fatalf("production keep recorded %d timings, want exactly one JEV row: %+v", len(d.RequestTimings), d.RequestTimings)
	}
	row := d.RequestTimings[0]
	if row.Provider != "jev" || row.Outcome != "ok" || row.ElapsedMS < 0 || row.StagesMS == nil {
		t.Fatalf("wrong JEV timing row: %+v", row)
	}
}

func TestProductionTimingRecordsBothLayers(t *testing.T) {
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(allowJEVBody))
	}))
	defer jev.Close()
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`))
	}))
	defer minimax.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.13.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	d, err := judge.Review(context.Background(), testCandidate())
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != domain.VerdictAllow {
		t.Fatalf("decision = %+v, want allow", d)
	}
	if judge.Stats().Attempts != 2 {
		t.Fatalf("HTTP attempts = %d, want exactly two (one per layer)", judge.Stats().Attempts)
	}
	if len(d.RequestTimings) != 2 {
		t.Fatalf("production two-layer review recorded %d timings, want 2: %+v", len(d.RequestTimings), d.RequestTimings)
	}
	seen := map[string]bool{}
	for _, row := range d.RequestTimings {
		if row.Outcome != "ok" || row.ElapsedMS < 0 || row.StagesMS == nil {
			t.Fatalf("incomplete timing row: %+v", row)
		}
		seen[row.Provider] = true
	}
	if !seen["jev"] || !seen["minimax"] {
		t.Fatalf("timings do not name both providers: %+v", seen)
	}
}

func TestProductionTimingMiniMaxTimeoutKeepsJEVFact(t *testing.T) {
	j := generalJudge(t, allowJEVBody, `{}`)
	// A fake transport lets the second layer fail at a known stage without
	// waiting for a real timeout; the production short-circuit is untouched.
	j.jevClient = &http.Client{Transport: generalTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(allowJEVBody)), Header: make(http.Header), Request: r}, nil
	})}
	j.client = &http.Client{Transport: generalTransport(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	if j.ReviewBoth {
		t.Fatal("this test must exercise the production path")
	}
	d, err := j.Review(context.Background(), testCandidate())
	if err == nil || d.Verdict == domain.VerdictAllow {
		t.Fatalf("a MiniMax timeout must fail closed: d=%+v err=%v", d, err)
	}
	if len(d.RequestTimings) != 2 {
		t.Fatalf("timeout kept %d timings, want the JEV fact + MiniMax failure: %+v", len(d.RequestTimings), d.RequestTimings)
	}
	if jev := d.RequestTimings[0]; jev.Provider != "jev" || jev.Outcome != "ok" {
		t.Fatalf("the JEV request fact was lost: %+v", jev)
	}
	mm := d.RequestTimings[1]
	if mm.Provider != "minimax" || mm.Outcome != "timeout" || mm.ElapsedMS < 0 || mm.Phase == "" {
		t.Fatalf("MiniMax failure stage or elapsed time missing: %+v", mm)
	}
	if j.Stats().MiniMax != 1 {
		t.Fatalf("MiniMax was retried: attempts=%d", j.Stats().MiniMax)
	}
}

func TestProductionTimingBudgetRefusalHasNoTimings(t *testing.T) {
	j := generalJudge(t, allowJEVBody, `{}`)
	j.calls.Store(1)
	j.MaxCalls = 1
	if j.ReviewBoth {
		t.Fatal("this test must exercise the production path")
	}
	d, _ := j.Review(context.Background(), testCandidate())
	if len(d.RequestTimings) != 0 {
		t.Fatalf("a budget refusal invented a request timing: %+v", d.RequestTimings)
	}
	if j.Stats().Attempts != 0 {
		t.Fatalf("a budget refusal made a paid request: %+v", j.Stats())
	}
}
