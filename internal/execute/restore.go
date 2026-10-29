package execute

import (
	"context"
	"encoding/json"
	"errors"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// RestoreOptions describes one restore request.
type RestoreOptions struct {
	// DestinationRelative is empty to restore to the original path.
	DestinationRelative []byte
}

// Restore returns one quarantined object to a verified location without ever
// overwriting anything, and protects both the original and the actual path.
//
// Restoring is allowed regardless of whether the rule that moved the file is
// still enabled, but it must still satisfy root/vault identity and the
// no-overwrite conditions.
func (e *Executor) Restore(ctx context.Context, sc *ScopeCtx, itemID domain.ItemID, opt RestoreOptions) (domain.OperationResult, error) {
	res := domain.OperationResult{Item: itemID}

	it, err := e.Store.GetItem(ctx, itemID)
	if err != nil {
		return res, err
	}
	if it.State != domain.StateQuarantined && it.State != domain.StateHeld {
		return res, errs.Newf(errs.CodePlanStale, "item %s is %s, not quarantined", it.ID, it.State)
	}
	if it.CapturedIdentityJSON == nil || it.CapturedTokenJSON == nil {
		return res, errs.New(errs.CodeHeld,
			"this object has no recorded post-move identity; reconcile before restoring")
	}

	capturedID, err := decodeID(*it.CapturedIdentityJSON)
	if err != nil {
		return res, err
	}

	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return res, errs.Newf(errs.CodeHeld,
			"the quarantined object is not readable: %v; reconcile before restoring", err)
	}
	defer obj.Close()
	if !obj.Identity.Equal(capturedID) {
		return res, errs.New(errs.CodeHeld,
			"the quarantined object is not the object that was recorded; refusing to restore")
	}

	destRel := it.RawRelative
	custom := false
	if len(opt.DestinationRelative) > 0 {
		destRel = opt.DestinationRelative
		custom = true
	}
	if _, err := fsops.SplitRelative(destRel); err != nil {
		return res, errs.Newf(errs.CodeInvalidArgument, "destination: %v", err)
	}

	destParent, err := sc.Root.OpenParent(destRel)
	if err != nil {
		// One phase-one rule: the parent must already exist. No directory tree
		// is created on the user's behalf.
		return res, errs.Newf(errs.CodeRestoreConflict,
			"the destination directory does not exist or cannot be verified: %v", err)
	}
	defer destParent.Close()

	if !custom {
		storedParent, err := decodeID(it.SourceParentIdentityJSON)
		if err != nil {
			return res, err
		}
		nowParent, _, err := destParent.Stat()
		if err != nil {
			return res, errs.Wrap(errs.CodeAccessDenied, err)
		}
		if !nowParent.Equal(storedParent) {
			return res, errs.New(errs.CodeRestoreConflict,
				"the original parent directory is now a different directory; choose an explicit destination")
		}
	}

	exists, err := destParent.Exists()
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if exists {
		// The object stays in the vault. The user may pick another destination.
		return res, errs.Newf(errs.CodeRestoreConflict,
			"a file already exists at the restore destination; nothing was overwritten (object %s is still quarantined)", it.ID)
	}

	nowNS := e.Clock.Wall().UnixNano()
	// The destination directory is recorded, not re-derived later: after a crash
	// reconciliation has to decide whether the object landed where it was meant
	// to, and "where it was meant to" is a fact about this instant.
	destParentID, _, perr := destParent.Stat()
	if perr != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, perr)
	}
	protections := []store.Protection{{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.Scope.ID,
		RawRelative:  it.RawRelative,
		MatchKind:    domain.MatchExact,
		Source:       domain.ProtectRestore,
		EvidenceJSON: `{"reason":"restored object must not be quarantined again"}`,
		CreatedAtNS:  nowNS,
	}}
	if custom && string(destRel) != string(it.RawRelative) {
		protections = append(protections, store.Protection{
			ID:           domain.ProtectionID(domain.NewID("prot")),
			ScopeID:      sc.Scope.ID,
			RawRelative:  destRel,
			MatchKind:    domain.MatchExact,
			Source:       domain.ProtectRestore,
			EvidenceJSON: `{"reason":"restore destination must not be quarantined again"}`,
			CreatedAtNS:  nowNS,
		})
	}

	opID := domain.OperationID(domain.NewID("op"))
	var sourceParentID domain.FileIdentity
	if it.SourceParentIdentityJSON != "" {
		if id, ierr := decodeID(it.SourceParentIdentityJSON); ierr == nil {
			sourceParentID = id
		}
	}
	contract := plan.OperationContractV1{
		BindingVersion:     plan.BindingVersion,
		ItemID:             string(it.ID),
		PriorState:         it.State,
		NextState:          domain.StateRIntent,
		Purpose:            plan.PurposeNormal,
		ScopeID:            string(sc.Scope.ID),
		RawRelativeB64:     plan.B64(destRel),
		ObjectName:         it.ObjectName,
		ParentIdentity:     sourceParentID,
		DestParentIdentity: &destParentID,
		ExpectedIdent:      capturedID,
		MaxFiles:           e.Config.MaxFiles,
		MaxLogicalBytes:    e.Config.MaxBytes,
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	intent := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "restore",
		PriorState:   it.State,
		NextState:    domain.StateRIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: nowNS,
	}

	// The protections are written in the same transaction as the intent, so a
	// crash cannot leave a restored file unprotected.
	if err := e.Store.RestoreIntent(ctx, intent, protections); err != nil {
		return res, err
	}

	if err := fsops.RenameNoReplace(sc.Objects, it.ObjectName, destParent, destParent.Name()); err != nil {
		if errors.Is(err, fsops.ErrExists) {
			aerr := e.Store.AbortIntent(ctx, intent, it.State,
				`{"reason":"destination appeared during restore","source":"observed"}`)
			return res, recordOr(aerr, errs.Newf(errs.CodeRestoreConflict,
				"a file appeared at the destination; nothing was overwritten (object %s is still quarantined)", it.ID))
		}
		aerr := e.Store.AbortIntent(ctx, intent, it.State,
			`{"reason":"rename failed","detail":`+quoteJSON(err.Error())+`}`)
		return res, recordOr(aerr, errs.Newf(errs.CodeIOError, "restore did not move the object: %v", err))
	}

	// Re-open what actually landed. A failure here used to be ignored, which
	// meant a restore could be recorded as complete without ever proving that
	// the object at the destination is the object that moved.
	back, err := fsops.OpenRegularAt(destParent, sc.RootDev)
	if err != nil {
		return res, e.park(ctx, intent, "RESTORE_VERIFY_FAILED", err)
	}
	if !back.Identity.Equal(capturedID) {
		_ = back.Close()
		herr := e.Store.HoldIntent(ctx, intent, "RESTORE_IDENTITY_MISMATCH",
			`{"source":"observed","result":"restored object identity differs from the record"}`)
		return res, recordOr(herr, errs.New(errs.CodeHeld,
			"the restored object is not the recorded object; the scope is held"))
	}
	if err := back.Sync(); err != nil {
		_ = back.Close()
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if err := back.Close(); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if err := fsops.DurableDir(destParent); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if err := fsops.DurableDir(sc.Objects); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if sc.AnchorFD() >= 0 {
		if err := fsops.Barrier(sc.AnchorFD(), []byte("restore")); err != nil {
			return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
		}
	}

	// The scope was held when this object's outcome could not be proven; that
	// reason is gone now that the object is back and its record says so. The
	// item's terminal record and the scope's transition commit together, so a
	// failure cannot leave a restored file behind a scope nobody can resume.
	if err := e.Store.CompleteRestoreReleasingHold(ctx, intent, `{"source":"observed"}`, it.ScopeID); err != nil {
		return res, err
	}
	if err := e.Store.ReleaseItemKeeperPin(ctx, it.ID); err != nil {
		return res, err
	}

	res.State = domain.StateRestored
	res.Code = string(errs.CodeOK)
	res.Operation = opID
	return res, nil
}

// RestoreDestinationPreview reports where a restore would land, without touching
// anything. The CLI uses it to explain a conflict before the user retries.
func (e *Executor) RestoreDestinationPreview(ctx context.Context, sc *ScopeCtx, itemID domain.ItemID, destRelative []byte) (string, bool, error) {
	it, err := e.Store.GetItem(ctx, itemID)
	if err != nil {
		return "", false, err
	}
	destRel := it.RawRelative
	if len(destRelative) > 0 {
		destRel = destRelative
	}
	parent, err := sc.Root.OpenParent(destRel)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return string(destRel), false, nil
		}
		return "", false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer parent.Close()
	occupied, err := parent.Exists()
	if err != nil {
		return "", false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	return string(destRel), occupied, nil
}
