// Package recovery reconciles operations whose intent committed but whose
// outcome was never recorded.
//
// The matrix in section 11.1 is implemented literally. Nothing here guesses: a
// state that cannot be explained becomes "unknown" and stays visible, and a
// reconciliation never invents an earlier retention start so that something can
// be purged sooner.
package recovery

import (
	"context"
	"encoding/json"
	"errors"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Reconciler resolves pending operations.
type Reconciler struct {
	Store     *store.Store
	Clock     domain.Clock
	Registrar *scope.Registrar
	Exec      *execute.Executor
}

// Result is the outcome for one pending operation.
type Result struct {
	Item domain.ItemID `json:"item_id"`
	// OpID is the intent this pass finished. It is carried so that the
	// operational log and the envelope name the same operation: "pending" is an
	// item state, but the thing that was left half-done is an operation.
	OpID    domain.OperationID `json:"op_id,omitempty"`
	Action  string             `json:"action"`
	Outcome string             `json:"outcome"`
	Detail  string             `json:"detail,omitempty"`
	// Scope names the scope that has to stop taking new actions when Outcome is
	// "blocked". It is carried so the report can be read without a second lookup.
	Scope string `json:"scope_id,omitempty"`
	// RecordError says that this outcome could not be written down.
	//
	// The object may be in a perfectly fine state, but the record of what was
	// decided is missing. A caller that prints "parked" without this would claim
	// that a retry was scheduled when nothing was recorded to retry from.
	RecordError string `json:"record_error,omitempty"`
}

// Report summarises a reconciliation pass.
//
// Parked, Busy and Blocked exist because the previous report had one bucket,
// "unknown", for three different situations, and the bucket cleared the pending
// marker. A parked operation is still unfinished and will be retried; a busy
// scope was never observed at all; a blocked scope could not be verified. Only
// Held is a verdict.
type Report struct {
	Examined  int      `json:"examined"`
	Completed int      `json:"completed"`
	Aborted   int      `json:"aborted"`
	Held      int      `json:"held"`
	Parked    int      `json:"parked"`
	Busy      int      `json:"busy"`
	Blocked   int      `json:"blocked"`
	Unknown   int      `json:"unknown"`
	Results   []Result `json:"results"`
	// BlockedScopes lists the scopes that must stop taking new actions. It is
	// named for the fact rather than for the field it used to share with holds.
	BlockedScopes []string `json:"blocked_scopes,omitempty"`
	// RecordFailures counts outcomes that could not be written down. A pass with
	// a non-zero count here has not finished its work, whatever the other
	// counters say, so the caller must not report success.
	RecordFailures int `json:"record_failures"`
}

// Inspect lists pending operations without changing anything.
func (r *Reconciler) Inspect(ctx context.Context) ([]store.PendingOp, error) {
	return r.Store.PendingOps(ctx)
}

// Run resolves every pending operation. It is idempotent: a second run over the
// same state performs no further moves and records nothing new.
func (r *Reconciler) Run(ctx context.Context) (Report, error) {
	var rep Report
	pend, err := r.Store.PendingOps(ctx)
	if err != nil {
		return rep, err
	}
	rep.Examined = len(pend)
	if len(pend) == 0 {
		return rep, nil
	}

	// One scope context per scope, closed deterministically.
	scopes := map[domain.ScopeID]*execute.ScopeCtx{}
	defer func() {
		for _, sc := range scopes {
			sc.Close()
		}
	}()

	for _, po := range pend {
		sc, ok := scopes[po.Item.ScopeID]
		if !ok {
			opened, err := execute.OpenScopeCtx(ctx, r.Registrar, po.Item.ScopeID)
			if err != nil {
				// Not having the lock is not an observation about the object.
				// Writing "unknown" or holding the scope here turned "somebody
				// else is working on this tree" into "this object is broken",
				// and the second one used to be permanent.
				if errs.CodeOf(err) == errs.CodeInstanceBusy {
					rep.Busy++
					rep.Results = append(rep.Results, Result{Item: po.Item.ID, OpID: po.OpID, Action: po.Action,
						Outcome: "busy", Detail: "another task holds the scope lock; nothing was changed"})
					continue
				}
				// A scope that cannot be verified has not been observed either.
				// The scope status records the problem; the item keeps its
				// pending marker so the next pass can still finish it -
				// unless the status itself cannot be written, and then the
				// caller has to hear about it.
				blocked := Result{Item: po.Item.ID, OpID: po.OpID, Action: po.Action,
					Scope:   string(po.Item.ScopeID),
					Outcome: "blocked", Detail: "the scope root could not be verified: " + err.Error()}
				if serr := r.Store.SetScopeStatus(ctx, po.Item.ScopeID, domain.ScopeHeld); serr != nil {
					blocked.RecordError = "the scope could not be marked held: " + serr.Error()
					rep.RecordFailures++
				}
				rep.Blocked++
				if !containsScope(rep.BlockedScopes, string(po.Item.ScopeID)) {
					rep.BlockedScopes = append(rep.BlockedScopes, string(po.Item.ScopeID))
				}
				rep.Results = append(rep.Results, blocked)
				continue
			}
			scopes[po.Item.ScopeID] = opened
			sc = opened
		}

		res := r.reconcileOne(ctx, sc, po)
		switch res.Outcome {
		case "completed":
			rep.Completed++
		case "aborted":
			rep.Aborted++
		case "held":
			rep.Held++
		case "parked":
			rep.Parked++
		case "unknown":
			rep.Unknown++
		case "blocked":
			// Blocked is not a verdict and not a completion: the row was left as
			// it was and the scope has to stop taking new actions until an
			// operator decides what the record means.
			rep.Blocked++
			if res.Scope != "" && !containsScope(rep.BlockedScopes, res.Scope) {
				rep.BlockedScopes = append(rep.BlockedScopes, res.Scope)
			}
		}
		if res.RecordError != "" {
			rep.RecordFailures++
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

func (r *Reconciler) reconcileOne(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, OpID: po.OpID, Action: po.Action, Scope: string(it.ScopeID)}

	// Historical residue is identified before anything is dispatched, because
	// every ordinary branch would make it worse.
	//
	// An `adopt` pending belongs to an operation this build does not implement.
	// Passing it to the restore branch would converge it as a plain R, which
	// means inventing a decision the operator never made - and the item would
	// then be eligible for purge. It is left exactly as it is instead.
	if po.Action == "adopt" {
		return r.blockPending(ctx, po, out, "UNSUPPORTED_HISTORICAL_ACTION",
			"this pending operation was started by an older build as an adoption; this build does not converge it, so the record and the object are left unchanged")
	}

	// A pending marker for an operation that already ended contradicts itself:
	// one_terminal_per_op allows a single verdict, so writing another one either
	// fails or rewrites history. Neither is a repair.
	if terminal, err := r.Store.OpHasTerminalEvent(ctx, po.OpID); err != nil {
		return r.parkAttempt(ctx, po, out, "TERMINAL_CHECK_FAILED", err.Error())
	} else if terminal {
		return r.blockPending(ctx, po, out, "TERMINAL_EVENT_LEFT_PENDING",
			"the operation already has a terminal event while the item still names it as pending; nothing was written")
	}

	switch po.Action {
	case "quarantine":
		return r.reconcileQuarantine(ctx, sc, po)
	case "restore", "compensate":
		return r.reconcileRestore(ctx, sc, po)
	case "purge":
		return r.reconcilePurge(ctx, sc, po)
	default:
		return r.holdUnfixable(ctx, po, out, "UNKNOWN_ACTION",
			"the pending operation has no recorded action")
	}
}

// blockPending leaves a pending operation untouched and reports why.
//
// Nothing is written to the operation or to the item: those rows are the only
// record of what an older build did, and this build may not rewrite them into a
// shape it can act on. The scope is marked held so that new automatic and manual
// actions stop at it; reading the scope and restoring the healthy objects in it
// keep working.
func (r *Reconciler) blockPending(ctx context.Context, po store.PendingOp, out Result,
	code, detail string) Result {

	out.Outcome = "blocked"
	if err := r.Store.SetScopeStatus(ctx, po.Item.ScopeID, domain.ScopeHeld); err != nil {
		// The block is the point, so a failure to record it cannot be reported as
		// success. The pending marker is still there, which is what the shared
		// gate reads, but the caller is told that the scope was not actually
		// stopped.
		out.Detail = code + ": " + detail
		out.RecordError = "the scope could not be marked held: " + err.Error()
		return out
	}
	out.Detail = code + ": " + detail
	return out
}

// releasePin clears the keeper pin this item may hold.
//
// A quarantine records that a path is the keeper of the object it isolated, and
// the pin is what stops the surviving copy from being taken next. Finishing the
// operation without releasing it leaves the path unusable by any later plan - so
// a failure here is recorded rather than ignored, even though the object itself
// is fine.
func (r *Reconciler) releasePin(ctx context.Context, it store.Item, out *Result) {
	if err := r.Store.ReleaseItemKeeperPin(ctx, it.ID); err != nil {
		if out.RecordError != "" {
			out.RecordError += "; "
		}
		out.RecordError += "the keeper pin could not be released: " + err.Error()
	}
}

// containsScope reports whether a scope is already listed.
func containsScope(list []string, id string) bool {
	for _, s := range list {
		if s == id {
			return true
		}
	}
	return false
}

// retryableCode reports whether a failure is an environment problem that may
// clear on its own.
//
// The distinction matters because the two outcomes are different promises. A
// retryable failure keeps the pending marker, the original contract and the
// attempt count: the object may still be finished, and the next pass picks it up.
// Anything else is a verdict and becomes a terminal hold, which is visible and
// which `recover inspect` explains. What must never happen - and what this
// replaced - is a bare state write that clears the pending marker, because then
// nothing is retried and nothing stays visible either.
func retryableCode(c errs.Code) bool {
	switch c {
	case errs.CodeIOError, errs.CodeDurabilityFailed, errs.CodeAccessDenied,
		errs.CodeStateWriteFailed, errs.CodeDBError, errs.CodeInstanceBusy,
		errs.CodeBudgetExhausted, errs.CodeCancelled:
		return true
	}
	return false
}

// recordFailure parks or holds an operation depending on what kind of failure it
// met, and returns the result row for the report.
func (r *Reconciler) recordFailure(ctx context.Context, po store.PendingOp, out Result,
	code string, err error) Result {

	detail := code
	if err != nil {
		detail = code + ": " + err.Error()
	}
	if err == nil || retryableCode(errs.CodeOf(err)) {
		return r.parkAttempt(ctx, po, out, code, detail)
	}
	return r.holdUnfixableDetail(ctx, po, out, code, detail)
}

// parkAttempt keeps the operation open. The item stays "unknown" with its pending
// marker, so the next pass finds it again; the attempt count is recorded so an
// operator can see that nothing is progressing.
func (r *Reconciler) parkAttempt(ctx context.Context, po store.PendingOp, out Result,
	code, detail string) Result {

	attempts := 1
	if n, err := r.Store.ReconcileAttempts(ctx, po.Item.ID); err == nil {
		attempts = n + 1
	}
	if err := r.Store.NoteParked(ctx, po.Item.ID, code, detail, attempts); err != nil {
		// Failing to write the note does not make the object unexplainable - the
		// pending marker is still in place, which is the fact that matters - but
		// it does mean this pass did not record what it decided. The caller is
		// told so that a scheduler does not read "parked" as "a retry will find
		// the attempt count I just wrote".
		out.Outcome = "parked"
		out.Detail = detail
		out.RecordError = "the attempt could not be recorded: " + err.Error()
		return out
	}
	out.Outcome = "parked"
	out.Detail = detail
	return out
}

// holdUnfixable records a verdict: the observed state contradicts the record, or
// there is nothing left to observe.
func (r *Reconciler) holdUnfixable(ctx context.Context, po store.PendingOp, out Result, code, detail string) Result {
	return r.holdUnfixableDetail(ctx, po, out, code, detail)
}

func (r *Reconciler) holdUnfixableDetail(ctx context.Context, po store.PendingOp, out Result,
	code, detail string) Result {

	if err := r.Store.HoldIntent(ctx, intentOf(po), code,
		`{"source":"reconciled","result":"held without an automatic repair","detail":`+
			quoteJSON(detail)+`}`); err != nil {
		// Nothing was recorded, so this is not a hold: the pending marker is
		// still there and the next pass will meet the same state. Reporting
		// "held" here would claim a verdict that does not exist.
		out.Outcome = "unknown"
		out.Detail = detail
		out.RecordError = "the hold could not be recorded: " + err.Error()
		return out
	}
	out.Outcome = "held"
	out.Detail = detail
	return out
}

// reconcileQuarantine implements the Q_INTENT rows of the matrix.
//
// The evidence used to complete an interrupted quarantine comes from the intent
// contract, not from the item row. R08 documents both halves of the old bug: the
// completion wrote whatever the item happened to carry (often nothing) as the
// metadata hash, and the manifest was treated as optional. Either one leaves an
// object whose recorded evidence can never match the object itself, so the object
// can never be purged and can never be explained.
func (r *Reconciler) reconcileQuarantine(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, OpID: po.OpID, Action: "quarantine"}

	// The contract is read through the same binder as every other action. Parsing
	// it as "a quarantine contract" only proves the fields exist; it does not
	// prove they describe *this* pending operation. A well-formed contract bound
	// to another item, scope or path used to be consumed here - the source is
	// still present, the vault is empty, and the branch below writes "aborted"
	// without noticing.
	contract, cerr := r.operationContract(po)
	if cerr != nil {
		// The contract is the record of what was meant to move. Without it there
		// is nothing to compare against, so this is a verdict, not a retry.
		return r.recordFailure(ctx, po, out, "INTENT_CONTRACT_UNREADABLE", cerr)
	}
	expected := contract.ExpectedIdent

	srcID, srcFound, serr := statEntry(sc, it.RawRelative)
	if serr != nil {
		return r.recordFailure(ctx, po, out, "SOURCE_STAT_FAILED", serr)
	}
	vaultID, vaultFound, verr := statVault(sc, it.ObjectName)
	if verr != nil {
		return r.recordFailure(ctx, po, out, "VAULT_STAT_FAILED", verr)
	}

	switch {
	case srcFound && !vaultFound:
		// Nothing moved. Close the attempt; never replay an old plan.
		if err := r.Store.AbortIntent(ctx, intentOf(po), domain.StateAborted,
			`{"source":"reconciled","result":"source still present, vault empty"}`); err != nil {
			return r.recordFailure(ctx, po, out, "ABORT_FAILED", err)
		}
		r.releasePin(ctx, it, &out)
		out.Outcome = "aborted"
		out.Detail = "the file was never moved"
		return out

	case !srcFound && vaultFound && vaultID.Equal(expected) && srcID.IsZero():
		// The move happened but was not recorded. Complete it after re-verifying.
		completion, err := r.completeQuarantine(ctx, sc, po, contract)
		if err != nil {
			return r.recordFailure(ctx, po, out, "COMPLETION_VERIFY_FAILED", err)
		}
		out.Outcome = "completed"
		out.Detail = completion
		return out

	case srcFound && vaultFound && vaultID.Equal(expected) && srcID.Equal(expected):
		// The same object is in two places, which the single-link admission rule
		// forbids. This is not a state to repair automatically.
		return r.holdUnfixable(ctx, po, out, "DUPLICATE_LINK_STATE",
			"the object appears both at the source and in the vault")

	case vaultFound && !vaultID.Equal(expected):
		// The object in the vault is not the planned object. The file really did
		// leave the tree, and the evidence of what left is available right here -
		// so it is recorded.
		//
		// This branch used to write a plain hold. The consequence was a file the
		// product could neither purge (no captured evidence) nor restore (no
		// captured identity), preserved on disk but unreachable. It also used to
		// end the operation when the object merely could not be opened for a
		// moment; a read that failed is not a verdict, so that case parks and the
		// next pass records the evidence.
		if cerr := r.captureEvidence(ctx, sc, po); cerr != nil {
			return r.recordFailure(ctx, po, out, "CAPTURE_EVIDENCE_UNREADABLE", cerr)
		}
		out.Detail = "the vault holds an object that is not the planned one; its evidence was recorded so it can be restored"
		out.Outcome = "held"
		if err := r.Store.SetScopeStatus(ctx, sc.Scope.ID, domain.ScopeHeld); err != nil {
			out.RecordError = "the scope could not be marked held: " + err.Error()
		}
		return out

	case !srcFound && !vaultFound:
		return r.holdUnfixable(ctx, po, out, "OBJECT_MISSING_BOTH_PLACES",
			"neither the source nor the vault object exists")

	default:
		// A different object now sits at the source path. The vault object is
		// intact, so the quarantine itself can be completed without touching the
		// new file.
		if vaultFound && vaultID.Equal(expected) {
			completion, err := r.completeQuarantine(ctx, sc, po, contract)
			if err != nil {
				return r.recordFailure(ctx, po, out, "COMPLETION_VERIFY_FAILED", err)
			}
			out.Outcome = "completed"
			out.Detail = completion + "; a new file at the original path was left untouched"
			return out
		}
		return r.holdUnfixable(ctx, po, out, "UNEXPLAINED_QUARANTINE_STATE",
			"the observed source and vault state matches no explainable outcome")
	}
}

// captureEvidence records the object that actually moved, together with the
// evidence of what it is.
//
// The evidence is read from the object in the vault, never from the plan: the
// plan describes what was expected, and repeating it here would produce a hold
// whose "actual" record is a copy of the mistake. Every failure is returned as an
// error with its own category, so the caller can park a temporary one instead of
// ending an operation whose evidence simply could not be read yet.
func (r *Reconciler) captureEvidence(ctx context.Context, sc *execute.ScopeCtx,
	po store.PendingOp) error {

	it := po.Item
	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return errs.Wrapf(errs.CodeAccessDenied, err,
			"the vault object for %s cannot be opened, so the object that moved cannot be described", it.ID)
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return errs.Wrapf(errs.CodeAccessDenied, err,
			"the vault object for %s cannot be opened, so the object that moved cannot be described", it.ID)
	}
	defer obj.Close()

	idJSON, _ := json.Marshal(obj.Identity)
	tokJSON, _ := json.Marshal(obj.Token)
	payload := `{"source":"reconciled","result":"vault object is not the planned object","captured_identity":` +
		quoteJSON(string(idJSON)) + `,"captured_token":` + quoteJSON(string(tokJSON)) + `}`
	if err := r.Store.HoldIntentWithCapture(ctx, intentOf(po), "UNEXPECTED_VAULT_OBJECT",
		payload, string(idJSON), string(tokJSON)); err != nil {
		return errs.Wrapf(errs.CodeStateWriteFailed, err,
			"the evidence of the object that moved could not be recorded for %s", it.ID)
	}
	return nil
}

// completeQuarantine finishes a move that already happened.
//
// Every fact is taken from the contract and cross-checked against the manifest and
// the object itself. Nothing is "rebuilt from what is currently there": the point
// of the completion record is that it describes the object that was verified, and
// an object whose evidence cannot be established is held for a human, not
// completed with weaker claims.
func (r *Reconciler) completeQuarantine(ctx context.Context, sc *execute.ScopeCtx,
	po store.PendingOp, contract plan.OperationContractV1) (string, error) {

	it := po.Item

	wantContent, err := contract.ContentDigest()
	if err != nil {
		return "", err
	}
	wantMetadata, err := contract.MetadataDigest()
	if err != nil {
		return "", err
	}
	// The manifest is an independent record written before the move. A missing or
	// damaged manifest means the two records cannot be cross-checked, which the
	// review requires to be a refusal rather than a silent pass. A manifest that
	// cannot be *read* is a third case: the file may be perfectly fine, so it is
	// parked with its pending marker instead of ending the operation.
	manifest, err := r.readManifest(sc, it.ObjectName)
	if err != nil {
		return "", err
	}
	if manifest.ObjectID != string(it.ID) {
		return "", errs.Newf(errs.CodeRecoveryRequired,
			"the manifest describes object %q, not %q", manifest.ObjectID, it.ID)
	}
	if !manifest.ExpectedIdentity.Equal(contract.ExpectedIdent) {
		return "", errs.New(errs.CodeRecoveryRequired,
			"the manifest and the intent contract record different expected identities")
	}
	if manifest.ContentHash != string(wantContent) {
		return "", errs.New(errs.CodeRecoveryRequired,
			"the manifest and the intent contract record different content hashes")
	}
	if manifest.MetadataHash != wantMetadata {
		return "", errs.New(errs.CodeRecoveryRequired,
			"the manifest and the intent contract record different metadata hashes")
	}

	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		return "", err
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		return "", err
	}
	defer obj.Close()

	if !obj.Identity.Equal(contract.ExpectedIdent) {
		return "", errs.New(errs.CodeHeld, "vault object identity differs")
	}
	// Re-establish the object's own evidence with the same stable read the normal
	// path uses, rather than trusting a single hash of the current bytes.
	opt := r.Exec.VerifyOpt(obj.Token.Size)
	evidence, err := execute.ReadStable(obj, opt)
	if err != nil {
		return "", err
	}
	if evidence.Content != wantContent {
		return "", errs.New(errs.CodeHeld, "vault object content differs from the intent contract")
	}
	if string(evidence.Metadata) != wantMetadata {
		return "", errs.New(errs.CodeHeld, "vault object metadata differs from the intent contract")
	}
	// The rename changed the vault object's ctime, so the whole token cannot match.
	// Everything a rename leaves alone still has to.
	if evidence.Token.Size != contract.ExpectedToken.Size ||
		evidence.Token.NLink != contract.ExpectedToken.NLink ||
		evidence.Token.Mode != contract.ExpectedToken.Mode ||
		evidence.Token.UID != contract.ExpectedToken.UID ||
		evidence.Token.GID != contract.ExpectedToken.GID {
		return "", errs.New(errs.CodeHeld, "the vault object does not match the token in the intent contract")
	}

	// Same persistence protocol as the normal path: object data, both parent
	// directories, then the anchor barrier.
	srcParent, perr := sc.Root.OpenParent(it.RawRelative)
	if perr != nil {
		return "", errs.Wrap(errs.CodeDurabilityFailed, perr)
	}
	defer srcParent.Close()
	if err := execute.SyncAfterMove(obj.FD(), srcParent, sc.Objects, sc.AnchorFD(), "reconcile-quarantine"); err != nil {
		return "", err
	}

	// Retention starts now, from a verified completion. Reconciliation must
	// never backdate it to a moment it cannot prove.
	nowNS := r.Clock.Wall().UnixNano()
	purgeNotBefore, ok := domain.AddSecondsNS(nowNS, it.BoundRetentionSeconds)
	if !ok {
		return "", errs.New(errs.CodeHeld, "retention overflow")
	}
	idJSON, _ := json.Marshal(evidence.Identity)
	tokJSON, _ := json.Marshal(evidence.Token)
	if err := r.Store.CompleteQuarantine(ctx, intentOf(po), store.QuarantineResult{
		CapturedIdentityJSON: string(idJSON),
		CapturedTokenJSON:    string(tokJSON),
		QuarantinedAtNS:      nowNS,
		PurgeNotBeforeNS:     purgeNotBefore,
		ContentHash:          string(wantContent),
		MetadataHash:         wantMetadata,
	}, `{"source":"reconciled","result":"RECONCILED_OBSERVED"}`); err != nil {
		return "", err
	}
	return "the move was already done; its record was completed and the retention window restarted from now", nil
}

// reconcileRestore implements the R_INTENT rows of the matrix.
//
// Both the destination and the state an aborted attempt returns to are read from
// the versioned contract. The previous version decoded one field loosely and
// hardcoded "quarantined" as the return state, so a restore that was started from
// a hold came back as "quarantined" - which both cleared the hold semantics and
// started the object on the way to purge eligibility.
func (r *Reconciler) reconcileRestore(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, OpID: po.OpID, Action: po.Action, Scope: string(it.ScopeID)}

	contract, dest, cerr := r.restoreContract(po)
	if cerr != nil {
		return r.recordFailure(ctx, po, out, "RESTORE_CONTRACT_UNREADABLE", cerr)
	}
	// The state an aborted attempt returns to is read from the contract. When the
	// contract predates that field there is no answer, and the old fallback -
	// "it must have been quarantined" - is exactly how an object that was held
	// came back purge-eligible.
	backState, priorKnown := priorStateOf(contract)

	captured, err := identityOf(ptrString(it.CapturedIdentityJSON))
	if err != nil {
		return r.recordFailure(ctx, po, out, "CAPTURED_IDENTITY_UNREADABLE", err)
	}

	destID, destFound, derr := statEntry(sc, dest)
	if derr != nil {
		return r.recordFailure(ctx, po, out, "DEST_STAT_FAILED", derr)
	}
	vaultID, vaultFound, verr := statVault(sc, it.ObjectName)
	if verr != nil {
		return r.recordFailure(ctx, po, out, "VAULT_STAT_FAILED", verr)
	}

	switch {
	case destFound && destID.Equal(captured) && !vaultFound:
		// Same persistence protocol as the normal restore: the destination's own
		// data, the objects directory it came from, the destination's parent, then
		// the anchor barrier. R08 records that the destination parent sync was
		// ignored here and the object data was never synced at all, so a crash
		// after this record could leave a completion that was not on disk.
		destParent, perr := sc.Root.OpenParent(dest)
		if perr != nil {
			return r.recordFailure(ctx, po, out, "DEST_PARENT_UNAVAILABLE", perr)
		}
		defer destParent.Close()
		// The contract recorded the directory the object was meant to land in. If
		// that directory is now a different one, the completion would describe a
		// move that did not happen where it was approved.
		if contract != nil && contract.DestParentIdentity != nil {
			nowParent, _, ierr := destParent.Stat()
			if ierr != nil {
				return r.recordFailure(ctx, po, out, "DEST_PARENT_UNAVAILABLE", ierr)
			}
			if !nowParent.Equal(*contract.DestParentIdentity) {
				return r.holdUnfixable(ctx, po, out, "DEST_PARENT_REPLACED",
					"the destination directory is not the one the operation recorded")
			}
		}
		des, oerr := fsops.OpenRegularAt(destParent, sc.RootDev)
		if oerr != nil {
			return r.recordFailure(ctx, po, out, "DEST_OPEN_FAILED", oerr)
		}
		defer des.Close()
		if serr := execute.SyncAfterMove(des.FD(), sc.Objects, destParent,
			sc.AnchorFD(), "reconcile-restore"); serr != nil {
			return r.recordFailure(ctx, po, out, "SYNC_FAILED", serr)
		}
		if err := r.Store.CompleteRestoreReleasingHold(ctx, intentOf(po), `{"source":"reconciled"}`, it.ScopeID); err != nil {
			return r.recordFailure(ctx, po, out, "COMPLETE_FAILED", err)
		}
		r.releasePin(ctx, it, &out)
		out.Outcome = "completed"
		out.Detail = "the restore had already happened; its record was completed"
		return out

	case !destFound && vaultFound && vaultID.Equal(captured):
		// The restore never happened. End the attempt and return the item to the
		// state the contract says it came from: a hold stays a hold.
		if !priorKnown {
			return r.holdUnfixable(ctx, po, out, "PRIOR_STATE_UNKNOWN",
				"the restore did not happen and its contract records no state to return to; the object stays in the vault and the item keeps its pending marker")
		}
		if err := r.Store.AbortIntent(ctx, intentOf(po), backState,
			`{"source":"reconciled","result":"destination empty, object still in the vault","back_state":`+
				quoteJSON(backState)+`}`); err != nil {
			return r.recordFailure(ctx, po, out, "ABORT_FAILED", err)
		}
		out.Outcome = "aborted"
		out.Detail = "the restore did not happen; the object is back in " + backState
		return out

	case destFound && vaultFound:
		return r.holdUnfixable(ctx, po, out, "RESTORE_CONFLICT",
			"both the destination and the vault object exist; nothing was overwritten")

	case !destFound && !vaultFound:
		return r.holdUnfixable(ctx, po, out, "RESTORE_UNEXPLAINED",
			"neither the destination nor the vault object exists")

	default:
		return r.holdUnfixable(ctx, po, out, "RESTORE_UNEXPLAINED",
			"the observed state does not match any explainable restore outcome")
	}
}

// operationContract parses one operation's contract and checks the binding.
//
// Unlike restoreContract it has no legacy branch: the shapes it serves are the
// ones this build writes, and a record that does not answer "what state did this
// come from" is reported rather than guessed at.
func (r *Reconciler) operationContract(po store.PendingOp) (plan.OperationContractV1, error) {
	contract, err := plan.ParseContractFor(po.ContractJSON, po.Action)
	if err != nil {
		return contract, err
	}
	if err := checkContractBinding(po, contract); err != nil {
		return contract, err
	}
	return contract, nil
}

// restoreContract reads what a restore-family operation needs: where the object
// was going, and which state an aborted attempt returns to.
//
// Two shapes exist in the field. Contracts written before the binding version was
// recorded carry only the destination; they are read, labelled as legacy, and
// nothing else is claimed about them. A contract that names a binding version but
// does not parse is a refusal: that is a damaged record, not an old one.
func (r *Reconciler) restoreContract(po store.PendingOp) (*plan.OperationContractV1, []byte, error) {
	legacy := struct {
		BindingVersion int    `json:"binding_version"`
		RawRelativeB64 string `json:"raw_relative_b64"`
	}{}
	if po.ContractJSON != "" {
		if err := json.Unmarshal([]byte(po.ContractJSON), &legacy); err != nil {
			return nil, nil, errs.Newf(errs.CodeRecoveryRequired,
				"the operation contract is not JSON: %v", err)
		}
	}

	dest := po.Item.RawRelative
	if legacy.RawRelativeB64 != "" {
		raw, err := decodeB64(legacy.RawRelativeB64)
		if err != nil || len(raw) == 0 {
			return nil, nil, errs.Newf(errs.CodeRecoveryRequired,
				"the operation contract records a source path that is not valid base64: %v", err)
		}
		dest = raw
	}
	if legacy.BindingVersion == 0 {
		// Pre-versioning record. The destination is the only field it can be
		// trusted for, so that is the only thing returned.
		return nil, dest, nil
	}

	// The contract is checked against the action that wrote it: a restore without
	// a destination directory cannot be verified against the directory the
	// operator approved, and a purge must not be asked for one.
	contract, err := plan.ParseContractFor(po.ContractJSON, po.Action)
	if err != nil {
		return nil, nil, err
	}
	if err := checkContractBinding(po, contract); err != nil {
		return nil, nil, err
	}
	if raw, berr := decodeB64(contract.RawRelativeB64); berr == nil && len(raw) > 0 {
		dest = raw
	}
	return &contract, dest, nil
}

// checkContractBinding verifies that the contract describes this pending
// operation and not a neighbouring one.
//
// A contract is the only record of what was meant to happen, and reconciliation
// compares it with the filesystem. A contract for another item, another scope or
// another object would compare the wrong two things and still "succeed"; the
// review asks for the binding to be checked before either branch consumes it.
//
// The path field is read according to the action that wrote it, because the same
// field means two different things:
//
//   - a quarantine, a purge and a compensation name the path they act on, which is
//     the item's own path, and equality is the binding;
//   - a restore names where the object was sent, and `quarantine restore
//     --destination-relative` is an existing feature: the destination is allowed
//     to differ from the item's path, and it is checked for being a usable path
//     plus, at completion, against the destination directory recorded next to it.
//
// Requiring equality for every action was a regression: it judged the restore
// contracts this build writes itself to be mismatches, and a restore to another
// path that was interrupted then entered a terminal hold with the file sitting
// where the operator asked for it.
func checkContractBinding(po store.PendingOp, c plan.OperationContractV1) error {
	if c.ItemID != string(po.Item.ID) {
		return errs.Newf(errs.CodeRecoveryRequired,
			"the contract names item %q, but the pending operation is for %q", c.ItemID, po.Item.ID)
	}
	if c.ScopeID != string(po.Item.ScopeID) {
		return errs.Newf(errs.CodeRecoveryRequired,
			"the contract names scope %q, but the item belongs to %q", c.ScopeID, po.Item.ScopeID)
	}
	if c.ObjectName != po.Item.ObjectName {
		return errs.Newf(errs.CodeRecoveryRequired,
			"the contract names vault object %q, but the item is %q", c.ObjectName, po.Item.ObjectName)
	}
	raw, err := decodeB64(c.RawRelativeB64)
	if err != nil || len(raw) == 0 {
		return errs.Newf(errs.CodeRecoveryRequired,
			"the contract records a path that is not valid base64: %v", err)
	}
	if po.Action == "restore" {
		// A restore records its destination. It still has to be a path this build
		// would accept, so a corrupted or hostile record cannot point the
		// completion at "/etc" or at "../.."; the directory it was approved for is
		// checked separately, in reconcileRestore.
		if _, perr := fsops.SplitRelative(raw); perr != nil {
			return errs.Newf(errs.CodeRecoveryRequired,
				"the restore contract records destination %q, which is not a usable relative path: %v",
				string(raw), perr)
		}
		return nil
	}
	if string(raw) != string(po.Item.RawRelative) {
		return errs.Newf(errs.CodeRecoveryRequired,
			"the contract names path %q, but the item is %q", string(raw), string(po.Item.RawRelative))
	}
	return nil
}

// priorStateOf returns the state an aborted operation goes back to, and whether
// the record actually says.
//
// The item row is deliberately not consulted as a fallback. Its state at this
// point is "an intent is open", which is the same for a restore of a quarantined
// object and for a restore that started from a hold; deriving one from the other
// is how a held object came back as "quarantined" and started down the purge path.
func priorStateOf(contract *plan.OperationContractV1) (string, bool) {
	if contract == nil {
		return "", false
	}
	switch contract.PriorState {
	case domain.StateQuarantined, domain.StateHeld, domain.StateUnknown:
		return contract.PriorState, true
	}
	return "", false
}

// reconcilePurge implements the P_INTENT rows of the matrix.
//
// The contract is read and checked once, before the two branches, and both of them
// consume that result. The previous version passed the error away with `_` and
// only looked at the contract on the "object still there" branch, so a purge whose
// contract was damaged or bound to another item could be recorded as
// RECONCILED_ABSENT - a completion based on no evidence at all.
func (r *Reconciler) reconcilePurge(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, OpID: po.OpID, Action: "purge"}

	contract, cerr := r.operationContract(po)
	if cerr != nil {
		return r.recordFailure(ctx, po, out, "PURGE_CONTRACT_UNREADABLE", cerr)
	}
	backState, priorKnown := priorStateOf(&contract)
	// The prior state is required by both branches, so it is checked before the
	// split rather than inside one of them. When it was only checked on the
	// "object still there" branch, a contract that never recorded where the
	// object came from could still finish as RECONCILED_ABSENT: a terminal record
	// about a deletion, written from evidence that does not say which state the
	// deletion started in. "The object is gone" is not the same as "this purge
	// removed it".
	if !priorKnown {
		return r.recordFailure(ctx, po, out, "PRIOR_STATE_UNKNOWN",
			errs.New(errs.CodeRecoveryRequired,
				"the purge contract records no state to return to, so neither outcome can be recorded as a verdict; the item is held and the vault is left as it is. A contract is never rewritten by a later pass, so this needs a decision rather than a retry"))
	}

	captured, err := identityOf(ptrString(it.CapturedIdentityJSON))
	if err != nil {
		return r.recordFailure(ctx, po, out, "CAPTURED_IDENTITY_UNREADABLE", err)
	}
	vaultID, vaultFound, verr := statVault(sc, it.ObjectName)
	if verr != nil {
		return r.recordFailure(ctx, po, out, "VAULT_STAT_FAILED", verr)
	}

	if vaultFound {
		// The unlink did not happen (or did not take effect). Return the item to
		// the state the contract says it came from so a later, fully checked
		// attempt can be made.
		if !vaultID.Equal(captured) {
			return r.holdUnfixable(ctx, po, out, "UNEXPECTED_VAULT_OBJECT",
				"the vault object is not the recorded object")
		}
		if err := r.Store.AbortIntent(ctx, intentOf(po), backState,
			`{"source":"reconciled","result":"object still present, purge not confirmed","back_state":`+
				quoteJSON(backState)+`}`); err != nil {
			return r.recordFailure(ctx, po, out, "ABORT_FAILED", err)
		}
		out.Outcome = "aborted"
		out.Detail = "the purge did not happen; the object is still in " + backState
		return out
	}

	// The object is gone and the vault identity is trusted, so the intent is
	// complete. This is not an observation of a successful unlink and it does
	// not claim that any disk space was released.
	//
	// The steps are the ones the normal purge path issues. R08 found this branch
	// syncing only the objects directory and skipping the anchor barrier, so the
	// completion record could be durable while the removal it describes was not.
	if err := execute.SyncAfterUnlink(sc.Objects, sc.AnchorFD(), "reconcile-purge"); err != nil {
		return r.recordFailure(ctx, po, out, "SYNC_FAILED", err)
	}
	if err := r.Store.CompletePurge(ctx, intentOf(po),
		`{"source":"reconciled","result":"RECONCILED_ABSENT"}`); err != nil {
		return r.recordFailure(ctx, po, out, "COMPLETE_FAILED", err)
	}
	r.releasePin(ctx, it, &out)
	out.Outcome = "completed"
	out.Detail = "the object was already absent; recorded as RECONCILED_ABSENT, not as a verified unlink"
	return out
}

// readManifest reads the manifest of one vault object and classifies why it could
// not be used.
//
// Three outcomes, three meanings: absent is a contradiction (the manifest is
// written before the rename, so a moved object without one has lost its record),
// unreadable JSON is a damaged record, and a read error is an environment problem
// that may clear. Only the last one keeps the operation open; the first two are
// verdicts and carry CodeRecoveryRequired, which the caller turns into a hold.
func (r *Reconciler) readManifest(sc *execute.ScopeCtx, objectID string) (execute.ManifestV1, error) {
	var m execute.ManifestV1
	data, err := readManifestFile(sc, objectID)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return m, errs.Newf(errs.CodeRecoveryRequired,
				"the object was moved but its manifest is gone, so the move cannot be cross-checked: %v", err)
		}
		return m, errs.Wrapf(errs.CodeIOError, err,
			"the manifest could not be read; the completion is retried when it can be")
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, errs.Newf(errs.CodeRecoveryRequired,
			"the manifest is not readable JSON: %v", err)
	}
	return m, nil
}

// statEntry reports whether rel exists, and its identity when it does.
//
// A permission or I/O error is not "absent" and it is not "present but odd"
// either: it is "this was not checked". The previous shape returned found=true
// with a zero identity, which turned an unavailable check into an identity
// contradiction and let the caller record a verdict about a file nobody had
// looked at. The third return value says so, and the caller parks instead.
func statEntry(sc *execute.ScopeCtx, rel []byte) (domain.FileIdentity, bool, error) {
	parent, err := sc.Root.OpenParent(rel)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false, nil
		}
		return domain.FileIdentity{}, false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer parent.Close()
	id, _, err := parent.StatAtNoFollow()
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false, nil
		}
		return domain.FileIdentity{}, false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	return id, true, nil
}

// statVault is statEntry for the object inside the vault.
func statVault(sc *execute.ScopeCtx, name string) (domain.FileIdentity, bool, error) {
	parent, err := sc.Objects.ParentOf(name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false, nil
		}
		return domain.FileIdentity{}, false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	defer parent.Close()
	id, _, err := parent.StatAtNoFollow()
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false, nil
		}
		return domain.FileIdentity{}, false, errs.Wrap(errs.CodeAccessDenied, err)
	}
	return id, true, nil
}

func intentOf(po store.PendingOp) store.Intent {
	return store.Intent{
		OpID:         po.OpID,
		ItemID:       po.Item.ID,
		Action:       po.Action,
		ContractJSON: po.ContractJSON,
		OccurredAtNS: po.IntentAtNS,
	}
}

func identityOf(s string) (domain.FileIdentity, error) {
	var id domain.FileIdentity
	err := json.Unmarshal([]byte(s), &id)
	return id, err
}

func tokenOf(s string) (domain.ChangeToken, error) {
	var t domain.ChangeToken
	err := json.Unmarshal([]byte(s), &t)
	return t, err
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func decodeB64(s string) ([]byte, error) {
	return decodeB64Std(s)
}
