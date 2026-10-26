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
	Item    domain.ItemID `json:"item_id"`
	Action  string        `json:"action"`
	Outcome string        `json:"outcome"`
	Detail  string        `json:"detail,omitempty"`
}

// Report summarises a reconciliation pass.
type Report struct {
	Examined  int      `json:"examined"`
	Completed int      `json:"completed"`
	Aborted   int      `json:"aborted"`
	Held      int      `json:"held"`
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
				// A scope whose root cannot be verified is held, not guessed at.
				_ = r.Store.SetScopeStatus(ctx, po.Item.ScopeID, domain.ScopeHeld)
				_ = r.Store.MarkItemState(ctx, po.Item.ID, domain.StateUnknown, "SCOPE_UNVERIFIABLE")
				rep.Unknown++
				rep.Results = append(rep.Results, Result{Item: po.Item.ID, Action: po.Action,
					Outcome: "unknown", Detail: "scope root could not be verified"})
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
		case "unknown":
			rep.Unknown++
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

func (r *Reconciler) reconcileOne(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, Action: po.Action}

	switch po.Action {
	case "quarantine":
		return r.reconcileQuarantine(ctx, sc, po)
	case "restore", "compensate":
		return r.reconcileRestore(ctx, sc, po)
	case "purge":
		return r.reconcilePurge(ctx, sc, po)
	default:
		_ = r.Store.MarkItemState(ctx, it.ID, domain.StateUnknown, "UNKNOWN_ACTION")
		out.Outcome = "unknown"
		out.Detail = "the pending operation has no recorded action"
		return out
	}
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
	out := Result{Item: it.ID, Action: "quarantine"}

	contract, err := plan.ParseContract(po.ContractJSON)
	if err != nil {
		return r.markUnknown(ctx, it.ID, out, "INTENT_CONTRACT_UNREADABLE: "+err.Error())
	}
	expected := contract.ExpectedIdent

	srcID, srcFound := statEntry(sc, it.RawRelative)
	vaultID, vaultFound := statVault(sc, it.ObjectName)

	switch {
	case srcFound && !vaultFound:
		// Nothing moved. Close the attempt; never replay an old plan.
		if err := r.Store.AbortIntent(ctx, intentOf(po), domain.StateAborted,
			`{"source":"reconciled","result":"source still present, vault empty"}`); err != nil {
			return r.markUnknown(ctx, it.ID, out, "ABORT_FAILED")
		}
		_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)
		out.Outcome = "aborted"
		out.Detail = "the file was never moved"
		return out

	case !srcFound && vaultFound && vaultID.Equal(expected) && srcID.IsZero():
		// The move happened but was not recorded. Complete it after re-verifying.
		completion, err := r.completeQuarantine(ctx, sc, po, contract)
		if err != nil {
			return r.markUnknown(ctx, it.ID, out, "COMPLETION_VERIFY_FAILED: "+err.Error())
		}
		out.Outcome = "completed"
		out.Detail = completion
		return out

	case srcFound && vaultFound && vaultID.Equal(expected) && srcID.Equal(expected):
		// The same object is in two places, which the single-link admission rule
		// forbids. This is not a state to repair automatically.
		_ = r.Store.HoldIntent(ctx, intentOf(po), "DUPLICATE_LINK_STATE",
			`{"source":"reconciled","result":"object present at source and in vault"}`)
		out.Outcome = "held"
		out.Detail = "the object appears both at the source and in the vault"
		return out

	case vaultFound && !vaultID.Equal(expected):
		_ = r.Store.HoldIntent(ctx, intentOf(po), "UNEXPECTED_VAULT_OBJECT",
			`{"source":"reconciled","result":"vault object is not the recorded object"}`)
		_ = r.Store.SetScopeStatus(ctx, sc.Scope.ID, domain.ScopeHeld)
		out.Outcome = "held"
		out.Detail = "the vault holds an object that is not the planned one"
		return out

	case !srcFound && !vaultFound:
		_ = r.Store.MarkItemState(ctx, it.ID, domain.StateUnknown, "OBJECT_MISSING_BOTH_PLACES")
		out.Outcome = "unknown"
		out.Detail = "neither the source nor the vault object exists; this cannot be explained"
		return out

	default:
		// A different object now sits at the source path. The vault object is
		// intact, so the quarantine itself can be completed without touching the
		// new file.
		if vaultFound && vaultID.Equal(expected) {
			completion, err := r.completeQuarantine(ctx, sc, po, contract)
			if err != nil {
				return r.markUnknown(ctx, it.ID, out, "COMPLETION_VERIFY_FAILED: "+err.Error())
			}
			out.Outcome = "completed"
			out.Detail = completion + "; a new file at the original path was left untouched"
			return out
		}
		return r.markUnknown(ctx, it.ID, out, "UNEXPLAINED_QUARANTINE_STATE")
	}
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
func (r *Reconciler) reconcileRestore(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, Action: po.Action}

	var contract struct {
		RawRelativeB64 string `json:"raw_relative_b64"`
	}
	_ = json.Unmarshal([]byte(po.ContractJSON), &contract)
	dest := it.RawRelative
	if contract.RawRelativeB64 != "" {
		if raw, err := decodeB64(contract.RawRelativeB64); err == nil && len(raw) > 0 {
			dest = raw
		}
	}

	captured, err := identityOf(ptrString(it.CapturedIdentityJSON))
	if err != nil {
		return r.markUnknown(ctx, it.ID, out, "CAPTURED_IDENTITY_UNREADABLE")
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
			return r.markUnknown(ctx, it.ID, out, "DEST_PARENT_UNAVAILABLE")
		}
		defer destParent.Close()
		des, oerr := fsops.OpenRegularAt(destParent, sc.RootDev)
		if oerr != nil {
			return r.markUnknown(ctx, it.ID, out, "DEST_OPEN_FAILED")
		}
		defer des.Close()
		if serr := execute.SyncAfterMove(des.FD(), sc.Objects, destParent,
			sc.AnchorFD(), "reconcile-restore"); serr != nil {
			return r.markUnknown(ctx, it.ID, out, "SYNC_FAILED: "+serr.Error())
		}
		if err := r.Store.CompleteRestore(ctx, intentOf(po), `{"source":"reconciled"}`); err != nil {
			return r.markUnknown(ctx, it.ID, out, "COMPLETE_FAILED")
		}
		_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)
		out.Outcome = "completed"
		out.Detail = "the restore had already happened; its record was completed"
		return out

	case !destFound && vaultFound && vaultID.Equal(captured):
		// The restore never happened. End the attempt; the object stays safe.
		if err := r.Store.AbortIntent(ctx, intentOf(po), domain.StateQuarantined,
			`{"source":"reconciled","result":"destination empty, object still quarantined"}`); err != nil {
			return r.markUnknown(ctx, it.ID, out, "ABORT_FAILED")
		}
		out.Outcome = "aborted"
		out.Detail = "the restore did not happen; the object is still quarantined"
		return out

	case destFound && vaultFound:
		_ = r.Store.HoldIntent(ctx, intentOf(po), "RESTORE_CONFLICT",
			`{"source":"reconciled","result":"destination occupied and object still in vault"}`)
		out.Outcome = "held"
		out.Detail = "both the destination and the vault object exist; nothing was overwritten"
		return out

	case !destFound && !vaultFound:
		_ = r.Store.MarkItemState(ctx, it.ID, domain.StateUnknown, "RESTORE_UNEXPLAINED")
		out.Outcome = "unknown"
		out.Detail = "neither the destination nor the vault object exists"
		return out

	default:
		_ = r.Store.HoldIntent(ctx, intentOf(po), "RESTORE_UNEXPLAINED",
			`{"source":"reconciled","result":"state does not match the matrix"}`)
		out.Outcome = "held"
		out.Detail = "the observed state does not match any explainable restore outcome"
		return out
	}
}

// reconcilePurge implements the P_INTENT rows of the matrix.
func (r *Reconciler) reconcilePurge(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, Action: "purge"}

	captured, err := identityOf(ptrString(it.CapturedIdentityJSON))
	if err != nil {
		return r.markUnknown(ctx, it.ID, out, "CAPTURED_IDENTITY_UNREADABLE")
	}
	vaultID, vaultFound := statVault(sc, it.ObjectName)

	if vaultFound {
		// The unlink did not happen (or did not take effect). Return the item to
		// quarantined so a later, fully checked attempt can be made.
		if !vaultID.Equal(captured) {
			_ = r.Store.HoldIntent(ctx, intentOf(po), "UNEXPECTED_VAULT_OBJECT",
				`{"source":"reconciled","result":"vault object changed"}`)
			out.Outcome = "held"
			out.Detail = "the vault object is not the recorded object"
			return out
		}
		if err := r.Store.AbortIntent(ctx, intentOf(po), domain.StateQuarantined,
			`{"source":"reconciled","result":"object still present, purge not confirmed"}`); err != nil {
			return r.markUnknown(ctx, it.ID, out, "ABORT_FAILED")
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
		return r.markUnknown(ctx, it.ID, out, "SYNC_FAILED: "+err.Error())
	}
	if err := r.Store.CompletePurge(ctx, intentOf(po),
		`{"source":"reconciled","result":"RECONCILED_ABSENT"}`); err != nil {
		return r.markUnknown(ctx, it.ID, out, "COMPLETE_FAILED")
	}
	_ = r.Store.ReleaseItemKeeperPin(ctx, it.ID)
	out.Outcome = "completed"
	out.Detail = "the object was already absent; recorded as RECONCILED_ABSENT, not as a verified unlink"
	return out
}

func (r *Reconciler) markUnknown(ctx context.Context, id domain.ItemID, out Result, code string) Result {
	_ = r.Store.MarkItemState(ctx, id, domain.StateUnknown, code)
	out.Outcome = "unknown"
	out.Detail = code
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
