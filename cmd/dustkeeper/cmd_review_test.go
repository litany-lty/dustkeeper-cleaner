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
