package store

import (
	"context"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// B23: acknowledging a clock anomaly must only restart retention for objects
// whose deadline is no longer provable. An older object whose deadline is
// still in the future keeps its existing deadline.
func TestB23ClockAcknowledgementDoesNotExtendUnaffectedObject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)
	seedItem(t, s, "exports", "before_future", "plan_before_future")
	seedItem(t, s, "exports", "after_latch", "plan_after_latch")
	seedItem(t, s, "exports", "before_overdue", "plan_before_overdue")

	const retention = int64((30 * 24 * time.Hour) / time.Second)
	ackNS := int64(100 * 24 * time.Hour)
	latchNS := ackNS - int64(time.Hour)
	newDue := ackNS + int64(30*24*time.Hour)
	type clockRow struct {
		id            string
		quarantinedNS int64
		dueNS         int64
	}
	rows := []clockRow{
		{id: "before_future", quarantinedNS: latchNS - int64(20*24*time.Hour), dueNS: ackNS + int64(10*24*time.Hour)},
		{id: "after_latch", quarantinedNS: latchNS + 1, dueNS: ackNS + int64(10*24*time.Hour)},
		{id: "before_overdue", quarantinedNS: latchNS - int64(40*24*time.Hour), dueNS: ackNS - int64(10*24*time.Hour)},
	}
	for _, row := range rows {
		if _, err := s.db.ExecContext(ctx, `
UPDATE items SET state='quarantined', bound_retention_seconds=?, quarantined_at_ns=?, purge_not_before_ns=?
WHERE item_id=?`, retention, row.quarantinedNS, row.dueNS, row.id); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	extended, err := s.ClearClockLatchAndExtend(ctx, ackNS, latchNS)
	if err != nil {
		t.Fatalf("acknowledge clock: %v", err)
	}
	if extended != 2 {
		t.Fatalf("extended = %d, want 2", extended)
	}

	wantDue := map[string]int64{
		"before_future":  rows[0].dueNS,
		"after_latch":    newDue,
		"before_overdue": newDue,
	}
	for id, want := range wantDue {
		it, err := s.GetItem(ctx, domain.ItemID(id))
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if it.PurgeNotBeforeNS == nil || *it.PurgeNotBeforeNS != want {
			t.Fatalf("%s due = %v, want %d", id, it.PurgeNotBeforeNS, want)
		}
	}
}

// B24: the execution plan recorded for a quarantine intent must be the plan
// that owns the item. Merely naming another existing plan must not relabel the
// audit trail or authorise a transition for an item that plan never selected.
func TestB24QuarantineIntentRejectsDifferentExecutionPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)
	seedItem(t, s, "exports", "item_1", "plan_1")
	seedItem(t, s, "exports", "item_2", "plan_2")
	wrongPlan := domain.PlanID("plan_2")

	err := s.BeginIntent(ctx, Intent{
		Review: storageReviewFixture{}, OpID: "op_1", ItemID: "item_1", PlanID: &wrongPlan,
		Action: "quarantine", PriorState: domain.StatePlanned, NextState: domain.StateQIntent,
		ContractJSON: `{}`, OccurredAtNS: 1,
	})
	if errs.CodeOf(err) != errs.CodePlanStale {
		t.Fatalf("error = %v, want PLAN_STALE", err)
	}
	it, getErr := s.GetItem(ctx, "item_1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if it.State != domain.StatePlanned || it.PendingOpID != nil {
		t.Fatalf("rejected cross-plan intent changed item: %+v", it)
	}
	events, listErr := s.EventsForItem(ctx, "item_1")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(events) != 0 {
		t.Fatalf("rejected cross-plan intent left events: %+v", events)
	}
}
