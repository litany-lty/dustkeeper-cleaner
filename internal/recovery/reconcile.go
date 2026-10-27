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
	Scopes    []string `json:"scopes_held,omitempty"`
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
				// pending marker so the next pass can still finish it.
				_ = r.Store.SetScopeStatus(ctx, po.Item.ScopeID, domain.ScopeHeld)
				rep.Blocked++
				rep.Results = append(rep.Results, Result{Item: po.Item.ID, OpID: po.OpID, Action: po.Action,
					Outcome: "blocked", Detail: "the scope root could not be verified: " + err.Error()})
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
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

func (r *Reconciler) reconcileOne(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, OpID: po.OpID, Action: po.Action}

	switch po.Action {
	case "quarantine":
		return r.reconcileQuarantine(ctx, sc, po)
	case "restore", "compensate", "adopt":
		return r.reconcileRestore(ctx, sc, po)
	case "purge":
		return r.reconcilePurge(ctx, sc, po)
	default:
		return r.holdUnfixable(ctx, po, out, "UNKNOWN_ACTION",
			"the pending operation has no recorded action")
	}
}

// retryableCode reports whether a failure is an environment problem that may
// clear on its own.
//
// The distinction matters because the two outcomes are different promises. A
// retryable failure keeps the pending marker, the original contract and the
// attempt count: the object may still be finished, and the next pass picks it up.
// Anything else is a verdict and becomes a terminal hold, which is visible and
// which `recover adopt` can converge. What must never happen - and what this
// replaced - is a bare state write that clears the pending marker, because then
// nothing is retried and nothing can be adopted either.
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
		// Failing to write the note does not make the object explainable. The
		// pending marker is still in place, which is the fact that matters.
		out.Outcome = "parked"
		out.Detail = detail + "; the attempt could not be recorded: " + err.Error()
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
		out.Outcome = "unknown"
		out.Detail = detail + "; the hold could not be recorded: " + err.Error()
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

	contract, err := plan.ParseContract(po.ContractJSON)
	if err != nil {
		// The contract is the record of what was meant to move. Without it there
		// is nothing to compare against, so this is a verdict, not a retry.
		return r.recordFailure(ctx, po, out, "INTENT_CONTRACT_UNREADABLE", err)
	}
	expected := contract.ExpectedIdent

	srcID, srcFound := statEntry(sc, it.RawRelative)
	vaultID, vaultFound := statVault(sc, it.ObjectName)

	switch {
	case srcFound && !vaultFound:
		// Nothing moved. Close the attempt; never replay an old plan.
		if err := r.Store.AbortIntent(ctx, intentOf(po), domain.StateAborted,
			`{"source":"reconciled","result":"source still present, vault empty"}`); err != nil {
			return r.recordFailure(ctx, po, out, "ABORT_FAILED", err)
		}
		_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)
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
		// captured identity), preserved on disk but unreachable.
		if why := r.holdCapturedObject(ctx, sc, po, out); why != "" {
			out.Detail = "the vault holds an object that is not the planned one; " + why
		} else {
			out.Detail = "the vault holds an object that is not the planned one; its evidence was recorded so it can be restored"
		}
		out.Outcome = "held"
		_ = r.Store.SetScopeStatus(ctx, sc.Scope.ID, domain.ScopeHeld)
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

// holdCapturedObject records a terminal hold together with the evidence of the
// object that actually moved.
//
// The evidence is read from the object in the vault, never from the plan: the
// plan describes what was expected, and repeating it here would produce a hold
// whose "actual" record is a copy of the mistake. It returns a description of why
// the evidence could not be read, or "" on success.
func (r *Reconciler) holdCapturedObject(ctx context.Context, sc *execute.ScopeCtx,
	po store.PendingOp, out Result) string {

	it := po.Item
	objParent, err := sc.Objects.ParentOf(it.ObjectName)
	if err != nil {
		r.holdUnfixable(ctx, po, out, "UNEXPECTED_VAULT_OBJECT",
			"the vault object cannot be opened, so the object that moved cannot be described")
		return "the object could not be opened"
	}
	defer objParent.Close()
	obj, err := fsops.OpenRegularAt(objParent, sc.RootDev)
	if err != nil {
		r.holdUnfixable(ctx, po, out, "UNEXPECTED_VAULT_OBJECT",
			"the vault object cannot be opened, so the object that moved cannot be described")
		return "the object could not be opened"
	}
	defer obj.Close()

	idJSON, _ := json.Marshal(obj.Identity)
	tokJSON, _ := json.Marshal(obj.Token)
	payload := `{"source":"reconciled","result":"vault object is not the planned object","captured_identity":` +
		quoteJSON(string(idJSON)) + `,"captured_token":` + quoteJSON(string(tokJSON)) + `}`
	if err := r.Store.HoldIntentWithCapture(ctx, intentOf(po), "UNEXPECTED_VAULT_OBJECT",
		payload, string(idJSON), string(tokJSON)); err != nil {
		return "the evidence could not be recorded: " + err.Error()
	}
	return ""
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
	// review requires to be a refusal rather than a silent pass.
	manifest, err := r.readManifest(sc, it.ObjectName)
	if err != nil {
		return "", errs.Newf(errs.CodeRecoveryRequired,
			"the manifest for %s is missing or unreadable, so the move cannot be cross-checked: %v",
			it.ID, err)
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
	out := Result{Item: it.ID, OpID: po.OpID, Action: po.Action}

	contract, dest, cerr := r.restoreContract(po)
	if cerr != nil {
		return r.recordFailure(ctx, po, out, "RESTORE_CONTRACT_UNREADABLE", cerr)
	}
	backState := priorStateOf(contract, it)

	captured, err := identityOf(ptrString(it.CapturedIdentityJSON))
	if err != nil {
		return r.recordFailure(ctx, po, out, "CAPTURED_IDENTITY_UNREADABLE", err)
	}

	destID, destFound := statEntry(sc, dest)
	vaultID, vaultFound := statVault(sc, it.ObjectName)

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
		if err := r.Store.CompleteRestore(ctx, intentOf(po), `{"source":"reconciled"}`); err != nil {
			return r.recordFailure(ctx, po, out, "COMPLETE_FAILED", err)
		}
		_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)
		out.Outcome = "completed"
		out.Detail = "the restore had already happened; its record was completed"
		return out

	case !destFound && vaultFound && vaultID.Equal(captured):
		// The restore never happened. End the attempt and return the item to the
		// state the contract says it came from: a hold stays a hold.
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
		if raw, err := decodeB64(legacy.RawRelativeB64); err == nil && len(raw) > 0 {
			dest = raw
		}
	}
	if legacy.BindingVersion == 0 {
		// Pre-versioning record. The destination is the only field it can be
		// trusted for, so that is the only thing returned.
		return nil, dest, nil
	}

	contract, err := plan.ParseContract(po.ContractJSON)
	if err != nil {
		return nil, nil, err
	}
	if contract.RawRelativeB64 != "" {
		if raw, berr := decodeB64(contract.RawRelativeB64); berr == nil && len(raw) > 0 {
			dest = raw
		}
	}
	return &contract, dest, nil
}

// priorStateOf returns the state an aborted operation goes back to.
func priorStateOf(contract *plan.OperationContractV1, it store.Item) string {
	if contract != nil {
		switch contract.PriorState {
		case domain.StateQuarantined, domain.StateHeld, domain.StateUnknown:
			return contract.PriorState
		}
	}
	// The item row is the fallback: it says where the operation started, and for
	// a restore of a quarantined object that is the only sensible answer.
	switch it.State {
	case domain.StateRIntent, domain.StateQuarantined:
		return domain.StateQuarantined
	}
	return domain.StateHeld
}

// reconcilePurge implements the P_INTENT rows of the matrix.
func (r *Reconciler) reconcilePurge(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, OpID: po.OpID, Action: "purge"}

	captured, err := identityOf(ptrString(it.CapturedIdentityJSON))
	if err != nil {
		return r.recordFailure(ctx, po, out, "CAPTURED_IDENTITY_UNREADABLE", err)
	}
	vaultID, vaultFound := statVault(sc, it.ObjectName)

	if vaultFound {
		// The unlink did not happen (or did not take effect). Return the item to
		// the state the contract says it came from so a later, fully checked
		// attempt can be made.
		if !vaultID.Equal(captured) {
			return r.holdUnfixable(ctx, po, out, "UNEXPECTED_VAULT_OBJECT",
				"the vault object is not the recorded object")
		}
		contract, _, _ := r.restoreContract(po)
		if err := r.Store.AbortIntent(ctx, intentOf(po), priorStateOf(contract, it),
			`{"source":"reconciled","result":"object still present, purge not confirmed"}`); err != nil {
			return r.recordFailure(ctx, po, out, "ABORT_FAILED", err)
		}
		out.Outcome = "aborted"
		out.Detail = "the purge did not happen; the object is still quarantined"
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
	_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)
	out.Outcome = "completed"
	out.Detail = "the object was already absent; recorded as RECONCILED_ABSENT, not as a verified unlink"
	return out
}

func (r *Reconciler) readManifest(sc *execute.ScopeCtx, objectID string) (execute.ManifestV1, error) {
	var m execute.ManifestV1
	data, err := readManifestFile(sc, objectID)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// statEntry reports whether rel exists, and its identity when it does.
// A permission error is not "absent": it returns found=true with a zero identity
// so the caller treats it as unexplainable rather than empty.
func statEntry(sc *execute.ScopeCtx, rel []byte) (domain.FileIdentity, bool) {
	parent, err := sc.Root.OpenParent(rel)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false
		}
		return domain.FileIdentity{}, true
	}
	defer parent.Close()
	id, _, err := parent.StatAtNoFollow()
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false
		}
		return domain.FileIdentity{}, true
	}
	return id, true
}

func statVault(sc *execute.ScopeCtx, name string) (domain.FileIdentity, bool) {
	parent, err := sc.Objects.ParentOf(name)
	if err != nil {
		return domain.FileIdentity{}, true
	}
	defer parent.Close()
	id, _, err := parent.StatAtNoFollow()
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return domain.FileIdentity{}, false
		}
		return domain.FileIdentity{}, true
	}
	return id, true
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
