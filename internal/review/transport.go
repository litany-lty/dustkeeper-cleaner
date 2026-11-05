package review

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http/httptrace"
	"sync"
	"time"
)

// RequestTiming contains no URL, address, headers, body or provider message.
type RequestTiming struct {
	Provider  string           `json:"provider"`
	Outcome   string           `json:"outcome"`
	Phase     string           `json:"phase"`
	ElapsedMS int64            `json:"elapsed_ms"`
	StagesMS  map[string]int64 `json:"stages_ms"`
}
type timingKey struct{}
type timingBatch struct {
	mu   sync.Mutex
	rows []RequestTiming
}

func (b *timingBatch) add(t RequestTiming) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rows = append(b.rows, t)
}
func (b *timingBatch) snapshot() []RequestTiming {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]RequestTiming(nil), b.rows...)
}

type requestTiming struct {
	mu              sync.Mutex
	start           time.Time
	provider, phase string
	stages          map[string]int64
}

func newRequestTiming(provider string) *requestTiming {
	return &requestTiming{start: time.Now(), provider: provider, phase: "connect", stages: map[string]int64{}}
}
func (t *requestTiming) mark(event, phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stages[event] = time.Since(t.start).Milliseconds()
	t.phase = phase
}
func (t *requestTiming) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { t.mark("dns_start", "dns") }, DNSDone: func(i httptrace.DNSDoneInfo) {
			phase := "connect"
			if i.Err != nil {
				phase = "dns"
			}
			t.mark("dns_done", phase)
		},
		ConnectStart: func(string, string) { t.mark("connect_start", "connect") }, ConnectDone: func(_ string, _ string, err error) {
			phase := "tls"
			if err != nil {
				phase = "connect"
			}
			t.mark("connect_done", phase)
		},
		TLSHandshakeStart: func() { t.mark("tls_start", "tls") }, TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			phase := "response_headers"
			if err != nil {
				phase = "tls"
			}
			t.mark("tls_done", phase)
		},
		GotConn: func(httptrace.GotConnInfo) { t.mark("connection_ready", "response_headers") }, GotFirstResponseByte: func() { t.mark("first_byte", "response_body") },
	}
}
func (t *requestTiming) finish(outcome string) RequestTiming {
	t.mu.Lock()
	defer t.mu.Unlock()
	stages := map[string]int64{}
	for k, v := range t.stages {
		stages[k] = v
	}
	return RequestTiming{Provider: t.provider, Outcome: outcome, Phase: t.phase, ElapsedMS: time.Since(t.start).Milliseconds(), StagesMS: stages}
}
func (t *requestTiming) failure(err error) error {
	kind := transportFailureKind(err)
	v := t.finish(kind)
	return fmt.Errorf("model request %s at %s after %dms", kind, v.Phase, v.ElapsedMS)
}

func transportFailureKind(err error) string {
	kind := "transport_error"
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		kind = "timeout"
	} else if errors.Is(err, context.Canceled) {
		kind = "cancelled"
	}
	return kind
}

// transportFailureReason names the failure class in words an operator can act
// on. The underlying error is never echoed: a transport error may carry the
// proxy address or the endpoint, and this text is stored in the run record.
// A timeout must stay distinguishable from a refusal, because the first is
// fixed by waiting longer and the second is not.
func transportFailureReason(err error, timeout time.Duration) string {
	switch transportFailureKind(err) {
	case "timeout":
		return fmt.Sprintf("model request timed out after %s", timeout)
	case "cancelled":
		return "model request was cancelled"
	}
	return "model request failed before any response (connection, proxy or TLS)"
}
