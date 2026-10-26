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
func (r *Reconciler) reconcileQuarantine(ctx context.Context, sc *execute.ScopeCtx, po store.PendingOp) Result {
	it := po.Item
	out := Result{Item: it.ID, Action: "quarantine"}

	expected, err := identityOf(it.PreIdentityJSON)
	if err != nil {
		return r.markUnknown(ctx, it.ID, out, "ITEM_IDENTITY_UNREADABLE")
	}
	expectedTok, err := tokenOf(it.PreTokenJSON)
	if err != nil {
		return r.markUnknown(ctx, it.ID, out, "ITEM_TOKEN_UNREADABLE")
	}

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
		completion, err := r.completeQuarantine(ctx, sc, po, expected, expectedTok)
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
			completion, err := r.completeQuarantine(ctx, sc, po, expected, expectedTok)
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

func (r *Reconciler) completeQuarantine(ctx context.Context, sc *execute.ScopeCtx,
	po store.PendingOp, expected domain.FileIdentity, expectedTok domain.ChangeToken) (string, error) {

	it := po.Item
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

	if !obj.Identity.Equal(expected) {
		return "", errs.New(errs.CodeHeld, "vault object identity differs")
	}
	// Re-establish that the bytes are what the manifest recorded.
	opt := r.Exec.VerifyOpt(obj.Token.Size)
	hash, _, err := execute.HashFile(obj, opt)
	if err != nil {
		return "", err
	}
	ok := true
	if m, mErr := r.readManifest(sc, it.ObjectName); mErr == nil && m.ContentHash != "" {
		ok = string(hash) == m.ContentHash
	}
	if !ok {
		return "", errs.New(errs.CodeHeld, "vault object content differs from the manifest")
	}

	if err := fsops.DurableFile(obj.FD()); err != nil {
		return "", err
	}
	if err := fsops.DurableDir(sc.Objects); err != nil {
		return "", err
	}
	if srcParent, perr := sc.Root.OpenParent(it.RawRelative); perr == nil {
		_ = fsops.DurableDir(srcParent)
		srcParent.Close()
	}

	// Retention starts now, from a verified completion. Reconciliation must
	// never backdate it to a moment it cannot prove.
	nowNS := r.Clock.Wall().UnixNano()
	purgeNotBefore, ok2 := domain.AddSecondsNS(nowNS, it.BoundRetentionSeconds)
	if !ok2 {
		return "", errs.New(errs.CodeHeld, "retention overflow")
	}
	idJSON, _ := json.Marshal(obj.Identity)
	tokJSON, _ := json.Marshal(obj.Token)
	metaHash := ""
	if it.MetadataHash != nil {
		metaHash = *it.MetadataHash
	}
	if err := r.Store.CompleteQuarantine(ctx, intentOf(po), store.QuarantineResult{
		CapturedIdentityJSON: string(idJSON),
		CapturedTokenJSON:    string(tokJSON),
		QuarantinedAtNS:      nowNS,
		PurgeNotBeforeNS:     purgeNotBefore,
		ContentHash:          string(hash),
		MetadataHash:         metaHash,
	}, `{"source":"reconciled","result":"RECONCILED_OBSERVED"}`); err != nil {
		return "", err
	}
	_ = expectedTok
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
		if err := fsops.DurableDir(sc.Objects); err != nil {
			return r.markUnknown(ctx, it.ID, out, "SYNC_FAILED")
		}
		if parent, err := sc.Root.OpenParent(dest); err == nil {
			_ = fsops.DurableDir(parent)
			parent.Close()
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
	if err := fsops.DurableDir(sc.Objects); err != nil {
		return r.markUnknown(ctx, it.ID, out, "SYNC_FAILED")
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
