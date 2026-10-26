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

	// A path that became protected after planning must not move.
	if ok, why := policy.GlobalProtected(string(it.RawRelative)); ok {
		res.Code = string(errs.CodeSkippedProtected)
		res.Message = "global protection: " + why
		return res, nil
	}

	preID, err := decodeID(it.PreIdentityJSON)
	if err != nil {
		return res, err
	}
	preTok, err := decodeTok(it.PreTokenJSON)
	if err != nil {
		return res, err
	}
	if ok, why := e.Guard.CheckExact(string(it.RawRelative), preID); ok {
		res.Code = string(errs.CodeSkippedProtected)
		res.Message = why
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

	ev, err := MetadataOf(src)
	if err != nil {
		return res, err
	}
	if ev.Unsupported != "" {
		res.Code = string(errs.CodeUnsupportedMetadata)
		res.Message = "metadata cannot be normalised safely: " + ev.Unsupported
		return res, nil
	}

	opt := e.VerifyOpt(src.Token.Size)
	opt.Budget = budget
	preHash, _, err := HashFile(src, opt)
	if err != nil {
		return res, err
	}
	metaHash := MetadataDigest(ev, src.Token)

	if err := checkNoWriterChange(src, preID, preTok); err != nil {
		return res, err
	}

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
		return e.miscapture(ctx, sc, it, intent, obj.Identity)
	}

	opt2 := e.VerifyOpt(obj.Token.Size)
	opt2.Budget = budget
	postHash, _, err := HashFile(obj, opt2)
	if err != nil {
		return res, e.park(ctx, intent, "POST_VERIFY_READ_FAILED", err)
	}
	if postHash != preHash {
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
		ContentHash:          string(postHash),
		MetadataHash:         string(metaHash),
	}, `{"source":"observed"}`); err != nil {
		return res, err
	}

	res.State = domain.StateQuarantined
	res.Code = string(errs.CodeOK)
	res.Operation = opID
	return res, nil
}

// hold freezes an item after the namespace changed but the outcome could not be
// verified. A held item never becomes purge-eligible automatically.
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
// re-reads the vault and finishes or explains it. Calling hold here would clear
// the marker and orphan a file that really did move.
func (e *Executor) park(ctx context.Context, in store.Intent, code string, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	payload := `{"hold_code":` + quoteJSON(code) + `,"detail":` + quoteJSON(detail) + `,"source":"observed","after_move":true}`
	_ = e.Store.ParkIntent(ctx, in, code, payload)
	return errs.Newf(errs.CodeHeld,
		"item %s moved but its outcome could not be proven (%s); run recover reconcile", in.ItemID, code)
}

// miscapture handles the case where the object that moved is not the object the
// plan described. The scope stops, the real identity is recorded, and a
// compensation restore is attempted only if the original slot is provably safe.
func (e *Executor) miscapture(ctx context.Context, sc *ScopeCtx, it store.Item,
	in store.Intent, actual domain.FileIdentity) (domain.OperationResult, error) {

	res := domain.OperationResult{Item: it.ID, State: domain.StateHeld, Operation: in.OpID}
	actualJSON, _ := json.Marshal(actual)
	payload := `{"hold_code":"MISCAPTURE","captured_identity":` + quoteJSON(string(actualJSON)) + `,"source":"observed"}`
	_ = e.Store.HoldIntent(ctx, in, "MISCAPTURE", payload)
	_ = e.Store.SetHoldCode(ctx, it.ID, "MISCAPTURE")
	_ = e.Store.SetScopeStatus(ctx, sc.Scope.ID, domain.ScopeHeld)

	if e.tryCompensate(ctx, sc, it, actual) {
		res.Code = string(errs.CodeHeld)
		res.Message = "the wrong object was captured and has been put back; the scope is held"
	} else {
		res.Code = string(errs.CodeHeld)
		res.Message = "the wrong object was captured; both objects were preserved; the scope is held"
	}
	return res, errs.Newf(errs.CodeHeld,
		"item %s: the object at that path was not the planned object", it.ID)
}

// tryCompensate puts a wrongly captured object back without overwriting
// anything. Any doubt leaves both objects in place.
func (e *Executor) tryCompensate(ctx context.Context, sc *ScopeCtx, it store.Item, actual domain.FileIdentity) bool {
	parent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		return false
	}
	defer parent.Close()

	parentID, _, err := parent.Stat()
	if err != nil {
		return false
	}
	storedParent, err := decodeID(it.SourceParentIdentityJSON)
	if err != nil || !parentID.Equal(storedParent) {
		return false
	}
	exists, err := parent.Exists()
	if err != nil || exists {
		return false
	}

	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return false
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return false
	}
	defer obj.Close()
	if !obj.Identity.Equal(actual) {
		return false
	}

	opID := domain.OperationID(domain.NewID("op"))
	comp := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "compensate",
		PriorState:   domain.StateHeld,
		NextState:    domain.StateRIntent,
		ContractJSON: `{"purpose":"miscapture_compensation"}`,
		OccurredAtNS: e.Clock.Wall().UnixNano(),
	}
	if err := e.Store.BeginIntent(ctx, comp); err != nil {
		return false
	}
	if err := fsops.RenameNoReplace(sc.Objects, it.ObjectName, parent, parent.Name()); err != nil {
		_ = e.Store.AbortIntent(ctx, comp, domain.StateHeld, `{"reason":"compensation rename failed"}`)
		return false
	}
	if back, err := fsops.OpenRegularAt(parent, sc.RootDev); err == nil {
		_ = back.Sync()
		back.Close()
	}
	_ = fsops.DurableDir(parent)
	_ = fsops.DurableDir(sc.Objects)

	// The attempt ends, but the item stays held: a compensated miscapture never
	// regains purge eligibility on its own.
	_ = e.Store.AbortIntent(ctx, comp, domain.StateHeld,
		`{"source":"compensated","result":"original slot restored; item remains held"}`)
	return true
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
