package execute

import (
	"context"
	"encoding/json"
	"errors"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// PurposeDirectDelete marks a contract whose action removed the file at its
// original path rather than moving it into the vault.
const PurposeDirectDelete = "direct_delete"

// deleteDirect removes one file at its original path.
//
// This is the only place in the product where a byte is destroyed without a
// copy existing first, so the order is stricter than the move's:
//
//	re-check authority -> verify identity -> stable read -> intent commit ->
//	verify unchanged -> unlink -> flush directory -> commit
//
// Every step before the unlink can refuse and leave the file exactly where it
// was. The steps after it cannot be undone, so a failure there is recorded as
// parked (still explainable, still unfinished) rather than as a verdict.
func (e *Executor) deleteDirect(ctx context.Context, sc *ScopeCtx, p store.Plan, it store.Item,
	binding plan.ItemBindingV1, budget *domain.Budget, outcome review.Outcome) (domain.OperationResult, error) {

	res := domain.OperationResult{Item: it.ID, State: it.State}
	approval := outcome.Approval
	if approval == nil {
		return res, errs.New(errs.CodeReviewRequired, "a review allowed deletion but carries no approval")
	}

	// The authority that made this branch reachable is re-read here. Between the
	// review and this call a user may have revoked the grant or narrowed the
	// policy, and "it was allowed a moment ago" is not a permission.
	pol, err := cleanupPolicyOf(ctx, e, sc, it, binding)
	if err != nil {
		return res, err
	}
	if !pol {
		return res, errs.New(errs.CodeGrantRequired,
			"the authority for permanent deletion is no longer in place; nothing was deleted")
	}
	if err := e.validateItem(sc, p, it, binding); err != nil {
		return res, err
	}
	if err := e.requireQuarantineTrust(ctx); err != nil {
		return res, err
	}
	if err := e.scopeGate(ctx, sc); err != nil {
		return res, err
	}

	preID, err := decodeID(it.PreIdentityJSON)
	if err != nil {
		return res, err
	}
	preTok, err := decodeTok(it.PreTokenJSON)
	if err != nil {
		return res, err
	}
	if ok, why, perr := e.protected(ctx, sc, it.RawRelative, preID); perr != nil {
		return res, perr
	} else if ok {
		return skipped(res, why), nil
	}

	parent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return res, errs.Newf(errs.CodePlanStale, "%q no longer exists", it.RawRelative)
		}
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer parent.Close()

	parentID, _, err := parent.Stat()
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	storedParent, err := decodeID(it.SourceParentIdentityJSON)
	if err != nil {
		return res, err
	}
	if !parentID.Equal(storedParent) {
		return res, errs.New(errs.CodePlanStale,
			"the parent directory was replaced after planning; this plan is not safe to apply")
	}

	src, err := fsops.OpenRegularAt(parent, sc.RootDev)
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer src.Close()

	if ok, why := sameStableObject(src.Identity, src.Token, preID, preTok); !ok {
		return res, errs.Newf(errs.CodePlanStale,
			"%q changed after planning (%s); plan again", it.RawRelative, why)
	}
	if src.Token.NLink > 1 {
		return skipped(res, "file has more than one hard link"), nil
	}

	// One read, not two: the file will not exist afterwards, so there is no
	// second verification to reserve budget for.
	if budget != nil && budget.RemainingRead() < src.Token.Size {
		return res, errs.New(errs.CodeBudgetExhausted,
			"not enough read budget left to verify this file before deleting it")
	}
	if budget != nil && !budget.CanReserve(src.Token.Size) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the deletion allowance for this run does not permit another action")
	}
	if budget != nil && budget.TimeUp(e.Clock) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the run's time slice was spent before this file was read")
	}

	opt := e.VerifyOpt(src.Token.Size)
	opt.Budget = budget
	runBeforeStableReadHook()
	preEv, err := ReadStable(src, opt)
	if err != nil {
		if errors.Is(err, ErrUnstableObject) {
			return res, errs.Newf(errs.CodePlanStale,
				"%q changed while it was being verified; plan again", it.RawRelative)
		}
		return res, err
	}
	if ok, why := sameStableObject(preEv.Identity, preEv.Token, preID, preTok); !ok {
		return res, errs.Newf(errs.CodePlanStale,
			"%q changed before it could be accepted (%s); nothing was deleted", it.RawRelative, why)
	}
	preHash := preEv.Content

	opID := domain.OperationID(domain.NewID("op"))
	contract := plan.OperationContractV1{
		BindingVersion:  plan.BindingVersion,
		ItemID:          string(it.ID),
		ExecutionPlanID: strPtr(string(p.ID)),
		PriorState:      domain.StatePlanned,
		NextState:       domain.StatePIntent,
		Purpose:         PurposeDirectDelete,
		ScopeID:         string(sc.Scope.ID),
		RawRelativeB64:  plan.B64(it.RawRelative),
		ObjectName:      it.ObjectName,
		ParentIdentity:  parentID,
		ExpectedIdent:   preID,
		ExpectedToken:   preTok,
		ContentHash:     strPtr(string(preHash)),
		MetadataHash:    strPtr(string(preEv.Metadata)),
		Binding:         binding,
		MaxFiles:        e.Config.MaxFiles,
		MaxLogicalBytes: e.Config.MaxBytes,
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	intent := store.Intent{
		OpID:              opID,
		ItemID:            it.ID,
		PlanID:            &p.ID,
		Action:            domain.ActionDelete,
		Review:            approval,
		ReviewContentHash: string(preHash),
		PriorState:        domain.StatePlanned,
		NextState:         domain.StatePIntent,
		ContractJSON:      string(contractJSON),
		OccurredAtNS:      e.Clock.Wall().UnixNano(),
	}

	// Last look before the intent commits. Nothing has been removed, so a refusal
	// here costs this item only.
	if err := e.validateItem(sc, p, it, binding); err != nil {
		return res, err
	}
	if err := approval.Verify(string(preHash)); err != nil {
		return res, err
	}
	if budget != nil && budget.TimeUp(e.Clock) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the run's time slice ran out before this file could be accepted; nothing was deleted")
	}
	runBeforeAcceptanceHook()
	if err := ConfirmUnchanged(src, preEv); err != nil {
		return res, changedAfterVerification(it.RawRelative, err)
	}
	if ok, why, perr := e.protected(ctx, sc, it.RawRelative, preID); perr != nil {
		return res, perr
	} else if ok {
		return skipped(res, why), nil
	}
	if err := e.Gate.CheckAndCommit(func() error {
		if err := e.scopeGate(ctx, sc); err != nil {
			return err
		}
		return e.Store.BeginIntent(ctx, intent)
	}); err != nil {
		return res, err
	}

	// The last moment at which the file can still be proven to be the one the
	// review and the plan described. unlink has no "only if still inode X", so
	// this narrows the window without closing it.
	finalID, _, err := parent.StatAtNoFollow()
	if err != nil {
		return res, e.hold(ctx, intent, "SOURCE_UNREADABLE", err)
	}
	if !finalID.Equal(preID) {
		return res, e.hold(ctx, intent, "SOURCE_REPLACED", nil)
	}
	// The descriptor is still open, so the bytes cannot have been freed by a
	// concurrent writer; identity plus the open handle is the strongest
	// statement available at this point.
	if ok, why := sameStableObject(src.Identity, src.Token, preID, preTok); !ok {
		return res, e.hold(ctx, intent, "SOURCE_REPLACED", errs.Newf(errs.CodePlanStale, "%s", why))
	}
	runPreUnlinkHook()

	if err := fsops.UnlinkAt(parent, parent.Name()); err != nil {
		if errors.Is(err, unix.ENOENT) {
			// The name is gone but this operation did not observe the removal.
			// Saying "deleted" would claim credit for someone else's unlink, and
			// saying "nothing happened" would be false. Parked is the honest
			// state: the path holds nothing and reconciliation confirms it.
			return res, e.park(ctx, intent, "SOURCE_VANISHED_BEFORE_UNLINK", nil)
		}
		// The file is untouched, so this attempt can be closed without a removal.
		if abortErr := e.Store.AbortIntent(ctx, intent, domain.StateAborted,
			`{"reason":"unlink failed","detail":`+quoteJSON(err.Error())+`}`); abortErr != nil {
			return res, abortErr
		}
		return res, errs.Newf(errs.CodeIOError, "the file was not deleted: %v", err)
	}

	// The entry is gone from the namespace; now make that durable. A directory
	// flush that fails leaves a deletion that a crash could undo, which is
	// recorded as parked rather than as a completed deletion.
	if err := fsops.DurableDir(parent); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if sc.AnchorFD() >= 0 {
		if err := fsops.Barrier(sc.AnchorFD(), []byte("delete")); err != nil {
			return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
		}
	}

	if err := runBeforeCompleteHook(domain.ActionDelete); err != nil {
		return res, errs.Newf(errs.CodeStateWriteFailed,
			"the file is deleted but its record could not be closed: %v; the operation stays pending for `recover reconcile`", err)
	}
	resultJSON := `{"source":"observed","path_removed":true,"content_hash":` + quoteJSON(string(preHash)) +
		`,"size_bytes":` + jsonInt(src.Token.Size) + `,"reason":` + quoteJSON(outcome.Reason) + `}`
	if err := e.Store.CompletePurge(ctx, intent, resultJSON); err != nil {
		return res, err
	}
	if budget != nil {
		budget.AddAction(src.Token.Size)
	}

	res.State = domain.StatePurged
	res.Code = string(errs.CodeOK)
	res.Control = domain.ControlContinue
	res.Evidence = domain.EvidencePersisted
	res.Operation = opID
	return res, nil
}

// cleanupPolicyOf answers "may permanent deletion happen for this item, right
// now" using the same four conditions the review was told about.
func cleanupPolicyOf(ctx context.Context, e *Executor, sc *ScopeCtx, it store.Item, b plan.ItemBindingV1) (bool, error) {
	pol, err := cleanup.Load(ctx, e.Store)
	if err != nil {
		return false, err
	}
	_, authorized, _ := e.authorityFor(ctx, sc, it, b, pol, cleanup.CategoryOf(string(it.RawRelative)))
	return authorized, nil
}

// jsonInt renders an int64 without going through float formatting.
func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
