package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

func TestSafetyReviewQuarantineWithoutApprovalMovesNothing(t *testing.T) {
	e, sc, res := safetyReviewPlan(t)
	e.exec.Review = nil

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireReviewFailure(t, out, errs.CodeReviewRequired)
	if !e.exists("exports", "candidate.zip") {
		t.Fatal("candidate moved even though no review approval was available")
	}
	it, err := e.st.GetItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if it.State != domain.StatePlanned || it.PendingOpID != nil {
		t.Fatalf("review refusal changed item state: state=%s pending=%v", it.State, it.PendingOpID)
	}
}

func TestSafetyReviewPurgeWithoutOrWithRejectedApprovalDeletesNothing(t *testing.T) {
	tests := []struct {
		name   string
		review execute.ReviewFunc
	}{
		{name: "missing", review: nil},
		{name: "rejected", review: legacyReview(rejectingReview)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, sc, it, p, objectPath := safetyReviewDueObject(t)
			e.exec.Review = tc.review
			sctx := e.scopeCtx(sc.ID)
			e.exec.Guard = e.guard(sctx)

			out, err := e.exec.ApplyPurgePlan(context.Background(), sctx, p.ID, p.Digest,
				domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
			if err != nil {
				t.Fatalf("apply purge: %v", err)
			}
			requireReviewFailure(t, out, errs.CodeReviewRequired)
			if !pathExists(objectPath) {
				t.Fatal("vault object was deleted without an accepted review")
			}
			got, err := e.st.GetItem(context.Background(), it.ID)
			if err != nil {
				t.Fatalf("get item: %v", err)
			}
			if got.State != domain.StateQuarantined || got.PendingOpID != nil {
				t.Fatalf("review refusal changed purge state: state=%s pending=%v", got.State, got.PendingOpID)
			}

			if _, err := e.exec.Restore(context.Background(), sctx, it.ID, execute.RestoreOptions{}); err != nil {
				t.Fatalf("review refusal must not block restore: %v", err)
			}
			if !e.exists("exports", "candidate.zip") {
				t.Fatal("restore did not return the retained candidate")
			}
		})
	}
}

func TestSafetyReviewApprovalDoesNotReplaceExistingAuthority(t *testing.T) {
	t.Run("quarantine plan digest", func(t *testing.T) {
		e, sc, res := safetyReviewPlan(t)
		_, err := e.apply(sc, res.Plan, domain.Digest("sha256:not-the-approved-plan"))
		requireCode(t, err, errs.CodeDigestMismatch)
		if !e.exists("exports", "candidate.zip") {
			t.Fatal("fake review bypassed the quarantine plan digest")
		}
	})

	t.Run("purge plan authority", func(t *testing.T) {
		e, sc, it, _, objectPath := safetyReviewDueObject(t)
		sctx := e.scopeCtx(sc.ID)
		e.exec.Guard = e.guard(sctx)
		_, err := e.exec.Purge(context.Background(), sctx, it, execute.PurgeOptions{
			Budget: domain.NewBudget(300, 1<<30, 10, 1<<30, 0),
			Trust:  trustOK,
		})
		requireCode(t, err, errs.CodeGrantRequired)
		if !pathExists(objectPath) {
			t.Fatal("fake review bypassed the purge plan requirement")
		}
	})
}

func TestSafetyReviewApprovalAuditIsStoredWithTheIntent(t *testing.T) {
	e, sc, res := safetyReviewPlan(t)
	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("expected one quarantine, got %+v", out)
	}
	events, err := e.st.EventsForItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, ev := range events {
		if ev.Phase != domain.PhaseIntent || ev.Action != "quarantine" {
			continue
		}
		if ev.ResultJSON == nil || !json.Valid([]byte(*ev.ResultJSON)) || *ev.ResultJSON != `{"source":"integration-lifecycle-fixture"}` {
			t.Fatalf("intent review audit = %v", ev.ResultJSON)
		}
		return
	}
	t.Fatal("quarantine intent event was not stored")
}

// TestSafetyReviewServiceReachesQuarantineOnAgreement runs the real Service over
// the facts the executor measured. There is no per-file evidence file any more:
// the agreement itself, plus the executor's own local checks, is what the move
// stands on.
func TestSafetyReviewServiceReachesQuarantineOnAgreement(t *testing.T) {
	e, sc, res := safetyReviewPlan(t)
	scopeRoot, err := filepath.EvalSymlinks(string(sc.RawRoot))
	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	contentHash := string(hashFile(t, filepath.Join(scopeRoot, "candidate.zip")))

	svc := &review.Service{Judge: fixedAllowJudge{}, Now: e.clock.Wall, MaxCandidates: 10}
	e.exec.Review = serviceReview(svc, scopeRoot)

	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("quarantine through service: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("dual agreement did not reach quarantine: %+v", out)
	}
	events, err := e.st.EventsForItem(context.Background(), res.Items[0].ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	foundAudit := false
	for _, ev := range events {
		if ev.Phase != domain.PhaseIntent || ev.Action != "quarantine" || ev.ResultJSON == nil {
			continue
		}
		var audit struct {
			SHA256    string          `json:"sha256"`
			Requested string          `json:"requested_action"`
			Allowed   []string        `json:"allowed_actions"`
			Decision  review.Decision `json:"decision"`
		}
		if err := json.Unmarshal([]byte(*ev.ResultJSON), &audit); err != nil {
			t.Fatalf("intent audit: %v", err)
		}
		if audit.SHA256 != contentHash || audit.Requested != "quarantine" ||
			len(audit.Allowed) != 1 || audit.Allowed[0] != "quarantine" ||
			audit.Decision.JEVModel != "jev-fixed-test" || audit.Decision.LLMModel != "minimax-fixed-test" {
			t.Fatalf("intent audit does not bind the reviewed facts: %+v", audit)
		}
		foundAudit = true
	}
	if !foundAudit {
		t.Fatal("quarantine intent has no review audit")
	}
}

// fixedAllowJudge replaces only the external model calls. The real Service still
// validates evidence, backup independence, content, identity, size and expiry.
type fixedAllowJudge struct{}

func (fixedAllowJudge) Review(context.Context, review.Facts) (review.Decision, error) {
	return review.Decision{Verdict: "allow", Reason: "fixed integration decision", JEVModel: "jev-fixed-test", LLMModel: "minimax-fixed-test"}, nil
}

// serviceReview runs the real Service over facts the executor measured. There is
// no evidence file and no scope root any more: everything the reviewers are
// shown was measured by the program, so this adapter has nothing to read.
func serviceReview(svc *review.Service, _ string) execute.ReviewFunc {
	return func(ctx context.Context, r review.Request) (review.Outcome, error) {
		return svc.Triage(ctx, r)
	}
}

func rejectingReview(_ context.Context, action string, _ store.Scope, it store.Item) (review.Approval, error) {
	return &fakeApproval{
		itemID: string(it.ID), action: action,
		reject: errs.New(errs.CodeReviewRequired, "fake reviewer rejected this action"),
	}, nil
}

func safetyReviewPlan(t *testing.T) (*env, store.Scope, plan.RulePlanResult) {
	t.Helper()
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "candidate.zip", "candidate-payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "safety-review", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	return e, sc, e.planFor(sc, r, 10)
}

func safetyReviewDueObject(t *testing.T) (*env, store.Scope, store.Item, store.Plan, string) {
	t.Helper()
	e, sc, res := safetyReviewPlan(t)
	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil {
		t.Fatalf("quarantine setup: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("quarantine setup result: %+v", out)
	}
	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due items: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected one due item, got %d", len(due))
	}
	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, due, domain.PlanManualPurge,
		e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatalf("purge plan: %v", err)
	}
	objectPath := filepath.Join(e.base, "trees", "exports", ".dustkeeper", "objects", due[0].ObjectName)
	return e, sc, due[0], p, objectPath
}

func requireReviewFailure(t *testing.T, out execute.ApplyResult, code errs.Code) {
	t.Helper()
	if out.Completed != 0 || len(out.Results) != 1 || out.Results[0].Code != string(code) {
		t.Fatalf("expected one %s refusal and no completed action, got %+v", code, out)
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
