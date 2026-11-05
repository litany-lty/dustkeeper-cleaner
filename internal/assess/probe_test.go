package assess

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRound6UnavailableUsageAndProcessProbeRemainUnknown(t *testing.T) {
	bad := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("denied") }
	p := ProbeOpenFiles(context.Background(), "/scoped", bad)
	if p.Coverage == "complete" || len(p.Paths) != 0 {
		t.Fatal("failed process listing claimed complete")
	}
	u := ProbeLastUsed(context.Background(), "/scoped/a", func(context.Context, string, ...string) ([]byte, error) { return []byte("(null)"), nil })
	if !u.At.IsZero() || u.Status != "unknown" {
		t.Fatal("missing last-use became unused")
	}
	u = ProbeLastUsed(context.Background(), "/scoped/a", func(context.Context, string, ...string) ([]byte, error) {
		return []byte("2026-10-30 12:00:00 +0000"), nil
	})
	if u.At.IsZero() {
		t.Fatal("known timestamp lost")
	}
	r := Assess(Facts{Path: "a.tmp", Regular: true, NLink: 1, LastUsed: u.At}, 30, time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC))
	if r.Disposition != "keep" {
		t.Fatal("known recent use ignored")
	}
}
