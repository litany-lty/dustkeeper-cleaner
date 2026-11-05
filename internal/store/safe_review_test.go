package store

import (
	"context"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"testing"
)

// This stub isolates storage atomicity from model behavior; it is not review evidence.
type storageReviewFixture struct{ reject bool }

func (storageReviewFixture) Verify(string) error { return nil }
func (storageReviewFixture) Disposition() string  { return domain.DispositionQuarantine }
func (v storageReviewFixture) CheckIntent(string, string, int64) error {
	if v.reject {
		return errs.New(errs.CodeReviewRequired, "rejected")
	}
	return nil
}
func (storageReviewFixture) AuditJSON() string { return `{"fixture_review":true}` }
func TestSafetyReviewIntentRequiresValidApproval(t *testing.T) {
	for _, kind := range []string{"missing", "denied", "allow"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			seedScope(t, s, "exports", domain.KindManagedDisposable)
			seedItem(t, s, "exports", "item_1", "plan_1")
			in := Intent{OpID: "op", ItemID: "item_1", PlanID: planRef("plan_1"), Action: "quarantine", PriorState: domain.StatePlanned, NextState: domain.StateQIntent, ContractJSON: `{}`, OccurredAtNS: 1}
			if kind != "missing" {
				in.Review = storageReviewFixture{reject: kind == "denied"}
			}
			err := s.BeginIntent(ctx, in)
			it, _ := s.GetItem(ctx, "item_1")
			events, _ := s.EventsForItem(ctx, "item_1")
			if kind == "allow" {
				if err != nil || it.State != domain.StateQIntent || len(events) != 1 || events[0].ResultJSON == nil || *events[0].ResultJSON != in.Review.AuditJSON() {
					t.Fatalf("review and intent not atomic: %v %+v %+v", err, it, events)
				}
			} else {
				if errs.CodeOf(err) != errs.CodeReviewRequired || it.State != domain.StatePlanned || it.PendingOpID != nil || len(events) != 0 {
					t.Fatalf("refusal changed state: %v %+v %+v", err, it, events)
				}
			}
		})
	}
}

func TestSafetyReviewAuditFailureLeavesNoIntent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)
	seedItem(t, s, "exports", "item_1", "plan_1")
	if _, err := s.db.Exec(`CREATE TRIGGER reject_review BEFORE INSERT ON events BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.BeginIntent(ctx, Intent{Review: storageReviewFixture{}, OpID: "op", ItemID: "item_1", PlanID: planRef("plan_1"), Action: "quarantine", PriorState: domain.StatePlanned, NextState: domain.StateQIntent, ContractJSON: `{}`, OccurredAtNS: 1})
	it, _ := s.GetItem(ctx, "item_1")
	events, _ := s.EventsForItem(ctx, "item_1")
	if err == nil || it.State != domain.StatePlanned || it.PendingOpID != nil || len(events) != 0 {
		t.Fatalf("audit failure did not roll back intent: %v %+v %+v", err, it, events)
	}
}
