package debugrun

import (
	"context"
	"dustkeeper/internal/assess"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/review"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestGeneral29ModelReceivesObservationTimeWindow(t *testing.T) {
	opt, _ := fixture(t)
	opt.Advisory = true
	opt.Now = time.Now()
	opt.Service.Judge = testJudge(func(_ context.Context, c review.Facts) (review.Decision, error) {
		raw, _ := json.Marshal(c.Observation)
		var f map[string]any
		_ = json.Unmarshal(raw, &f)
		// The reviewers are told when the observation was made and how far the
		// observation reaches; they are not told that "nothing was observed" means
		// "nothing depends on it".
		if f["observed_at_ns"] != float64(opt.Now.UnixNano()) {
			t.Errorf("model lacks the observation clock: %s", raw)
		}
		if f["reference_coverage"] == nil || f["process_visibility"] == nil {
			t.Errorf("model was not told how far the observation reaches: %s", raw)
		}
		return review.Decision{}, nil
	})
	if _, err := Run(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
}

func TestGeneral26StaleSourceStopsSubsequentCandidates(t *testing.T) {
	opt, path := fixture(t)
	opt.Advisory = true
	opt.MaxCandidates = 3
	if err := os.Rename(path, filepath.Join(opt.Root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, name := range []string{"b.txt", "c.txt"} {
		p := filepath.Join(opt.Root, name)
		if err := os.WriteFile(p, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(opt.Root, "b.txt"), []byte("edit"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return review.Decision{}, nil
	})
	r, err := Run(context.Background(), opt)
	if err == nil || calls != 1 || r.Entries[2].Outcome != "not_reviewed" {
		t.Fatalf("source anomaly did not stop: calls=%d err=%v", calls, err)
	}
}

func TestGeneral21MissingJudgeReturnsControlledRefusal(t *testing.T) {
	opt, _ := fixture(t)
	opt.Advisory = true
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("missing judge crashed debug: %v", v)
		}
	}()
	_, err := Run(context.Background(), opt)
	if err == nil {
		t.Fatal("missing judge accepted")
	}
}

func TestGeneral19OldBackupCanReceiveAdvisoryButNotPermission(t *testing.T) {
	opt, path := fixture(t)
	oldPath := filepath.Join(opt.Root, "retired.backup")
	if err := os.Rename(path, oldPath); err != nil {
		t.Fatal(err)
	}
	opt.Advisory = true
	opt.OwnerConfirmedUnused = map[string]bool{"retired.backup": true}
	calls := 0
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		return review.Decision{Verdict: "allow", JEVModel: "j", LLMModel: "m"}, nil
	})
	r, err := Run(context.Background(), opt)
	if err != nil || calls != 1 || r.SimulatedCleanedFiles != 0 || r.Entries[0].Outcome != "advisory_only" {
		t.Fatalf("advisory blocked or promoted: calls=%d report=%+v err=%v", calls, r, err)
	}
}

func TestGeneral17NewReferenceDuringReviewInvalidatesSnapshot(t *testing.T) {
	opt, _ := fixture(t)
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		if err := os.Symlink("old-notes.txt", filepath.Join(opt.Root, "new-reference")); err != nil {
			t.Fatal(err)
		}
		return review.Decision{Verdict: "allow", JEVModel: "j", LLMModel: "m"}, nil
	})
	r, err := Run(context.Background(), opt)
	if err == nil || r.SimulatedCleanedFiles != 0 || r.Entries[0].Reason != "reference_observation_stale" {
		t.Fatalf("new reference ignored: simulated=%d err=%v rows=%+v", r.SimulatedCleanedFiles, err, r.Entries)
	}
}

func TestGeneral12DebugDoesNotChangeCallerReviewPolicy(t *testing.T) {
	opt, _ := fixture(t)
	opt.Advisory = true
	service := opt.Service
	service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) { return review.Decision{}, nil })
	_, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if service.MaxCandidates != 0 {
		t.Fatalf("debug mutated caller policy: %+v", service)
	}
}

func TestGeneral11OversizedCandidateDoesNotConsumeModelSlot(t *testing.T) {
	opt, _ := fixture(t)
	opt.Advisory = true
	opt.MaxReadBytes = 1024
	large := filepath.Join(opt.Root, "large-unused-document.txt")
	if err := os.WriteFile(large, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(large, old, old); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		return review.Decision{}, nil
	})
	r, err := Run(context.Background(), opt)
	if err != nil || calls != 1 || r.Entries[1].Outcome != "advisory_only" {
		t.Fatalf("oversized item starved affordable candidate: calls=%d rows=%+v err=%v", calls, r.Entries, err)
	}
}

func TestGeneral06ScanSnapshotMustStillMatch(t *testing.T) {
	opt, path := fixture(t)
	opt.Advisory = true
	scan, err := assess.Scan(context.Background(), opt.Root, assess.Options{Days: 30, MaxEntries: 50})
	if err != nil {
		t.Fatal(err)
	}
	f := scan.Files[0]
	if err := os.WriteFile(path, []byte("edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, f.Facts.ATime, f.Facts.MTime); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		return review.Decision{}, nil
	})
	root, err := fsops.OpenRoot(opt.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	row, err := inspect(context.Background(), root, f, Entry{File: f}, opt, domain.NewBudget(120, 1024, 0, 0, 0))
	if err == nil || calls != 0 || row.Reason != "source_changed_since_scan" {
		t.Fatalf("stale scan accepted: calls=%d row=%+v err=%v", calls, row, err)
	}
}

func TestGeneral01PartialScanDoesNotExposeUnselectedFiles(t *testing.T) {
	opt, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(opt.Root, "a-private.txt"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	opt.Execute = false
	opt.MaxEntries = 1
	opt.IncludePaths = map[string]bool{"old-notes.txt": true}
	r, err := Run(context.Background(), opt)
	if err == nil {
		t.Fatal("partial scan unexpectedly succeeded")
	}
	for _, file := range r.Scan.Files {
		if file.Facts.Path != "old-notes.txt" {
			t.Fatalf("unselected name escaped include boundary: %q", file.Facts.Path)
		}
	}
}

func TestGeneral03OwnerIntentIsConsistentInReport(t *testing.T) {
	opt, _ := fixture(t)
	opt.Advisory = true
	opt.OwnerConfirmedUnused = map[string]bool{"old-notes.txt": true}
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		return review.Decision{Reason: "insufficient"}, nil
	})
	r, err := Run(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	missing := r.Entries[0].File.Assessment.Missing
	if slices.Contains(missing, "disposal_intent") || slices.Contains(missing, "document_disposal_basis") {
		t.Fatalf("confirmed owner intent still reported missing: %v", missing)
	}
	if r.SimulatedCleanedFiles != 0 {
		t.Fatal("owner intent granted cleanup")
	}
}

func TestGeneral04CancellationDuringJudgeStopsRecheck(t *testing.T) {
	opt, _ := fixture(t)
	opt.Advisory = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		cancel()
		return review.Decision{Verdict: "allow", JEVModel: "test", LLMModel: "test"}, nil
	})
	r, err := Run(ctx, opt)
	if !errors.Is(err, context.Canceled) || r.SourceReadBytes != 4 {
		t.Fatalf("cancelled review completed/re-read source: read=%d err=%v", r.SourceReadBytes, err)
	}
}

func TestGeneral05SensitiveRootIsNotSentToModels(t *testing.T) {
	opt, _ := fixture(t)
	privateRoot := filepath.Join(filepath.Dir(opt.Root), ".ssh")
	if err := os.Rename(opt.Root, privateRoot); err != nil {
		t.Fatal(err)
	}
	opt.Root = privateRoot
	opt.Advisory = true
	calls := 0
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		return review.Decision{}, nil
	})
	_, err := Run(context.Background(), opt)
	if err == nil || calls != 0 {
		t.Fatalf("sensitive root reviewed: calls=%d err=%v", calls, err)
	}
}

func TestGeneral02StoppedRunListsUntouchedCandidates(t *testing.T) {
	opt, _ := fixture(t)
	opt.MaxCandidates = 2
	path := filepath.Join(opt.Root, "second.txt")
	if err := os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		return review.Decision{}, errors.New("synthetic provider failure")
	})
	r, err := Run(context.Background(), opt)
	if err == nil || calls != 1 || len(r.Entries) != 2 || r.Entries[1].Outcome != "not_reviewed" {
		t.Fatalf("interrupted run lost untouched candidate: entries=%+v calls=%d err=%v", r.Entries, calls, err)
	}
}
