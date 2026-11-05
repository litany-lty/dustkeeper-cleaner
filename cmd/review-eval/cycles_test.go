package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type failSummaryWriter struct{ bytes.Buffer }

func (w *failSummaryWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"type":"summary"`)) {
		return 0, errors.New("sink unavailable")
	}
	return w.Buffer.Write(p)
}
func TestCycle22SummaryWriteFailureCannotReportSuccess(t *testing.T) {
	j := &fakeJudge{}
	n := 0
	deps := testDependencies(j, &n, threeKeepSamples)
	var out failSummaryWriter
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"--config", "test", "--max-cases", "1", "--execute"}, &out, &stderr, deps); code == 0 {
		t.Fatal("summary write failed but CLI succeeded")
	}
}
