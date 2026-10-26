package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/migrations"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedScope(t *testing.T, s *Store, id domain.ScopeID, kind string) {
	t.Helper()
	err := s.InsertScope(context.Background(), Scope{
		ID:                 id,
		RawRoot:            []byte("/tmp/" + string(id)),
		RootIdentityJSON:   `{"persistent":{"inode":1}}`,
		InstanceID:         "inst-1",
		Kind:               kind,
		Generation:         1,
		ProtectionRevision: 1,
		Status:             domain.ScopeActive,
		ScanEpoch:          0,
		CreatedAtNS:        1,
	})
	if err != nil {
		t.Fatalf("InsertScope: %v", err)
	}
}

func seedItem(t *testing.T, s *Store, scope domain.ScopeID, itemID domain.ItemID, planID domain.PlanID) {
	t.Helper()
	ctx := context.Background()
	p := Plan{
		ID:          planID,
		Kind:        domain.PlanManualRule,
		HeaderJSON:  `{"plan_id":"` + string(planID) + `"}`,
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Status:      domain.PlanReady,
		CreatedAtNS: 1,
		ExpiresAtNS: 1 << 60,
	}
	it := Item{
		ID:                       itemID,
		Ordinal:                  0,
		ScopeID:                  scope,
		RawRelative:              []byte("old.zip"),
		ObjectName:               string(itemID),
		Reason:                   domain.ReasonLifecycle,
		SourceKind:               domain.SourceManualRule,
		BindingJSON:              `{}`,
		SourceParentIdentityJSON: `{}`,
		PreIdentityJSON:          `{"inode":5}`,
		PreTokenJSON:             `{"size":10}`,
		BoundRetentionSeconds:    2592000,
		State:                    domain.StatePlanned,
	}
	if err := s.InsertPlan(ctx, p, []Item{it}); err != nil {
		t.Fatalf("InsertPlan: %v", err)
	}
}

// TestSchemaAndPragmas checks the migration and the read-back of every pragma.
func TestSchemaAndPragmas(t *testing.T) {
	s := newTestStore(t)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrations.CurrentVersion {
		t.Fatalf("user_version = %d, want %d", version, migrations.CurrentVersion)
	}
	meta, err := s.MetaGet(context.Background(), "schema_version")
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(migrations.CurrentVersion); meta != want {
		t.Fatalf("meta schema_version = %q, want %q", meta, want)
	}
}

// TestNewerSchemaRefused proves a newer database is never opened by an older
// binary: downgrading would keep cleaning with the wrong understanding.
func TestNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("expected refusal to open a newer schema")
	}
	if code := errs.CodeOf(err); code != errs.CodeMigrationTooNew {
		t.Fatalf("code = %s, want %s", code, errs.CodeMigrationTooNew)
	}
}

// TestIntentAndStateCommitTogether proves the state change and the intent event
// are one transaction: if the intent cannot be written, the state does not move.
func TestIntentAndStateCommitTogether(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)
	seedItem(t, s, "exports", "item_1", "plan_1")

	in := Intent{
		OpID:         "op_1",
		ItemID:       "item_1",
		Action:       "quarantine",
		PriorState:   domain.StatePlanned,
		NextState:    domain.StateQIntent,
		ContractJSON: `{"purpose":"normal"}`,
		OccurredAtNS: 100,
	}
	if err := s.BeginIntent(ctx, in); err != nil {
		t.Fatalf("BeginIntent: %v", err)
	}
	it, err := s.GetItem(ctx, "item_1")
	if err != nil {
		t.Fatal(err)
	}
	if it.State != domain.StateQIntent {
		t.Fatalf("state = %q", it.State)
	}
	if it.PendingOpID == nil || *it.PendingOpID != "op_1" {
		t.Fatalf("pending op = %v", it.PendingOpID)
	}
	events, err := s.EventsForItem(ctx, "item_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Phase != domain.PhaseIntent {
		t.Fatalf("events = %+v", events)
	}

	// A second intent for the same item must be refused, not queued.
	err = s.BeginIntent(ctx, Intent{
		OpID: "op_2", ItemID: "item_1", Action: "quarantine",
		PriorState: domain.StateQIntent, NextState: domain.StateQIntent,
		ContractJSON: `{}`, OccurredAtNS: 101,
	})
	if err == nil {
		t.Fatal("second intent should be refused")
	}
	if code := errs.CodeOf(err); code != errs.CodePlanStale {
		t.Fatalf("code = %s, want PLAN_STALE", code)
	}
}

// TestWrongPriorStateLeavesNoTrace is the "half-written state must not drive a
// file action" test: a refused intent changes nothing.
func TestWrongPriorStateLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)
	seedItem(t, s, "exports", "item_1", "plan_1")

	err := s.BeginIntent(ctx, Intent{
		OpID: "op_1", ItemID: "item_1", Action: "quarantine",
		PriorState: domain.StateQuarantined, NextState: domain.StateRIntent,
		ContractJSON: `{}`, OccurredAtNS: 1,
	})
	if err == nil {
		t.Fatal("intent with wrong prior state should fail")
	}
	it, _ := s.GetItem(ctx, "item_1")
	if it.State != domain.StatePlanned {
		t.Fatalf("state changed to %q on a failed intent", it.State)
	}
	if it.PendingOpID != nil {
		t.Fatal("pending op set by a failed intent")
	}
	events, _ := s.EventsForItem(ctx, "item_1")
	if len(events) != 0 {
		t.Fatalf("failed intent left events: %+v", events)
	}
}

// TestCompleteQuarantineIsIdempotent covers the reconciliation requirement that
// replaying a terminal step does not duplicate audit records.
func TestCompleteQuarantineIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)
	seedItem(t, s, "exports", "item_1", "plan_1")

	in := Intent{
		OpID: "op_1", ItemID: "item_1", Action: "quarantine",
		PriorState: domain.StatePlanned, NextState: domain.StateQIntent,
		ContractJSON: `{}`, OccurredAtNS: 10,
	}
	if err := s.BeginIntent(ctx, in); err != nil {
		t.Fatal(err)
	}
	res := QuarantineResult{
		CapturedIdentityJSON: `{"inode":5}`,
		CapturedTokenJSON:    `{"size":10}`,
		QuarantinedAtNS:      200,
		PurgeNotBeforeNS:     300,
		ContentHash:          "sha256:aa",
		MetadataHash:         "sha256:bb",
	}
	if err := s.CompleteQuarantine(ctx, in, res, `{}`); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.CompleteQuarantine(ctx, in, res, `{}`); err != nil {
		t.Fatalf("idempotent complete returned %v", err)
	}

	events, _ := s.EventsForItem(ctx, "item_1")
	terminals := 0
	for _, ev := range events {
		if ev.Phase == domain.PhaseComplete {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want 1", terminals)
	}
	it, _ := s.GetItem(ctx, "item_1")
	if it.State != domain.StateQuarantined || it.PendingOpID != nil {
		t.Fatalf("item = %+v", it)
	}
	if it.PurgeNotBeforeNS == nil || *it.PurgeNotBeforeNS != 300 {
		t.Fatalf("purge_not_before = %v", it.PurgeNotBeforeNS)
	}
}

// TestProtectionBumpsRevision proves a new protection invalidates plans bound to
// the previous protection revision.
func TestProtectionBumpsRevision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedScope(t, s, "exports", domain.KindManagedDisposable)

	sc, _ := s.GetScope(ctx, "exports")
	before := sc.ProtectionRevision

	rev, err := s.InsertProtection(ctx, Protection{
		ID: "prot_1", ScopeID: "exports", RawRelative: []byte("keep/old.zip"),
		MatchKind: domain.MatchExact, Source: domain.ProtectUser,
		EvidenceJSON: `{}`, CreatedAtNS: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rev <= before {
		t.Fatalf("protection revision did not advance: %d -> %d", before, rev)
	}

	list, err := s.ListProtections(ctx, "exports", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || string(list[0].RawRelative) != "keep/old.zip" {
		t.Fatalf("protections = %+v", list)
	}

	if _, err := s.DeactivateProtectionByPath(ctx, "exports", []byte("keep/old.zip")); err != nil {
		t.Fatal(err)
	}
	sc2, _ := s.GetScope(ctx, "exports")
	if sc2.ProtectionRevision <= rev {
		t.Fatalf("deactivate did not advance revision: %d", sc2.ProtectionRevision)
	}
}
