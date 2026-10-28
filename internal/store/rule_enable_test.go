package store

import (
	"context"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// enableFixture is one registered scope with a stored rule revision, plus the
// exact expected values a caller would have read immediately before enabling.
type enableFixture struct {
	Store  *Store
	Scope  Scope
	Rule   Rule
	Review planReview
}

// planReview is the small piece of the review binding this package needs: the
// store never imports plan, so the test only pins the JSON bytes it is handed.
type planReview struct {
	JSON string
}

const (
	enableScopeID   = domain.ScopeID("scp_enable")
	enableRuleID    = domain.RuleID("rule_enable")
	enableRuleRev   = int64(1)
	enableRootJSON  = `{"persistent":{"inode":11,"device":12}}`
	enableRuleBody  = `{"retention_days":30}`
	enableRuleHash  = domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	enableGrantHash = domain.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	enablePurgeHash = domain.Digest("sha256:3333333333333333333333333333333333333333333333333333333333333333")
)

func newEnableFixture(t *testing.T) enableFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()

	sc := Scope{
		ID:                 enableScopeID,
		RawRoot:            []byte("/tmp/enable-target"),
		RootIdentityJSON:   enableRootJSON,
		InstanceID:         "inst-1",
		Kind:               domain.KindManagedDisposable,
		Generation:         3,
		ProtectionRevision: 1,
		Status:             domain.ScopeActive,
		CreatedAtNS:        1,
	}
	if err := s.InsertScope(ctx, sc); err != nil {
		t.Fatalf("InsertScope: %v", err)
	}
	r := Rule{
		ID:          enableRuleID,
		Revision:    enableRuleRev,
		ScopeID:     enableScopeID,
		DSLJSON:     enableRuleBody,
		Digest:      enableRuleHash,
		Status:      domain.RuleDraft,
		CreatedAtNS: 2,
	}
	if err := s.InsertRule(ctx, r); err != nil {
		t.Fatalf("InsertRule: %v", err)
	}
	return enableFixture{
		Store:  s,
		Scope:  sc,
		Rule:   r,
		Review: planReview{JSON: `{"scope_id":"scp_enable","scope_generation":3}`},
	}
}

// request is the enable request that matches what the fixture stored.
func (f enableFixture) request(grants ...Grant) EnableRuleRequest {
	return EnableRuleRequest{
		RuleID:                     f.Rule.ID,
		Revision:                   f.Rule.Revision,
		ExpectedRuleDigest:         f.Rule.Digest,
		ExpectedScopeID:            f.Scope.ID,
		ExpectedScopeGeneration:    f.Scope.Generation,
		ExpectedProtectionRevision: f.Scope.ProtectionRevision,
		ExpectedRootIdentityJSON:   f.Scope.RootIdentityJSON,
		ReviewBindingJSON:          f.Review.JSON,
		Grants:                     grants,
	}
}

func quarantineGrant(id domain.GrantID) Grant {
	return Grant{
		ID:           id,
		ScopeID:      enableScopeID,
		RuleID:       enableRuleID,
		RuleRevision: enableRuleRev,
		Capability:   domain.CapAutoQuarantine,
		BindingJSON:  `{"capability":"auto_quarantine"}`,
		Digest:       enableGrantHash,
		Status:       domain.GrantActive,
		ApprovedAtNS: 100,
	}
}

func purgeGrant(id domain.GrantID) Grant {
	return Grant{
		ID:           id,
		ScopeID:      enableScopeID,
		RuleID:       enableRuleID,
		RuleRevision: enableRuleRev,
		Capability:   domain.CapAutoPurge,
		BindingJSON:  `{"capability":"auto_purge"}`,
		Digest:       enablePurgeHash,
		Status:       domain.GrantActive,
		ApprovedAtNS: 100,
	}
}

// TestSlimEnableTransaction is A04: one confirmation either happens completely or
// not at all, a retry of the same approval is a no-op, and an approval that was
// taken away is never silently restored.
func TestSlimEnableTransaction(t *testing.T) {
	t.Run("a mismatch writes nothing at all", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		req := f.request(quarantineGrant("grant_q"))
		req.ExpectedScopeGeneration = 2 // the scope was re-registered after it was shown

		_, err := f.Store.EnableRule(ctx, req)
		if err == nil {
			t.Fatal("a stale scope generation was accepted")
		}
		if code := errs.CodeOf(err); code != errs.CodePlanStale {
			t.Fatalf("code = %s, want %s", code, errs.CodePlanStale)
		}
		assertNothingEnabled(t, f)
	})

	t.Run("a changed rule digest writes nothing at all", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		req := f.request(quarantineGrant("grant_q"))
		req.ExpectedRuleDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"

		if _, err := f.Store.EnableRule(ctx, req); err == nil {
			t.Fatal("a changed rule digest was accepted")
		}
		assertNothingEnabled(t, f)
	})

	t.Run("a different root in the same scope writes nothing at all", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		req := f.request(quarantineGrant("grant_q"))
		req.ExpectedRootIdentityJSON = `{"persistent":{"inode":99,"device":12}}`

		if _, err := f.Store.EnableRule(ctx, req); err == nil {
			t.Fatal("a swapped root was accepted")
		}
		assertNothingEnabled(t, f)
	})

	t.Run("a failure halfway rolls the whole enable back", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		// Store the quarantine grant, then refuse the purge grant. The refusal
		// happens after the rule was already activated and after the first grant
		// was written, so both of those have to be undone with it.
		if _, err := f.Store.db.Exec(`
CREATE TRIGGER test_refuse_purge_grant BEFORE INSERT ON grants
WHEN NEW.capability='auto_purge'
BEGIN SELECT RAISE(ABORT, 'injected failure'); END;`); err != nil {
			t.Fatalf("install trigger: %v", err)
		}

		_, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"), purgeGrant("grant_p")))
		if err == nil {
			t.Fatal("the enable succeeded although one grant could not be stored")
		}
		// Not one of the three steps survived.
		assertNothingEnabled(t, f)
		if _, gerr := f.Store.GetGrant(ctx, "grant_q"); gerr == nil {
			t.Fatal("the grant written before the failure survived the rollback")
		}
	})

	t.Run("the same approval twice keeps one grant with its original id", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		req := f.request(quarantineGrant("grant_q"))

		first, err := f.Store.EnableRule(ctx, req)
		if err != nil {
			t.Fatalf("first enable: %v", err)
		}
		if len(first) != 1 || first[0].ID != "grant_q" {
			t.Fatalf("first enable returned %v", first)
		}

		// A second confirmation of the very same screen: the user pressed the
		// same button twice.
		req.Grants = []Grant{quarantineGrant("")} // even without an id
		second, err := f.Store.EnableRule(ctx, req)
		if err != nil {
			t.Fatalf("second enable: %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("second enable returned %d grant(s), want 1", len(second))
		}
		if second[0].ID != "grant_q" {
			t.Fatalf("second enable created %s; the identical active grant was not reused", second[0].ID)
		}
		if second[0].ApprovedAtNS != 100 {
			t.Fatalf("approval time became %d; a retry must not look like a fresh approval", second[0].ApprovedAtNS)
		}
		if n := countGrants(t, f, "auto_quarantine", domain.GrantActive); n != 1 {
			t.Fatalf("%d active quarantine grants, want 1", n)
		}
	})

	t.Run("a revoked approval is not revived under its old id", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"))); err != nil {
			t.Fatalf("enable: %v", err)
		}
		if n, err := f.Store.RevokeGrants(ctx, enableRuleID, enableRuleRev, domain.CapAutoQuarantine); err != nil || n != 1 {
			t.Fatalf("RevokeGrants = %d, %v", n, err)
		}

		// The same numbers approved again: the old row stays revoked and a new
		// grant id is handed out, so "which approval is this object running
		// under" keeps one answer.
		again, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q")))
		if err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		if len(again) != 1 {
			t.Fatalf("re-enable returned %d grant(s)", len(again))
		}
		if again[0].ID == "grant_q" {
			t.Fatal("the revoked grant was revived under its old id")
		}
		if again[0].Status != domain.GrantActive {
			t.Fatalf("new grant status = %s, want %s", again[0].Status, domain.GrantActive)
		}
		old, err := f.Store.GetGrant(ctx, "grant_q")
		if err != nil {
			t.Fatalf("GetGrant: %v", err)
		}
		if old.Status != domain.GrantRevoked {
			t.Fatalf("old grant status = %s, want %s", old.Status, domain.GrantRevoked)
		}
	})

	t.Run("switching back to quarantine takes the purge authority away", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"), purgeGrant("grant_p"))); err != nil {
			t.Fatalf("enable with both: %v", err)
		}
		if n := countGrants(t, f, "auto_purge", domain.GrantActive); n != 1 {
			t.Fatalf("%d active purge grants before the switch, want 1", n)
		}

		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"))); err != nil {
			t.Fatalf("enable quarantine only: %v", err)
		}
		if n := countGrants(t, f, "auto_purge", domain.GrantActive); n != 0 {
			t.Fatalf("%d active purge grants after switching back to quarantine, want 0", n)
		}
		if n := countGrants(t, f, "auto_quarantine", domain.GrantActive); n != 1 {
			t.Fatalf("%d active quarantine grants, want 1", n)
		}
	})

	t.Run("a selection that means two things is refused", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		cases := map[string]EnableRuleRequest{
			"unknown capability": f.request(Grant{
				ScopeID: enableScopeID, RuleID: enableRuleID, RuleRevision: enableRuleRev,
				Capability: "auto_delete_everything", Digest: enableGrantHash,
			}),
			"same capability twice": f.request(quarantineGrant("a"), quarantineGrant("b")),
			"grant for another rule": f.request(Grant{
				ScopeID: enableScopeID, RuleID: "rule_other", RuleRevision: enableRuleRev,
				Capability: domain.CapAutoQuarantine, Digest: enableGrantHash,
			}),
			"grant for another scope": f.request(Grant{
				ScopeID: "scp_other", RuleID: enableRuleID, RuleRevision: enableRuleRev,
				Capability: domain.CapAutoQuarantine, Digest: enableGrantHash,
			}),
			"grant without a binding digest": f.request(Grant{
				ScopeID: enableScopeID, RuleID: enableRuleID, RuleRevision: enableRuleRev,
				Capability: domain.CapAutoQuarantine,
			}),
		}
		for name, req := range cases {
			if _, err := f.Store.EnableRule(ctx, req); err == nil {
				t.Fatalf("%s: the enable was accepted", name)
			} else if code := errs.CodeOf(err); code != errs.CodeInvalidArgument {
				t.Fatalf("%s: code = %s, want %s", name, code, errs.CodeInvalidArgument)
			}
		}
		assertNothingEnabled(t, f)
	})

	t.Run("enabling purge does not reach back to the objects already isolated", func(t *testing.T) {
		f := newEnableFixture(t)
		ctx := context.Background()
		seedItem(t, f.Store, enableScopeID, "itm_old", "pln_old")
		// The old item carries the approval that stopped it, and nothing else.
		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"))); err != nil {
			t.Fatalf("enable quarantine: %v", err)
		}
		before := itemRow(t, f.Store, "itm_old")

		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"), purgeGrant("grant_p"))); err != nil {
			t.Fatalf("enable cleanup: %v", err)
		}
		after := itemRow(t, f.Store, "itm_old")
		if after["auto_purge_grant_id"] != before["auto_purge_grant_id"] {
			t.Fatalf("purge grant on the old item changed from %q to %q",
				before["auto_purge_grant_id"], after["auto_purge_grant_id"])
		}
		if after["auto_purge_grant_id"] != "" {
			t.Fatalf("the old item was back-filled with purge authority %q", after["auto_purge_grant_id"])
		}
	})
}

func assertNothingEnabled(t *testing.T, f enableFixture) {
	t.Helper()
	ctx := context.Background()
	got, err := f.Store.GetRule(ctx, enableRuleID, enableRuleRev)
	if err != nil {
		t.Fatalf("GetRule: %v", err)
	}
	if got.Status == domain.RuleActive {
		t.Fatal("the rule was activated although the enable failed")
	}
	if strings.TrimSpace(got.ReviewBindingJSON) != "" {
		t.Fatalf("a review binding was written although the enable failed: %q", got.ReviewBindingJSON)
	}
	if n := countAllGrants(t, f); n != 0 {
		t.Fatalf("%d grant(s) were written although the enable failed", n)
	}
}

func countGrants(t *testing.T, f enableFixture, capability, status string) int {
	t.Helper()
	var n int
	if err := f.Store.db.QueryRow(
		"SELECT COUNT(*) FROM grants WHERE rule_id=? AND capability=? AND status=?",
		string(enableRuleID), capability, status).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

func countAllGrants(t *testing.T, f enableFixture) int {
	t.Helper()
	var n int
	if err := f.Store.db.QueryRow("SELECT COUNT(*) FROM grants WHERE rule_id=?", string(enableRuleID)).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

func itemRow(t *testing.T, s *Store, id domain.ItemID) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, col := range []string{"state", "auto_purge_grant_id", "purge_grant_id", "auto_quarantine_grant_id"} {
		var v *string
		err := s.db.QueryRow("SELECT "+col+" FROM items WHERE item_id=?", string(id)).Scan(&v)
		if err != nil {
			continue
		}
		if v != nil {
			out[col] = *v
		} else {
			out[col] = ""
		}
	}
	return out
}

// legacyActiveGrant inserts an active row the way an older build could: straight
// into the table, bypassing the replacement rule. It exists to describe a state
// that is already on disk, not to describe how this build writes.
func legacyActiveGrant(t *testing.T, f enableFixture, id domain.GrantID, capability string,
	digest domain.Digest, approvedAt int64) {

	t.Helper()
	if _, err := f.Store.db.Exec(`
INSERT INTO grants(grant_id, scope_id, rule_id, rule_revision, capability,
                   binding_json, digest, status, approved_at_ns)
VALUES(?,?,?,?,?,?,?, 'active', ?)`,
		string(id), string(enableScopeID), string(enableRuleID), enableRuleRev,
		capability, `{"legacy":true}`, string(digest), approvedAt); err != nil {
		t.Fatalf("insert a legacy grant: %v", err)
	}
}

// TestSlimOneActiveGrantPerCapability is D01: the approval the user confirmed last
// is the one that is active. Two active grants for one capability are refused
// rather than resolved by whichever row a query returns first.
func TestSlimOneActiveGrantPerCapability(t *testing.T) {
	ctx := context.Background()

	t.Run("a smaller approval retires the larger one", func(t *testing.T) {
		f := newEnableFixture(t)
		big := purgeGrant("grant_p100")
		big.Digest = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if _, err := f.Store.ReplaceGrant(ctx, big); err != nil {
			t.Fatalf("grant the larger ceiling: %v", err)
		}
		small := purgeGrant("grant_p10")
		small.Digest = domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		kept, err := f.Store.ReplaceGrant(ctx, small)
		if err != nil {
			t.Fatalf("grant the smaller ceiling: %v", err)
		}
		if kept.ID != "grant_p10" {
			t.Fatalf("the smaller approval came back as %s", kept.ID)
		}
		old, err := f.Store.GetGrant(ctx, "grant_p100")
		if err != nil {
			t.Fatalf("GetGrant: %v", err)
		}
		if old.Status != domain.GrantRevoked {
			t.Fatalf("the superseded grant is %s; confirming 10 files must not leave 100 in force", old.Status)
		}
		if n := countGrants(t, f, domain.CapAutoPurge, domain.GrantActive); n != 1 {
			t.Fatalf("%d active purge grants after the smaller approval, want 1", n)
		}
		active, err := f.Store.ActiveGrant(ctx, enableScopeID, enableRuleID, enableRuleRev, domain.CapAutoPurge)
		if err != nil {
			t.Fatalf("ActiveGrant: %v", err)
		}
		if active.ID != "grant_p10" {
			t.Fatalf("the active purge grant is %s, want the one just confirmed", active.ID)
		}
	})

	t.Run("reusing an identical approval retires a leftover row", func(t *testing.T) {
		f := newEnableFixture(t)
		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"))); err != nil {
			t.Fatalf("enable: %v", err)
		}
		// An older build could leave a second active row with the same digest.
		legacyActiveGrant(t, f, "grant_q_old", domain.CapAutoQuarantine, enableGrantHash, 1)
		if n := countGrants(t, f, domain.CapAutoQuarantine, domain.GrantActive); n != 2 {
			t.Fatalf("setup produced %d active grants, want 2", n)
		}

		// Confirming the same screen again reuses the existing approval; the row
		// it did not choose must not stay active beside it.
		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant(""))); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		if n := countGrants(t, f, domain.CapAutoQuarantine, domain.GrantActive); n != 1 {
			t.Fatalf("%d active quarantine grants after the retry, want 1", n)
		}
	})

	t.Run("two active grants are refused, not picked", func(t *testing.T) {
		f := newEnableFixture(t)
		legacyActiveGrant(t, f, "grant_a", domain.CapAutoPurge, enablePurgeHash, 1)
		other := domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
		legacyActiveGrant(t, f, "grant_b", domain.CapAutoPurge, other, 2)

		got, err := f.Store.ActiveGrant(ctx, enableScopeID, enableRuleID, enableRuleRev, domain.CapAutoPurge)
		if err == nil {
			t.Fatalf("an ambiguous pair was resolved to %s", got.ID)
		}
		if code := errs.CodeOf(err); code != errs.CodeGrantRequired {
			t.Fatalf("code = %s, want %s", code, errs.CodeGrantRequired)
		}
		if !strings.Contains(err.Error(), "grant_a") || !strings.Contains(err.Error(), "grant_b") {
			t.Fatalf("the refusal does not name both rows: %v", err)
		}
	})

	t.Run("a single active grant still resolves", func(t *testing.T) {
		f := newEnableFixture(t)
		if _, err := f.Store.EnableRule(ctx, f.request(quarantineGrant("grant_q"))); err != nil {
			t.Fatalf("enable: %v", err)
		}
		got, err := f.Store.ActiveGrant(ctx, enableScopeID, enableRuleID, enableRuleRev, domain.CapAutoQuarantine)
		if err != nil {
			t.Fatalf("ActiveGrant: %v", err)
		}
		if got.ID != "grant_q" {
			t.Fatalf("ActiveGrant returned %s", got.ID)
		}
	})
}
