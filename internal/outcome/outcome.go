// Package outcome maps a reviewed candidate onto exactly one of the product's
// three results.
//
// It is a separate package for one structural reason: both the review client
// and the cleanup policy have to agree on what those three results mean, and
// neither may depend on the other. Everything here imports only domain, so the
// mapping can be exhaustively tested with no filesystem, database or network.
package outcome

import "dustkeeper/internal/domain"

// Keep reasons. Every one of them is a different promise to the user, so they
// are never collapsed into a single "skipped": "the model service is down" and
// "the models say this is still needed" are not the same fact.
const (
	// ReasonReviewUnavailable — the review could not be completed (network,
	// credentials, malformed answer). Nothing was moved or removed.
	ReasonReviewUnavailable = "review_unavailable"
	// ReasonFactsMissing — a fact the decision needs could not be measured. The
	// program refuses to let a model fill the gap.
	ReasonFactsMissing = "facts_missing"
	// ReasonModelSaysKeep — at least one reviewer said the item must stay.
	ReasonModelSaysKeep = "model_says_keep"
	// ReasonMoveSafetyUnknown — nothing established that moving the file is
	// safe, so it stays.
	ReasonMoveSafetyUnknown = "move_safety_unknown"
	// ReasonNoAuthority — the item qualifies for nothing this build is allowed
	// to do: no direct-delete category, no grant, or a scope outside the
	// confirmation.
	ReasonNoAuthority = "no_authority"
	// ReasonNotEnoughForDeletion — the facts do not justify permanent deletion,
	// but moving is safe, so the item is observed instead.
	ReasonNotEnoughForDeletion = "not_enough_for_deletion"
	// ReasonSafeToObserve — both reviewers allowed reversible isolation.
	ReasonSafeToObserve = "safe_to_observe"
	// ReasonDualReviewAgrees — both reviewers allowed permanent deletion.
	ReasonDualReviewAgrees = "dual_review_agrees"
)

// ReasonText renders a reason for the result page. It is presentation, but it
// lives next to the codes so that a new code cannot be added without a sentence
// a user can read.
func ReasonText(reason string) string {
	switch reason {
	case ReasonReviewUnavailable:
		return "模型服务不可用，本轮未处理"
	case ReasonFactsMissing:
		return "缺少判断所需的事实，未处理"
	case ReasonModelSaysKeep:
		return "模型明确要求保留"
	case ReasonMoveSafetyUnknown:
		return "无法确认移动是否安全"
	case ReasonNoAuthority:
		return "不在已批准的清理类别或范围内"
	case ReasonNotEnoughForDeletion:
		return "不足以永久删除，进入观察区"
	case ReasonSafeToObserve:
		return "可安全移动，进入观察区"
	case ReasonDualReviewAgrees:
		return "双模型同意永久删除"
	default:
		return reason
	}
}

// Inputs is everything the decision uses. Local facts decide whether an action
// can be taken at all; the models can only veto or authorise.
type Inputs struct {
	// Action is what the reviewers were asked about:
	// domain.DispositionDelete or domain.DispositionQuarantine.
	Action string
	// Verdict is the normalised answer (domain.VerdictAllow and friends).
	Verdict string
	// ModelFailed reports that the review did not complete.
	ModelFailed bool
	// FactsComplete reports that every fact the decision needs was measured.
	FactsComplete bool
	// Movable reports that local checks established reversible isolation is
	// safe for this object (regular file, single link, not protected, not in
	// use, parent unchanged, space available).
	Movable bool
	// Authorized reports that permanent deletion is allowed for this item:
	// category in the confirmed set, an active auto_purge grant bound to the
	// current rule revision, and a scope inside the confirmation.
	Authorized bool
}

// Decide maps the reviewed state onto exactly one disposition.
//
// Order matters and is the safety argument:
//
//  1. a failed review is never an approval;
//  2. missing facts are never filled in by a model;
//  3. an explicit keep from either reviewer is final;
//  4. an object that cannot be proven movable stays put;
//  5. permanent deletion needs both reviewers AND standing authority;
//  6. everything else that is movable is observed, not deleted.
func Decide(in Inputs) (disposition, reason string) {
	switch {
	case in.ModelFailed:
		return domain.DispositionKeep, ReasonReviewUnavailable
	case !in.FactsComplete:
		return domain.DispositionKeep, ReasonFactsMissing
	case in.Verdict == domain.VerdictKeep:
		return domain.DispositionKeep, ReasonModelSaysKeep
	case !in.Movable:
		return domain.DispositionKeep, ReasonMoveSafetyUnknown
	}

	switch in.Action {
	case domain.DispositionDelete, domain.ActionPurge:
		// A purge is the same irreversible outcome as a direct deletion; the
		// only difference is that the bytes have been in the observation area
		// first, so the observation period is the extra check, not a substitute
		// for the authority.
		if !in.Authorized {
			return domain.DispositionKeep, ReasonNoAuthority
		}
		switch in.Verdict {
		case domain.VerdictAllow:
			return domain.DispositionDelete, ReasonDualReviewAgrees
		case domain.VerdictInsufficient:
			// Not enough for deletion is exactly the case the observation area
			// exists for: the item may still be moved safely.
			return domain.DispositionQuarantine, ReasonNotEnoughForDeletion
		}
	case domain.DispositionQuarantine:
		if in.Verdict == domain.VerdictAllow {
			return domain.DispositionQuarantine, ReasonSafeToObserve
		}
		// The reviewers were asked whether moving is safe. "Insufficient" there
		// means movement itself is unproven, which is the one case that must
		// stay put.
		return domain.DispositionKeep, ReasonMoveSafetyUnknown
	}
	return domain.DispositionKeep, ReasonNoAuthority
}
