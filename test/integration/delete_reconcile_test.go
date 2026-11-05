package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// A permanent deletion is the only action whose success cannot be checked
// afterwards: there is no copy left to compare against. What reconciliation can
// do is tell the three observable states apart, and these tests pin all three.
//
// The state is produced the way a crash produces it: the intent commits, and then
// nothing else happens.
func TestInterruptedDirectDeletionIsReconciledThreeWays(t *testing.T) {
	t.Run("the file is still there: the attempt is closed and the plan stands", func(t *testing.T) {
		e, sc, p, it := pendingDelete(t)
		opID := beginDeleteIntent(t, e, p, it)
		if got := e.pendingOf(t, it.ID); got != string(opID) {
			t.Fatalf("the fixture did not leave a pending operation: %q", got)
		}

		rep, err := e.rec.Run(context.Background())
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rep.Aborted != 1 || rep.Completed != 0 {
			t.Fatalf("expected the attempt to be closed, got %+v", rep)
		}
		if !e.exists("scratch", "discard.zip") {
			t.Fatal("reconciliation removed a file the operation never removed")
		}
		got, err := e.st.GetItem(context.Background(), it.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if got.State != domain.StatePlanned || got.PendingOpID != nil {
			t.Fatalf("state=%s pending=%v, want the item back in its plan", got.State, got.PendingOpID)
		}
		// A deletion that removed nothing must not leave the scope stopped: the
		// item is back in its plan and the next apply has to be able to run.
		if status := e.scopeStatus(t, sc.ID); status == domain.ScopeHeld {
			t.Fatalf("scope %s was held by an operation that removed nothing", sc.ID)
		}
	})

	t.Run("the path is empty: the record is closed as reconciled absent", func(t *testing.T) {
		e, _, p, it := pendingDelete(t)
		beginDeleteIntent(t, e, p, it)
		if err := os.Remove(e.treePath("scratch", "discard.zip")); err != nil {
			t.Fatalf("simulate the unlink: %v", err)
		}

		rep, err := e.rec.Run(context.Background())
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rep.Completed != 1 {
			t.Fatalf("expected the operation to be finished, got %+v", rep)
		}
		got, err := e.st.GetItem(context.Background(), it.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if got.State != domain.StatePurged || got.PendingOpID != nil {
			t.Fatalf("state=%s pending=%v, want the item settled", got.State, got.PendingOpID)
		}
		// The wording is the point: a recovery pass did not observe an unlink, so
		// it may not record one. The completion has to say what it knows.
		recorded := false
		for _, ev := range e.events(t, it.ID) {
			if ev.Phase != domain.PhaseComplete || ev.ResultJSON == nil {
				continue
			}
			if !strings.Contains(*ev.ResultJSON, "RECONCILED_ABSENT") {
				t.Fatalf("the completion claims more than was observed: %s", *ev.ResultJSON)
			}
			recorded = true
		}
		if !recorded {
			t.Fatal("the item was settled with no completion record")
		}
	})

	t.Run("a different file is there now: it is held and nothing is removed", func(t *testing.T) {
		e, _, p, it := pendingDelete(t)
		beginDeleteIntent(t, e, p, it)
		path := e.treePath("scratch", "discard.zip")
		if err := os.Remove(path); err != nil {
			t.Fatalf("simulate the unlink: %v", err)
		}
		if err := os.WriteFile(path, []byte("a different file, with different bytes"), 0o644); err != nil {
			t.Fatalf("write the replacement: %v", err)
		}

		rep, err := e.rec.Run(context.Background())
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rep.Held != 1 {
			t.Fatalf("expected a hold, got %+v", rep)
		}
		if !e.exists("scratch", "discard.zip") {
			t.Fatal("the file that was never reviewed was removed")
		}
		got, err := e.st.GetItem(context.Background(), it.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if got.State != domain.StateHeld || got.PendingOpID != nil {
			t.Fatalf("state=%s pending=%v, want a settled hold", got.State, got.PendingOpID)
		}
		if got.HoldCode == nil || *got.HoldCode != "SOURCE_REPLACED" {
			t.Fatalf("hold code = %v, want SOURCE_REPLACED", got.HoldCode)
		}
	})
}

// TestDirectDeletionRecordsItselfThroughTheRecoveryPass reaches the same state
// through the real executor: the unlink lands, the closing write does not.
func TestDirectDeletionRecordsItselfThroughTheRecoveryPass(t *testing.T) {
	e, sc, res := scheduledDeleteSetup(t)
	e.savePolicy(directDeletePolicy(sc.ID))
	e.exec.Review = agreeWithTheQuestion

	// Fail only the closing write of a deletion, which is the window between "the
	// bytes are gone and the directory is flushed" and "the record says so".
	execute.TestHookBeforeComplete = func(action string) error {
		if action == domain.ActionDelete {
			return errors.New("injected: the state database refused the write")
		}
		return nil
	}
	t.Cleanup(func() { execute.TestHookBeforeComplete = nil })

	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if e.exists("scratch", "discard.zip") {
		t.Fatal("the file is still there; this test is about a deletion that happened")
	}
	item, err := e.st.GetItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != domain.StatePIntent || item.PendingOpID == nil {
		t.Fatalf("state=%s pending=%v, want an open operation for reconciliation",
			item.State, item.PendingOpID)
	}

	execute.TestHookBeforeComplete = nil
	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 1 {
		t.Fatalf("the open deletion was not finished: %+v", rep)
	}
	settled, err := e.st.GetItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if settled.State != domain.StatePurged || settled.PendingOpID != nil {
		t.Fatalf("state=%s pending=%v, want the item settled", settled.State, settled.PendingOpID)
	}
}

// pendingDelete produces one planned candidate whose plan carries the automatic
// purge grant, so the intent built for it is one the product would really write.
func pendingDelete(t *testing.T) (*env, store.Scope, store.Plan, store.Item) {
	t.Helper()
	e, sc, res := scheduledDeleteSetup(t)
	return e, sc, res.Plan, res.Items[0]
}

// beginDeleteIntent commits the intent a direct deletion writes, and stops there.
//
// The contract is built from the item rather than from the review, because that is
// what the executor does: the item records the identity the plan approved, and the
// contract records what was about to happen to it.
func beginDeleteIntent(t *testing.T, e *env, p store.Plan, it store.Item) domain.OperationID {
	t.Helper()
	ctx := context.Background()
	opID := domain.OperationID(domain.NewID("op"))
	approval, hash := fakeIntentApproval(it.ID, domain.ActionDelete)
	contentHash := hash
	planID := p.ID
	planIDStr := string(planID)
	body, err := json.Marshal(plan.OperationContractV1{
		BindingVersion:  plan.BindingVersion,
		ItemID:          string(it.ID),
		ExecutionPlanID: &planIDStr,
		PriorState:      domain.StatePlanned,
		NextState:       domain.StatePIntent,
		Purpose:         execute.PurposeDirectDelete,
		ScopeID:         string(it.ScopeID),
		RawRelativeB64:  plan.B64(it.RawRelative),
		ObjectName:      it.ObjectName,
		ExpectedIdent:   decodeIdentity(t, it.PreIdentityJSON),
		ContentHash:     &contentHash,
	})
	if err != nil {
		t.Fatalf("marshal delete contract: %v", err)
	}
	if err := e.st.BeginIntent(ctx, store.Intent{
		OpID:              opID,
		ItemID:            it.ID,
		PlanID:            &planID,
		Action:            domain.ActionDelete,
		PriorState:        domain.StatePlanned,
		NextState:         domain.StatePIntent,
		Review:            approval,
		ReviewContentHash: hash,
		ContractJSON:      string(body),
		OccurredAtNS:      e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("begin delete intent: %v", err)
	}
	return opID
}

// events is the recorded history of one item.
func (e *env) events(t *testing.T, id domain.ItemID) []store.Event {
	t.Helper()
	events, err := e.st.EventsForItem(context.Background(), id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return events
}

// scopeStatus reads the stored status of a scope.
func (e *env) scopeStatus(t *testing.T, id domain.ScopeID) string {
	t.Helper()
	sc, err := e.st.GetScope(context.Background(), id)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	return sc.Status
}
