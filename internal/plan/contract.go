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

// ParseContractFor decodes the contract of one action and checks the fields that
// action cannot be reasoned about without.
//
// The generic check answers "is this record readable". This one answers "is it
// readable *as a restore*": a restore whose contract records no destination
// directory cannot be checked against the directory the operator approved, and
// the old code silently skipped that comparison because the field was optional
// for everyone. A purge, by contrast, has no destination and must not be asked
// for one.
func ParseContractFor(raw, action string) (OperationContractV1, error) {
	c, err := ParseContract(raw)
	if err != nil {
		return c, err
	}
	if err := c.validateForAction(action); err != nil {
		return c, err
	}
	return c, nil
}

// validateForAction names what each action requires beyond the common fields.
func (c OperationContractV1) validateForAction(action string) error {
	var wantNext string
	switch action {
	case "quarantine":
		wantNext = domain.StateQIntent
	case "restore", "compensate":
		wantNext = domain.StateRIntent
	case "purge":
		wantNext = domain.StatePIntent
	default:
		return errs.Newf(errs.CodeRecoveryRequired,
			"no contract shape is defined for operation %q, so it cannot be reconciled", action)
	}
	if c.NextState != wantNext {
		return errs.Newf(errs.CodeRecoveryRequired,
			"the %s contract records next state %q; a %s ends in %q",
			action, c.NextState, action, wantNext)
	}

	switch action {
	case "quarantine":
		// Both hashes are the evidence a later purge compares against. A
		// quarantine cannot be completed without them, so discovering that at
		// completion time - after the file has already moved - is too late.
		if _, err := c.ContentDigest(); err != nil {
			return err
		}
		if _, err := c.MetadataDigest(); err != nil {
			return err
		}
	case "restore", "compensate":
		// The destination is what makes "did the object land where it was
		// approved" answerable. Without it the only remaining answer would be
		// re-derived from the item row, which describes where the object came
		// from.
		if c.DestParentIdentity == nil || c.DestParentIdentity.IsZero() {
			return errs.New(errs.CodeRecoveryRequired,
				"the "+action+" contract records no destination directory, so the place the object was meant to land cannot be checked")
		}
	}
	return nil
}

// validate names the fields reconciliation depends on, whatever the action.
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
