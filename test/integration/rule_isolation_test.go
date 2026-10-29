package integration

import (
	"errors"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
)

// R04 is about one question: when something stops a batch, does the thing after
// it also stop? The run walks rules within a scope and scopes within a run, and
// a failure that is read as "carry on with the next rule" turns one bad rule
// into a scope-wide mess. These two tests pin the two levels.

// TestARuleThatStopsTheScopeLeavesTheNextRuleAlone: the first rule cannot start
// because its approvals contradict each other. The scope stops, and the second
// rule - which is perfectly healthy - does not get to run on the way out.
func TestARuleThatStopsTheScopeLeavesTheNextRuleAlone(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.log", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)

	// rule-1 carries two active approvals, the state an older build left behind.
	first := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(first, domain.CapAutoQuarantine, 5, 1<<20)
	legacyActiveGrant(t, e, first, domain.CapAutoQuarantine,
		domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"))

	// rule-2 is healthy and would match b.log.
	second := e.rule(sc.ID, "rule-2", []string{"*.log"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(second, domain.CapAutoQuarantine, 5, 1<<20)

	rep := e.runOnce()
	if rep.FailureCode != errs.CodeRecoveryRequired {
		t.Fatalf("the run ended with %q, want the ambiguity reported: %+v", rep.FailureCode, rep.Failures)
	}
	if rep.Quarantined != 0 {
		t.Fatalf("%d object(s) were taken while the scope was stopping", rep.Quarantined)
	}
	if !e.exists("exports", "b.log") {
		t.Fatal("the second rule ran after the first one stopped the scope")
	}
	if got := len(e.quarantinedItems(sc.ID)); got != 0 {
		t.Fatalf("%d object(s) reached the queue although the scope stopped before any rule ran", got)
	}
	// No intent was written for the healthy rule either: "stop" has to mean the
	// second rule did not get as far as planning.
	ctx := t.Context()
	pending, err := e.st.CountItemsByState(ctx, sc.ID, domain.StateQuarantined)
	if err != nil {
		t.Fatalf("count quarantined items: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d item(s) are quarantined; the stop did not hold", pending)
	}
}

// TestARunLevelFailureLeavesTheNextRuleAlone: this time the first rule starts and
// its terminal write fails after the file has already moved. That is a
// run-level stop - the state file is not accepting writes, so nothing else in
// this pass may start - and the second rule must not run behind it.
func TestARunLevelFailureLeavesTheNextRuleAlone(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.log", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)

	first := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(first, domain.CapAutoQuarantine, 5, 1<<20)
	second := e.rule(sc.ID, "rule-2", []string{"*.log"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(second, domain.CapAutoQuarantine, 5, 1<<20)

	execute.TestHookBeforeComplete = func(action string) error {
		if action == "quarantine" {
			return errors.New("sqlite: disk I/O error")
		}
		return nil
	}
	t.Cleanup(func() { execute.TestHookBeforeComplete = nil })

	rep, err := e.run.Run(t.Context())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.FailureCode != errs.CodeStateWriteFailed {
		t.Fatalf("the run ended with %q, want the write failure reported: %+v", rep.FailureCode, rep.Failures)
	}
	if rep.Control != domain.ControlStopRun {
		t.Fatalf("the run control is %q, want %q", rep.Control, domain.ControlStopRun)
	}
	if rep.Quarantined != 0 {
		// The move happened but the record did not, so nothing counts as
		// quarantined; a run that reported one here would be describing a state
		// the database does not have.
		t.Fatalf("%d object(s) were counted as quarantined although the record failed", rep.Quarantined)
	}
	if !e.exists("exports", "b.log") {
		t.Fatal("the second rule ran after the state file refused a write")
	}
	if e.exists("exports", "a.zip") {
		t.Fatal("the first object was not moved at all; the failure is supposed to happen after the move")
	}
	// The moved object is parked in q_intent with its pending marker, which is
	// what reconciliation needs; it is not silently reclassified.
	items := 0
	for _, state := range []string{domain.StateQIntent, domain.StateQuarantined} {
		n, cerr := e.st.CountItemsByState(t.Context(), sc.ID, state)
		if cerr != nil {
			t.Fatalf("count %s: %v", state, cerr)
		}
		items += int(n)
	}
	if items != 1 {
		t.Fatalf("%d item(s) exist for the object whose record failed, want exactly 1", items)
	}
	if got, cerr := e.st.CountItemsByState(t.Context(), sc.ID, domain.StateQIntent); cerr != nil || got != 1 {
		t.Fatalf("the failed object is not parked in q_intent: %d %v", got, cerr)
	}
}
