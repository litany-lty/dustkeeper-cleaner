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

func TestReviewCyclePurgeChecksBudgetBeforeReview(t *testing.T) {
	e, sc, it, p, objectPath := safetyReviewDueObject(t)
	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	calls := 0
	e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
		calls++
		return fakeLifecycleReview(ctx, action, sc, it)
	})
	_, err := e.exec.Purge(context.Background(), sctx, it, execute.PurgeOptions{
		Plan: &p, Trust: trustOK, Budget: domain.NewBudget(300, 1<<30, 0, 1<<30, 0),
	})
	if errs.CodeOf(err) != errs.CodeBudgetExhausted || calls != 0 || !pathExists(objectPath) {
		t.Fatalf("purge spent review calls without an action allowance: calls=%d error=%v retained=%v", calls, err, pathExists(objectPath))
	}
}

func TestReviewCyclePurgeRechecksApprovedMembers(t *testing.T) {
	e, sc, it, p, objectPath := safetyReviewDueObject(t)
	e.execRaw(t, "UPDATE items SET raw_relative=? WHERE item_id=?", []byte("changed-after-approval.zip"), string(it.ID))
	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	calls := 0
	e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
		calls++
		return fakeLifecycleReview(ctx, action, sc, it)
	})
	_, err := e.exec.ApplyPurgePlan(context.Background(), sctx, p.ID, p.Digest,
		domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
	if errs.CodeOf(err) != errs.CodeDigestMismatch || calls != 0 || !pathExists(objectPath) {
		t.Fatalf("changed members reused approval: calls=%d error=%v retained=%v", calls, err, pathExists(objectPath))
	}
}

func TestReviewCycleScopePauseDuringReviewStopsAction(t *testing.T) {
	for _, action := range []string{"quarantine", "purge"} {
		t.Run(action, func(t *testing.T) {
			var e *env
			var sc store.Scope
			var it store.Item
			var p store.Plan
			var objectPath string
			if action == "purge" {
				e, sc, it, p, objectPath = safetyReviewDueObject(t)
			} else {
				e2, sc2, result := safetyReviewPlan(t)
				e, sc, p, it = e2, sc2, result.Plan, result.Items[0]
			}
			sctx := e.scopeCtx(sc.ID)
			e.exec.Guard = e.guard(sctx)
			e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
				if err := e.st.SetScopeStatus(ctx, sc.ID, domain.ScopePaused); err != nil {
					t.Fatal(err)
				}
				return fakeLifecycleReview(ctx, action, sc, it)
			})
			var err error
			budget := domain.NewBudget(300, 1<<30, 10, 1<<30, 0)
			if action == "purge" {
				_, err = e.exec.Purge(context.Background(), sctx, it, execute.PurgeOptions{Plan: &p, Trust: trustOK, Budget: budget})
			} else {
				_, err = e.exec.Quarantine(context.Background(), sctx, p, it, budget)
			}
			if errs.CodeOf(err) != errs.CodePaused {
				t.Fatalf("scope pause during review did not stop %s: %v", action, err)
			}
			if action == "purge" && !pathExists(objectPath) || action == "quarantine" && !e.exists("exports", "candidate.zip") {
				t.Fatal("paused scope file changed")
			}
		})
	}
}

func TestReviewCyclePartialPlanResumesRemainingMembers(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "first", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "second", 40*24*time.Hour)
	e.scan(sc.ID)
	rule := e.rule(sc.ID, "partial-review", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	p := e.planFor(sc, rule, 10).Plan
	calls := 0
	e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
		calls++
		if calls == 2 {
			return nil, errs.New(errs.CodeModelUnavailable, "temporary fake provider failure")
		}
		return fakeLifecycleReview(ctx, action, sc, it)
	})
	first, err := e.apply(sc, p, p.Digest)
	if err != nil || first.Status != domain.PlanPartial || first.Completed != 1 {
		t.Fatalf("partial setup: %+v %v", first, err)
	}
	e.closeScopeCtxs()
	e.exec.Review = legacyReview(fakeLifecycleReview)
	next, err := e.apply(sc, p, p.Digest)
	if err != nil || next.Status != domain.PlanComplete || next.Completed != 1 || next.Skipped != 1 {
		t.Fatalf("remaining member did not resume once: %+v %v", next, err)
	}
}

func TestReviewCycleNewProtectionDuringReviewStopsAction(t *testing.T) {
	for _, action := range []string{"quarantine", "purge"} {
		t.Run(action, func(t *testing.T) {
			var e *env
			var sc store.Scope
			var it store.Item
			var p store.Plan
			var objectPath string
			if action == "purge" {
				e, sc, it, p, objectPath = safetyReviewDueObject(t)
			} else {
				e2, sc2, result := safetyReviewPlan(t)
				e, sc, p, it = e2, sc2, result.Plan, result.Items[0]
			}
			sctx := e.scopeCtx(sc.ID)
			e.exec.Guard = e.guard(sctx)
			e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
				_, err := e.st.InsertProtection(ctx, store.Protection{
					ID: "during-review", ScopeID: sc.ID, RawRelative: it.RawRelative,
					MatchKind: "exact", Source: "user", EvidenceJSON: "{}", CreatedAtNS: e.clock.Wall().UnixNano(),
				})
				if err != nil {
					t.Fatal(err)
				}
				return fakeLifecycleReview(ctx, action, sc, it)
			})
			var err error
			budget := domain.NewBudget(300, 1<<30, 10, 1<<30, 0)
			if action == "purge" {
				_, err = e.exec.Purge(context.Background(), sctx, it, execute.PurgeOptions{Plan: &p, Trust: trustOK, Budget: budget})
			} else {
				_, err = e.exec.Quarantine(context.Background(), sctx, p, it, budget)
			}
			if errs.CodeOf(err) != errs.CodePlanStale {
				t.Fatalf("new protection did not invalidate %s: %v", action, err)
			}
			if action == "purge" && !pathExists(objectPath) || action == "quarantine" && !e.exists("exports", "candidate.zip") {
				t.Fatal("newly protected file changed")
			}
		})
	}
}

func TestReviewCyclePartialPurgeResumesRemainingMembers(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "first", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "second", 40*24*time.Hour)
	e.scan(sc.ID)
	rule := e.rule(sc.ID, "partial-purge-review", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	q := e.planFor(sc, rule, 10).Plan
	if out, err := e.apply(sc, q, q.Digest); err != nil || out.Completed != 2 {
		t.Fatalf("quarantine setup: %+v %v", out, err)
	}
	e.closeScopeCtxs()
	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, due, domain.PlanManualPurge, e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatal(err)
	}
	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	calls := 0
	e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
		calls++
		if calls == 2 {
			return nil, errs.New(errs.CodeModelUnavailable, "temporary fake provider failure")
		}
		return fakeLifecycleReview(ctx, action, sc, it)
	})
	apply := func() (execute.ApplyResult, error) {
		return e.exec.ApplyPurgePlan(context.Background(), sctx, p.ID, p.Digest, domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
	}
	first, err := apply()
	if err != nil || first.Status != domain.PlanPartial || first.Completed != 1 {
		t.Fatalf("partial setup: %+v %v", first, err)
	}
	e.exec.Review = legacyReview(fakeLifecycleReview)
	next, err := apply()
	if err != nil || next.Status != domain.PlanComplete || next.Completed != 1 || next.Skipped != 1 {
		t.Fatalf("remaining purge did not resume once: %+v %v", next, err)
	}
}

func TestReviewCycleUnstartedMembersKeepActualStopReason(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "first", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "second", 40*24*time.Hour)
	e.scan(sc.ID)
	rule := e.rule(sc.ID, "stop-reason", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	p := e.planFor(sc, rule, 10).Plan
	e.exec.Review = legacyReview(func(context.Context, string, store.Scope, store.Item) (review.Approval, error) {
		return nil, errs.New(errs.CodeModelUnavailable, "fake provider unavailable")
	})
	out, err := e.apply(sc, p, p.Digest)
	if err != nil || len(out.Results) != 2 { t.Fatalf("bad result: %+v %v", out, err) }
	if out.Results[1].Code != string(errs.CodeModelUnavailable) || out.Results[1].Evidence != domain.EvidenceNotStarted {
		t.Fatalf("unstarted member was given a misleading reason: %+v", out.Results[1])
	}
}

func TestReviewCycleExpiredPurgeIncludesEveryResult(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "first", 40*24*time.Hour)
	e.writeFile("exports", "b.zip", "second", 40*24*time.Hour)
	e.scan(sc.ID)
	rule := e.rule(sc.ID, "expiry-results", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	q := e.planFor(sc, rule, 10).Plan
	if out, err := e.apply(sc, q, q.Digest); err != nil || out.Completed != 2 { t.Fatalf("setup: %+v %v", out, err) }
	e.closeScopeCtxs()
	e.clock.Advance(31*24*time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), sc.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil { t.Fatal(err) }
	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, due, domain.PlanManualPurge, e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil { t.Fatal(err) }
	execute.TestHookBeforeComplete = func(action string) error {
		if action == "purge" { e.clock.SetWall(time.Unix(0, p.ExpiresAtNS+1)) }
		return nil
	}
	t.Cleanup(func() { execute.TestHookBeforeComplete = nil })
	sctx := e.scopeCtx(sc.ID)
	out, err := e.exec.ApplyPurgePlan(context.Background(), sctx, p.ID, p.Digest, domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
	if err != nil || out.Completed != 1 || out.Failed != 1 || len(out.Results) != 2 {
		t.Fatalf("expiry lost member-level outcome: %+v %v", out, err)
	}
	if out.Results[1].Code != string(errs.CodePlanExpired) || out.Results[1].Evidence != domain.EvidenceNotStarted {
		t.Fatalf("expiry result is incomplete: %+v", out.Results[1])
	}
}

func TestReviewCycleGlobalPausePreventsModelCalls(t *testing.T) {
	e, sc, res := safetyReviewPlan(t)
	if _, err := e.exec.Gate.Request("test pause"); err != nil { t.Fatal(err) }
	calls := 0
	e.exec.Review = legacyReview(func(ctx context.Context, action string, sc store.Scope, it store.Item) (review.Approval, error) {
		calls++
		return fakeLifecycleReview(ctx, action, sc, it)
	})
	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err != nil || out.Code != string(errs.CodePaused) || calls != 0 || !e.exists("exports", "candidate.zip") {
		t.Fatalf("paused instance contacted reviewer: calls=%d result=%+v err=%v", calls, out, err)
	}
}

func TestReviewCycleChangedProtectionContextInvalidatesGrant(t *testing.T) {
	e:=newEnv(t);sc:=e.addScope("exports",domain.KindManagedDisposable)
	e.writeFile("exports","old.zip","payload",40*24*time.Hour);e.scan(sc.ID)
	rule:=e.rule(sc.ID,"context-grant",[]string{"*.zip"},nil,30*24*time.Hour,7*24*time.Hour)
	e.grant(rule,domain.CapAutoQuarantine,10,1<<30)
	ctx:=context.Background()
	_,err:=e.st.InsertProtection(ctx,store.Protection{ID:"context-change",ScopeID:sc.ID,RawRelative:[]byte("other"),MatchKind:domain.MatchExact,Source:domain.ProtectUser,EvidenceJSON:"{}",CreatedAtNS:e.clock.Wall().UnixNano()})
	if err!=nil {t.Fatal(err)}
	if err:=e.st.DeactivateProtection(ctx,"context-change");err!=nil {t.Fatal(err)}
	calls:=0
	e.exec.Review=legacyReview(func(ctx context.Context,action string,sc store.Scope,it store.Item)(review.Approval,error){calls++;return fakeLifecycleReview(ctx,action,sc,it)})
	rep,err:=e.run.Run(ctx)
	if rep.Quarantined!=0 || calls!=0 || !e.exists("exports","old.zip") {t.Fatalf("old grant silently reused under a new protection context: %+v err=%v calls=%d",rep,err,calls)}
}

func TestReviewCycleExactDoubleReadBudgetCanQuarantine(t *testing.T) {
	e,sc,res:=safetyReviewPlan(t)
	info,err:=os.Stat(filepath.Join(e.base,"trees","exports","candidate.zip"));if err!=nil {t.Fatal(err)}
	sctx:=e.scopeCtx(sc.ID);e.exec.Guard=e.guard(sctx)
	budget:=domain.NewBudget(300,2*info.Size(),1,info.Size(),0)
	out,err:=e.exec.ApplyQuarantinePlan(context.Background(),sctx,res.Plan.ID,res.Plan.Digest,budget)
	if err!=nil||out.Completed!=1 {t.Fatalf("exactly budgeted double-read did not complete: %+v err=%v",out,err)}
}

func TestReviewCycleRetiredPolicyCannotReplayApprovedPlan(t *testing.T) {
	e,sc,res:=safetyReviewPlan(t)
	p:=res.Plan
	var header domain.PlanHeader
	if err:=json.Unmarshal([]byte(p.HeaderJSON),&header);err!=nil {t.Fatal(err)}
	header.PolicyDigest=domain.DigestOf("retired-policy",nil)
	raw,err:=json.Marshal(header);if err!=nil {t.Fatal(err)};p.HeaderJSON=string(raw)
	doc,err:=plan.BuildDocFromStore(p,res.Items);if err!=nil {t.Fatal(err)}
	p.Digest,err=doc.ComputeDigest();if err!=nil {t.Fatal(err)}
	e.execRaw(t,"UPDATE plans SET header_json=?,digest=? WHERE plan_id=?",p.HeaderJSON,string(p.Digest),string(p.ID))
	out,err:=e.apply(sc,p,p.Digest)
	if errs.CodeOf(err)!=errs.CodePlanStale || out.Completed!=0 || !e.exists("exports","candidate.zip") {t.Fatalf("retired policy approval was replayed: %+v err=%v",out,err)}
}

func TestReviewCyclePurgeRequiresCompleteRetentionEvidence(t *testing.T) {
	for _,kind:=range []string{"missing_start","early_deadline","changed_duration"} {
		t.Run(kind,func(t *testing.T){
			e,sc,it,_,object:=safetyReviewDueObject(t)
			switch kind {
			case "missing_start":e.execRaw(t,"UPDATE items SET quarantined_at_ns=NULL WHERE item_id=?",string(it.ID))
			default:
				e.clock.SetWall(time.Unix(0,*it.QuarantinedAtNS).Add(24*time.Hour))
				e.execRaw(t,"UPDATE items SET purge_not_before_ns=? WHERE item_id=?",e.clock.Wall().Add(-time.Second).UnixNano(),string(it.ID))
				if kind=="changed_duration" {
					e.clock.SetWall(time.Unix(0,*it.QuarantinedAtNS).Add(8*24*time.Hour))
					e.execRaw(t,"UPDATE items SET bound_retention_seconds=604800 WHERE item_id=?",string(it.ID))
				}
			}
			it,err:=e.st.GetItem(context.Background(),it.ID);if err!=nil {t.Fatal(err)}
			p,err:=plan.CreatePurgePlan(context.Background(),e.st,sc,[]store.Item{it},domain.PlanManualPurge,e.clock.Wall().UnixNano(),int64(plan.DefaultTTL));if err!=nil {t.Fatal(err)}
			sctx:=e.scopeCtx(sc.ID);calls:=0
			e.exec.Review=legacyReview(func(ctx context.Context,action string,sc store.Scope,it store.Item)(review.Approval,error){calls++;return fakeLifecycleReview(ctx,action,sc,it)})
			_,err=e.exec.Purge(context.Background(),sctx,it,execute.PurgeOptions{Plan:&p,Trust:trustOK,Budget:domain.NewBudget(300,1<<30,10,1<<30,0)})
			if err==nil||calls!=0||!pathExists(object) {t.Fatalf("incomplete retention proof permitted purge: kind=%s calls=%d err=%v retained=%v",kind,calls,err,pathExists(object))}
		})
	}
}

func TestReviewCycleMissingMetadataProofCannotPurge(t *testing.T) {
	e,sc,it,p,object:=safetyReviewDueObject(t)
	e.execRaw(t,"UPDATE items SET metadata_hash=NULL WHERE item_id=?",string(it.ID))
	var err error;it,err=e.st.GetItem(context.Background(),it.ID);if err!=nil {t.Fatal(err)}
	sctx:=e.scopeCtx(sc.ID);calls:=0
	e.exec.Review=legacyReview(func(ctx context.Context,action string,sc store.Scope,it store.Item)(review.Approval,error){calls++;return fakeLifecycleReview(ctx,action,sc,it)})
	_,err=e.exec.Purge(context.Background(),sctx,it,execute.PurgeOptions{Plan:&p,Trust:trustOK,Budget:domain.NewBudget(300,1<<30,10,1<<30,0)})
	if errs.CodeOf(err)!=errs.CodeHeld||calls!=0||!pathExists(object) {t.Fatalf("missing metadata proof permitted action: err=%v calls=%d retained=%v",err,calls,pathExists(object))}
}
