package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
)

// These are the numbers the real JEV endpoint returned for the clearest
// deletion candidate in the sample set: a 90-day-old build cache whose category
// the confirmed policy authorises, with every local check clean. The built-in
// gate is .9/.9, so it blocked this answer twice in a row — and because JEV
// authorises both deletion and isolation, a gate nothing can reach means the
// product neither deletes nor moves anything.
const observedAllowAnswer = `{"model":"jev-1.14.0","answers":{"quarantine_disposition":{"type":"choice","choice":"allow_review","confidence":0.72,"probabilities":{"allow_review":0.81,"keep":0.01,"insufficient":0.18}}}}`

func thresholdFixture(t *testing.T) *HTTPJudge {
	t.Helper()
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(observedAllowAnswer))
	}))
	t.Cleanup(jev.Close)
	minimax := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		decision := `{"decision":"allow","reason_code":"disposable"}`
		// A diagnostic session asks for the evidence behind the decision, so the
		// fixture answers that question when it is the one being asked.
		var request struct {
			System string `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(request.System, "For debug only") {
			decision = `{"decision":"allow","reason_code":"disposable","explanation":"Synthetic evidence fixture.","missing_evidence":[]}`
		}
		_, _ = w.Write([]byte(`{"id":"msg-fixture","type":"message","role":"assistant","model":"MiniMax-M2.1","content":[{"type":"text","text":"` + strings.ReplaceAll(decision, `"`, `\"`) + `"}],"stop_reason":"end_turn"}`))
	}))
	t.Cleanup(minimax.Close)

	judge, err := NewHTTPJudge(
		Endpoint{URL: jev.URL, Model: "jev-1.14.0", APIKey: "jev-test"},
		Endpoint{URL: minimax.URL, Model: "MiniMax-M2.1", APIKey: "minimax-test"},
	)
	if err != nil {
		t.Fatalf("NewHTTPJudge: %v", err)
	}
	return judge
}

func TestTheBuiltInGateBlocksTheObservedAnswer(t *testing.T) {
	judge := thresholdFixture(t)

	decision, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "delete", ScopeID: "scope-1", RelativePath: "build-cache/old.tmp", ContentSHA256: "abc", SizeBytes: 12})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if decision.Verdict != domain.VerdictInsufficient {
		t.Fatalf("verdict = %q, want insufficient: the default gate must not authorise this", decision.Verdict)
	}
	if decision.JEV == nil || decision.JEV.GatePassed {
		t.Fatalf("the diagnostic claims the gate passed: %+v", decision.JEV)
	}
	if len(decision.JEV.Blockers) != 2 {
		t.Fatalf("blockers = %v, want both thresholds reported", decision.JEV.Blockers)
	}
}

// A calibrated gate must change the answer for this exact response, and it must
// record the value it used: a threshold nobody can see is a threshold nobody can
// review.
func TestACalibratedGateUsesTheConfiguredValues(t *testing.T) {
	judge := thresholdFixture(t)
	if err := judge.SetThresholds(0.70, 0.80); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}

	decision, err := judge.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "delete", ScopeID: "scope-1", RelativePath: "build-cache/old.tmp", ContentSHA256: "abc", SizeBytes: 12})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if decision.Verdict != domain.VerdictAllow {
		t.Fatalf("verdict = %q, want allow at a gate of .70/.80", decision.Verdict)
	}
	if decision.JEV == nil || decision.JEV.MinConfidence != 0.70 || decision.JEV.MinAllowProbability != 0.80 {
		t.Fatalf("the diagnostic does not record the gate it used: %+v", decision.JEV)
	}
}

// TestADebugSessionKeepsTheCalibratedGate is R10.
//
// A diagnostic run exists to answer "what would this endpoint say", and the
// answer only means something against the gate the product actually applies. A
// session that quietly fell back to .9/.9 measured a different question from the
// one the user configured, and nothing in its output said so.
func TestADebugSessionKeepsTheCalibratedGate(t *testing.T) {
	judge := thresholdFixture(t)
	if err := judge.SetThresholds(0.70, 0.80); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}
	session, err := judge.DebugSession(4, 10*time.Second)
	if err != nil {
		t.Fatalf("DebugSession: %v", err)
	}
	if conf, allow := session.Thresholds(); conf != 0.70 || allow != 0.80 {
		t.Fatalf("the debug session applies %v/%v, want the configured .70/.80", conf, allow)
	}
	decision, err := session.Review(context.Background(), Facts{Category: "temporary_name_only", Action: "delete", ScopeID: "scope-1", RelativePath: "build-cache/old.tmp", ContentSHA256: "abc", SizeBytes: 12})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if decision.Verdict != domain.VerdictAllow {
		t.Fatalf("verdict = %q: the session did not use the calibrated gate", decision.Verdict)
	}
	if decision.JEV == nil || decision.JEV.MinConfidence != 0.70 || decision.JEV.MinAllowProbability != 0.80 {
		t.Fatalf("the diagnostic does not record the gate the session used: %+v", decision.JEV)
	}
	// The ordinary judge is unchanged: a diagnostic run may not recalibrate the
	// installation it was run against.
	if conf, allow := judge.Thresholds(); conf != 0.70 || allow != 0.80 {
		t.Fatalf("the parent judge was modified: %v/%v", conf, allow)
	}
}

func TestUnsetThresholdsKeepTheBuiltInDefaults(t *testing.T) {
	judge := thresholdFixture(t)
	if conf, allow := judge.Thresholds(); conf != jevMinConfidence || allow != jevMinAllowProbability {
		t.Fatalf("default thresholds = %v/%v, want %v/%v", conf, allow, jevMinConfidence, jevMinAllowProbability)
	}
	for _, bad := range [][2]float64{{0, 0.8}, {-0.1, 0.8}, {0.8, 1.5}} {
		if err := judge.SetThresholds(bad[0], bad[1]); err == nil {
			t.Errorf("SetThresholds(%v, %v) accepted a value outside (0,1]", bad[0], bad[1])
		}
	}
}

// validThresholdSettings is a minimal settings document that passes validation,
// so these tests fail on the threshold and on nothing else.
func validThresholdSettings(confidencePct *int) *Settings {
	return &Settings{
		MaxCandidates: 5,
		JEVURL:        "https://127.0.0.1:9/jev", JEVModel: "jev-1.14.0",
		MiniMaxURL: "https://127.0.0.1:9/mm", MiniMaxModel: "MiniMax-M3.1-Flash",
		JEVMinConfidencePct: confidencePct,
	}
}

// The plan printed before any call has to state the gate, otherwise "nothing was
// allowed" reads as a model opinion rather than as a threshold.
func TestSettingsReportTheirGateWithoutAJudge(t *testing.T) {
	seventyFive := 75
	s := validThresholdSettings(&seventyFive)
	if conf, allow := s.Thresholds(); conf != 0.75 || allow != jevMinAllowProbability {
		t.Fatalf("thresholds = %v/%v", conf, allow)
	}
	var nilSettings *Settings
	if conf, allow := nilSettings.Thresholds(); conf != jevMinConfidence || allow != jevMinAllowProbability {
		t.Fatalf("nil settings thresholds = %v/%v", conf, allow)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("a value inside 1..100 was refused: %v", err)
	}
	tooHigh := 150
	if err := validThresholdSettings(&tooHigh).Validate(); err == nil {
		t.Error("a confidence above 100 percent was accepted")
	}
	zero := 0
	if err := validThresholdSettings(&zero).Validate(); err == nil {
		t.Error("a zero confidence was accepted")
	}
}
