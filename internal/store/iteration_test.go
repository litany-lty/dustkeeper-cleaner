package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// The pool has exactly one connection. A streaming query keeps it until the last
// row is read, so a query issued from inside the row callback waits for a
// connection that the callback itself is preventing from being released. Nothing
// times out and nothing reports: the process stops, holding the write mutex, in
// the middle of deciding what to collect.
//
// The planner evaluates every observation in that callback, and it needs the
// filesystem to answer "is this object under a protected directory". That walk
// must stay outside the store, and this test is what keeps it there: the mistake
// is now a coded error naming the iterator instead of a hang that needs a
// debugger.
func TestANestedQueryInsideAnIteratorIsRefusedNotWaited(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedScope(t, s, "scope-iter", domain.KindManagedDisposable)
	if err := s.UpsertObservations(ctx, []Observation{{
		ScopeID:      "scope-iter",
		RawRelative:  []byte("a.zip"),
		IdentityJSON: `{"inode":1}`,
		TokenJSON:    `{"size":1}`,
		ParentJSON:   `{"inode":2}`,
		SizeBytes:    1,
		MTimeNS:      1,
		FirstSeenNS:  1,
		LastSeenNS:   1,
		ScanEpoch:    1,
		Status:       domain.ObsSeen,
	}}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	type outcome struct {
		rows int
		err  error
	}
	// The iteration runs on its own goroutine so a regression is reported as a
	// failure in a few seconds instead of as a test binary that never returns.
	done := make(chan outcome, 1)
	go func() {
		var nested []error
		rows := 0
		err := s.IterateObservations(ctx, "scope-iter", func(Observation) error {
			rows++
			// All three shapes of store access a callback could plausibly use.
			_, getErr := s.GetObservation(ctx, "scope-iter", []byte("a.zip"))
			setErr := s.MetaSet(ctx, "nested", "1")
			_, countErr := s.CountObservations(ctx, "scope-iter")
			nested = append(nested, getErr, setErr, countErr)
			return nil
		})
		if err != nil {
			done <- outcome{err: err}
			return
		}
		for _, nerr := range nested {
			if nerr == nil {
				done <- outcome{rows: rows, err: context.DeadlineExceeded}
				return
			}
			if errs.CodeOf(nerr) != errs.CodeDBError ||
				!strings.Contains(nerr.Error(), "open row iterator") {
				done <- outcome{rows: rows, err: nerr}
				return
			}
		}
		done <- outcome{rows: rows}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a nested store call inside the row callback waited instead of being refused")
	}
	if got.err != nil {
		t.Fatalf("a nested store call was not refused with a coded error: %v", got.err)
	}
	if got.rows != 1 {
		t.Fatalf("expected the callback to see one row, got %d", got.rows)
	}

	// The refusal is a property of the iteration, not of the store: once the
	// last row has been read, the same calls succeed again.
	if _, err := s.GetObservation(ctx, "scope-iter", []byte("a.zip")); err != nil {
		t.Fatalf("the store stayed closed after iteration: %v", err)
	}
	if err := s.MetaSet(ctx, "nested", "1"); err != nil {
		t.Fatalf("a write after iteration failed: %v", err)
	}
	if n, err := s.CountObservations(ctx, "scope-iter"); err != nil || n != 1 {
		t.Fatalf("a count after iteration failed: %d (%v)", n, err)
	}
}
