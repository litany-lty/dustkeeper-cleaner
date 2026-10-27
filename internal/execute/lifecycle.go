package execute

import (
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// requireLifecycleItem refuses a new quarantine or purge of an object that the
// simplified product does not cover.
//
// The automatic duplicate pipeline is gone from the call graph, but the rows it
// created are still in the database: a plan can be loaded by id through the
// generic `plan apply` route, and a stored item can be handed to the executor
// directly. Deleting the detector is therefore not enough. Removing the keeper
// verifier without this check would make an old duplicate *easier* to purge than
// it was before - the object would be unlinked with nobody having proved the
// surviving copy still exists - so the refusal belongs here, in the shared path,
// and not only in the CLI that no longer offers the command.
//
// Restore is deliberately not checked: getting a file back is not a new
// destructive action, and the historical objects are exactly the ones an operator
// may still need to recover.
func requireLifecycleItem(it store.Item) error {
	if it.Reason != domain.ReasonLifecycle {
		return errs.Newf(errs.CodePlanStale,
			"item %s is a %s object; this build quarantines and purges lifecycle objects only",
			it.ID, describeReason(it.Reason))
	}
	switch it.SourceKind {
	case domain.SourceManualRule, domain.SourceScheduledRule:
	default:
		return errs.Newf(errs.CodePlanStale,
			"item %s was produced by %q, which this build no longer creates or applies", it.ID, it.SourceKind)
	}
	// A lifecycle row that still carries duplicate-keeper evidence is a mixed
	// record: two products wrote to it and neither the keeper proof nor the rule
	// binding can be trusted for it. It is refused rather than resolved.
	if it.KeeperJSON != nil || it.KeeperScopeID != nil || it.KeeperPinActive {
		return errs.Newf(errs.CodePlanStale,
			"item %s carries duplicate-keeper evidence; it is a historical object and cannot be quarantined or purged by this build", it.ID)
	}
	return nil
}

// requireLifecycleMembers applies the same rule to every member before the first
// one is touched.
//
// Whole-plan refusal is the point: "apply the legal members, stop when a duplicate
// turns up" would leave a half-applied plan whose status nobody can read and would
// run the very delete the check exists to prevent.
func requireLifecycleMembers(items []store.Item) error {
	for _, it := range items {
		if err := requireLifecycleItem(it); err != nil {
			return err
		}
	}
	return nil
}

// purgeMemberItems flattens a purge member list into the items the shared check
// takes. The member row is the join; the item is what carries reason and source.
func purgeMemberItems(members []store.PurgeMemberRow) []store.Item {
	items := make([]store.Item, 0, len(members))
	for _, m := range members {
		items = append(items, m.Item)
	}
	return items
}

func describeReason(reason string) string {
	if reason == "" {
		return "reason-less"
	}
	return reason
}
