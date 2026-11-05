// Package notify delivers the one message the product sends: a system
// notification that says how many files were removed, how many were moved into
// the observation area, and where the details are.
//
// It exists as its own package because "was the user told" is a question the
// rest of the program has to be able to ask and answer. The earlier shape wrote
// a line to the log and treated that as notification; a log nobody reads is not
// a way of telling anyone anything, and a permanent deletion must not proceed on
// the strength of a line in a file.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

// Channels. One exists on purpose: this build runs on a Mac, and the document
// asks for one system notification plus a local details entry rather than a
// notification platform.
const (
	ChannelMacOS = "macos_notification"
	ChannelNone  = "none"
)

// Result is what one delivery attempt achieved.
//
// Attempted is separate from Delivered so that "we chose not to try" cannot be
// read as "we tried and the user was told". Both are failures to inform, but only
// one of them is an error in the environment, and an operator fixing the wrong
// one wastes their time.
type Result struct {
	Channel   string `json:"channel"`
	Detail    string `json:"detail,omitempty"`
	Delivered bool   `json:"delivered"`
	Attempted bool   `json:"attempted"`
}

// Sender delivers one notification.
type Sender interface {
	Send(ctx context.Context, title, body string) Result
}

// Runner executes an external command. It is a seam, not configuration: nothing
// reads it from the environment, and production supplies exec.CommandContext.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// MacOS sends one notification through the system's own notification centre.
//
// The command is osascript with a fixed script; nothing is shell-expanded, so a
// file name containing quotes cannot become part of the script. A failure - no
// GUI session, notifications disabled, the binary missing - is returned as a
// failure. There is no fallback that reports success: a notification the user
// never saw must not satisfy a gate that exists to protect them.
type MacOS struct {
	Run Runner
}

// Send implements Sender.
func (m MacOS) Send(ctx context.Context, title, body string) Result {
	run := m.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	script := "display notification " + appleScriptString(body) +
		" with title " + appleScriptString(title)
	out, err := run(ctx, "osascript", "-e", script)
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return Result{Channel: ChannelMacOS, Attempted: true, Delivered: false, Detail: detail}
	}
	return Result{Channel: ChannelMacOS, Attempted: true, Delivered: true}
}

// None is the sender for a user who asked not to be notified. It never claims a
// delivery.
type None struct{}

// Send implements Sender.
func (None) Send(context.Context, string, string) Result {
	return Result{Channel: ChannelNone, Attempted: false, Delivered: false,
		Detail: "notifications are turned off in the confirmed cleanup policy"}
}

// appleScriptString renders a Go string as an AppleScript string literal.
//
// Backslash and double quote are the only characters that can end the literal
// early. Newlines are legal inside one and are kept.
func appleScriptString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// EventAction and EventPhase identify a delivery record in the audit log. It is a
// phase=info event with no item, because a notification is about a run, not about
// one file.
const (
	EventAction = "notify"
	EventPhase  = domain.PhaseInfo
)

// Record stores the outcome of one delivery attempt.
//
// It is written for successes and failures alike. A record that only exists when
// delivery worked cannot answer "was the user told", which is the only question
// anyone asks of it.
func Record(ctx context.Context, st *store.Store, nowNS int64, summary string, res Result) error {
	payload, err := json.Marshal(struct {
		Summary string `json:"summary,omitempty"`
		Result
	}{Summary: summary, Result: res})
	if err != nil {
		return errors.New("the notification outcome could not be encoded: " + err.Error())
	}
	raw := string(payload)
	return st.AppendEvent(ctx, store.Event{
		EventID:      domain.NewID("ev"),
		Action:       EventAction,
		Phase:        EventPhase,
		OccurredAtNS: nowNS,
		ContractJSON: `{"source":"notify"}`,
		ResultJSON:   &raw,
	})
}
