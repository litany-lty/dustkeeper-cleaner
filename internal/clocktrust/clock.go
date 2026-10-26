// Package clocktrust answers one question: may this instant be used to prove
// that a retention period has elapsed?
//
// Three separate facts are kept apart, because collapsing them was the reason a
// single anomalous run could launder itself into a trusted baseline:
//
//	last_run_ns           any run, diagnostics only, never a trust baseline
//	last_trusted_run_ns   the last run whose wall clock was coherent
//	clock_latch           an unresolved anomaly; only an explicit acknowledgement
//	                      clears it
//
// A run that is not trusted records only that it ran. It does not move the
// baseline, so the next run cannot "resynchronise" by running twice.
package clocktrust

import (
	"context"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// Thresholds from section 11.2.
const (
	BackwardsTolerance = 5 * time.Minute
	JumpThreshold      = 48 * time.Hour
	DriftTolerance     = 5 * time.Minute
)

// Meta keys owned by this package.
const (
	MetaLastRun        = "last_run_ns"
	MetaLastTrustedRun = "last_trusted_run_ns"
	MetaLatchAt        = "clock_latch_ns"
	MetaLatchReason    = "clock_latch_reason"
	MetaAck            = "clock_ack_ns"
)

// Verdict is the answer for one instant.
type Verdict struct {
	Trusted bool   `json:"trusted"`
	Reason  string `json:"reason,omitempty"`
	Latched bool   `json:"latched"`
}

// Guard holds the fixed baseline of one run.
//
// The baseline is captured once, at start. Computing "the start" later from
// wall-minus-mono cancels itself out in the drift formula and measures nothing.
type Guard struct {
	Store *store.Store
	Clock domain.Clock
	wall0 int64
	mono0 time.Duration
}

// Start captures the run baseline.
func Start(st *store.Store, c domain.Clock) *Guard {
	return &Guard{Store: st, Clock: c, wall0: c.Wall().UnixNano(), mono0: c.Mono()}
}

// Baseline returns the captured wall and monotonic readings.
func (g *Guard) Baseline() (int64, time.Duration) { return g.wall0, g.mono0 }

// Assess evaluates the current instant.
func (g *Guard) Assess(ctx context.Context) (Verdict, error) {
	latchAt, latchReason, err := g.Store.ClockLatch(ctx)
	if err != nil {
		return Verdict{}, err
	}
	if latchAt > 0 {
		reason := latchReason
		if reason == "" {
			reason = "an earlier run saw an anomalous wall clock"
		}
		return Verdict{Trusted: false, Reason: reason, Latched: true}, nil
	}

	nowNS := g.Clock.Wall().UnixNano()

	baseStr, err := g.Store.MetaGet(ctx, MetaLastTrustedRun)
	if err != nil {
		return Verdict{}, err
	}
	var base int64
	if baseStr != "" {
		if v, perr := parseInt(baseStr); perr == nil {
			base = v
		}
	}

	if base > 0 {
		if nowNS < base-int64(BackwardsTolerance) {
			return Verdict{Trusted: false,
				Reason: "the wall clock moved backwards since the last trusted run; purging is suspended"}, nil
		}
		if nowNS-base > int64(JumpThreshold) {
			ackStr, aerr := g.Store.MetaGet(ctx, MetaAck)
			if aerr != nil {
				return Verdict{}, aerr
			}
			var ack int64
			if ackStr != "" {
				ack, _ = parseInt(ackStr)
			}
			if ack <= base {
				return Verdict{Trusted: false,
					Reason: "more than 48 hours passed between trusted runs; acknowledge the clock before purging"}, nil
			}
		}
	}

	// In-run drift, measured against the captured baseline and checked in both
	// directions. A clock that ran backwards during the run is as suspicious as
	// one that jumped forward.
	drift := (nowNS - g.wall0) - int64(g.Clock.Mono()-g.mono0)
	if drift > int64(DriftTolerance) || drift < -int64(DriftTolerance) {
		return Verdict{Trusted: false,
			Reason: "the wall clock drifted away from the monotonic clock during this run; purging is suspended"}, nil
	}
	return Verdict{Trusted: true}, nil
}

// Assert returns nil only when a purge may proceed. It fails closed when the
// ledger cannot be read.
func (g *Guard) Assert(ctx context.Context) error {
	v, err := g.Assess(ctx)
	if err != nil {
		return err
	}
	if !v.Trusted {
		return errs.New(errs.CodeClockUntrusted, v.Reason)
	}
	return nil
}

// RecordRun stores the outcome of the run.
//
// A trusted run advances the trusted baseline. An untrusted run only records
// that it happened, and latches the anomaly so the next run starts from the same
// doubt instead of from a fresh "now".
func (g *Guard) RecordRun(ctx context.Context, v Verdict) error {
	nowNS := g.Clock.Wall().UnixNano()
	if err := g.Store.MetaSet(ctx, MetaLastRun, formatInt(nowNS)); err != nil {
		return err
	}
	if v.Trusted {
		return g.Store.MetaSet(ctx, MetaLastTrustedRun, formatInt(nowNS))
	}
	return g.Store.LatchClock(ctx, nowNS, v.Reason)
}

// AckReport describes what an acknowledgement changed.
type AckReport struct {
	AtNS            int64  `json:"acknowledged_at_ns"`
	PreviousLatch   string `json:"previous_latch,omitempty"`
	ExtendedObjects int    `json:"objects_with_extended_retention"`
	Note            string `json:"note,omitempty"`
}

// Acknowledge clears the latch and gives every object whose retention cannot be
// proven a full new window, in one transaction.
//
// This is the part that a bare timestamp write was missing. After a 60-day
// outage, an operator saying "I accept the clock" must not make objects that
// were never observed during those 60 days instantly deletable: they get the
// whole retention period again, measured from the acknowledgement.
func Acknowledge(ctx context.Context, st *store.Store, c domain.Clock) (AckReport, error) {
	nowNS := c.Wall().UnixNano()
	latchAt, latchReason, err := st.ClockLatch(ctx)
	if err != nil {
		return AckReport{}, err
	}
	extended, err := st.ClearClockLatchAndExtend(ctx, nowNS, latchAt)
	if err != nil {
		return AckReport{}, err
	}
	return AckReport{
		AtNS:            nowNS,
		PreviousLatch:   latchReason,
		ExtendedObjects: extended,
		Note:            "objects whose retention window could not be proven now have a full new window, measured from this acknowledgement",
	}, nil
}

func parseInt(s string) (int64, error) {
	var v int64
	var neg bool
	if s == "" {
		return 0, errs.New(errs.CodeInvalidArgument, "empty integer")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && (c == '-' || c == '+') {
			neg = c == '-'
			continue
		}
		if c < '0' || c > '9' {
			return 0, errs.Newf(errs.CodeInvalidArgument, "%q is not an integer", s)
		}
		v = v*10 + int64(c-'0')
	}
	if neg {
		v = -v
	}
	return v, nil
}

func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [24]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
