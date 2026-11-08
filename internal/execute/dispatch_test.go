package execute

import (
	"context"
	"encoding/json"
	"strings"
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

// A pending dispatch blocks a re-ask; a manual resolution lifts exactly that
// one record, every other pending dispatch keeps its own hold, and the original
// request history stays queryable.
func TestHeldUnknownRequestLiftsOnlyResolvedDispatch(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	identity, token := `{"size":10}`, `{"mtime":1}`
	prep := func(id string) {
		if err := st.PrepareReviewDispatch(ctx, store.ReviewDispatch{
			DispatchID: id, ReviewID: "rev-" + id, RunID: "run-1",
			ItemID: "item-x", ScopeID: "scope-x", Action: "purge",
			IdentityJSON: identity, TokenJSON: token, BudgetJSON: "{}",
			CreatedNS: time.Now().UnixNano(), State: "pending", CostStatus: "not_sent",
		}); err != nil {
			t.Fatal(err)
		}
	}
	prep("disp-1")
	prep("disp-2")

	e := &Executor{Store: st}
	sc := &ScopeCtx{Scope: store.Scope{ID: domain.ScopeID("scope-x")}}
	it := store.Item{ID: domain.ItemID("item-x"), RawRelative: []byte("a.zip")}
	held := func() bool {
		h, err := e.heldUnknownRequest(ctx, sc, it, "purge", identity, token)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if !held() {
		t.Fatal("pending dispatches did not block a re-ask")
	}

	at := time.Now().UnixNano()
	res := store.DispatchResolution{ResolvedNS: at, RequestStatus: "completed",
		BillingStatus: "charged", Note: "checked"}
	if err := st.ResolveReviewDispatch(ctx, "disp-1", res,
		store.NewReviewDispatchResolutionEvent("disp-1", res, at)); err != nil {
		t.Fatal(err)
	}
	if !held() {
		t.Fatal("an unrelated pending dispatch no longer blocks a re-ask")
	}
	if err := st.ResolveReviewDispatch(ctx, "disp-2", res,
		store.NewReviewDispatchResolutionEvent("disp-2", res, at)); err != nil {
		t.Fatal(err)
	}
	if held() {
		t.Fatal("resolving both dispatches did not lift the hold")
	}
	// History is still queryable through the --all view.
	all, err := st.ListReviewDispatch(ctx, true, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("history lost: %v, %v", all, err)
	}
}

// The measured timings must reach both the operational log record and the
// settlement event, keyed by the real review id, so the result page can join
// them. Neither view may contain a URL, header, body or credential.
func TestSettleReviewCarriesTimingsToDiagAndEvent(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	var got []ReviewDiag
	e := &Executor{Store: st, Clock: domain.NewSystemClock(), RunID: "run-timing",
		Diag: func(d ReviewDiag) { got = append(got, d) }}
	it := store.Item{ID: domain.ItemID("itm-timing")}
	// The settlement event carries the item id as a foreign key, so the item
	// must exist together with its scope and plan.
	scopeID := domain.ScopeID("scope-timing")
	if err := st.InsertScope(ctx, store.Scope{
		ID: scopeID, RawRoot: []byte("/tmp/scope-timing"),
		RootIdentityJSON: `{"persistent":{"inode":1}}`, InstanceID: "inst-1",
		Kind: domain.KindManagedDisposable, Generation: 1, ProtectionRevision: 1,
		Status: domain.ScopeActive, ScanEpoch: 0, CreatedAtNS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertPlan(ctx, store.Plan{
		ID: domain.PlanID("plan-timing"), Kind: domain.PlanManualRule, HeaderJSON: `{}`,
		Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Status: domain.PlanReady, CreatedAtNS: 1, ExpiresAtNS: 1 << 60,
	}, []store.Item{{
		ID: it.ID, ScopeID: scopeID, RawRelative: []byte("old.zip"),
		ObjectName: "old.zip", Reason: domain.ReasonLifecycle,
		SourceKind: domain.SourceManualRule, BindingJSON: `{}`,
		SourceParentIdentityJSON: `{}`, PreIdentityJSON: `{"inode":5}`,
		PreTokenJSON: `{"size":10}`, BoundRetentionSeconds: 2592000,
		State: domain.StatePlanned,
	}}); err != nil {
		t.Fatal(err)
	}
	timings := []review.RequestTiming{
		{Provider: "jev", Outcome: "ok", Phase: "response_body", ElapsedMS: 120,
			StagesMS: map[string]int64{"dns_start": 1, "dns_done": 6, "connection_ready": 22, "first_byte": 100}},
		{Provider: "minimax", Outcome: "ok", Phase: "response_body", ElapsedMS: 210,
			StagesMS: map[string]int64{"connection_ready": 5, "first_byte": 150}},
	}
	out := review.Outcome{
		ReviewID: "rev-timing",
		Decision: review.Decision{Verdict: domain.VerdictKeep, RequestTimings: timings},
	}
	if err := e.prepareReview(ctx, it, "scope-timing", domain.ActionQuarantine, "rev-timing",
		`{"action":"quarantine"}`, `{"size":1}`, `{"mtime":1}`); err != nil {
		t.Fatal(err)
	}
	if err := e.settleReview(ctx, it, nil, "rev-timing", domain.ActionQuarantine,
		time.Now(), out, `{"action":"quarantine"}`); err != nil {
		t.Fatal(err)
	}

	var settled []ReviewDiag
	for _, d := range got {
		if d.State == "settled" {
			settled = append(settled, d)
		}
	}
	if len(settled) != 1 {
		t.Fatalf("got %d settled diag records, want 1: %+v", len(settled), got)
	}
	if len(settled[0].RequestTimings) != 2 || settled[0].RequestTimings[0].StagesMS["dns_done"] != 6 {
		t.Fatalf("timings or stages_ms lost in the diag record: %+v", settled[0].RequestTimings)
	}

	events, err := st.ListEvents(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		ReviewID       string                 `json:"review_id"`
		RequestTimings []review.RequestTiming `json:"request_timings"`
	}
	settlementJSON, found := "", false
	for _, ev := range events {
		if ev.Action != "review" || ev.ResultJSON == nil {
			continue
		}
		if json.Unmarshal([]byte(*ev.ResultJSON), &body) == nil && body.ReviewID == "rev-timing" {
			settlementJSON, found = *ev.ResultJSON, true
		}
	}
	if !found || len(body.RequestTimings) != 2 || body.RequestTimings[1].StagesMS["first_byte"] != 150 {
		t.Fatalf("settlement event lost timings: found=%v body=%+v", found, body)
	}
	for _, forbidden := range []string{"http://", "https://", "Authorization", "Bearer", "api_key", "proxy", "secret"} {
		if strings.Contains(settlementJSON, forbidden) {
			t.Errorf("settlement event carries forbidden payload %q: %s", forbidden, settlementJSON)
		}
	}
}
