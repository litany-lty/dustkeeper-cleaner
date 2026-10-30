package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

// The status view answers "how much is waiting, and is it going down". A number
// that stops at one tells the operator the backlog is one no matter how large it
// is, and a read error reported as zero says the backlog is empty. Both were true
// of this command: the count came from len(ItemsDueForPurge(..., 1)) and three
// reads threw their error away.

// queueItem describes one object placed in the state database directly. Only the
// columns the purge-queue filter reads are interesting here; everything else is
// filler that satisfies the schema.
type queueItem struct {
	objectName    string
	reason        string
	state         string
	notBeforeNano *int64
	pendingOp     *domain.OperationID
}

// seedQueue writes one plan holding exactly these objects, so the tests own the
// population instead of inheriting it from a scan.
func seedQueue(t *testing.T, scopeID domain.ScopeID, specs []queueItem) {
	t.Helper()
	if len(specs) == 0 {
		return
	}
	st := openStateStore(t)
	defer st.Close()

	now := time.Now()
	quarantined := now.Add(-2 * time.Hour).UnixNano()
	items := make([]store.Item, 0, len(specs))
	for i, spec := range specs {
		items = append(items, store.Item{
			ID:                       domain.ItemID(fmt.Sprintf("item-%02d", i)),
			Ordinal:                  i,
			ScopeID:                  scopeID,
			RawRelative:              []byte(spec.objectName + ".zip"),
			ObjectName:               spec.objectName,
			Reason:                   spec.reason,
			SourceKind:               "manual_rule",
			BindingJSON:              "{}",
			SourceParentIdentityJSON: "{}",
			PreIdentityJSON:          "{}",
			PreTokenJSON:             "{}",
			BoundRetentionSeconds:    7 * 24 * 60 * 60,
			State:                    spec.state,
			PendingOpID:              spec.pendingOp,
			QuarantinedAtNS:          &quarantined,
			PurgeNotBeforeNS:         spec.notBeforeNano,
		})
	}
	plan := store.Plan{
		ID:          "plan-queue",
		Kind:        "manual_rule",
		HeaderJSON:  "{}",
		Digest:      "digest-queue",
		Status:      "complete",
		CreatedAtNS: now.UnixNano(),
		ExpiresAtNS: now.Add(time.Hour).UnixNano(),
	}
	if err := st.InsertPlan(context.Background(), plan, items); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
}

func dueAt(t *testing.T, d time.Duration) *int64 {
	t.Helper()
	v := time.Now().Add(d).UnixNano()
	return &v
}

func pendingOp() *domain.OperationID {
	v := domain.OperationID("op-in-flight")
	return &v
}

// newScopeWithQueue creates an installation with one manageable scope and puts
// the given objects into its state database.
func newScopeWithQueue(t *testing.T, specs []queueItem) domain.ScopeID {
	t.Helper()
	base := newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init exited %d: %s", r.code, r.stdout)
	}
	dir := filepath.Join(base, "tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	add := cli(t, "scope", "add", "--path", dir, "--kind", "managed_disposable", "--name", "demo", "--json")
	if add.code != 0 {
		t.Fatalf("scope add exited %d: %s", add.code, add.stdout)
	}
	seedQueue(t, domain.ScopeID(add.str(t, "scope_id")), specs)
	return domain.ScopeID(add.str(t, "scope_id"))
}

// dueFor reads one scope's due_for_purge_count out of a status envelope.
func dueFor(t *testing.T, r result, scopeID domain.ScopeID) int64 {
	t.Helper()
	data, ok := r.env["data"].(map[string]any)
	if !ok {
		t.Fatalf("status has no data object: %s", r.stdout)
	}
	list, _ := data["scopes"].([]any)
	for _, entry := range list {
		row, _ := entry.(map[string]any)
		if row["scope_id"] != string(scopeID) {
			continue
		}
		// The JSON number the operator reads must be the count, not a page of it.
		n, ok := row["due_for_purge_count"].(float64)
		if !ok {
			t.Fatalf("due_for_purge_count is %v, want a number", row["due_for_purge_count"])
		}
		return int64(n)
	}
	t.Fatalf("status did not report scope %q: %s", scopeID, r.stdout)
	return 0
}

// TestStatusReportsEveryDueObject is the defect itself: three objects were due
// and the command said one, because the query underneath returned at most one.
func TestStatusReportsEveryDueObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"none", 0},
		{"one", 1},
		{"three", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := make([]queueItem, 0, tc.n)
			for i := 0; i < tc.n; i++ {
				specs = append(specs, queueItem{
					objectName:    fmt.Sprintf("obj-%02d", i),
					reason:        "lifecycle",
					state:         domain.StateQuarantined,
					notBeforeNano: dueAt(t, -time.Minute),
				})
			}
			sc := newScopeWithQueue(t, specs)

			r := cli(t, "status", "--json")
			if r.code != 0 {
				t.Fatalf("status exited %d: %s", r.code, r.stdout)
			}
			if got := dueFor(t, r, sc); got != int64(tc.n) {
				t.Errorf("due_for_purge_count = %d, want %d", got, tc.n)
			}

			// The human line has to agree with the JSON: two views of one number
			// that disagree is how an operator learns to trust neither.
			human := cli(t, "status")
			if human.code != 0 {
				t.Fatalf("status exited %d: %s", human.code, human.stdout)
			}
			if !strings.Contains(human.stdout, fmt.Sprintf("due=%d", tc.n)) {
				t.Errorf("the human line does not say due=%d:\n%s", tc.n, human.stdout)
			}
		})
	}
}

// TestStatusCountsOnlyThePurgeQueue keeps the count tied to the filter: the same
// filter a run uses to pick work. An object that is not in the queue is not a
// backlog, whatever else it is.
func TestStatusCountsOnlyThePurgeQueue(t *testing.T) {
	const due = domain.StateQuarantined
	sc := newScopeWithQueue(t, []queueItem{
		{objectName: "due-1", reason: "lifecycle", state: due, notBeforeNano: dueAt(t, -time.Minute)},
		{objectName: "due-2", reason: "lifecycle", state: due, notBeforeNano: dueAt(t, -24*time.Hour)},
		{objectName: "due-3", reason: "lifecycle", state: due, notBeforeNano: dueAt(t, -time.Second)},
		{objectName: "too-early", reason: "lifecycle", state: due, notBeforeNano: dueAt(t, time.Hour)},
		{objectName: "in-flight", reason: "lifecycle", state: due, notBeforeNano: dueAt(t, -time.Minute), pendingOp: pendingOp()},
		{objectName: "no-bound", reason: "lifecycle", state: due},
		{objectName: "historical-duplicate", reason: "exact_duplicate", state: due, notBeforeNano: dueAt(t, -time.Minute)},
		{objectName: "restored", reason: "lifecycle", state: domain.StateRestored, notBeforeNano: dueAt(t, -time.Minute)},
		{objectName: "purged", reason: "lifecycle", state: "purged", notBeforeNano: dueAt(t, -time.Minute)},
		{objectName: "planned", reason: "lifecycle", state: domain.StatePlanned},
	})

	r := cli(t, "status", "--json")
	if r.code != 0 {
		t.Fatalf("status exited %d: %s", r.code, r.stdout)
	}
	if got := dueFor(t, r, sc); got != 3 {
		t.Errorf("due_for_purge_count = %d, want 3 (only the three that are due)", got)
	}
}

// TestStatusDoesNotReportAFailedCountAsZero injects one failing read and requires
// the command to fail with it.
//
// The fault is a column the count reads and nothing else does: renaming
// observations.status leaves the table-level totals in the same command working
// (COUNT(*) does not name a column) and leaves the pending-operation scan alone,
// so the only thing that breaks is the scoped count. A coarser fault - dropping a
// table or a whole column set - is caught earlier by Stats or PendingOps, which
// already reported their errors, so it would not show this defect at all.
func TestStatusDoesNotReportAFailedCountAsZero(t *testing.T) {
	newScopeWithQueue(t, []queueItem{
		{objectName: "due-1", reason: "lifecycle", state: domain.StateQuarantined, notBeforeNano: dueAt(t, -time.Minute)},
	})

	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	db, err := sql.Open("sqlite3", paths.DBFile)
	if err != nil {
		t.Fatalf("open state file: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE observations RENAME COLUMN status TO status_renamed`); err != nil {
		t.Fatalf("breaking the count read: %v", err)
	}

	r := cli(t, "status", "--json")
	if r.code == 0 {
		t.Fatalf("status reported success while a count could not be read:\n%s", r.stdout)
	}
	if code := r.str(t, "code"); code != "DB_ERROR" {
		t.Errorf("code = %q, want DB_ERROR", code)
	}
	if strings.Contains(r.stdout, "observation_count") {
		t.Errorf("status printed a count it could not read:\n%s", r.stdout)
	}

	// The per-scope view showed the same zeros and had the same silence.
	if show := cli(t, "scope", "show", "--id", "demo", "--json"); show.code == 0 {
		t.Errorf("scope show reported success while a count could not be read:\n%s", show.stdout)
	}
}
