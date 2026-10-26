package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/errs"
)

// ProviderOpenAICompatible is the only adapter shape phase one implements.
const ProviderOpenAICompatible = "openai-compatible"

// connectTimeout is fixed at 3 seconds; the total timeout comes from the
// configuration.
const connectTimeout = 3 * time.Second

// HTTPProvider is the single rule-draft adapter.
//
// Properties that matter more than features:
//   - it never follows a redirect, so a credential is never replayed to another
//     host;
//   - it never sends scanned names, hashes or inventory;
//   - a failure is reported as MODEL_UNAVAILABLE and leaves every stored rule
//     exactly as it was.
type HTTPProvider struct {
	model  config.Model
	apiKey string
	client *http.Client
}

// NewHTTPProvider validates the endpoint and builds a client with no redirects.
func NewHTTPProvider(m config.Model) (*HTTPProvider, error) {
	if !m.Enabled {
		return nil, errs.New(errs.CodeModelUnavailable,
			"the model adapter is disabled in the configuration; use `rule import` instead")
	}
	if m.Provider != ProviderOpenAICompatible {
		return nil, errs.Newf(errs.CodeInvalidArgument,
			"provider %q is not supported in phase one; use %q", m.Provider, ProviderOpenAICompatible)
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		return nil, errs.Newf(errs.CodeInvalidArgument, "base_url is not a URL: %v", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && config.IsLoopbackHost(u.Hostname())) {
		return nil, errs.New(errs.CodeInvalidArgument,
			"base_url must use https, except for an explicitly configured loopback service")
	}
	if m.RequestTimeoutSeconds <= 0 {
		return nil, errs.New(errs.CodeInvalidArgument, "request_timeout_seconds must be positive")
	}

	apiKey := ""
	if m.APIKeyEnv != "" {
		apiKey = os.Getenv(m.APIKeyEnv)
		if apiKey == "" {
			return nil, errs.Newf(errs.CodeModelUnavailable,
				"the API key environment variable %s is not set", m.APIKeyEnv)
		}
	}

	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: time.Duration(m.RequestTimeoutSeconds) * time.Second,
		DisableKeepAlives:     true,
	}
	return &HTTPProvider{
		model:  m,
		apiKey: apiKey,
		client: &http.Client{
			Transport: transport,
			Timeout:   time.Duration(m.RequestTimeoutSeconds) * time.Second,
			// No redirects at all: a 3xx is a failure, never a new request.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// chatRequest is the documented request body.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature int           `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Draft performs one request and returns the raw content string.
func (p *HTTPProvider) Draft(ctx context.Context, req DraftRequest) ([]byte, error) {
	body := chatRequest{
		Model: p.model.Model,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.Text},
		},
		Temperature: 0,
		MaxTokens:   1500,
		Stream:      false,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.model.BaseURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// The message deliberately excludes the URL and any header value.
		return nil, errs.Newf(errs.CodeModelUnavailable,
			"the rule-draft service could not be reached: %s", sanitizeNetError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, errs.New(errs.CodeModelUnavailable,
			"the rule-draft service tried to redirect; redirects are refused so a credential is never replayed")
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, errs.Newf(errs.CodeModelUnavailable,
			"the rule-draft service returned %d; existing rules are unaffected", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errs.Newf(errs.CodeModelUnavailable,
			"the rule-draft service returned %d; check the endpoint configuration", resp.StatusCode)
	}

	limit := p.model.MaxResponseBytes
	if limit <= 0 || limit > MaxDraftBytes {
		limit = MaxDraftBytes
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, errs.Newf(errs.CodeModelUnavailable, "reading the answer failed: %s", sanitizeNetError(err))
	}
	if len(raw) > limit {
		return nil, errs.Newf(errs.CodeInvalidArgument,
			"the answer exceeded the %d byte response limit", limit)
	}

	var parsed chatResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&parsed); err != nil {
		return nil, errs.New(errs.CodeModelUnavailable,
			"the rule-draft service returned a body that is not a chat completion")
	}
	if len(parsed.Choices) == 0 {
		return nil, errs.New(errs.CodeModelUnavailable, "the rule-draft service returned no choices")
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return nil, errs.New(errs.CodeModelUnavailable, "the rule-draft service returned an empty answer")
	}
	return []byte(content), nil
}

// sanitizeNetError keeps the failure class without the endpoint or credentials.
func sanitizeNetError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "://"); i > 0 {
		msg = msg[:i] + "://<redacted>"
	}
	return msg
}
