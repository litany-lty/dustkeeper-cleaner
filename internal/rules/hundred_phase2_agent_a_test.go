package rules

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dustkeeper/internal/config"
)

func TestA09ProviderRejectsIncompleteOrAccessBearingURLs(t *testing.T) {
	for _, raw := range []string{
		"https:///v1/chat/completions",
		"https://user:secret@example.com/v1/chat/completions",
		"https://example.com/v1/chat/completions?access_token=secret",
		"https://example.com/v1/chat/completions#fragment",
		"https://example.com:70000/v1/chat/completions",
	} {
		m := config.Default().Model
		m.Enabled = true
		m.BaseURL = raw
		m.APIKeyEnv = ""
		if _, err := NewHTTPProvider(*m); err == nil {
			t.Errorf("unsafe or incomplete provider URL accepted: %q", raw)
		}
	}
}

func TestA11ProviderRejectsTrailingResponseDocuments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"unsupported\",\"reason\":\"fixture\"}"},"finish_reason":"stop"}]} {"smuggled":true}`))
	}))
	defer srv.Close()
	m := *config.Default().Model
	m.Enabled = true
	m.BaseURL = srv.URL
	m.APIKeyEnv = ""
	provider, err := NewHTTPProvider(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Draft(context.Background(), DraftRequest{Text: "fixture"}); err == nil {
		t.Fatal("provider accepted a second JSON document after the chat response")
	}
}

func TestA12ProviderRejectsAmbiguousMultipleChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"unsupported\",\"reason\":\"first\"}"},"finish_reason":"stop"},{"message":{"content":"{\"kind\":\"draft\",\"dsl\":{}}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	m := *config.Default().Model
	m.Enabled = true
	m.BaseURL = srv.URL
	m.APIKeyEnv = ""
	provider, err := NewHTTPProvider(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Draft(context.Background(), DraftRequest{Text: "fixture"}); err == nil {
		t.Fatal("provider silently selected the first of multiple model choices")
	}
}

func TestA13ProviderRejectsTruncatedChoice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"kind\":\"unsupported\",\"reason\":\"syntactically complete prefix\"}"},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()
	m := *config.Default().Model
	m.Enabled = true
	m.BaseURL = srv.URL
	m.APIKeyEnv = ""
	provider, err := NewHTTPProvider(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Draft(context.Background(), DraftRequest{Text: "fixture"}); err == nil {
		t.Fatal("provider accepted a choice explicitly truncated by the token limit")
	}
}

func TestA10ProviderRejectsKeysThatCannotBeHTTPHeaders(t *testing.T) {
	for i, key := range []string{" key", "key\t", "left\nright", "left\x7fright"} {
		env := "DUSTKEEPER_A10_KEY_" + string(rune('A'+i))
		t.Setenv(env, key)
		m := *config.Default().Model
		m.Enabled = true
		m.BaseURL = "http://127.0.0.1:1/v1/chat/completions"
		m.APIKeyEnv = env
		if _, err := NewHTTPProvider(m); err == nil {
			t.Errorf("non-header-safe API key accepted: %q", key)
		}
	}
}
