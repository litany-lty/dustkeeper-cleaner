package dedup

import (
	"encoding/json"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/store"
)

// policyDigest re-exports the protection policy digest so duplicate plans bind
// to the same policy generation as lifecycle plans.
func policyDigest() domain.Digest { return policy.PolicyDigest() }

// scopeIdentityOf parses the stored root identity and hashes its persistent half.
func scopeIdentityOf(sc store.Scope) (string, domain.RootIdentity, error) {
	var id domain.RootIdentity
	if err := json.Unmarshal([]byte(sc.RootIdentityJSON), &id); err != nil {
		return "", domain.RootIdentity{}, errs.Newf(errs.CodeStateWriteFailed,
			"scope %s identity is unreadable: %v", sc.ID, err)
	}
	d, err := plan.Digest("dustkeeper/root-identity/v1", id.Persistent)
	if err != nil {
		return "", domain.RootIdentity{}, err
	}
	return string(d), id, nil
}
