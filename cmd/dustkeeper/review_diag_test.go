package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/diag"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/review"
	"dustkeeper/migrations"
)

func TestReviewDiagWritesBoundedCorrelationRecord(t *testing.T) {
	dir := t.TempDir()
	log, err := diag.Open(dir)
	if err != nil {
		t.Fatalf("open diagnostic log: %v", err)
	}
	a := &app{log: log, runID: "run_diag", instance: "inst_diag", cmd: "run"}
	a.reviewDiag()(execute.ReviewDiag{
		ReviewID: "rev_diag", ItemID: "itm_diag", Action: "quarantine",
		State: "settled", Journal: true, Attempts: 2, Failure: "",
		CostStatus: "usage_recorded", ElapsedMS: 321, HTTPStatus: 200,
		InputTokens: 12, OutputTokens: 7,
		Budget: `{"action":"quarantine","max_files":10}`,
		Note:   "dual_review_agrees",
	})
	if err := log.Close(); err != nil {
		t.Fatalf("close diagnostic log: %v", err)
	}

	recs := readRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("expected one review record, got %d", len(recs))
	}
	rec := recs[0]
	for key, want := range map[string]any{
		"msg":           "review",
		"phase":         "review",
		"version":       version,
		"commit":        commit,
		"schema":        float64(migrations.CurrentVersion),
		"run_id":        "run_diag",
		"instance":      "inst_diag",
		"command":       "run",
		"review_id":     "rev_diag",
		"item_id":       "itm_diag",
		"action":        "quarantine",
		"state":         "settled",
		"journal":       true,
		"attempts":      float64(2),
		"failure":       "",
		"cost_status":   "usage_recorded",
		"elapsed_ms":    float64(321),
		"http_status":   float64(200),
		"input_tokens":  float64(12),
		"output_tokens": float64(7),
		"note":          "dual_review_agrees",
	} {
		if got := rec[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if rec["budget"] != `{"action":"quarantine","max_files":10}` {
		t.Errorf("budget = %#v", rec["budget"])
	}
	// No payload: no prompt, no response, no path and no credential.
	for _, forbidden := range []string{"prompt", "response", "api_key", "raw_relative", "/Users/", "Authorization"} {
		if _, ok := rec[forbidden]; ok {
			t.Errorf("review record carries forbidden key %q", forbidden)
		}
	}
}

func TestReviewDiagFallsBackToTheAppRunID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	log, err := diag.Open(dir)
	if err != nil {
		t.Fatalf("open diagnostic log: %v", err)
	}
	a := &app{log: log, runID: "run_fallback", cmd: "run"}
	a.reviewDiag()(execute.ReviewDiag{
		State: "not_started", Failure: "batch_budget", CostStatus: "not_sent",
	})
	if err := log.Close(); err != nil {
		t.Fatalf("close diagnostic log: %v", err)
	}
	recs := readRecords(t, dir)
	if len(recs) != 1 || recs[0]["run_id"] != "run_fallback" {
		t.Fatalf("the record lost its run correlation: %v", recs)
	}
	if recs[0]["review_id"] != "" {
		t.Errorf("a candidate that was not started must not claim a review id: %v", recs[0])
	}
}

func TestReviewDiagWithNilLoggerDropsTheRecord(t *testing.T) {
	a := &app{runID: "run_nolog"}
	// A debug command owns no state directory. This must not panic.
	a.reviewDiag()(execute.ReviewDiag{ReviewID: "rev_nolog", State: "pending"})
}

func TestReviewDiagWritesRequestTimingsWithoutPayload(t *testing.T) {
	dir := t.TempDir()
	log, err := diag.Open(dir)
	if err != nil {
		t.Fatalf("open diagnostic log: %v", err)
	}
	a := &app{log: log, runID: "run_timing", cmd: "run"}
	decision := review.Decision{
		Verdict: domain.VerdictInsufficient,
		JEV: &review.JEVDiagnostic{Choice: "insufficient", GatePassed: false,
			Blockers: []string{"model_insufficient"}},
		LLMSent: true, LLMConsulted: true, LLMDecision: "insufficient",
		LLMReason: "insufficient_evidence", MissingEvidence: []string{"current_use", "references"},
		LLMExplanation: "raw model prose must not enter diagnostics",
	}
	explanation := review.NewExplanation(review.Request{
		Action: domain.DispositionQuarantine, FactsComplete: true, Movable: true,
		Checks: review.Checks{OpenByProcess: review.Bool(false), ParentStable: review.Bool(true)},
	}, decision, "move_safety_unknown")
	a.reviewDiag()(execute.ReviewDiag{
		ReviewID: "rev_timing", State: "settled", Journal: true, Attempts: 2,
		CostStatus:  "usage_recorded",
		Explanation: explanation,
		RequestTimings: []review.RequestTiming{
			{Provider: "jev", Outcome: "ok", Phase: "response_body", ElapsedMS: 120,
				StagesMS: map[string]int64{"dns_start": 1, "dns_done": 6, "connection_ready": 22, "first_byte": 100}},
			{Provider: "minimax", Outcome: "ok", Phase: "response_body", ElapsedMS: 210,
				StagesMS: map[string]int64{"connection_ready": 5, "first_byte": 150}},
		},
	})
	if err := log.Close(); err != nil {
		t.Fatalf("close diagnostic log: %v", err)
	}
	recs := readRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("expected one review record, got %d", len(recs))
	}
	raw, ok := recs[0]["request_timings"].(string)
	if !ok {
		t.Fatalf("request_timings not written as a bounded JSON string: %#v", recs[0]["request_timings"])
	}
	var rows []review.RequestTiming
	if json.Unmarshal([]byte(raw), &rows) != nil || len(rows) != 2 {
		t.Fatalf("request_timings are not two valid rows: %s", raw)
	}
	if rows[0].StagesMS["dns_done"] != 6 || rows[1].StagesMS["first_byte"] != 150 {
		t.Fatalf("stages_ms lost in the operational log: %s", raw)
	}
	explanationRaw, ok := recs[0]["review_explanation"].(string)
	if !ok {
		t.Fatalf("review_explanation not written as bounded JSON: %#v", recs[0]["review_explanation"])
	}
	var logged review.Explanation
	if json.Unmarshal([]byte(explanationRaw), &logged) != nil || logged.Cause != "model_insufficient_for_isolation" ||
		logged.Local == nil || !logged.Local.FactsComplete || !logged.Local.Movable ||
		logged.JEV == nil || logged.JEV.Choice != "insufficient" ||
		logged.MiniMax == nil || len(logged.MiniMax.MissingEvidence) != 2 {
		t.Fatalf("structured local/JEV/MiniMax explanation was lost: %s", explanationRaw)
	}
	for _, forbidden := range []string{"http://", "https://", "Bearer", "api_key", "proxy", "/Users/", "Authorization"} {
		if strings.Contains(raw+explanationRaw, forbidden) {
			t.Errorf("review diagnostic carries forbidden payload %q: timings=%s explanation=%s", forbidden, raw, explanationRaw)
		}
	}
	if strings.Contains(explanationRaw, decision.LLMExplanation) {
		t.Fatalf("raw model prose entered the structured explanation: %s", explanationRaw)
	}
}

func TestReviewDiagOmitsTimingsWhenNoRequestSent(t *testing.T) {
	dir := t.TempDir()
	log, err := diag.Open(dir)
	if err != nil {
		t.Fatalf("open diagnostic log: %v", err)
	}
	a := &app{log: log, runID: "run_unsent", cmd: "run"}
	a.reviewDiag()(execute.ReviewDiag{State: "not_started", CostStatus: review.CostNotSent,
		Explanation: &review.Explanation{Cause: "local_facts_missing"}})
	if err := log.Close(); err != nil {
		t.Fatalf("close diagnostic log: %v", err)
	}
	recs := readRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("expected one review record, got %d", len(recs))
	}
	// A request that never left the process must not get a 0ms timing row.
	if _, ok := recs[0]["request_timings"]; ok {
		t.Fatalf("an unsent request invented a timing record: %v", recs[0])
	}
	if _, ok := recs[0]["review_explanation"]; ok {
		t.Fatalf("an unsent request invented a review explanation: %v", recs[0])
	}
}
