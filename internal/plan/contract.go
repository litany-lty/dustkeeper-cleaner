package plan

import (
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/strictjson"
)

// ParseContract decodes the operation contract persisted in an intent event.
//
// Reconciliation compares belief with reality after a crash, so "the fields were
// zero" must never be confused with "the record did not say". R08 records that
// reconciliation read the item row instead of the contract and turned a missing
// hash into an empty string; the result was an object whose recorded evidence
// could never match itself again. Decoding strictly, with the mandatory fields
// named, makes an incomplete contract an explicit refusal instead of a silently
// weaker comparison.
func ParseContract(raw string) (OperationContractV1, error) {
	var c OperationContractV1
	if raw == "" {
		return c, errs.New(errs.CodeRecoveryRequired,
			"the pending operation carries no contract, so it cannot be reconciled")
	}
	if err := strictjson.Unmarshal([]byte(raw), &c, strictjson.Defaults()); err != nil {
		return c, errs.Newf(errs.CodeRecoveryRequired,
			"the pending operation contract is unreadable: %v", err)
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

// validate names the fields reconciliation depends on.
func (c OperationContractV1) validate() error {
	switch {
	case c.BindingVersion != BindingVersion:
		return errs.Newf(errs.CodeRecoveryRequired,
			"the pending operation contract has binding version %d, this build understands %d",
			c.BindingVersion, BindingVersion)
	case c.ItemID == "":
		return errs.New(errs.CodeRecoveryRequired, "the pending operation contract names no item")
	case c.Purpose == "":
		return errs.New(errs.CodeRecoveryRequired, "the pending operation contract names no purpose")
	case c.ObjectName == "":
		return errs.New(errs.CodeRecoveryRequired, "the pending operation contract names no object")
	case c.ExpectedIdent.IsZero():
		return errs.New(errs.CodeRecoveryRequired,
			"the pending operation contract records no expected identity")
	case c.ExpectedToken.Size < 0:
		return errs.New(errs.CodeRecoveryRequired,
			"the pending operation contract records a negative size")
	}
	return nil
}

// ContentDigest returns the recorded content hash, or an error when the contract
// never established one.
//
// A quarantine cannot be completed without it: the whole point of the completion
// is to record evidence that a later purge can compare against, and an empty hash
// compares equal to nothing.
func (c OperationContractV1) ContentDigest() (domain.Digest, error) {
	if c.ContentHash == nil || *c.ContentHash == "" {
		return "", errs.New(errs.CodeRecoveryRequired,
			"the pending operation contract records no content hash")
	}
	return domain.Digest(*c.ContentHash), nil
}

// MetadataDigest returns the recorded extended-metadata hash.
func (c OperationContractV1) MetadataDigest() (string, error) {
	if c.MetadataHash == nil || *c.MetadataHash == "" {
		return "", errs.New(errs.CodeRecoveryRequired,
			"the pending operation contract records no metadata hash")
	}
	return *c.MetadataHash, nil
}
