package review

import (
	"context"
	"dustkeeper/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDebugTimeoutNamesTheFailedNetworkStage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(40 * time.Millisecond) }))
	defer server.Close()
	j, err := NewHTTPJudge(Endpoint{URL: server.URL, Model: "jev-1.14.0", APIKey: "test"}, Endpoint{URL: server.URL, Model: "MiniMax-test", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	j.ReviewBoth = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	d, err := j.Review(ctx, testCandidate())
	if err == nil || !strings.Contains(d.ModelError, "timeout") || !strings.Contains(d.ModelError, "response_headers") {
		t.Fatalf("network failure lost stage: %+v %v", d, err)
	}
}

func TestDebugMiniMaxWaitDoesNotChangeJEVWait(t *testing.T) {
	j, err := NewHTTPJudge(Endpoint{URL: "http://127.0.0.1:1", Model: "jev-test", APIKey: "test"}, Endpoint{URL: "http://127.0.0.1:1", Model: "MiniMax-test", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = j.SetMiniMaxTimeout(45 * time.Second); err != nil {
		t.Fatal(err)
	}
	if j.client.Timeout != 45*time.Second || j.jevClient.Timeout != requestTimeout {
		// The point is that the JEV bound did not move, not that it is 20s.
		t.Fatal("diagnostic wait changed unrelated provider")
	}
	if j.SetMiniMaxTimeout(61*time.Second) == nil {
		t.Fatal("unbounded diagnostic wait allowed")
	}
}

func TestDebugEnvelopeDiagnosticDoesNotEchoProviderText(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":1008,"status_msg":"private-key-and-user-content"}}`)
	var response minimaxResponse
	err := miniMaxEnvelopeError(raw, decodeMiniMaxEnvelope(raw, &response))
	if !strings.Contains(err.Error(), "provider_status=1008") || strings.Contains(err.Error(), "private-key") {
		t.Fatalf("unsafe or empty diagnostic: %v", err)
	}
}

func TestMiniMaxOptionalProviderStatusIsValidated(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		valid       bool
	}{
		{"success", `,"base_resp":{"status_code":0,"status_msg":"success"}`, true},
		{"provider_error", `,"base_resp":{"status_code":1008,"status_msg":"failure"}`, false},
		{"missing_status", `,"base_resp":{"status_msg":"success"}`, false},
		{"null", `,"base_resp":null`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := `{"id":"msg","type":"message","role":"assistant","model":"MiniMax-test","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"` + test.extra + `}`
			var response minimaxResponse
			err := decodeMiniMaxEnvelope([]byte(payload), &response)
			if (err == nil) != test.valid {
				t.Fatalf("provider status validation valid=%v: %v", test.valid, err)
			}
		})
	}
}

func TestMiniMaxOutputTokenDetailsAreMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jev" {
			_, _ = w.Write([]byte(allowJEVBody))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","model":"MiniMax-test","content":[{"type":"text","text":"{\"decision\":\"insufficient\",\"reason_code\":\"insufficient_evidence\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens_details":{"reasoning_tokens":8}}}`))
	}))
	defer server.Close()
	j, err := NewHTTPJudge(Endpoint{URL: server.URL + "/jev", Model: "jev-1.13.0", APIKey: "test"}, Endpoint{URL: server.URL + "/minimax", Model: "MiniMax-test", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := j.Review(context.Background(), testCandidate())
	if err != nil || d.Verdict == domain.VerdictAllow || d.LLMUsage == nil || d.LLMUsage.OutputTokens != 12 {
		t.Fatalf("valid token details blocked model decision: %+v %v", d, err)
	}
}

func TestDebugCallLimitCountsFailedAndSuccessfulAttempts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	j, err := NewHTTPJudge(Endpoint{URL: server.URL, Model: "jev-1.14.0", APIKey: "test"}, Endpoint{URL: server.URL, Model: "MiniMax-test", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	j.MaxCalls = 1
	for i := 0; i < 2; i++ {
		decision, err := j.Review(context.Background(), testCandidate())
		if err == nil {
			t.Fatal("unavailable provider allowed")
		}
		if decision.ModelError == "" {
			t.Fatal("debug omitted safe provider failure detail")
		}
	}
	if j.Calls() != 1 || calls.Load() != 1 {
		t.Fatalf("call limit bypassed: attempts=%d calls=%d", j.Calls(), calls.Load())
	}
}

func TestDebugSecondOpinionCannotOverrideJEVRefusal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/jev" {
			_, _ = w.Write([]byte(`{"model":"jev-1.14.0","answers":{"quarantine_disposition":{"type":"choice","choice":"insufficient","confidence":0.95,"probabilities":{"allow_review":0.01,"keep":0.04,"insufficient":0.95}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"debug","type":"message","role":"assistant","model":"MiniMax-test","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\",\"explanation\":\"Synthetic independent second opinion.\",\"missing_evidence\":[]}"}],"stop_reason":"end_turn","stop_sequence":null}`))
	}))
	defer server.Close()
	j, err := NewHTTPJudge(Endpoint{URL: server.URL + "/jev", Model: "jev-1.14.0", APIKey: "test"}, Endpoint{URL: server.URL + "/minimax", Model: "MiniMax-test", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	j.ReviewBoth = true
	j.MaxCalls = 2
	d, err := j.Review(context.Background(), testCandidate())
	if err != nil || calls.Load() != 2 || d.LLMDecision != "allow" || d.Verdict == domain.VerdictAllow || d.Reason != "insufficient" {
		t.Fatalf("debug second opinion missing or overrode rejection: %+v calls=%d err=%v", d, calls.Load(), err)
	}
}
