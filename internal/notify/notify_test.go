package notify

import (
	"context"
	"errors"
	"testing"
)

func TestMacOSReportsFailureAsFailure(t *testing.T) {
	// The gate this feeds exists to protect a user from a permanent deletion they
	// were never told about. An environment with no GUI session is the ordinary
	// case for a scheduled task, so it is the case that must be reported
	// honestly rather than smoothed over.
	cases := []struct {
		name   string
		run    Runner
		detail string
	}{
		{
			name: "osascript fails with output",
			run: func(context.Context, string, ...string) ([]byte, error) {
				return []byte("execution error: Not authorized to send Apple events (-1743)"),
					errors.New("exit status 1")
			},
			detail: "Not authorized",
		},
		{
			name: "osascript fails silently",
			run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("exec: \"osascript\": executable file not found in $PATH")
			},
			detail: "not found in $PATH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := MacOS{Run: tc.run}.Send(context.Background(), "title", "body")
			if res.Delivered {
				t.Fatal("a failed delivery was reported as delivered")
			}
			if !res.Attempted {
				t.Fatal("a failed delivery was reported as not attempted")
			}
			if res.Channel != ChannelMacOS {
				t.Fatalf("channel = %q", res.Channel)
			}
			if res.Detail == "" || !contains(res.Detail, tc.detail) {
				t.Fatalf("detail %q does not carry %q", res.Detail, tc.detail)
			}
		})
	}
}

func TestMacOSReportsSuccessOnlyOnSuccess(t *testing.T) {
	var gotName string
	var gotArgs []string
	res := MacOS{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return nil, nil
	}}.Send(context.Background(), "本次清理", "删除 2 个文件；3 个进入待删除区")

	if !res.Delivered || !res.Attempted || res.Detail != "" {
		t.Fatalf("successful delivery reported as %+v", res)
	}
	if gotName != "osascript" || len(gotArgs) != 2 || gotArgs[0] != "-e" {
		t.Fatalf("command was %s %v", gotName, gotArgs)
	}
	if !contains(gotArgs[1], "display notification") {
		t.Fatalf("script = %q", gotArgs[1])
	}
}

// TestQuotedTextCannotEndTheScriptEarly matters because the body carries file
// names, and a file name is user-controlled text.
func TestQuotedTextCannotEndTheScriptEarly(t *testing.T) {
	var script string
	res := MacOS{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		script = args[1]
		return nil, nil
	}}.Send(context.Background(), `a "quoted" title`, "body \" with a quote\ntell application \"Finder\" to quit")

	if !res.Delivered {
		t.Fatalf("delivery reported as %+v", res)
	}
	if count := occurrences(script, "display notification"); count != 1 {
		t.Fatalf("script has %d statements: %s", count, script)
	}
	if contains(script, "tell application \"Finder\"") {
		t.Fatalf("a body escaped its string literal: %s", script)
	}
	if !contains(script, `\"`) {
		t.Fatalf("quotes were dropped rather than escaped: %s", script)
	}
}

func TestNoneNeverClaimsDelivery(t *testing.T) {
	res := None{}.Send(context.Background(), "t", "b")
	if res.Delivered || res.Attempted {
		t.Fatalf("the off switch claimed a delivery: %+v", res)
	}
	if res.Channel != ChannelNone || res.Detail == "" {
		t.Fatalf("the off switch does not explain itself: %+v", res)
	}
}

func TestAppleScriptStringEscapesOnlyWhatEndsALiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"line\nbreak", "\"line\nbreak\""},
	}
	for _, tc := range cases {
		if got := appleScriptString(tc.in); got != tc.want {
			t.Fatalf("appleScriptString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func occurrences(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}
