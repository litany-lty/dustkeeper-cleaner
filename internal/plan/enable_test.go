package plan

import (
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// newGrants builds the two capability bindings for rule r1 revision 1.
func newGrants(t *testing.T) (ReviewBindingV1, GrantBindingV1, GrantBindingV1) {
	t.Helper()
	rb, _, err := NewReviewBinding(testScope(), "sha256:rule")
	if err != nil {
		t.Fatalf("review binding: %v", err)
	}
	q, _, err := NewGrantBinding(rb, "r1", 1, domain.CapAutoQuarantine, 10, 1<<20, 604800)
	if err != nil {
		t.Fatalf("quarantine grant: %v", err)
	}
	p, _, err := NewGrantBinding(rb, "r1", 1, domain.CapAutoPurge, 5, 1<<19, 604800)
	if err != nil {
		t.Fatalf("purge grant: %v", err)
	}
	return rb, q, p
}

// TestEnableBindingIsOneApprovalOfOneSet is A04's digest half: the selection is
// what the user confirmed, so the order, the members and the ceilings are all
// part of it, and the digest cannot be reused for a different screen.
func TestEnableBindingIsOneApprovalOfOneSet(t *testing.T) {
	rb, q, p := newGrants(t)

	both, bothDigest, err := NewEnableBinding(rb, "r1", 1, []GrantBindingV1{p, q})
	if err != nil {
		t.Fatalf("enable binding: %v", err)
	}
	// Quarantine first, whatever order the capabilities were handed over in.
	if len(both.Grants) != 2 || both.Grants[0].Capability != domain.CapAutoQuarantine ||
		both.Grants[1].Capability != domain.CapAutoPurge {
		t.Fatalf("the approval ordered the capabilities as %v", both.Grants)
	}

	again, againDigest, err := NewEnableBinding(rb, "r1", 1, []GrantBindingV1{q, p})
	if err != nil {
		t.Fatalf("enable binding: %v", err)
	}
	if againDigest != bothDigest {
		t.Fatal("the same selection produced two digests")
	}
	if again.RuleID != "r1" || again.RuleRevision != 1 {
		t.Fatalf("the approval lost the rule it applies to: %+v", again)
	}

	// Dropping a capability is a different approval: a digest that covered purge
	// must not be replayable as a quarantine-only one.
	_, qOnlyDigest, err := NewEnableBinding(rb, "r1", 1, []GrantBindingV1{q})
	if err != nil {
		t.Fatalf("enable binding: %v", err)
	}
	if qOnlyDigest == bothDigest {
		t.Fatal("the digest of a smaller selection equals the digest of the larger one")
	}

	// A different review underneath is a different approval too.
	otherScope := testScope()
	otherScope.ProtectionRevision = 6
	otherReview, _, err := NewReviewBinding(otherScope, "sha256:rule")
	if err != nil {
		t.Fatalf("review binding: %v", err)
	}
	// The grant has to be rebuilt on that review: a grant that reviewed different
	// state cannot ride along with an approval nobody looked at.
	otherGrant, _, err := NewGrantBinding(otherReview, "r1", 1, domain.CapAutoQuarantine, 10, 1<<20, 604800)
	if err != nil {
		t.Fatalf("grant binding: %v", err)
	}
	if _, d, err := NewEnableBinding(otherReview, "r1", 1, []GrantBindingV1{otherGrant}); err != nil {
		t.Fatalf("enable binding: %v", err)
	} else if d == qOnlyDigest {
		t.Fatal("a changed protection revision did not change the enable digest")
	}

	// A different ceiling is a different approval.
	wider, _, err := NewGrantBinding(rb, "r1", 1, domain.CapAutoQuarantine, 11, 1<<20, 604800)
	if err != nil {
		t.Fatalf("grant binding: %v", err)
	}
	if _, d, err := NewEnableBinding(rb, "r1", 1, []GrantBindingV1{wider}); err != nil {
		t.Fatalf("enable binding: %v", err)
	} else if d == qOnlyDigest {
		t.Fatal("raising the approved ceiling did not change the enable digest")
	}
}

// TestEnableBindingRefusesASelectionThatMeansTwoThings covers the shapes the
// atomic enable must never accept, because each of them has two readings.
func TestEnableBindingRefusesASelectionThatMeansTwoThings(t *testing.T) {
	rb, q, p := newGrants(t)

	cases := map[string]struct {
		id     domain.RuleID
		rev    int64
		grants []GrantBindingV1
	}{
		"no capability at all": {id: "r1", rev: 1},
		"no rule":              {id: "", rev: 1, grants: []GrantBindingV1{q}},
		"no revision":          {id: "r1", rev: 0, grants: []GrantBindingV1{q}},
		"the same twice":       {id: "r1", rev: 1, grants: []GrantBindingV1{q, q}},
		"a grant for another rule": {id: "r1", rev: 1, grants: []GrantBindingV1{
			func() GrantBindingV1 { g := q; g.RuleID = "r2"; return g }(),
		}},
		"a grant for another revision": {id: "r1", rev: 1, grants: []GrantBindingV1{
			func() GrantBindingV1 { g := q; g.RuleRevision = 2; return g }(),
		}},
		"a grant from another review": {id: "r1", rev: 1, grants: []GrantBindingV1{
			func() GrantBindingV1 { g := q; g.ProtectionRevision = 9; return g }(),
		}},
		"an unknown capability": {id: "r1", rev: 1, grants: []GrantBindingV1{
			func() GrantBindingV1 { g := q; g.Capability = "auto_delete"; return g }(),
		}},
		"a zero ceiling": {id: "r1", rev: 1, grants: []GrantBindingV1{
			func() GrantBindingV1 { g := p; g.ApprovedMaxFiles = 0; return g }(),
		}},
		"a zero byte ceiling": {id: "r1", rev: 1, grants: []GrantBindingV1{
			func() GrantBindingV1 { g := p; g.ApprovedMaxLogicalBytes = 0; return g }(),
		}},
	}
	for name, tc := range cases {
		_, _, err := NewEnableBinding(rb, tc.id, tc.rev, tc.grants)
		if err == nil {
			t.Fatalf("%s was accepted as an approval", name)
		}
		if code := errs.CodeOf(err); code != errs.CodeInvalidArgument {
			t.Fatalf("%s: code = %s, want %s", name, code, errs.CodeInvalidArgument)
		}
	}
}

// TestEnableDigestIsNotAReviewDigest keeps the three purposes apart. A client
// that captures one digest must not be able to spend it on another step.
func TestEnableDigestIsNotAReviewDigest(t *testing.T) {
	rb, q, _ := newGrants(t)
	_, reviewDigest, err := NewReviewBinding(testScope(), "sha256:rule")
	if err != nil {
		t.Fatalf("review binding: %v", err)
	}
	_, grantDigest, err := NewGrantBinding(rb, "r1", 1, domain.CapAutoQuarantine, 10, 1<<20, 604800)
	if err != nil {
		t.Fatalf("grant binding: %v", err)
	}
	_, enableDigest, err := NewEnableBinding(rb, "r1", 1, []GrantBindingV1{q})
	if err != nil {
		t.Fatalf("enable binding: %v", err)
	}
	if enableDigest == reviewDigest || enableDigest == grantDigest || reviewDigest == grantDigest {
		t.Fatalf("two purposes produced the same digest: review=%s grant=%s enable=%s",
			reviewDigest, grantDigest, enableDigest)
	}
}
