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

// deleteDirect destroys one reviewed file.
//
// The shape is deliberately the same as the reversible move, with one extra
// step at the end:
//
//	re-check authority -> verify identity -> stable read -> intent commit ->
//	capture into the vault -> verify the captured object -> unlink it ->
//	flush -> commit
//
// The capture is not ceremony. `unlink` has no "only if the path still names
// inode X" form, so deleting by the original name always leaves a window in
// which the reviewed file has been renamed away and an unreviewed file has
// taken its place: the last check before the unlink cannot close that window,
// it only narrows it. Renaming first empties the path, and what is deleted
// afterwards is a name inside the private vault that the program created for
// this operation. A file that appears at the original path during the window is
// therefore never the thing that gets unlinked.
//
// If the capture itself takes the wrong object, the existing compensation path
// puts it back and the scope is held: the reviewed file is still wherever it
// went, and the replacement is exactly as it was.
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

	// Two reads: one before the capture, one after it. The second is what proves
	// the object the program captured is the object the review approved, and a
	// deletion that cannot prove that is exactly the defect this branch exists to
	// prevent.
	if budget != nil && budget.RemainingRead() < src.Token.Size*2 {
		return res, errs.New(errs.CodeBudgetExhausted,
			"not enough read budget left to verify this file before and after capturing it")
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
	metaHash := preEv.Metadata

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
		MetadataHash:    strPtr(string(metaHash)),
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

	// The last moment at which the path can still be shown to hold the reviewed
	// file. After this the file leaves the path, so anything that appears there
	// is a different object and this operation has no name for it.
	finalID, _, err := parent.StatAtNoFollow()
	if err != nil {
		return res, e.hold(ctx, intent, "SOURCE_UNREADABLE", err)
	}
	if !finalID.Equal(preID) {
		return res, e.hold(ctx, intent, "SOURCE_REPLACED", nil)
	}
	runPreRenameHook()

	if err := fsops.RenameNoReplace(parent, parent.Name(), sc.Objects, it.ObjectName); err != nil {
		if errors.Is(err, fsops.ErrExists) {
			return res, e.hold(ctx, intent, "VAULT_NAME_COLLISION", err)
		}
		if abortErr := e.Store.AbortIntent(ctx, intent, domain.StatePlanned,
			`{"reason":"capture failed","detail":`+quoteJSON(err.Error())+`}`); abortErr != nil {
			return res, abortErr
		}
		return res, errs.Newf(errs.CodeIOError, "the file was not captured, so nothing was deleted: %v", err)
	}

	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return res, e.park(ctx, intent, "CAPTURE_UNREADABLE", err)
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return res, e.park(ctx, intent, "CAPTURE_NOT_REGULAR", err)
	}
	defer obj.Close()

	// What was captured, not what was meant to be captured. A file that replaced
	// the reviewed one between the last check and the rename is now in the
	// vault, and the only acceptable outcome is to put it back.
	if !obj.Identity.Equal(preID) {
		return e.miscapture(ctx, sc, it, intent, obj)
	}

	opt2 := e.VerifyOpt(obj.Token.Size)
	opt2.Budget = budget
	postEv, err := ReadStable(obj, opt2)
	if err != nil {
		return res, e.park(ctx, intent, "CAPTURE_VERIFY_FAILED", err)
	}
	if postEv.Content != preHash {
		// The bytes in the vault are not the bytes the review saw. Nothing is
		// deleted: the object stays in the vault with its evidence, which is a
		// state a person can resolve.
		return res, e.park(ctx, intent, "CONTENT_CHANGED_AFTER_CAPTURE", nil)
	}
	if err := approval.Verify(string(postEv.Content)); err != nil {
		return res, e.park(ctx, intent, "APPROVAL_LOST_AFTER_CAPTURE", err)
	}

	runPreUnlinkHook()
	if err := fsops.UnlinkAt(sc.Objects, it.ObjectName); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return res, e.park(ctx, intent, "CAPTURE_VANISHED_BEFORE_UNLINK", nil)
		}
		return res, e.park(ctx, intent, "UNLINK_FAILED", err)
	}

	// The entry is gone from the vault namespace; now make that durable. A
	// directory flush that fails leaves a deletion a crash could undo, which is
	// recorded as parked rather than as a completed deletion.
	if err := fsops.DurableDir(sc.Objects); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
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
	resultJSON := `{"source":"observed","path_removed":true,"captured_then_unlinked":true,"content_hash":` +
		quoteJSON(string(postEv.Content)) + `,"size_bytes":` + jsonInt(postEv.Token.Size) +
		`,"reason":` + quoteJSON(outcome.Reason) + `}`
	if err := e.Store.CompletePurge(ctx, intent, resultJSON); err != nil {
		return res, err
	}
	if budget != nil {
		budget.AddAction(postEv.Token.Size)
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
