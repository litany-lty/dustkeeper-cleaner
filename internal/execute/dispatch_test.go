package execute

import (
	"encoding/json"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

func TestEmitDiagSuppliesTheRunCorrelation(t *testing.T) {
	var got []ReviewDiag
	e := &Executor{RunID: "run_emit", Diag: func(d ReviewDiag) { got = append(got, d) }}
	e.emitDiag(ReviewDiag{ReviewID: "rev_1", State: "pending"})
	e.emitDiag(ReviewDiag{ReviewID: "rev_1", RunID: "run_override", State: "settled"})
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].RunID != "run_emit" {
		t.Errorf("the executor's run id was not filled in: %+v", got[0])
	}
	if got[1].RunID != "run_override" {
		t.Errorf("an explicit run id was overwritten: %+v", got[1])
	}
}

func TestDiagNoRequestCarriesNoReviewIDOrUnknownCost(t *testing.T) {
	var got []ReviewDiag
	e := &Executor{RunID: "run_none", Diag: func(d ReviewDiag) { got = append(got, d) }}
	e.diagNoRequest(store.Item{ID: domain.ItemID("itm_none")}, "purge",
		"not_started", review.FailureBatchBudget, `{"action":"purge"}`)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	d := got[0]
	if d.ReviewID != "" {
		t.Errorf("a request that never started must not carry an invented review id: %+v", d)
	}
	if d.Journal {
		t.Errorf("no dispatch row backs this record: %+v", d)
	}
	if d.CostStatus != review.CostNotSent {
		t.Errorf("a request that never started cannot have an unknown cost: %+v", d)
	}
	if d.Failure != review.FailureBatchBudget || d.Note != review.FailureBatchBudget ||
		d.State != "not_started" || d.Action != "purge" {
		t.Errorf("the reason for not starting was lost: %+v", d)
	}
}

func TestDispatchBudgetJSONIsBoundedAndPayloadFree(t *testing.T) {
	run := &domain.RunLedger{SoftTime: 300 * time.Second, MaxRead: 8192}
	b := &domain.Budget{MaxFiles: 10, MaxBytes: 4096, Run: run}
	raw := dispatchBudgetJSON("quarantine", b)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("budget is not JSON: %v (%s)", err, raw)
	}
	if body["action"] != "quarantine" || body["max_files"] != float64(10) || body["soft_seconds"] != float64(300) {
		t.Errorf("budget summary lost the allowances: %s", raw)
	}
	for _, forbidden := range []string{"prompt", "identity", "token", "path", "credential"} {
		if _, ok := body[forbidden]; ok {
			t.Errorf("budget summary carries %q", forbidden)
		}
	}
	if len(raw) > 512 {
		t.Errorf("budget summary is not bounded: %d bytes", len(raw))
	}
	// A nil budget still yields a usable record rather than an empty field.
	if dispatchBudgetJSON("purge", nil) == "" {
		t.Fatal("a debug path produced no budget summary")
	}
}
