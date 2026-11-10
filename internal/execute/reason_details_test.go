package execute

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/outcome"
	"dustkeeper/internal/review"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

func TestKeepRecordsMeasuredCauseWithoutModelFreeText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		movable bool
		cause   string
	}{
		{"local", false, "local_move_safety_unknown"},
		{"model", true, "model_insufficient_for_isolation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Executor{Gate: scope.PauseGate{StateDir: t.TempDir()}, Review: func(_ context.Context, r review.Request) (review.Outcome, error) {
				return review.Outcome{Disposition: domain.DispositionKeep, Reason: outcome.ReasonMoveSafetyUnknown,
					Decision: review.Decision{Verdict: domain.VerdictInsufficient, LLMConsulted: true,
						LLMDecision: "insufficient", LLMReason: "insufficient_evidence", MissingEvidence: []string{"current_use", "recoverability"},
						LLMExplanation: "secret-body-should-not-log", JEVModel: "secret-model-path",
						JEV: &review.JEVDiagnostic{Choice: "allow_review", GatePassed: true}}}, nil
			}}
			r, err := e.reviewItem(context.Background(), review.Request{Action: domain.DispositionQuarantine,
				FactsComplete: true, Movable: tc.movable, Checks: review.Checks{OpenByProcess: review.Bool(!tc.movable)}})
			if err != nil {
				t.Fatal(err)
			}
			if r.Disposition != domain.DispositionKeep || r.Reason != outcome.ReasonMoveSafetyUnknown {
				t.Fatalf("decision changed: %+v", r)
			}
			raw := reviewPayload(r, store.Item{})
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatal(err)
			}
			var detail struct {
				Cause string `json:"cause"`
				Local struct {
					Movable bool `json:"movable"`
				} `json:"local"`
				MiniMax struct {
					Missing []string `json:"missing_evidence"`
				} `json:"minimax"`
			}
			if err := json.Unmarshal(payload["review_explanation"], &detail); err != nil {
				t.Fatalf("missing explanation: %v", err)
			}
			if detail.Cause != tc.cause || detail.Local.Movable != tc.movable || len(detail.MiniMax.Missing) != 2 {
				t.Fatalf("lost actual cause: %+v", detail)
			}
			if strings.Contains(raw, "secret-") {
				t.Fatal("model free text or unrelated metadata leaked")
			}
		})
	}
}
