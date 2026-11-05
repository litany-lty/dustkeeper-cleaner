package plan

import (
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
	"dustkeeper/internal/strictjson"
)

// CheckAutomaticGrant compares the approval with the live context it approved.
// A new plan cannot silently replace the scope or protection revision in a grant.
func CheckAutomaticGrant(g store.Grant, sc store.Scope, rule store.Rule, capability string) error {
	refuse := func() error {
		return errs.New(errs.CodeGrantRequired, "automatic grant no longer matches the reviewed rule and scope; review and enable the rule again")
	}
	if sc.Kind != domain.KindManagedDisposable || g.Status != domain.GrantActive ||
		g.ScopeID != sc.ID || g.RuleID != rule.ID || g.RuleRevision != rule.Revision || g.Capability != capability {
		return refuse()
	}
	var binding GrantBindingV1
	if err := strictjson.Unmarshal([]byte(g.BindingJSON), &binding, strictjson.Defaults()); err != nil {
		return refuse()
	}
	expected, _, err := NewReviewBinding(sc, rule.Digest)
	if err != nil {
		return err
	}
	if binding.ReviewBindingV1 != expected || binding.RuleID != string(rule.ID) ||
		binding.RuleRevision != rule.Revision || binding.Capability != capability ||
		binding.ApprovedMaxFiles <= 0 || binding.ApprovedMaxLogicalBytes <= 0 || binding.BoundRetentionSeconds <= 0 {
		return refuse()
	}
	digest, err := Digest(PurposeGrantBinding, binding)
	if err != nil {
		return err
	}
	if digest != g.Digest {
		return refuse()
	}
	return nil
}
