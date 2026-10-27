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
	"dustkeeper/internal/policy"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Executor performs quarantine, restore and purge.
type Executor struct {
	Store  *store.Store
	Clock  domain.Clock
	Config ConfigView
	Gate   scope.PauseGate
	Guard  *policy.Guard
	// Keeper verifies that a duplicate's surviving copy is still intact. It is
	// an interface so that duplicate detection stays out of this package.
	Keeper KeeperVerifier
	// TmpBuf is reused across reads so a large plan does not allocate per item.
	TmpBuf []byte
}

// ConfigView is the subset of configuration the executor needs.
type ConfigView struct {
	BufferBytes    int
	ReadRatePerSec int64
	MaxFiles       int
	MaxBytes       int64
	MinFreeBytes   int64
	MinFreePercent int
}

// VerifyOpt builds the I/O options for one item.
func (e *Executor) VerifyOpt(bufSize int64) VerifyOptions {
	buf := e.TmpBuf
	if len(buf) == 0 {
		n := e.Config.BufferBytes
		if n <= 0 {
			n = 1 << 20
		}
		buf = make([]byte, n)
	}
	return VerifyOptions{Buffer: buf, RatePerSec: e.Config.ReadRatePerSec}
}

// validateItem checks that the plan and its item still describe the world.
// Expiry and binding are re-checked before every item, as section 6.5 requires.
func (e *Executor) validateItem(sc *ScopeCtx, p store.Plan, it store.Item, b plan.ItemBindingV1) error {
	nowNS := e.Clock.Wall().UnixNano()
	if nowNS > p.ExpiresAtNS {
		return errs.Newf(errs.CodePlanExpired, "plan %s expired; plan again", p.ID)
	}
	if p.Status != domain.PlanReady && p.Status != domain.PlanApplying {
		return errs.Newf(errs.CodePlanStale, "plan %s is %s", p.ID, p.Status)
	}
	if b.ScopeGeneration != sc.Scope.Generation {
		return errs.Newf(errs.CodeScopeChanged,
			"scope %s was re-registered (generation %d -> %d)", sc.Scope.ID, b.ScopeGeneration, sc.Scope.Generation)
	}
	if b.ProtectionRevision != sc.Scope.ProtectionRevision {
		return errs.New(errs.CodePlanStale,
			"the protection set changed after this plan was approved; preview it again")
	}
	if b.ScopeID != string(sc.Scope.ID) {
		return errs.New(errs.CodePlanStale, "this item belongs to a different scope")
	}
	return nil
}

// Quarantine moves one item into the private vault.
//
// The order is load-bearing:
//
//	verify -> hash -> manifest durable -> intent commit -> rename -> verify -> sync -> commit
//
// Any interruption leaves the file explainable: either it is still where the
// plan says, or the vault holds an object whose identity and content hash were
// recorded before the move.
func (e *Executor) Quarantine(ctx context.Context, sc *ScopeCtx, p store.Plan, it store.Item, budget *domain.Budget) (domain.OperationResult, error) {
	res := domain.OperationResult{Item: it.ID, State: it.State}

	binding, err := PlanBindingOf(it)
	if err != nil {
		return res, err
	}
	if err := e.validateItem(sc, p, it, binding); err != nil {
		return res, err
	}
	// A paused or held scope accepts no new business operation. The plan may
	// predate the pause, so this is checked per item rather than per batch.
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

	// One protection answer for scan, plan, quarantine and purge. Subtree
	// protections and project trees are part of it; checking only an exact path
	// match let a subtree protection added after a scan be bypassed by simply
	// creating a new plan.
	if ok, why, perr := e.protected(ctx, sc, it.RawRelative, preID); perr != nil {
		return res, perr
	} else if ok {
		res.Code = string(errs.CodeSkippedProtected)
		res.Message = why
		res.Control = domain.ControlContinue
		res.Evidence = domain.EvidenceNotStarted
		return res, nil
	}

	// Section 9.5: a path that some isolated object still depends on as its
	// keeper must not be quarantined, or that dependency would point at nothing
	// and the surviving copy would be the one that moved.
	if inUse, err := e.Store.KeeperInUse(ctx, sc.Scope.ID, it.RawRelative); err != nil {
		return res, err
	} else if inUse {
		res.Code = string(errs.CodeSkippedProtected)
		res.Message = "this file is the keeper of an isolated duplicate; restore or purge that object first"
		return res, nil
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

	if ok, why := sameIdentityAndToken(src.Identity, src.Token, preID, preTok); !ok {
		return res, errs.Newf(errs.CodePlanStale,
			"%q changed after planning (%s); plan again", it.RawRelative, why)
	}
	if src.Token.NLink > 1 {
		res.Code = string(errs.CodeSkippedProtected)
		res.Message = "file has more than one hard link"
		return res, nil
	}

	// Reserve both reads before starting: one hash now, one verification after
	// the move. Discovering the shortfall mid-read would leave work undone.
	if budget != nil && budget.RemainingRead() < src.Token.Size*2 {
		return res, errs.New(errs.CodeBudgetExhausted,
			"not enough read budget left to verify this file twice")
	}
	// The action allowance is checked before the content read, not after it. A
	// batch that is not allowed to move anything must not hash anything either.
	if budget != nil && !budget.CanReserve(src.Token.Size) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the quarantine allowance for this run does not permit another action")
	}
	if budget != nil && budget.TimeUp(e.Clock) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the run's time slice was spent before this file was read")
	}

	opt := e.VerifyOpt(src.Token.Size)
	opt.Budget = budget
	preEv, err := ReadStable(src, opt)
	if err != nil {
		if errors.Is(err, ErrUnstableObject) {
			// Nothing has moved yet, so this attempt can be closed cleanly.
			return res, errs.Newf(errs.CodePlanStale,
				"%q changed while it was being verified; plan again", it.RawRelative)
		}
		return res, err
	}
	// Checking the deadline again here, before the manifest is written, keeps the
	// abandonment clean: nothing durable exists yet, so there is no orphan
	// manifest for the next run to reconcile.
	if budget != nil && budget.TimeUp(e.Clock) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the run's time slice ran out while this file was being verified; nothing was moved")
	}
	preHash := preEv.Content
	metaHash := preEv.Metadata

	manifest := ManifestV1{
		FormatVersion:    1,
		InstanceID:       sc.Scope.InstanceID,
		ScopeID:          string(sc.Scope.ID),
		ObjectID:         string(it.ID),
		RawRelativeB64:   plan.B64(it.RawRelative),
		ParentIdentity:   parentID,
		ExpectedIdentity: preID,
		ContentHash:      string(preHash),
		MetadataHash:     string(metaHash),
		RuleDigest:       binding.RuleDigest,
		PlanDigest:       string(p.Digest),
		CreatedAtNS:      e.Clock.Wall().UnixNano(),
	}
	if err := sc.WriteManifest(manifest); err != nil {
		return res, err
	}

	opID := domain.OperationID(domain.NewID("op"))
	contract := plan.OperationContractV1{
		BindingVersion:  plan.BindingVersion,
		ItemID:          string(it.ID),
		ExecutionPlanID: strPtr(string(p.ID)),
		PriorState:      domain.StatePlanned,
		NextState:       domain.StateQIntent,
		Purpose:         plan.PurposeNormal,
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
		OpID:         opID,
		ItemID:       it.ID,
		PlanID:       &p.ID,
		Action:       "quarantine",
		PriorState:   domain.StatePlanned,
		NextState:    domain.StateQIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: e.Clock.Wall().UnixNano(),
	}

	// The pause check and the intent commit share one critical section, so a
	// pause either lands before this item is accepted or after it converges.
	if err := e.Gate.CheckAndCommit(func() error { return e.Store.BeginIntent(ctx, intent) }); err != nil {
		return res, err
	}

	// Last look before the rename. This narrows the window described in 2.4 but
	// cannot close it: rename has no "only if still inode X" parameter.
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
		// The source is untouched, so this attempt can be closed without a move.
		if abortErr := e.Store.AbortIntent(ctx, intent, domain.StateAborted,
			`{"reason":"rename failed","detail":`+quoteJSON(err.Error())+`}`); abortErr != nil {
			return res, abortErr
		}
		return res, errs.Newf(errs.CodeIOError, "the file was not moved: %v", err)
	}

	// Verify what actually moved, not what we intended to move.
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

	if !obj.Identity.Equal(preID) {
		return e.miscapture(ctx, sc, it, intent, obj)
	}

	opt2 := e.VerifyOpt(obj.Token.Size)
	opt2.Budget = budget
	postEv, err := ReadStable(obj, opt2)
	if err != nil {
		return res, e.park(ctx, intent, "POST_VERIFY_READ_FAILED", err)
	}
	if postEv.Content != preHash {
		return res, e.park(ctx, intent, "CONTENT_CHANGED_AFTER_MOVE", nil)
	}
	if err := SyncAfterMove(obj.FD(), parent, sc.Objects, sc.AnchorFD(), "quarantine"); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}

	nowNS := e.Clock.Wall().UnixNano()
	purgeNotBefore, ok := domain.AddSecondsNS(nowNS, it.BoundRetentionSeconds)
	if !ok {
		return res, e.park(ctx, intent, "RETENTION_OVERFLOW", nil)
	}

	capturedIDJSON, err := json.Marshal(obj.Identity)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	capturedTokJSON, err := json.Marshal(obj.Token)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	if err := e.Store.CompleteQuarantine(ctx, intent, store.QuarantineResult{
		CapturedIdentityJSON: string(capturedIDJSON),
		CapturedTokenJSON:    string(capturedTokJSON),
		QuarantinedAtNS:      nowNS,
		PurgeNotBeforeNS:     purgeNotBefore,
		ContentHash:          string(postEv.Content),
		MetadataHash:         string(metaHash),
	}, `{"source":"observed"}`); err != nil {
		return res, err
	}
	if budget != nil {
		budget.AddAction(obj.Token.Size)
	}

	res.State = domain.StateQuarantined
	res.Code = string(errs.CodeOK)
	res.Control = domain.ControlContinue
	res.Evidence = domain.EvidencePersisted
	res.Operation = opID
	return res, nil
}

// hold freezes an item after the namespace changed but the outcome could not be
// verified, or after a state that cannot be explained. A held item never becomes
// purge-eligible automatically.
func (e *Executor) hold(ctx context.Context, in store.Intent, code string, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	payload := `{"hold_code":` + quoteJSON(code) + `,"detail":` + quoteJSON(detail) + `,"source":"observed"}`
	_ = e.Store.HoldIntent(ctx, in, code, payload)
	_ = e.Store.SetHoldCode(ctx, in.ItemID, code)
	return errs.Newf(errs.CodeHeld, "item %s is on hold: %s", in.ItemID, code)
}

// park records a failure that happened after the file already moved.
//
// The item becomes "unknown" and keeps its pending marker, so reconciliation
// re-reads the vault and finishes or explains it. The event it writes is a
// non-terminal park, not a hold: an operation that stopped being provable is
// still an operation that can be finished once the cause clears.
func (e *Executor) park(ctx context.Context, in store.Intent, code string, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	payload := `{"hold_code":` + quoteJSON(code) + `,"detail":` + quoteJSON(detail) +
		`,"source":"observed","after_move":true,"pending":true}`
	_ = e.Store.ParkIntent(ctx, in, code, payload)
	return errs.Newf(errs.CodeHeld,
		"item %s moved but its outcome could not be proven (%s); run recover reconcile", in.ItemID, code)
}

// miscapture handles the case where the object that moved is not the object the
// plan described.
//
// Two facts are recorded before anything else happens: the object that actually
// moved (identity and token) and the plan's original expectation. Without the
// first, the file sitting in the vault has no product path back: reconcile reads
// item.captured_*, restore requires it, and the hold used to write it only into
// an event payload. "The file is preserved" is not the same as "the operator can
// get it back".
func (e *Executor) miscapture(ctx context.Context, sc *ScopeCtx, it store.Item,
	in store.Intent, obj *fsops.File) (domain.OperationResult, error) {

	res := domain.OperationResult{Item: it.ID, State: domain.StateHeld, Operation: in.OpID}
	res.Control = domain.ControlStopScope
	res.Evidence = domain.EvidencePendingRecn

	actualJSON, _ := json.Marshal(obj.Identity)
	tokJSON, _ := json.Marshal(obj.Token)
	payload := `{"hold_code":"MISCAPTURE","captured_identity":` + quoteJSON(string(actualJSON)) +
		`,"captured_token":` + quoteJSON(string(tokJSON)) + `,"source":"observed"}`
	_ = e.Store.HoldIntentWithCapture(ctx, in, "MISCAPTURE", payload,
		string(actualJSON), string(tokJSON))
	_ = e.Store.SetScopeStatus(ctx, sc.Scope.ID, domain.ScopeHeld)

	compensated, detail := e.tryCompensate(ctx, sc, it, obj)
	if compensated {
		res.Code = string(errs.CodeHeld)
		res.Message = "the wrong object was captured and has been put back; the scope is held"
	} else {
		res.Code = string(errs.CodeHeld)
		res.Message = "the wrong object was captured; nothing was overwritten and the captured object can be restored explicitly"
	}
	return res, errs.Newf(errs.CodeHeld,
		"item %s: the object at that path was not the planned object (%s)", it.ID, detail)
}

// tryCompensate puts a wrongly captured object back, but only when that is
// provably safe: same parent, entry absent, and the vault object still the one
// that moved.
//
// It reuses the full restore protocol rather than a bare rename. The earlier
// version saved a one-field JSON purpose, ignored the file sync, both directory
// syncs and the abort error, and hardcoded "quarantined" as the state to return
// to. A compensation that fails halfway must return the item to held, not to a
// state that implies the original was still there.
func (e *Executor) tryCompensate(ctx context.Context, sc *ScopeCtx, it store.Item, obj *fsops.File) (bool, string) {
	parent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		return false, "the original directory cannot be resolved; the captured object stays in the vault"
	}
	defer parent.Close()

	parentID, _, err := parent.Stat()
	if err != nil {
		return false, "the original directory cannot be inspected; the captured object stays in the vault"
	}
	storedParent, err := decodeID(it.SourceParentIdentityJSON)
	if err != nil || !parentID.Equal(storedParent) {
		return false, "the original directory was replaced; the captured object stays in the vault"
	}
	exists, err := parent.Exists()
	if err != nil {
		return false, "the original path cannot be inspected; the captured object stays in the vault"
	}
	if exists {
		return false, "the original path now holds another file; that file was left untouched"
	}

	opID := domain.OperationID(domain.NewID("op"))
	nowNS := e.Clock.Wall().UnixNano()
	destParentID, _, perr := parent.Stat()
	if perr != nil {
		return false, "the original directory cannot be inspected; the captured object stays in the vault"
	}
	// The destination directory is written into the contract for the same reason
	// the normal restore writes it: after a crash the question "did the object
	// land where it was meant to" must be answerable from the record, not from a
	// directory that may since have been replaced.
	contract := plan.OperationContractV1{
		BindingVersion:     plan.BindingVersion,
		ItemID:             string(it.ID),
		NextState:          domain.StateRIntent,
		PriorState:         domain.StateHeld,
		Purpose:            plan.PurposeMiscaptureFixup,
		ScopeID:            string(sc.Scope.ID),
		RawRelativeB64:     plan.B64(it.RawRelative),
		ObjectName:         it.ObjectName,
		ParentIdentity:     storedParent,
		DestParentIdentity: &destParentID,
		ExpectedIdent:      obj.Identity,
		ExpectedToken:      obj.Token,
		MaxFiles:           e.Config.MaxFiles,
		MaxLogicalBytes:    e.Config.MaxBytes,
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return false, "the compensation contract could not be written"
	}
	comp := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "compensate",
		PriorState:   domain.StateHeld,
		NextState:    domain.StateRIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: nowNS,
	}
	// The compensation reuses the restore transaction, protections included. The
	// earlier version began a bare intent, so an object that was put back could
	// be quarantined again by the very next scan: it was restored in fact and
	// unprotected in the record.
	protections := []store.Protection{{
		ID:           domain.ProtectionID(domain.NewID("prot")),
		ScopeID:      sc.Scope.ID,
		RawRelative:  it.RawRelative,
		MatchKind:    domain.MatchExact,
		Source:       domain.ProtectRestore,
		EvidenceJSON: `{"reason":"a compensated miscapture must not be quarantined again"}`,
		CreatedAtNS:  nowNS,
	}}
	if err := e.Store.RestoreIntent(ctx, comp, protections); err != nil {
		return false, "a compensation intent could not be recorded"
	}
	runCompensateHook()
	if err := fsops.RenameNoReplace(sc.Objects, it.ObjectName, parent, parent.Name()); err != nil {
		_ = e.Store.AbortIntent(ctx, comp, domain.StateHeld,
			`{"source":"observed","reason":"compensation rename failed"}`)
		return false, "the compensation move failed; the captured object stays in the vault"
	}

	back, err := fsops.OpenRegularAt(parent, sc.RootDev)
	if err != nil {
		// The object moved back but cannot be re-opened: the compensation intent
		// stays open so reconciliation can finish it from the observed state.
		return false, "the object was moved back but could not be re-verified; reconcile will finish it"
	}
	if !back.Identity.Equal(obj.Identity) {
		back.Close()
		return false, "the object that moved back is not the captured object; reconcile will explain it"
	}
	if err := back.Sync(); err != nil {
		back.Close()
		return false, "the object was moved back but its data could not be flushed; reconcile will finish it"
	}
	back.Close()
	if err := fsops.DurableDir(parent); err != nil {
		return false, "the destination directory could not be flushed; reconcile will finish it"
	}
	if err := fsops.DurableDir(sc.Objects); err != nil {
		return false, "the vault directory could not be flushed; reconcile will finish it"
	}
	if sc.AnchorFD() >= 0 {
		if err := fsops.Barrier(sc.AnchorFD(), []byte("compensate")); err != nil {
			return false, "the durability barrier failed; reconcile will finish it"
		}
	}

	// The attempt ends where it started: held. A compensated miscapture never
	// regains purge eligibility on its own.
	if err := e.Store.AbortIntent(ctx, comp, domain.StateHeld,
		`{"source":"compensated","result":"original slot restored; item remains held"}`); err != nil {
		return false, "the object was put back but the record could not be closed; reconcile will finish it"
	}
	_ = e.Store.ReleaseItemKeeperPin(ctx, it.ID)
	return true, "put back"
}

func strPtr(s string) *string { return &s }

// quoteJSON renders a string as a JSON string literal.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func decodeID(s string) (domain.FileIdentity, error) {
	var id domain.FileIdentity
	if err := json.Unmarshal([]byte(s), &id); err != nil {
		return id, errs.Newf(errs.CodeStateWriteFailed, "identity is unreadable: %v", err)
	}
	return id, nil
}

func decodeTok(s string) (domain.ChangeToken, error) {
	var t domain.ChangeToken
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return t, errs.Newf(errs.CodeStateWriteFailed, "change token is unreadable: %v", err)
	}
	return t, nil
}
