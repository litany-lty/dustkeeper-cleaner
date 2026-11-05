package main

import (
	"bytes"
	"context"
	"dustkeeper/internal/review"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneral24PlanDoesNotClaimUnreadCredentials(t *testing.T) {
	calls := 0
	deps := testDependencies(&fakeJudge{}, &calls, threeKeepSamples)
	deps.newJudge = func(*review.Settings) (review.Judge, error) { return nil, errors.New("not read") }
	var out, errout bytes.Buffer
	code := run(context.Background(), []string{"--config", "synthetic", "--max-cases", "1", "--execute"}, &out, &errout, deps)
	if code != 2 || strings.Contains(out.String(), `"credentials_read":true`) {
		t.Fatalf("future credential read reported as done: code=%d %s", code, out.String())
	}
}

func TestGeneral23AbstentionIsNotReportedAsDefinitivePass(t *testing.T) {
	judge := &fakeJudge{decisions: []review.Decision{{Verdict: "insufficient", Reason: "insufficient", JEVModel: "j"}}}
	calls := 0
	deps := testDependencies(judge, &calls, threeKeepSamples)
	var out, errout bytes.Buffer
	code := run(context.Background(), []string{"--config", "synthetic", "--max-cases", "1", "--execute"}, &out, &errout, deps)
	if code == 0 || !strings.Contains(out.String(), `"abstentions":1`) || !strings.Contains(out.String(), `"judgment":"insufficient"`) {
		t.Fatalf("unknown counted as acceptance: code=%d %s", code, out.String())
	}
}

func TestGeneral22EvaluationInputIsBoundedBeforeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "too-large.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte(" "), 300<<10), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := productionDependencies().loadSamples(path)
	if err == nil || len(data) != 0 {
		t.Fatalf("unbounded sample input read: bytes=%d err=%v", len(data), err)
	}
}
