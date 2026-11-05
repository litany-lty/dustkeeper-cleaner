package review

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"dustkeeper/internal/domain"
)

// miniMaxEnvelope wraps one decision in the Anthropic-shaped message the second
// reviewer is expected to return.
func miniMaxEnvelope(decision string) string {
	return `{"id":"msg-test","type":"message","role":"assistant","model":"mm-test","content":[{"type":"text","text":` +
		strconv.Quote(decision) + `}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":5}}`
}

func TestJEVDiagnosticSeparatesThresholdFromChoice(t *testing.T) {
	cases := []struct {
		choice     string
		confidence float64
		allow      float64
		block      string
	}{{"allow_review", .8, .95, "confidence_below_threshold"}, {"allow_review", .95, .85, "probability_below_threshold"}, {"insufficient", .95, .01, "model_insufficient"}}
	for _, tc := range cases {
		t.Run(tc.block, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/jev" {
					fmt.Fprintf(w, `{"model":"jev-test","answers":{"quarantine_disposition":{"type":"choice","choice":%q,"confidence":%g,"probabilities":{"allow_review":%g,"keep":0,"insufficient":%g}}},"usage":{"input_tokens":20,"output_tokens":2}}`, tc.choice, tc.confidence, tc.allow, 1-tc.allow)
					return
				}
				fmt.Fprint(w, miniMaxEnvelope(`{"decision":"insufficient","reason_code":"insufficient_evidence"}`))
			}))
			defer srv.Close()
			j, err := NewHTTPJudge(Endpoint{URL: srv.URL + "/jev", Model: "jev-test", APIKey: "fixture"}, Endpoint{URL: srv.URL + "/mm", Model: "mm-test", APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			d, err := j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"})
			if err != nil {
				t.Fatal(err)
			}
			if d.Verdict == domain.VerdictAllow || d.JEV == nil || d.JEV.Choice != tc.choice || d.JEV.Confidence != tc.confidence || len(d.JEV.Blockers) != 1 || d.JEV.Blockers[0] != tc.block || d.JEV.Usage.InputTokens != 20 {
				t.Fatalf("lost diagnostic: %+v", d)
			}
		})
	}
}

func TestJEVExplicitProxyDoesNotRouteMiniMaxOrFallBack(t *testing.T) {
	var directJEV, mini, proxyHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jev" {
			directJEV.Add(1)
			http.Error(w, "unexpected direct route", 500)
			return
		}
		mini.Add(1)
		fmt.Fprint(w, `{"id":"msg-fixture","type":"message","role":"assistant","model":"mm-test","content":[{"type":"text","text":"{\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"}],"stop_reason":"end_turn"}`)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.URL.Path != "/jev" {
			t.Error("MiniMax went through JEV proxy")
		}
		fmt.Fprint(w, `{"model":"jev-test","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.99,"probabilities":{"allow_review":0.99,"keep":0.005,"insufficient":0.005}}}}`)
	}))
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	j, err := NewHTTPJudge(Endpoint{URL: target.URL + "/jev", Model: "jev-test", APIKey: "fixture", ProxyURL: proxy.URL}, Endpoint{URL: target.URL + "/mini", Model: "mm-test", APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"})
	if err != nil || d.Verdict == domain.VerdictAllow || d.JEV.Transport != "explicit_http_proxy" || proxyHits.Load() != 1 || directJEV.Load() != 0 || mini.Load() != 1 {
		t.Fatalf("wrong route: %+v %v proxy=%d direct=%d mini=%d", d, err, proxyHits.Load(), directJEV.Load(), mini.Load())
	}
	proxy.Close()
	if _, err = j.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); err == nil {
		t.Fatal("dead proxy accepted")
	}
	if directJEV.Load() != 0 || mini.Load() != 1 {
		t.Fatal("proxy failure fell back or reached MiniMax")
	}
}
