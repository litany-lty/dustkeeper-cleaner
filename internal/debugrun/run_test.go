package debugrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/review"
)

type testJudge func(context.Context, review.Facts) (review.Decision, error)

func (f testJudge) Review(c context.Context, v review.Facts) (review.Decision, error) {
	return f(c, v)
}

func fixture(t *testing.T) (Options, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "old-notes.txt")
	payload := []byte("data")
	if err := os.WriteFile(file, payload, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	return Options{Root: root, ScopeID: "debug", Action: "purge", Execute: true, MaxEntries: 50, MaxCandidates: 1, Days: 30, MaxReadBytes: 1 << 20, Service: &review.Service{}}, file
}

func TestChangedFileDuringDebugIsNeverSimulatedAsCleaned(t *testing.T) {
	opt, file := fixture(t)
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		if err := os.WriteFile(file, []byte("evil"), 0600); err != nil {
			t.Fatal(err)
		}
		return review.Decision{Verdict: "allow", JEVModel: "test", LLMModel: "test"}, nil
	})
	r, err := Run(context.Background(), opt)
	if err == nil || r.SimulatedCleanedFiles != 0 || r.ActualDeletedFiles != 0 || r.Entries[0].SourceUnchanged {
		t.Fatalf("changed source was accepted: %+v %v", r, err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "evil" {
		t.Fatal("debug changed the bytes written by the concurrent writer")
	}
}

func TestKnownReferenceIsRetainedWithoutCallingModel(t *testing.T) {
	opt, _ := fixture(t)
	calls := 0
	opt.References = map[string]bool{"old-notes.txt": true}
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		calls++
		return review.Decision{Verdict: "allow", JEVModel: "test", LLMModel: "test"}, nil
	})
	r, err := Run(context.Background(), opt)
	if err != nil || calls != 0 || r.SimulatedCleanedFiles != 0 || r.Entries[0].Outcome != "keep" {
		t.Fatalf("referenced file was reviewed or selected: %+v %v", r, err)
	}
}

func TestAdvisoryCannotPromoteModelApprovalIntoSimulatedCleanup(t *testing.T) {
	opt, _ := fixture(t)
	extra := filepath.Join(opt.Root, "unselected.txt")
	if err := os.WriteFile(extra, []byte("unselected private data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(extra, old, old); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opt.Advisory = true
	opt.OwnerConfirmedUnused = map[string]bool{"old-notes.txt": true}
	opt.IncludePaths = map[string]bool{"old-notes.txt": true}
	opt.Service.Judge = testJudge(func(_ context.Context, c review.Facts) (review.Decision, error) {
		calls++
		if c.FactsSource != "measured_by_program" {
			t.Fatalf("facts were not labelled as measured: %+v", c)
		}
		return review.Decision{Verdict: "allow", JEVModel: "test", LLMModel: "test"}, nil
	})
	r, err := Run(context.Background(), opt)
	if err != nil || calls != 1 || len(r.Entries) != 1 || len(r.Scan.Files) != 1 || r.SimulatedCleanedFiles != 0 || r.Entries[0].Outcome != "advisory_only" {
		t.Fatalf("advisory failed or gained authority: %+v %v calls=%d", r, err, calls)
	}
}

func TestIncompleteSelectedScanReportsLimitBeforeMissingFile(t *testing.T) {
	opt, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(opt.Root, "a.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	opt.MaxEntries = 1
	opt.IncludePaths = map[string]bool{"old-notes.txt": true}
	called := false
	opt.Service.Judge = testJudge(func(context.Context, review.Facts) (review.Decision, error) {
		called = true
		return review.Decision{}, nil
	})
	r, err := Run(context.Background(), opt)
	if err == nil || r.StoppedReason != "incomplete_scan" || called {
		t.Fatalf("partial scan became a missing-file claim or model call: %+v %v", r, err)
	}
}
