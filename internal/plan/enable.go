package plan

import (
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// PurposeEnableBinding is the digest purpose of the one-shot enable approval.
//
// It is deliberately a third purpose: a review digest may not be replayed as a
// grant digest, and neither may be replayed as an enable digest, because the
// enable digest is the only one that covers the exact set of capabilities.
const PurposeEnableBinding = "dustkeeper/enable-binding/v1"

// EnableBindingV1 is what the user confirms when a rule is enabled.
//
// It answers one question: "this rule revision, in this scope, may do exactly
// these actions with exactly these ceilings." The review binding underneath it
// pins the rule digest and the scope state, the grant bindings pin the
// per-action ceiling, and the list is the complete selected set - so removing a
// capability changes the digest instead of leaving an older approval valid.
//
// Candidate samples are deliberately absent. A preview shows them, but they are
// a description of a directory at one instant; binding them would make a normal
// long-lived rule look "changed" the moment a new file appeared.
type EnableBindingV1 struct {
	ReviewBindingV1
	RuleID       string           `json:"rule_id"`
	RuleRevision int64            `json:"rule_revision"`
	Grants       []GrantBindingV1 `json:"grants"`
}

// capabilityOrder is the fixed order the grants are stored and hashed in.
//
// It is written out rather than sorted: alphabetical order would put
// auto_purge before auto_quarantine, which reads as "purge first" in a document
// whose whole point is that quarantine comes first. A fixed order also means the
// same selection always produces the same digest.
var capabilityOrder = []string{domain.CapAutoQuarantine, domain.CapAutoPurge}

// NewEnableBinding joins a review binding and the selected capabilities into one
// approval, and returns the digest the user has to repeat.
func NewEnableBinding(rb ReviewBindingV1, id domain.RuleID, rev int64,
	grants []GrantBindingV1) (EnableBindingV1, domain.Digest, error) {

	if id == "" {
		return EnableBindingV1{}, "", errs.New(errs.CodeInvalidArgument,
			"an enable approval must name the rule it applies to")
	}
	if rev <= 0 {
		return EnableBindingV1{}, "", errs.New(errs.CodeInvalidArgument,
			"an enable approval must name the rule revision it applies to")
	}
	if len(grants) == 0 {
		return EnableBindingV1{}, "", errs.New(errs.CodeInvalidArgument,
			"an enable approval must select at least one capability")
	}

	ordered, err := orderGrants(grants, rb, id, rev)
	if err != nil {
		return EnableBindingV1{}, "", err
	}
	eb := EnableBindingV1{
		ReviewBindingV1: rb,
		RuleID:          string(id),
		RuleRevision:    rev,
		Grants:          ordered,
	}
	d, err := Digest(PurposeEnableBinding, eb)
	if err != nil {
		return EnableBindingV1{}, "", err
	}
	return eb, d, nil
}

// orderGrants checks that the selection is complete, unambiguous and about the
// same rule revision as the review binding, then returns it in the fixed order.
func orderGrants(grants []GrantBindingV1, rb ReviewBindingV1, id domain.RuleID, rev int64) ([]GrantBindingV1, error) {
	byCapability := make(map[string]GrantBindingV1, len(grants))
	for _, g := range grants {
		switch g.Capability {
		case domain.CapAutoQuarantine, domain.CapAutoPurge:
		default:
			return nil, errs.Newf(errs.CodeInvalidArgument,
				"capability %q is not one of %s, %s", g.Capability, domain.CapAutoQuarantine, domain.CapAutoPurge)
		}
		if _, dup := byCapability[g.Capability]; dup {
			return nil, errs.Newf(errs.CodeInvalidArgument,
				"capability %q was selected twice", g.Capability)
		}
		if g.RuleID != string(id) || g.RuleRevision != rev {
			return nil, errs.Newf(errs.CodeInvalidArgument,
				"the %s grant is for rule %s revision %d, not %s revision %d",
				g.Capability, g.RuleID, g.RuleRevision, id, rev)
		}
		// The grant has to be built on the same review binding: a grant that
		// reviewed a different rule digest or a different scope state would let a
		// capability ride along with an approval nobody looked at.
		if g.ReviewBindingV1 != rb {
			return nil, errs.Newf(errs.CodeInvalidArgument,
				"the %s grant was not built from this review binding", g.Capability)
		}
		if g.ApprovedMaxFiles <= 0 || g.ApprovedMaxLogicalBytes <= 0 {
			return nil, errs.Newf(errs.CodeInvalidArgument,
				"the %s grant has a zero ceiling, which forbids the action", g.Capability)
		}
		byCapability[g.Capability] = g
	}

	out := make([]GrantBindingV1, 0, len(byCapability))
	for _, capability := range capabilityOrder {
		if g, ok := byCapability[capability]; ok {
			out = append(out, g)
		}
	}
	return out, nil
}
