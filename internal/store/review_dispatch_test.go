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
