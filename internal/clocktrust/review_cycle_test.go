package clocktrust_test

import (
	"context"
	"testing"
	"time"

	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/errs"
)

func TestReviewCycleInvalidTrustedBaselineCannotAuthorize(t *testing.T) {
	for _, value := range []string{"not-a-timestamp", "+", "18446744073709551616"} {
		t.Run(value, func(t *testing.T) {
			st := newStore(t)
			if err := st.MetaSet(context.Background(), clocktrust.MetaLastTrustedRun, value); err != nil {
				t.Fatal(err)
			}
			guard := clocktrust.Start(st, newMovableClock(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)))
			if err := guard.Assert(context.Background()); errs.CodeOf(err) != errs.CodeClockUntrusted {
				t.Fatalf("corrupt trusted baseline became deletion authority: value=%q err=%v", value, err)
			}
		})
	}
}

func TestReviewCycleInvalidLatchNeverLooksClear(t *testing.T) {
	for _, value := range []string{"broken", "-", "18446744073709551616"} {
		t.Run(value, func(t *testing.T) {
			st := newStore(t)
			if err := st.MetaSet(context.Background(), clocktrust.MetaLatchAt, value); err != nil {
				t.Fatal(err)
			}
			guard := clocktrust.Start(st, newMovableClock(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)))
			if err := guard.Assert(context.Background()); errs.CodeOf(err) != errs.CodeClockUntrusted {
				t.Fatalf("corrupt latch was ignored: %q %v", value, err)
			}
		})
	}
}

func TestReviewCycleAcknowledgementRepairsInvalidBaseline(t *testing.T) {
	st := newStore(t)
	if err := st.MetaSet(context.Background(), clocktrust.MetaLastTrustedRun, "broken"); err != nil {
		t.Fatal(err)
	}
	clock := newMovableClock(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	if _, err := clocktrust.Acknowledge(context.Background(), st, clock); err != nil {
		t.Fatal(err)
	}
	if err := clocktrust.Start(st, clock).Assert(context.Background()); err != nil {
		t.Fatalf("explicit acknowledgement left invalid baseline: %v", err)
	}
}
