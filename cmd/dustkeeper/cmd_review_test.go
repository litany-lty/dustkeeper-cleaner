package main

// The unresolved-request list is the only place a person can see what stopped
// the next run from asking again. A command that crashes while opening that list
// leaves no way to settle it except deleting the state - which is exactly the
// history-erasing move the record exists to prevent.
//
// These tests go through dispatchWith (the production router), so a nil store
// is a real panic, not a mocked error.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/store"
)

func TestReviewUnknownOpensTheStateDatabase(t *testing.T) {
	// One real scope and one real item: review_unknown has a foreign key to
	// items, so the fixture has to be a plausible installation rather than a
	// bare row.
	scopeID := newScopeWithQueue(t, []queueItem{{
		objectName: "release",
		reason:     "lifecycle",
		state:      "quarantined",
	}})

	// A command that never opened the store used to panic here. It must instead
	// report the same refusal every other read-only command gives.
	list := cli(t, "review", "unknown", "list", "--json")
	if list.code != 0 {
		t.Fatalf("list on an initialised instance exited %d\nstdout:\n%s\nstderr:\n%s",
			list.code, list.stdout, list.stderr)
	}
	data := dataOf(t, list)
	if got, _ := data["count"].(float64); got != 0 {
		t.Fatalf("count = %v, want 0 before any request was recorded", got)
	}

	st := openStateStore(t)
	err := st.RecordReviewUnknown(context.Background(), store.ReviewUnknown{
		ItemID:       "item-00",
		Action:       "purge",
		ScopeID:      string(scopeID),
		PathB64:      base64.StdEncoding.EncodeToString([]byte("releases/old/application.jar")),
		IdentityJSON: `{"size":42}`,
		TokenJSON:    `{"token":"t"}`,
		CreatedNS:    time.Now().UnixNano(),
		Attempts:     2,
		Failure:      "timeout",
		CostStatus:   "unknown",
	})
	st.Close()
	if err != nil {
		t.Fatalf("record an unresolved request: %v", err)
	}

	list = cli(t, "review", "unknown", "list", "--json")
	if list.code != 0 {
		t.Fatalf("list with one row exited %d\nstdout:\n%s", list.code, list.stdout)
	}
	data = dataOf(t, list)
	rows, ok := data["requests"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("requests = %v, want one row", data["requests"])
	}
	row, _ := rows[0].(map[string]any)
	if got, _ := row["path"].(string); got != "releases/old/application.jar" {
		t.Errorf("path = %q, want the decoded relative path", got)
	}
	if got, _ := row["cost_status"].(string); got != "unknown" {
		t.Errorf("cost_status = %q, want unknown", got)
	}

	// Neither flag alone is a settlement. Both are required, and neither call
	// may touch the row or crash on a nil store.
	for _, args := range [][]string{
		{"review", "unknown", "resolve", "--item", "item-00", "--action", "purge", "--note", "checked"},
		{"review", "unknown", "resolve", "--item", "item-00", "--action", "purge", "--confirm"},
	} {
		args = append(args, "--json")
		r := cli(t, args...)
		if r.code != 2 || r.str(t, "code") != "NEEDS_INPUT" {
			t.Errorf("%v exited %d (%s), want NEEDS_INPUT/2", args, r.code, r.stdout)
		}
	}

	resolved := cli(t, "review", "unknown", "resolve", "--item", "item-00",
		"--action", "purge", "--note", "supplier console: timed out, not billed", "--confirm", "--json")
	if resolved.code != 0 {
		t.Fatalf("resolve exited %d\nstdout:\n%s\nstderr:\n%s", resolved.code, resolved.stdout, resolved.stderr)
	}

	after := cli(t, "review", "unknown", "list", "--json")
	if got, _ := dataOf(t, after)["count"].(float64); got != 0 {
		t.Errorf("open count after resolve = %v, want 0", got)
	}
	all := cli(t, "review", "unknown", "list", "--all", "--json")
	if got, _ := dataOf(t, all)["count"].(float64); got != 1 {
		t.Errorf("resolved row disappeared from --all: count = %v", got)
	}
}

func TestReviewDispatchListShowsPendingRequests(t *testing.T) {
	newScopeWithQueue(t, []queueItem{{
		objectName: "release",
		reason:     "lifecycle",
		state:      "quarantined",
	}})
	empty := cli(t, "review", "dispatch", "list", "--json")
	if empty.code != 0 || dataOf(t, empty)["count"] != float64(0) {
		t.Fatalf("empty dispatch list: exit %d %s", empty.code, empty.stdout)
	}

	st := openStateStore(t)
	err := st.PrepareReviewDispatch(context.Background(), store.ReviewDispatch{
		DispatchID: "disp-1", ReviewID: "rev-1", RunID: "run-1",
		ItemID: "item-00", ScopeID: "scope-dispatch", Action: "purge",
		IdentityJSON: `{"size":1}`, TokenJSON: `{"mtime":1}`, BudgetJSON: "{}",
		CreatedNS: time.Now().UnixNano(), State: "pending", CostStatus: "not_sent",
	})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	listed := cli(t, "review", "dispatch", "list", "--json")
	if listed.code != 0 {
		t.Fatalf("dispatch list exited %d\n%s", listed.code, listed.stdout)
	}
	data := dataOf(t, listed)
	if data["count"] != float64(1) {
		t.Fatalf("count = %v, want 1", data["count"])
	}
	rows, _ := data["requests"].([]any)
	if len(rows) != 1 {
		t.Fatalf("requests = %v", data["requests"])
	}
	row, _ := rows[0].(map[string]any)
	if row["review_id"] != "rev-1" || row["state"] != "pending" || row["cost_status"] != "not_sent" {
		t.Errorf("pending request was not described honestly: %v", row)
	}
}

func TestReviewUnknownWithoutAnInstanceReportsItInsteadOfCrashing(t *testing.T) {
	newHome(t)
	r := cli(t, "review", "unknown", "list", "--json")
	if r.code == 0 {
		t.Fatalf("list without init exited 0: %s", r.stdout)
	}
	if got := r.str(t, "code"); got != "INVALID_ARGUMENT" {
		t.Errorf("code = %q, want INVALID_ARGUMENT", got)
	}
	if msg := r.str(t, "message"); !strings.Contains(msg, "run `dustkeeper init` first") {
		t.Errorf("the message does not explain the missing state: %q", msg)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatalf("refusal is not a JSON envelope: %v", err)
	}
}

// seedPendingDispatch puts one prepared request into the test instance without
// going through a model, so a resolve command has a real pending row to act on.
func seedPendingDispatch(t *testing.T, id string) {
	t.Helper()
	st := openStateStore(t)
	err := st.PrepareReviewDispatch(context.Background(), store.ReviewDispatch{
		DispatchID: id, ReviewID: "rev-" + id, RunID: "run-1",
		ItemID: "item-00", ScopeID: "scope-dispatch", Action: "purge",
		IdentityJSON: `{"size":1}`, TokenJSON: `{"mtime":1}`, BudgetJSON: "{}",
		CreatedNS: time.Now().UnixNano(), State: "pending", CostStatus: "not_sent",
	})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func anyDispatchResolution(t *testing.T) bool {
	t.Helper()
	st := openStateStore(t)
	defer st.Close()
	rows, err := st.ListReviewDispatch(context.Background(), true, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Resolution != nil {
			return true
		}
	}
	return false
}

// Missing confirmation/note, unchecked (unknown) statuses, a contradictory
// combination and an over-long note are all refused without changing the DB.
func TestReviewDispatchResolveRejectsUncheckedInputWithoutWriting(t *testing.T) {
	newScopeWithQueue(t, []queueItem{{objectName: "release", reason: "lifecycle", state: "quarantined"}})
	seedPendingDispatch(t, "disp-1")

	bad := [][]string{
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "completed", "--billing-status", "charged", "--note", "checked"},                                                           // no --confirm
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "completed", "--billing-status", "charged", "--confirm"},                                                                   // no note
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "completed", "--billing-status", "charged", "--note", "", "--confirm"},                                                     // empty note
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "unknown", "--billing-status", "charged", "--note", "checked", "--confirm"},                                                // unknown result
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "completed", "--billing-status", "unknown", "--note", "checked", "--confirm"},                                              // unknown cost
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "not_sent", "--billing-status", "charged", "--note", "checked", "--confirm"},                                               // contradiction
		{"review", "dispatch", "resolve", "--dispatch", "disp-1", "--request-status", "completed", "--billing-status", "charged", "--note", strings.Repeat("x", store.MaxDispatchResolutionNote+1), "--confirm"}, // too long
		{"review", "dispatch", "resolve", "--request-status", "completed", "--billing-status", "charged", "--note", "checked", "--confirm"},                                                                      // missing dispatch
	}
	for _, args := range bad {
		withJSON := append(append([]string{}, args...), "--json")
		r := cli(t, withJSON...)
		if r.code == 0 {
			t.Errorf("%v exited 0; want a refusal", args)
		}
	}
	if anyDispatchResolution(t) {
		t.Fatal("a refused resolve still wrote a resolution (database changed)")
	}
	// The unresolved row is still listed, with a copyable dispatch id.
	list := cli(t, "review", "dispatch", "list")
	if list.code != 0 || !strings.Contains(list.stdout, "dispatch_id=disp-1") {
		t.Fatalf("unresolved list lost the copyable dispatch id: exit %d\n%s", list.code, list.stdout)
	}
}

// The human output must state the gates, the new-id rule and the operator's own
// checks, and resolving the only pending row empties the default (unresolved) view.
func TestReviewDispatchResolveHumanOutputStatesTheGates(t *testing.T) {
	newScopeWithQueue(t, []queueItem{{objectName: "release", reason: "lifecycle", state: "quarantined"}})
	seedPendingDispatch(t, "disp-1")

	r := cli(t, "review", "dispatch", "resolve",
		"--dispatch", "disp-1", "--request-status", "completed",
		"--billing-status", "charged", "--note", "supplier console: billed once", "--confirm")
	if r.code != 0 {
		t.Fatalf("resolve exited %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"does NOT call a model",
		"normal budget, identity, protection, rule and model gates",
		"new request/review ID",
		"verify the approval",
		"remaining allowance",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("human output missing %q:\n%s", want, r.stdout)
		}
	}
	def := cli(t, "review", "dispatch", "list", "--json")
	if got, _ := dataOf(t, def)["count"].(float64); got != 0 {
		t.Errorf("default unresolved count = %v, want 0", got)
	}
	all := cli(t, "review", "dispatch", "list", "--all", "--json")
	if got, _ := dataOf(t, all)["count"].(float64); got != 1 {
		t.Errorf("--all lost the resolved row: count = %v, want 1", got)
	}
}

// A resolve lifts exactly one request; the JSON output carries the same
// constraints; --all keeps the history with the original facts unchanged and
// the manual conclusion separate; a repeat cannot overwrite the first record.
func TestReviewDispatchResolveLiftsOnlyOneAndKeepsHistory(t *testing.T) {
	newScopeWithQueue(t, []queueItem{{objectName: "release", reason: "lifecycle", state: "quarantined"}})
	seedPendingDispatch(t, "disp-1")
	seedPendingDispatch(t, "disp-2")

	resolve := cli(t, "review", "dispatch", "resolve",
		"--dispatch", "disp-1", "--request-status", "completed",
		"--billing-status", "charged", "--note", "supplier console: billed once", "--confirm", "--json")
	if resolve.code != 0 {
		t.Fatalf("resolve exited %d\n%s\n%s", resolve.code, resolve.stdout, resolve.stderr)
	}
	// data.notice must carry the same constraints in JSON mode.
	notice, _ := dataOf(t, resolve)["notice"].([]any)
	joined := ""
	for _, n := range notice {
		if s, ok := n.(string); ok {
			joined += s + "\n"
		}
	}
	for _, want := range []string{
		"does NOT call a model", "model gates", "new request/review ID",
		"verify the approval", "remaining allowance", "other unresolved requests",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("data.notice missing %q:\n%s", want, joined)
		}
	}

	// Default list: only the other unresolved row remains.
	def := cli(t, "review", "dispatch", "list", "--json")
	if got, _ := dataOf(t, def)["count"].(float64); got != 1 {
		t.Fatalf("default unresolved count = %v, want 1", got)
	}
	rows, _ := dataOf(t, def)["requests"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["dispatch_id"] != "disp-2" {
		t.Errorf("default list should show only disp-2: %v", rows)
	}

	// --all: both rows, original facts unchanged, independent conclusion visible.
	allJSON := cli(t, "review", "dispatch", "list", "--all", "--json")
	allRows, _ := dataOf(t, allJSON)["requests"].([]any)
	if len(allRows) != 2 {
		t.Fatalf("--all count = %v, want 2", allRows)
	}
	var resolvedRow map[string]any
	for _, raw := range allRows {
		row := raw.(map[string]any)
		if row["dispatch_id"] == "disp-1" {
			resolvedRow = row
		}
	}
	if resolvedRow == nil {
		t.Fatal("the resolved request disappeared from --all")
	}
	if resolvedRow["state"] != "pending" || resolvedRow["cost_status"] != "not_sent" {
		t.Errorf("original facts rewritten: %v", resolvedRow)
	}
	resMap, ok := resolvedRow["resolution"].(map[string]any)
	if !ok || resMap["request_status"] != "completed" {
		t.Errorf("independent resolution missing: %v", resolvedRow)
	}

	// Human --all separates preparation-time facts from the manual conclusion.
	allHuman := cli(t, "review", "dispatch", "list", "--all")
	for _, want := range []string{"recorded when prepared", "checked by a person later", "request_status=completed"} {
		if !strings.Contains(allHuman.stdout, want) {
			t.Errorf("--all human output missing %q:\n%s", want, allHuman.stdout)
		}
	}

	// Repeating does not overwrite the first record.
	again := cli(t, "review", "dispatch", "resolve",
		"--dispatch", "disp-1", "--request-status", "not_sent",
		"--billing-status", "not_charged", "--note", "a different story", "--confirm", "--json")
	if again.code == 0 {
		t.Fatal("a second confirmation was accepted")
	}
	after := cli(t, "review", "dispatch", "list", "--all", "--json")
	for _, raw := range dataOf(t, after)["requests"].([]any) {
		row := raw.(map[string]any)
		if row["dispatch_id"] == "disp-1" {
			res := row["resolution"].(map[string]any)
			if res["note"] != "supplier console: billed once" {
				t.Errorf("first record overwritten: %v", res)
			}
		}
	}
}
