package integration

// C07: two active grants for one capability is not "no authority".
//
// The store refuses to pick one, which is right. But it used to refuse with
// GRANT_REQUIRED, the same code as "this rule has no grant for this action" - and
// the scheduler skips that code as an ordinary state. An installation that had
// accumulated two approvals therefore ran to completion, quarantined nothing, and
// reported OK. The ambiguity is a record that contradicts itself, so it has to stop
// the scope and say why.

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// legacyActiveGrant writes a second active grant row straight into the state
// file, because the current API cannot produce one: every approval retires the
// grants it replaces. Two active rows for one capability are what an older build
// left behind, and a fixture that went through the current code would not be the
// state this test is about.
func legacyActiveGrant(t *testing.T, e *env, r store.Rule, capability string, digest domain.Digest) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+e.paths.DBFile+"?_busy_timeout=2000&_txlock=immediate")
	if err != nil {
		t.Fatalf("open the state file: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
INSERT INTO grants(grant_id, scope_id, rule_id, rule_revision, capability,
                   binding_json, digest, status, approved_at_ns)
VALUES(?,?,?,?,?,?,?, 'active', ?)`,
		string(domain.NewID("grant")), string(r.ScopeID), string(r.ID), r.Revision,
		capability, `{"legacy":true}`, string(digest), e.clock.Wall().UnixNano()); err != nil {
		t.Fatalf("write a legacy grant: %v", err)
	}
}

func TestAnAmbiguousQuarantineGrantStopsTheScope(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)

	// Two approvals for the same capability and revision, which is what an older
	// build left behind and what re-confirming is supposed to replace.
	e.grant(r, domain.CapAutoQuarantine, 5, 1<<20)
	legacyActiveGrant(t, e, r, domain.CapAutoQuarantine,
		domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))

	rep := e.runOnce()
	if rep.FailureCode != errs.CodeRecoveryRequired {
		t.Fatalf("the run ended with %q; an ambiguous grant has to be reported: %+v", rep.FailureCode, rep.Failures)
	}
	if rep.Quarantined != 0 {
		t.Fatalf("%d object(s) were quarantined under an ambiguous authority", rep.Quarantined)
	}
	if len(rep.Scopes) != 1 {
		t.Fatalf("the run reports %d scopes", len(rep.Scopes))
	}
	got := rep.Scopes[0]
	if got.Control != domain.ControlStopScope {
		t.Fatalf("the scope control is %q, want %q", got.Control, domain.ControlStopScope)
	}
	if got.FailureCode != errs.CodeRecoveryRequired {
		t.Fatalf("the scope failure code is %q", got.FailureCode)
	}
	if !strings.Contains(got.Message, "active auto_quarantine grants") ||
		!strings.Contains(got.Message, "re-enable the rule") {
		t.Fatalf("the message does not explain the ambiguity or how to fix it: %q", got.Message)
	}
	if !e.exists("exports", "a.zip") {
		t.Fatal("the file left the tree although no approval could be chosen")
	}
}

// The same for permanent deletion, where the consequence of guessing is worse.
//
// A purge that is already due is decided per object, under the approval that
// authorised that object - that is a separate check and this test does not move
// it. What this test covers is the other half: picking new work needs one
// authority the rule can name, and two active purge approvals mean the scope
// cannot name one.
func TestAnAmbiguousPurgeGrantStopsTheScope(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)

	e.grant(r, domain.CapAutoQuarantine, 5, 1<<20)
	e.grant(r, domain.CapAutoPurge, 5, 1<<20)

	// One ordinary run first, so the setup is a working installation rather than
	// a tree that was never processed.
	if first := e.runOnce(); first.Quarantined != 1 {
		t.Fatalf("the first run quarantined %d objects: %+v", first.Quarantined, first.Failures)
	}

	// The second approval an older build left behind, plus a second candidate
	// arriving afterwards: the run has something to pick and no way to justify
	// picking it.
	legacyActiveGrant(t, e, r, domain.CapAutoPurge,
		domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	e.writeFile("exports", "b.zip", "payload-b", 40*24*time.Hour)

	rep := e.runOnce()
	if rep.FailureCode != errs.CodeRecoveryRequired {
		t.Fatalf("the run ended with %q; an ambiguous purge grant has to be reported: %+v", rep.FailureCode, rep.Failures)
	}
	if rep.Quarantined != 0 || rep.Purged != 0 {
		t.Fatalf("the run did %d quarantine(s) and %d purge(s) under an ambiguous authority", rep.Quarantined, rep.Purged)
	}
	if len(rep.Scopes) != 1 {
		t.Fatalf("the run reports %d scopes", len(rep.Scopes))
	}
	if got := rep.Scopes[0]; got.Control != domain.ControlStopScope ||
		!strings.Contains(got.Message, "auto_purge grants") {
		t.Fatalf("the scope does not stop and name the ambiguous capability: %+v", got)
	}
	if !e.exists("exports", "b.zip") {
		t.Fatal("the second candidate left the tree although no approval could be chosen")
	}
	if items := e.quarantinedItems(sc.ID); len(items) != 1 {
		t.Fatalf("%d objects are in the vault, want the one the first run stored", len(items))
	}
}

// And the ordinary case stays ordinary: a quarantine-only approval with no purge
// grant runs normally. Distinguishing the two states must not make "no purge
// grant" an error.
func TestAQuarantineOnlyGrantStillRuns(t *testing.T) {
	e := newEnv(t)
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 5, 1<<20)

	rep := e.runOnce()
	if rep.FailureCode != "" {
		t.Fatalf("a quarantine-only grant produced failure %q: %+v", rep.FailureCode, rep.Failures)
	}
	if rep.Quarantined != 1 {
		t.Fatalf("the run quarantined %d objects, want 1", rep.Quarantined)
	}
	if !e.exists("exports", ".dustkeeper/objects") {
		t.Fatal("the vault was not created")
	}
}
