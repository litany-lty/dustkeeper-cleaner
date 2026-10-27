package recovery

import (
	"context"
	"encoding/json"
	"errors"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// Adoption decisions. Each one is a state an operator is allowed to put an
// unreconcilable object into, and each one is reached by running the same
// protocol as the ordinary path for that direction.
const (
	AdoptRestore    = "restore"
	AdoptQuarantine = "quarantine"
)

// AdoptOptions is one explicit operator decision about one item.
type AdoptOptions struct {
	ItemID   domain.ItemID
	Decision string
	// To overrides the restore destination. Empty means the original path.
	To []byte
	// Reason is recorded verbatim. An adoption never happens silently.
	Reason string
	Actor  string
}

// AdoptResult reports what the decision produced.
type AdoptResult struct {
	Item        domain.ItemID      `json:"item_id"`
	Action      string             `json:"action"`
	Decision    string             `json:"decision"`
	NewOpID     domain.OperationID `json:"new_operation_id"`
	PriorOpID   domain.OperationID `json:"prior_operation_id,omitempty"`
	PriorState  string             `json:"prior_state"`
	State       string             `json:"state"`
	Destination string             `json:"destination,omitempty"`
	Detail      string             `json:"detail,omitempty"`
}

// Adopt converges an item that the automatic protocol could not finish.
//
// Two shapes of damage need this. A terminal hold: the operation was closed as a
// verdict, so reconciliation has nothing left to retry, and before this operation
// existed the only exits were a manual file move or a database edit. And an item
// whose pending marker an older build cleared: the file is in the vault, the
// audit trail still names the operation, but the item row no longer does, so
// neither reconciliation nor restore could reach it.
//
// Adoption never rewrites the earlier operation. It starts a new operation that
// names it, so the hold event and its evidence stay exactly as they were and the
// unique-terminal-event constraint is untouched. What an adoption cannot do is
// invent evidence: every decision is made against the object that is actually in
// the vault, compared with the best record available.
func (r *Reconciler) Adopt(ctx context.Context, opt AdoptOptions) (AdoptResult, error) {
	res := AdoptResult{Item: opt.ItemID, Action: "adopt", Decision: opt.Decision}

	if opt.Decision != AdoptRestore && opt.Decision != AdoptQuarantine {
		return res, errs.Newf(errs.CodeInvalidArgument,
			"decision %q is not one of %s, %s", opt.Decision, AdoptRestore, AdoptQuarantine)
	}
	it, err := r.Store.GetItem(ctx, opt.ItemID)
	if err != nil {
		return res, err
	}
	res.PriorState = it.State
	if it.PendingOpID != nil {
		return res, errs.Newf(errs.CodeRecoveryRequired,
			"item %s has an open operation (%s); reconcile it first, adoption is for items the automatic path has finished with",
			it.ID, *it.PendingOpID)
	}
	switch it.State {
	case domain.StateHeld, domain.StateUnknown:
	case domain.StateQuarantined:
		return res, errs.Newf(errs.CodeInvalidArgument,
			"item %s is quarantined; use `restore` or `purge` instead of adoption", it.ID)
	default:
		return res, errs.Newf(errs.CodeInvalidArgument,
			"item %s is %s; adoption only applies to held or unknown items", it.ID, it.State)
	}

	prior, ok, err := r.Store.LastIntentForItem(ctx, it.ID)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, errs.Newf(errs.CodeRecoveryRequired,
			"item %s has no recorded operation; there is nothing to adopt", it.ID)
	}
	res.PriorOpID = prior.OpID

	sc, err := execute.OpenScopeCtx(ctx, r.Registrar, it.ScopeID)
	if err != nil {
		return res, err
	}
	defer sc.Close()

	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return res, errs.Newf(errs.CodeHeld,
			"the object in the vault cannot be opened, so no decision can be made about it: %v", err)
	}
	defer obj.Close()

	if err := r.adoptable(ctx, sc, it, prior, obj); err != nil {
		return res, err
	}

	switch opt.Decision {
	case AdoptRestore:
		return r.adoptRestore(ctx, sc, it, prior, obj, opt, res)
	default:
		return r.adoptQuarantine(ctx, sc, it, prior, obj, opt, res)
	}
}

// adoptable refuses when the object in the vault is not the object the records
// describe. An adoption of an unknown object would put an unidentified file back
// into the tree.
func (r *Reconciler) adoptable(ctx context.Context, sc *execute.ScopeCtx, it store.Item,
	prior store.PendingOp, obj *fsops.File) error {

	if it.CapturedIdentityJSON != nil && *it.CapturedIdentityJSON != "" {
		captured, err := identityOf(*it.CapturedIdentityJSON)
		if err != nil {
			return errs.New(errs.CodeRecoveryRequired,
				"the captured evidence of this item is unreadable, so the object cannot be identified")
		}
		if !obj.Identity.Equal(captured) {
			return errs.New(errs.CodeHeld,
				"the object in the vault is not the object recorded when it was captured")
		}
		return nil
	}
	if contract, err := plan.ParseContract(prior.ContractJSON); err == nil &&
		!contract.ExpectedIdent.IsZero() {
		if !obj.Identity.Equal(contract.ExpectedIdent) {
			return errs.New(errs.CodeHeld,
				"the object in the vault is not the object the operation intended to move")
		}
		return nil
	}
	// Last resort: the manifest written before the move.
	if manifest, err := r.readManifest(sc, it.ObjectName); err == nil {
		if !manifest.ExpectedIdentity.IsZero() && !obj.Identity.Equal(manifest.ExpectedIdentity) {
			return errs.New(errs.CodeHeld,
				"the object in the vault is not the object the manifest describes")
		}
		if manifest.ObjectID == string(it.ID) {
			return nil
		}
	}
	return errs.New(errs.CodeRecoveryRequired,
		"no record identifies the object in the vault, so adopting it would be a guess")
}

// adoptRestore puts the object back, using the restore protocol rather than a
// bare rename: protections, destination-directory identity, both directory syncs
// and the anchor barrier are all part of what "restored" means.
func (r *Reconciler) adoptRestore(ctx context.Context, sc *execute.ScopeCtx, it store.Item,
	prior store.PendingOp, obj *fsops.File, opt AdoptOptions, res AdoptResult) (AdoptResult, error) {

	dest := it.RawRelative
	custom := len(opt.To) > 0
	if custom {
		dest = opt.To
	}
	if _, err := fsops.SplitRelative(dest); err != nil {
		return res, errs.Newf(errs.CodeInvalidArgument, "destination: %v", err)
	}
	destParent, err := sc.Root.OpenParent(dest)
	if err != nil {
		return res, errs.Newf(errs.CodeRestoreConflict,
			"the destination directory does not exist or cannot be verified: %v", err)
	}
	defer destParent.Close()

	destID, _, err := destParent.Stat()
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if !custom && it.SourceParentIdentityJSON != "" {
		storedParent, perr := identityOf(it.SourceParentIdentityJSON)
		if perr != nil {
			return res, errs.New(errs.CodeRecoveryRequired,
				"the recorded parent identity is unreadable; choose an explicit destination")
		}
		if !destID.Equal(storedParent) {
			return res, errs.New(errs.CodeRestoreConflict,
				"the original directory is now a different directory; choose an explicit destination")
		}
	}
	occupied, err := destParent.Exists()
	if err != nil {
		return res, errs.Wrap(errs.CodeAccessDenied, err)
	}
	if occupied {
		return res, errs.Newf(errs.CodeRestoreConflict,
			"a file already exists at the destination; nothing was overwritten")
	}

	nowNS := r.Clock.Wall().UnixNano()
	priorOp := string(prior.OpID)
	contract := plan.OperationContractV1{
		BindingVersion:     plan.BindingVersion,
		ItemID:             string(it.ID),
		PriorState:         it.State,
		NextState:          domain.StateRIntent,
		Purpose:            plan.PurposeAdopt,
		ScopeID:            string(sc.Scope.ID),
		RawRelativeB64:     plan.B64(dest),
		ObjectName:         it.ObjectName,
		ParentIdentity:     decodeOrZeroID(it.SourceParentIdentityJSON),
		DestParentIdentity: &destID,
		ExpectedIdent:      obj.Identity,
		ExpectedToken:      obj.Token,
		PriorOpID:          &priorOp,
		MaxFiles:           r.Exec.Config.MaxFiles,
		MaxLogicalBytes:    r.Exec.Config.MaxBytes,
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	opID := domain.OperationID(domain.NewID("op"))
	res.NewOpID = opID
	intent := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "adopt",
		PriorState:   it.State,
		NextState:    domain.StateRIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: nowNS,
	}
	protections := []store.Protection{{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.Scope.ID,
		RawRelative:  it.RawRelative,
		MatchKind:    domain.MatchExact,
		Source:       domain.ProtectRestore,
		EvidenceJSON: `{"reason":"object adopted back into the tree; it must not be quarantined again","adopted_from":` + quoteJSON(string(prior.OpID)) + `}`,
		CreatedAtNS:  nowNS,
	}}
	if custom && string(dest) != string(it.RawRelative) {
		protections = append(protections, store.Protection{
			ID:           domain.ProtectionID(domain.NewID("prot")),
			ScopeID:      sc.Scope.ID,
			RawRelative:  dest,
			MatchKind:    domain.MatchExact,
			Source:       domain.ProtectRestore,
			EvidenceJSON: `{"reason":"adoption destination must not be quarantined again"}`,
			CreatedAtNS:  nowNS,
		})
	}
	if err := r.Store.RestoreIntent(ctx, intent, protections); err != nil {
		return res, err
	}

	if err := fsops.RenameNoReplace(sc.Objects, it.ObjectName, destParent, destParent.Name()); err != nil {
		if errors.Is(err, fsops.ErrExists) {
			_ = r.Store.AbortIntent(ctx, intent, it.State,
				`{"source":"adopted","result":"destination appeared during adoption"}`)
			return res, errs.New(errs.CodeRestoreConflict,
				"a file appeared at the destination; nothing was overwritten")
		}
		_ = r.Store.AbortIntent(ctx, intent, it.State,
			`{"source":"adopted","result":"rename failed","detail":`+quoteJSON(err.Error())+`}`)
		return res, errs.Newf(errs.CodeIOError, "adoption did not move the object: %v", err)
	}

	back, err := fsops.OpenRegularAt(destParent, sc.RootDev)
	if err != nil {
		return res, r.parkAdopt(ctx, intent, "ADOPT_VERIFY_FAILED", err)
	}
	if !back.Identity.Equal(obj.Identity) {
		_ = back.Close()
		return res, r.parkAdopt(ctx, intent, "ADOPT_IDENTITY_MISMATCH",
			errs.New(errs.CodeHeld, "the object at the destination is not the object that moved"))
	}
	if err := back.Sync(); err != nil {
		_ = back.Close()
		return res, r.parkAdopt(ctx, intent, "DURABILITY_FAILED", err)
	}
	if err := back.Close(); err != nil {
		return res, r.parkAdopt(ctx, intent, "DURABILITY_FAILED", err)
	}
	if err := fsops.DurableDir(destParent); err != nil {
		return res, r.parkAdopt(ctx, intent, "DURABILITY_FAILED", err)
	}
	if err := fsops.DurableDir(sc.Objects); err != nil {
		return res, r.parkAdopt(ctx, intent, "DURABILITY_FAILED", err)
	}
	if sc.AnchorFD() >= 0 {
		if err := fsops.Barrier(sc.AnchorFD(), []byte("adopt")); err != nil {
			return res, r.parkAdopt(ctx, intent, "DURABILITY_FAILED", err)
		}
	}

	result := `{"source":"adopted","decision":"restore","prior_operation_id":` +
		quoteJSON(string(prior.OpID)) + `,"reason":` + quoteJSON(opt.Reason) +
		`,"actor":` + quoteJSON(opt.Actor) + `}`
	if err := r.Store.CompleteRestore(ctx, intent, result); err != nil {
		return res, err
	}
	_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)

	res.State = domain.StateRestored
	res.Destination = string(dest)
	res.Detail = "the object was put back by an explicit decision; the earlier operation keeps its own record"
	return res, nil
}

// adoptQuarantine accepts the object as quarantined.
//
// "Accepted" is not "trusted": the content and metadata are read from the object
// itself, the retention window starts now, and the decision is recorded. An item
// that could never be purged because its evidence was missing gains evidence, not
// an exemption.
func (r *Reconciler) adoptQuarantine(ctx context.Context, sc *execute.ScopeCtx, it store.Item,
	prior store.PendingOp, obj *fsops.File, opt AdoptOptions, res AdoptResult) (AdoptResult, error) {

	if it.BoundRetentionSeconds <= 0 {
		return res, errs.New(errs.CodeHeld,
			"this item has no retention period, so accepting it as quarantined would produce a deadline nobody chose")
	}
	evidence, err := execute.ReadStable(obj, r.Exec.VerifyOpt(obj.Token.Size))
	if err != nil {
		return res, errs.Newf(errs.CodeHeld,
			"the object could not be verified well enough to be accepted as quarantined: %v", err)
	}

	nowNS := r.Clock.Wall().UnixNano()
	purgeNotBefore, ok := domain.AddSecondsNS(nowNS, it.BoundRetentionSeconds)
	if !ok {
		return res, errs.New(errs.CodeHeld, "retention overflow")
	}
	priorOp := string(prior.OpID)
	contract := plan.OperationContractV1{
		BindingVersion: plan.BindingVersion,
		ItemID:         string(it.ID),
		PriorState:     it.State,
		NextState:      domain.StateQIntent,
		Purpose:        plan.PurposeAdopt,
		ScopeID:        string(sc.Scope.ID),
		RawRelativeB64: plan.B64(it.RawRelative),
		ObjectName:     it.ObjectName,
		ParentIdentity: decodeOrZeroID(it.SourceParentIdentityJSON),
		ExpectedIdent:  evidence.Identity,
		ExpectedToken:  evidence.Token,
		ContentHash:    strPtr(string(evidence.Content)),
		MetadataHash:   strPtr(string(evidence.Metadata)),
		PriorOpID:      &priorOp,
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	opID := domain.OperationID(domain.NewID("op"))
	res.NewOpID = opID
	intent := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "adopt",
		PriorState:   it.State,
		NextState:    domain.StateQIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: nowNS,
	}
	if err := r.Store.BeginIntent(ctx, intent); err != nil {
		return res, err
	}
	idJSON, _ := json.Marshal(evidence.Identity)
	tokJSON, _ := json.Marshal(evidence.Token)
	result := `{"source":"adopted","decision":"quarantine","prior_operation_id":` +
		quoteJSON(string(prior.OpID)) + `,"reason":` + quoteJSON(opt.Reason) +
		`,"actor":` + quoteJSON(opt.Actor) + `,"retention_restarted_at_ns":` +
		quoteJSON(itoa(nowNS)) + `}`
	if err := r.Store.CompleteQuarantine(ctx, intent, store.QuarantineResult{
		CapturedIdentityJSON: string(idJSON),
		CapturedTokenJSON:    string(tokJSON),
		QuarantinedAtNS:      nowNS,
		PurgeNotBeforeNS:     purgeNotBefore,
		ContentHash:          string(evidence.Content),
		MetadataHash:         string(evidence.Metadata),
	}, result); err != nil {
		return res, err
	}

	res.State = domain.StateQuarantined
	res.Detail = "the object was accepted as quarantined with fresh evidence and a retention window starting now"
	return res, nil
}

// parkAdopt keeps the adoption open when the filesystem already changed but the
// result could not be proven. The object is never left both moved and unexplained.
func (r *Reconciler) parkAdopt(ctx context.Context, intent store.Intent, code string, cause error) error {
	payload := `{"source":"adopted","result":"parked","hold_code":` + quoteJSON(code) +
		`,"detail":` + quoteJSON(cause.Error()) + `}`
	if err := r.Store.ParkIntent(ctx, intent, code, payload); err != nil {
		return errs.Newf(errs.CodeDurabilityFailed,
			"%s: the move happened but neither the outcome nor the doubt could be recorded: %v", code, err)
	}
	return errs.Newf(errs.CodeRecoveryRequired,
		"%s: the object moved but could not be verified; reconciliation will finish it: %v", code, cause)
}

func decodeOrZeroID(s string) domain.FileIdentity {
	if s == "" {
		return domain.FileIdentity{}
	}
	id, err := identityOf(s)
	if err != nil {
		return domain.FileIdentity{}
	}
	return id
}

func strPtr(s string) *string { return &s }

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [24]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
