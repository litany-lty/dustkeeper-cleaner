package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// Section 9.5: from Q_INTENT on, the keeper's scope plus original path is an
// internal protection until every object depending on it is restored or purged.
// A later lifecycle rule must not be able to take the surviving copy away.
func TestTheKeeperOfAnIsolatedDuplicateIsNotQuarantined(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("dups", domain.KindAssisted)
	e.writeFile("dups", "one.bin", "same-bytes", 40*24*time.Hour)
	e.writeFile("dups", "two.bin", "same-bytes", 40*24*time.Hour)
	e.scan(sc.ID)

	res, err := e.det.Detect(context.Background(), e.scopeCtx(sc.ID), nil)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.PlannableGroups != 1 {
		t.Fatalf("expected one plannable group, got %d (%v)", res.PlannableGroups, res.Notes)
	}
	p, items, err := e.det.CreatePlan(context.Background(), e.scopeCtx(sc.ID), res.Groups)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := e.apply(sc, p, p.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one copy to be isolated, got %d", len(items))
	}
	if !e.exists("dups", "one.bin") {
		t.Fatal("the keeper itself moved")
	}

	// A lifecycle rule now selects every *.bin in the scope, keeper included.
	r := e.rule(sc.ID, "sweep", []string{"*.bin"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res2 := e.planFor(sc, r, 10)
	out, err := e.apply(sc, res2.Plan, res2.Plan.Digest)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if !e.exists("dups", "one.bin") {
		t.Fatal("the keeper was quarantined while an isolated object still depends on it")
	}
	if out.Completed != 0 {
		t.Fatalf("the keeper was moved: %+v", out)
	}
	var explained bool
	for _, r := range out.Results {
		if r.Code == string(errs.CodeSkippedProtected) &&
			strings.Contains(r.Message, "keeper of an isolated duplicate") {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("no result explained the keeper by its keeper dependency, got %+v", out.Results)
	}
}
