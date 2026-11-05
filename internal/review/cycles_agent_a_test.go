package review

import (
	"context"
	"dustkeeper/internal/domain"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestA04EndpointRejectsKeysThatCannotBeHTTPHeaders(t *testing.T) {
	for _, key := range []string{" key", "key\t", "left\nright", "left\rright", "left\x00right", "left\x7fright"} {
		_, err := NewHTTPJudge(
			Endpoint{URL: "http://127.0.0.1:1/j", Model: "jev-test", APIKey: key},
			Endpoint{URL: "http://127.0.0.1:1/m", Model: "mm-test", APIKey: "fixture"},
		)
		if err == nil {
			t.Fatalf("non-header-safe API key accepted: %q", key)
		}
	}
}

func TestA05MiniMaxUsageRejectsNullOptionalCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/j" {
			_, _ = w.Write([]byte(allowJEVBody))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg-fixture","type":"message","role":"assistant","model":"mm","content":[{"type":"text","text":"{\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":null}}`))
	}))
	defer srv.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: srv.URL + "/j", Model: "jev-1.13.0", APIKey: "fixture"},
		Endpoint{URL: srv.URL + "/m", Model: "mm", APIKey: "fixture"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); err == nil {
		t.Fatal("null cache usage counter was recorded as zero")
	}
}

func TestA06MiniMaxRejectsNonAssistantEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/j" {
			_, _ = w.Write([]byte(allowJEVBody))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"user","model":"mm","content":[{"type":"text","text":"{\"decision\":\"allow\",\"reason_code\":\"disposable\"}"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: srv.URL + "/j", Model: "jev-1.13.0", APIKey: "fixture"},
		Endpoint{URL: srv.URL + "/m", Model: "mm", APIKey: "fixture"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); err == nil || decision.Verdict == domain.VerdictAllow {
		t.Fatalf("non-assistant envelope authorized deletion: decision=%+v err=%v", decision, err)
	}
}

func TestA07EndTurnRejectsNonNullStopSequence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/j" {
			_, _ = w.Write([]byte(allowJEVBody))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"mm","content":[{"type":"text","text":"{\"decision\":\"deny\",\"reason_code\":\"still_needed\"}"}],"stop_reason":"end_turn","stop_sequence":"unexpected"}`))
	}))
	defer srv.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: srv.URL + "/j", Model: "jev-1.13.0", APIKey: "fixture"},
		Endpoint{URL: srv.URL + "/m", Model: "mm", APIKey: "fixture"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); err == nil {
		t.Fatal("end_turn response accepted a non-null stop_sequence")
	}
}

func TestA08JEVLatestRejectsMovingAliasAsActualModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"quarantine_disposition":{"type":"choice","choice":"keep","confidence":0.99,"probabilities":{"allow_review":0.005,"keep":0.99,"insufficient":0.005}}}}`))
	}))
	defer srv.Close()
	judge, err := NewHTTPJudge(
		Endpoint{URL: srv.URL, Model: "jev-latest", APIKey: "fixture"},
		Endpoint{URL: srv.URL, Model: "mm", APIKey: "fixture"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "quarantine"}); err == nil || decision.JEVModel != "" {
		t.Fatalf("moving alias accepted as actual model: decision=%+v err=%v", decision, err)
	}
}

func TestA01CredentialsRejectCaseFoldedCollision(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(dir, "credentials.json")
	body := `{"typesafe_api_key":"first","TYPESAFE_API_KEY":"override","minimax_api_key":"second"}`
	if err := os.WriteFile(credentials, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	s := validAgentASettings()
	s.CredentialsFile = credentials
	if _, err := s.NewService(); err == nil {
		t.Fatal("case-folded credential key collision selected an ambiguous secret")
	}
}

func TestA02SettingsRejectModelNamesWithBoundaryWhitespace(t *testing.T) {
	for _, mutate := range []func(*Settings){
		func(s *Settings) { s.JEVModel = " jev-latest" },
		func(s *Settings) { s.MiniMaxModel = "MiniMax-M3.1-Flash\t" },
		func(s *Settings) { s.MiniMaxModel = "MiniMax\nM3.1" },
	} {
		s := validAgentASettings()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Fatalf("ambiguous model name accepted: JEV=%q MiniMax=%q", s.JEVModel, s.MiniMaxModel)
		}
	}
}

func TestA03SettingsRejectOutOfRangeProviderPort(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.typesafe.ai:70000/v1/systemone",
		"https://api.minimaxi.com:70000/anthropic/v1/messages",
	} {
		s := validAgentASettings()
		if strings.Contains(endpoint, "typesafe") {
			s.JEVURL = endpoint
		} else {
			s.MiniMaxURL = endpoint
		}
		if err := s.Validate(); err == nil {
			t.Fatalf("out-of-range provider port accepted: %s", endpoint)
		}
	}
}

func validAgentASettings() Settings {
	return Settings{
		JEVProxyURL:   "http://127.0.0.1:7890",
		JEVURL:        "https://api.typesafe.ai/v1/systemone",
		JEVModel:      "jev-latest",
		MiniMaxURL:    "https://api.minimaxi.com/anthropic/v1/messages",
		MiniMaxModel:  "MiniMax-M3.1-Flash",
		MaxCandidates: 10,
	}
}
