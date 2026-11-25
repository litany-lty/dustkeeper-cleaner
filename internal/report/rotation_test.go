package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

func TestR10WritePrunesOldSnapshotsAndKeepsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "keep-my-note.txt")
	if err := os.WriteFile(foreign, []byte("operator note"), 0o600); err != nil {
		t.Fatalf("write foreign: %v", err)
	}

	baseTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 35; i++ {
		runID := fmt.Sprintf("run_%03d", i)
		page := Page{
			RunID:         runID,
			GeneratedAtNS: baseTime.Add(time.Duration(i) * time.Hour).UnixNano(),
			WindowStartNS: baseTime.UnixNano(),
			WindowEndNS:   baseTime.Add(time.Duration(i) * time.Hour).UnixNano(),
		}
		paths, err := Write(dir, page)
		if err != nil {
			t.Fatalf("write %s: %v", runID, err)
		}
		mt := baseTime.Add(time.Duration(i) * time.Hour)
		_ = os.Chtimes(paths.MarkdownSnapshot, mt, mt)
		_ = os.Chtimes(paths.JSONSnapshot, mt, mt)
	}

	// Verify foreign file and latest.* remain untouched.
	for _, keep := range []string{foreign, filepath.Join(dir, LatestMarkdown), filepath.Join(dir, LatestJSON)} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("expected %s to survive rotation: %v", keep, err)
		}
	}

	// Count remaining result-*.md snapshots: must be capped at maxReportSnapshotRuns (30).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	mdCount, jsonCount := 0, 0
	hasNewest, hasOldest := false, false
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "result-") && strings.HasSuffix(name, ".md") {
			mdCount++
			if name == "result-run_035.md" {
				hasNewest = true
			}
			if name == "result-run_001.md" {
				hasOldest = true
			}
		}
		if strings.HasPrefix(name, "result-") && strings.HasSuffix(name, ".json") {
			jsonCount++
		}
	}
	if mdCount != maxReportSnapshotRuns || jsonCount != maxReportSnapshotRuns {
		t.Fatalf("expected %d md and json snapshots, got md=%d json=%d", maxReportSnapshotRuns, mdCount, jsonCount)
	}
	if !hasNewest || hasOldest {
		t.Fatalf("rotation kept wrong runs: hasNewest=%v hasOldest=%v", hasNewest, hasOldest)
	}
}

func TestR10EventsInWindowAndPruneStaleObservations(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Insert out-of-window, in-window, and clock-jumped events to prove
	// EventsInWindow filters in SQL while preserving sequence order.
	times := []int64{100, 500, 300, 900, 400}
	for i, ts := range times {
		if err := st.AppendEvent(ctx, store.Event{
			EventID:      fmt.Sprintf("ev_%d", i),
			Action:       "keep",
			Phase:        domain.PhaseInfo,
			OccurredAtNS: ts,
			ContractJSON: "{}",
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}
	got, err := eventsInWindow(ctx, st, 250, 600)
	if err != nil {
		t.Fatalf("eventsInWindow: %v", err)
	}
	wantIDs := []string{"ev_1", "ev_2", "ev_4"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].EventID != want {
			t.Fatalf("event[%d] = %q, want %q", i, got[i].EventID, want)
		}
	}

	// Verify PruneStaleObservations deletes only old stale rows while preserving
	// active rows and recently-stale rows.
	scID := domain.ScopeID("exports")
	if err := st.InsertScope(ctx, store.Scope{
		ID:                 scID,
		RawRoot:            []byte("/tmp/exports"),
		RootIdentityJSON:   "{}",
		InstanceID:         "inst_1",
		Kind:               domain.KindManagedDisposable,
		Generation:         1,
		ProtectionRevision: 1,
		Status:             domain.ScopeActive,
		ScanEpoch:          2,
		CreatedAtNS:        100,
	}); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	oldTS := int64(1_000)
	recentTS := int64(5_000)
	if err := st.UpsertObservations(ctx, []store.Observation{
		{ScopeID: scID, RawRelative: []byte("old-stale.zip"), IdentityJSON: "{}", TokenJSON: "{}", ParentJSON: "{}", FirstSeenNS: oldTS, LastSeenNS: oldTS, ScanEpoch: 1, Status: domain.ObsStale},
		{ScopeID: scID, RawRelative: []byte("recent-stale.zip"), IdentityJSON: "{}", TokenJSON: "{}", ParentJSON: "{}", FirstSeenNS: recentTS, LastSeenNS: recentTS, ScanEpoch: 1, Status: domain.ObsStale},
		{ScopeID: scID, RawRelative: []byte("old-seen.zip"), IdentityJSON: "{}", TokenJSON: "{}", ParentJSON: "{}", FirstSeenNS: oldTS, LastSeenNS: oldTS, ScanEpoch: 2, Status: domain.ObsSeen},
	}); err != nil {
		t.Fatalf("upsert obs: %v", err)
	}
	pruned, err := st.PruneStaleObservations(ctx, scID, 3_000)
	if err != nil {
		t.Fatalf("prune stale: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 stale row pruned, got %d", pruned)
	}
	if _, err := st.GetObservation(ctx, scID, []byte("old-stale.zip")); err == nil {
		t.Fatal("old-stale.zip should have been pruned")
	}
	if _, err := st.GetObservation(ctx, scID, []byte("recent-stale.zip")); err != nil {
		t.Fatalf("recent-stale.zip should be kept: %v", err)
	}
	if _, err := st.GetObservation(ctx, scID, []byte("old-seen.zip")); err != nil {
		t.Fatalf("old-seen.zip should be kept: %v", err)
	}
}
