package review

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A recorded reason is the only trace a failed review leaves behind, so its
// wording has to separate the failures an operator can act on: waiting longer
// fixes a timeout, nothing fixes a refused proxy, and a cancelled request means
// the caller gave up. All three used to read "model request failed".
func TestTransportFailureReasonNamesTheCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "timeout", err: &net.OpError{Op: "dial", Err: timeoutError{}}, want: "model request timed out after 20s"},
		{name: "deadline", err: fmt.Errorf("wait: %w", context.DeadlineExceeded), want: "model request timed out after 20s"},
		{name: "cancelled", err: fmt.Errorf("wait: %w", context.Canceled), want: "model request was cancelled"},
		{name: "refused", err: errors.New("dial tcp 127.0.0.1:7890: connect: connection refused"),
			want: "model request failed before any response (connection, proxy or TLS)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := transportFailureReason(c.err, 20*time.Second)
			if got != c.want {
				t.Fatalf("reason = %q, want %q", got, c.want)
			}
			// The transport error string can carry the proxy address, and this
			// text is stored in the run record.
			for _, leak := range []string{"127.0.0.1", "dial", "http"} {
				if strings.Contains(got, leak) {
					t.Fatalf("reason %q echoed transport detail %q", got, leak)
				}
			}
		})
	}
}

// The timeout must be visible in the reason: a hidden ceiling is the
// difference between "the model is slow" and "the model refused".
func TestTransportFailureReasonStatesTheConfiguredTimeout(t *testing.T) {
	got := transportFailureReason(timeoutError{}, 45*time.Second)
	if !strings.Contains(got, "45s") {
		t.Fatalf("reason = %q; the timeout it describes is not in it", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
