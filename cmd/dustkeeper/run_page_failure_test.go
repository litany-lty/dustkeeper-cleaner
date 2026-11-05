package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// TestARunWhosePageCannotBeWrittenIsNotReportedAsSuccess is R08, at the entry
// point a scheduler actually uses.
//
// The result page is how the product keeps its promise that "there is a page at a
// fixed place". When the page cannot be written, the user is pointed at the
// previous run instead, so the command may not answer with exit 0 and code OK:
// that is the answer a scheduler reads as "everything is fine, nobody needs to
// look". The check on the envelope used to compare against the empty string, and
// a normal run's code is `OK` rather than empty, so the branch was dead.
func TestARunWhosePageCannotBeWrittenIsNotReportedAsSuccess(t *testing.T) {
	clk := domain.NewFakeClock(time.Now())
	base, tree := scopeWithRule(t, clk)
	// No candidates: this test is about the page, not about the review, and a run
	// that stops for a model failure would be reported as failed for that reason
	// instead.
	if err := os.Remove(filepath.Join(tree, "old.zip")); err != nil {
		t.Fatalf("remove the candidate: %v", err)
	}
	// A regular file stands where the report directory would have to be, so the
	// write fails for a reason nothing retries its way out of.
	blocker := filepath.Join(tree, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write the blocker: %v", err)
	}
	writePolicyWithResultDir(t, base, filepath.Join(blocker, "reports"))

	ran := cliOn(t, clk, "run", "--json")
	if ran.code != 9 {
		t.Fatalf("exit = %d, want 9 for %s\n%s", ran.code, errs.CodePartial, ran.stdout)
	}
	if got := ran.str(t, "code"); got != string(errs.CodePartial) {
		t.Fatalf("code = %q, want %q", got, errs.CodePartial)
	}
	if got := ran.str(t, "status"); got != string(errs.StatusPartial) {
		t.Fatalf("status = %q, want %q", got, errs.StatusPartial)
	}
	msg := ran.str(t, "message")
	if !strings.Contains(msg, "result page could not be written") {
		t.Fatalf("the envelope does not say what went wrong: %q", msg)
	}
}

// The ordinary case has to keep working: a page that is written is reported as a
// finished run, not as a partial one. A gate that failed everything would pass
// the test above and break the product.
func TestARunWhosePageIsWrittenIsReportedAsSuccess(t *testing.T) {
	clk := domain.NewFakeClock(time.Now())
	base, tree := scopeWithRule(t, clk)
	if err := os.Remove(filepath.Join(tree, "old.zip")); err != nil {
		t.Fatalf("remove the candidate: %v", err)
	}
	// The default result directory: nothing is standing in its way.
	writePolicyWithResultDir(t, base, "")

	ran := mustCLIOn(t, clk, "run", "--json")
	if got := ran.str(t, "code"); got != string(errs.CodeOK) {
		t.Fatalf("code = %q, want %q\n%s", got, errs.CodeOK, ran.stdout)
	}
	page := filepath.Join(base, "state", "reports", "latest.md")
	if _, err := os.Stat(page); err != nil {
		t.Fatalf("the page the run reported is not there: %v", err)
	}
}

// writePolicyWithResultDir stores the confirmed policy with the given result
// directory, leaving everything else as the fixture confirmed it.
func writePolicyWithResultDir(t *testing.T, base, dir string) {
	t.Helper()
	st, err := store.Open(filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	pol, err := cleanup.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	pol.ResultDir = dir
	if pol.ConfirmedAtNS == 0 {
		pol.ConfirmedAtNS = time.Now().UnixNano()
	}
	if err := cleanup.Save(context.Background(), st, pol); err != nil {
		t.Fatalf("save policy: %v", err)
	}
}
