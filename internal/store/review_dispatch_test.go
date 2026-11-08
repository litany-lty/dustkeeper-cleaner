package store

// A prepared request has exactly three exits: settled with its decision event,
// refused because the review never started, or still pending because the process
// died. These tests pin all three, plus the correlation that stops a re-ask.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/domain"
)

func dispatchStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := t.TempDir() + "/state.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// The dispatch table has no foreign key to items, but an event does; seed a
	// minimal scope, plan and item so the settlement event is a real one.
	now := time.Now().UnixNano()
	if err := st.write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO scopes(scope_id, raw_root, root_identity_json,
			instance_id, kind, generation, status, created_at_ns)
			VALUES('scope-dispatch', ?, '{}', 'inst-dispatch', 'managed_disposable', 1, 'active', ?)`,
			[]byte("/tmp/dispatch"), now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertPlan(context.Background(), Plan{
		ID: "plan-dispatch", Kind: "manual_rule", HeaderJSON: "{}", Digest: "d",
		Status: "complete", CreatedAtNS: now, ExpiresAtNS: now + int64(time.Hour),
	}, []Item{{
		ID: "item-dispatch", Ordinal: 0, ScopeID: "scope-dispatch",
		RawRelative: []byte("a.zip"), ObjectName: "obj-dispatch", Reason: "lifecycle",
		SourceKind: "manual_rule", BindingJSON: "{}", SourceParentIdentityJSON: "{}",
		PreIdentityJSON: "{}", PreTokenJSON: "{}", BoundRetentionSeconds: 7 * 24 * 60 * 60,
		State: "planned",
	}}); err != nil {
		t.Fatal(err)
	}
	// InsertPlan does not create a scope row for this fixture; events only need
	// the item, and scope_id on the dispatch row is plain text.
	return st, path
}

func preparedDispatch() ReviewDispatch {
	return ReviewDispatch{
		DispatchID: "disp-1", ReviewID: "rev-1", RunID: "run-1",
		ItemID: "item-dispatch", ScopeID: "scope-dispatch", Action: "quarantine",
		IdentityJSON: `{"size":10}`, TokenJSON: `{"mtime":1}`, BudgetJSON: "{}",
		CreatedNS: time.Now().UnixNano(), State: "pending", CostStatus: "not_sent",
	}
}

func TestPreparedRequestIsSettledWithItsDecisionEvent(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	pending, err := st.OpenDispatchFor(ctx, "item-dispatch", "quarantine", `{"size":10}`, `{"mtime":1}`)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v; want one row", pending, err)
	}
	if got, err := st.OpenDispatchFor(ctx, "item-dispatch", "quarantine", `{"size":11}`, `{"mtime":1}`); err != nil || len(got) != 0 {
		t.Fatalf("a changed file was treated as the same question: %v, %v", got, err)
	}
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err == nil {
		t.Fatal("a second pending row for one review was accepted")
	}

	item := domain.ItemID("item-dispatch")
	plan := domain.PlanID("plan-dispatch")
	settled := DispatchSettlement{Attempts: 2, CostStatus: "usage_recorded",
		ElapsedMS: 320, HTTPStatus: 200, InputTokens: 12, OutputTokens: 4,
		SettledNS: time.Now().UnixNano(), Note: "dual_review_agrees"}
	ev := NewReviewEvent("rev-1", item, &plan, settled.SettledNS, settled, domain.VerdictAllow)
	if err := st.SettleReviewDispatch(ctx, "rev-1", settled, ev); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListReviewDispatch(ctx, true, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("rows = %v, %v", all, err)
	}
	if all[0].State != "settled" || all[0].Attempts != 2 || all[0].InputTokens != 12 ||
		all[0].CostStatus != "usage_recorded" || all[0].SettledNS == nil {
		t.Fatalf("settlement facts lost: %+v", all[0])
	}
	if got, _ := st.ListReviewDispatch(ctx, false, 10); len(got) != 0 {
		t.Errorf("settled row is still pending: %v", got)
	}
	rows, err := st.ListEvents(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "review" && strings.Contains(*row.ResultJSON, `"review_id":"rev-1"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("the settlement event was not written with the decision")
	}
	// Settling twice is refused: history is not rewritable.
	if err := st.SettleReviewDispatch(ctx, "rev-1", settled, ev); err == nil {
		t.Fatal("a settled dispatch was settled again")
	}
}

func TestALocalRefusalClosesThePreparedRequest(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	item := domain.ItemID("item-dispatch")
	plan := domain.PlanID("plan-dispatch")
	at := time.Now().UnixNano()
	stt := DispatchSettlement{CostStatus: "not_sent", SettledNS: at}
	if err := st.RefuseReviewDispatch(ctx, "rev-1", "scope paused before review", at,
		NewReviewEvent("rev-1", item, &plan, at, stt, "not_started")); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListReviewDispatch(ctx, true, 10)
	if len(all) != 1 || all[0].State != "refused" || all[0].CostStatus != "not_sent" ||
		!strings.Contains(all[0].Note, "paused") {
		t.Fatalf("refusal was not recorded: %+v", all)
	}
	if pending, _ := st.OpenDispatchFor(ctx, "item-dispatch", "quarantine", `{"size":10}`, `{"mtime":1}`); len(pending) != 0 {
		t.Errorf("a refused request still blocks a future question: %v", pending)
	}
}

func TestSettlementIsOneTransaction(t *testing.T) {
	st, path := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	item := domain.ItemID("item-dispatch")
	plan := domain.PlanID("plan-dispatch")
	settled := DispatchSettlement{Attempts: 1, CostStatus: "unknown", SettledNS: time.Now().UnixNano()}
	// An event with a duplicate id forces the second half of the transaction to
	// fail. The row must stay pending and no review event may be left behind.
	existing := NewReviewEvent("rev-1", item, &plan, settled.SettledNS, settled, "keep")
	existing.Action = "keep" // occupy the event id without looking like a settlement
	st.AppendEvent(ctx, existing)
	bad := existing
	bad.EventID = existing.EventID
	if err := st.SettleReviewDispatch(ctx, "rev-1", settled, bad); err == nil {
		t.Fatal("a duplicate event id did not fail the settlement")
	}
	pending, err := st.OpenDispatchFor(ctx, "item-dispatch", "quarantine", `{"size":10}`, `{"mtime":1}`)
	if err != nil || len(pending) != 1 {
		t.Fatalf("a failed settlement closed the request: %v, %v", pending, err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reviews int
	if err := db.QueryRow("SELECT count(*) FROM events WHERE action='review'").Scan(&reviews); err != nil {
		t.Fatal(err)
	}
	if reviews != 0 {
		t.Errorf("a rolled-back settlement left %d review event(s)", reviews)
	}
}

// checkedResolution is one honest manual conclusion for a dispatch id.
func checkedResolution(id string, at int64) (DispatchResolution, Event) {
	res := DispatchResolution{
		ResolvedNS:    at,
		RequestStatus: "completed",
		BillingStatus: "charged",
		Note:          "supplier console: billed once",
	}
	return res, NewReviewDispatchResolutionEvent(id, res, at)
}

func openDispatches(t *testing.T, st *Store) []ReviewDispatch {
	t.Helper()
	rows, err := st.OpenDispatchFor(context.Background(), "item-dispatch", "quarantine",
		`{"size":10}`, `{"mtime":1}`)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A pending request blocks a re-ask until a person reconciles it; resolving one
// lifts exactly that one request, and the history stays queryable.
func TestManualResolutionLiftsOnlyThatDispatch(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	first := preparedDispatch()
	second := preparedDispatch()
	second.DispatchID, second.ReviewID = "disp-2", "rev-2"
	if err := st.PrepareReviewDispatch(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.PrepareReviewDispatch(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := len(openDispatches(t, st)); got != 2 {
		t.Fatalf("pending before resolution = %d, want 2", got)
	}

	at := time.Now().UnixNano()
	res, ev := checkedResolution("disp-1", at)
	if err := st.ResolveReviewDispatch(ctx, "disp-1", res, ev); err != nil {
		t.Fatal(err)
	}
	if got := len(openDispatches(t, st)); got != 1 {
		t.Fatalf("one resolution lifted %d requests, want exactly 1", got)
	}

	// Default list = the one still-unresolved row.
	pending, err := st.ListReviewDispatch(ctx, false, 10)
	if err != nil || len(pending) != 1 || pending[0].DispatchID != "disp-2" {
		t.Fatalf("default list = %v, %v; want only disp-2", pending, err)
	}
	// --all keeps history and carries the separate, independent conclusion.
	all, err := st.ListReviewDispatch(ctx, true, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("--all lost history: %v, %v", all, err)
	}
	var resolvedRow *ReviewDispatch
	for i := range all {
		if all[i].DispatchID == "disp-1" {
			resolvedRow = &all[i]
		}
	}
	if resolvedRow == nil {
		t.Fatal("the resolved request disappeared from --all")
	}
	// The original row is NOT rewritten: still pending, preparation-time facts
	// intact, and the conclusion is a separate object.
	if resolvedRow.State != "pending" || resolvedRow.CostStatus != "not_sent" || resolvedRow.Attempts != 0 {
		t.Errorf("manual resolution rewrote the original row: %+v", resolvedRow)
	}
	if resolvedRow.Resolution == nil || resolvedRow.Resolution.RequestStatus != "completed" ||
		resolvedRow.Resolution.BillingStatus != "charged" || resolvedRow.Resolution.Note != "supplier console: billed once" {
		t.Errorf("independent conclusion not shown: %+v", resolvedRow.Resolution)
	}
	// The independent event is written and adds no model/attempt/usage facts.
	events, err := st.ListEvents(ctx, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == "review_dispatch_resolution" && strings.Contains(*e.ResultJSON, `"dispatch_id":"disp-1"`) {
			found = true
			for _, forbidden := range []string{"attempts", "input_tokens", "verdict", "http_attempts"} {
				if strings.Contains(*e.ResultJSON, forbidden) {
					t.Errorf("resolution event carries %q", forbidden)
				}
			}
		}
	}
	if !found {
		t.Fatal("the independent manual event was not written")
	}
}

// Missing/unchecked facts must be refused without writing a row or an event.
func TestManualResolutionRejectsUncheckedInput(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixNano()
	bad := []struct{ name, req, bill, note string }{
		{"unknown request result", "unknown", "charged", "checked"},
		{"unknown billing", "completed", "unknown", "checked"},
		{"contradictory", "not_sent", "charged", "checked"},
		{"empty note", "completed", "charged", ""},
		{"blank note", "completed", "charged", "   "},
		{"over-long note", "completed", "charged", strings.Repeat("x", MaxDispatchResolutionNote+1)},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			res := DispatchResolution{ResolvedNS: at, RequestStatus: c.req, BillingStatus: c.bill, Note: c.note}
			ev := NewReviewDispatchResolutionEvent("disp-1", res, at)
			if err := st.ResolveReviewDispatch(ctx, "disp-1", res, ev); err == nil {
				t.Fatalf("accepted unchecked input %+v", c)
			}
		})
	}
	all, _ := st.ListReviewDispatch(ctx, true, 10)
	for _, r := range all {
		if r.Resolution != nil {
			t.Fatal("refused input still wrote a resolution row")
		}
	}
	evs, _ := st.ListEvents(ctx, 50, 0)
	for _, e := range evs {
		if e.Action == "review_dispatch_resolution" {
			t.Fatal("refused input still wrote a resolution event")
		}
	}
	if got := len(openDispatches(t, st)); got != 1 {
		t.Fatalf("refused input unblocked the request: pending = %d", got)
	}
}

// A second confirmation cannot overwrite the first record, and a settled or
// refused request cannot be reopened.
func TestManualResolutionKeepsFirstRecordAndClosedRows(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixNano()
	res, ev := checkedResolution("disp-1", at)
	if err := st.ResolveReviewDispatch(ctx, "disp-1", res, ev); err != nil {
		t.Fatal(err)
	}

	second := DispatchResolution{ResolvedNS: at + 1, RequestStatus: "not_sent",
		BillingStatus: "not_charged", Note: "a different story"}
	ev2 := NewReviewDispatchResolutionEvent("disp-1", second, at+1)
	if err := st.ResolveReviewDispatch(ctx, "disp-1", second, ev2); err == nil {
		t.Fatal("a second confirmation overwrote the first record")
	}
	all, _ := st.ListReviewDispatch(ctx, true, 10)
	for _, r := range all {
		if r.DispatchID == "disp-1" {
			if r.Resolution == nil || r.Resolution.Note != "supplier console: billed once" {
				t.Fatalf("first record was overwritten: %+v", r.Resolution)
			}
		}
	}

	// A refused request is closed and cannot be opened by this command.
	refused := preparedDispatch()
	refused.DispatchID, refused.ReviewID = "disp-2", "rev-2"
	if err := st.PrepareReviewDispatch(ctx, refused); err != nil {
		t.Fatal(err)
	}
	item := domain.ItemID("item-dispatch")
	plan := domain.PlanID("plan-dispatch")
	stt := DispatchSettlement{CostStatus: "not_sent", SettledNS: at}
	if err := st.RefuseReviewDispatch(ctx, "rev-2", "paused", at,
		NewReviewEvent("rev-2", item, &plan, at, stt, "not_started")); err != nil {
		t.Fatal(err)
	}
	res2, ev2b := checkedResolution("disp-2", at)
	if err := st.ResolveReviewDispatch(ctx, "disp-2", res2, ev2b); err == nil {
		t.Fatal("a refused request was reopened")
	}
	res3, ev3 := checkedResolution("does-not-exist", at)
	if err := st.ResolveReviewDispatch(ctx, "does-not-exist", res3, ev3); err == nil {
		t.Fatal("a resolution for a non-existent dispatch was accepted")
	}
}

// If the independent event write fails, the resolution row must roll back too:
// "unblocked but no evidence" is forbidden.
func TestManualResolutionRollsBackWhenEventFails(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixNano()
	res, _ := checkedResolution("disp-1", at)
	// Occupy an event id, then make the resolution's event reuse it. The same
	// duplicate-id fault injection the settlement test uses.
	occupied := NewReviewDispatchResolutionEvent("disp-1", res, at)
	occupied.Action = "keep"
	if err := st.AppendEvent(ctx, occupied); err != nil {
		t.Fatal(err)
	}
	ev := NewReviewDispatchResolutionEvent("disp-1", res, at)
	ev.EventID = occupied.EventID
	if err := st.ResolveReviewDispatch(ctx, "disp-1", res, ev); err == nil {
		t.Fatal("a duplicate event id did not fail the resolution")
	}
	if got := len(openDispatches(t, st)); got != 1 {
		t.Fatalf("a failed resolution unblocked the request: pending = %d", got)
	}
	all, _ := st.ListReviewDispatch(ctx, true, 10)
	if len(all) != 1 || all[0].Resolution != nil {
		t.Fatalf("a rolled-back resolution left a conclusion: %+v", all)
	}
	evs, _ := st.ListEvents(ctx, 50, 0)
	for _, e := range evs {
		if e.Action == "review_dispatch_resolution" {
			t.Fatal("a rolled-back resolution left its event")
		}
	}
}

// Resolving a dispatch does not touch an independent review_unknown record.
func TestManualResolutionLeavesReviewUnknownIndependent(t *testing.T) {
	st, _ := dispatchStore(t)
	ctx := context.Background()
	if err := st.PrepareReviewDispatch(ctx, preparedDispatch()); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordReviewUnknown(ctx, ReviewUnknown{
		ItemID: "item-dispatch", Action: "quarantine", ScopeID: "scope-dispatch",
		PathB64: "YS56aXA=", IdentityJSON: `{"size":10}`, TokenJSON: `{"mtime":1}`,
		CreatedNS: time.Now().UnixNano(), Attempts: 1, Failure: "timeout", CostStatus: "unknown",
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixNano()
	res, ev := checkedResolution("disp-1", at)
	if err := st.ResolveReviewDispatch(ctx, "disp-1", res, ev); err != nil {
		t.Fatal(err)
	}
	u, err := st.OpenReviewUnknown(ctx, "scope-dispatch", "YS56aXA=", "quarantine")
	if err != nil || len(u) != 1 {
		t.Fatalf("review_unknown lost its independent hold: %v, %v", u, err)
	}
	if u[0].ResolvedNS != nil {
		t.Error("review_unknown was wrongly resolved")
	}
}
