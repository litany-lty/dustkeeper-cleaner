package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// These tests are about the three results the product is defined by, exercised
// through the real executor: a file that is left exactly where it is, a file
// that is moved into the observation area, and a file that is destroyed.
//
// They are written as statements a user could check:
//
//   - a "keep" moves nothing and deletes nothing, and leaves a record saying why;
//   - an agreement between the reviewers is never sufficient on its own for a
//     permanent deletion;
//   - a permanent deletion happens only when the local authority holds as well;
//   - a policy change never shortens a window that was already promised.

// keepReview answers every request with "leave it" and a stated reason.
func keepReview(reason string) execute.ReviewFunc {
	return func(context.Context, review.Request) (review.Outcome, error) {
		return review.Outcome{
			Disposition: domain.DispositionKeep,
			Reason:      reason,
			Decision: review.Decision{
				Verdict:  domain.VerdictKeep,
				Reason:   reason,
				JEVModel: "jev-fixture", LLMModel: "minimax-fixture",
			},
		}, nil
	}
}

// outageReview is a review that could not be completed: the models never
// answered. Nothing about the file was decided, and that has to be visible.
func outageReview(context.Context, review.Request) (review.Outcome, error) {
	return review.Outcome{
		Disposition: domain.DispositionKeep,
		Reason:      "review_unavailable",
		Decision: review.Decision{
			Verdict:    domain.VerdictKeep,
			Reason:     "review_unavailable",
			ModelError: "the model endpoint refused the connection",
			JEVModel:   "jev-fixture",
		},
	}, nil
}

// agreeWithTheQuestion is a pair of reviewers that always agree with whatever
// they were asked.
//
// It is the strongest form of "the models said yes", and it is deliberately used
// to prove that agreement alone is not enough: with this reviewer in place, an
// irreversible action can only happen when the executor itself decided the item
// qualified for one.
func agreeWithTheQuestion(_ context.Context, r review.Request) (review.Outcome, error) {
	disposition := domain.DispositionQuarantine
	if r.Action == domain.ActionPurge || r.Action == domain.DispositionDelete {
		disposition = domain.DispositionDelete
	}
	return review.Outcome{
		Approval:    &fakeApproval{itemID: r.ItemID, action: r.Action},
		Disposition: disposition,
		Reason:      "integration fixture: both reviewers agreed",
		Decision: review.Decision{
			Verdict:  domain.VerdictAllow,
			Reason:   "integration fixture",
			JEVModel: "jev-fixture", LLMModel: "minimax-fixture",
		},
	}, nil
}

// planForScheduledRule builds a plan whose items carry an automatic purge grant.
//
// A direct deletion needs standing authority, and that authority only exists for
// a plan a scheduled rule produced: a manual plan is something a human is
// watching, and the automatic branches deliberately do not extend to it.
func (e *env) planForScheduledRule(sc store.Scope, r store.Rule, grant store.Grant, maxFiles int) plan.RulePlanResult {
	e.t.Helper()
	sctx := e.scopeCtx(sc.ID)
	parsed := mustParse(e.t, r)
	res, err := e.plan.CreateRulePlan(context.Background(), plan.RulePlanRequest{
		Scope:      sc,
		Rule:       parsed,
		RuleRef:    domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:       domain.PlanScheduledRule,
		Guard:      e.guard(sctx),
		MaxFiles:   maxFiles,
		MaxBytes:   1 << 30,
		Config:     e.cfg,
		NowNS:      e.clock.Wall().UnixNano(),
		PurgeGrant: &grant,
		AncestorsOf: func(dir string) ([]domain.FileIdentity, error) {
			return execute.AncestorIdentities(sctx, []byte(dir))
		},
	})
	if err != nil {
		e.t.Fatalf("create scheduled plan: %v", err)
	}
	return res
}

// TestKeepMovesNothingAndSaysWhy is the first of the three results.
//
// A keep used to leave no trace at all: the file was still there, the counts were
// zero, and nothing said whether that was because no rule matched or because the
// reviewers had refused. Both outcomes now have to be distinguishable, and a keep
// must not hold the scope for the next item either.
func TestKeepMovesNothingAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "keep-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	if len(res.Items) != 2 {
		t.Fatalf("expected two candidates, got %d", len(res.Items))
	}
	e.exec.Review = keepReview("model_says_keep")

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 0 || out.Skipped != 2 || out.Failed != 0 {
		t.Fatalf("keep was not reported as a skip: %+v", out)
	}
	if out.Control != domain.ControlContinue {
		t.Fatalf("a keep stopped the batch: %q", out.Control)
	}
	for _, rel := range []string{"a.zip", "b.zip"} {
		if !e.exists("exports", rel) {
			t.Fatalf("%s was moved or removed by a keep", rel)
		}
	}

	for _, item := range res.Items {
		got, err := e.st.GetItem(context.Background(), item.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		// A keep is an answer about this run, not a new lifecycle state: the item
		// stays planned with no pending marker, so the next run is not blocked by
		// it and the plan stays applicable.
		if got.State != domain.StatePlanned || got.PendingOpID != nil {
			t.Fatalf("keep changed the lifecycle: state=%s pending=%v", got.State, got.PendingOpID)
		}
		events, err := e.st.EventsForItem(context.Background(), item.ID)
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		recorded := false
		for _, ev := range events {
			if ev.Action != "keep" || ev.Phase != domain.PhaseInfo || ev.ResultJSON == nil {
				continue
			}
			var payload struct {
				Disposition string `json:"disposition"`
				Reason      string `json:"reason"`
				ReasonText  string `json:"reason_text"`
			}
			if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
				t.Fatalf("keep payload: %v", err)
			}
			if payload.Disposition != domain.DispositionKeep || payload.Reason != "model_says_keep" ||
				payload.ReasonText == "" || payload.ReasonText == payload.Reason {
				t.Fatalf("keep payload does not explain itself: %+v", payload)
			}
			recorded = true
		}
		if !recorded {
			t.Fatalf("item %s was kept with no record of why", item.ID)
		}
	}
}

// TestModelOutageIsNotACleanSweep is the fourth keep reason, and the one that must
// never be reported as a finished run.
//
// "Nothing qualified" and "the model service is down" produce the same counts and
// the same disk. Telling a user "0 deleted" for the second one describes an outage
// as a clean sweep, so the run stops and says which it was.
func TestModelOutageIsNotACleanSweep(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "outage-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	e.exec.Review = outageReview

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 0 {
		t.Fatalf("an outage completed work: %+v", out)
	}
	if out.Code != string(errs.CodeModelUnavailable) {
		t.Fatalf("code = %q, want %q", out.Code, errs.CodeModelUnavailable)
	}
	if out.Control != domain.ControlStopScope {
		t.Fatalf("control = %q, want %q", out.Control, domain.ControlStopScope)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file was touched while the reviewers were unavailable")
	}
	item, err := e.st.GetItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != domain.StatePlanned || item.PendingOpID != nil {
		t.Fatalf("an outage changed the lifecycle: state=%s pending=%v", item.State, item.PendingOpID)
	}
}

// TestAgreementAloneCannotDelete is the safety property the whole rewrite rests
// on: the reviewers can authorise, but they cannot grant authority.
//
// The same reviewer is installed in both halves. In the first the user has not
// confirmed a cleanup policy, so the executor asks the reversible question and the
// file ends up in the observation area. In the second the category, the scope, the
// scheduled plan and the grant are all in place, so the executor asks the
// irreversible question and the file is removed.
func TestAgreementAloneCannotDelete(t *testing.T) {
	t.Run("without a confirmed policy the file is only observed", func(t *testing.T) {
		e, sc, res := scheduledDeleteSetup(t)
		// No policy row: nothing may be deleted outright, whatever the models say.
		e.exec.Review = agreeWithTheQuestion

		out, err := e.apply(sc, res.Plan, res.Plan.Digest)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if out.Completed != 1 {
			t.Fatalf("the object was not observed: %+v", out)
		}
		if e.exists("scratch", "discard.zip") {
			t.Fatal("a file was deleted on the models' agreement alone")
		}
		item, err := e.st.GetItem(context.Background(), res.Items[0].ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if item.State != domain.StateQuarantined {
			t.Fatalf("state = %s, want the observation area", item.State)
		}
		if e.hasAction(t, item.ID, domain.ActionDelete) {
			t.Fatal("a delete operation was recorded for a file that was only moved")
		}
	})

	t.Run("with the confirmed category, scope and grant the file is removed", func(t *testing.T) {
		e, sc, res := scheduledDeleteSetup(t)
		e.savePolicy(directDeletePolicy(sc.ID))
		e.exec.Review = agreeWithTheQuestion

		out, err := e.apply(sc, res.Plan, res.Plan.Digest)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if out.Completed != 1 {
			t.Fatalf("the deletion did not complete: %+v", out)
		}
		if e.exists("scratch", "discard.zip") {
			t.Fatal("the confirmed deletion left the file in place")
		}
		item, err := e.st.GetItem(context.Background(), res.Items[0].ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if item.State != domain.StatePurged {
			t.Fatalf("state = %s, want %s", item.State, domain.StatePurged)
		}
		if !e.hasAction(t, item.ID, domain.ActionDelete) {
			t.Fatal("the permanent deletion was not recorded as a delete operation")
		}
		// The complete event has to say what was removed, not merely that
		// something was: a deletion with no hash is unanswerable afterwards.
		events, err := e.st.EventsForItem(context.Background(), item.ID)
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		recorded := false
		for _, ev := range events {
			if ev.Action != domain.ActionDelete || ev.Phase != domain.PhaseComplete || ev.ResultJSON == nil {
				continue
			}
			var payload struct {
				PathRemoved  bool   `json:"path_removed"`
				ContentHash  string `json:"content_hash"`
				SizeBytes    int64  `json:"size_bytes"`
				DispositionR string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
				t.Fatalf("delete payload: %v", err)
			}
			if !payload.PathRemoved || payload.ContentHash == "" || payload.SizeBytes <= 0 ||
				payload.DispositionR == "" {
				t.Fatalf("the deletion record does not say what was removed: %+v", payload)
			}
			recorded = true
		}
		if !recorded {
			t.Fatal("the deletion has no completion record")
		}
	})
}

// TestPolicyChangeNeverShortensAnExistingObjectsWindow is the promise made to the
// six objects that were already isolated under a thirty day rule.
//
// The policy is a forward-looking setting. Re-running `cleanup setup` may change
// what happens to files isolated from now on; it may not move a date that a user
// has already been told.
func TestPolicyChangeNeverShortensAnExistingObjectsWindow(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	long := e.rule(sc.ID, "long-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, long, 10)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	before, err := e.st.GetItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	const thirtyDays = int64(30 * 24 * 60 * 60)
	if before.BoundRetentionSeconds != thirtyDays {
		t.Fatalf("a thirty day rule bound %d seconds", before.BoundRetentionSeconds)
	}

	// The user now confirms the product default of fourteen days.
	e.confirmPolicy(cleanup.DefaultObservationDays, sc.ID)

	after, err := e.st.GetItem(context.Background(), before.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if after.BoundRetentionSeconds != thirtyDays || after.PurgeNotBeforeNS == nil ||
		*after.PurgeNotBeforeNS != *before.PurgeNotBeforeNS {
		t.Fatalf("the confirmation rewrote an existing window: %d seconds (was %d)",
			after.BoundRetentionSeconds, before.BoundRetentionSeconds)
	}

	// A file isolated after the confirmation under a seven day rule gets the
	// fourteen day default: the policy raises the floor, it does not lower it.
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)
	short := e.rule(sc.ID, "short-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	res2 := e.planFor(sc, short, 10)
	if len(res2.Items) != 1 {
		t.Fatalf("expected one new candidate, got %d", len(res2.Items))
	}
	if _, err := e.apply(sc, res2.Plan, res2.Plan.Digest); err != nil {
		t.Fatalf("apply second: %v", err)
	}
	fresh, err := e.st.GetItem(context.Background(), res2.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	const fourteenDays = int64(cleanup.DefaultObservationDays * 24 * 60 * 60)
	if fresh.BoundRetentionSeconds != fourteenDays {
		t.Fatalf("a seven day rule under the fourteen day default bound %d seconds",
			fresh.BoundRetentionSeconds)
	}
}

// scheduledDeleteSetup is one candidate in a scope whose plan carries the
// automatic purge grant a direct deletion needs.
func scheduledDeleteSetup(t *testing.T) (*env, store.Scope, plan.RulePlanResult) {
	t.Helper()
	e := newEnv(t)
	sc := e.addScope("scratch", domain.KindManagedDisposable)
	e.writeFile("scratch", "discard.zip", "discard-payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "delete-rule", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	g := e.grant(r, domain.CapAutoPurge, 10, 1<<30)
	res := e.planForScheduledRule(sc, r, g, 10)
	if len(res.Items) != 1 {
		t.Fatalf("expected one candidate, got %d", len(res.Items))
	}
	return e, sc, res
}

// directDeletePolicy is the full confirmation a permanent deletion stands on.
func directDeletePolicy(scopeID domain.ScopeID) cleanup.Policy {
	pol := cleanup.Default()
	pol.Scopes = []string{string(scopeID)}
	pol.DirectDeleteCategories = []string{assess.CatArchive}
	pol.Notify = cleanup.NotifyNone
	return pol
}

// hasAction reports whether the item carries an event for an action.
func (e *env) hasAction(t *testing.T, id domain.ItemID, action string) bool {
	t.Helper()
	events, err := e.st.EventsForItem(context.Background(), id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.Action == action {
			return true
		}
	}
	return false
}
