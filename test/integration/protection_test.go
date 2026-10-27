package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/store"
)

// The tests in this file are the four entry cases section 5.2 asks for. Each one
// asserts that a protection which was added at a different time, or spelled in a
// different way, still stops the action that would move or delete the object —
// and that the audit trail contains no intent for that action.

// insertProtection records one user protection.
func (e *env) insertProtection(sc store.Scope, rel string, kind string) {
	e.t.Helper()
	if _, err := e.st.InsertProtection(context.Background(), store.Protection{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.ID,
		RawRelative:  []byte(rel),
		MatchKind:    kind,
		Source:       domain.ProtectUser,
		EvidenceJSON: `{"source":"user"}`,
		Active:       true,
		CreatedAtNS:  e.clock.Wall().UnixNano(),
	}); err != nil {
		e.t.Fatalf("insert protection: %v", err)
	}
}

// lastIntentAction reports the action of the most recent intent for one item.
func (e *env) lastIntentAction(id domain.ItemID) string {
	e.t.Helper()
	po, ok, err := e.st.LastIntentForItem(context.Background(), id)
	if err != nil {
		e.t.Fatalf("last intent: %v", err)
	}
	if !ok {
		return ""
	}
	return po.Action
}

// tryPurge runs a manual purge with a plan of the right shape and returns the
// result. The executor's guard is deliberately left unset: the purge path has to
// resolve protections itself for a slot whose file no longer exists.
func (e *env) tryPurge(sc store.Scope, it store.Item) error {
	e.t.Helper()
	e.exec.Guard = nil
	p, err := plan.CreatePurgePlan(context.Background(), e.st, sc, []store.Item{it},
		domain.PlanManualPurge, e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		e.t.Fatalf("purge plan: %v", err)
	}
	sctx := e.scopeCtx(sc.ID)
	_, err = e.exec.Purge(context.Background(), sctx, it, execute.PurgeOptions{
		Plan:   &p,
		Budget: domain.NewBudget(300, 1<<30, 10, 1<<30, 0),
		Trust:  trustOK,
	})
	return err
}

// vaultObjectExists reports whether the quarantined object is still in the vault.
func (e *env) vaultObjectExists(sc store.Scope, it store.Item) bool {
	e.t.Helper()
	path := filepath.Join(string(sc.RawRoot), ".dustkeeper", "objects", it.ObjectName)
	_, err := os.Stat(path)
	return err == nil
}

// caseInsensitiveVolume reports whether the temporary volume ignores case, which
// is what makes "Folder" and "folder" one directory.
func caseInsensitiveVolume(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	name := filepath.Join(dir, "CaseProbe")
	if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// TestAProtectionAddedAfterTheScanIsHonouredByANewPlan covers the first entry
// case: the object was scanned, the protection arrived afterwards, and the plan
// is created after that. The plan is not stale — it is new — so only the shared
// protection judgement can stop it.
func TestAProtectionAddedAfterTheScanIsHonouredByANewPlan(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "folder/a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.insertProtection(sc, "folder", domain.MatchSubtree)

	// The plan is created after the protection, so it is not stale: only the
	// shared protection judgement can keep the object out of it.
	res, err := e.planForMayFail(sc, r, 10)
	requireCode(t, err, errs.CodeNoCandidates)
	if res.Reasons[policy.ReasonProtected] != 1 {
		t.Fatalf("the object was not recorded as protected: %+v", res.Reasons)
	}
	if !e.exists("exports", "folder/a.zip") {
		t.Fatal("the protected object was moved")
	}
}

// TestAProtectionAddedAfterThePlanIsHonouredByTheExecutor covers the second half
// of the same window: the plan was created while the subtree was unprotected, and
// the protection arrived before it ran. A stale plan must not be a licence.
func TestAProtectionAddedAfterThePlanIsHonouredByTheExecutor(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "folder/a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	if len(res.Items) != 1 {
		t.Fatalf("expected one plan member before the protection, got %d", len(res.Items))
	}
	e.insertProtection(sc, "folder", domain.MatchSubtree)

	// The plan was approved before the protection existed. Applying it must leave
	// the object where it is; whether the runner reports that as a skip or as a
	// refusal is a reporting detail, and both are acceptable.
	out, err := e.apply(sc, res.Plan, res.Plan.Digest)
	if err == nil && out.Completed != 0 {
		t.Fatalf("the plan moved %d objects after the subtree became protected", out.Completed)
	}
	if !e.exists("exports", "folder/a.zip") {
		t.Fatal("the protected object was moved")
	}
	if got := e.lastIntentAction(res.Items[0].ID); got != "" {
		t.Fatalf("an intent was recorded for a protected object: %s", got)
	}
}

// TestACaseVariantOfTheProtectedSubtreeStillProtects is the APFS case the review
// describes: the kernel resolves "Folder" and "folder" to one directory, so a
// string prefix comparison is not an answer.
func TestACaseVariantOfTheProtectedSubtreeStillProtects(t *testing.T) {
	if !caseInsensitiveVolume(t) {
		t.Skip("this volume distinguishes case, so the two spellings are two directories")
	}
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "Folder/a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.insertProtection(sc, "folder", domain.MatchSubtree)

	// The plan is created after the protection, so it is not stale: only the
	// shared protection judgement can keep the object out of it.
	res, err := e.planForMayFail(sc, r, 10)
	requireCode(t, err, errs.CodeNoCandidates)
	if res.Reasons[policy.ReasonProtected] != 1 {
		t.Fatalf("the object was not recorded as protected: %+v", res.Reasons)
	}
	if !e.exists("exports", "Folder/a.zip") {
		t.Fatal("the object under the differently spelled subtree was moved")
	}
}

// TestANameThatDiffersOnlyByNormalisationFreezesThePurge covers "隔离后新增
// Unicode exact": the object is already in the vault, and the protection that
// arrives afterwards is the same name written in the other normal form. Nothing
// can resolve that by identity — the entry is gone — so the answer is a hold, not
// a deletion.
func TestANameThatDiffersOnlyByNormalisationFreezesThePurge(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)

	const composed = "Caf\u00e9.zip"    // NFC
	const decomposed = "Cafe\u0301.zip" // NFD
	if composed == decomposed {
		t.Fatal("the fixture names are identical")
	}
	e.writeFile("exports", composed, "payload", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"Caf*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	if len(res.Items) != 1 {
		t.Fatalf("expected the object to be planned once, got %d", len(res.Items))
	}
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("expected one quarantined object, got %d", len(items))
	}
	due := items[0]
	if action := e.lastIntentAction(due.ID); action != "quarantine" {
		t.Fatalf("expected the last intent to be the quarantine, got %q", action)
	}

	// The protection arrives after the object is out of the tree, spelled the
	// other way.
	e.insertProtection(sc, decomposed, domain.MatchExact)
	e.clock.Advance(31 * 24 * time.Hour)

	err := e.tryPurge(sc, due)
	requireCode(t, err, errs.CodeProtectionAliasUnresolved)
	if action := e.lastIntentAction(due.ID); action != "quarantine" {
		t.Fatalf("a purge intent was recorded for an unresolved alias: %q", action)
	}
	if !e.vaultObjectExists(sc, due) {
		t.Fatal("the object was deleted while the alias was unresolved")
	}
}

// TestAReoccupiedSlotStaysProtected covers the fourth entry case: the slot now
// holds a different object, so identity says nothing, and the protection is
// spelled differently. A new inode at the same name must not make the protection
// somebody else's.
func TestAReoccupiedSlotStaysProtected(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("expected one quarantined object, got %d", len(items))
	}
	due := items[0]

	e.insertProtection(sc, "A.ZIP", domain.MatchExact)
	// Something else now sits in the slot.
	e.writeFile("exports", "a.zip", "a different payload", 24*time.Hour)
	e.clock.Advance(31 * 24 * time.Hour)

	if err := e.tryPurge(sc, due); err == nil {
		t.Fatal("the object was purged although its slot is protected under another spelling")
	}
	if !e.vaultObjectExists(sc, due) {
		t.Fatal("the object was deleted although its slot is protected")
	}
	if action := e.lastIntentAction(due.ID); action != "quarantine" {
		t.Fatalf("a purge intent was recorded for a protected slot: %q", action)
	}
}

// TestAnUnrelatedProtectionDoesNotBlockADuePurge keeps the fix from turning every
// protection in a directory into a freeze: a different name in the same
// directory is a different slot.
func TestAnUnrelatedProtectionDoesNotBlockADuePurge(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.writeFile("exports", "keep.zip", "payload-b", 40*24*time.Hour)
	e.scan(sc.ID)

	r := e.rule(sc.ID, "rule-1", []string{"a.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 10)
	if len(res.Items) != 1 {
		t.Fatalf("expected only a.zip to be planned, got %d", len(res.Items))
	}
	if _, err := e.apply(sc, res.Plan, res.Plan.Digest); err != nil {
		t.Fatalf("apply: %v", err)
	}
	e.insertProtection(sc, "keep.zip", domain.MatchExact)

	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("expected one quarantined object, got %d", len(items))
	}
	due := items[0]
	e.clock.Advance(31 * 24 * time.Hour)

	if err := e.tryPurge(sc, due); err != nil {
		t.Fatalf("a protection for a different name blocked the purge: %v", err)
	}
	if e.vaultObjectExists(sc, due) {
		t.Fatal("the due object was not purged")
	}
	if action := e.lastIntentAction(due.ID); action != "purge" {
		t.Fatalf("expected a purge intent, got %q", action)
	}
}

// TestSpellingsMayCollideIsNotACaseFold pins the comparison itself: it decides
// ambiguity, and it must not claim that two ordinary different names collide.
func TestSpellingsMayCollideIsNotACaseFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a.zip", "a.zip", true},
		{"a.zip", "A.ZIP", true},
		{"Caf\u00e9.zip", "Cafe\u0301.zip", true},
		{"a.zip", "b.zip", false},
		{"report.pdf", "report.pdx", false},
		{"folder", "folder2", false},
	}
	for _, c := range cases {
		if got := policy.SpellingsMayCollide(c.a, c.b); got != c.want {
			t.Fatalf("SpellingsMayCollide(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
