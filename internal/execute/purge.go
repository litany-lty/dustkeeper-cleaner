package execute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/store"
)

// TrustFunc asserts that the wall clock may be used to prove that a retention
// period elapsed. It returns nil only when it may.
//
// A boolean was the wrong shape for this: `purge apply` passed `true` and
// thereby skipped the check that the background path performed. A function
// cannot be hardcoded to "safe" without someone writing that intent into the
// call site, where a reviewer can see it.
type TrustFunc func(ctx context.Context) error

// PurgeOptions describes one purge request.
type PurgeOptions struct {
	Automatic bool
	Plan      *store.Plan
	Budget    *domain.Budget
	// Trust is mandatory. A nil Trust refuses the purge: "no assertion was made"
	// is not the same as "the clock is fine".
	Trust TrustFunc
}

// Purge permanently removes one quarantined object.
//
// It only ever unlinks a generated name inside the private vault. The original
// relative path is never passed to unlink, and every condition is re-checked at
// the last moment: the retention window having elapsed is not permission.
func (e *Executor) Purge(ctx context.Context, sc *ScopeCtx, it store.Item, opt PurgeOptions) (domain.OperationResult, error) {
	res := domain.OperationResult{Item: it.ID, State: it.State}

	// The purge path is the last place an old duplicate could still be deleted.
	// The detector is gone from the call graph, but its objects and plans are
	// still in the database, so the refusal lives here rather than in the command
	// that no longer exists.
	if err := requireLifecycleItem(it); err != nil {
		return res, err
	}
	if it.State != domain.StateQuarantined {
		return res, errs.Newf(errs.CodePlanStale, "item %s is %s, not quarantined", it.ID, it.State)
	}
	if opt.Trust == nil {
		return res, errs.New(errs.CodeClockUntrusted,
			"no clock assertion was supplied; a permanent deletion requires one on every entry point")
	}
	if err := opt.Trust(ctx); err != nil {
		return res, err
	}
	if err := e.scopeGate(ctx, sc); err != nil {
		return res, err
	}
	nowNS := e.Clock.Wall().UnixNano()
	if it.PurgeNotBeforeNS == nil || nowNS < *it.PurgeNotBeforeNS {
		return res, errs.New(errs.CodePlanStale, "the retention period for this object has not elapsed")
	}
	if it.CapturedIdentityJSON == nil || it.CapturedTokenJSON == nil || it.ContentHash == nil {
		return res, errs.New(errs.CodeHeld, "this object has no complete post-move evidence; reconcile first")
	}

	binding, err := PlanBindingOf(it)
	if err != nil {
		return res, err
	}

	// Authority. A manual purge needs an approved plan bound to the current
	// policy; an automatic purge needs the grant that was bound at quarantine
	// time, and that grant must still be active.
	if opt.Automatic {
		if err := e.checkAutomaticAuthority(ctx, sc, it, binding); err != nil {
			return res, err
		}
	} else {
		if opt.Plan == nil {
			return res, errs.New(errs.CodeGrantRequired, "a purge requires an explicit plan")
		}
		if opt.Plan.Kind != domain.PlanManualPurge && opt.Plan.Kind != domain.PlanScheduledPurge {
			return res, errs.New(errs.CodePlanStale, "this plan is not a purge plan")
		}
	}

	// Protection is checked against the recorded original path, not against a
	// path that currently exists. A missing file does not mean the protection
	// went away.
	if blocked, why, err := e.purgeBlockedByProtection(ctx, sc, it); err != nil {
		return res, err
	} else if blocked {
		return res, errs.Newf(errs.CodeHeld, "the original path is protected: %s", why)
	}

	capturedID, err := decodeID(*it.CapturedIdentityJSON)
	if err != nil {
		return res, err
	}
	capturedTok, err := decodeTok(*it.CapturedTokenJSON)
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
			"the quarantined object is missing or unreadable: %v; reconcile before purging", err)
	}
	defer obj.Close()

	if !obj.Identity.Equal(capturedID) {
		return res, errs.New(errs.CodeHeld, "the quarantined object is not the recorded object")
	}
	if obj.Token.Size != capturedTok.Size || obj.Token.MTimeNS != capturedTok.MTimeNS {
		return res, errs.New(errs.CodeHeld, "the quarantined object changed after it was isolated")
	}

	// Metadata is re-established inside ReadStable, together with the trailing
	// identity check; doing it here as well would read the same evidence twice.

	if opt.Budget != nil && !opt.Budget.CanReserve(obj.Token.Size) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the purge allowance for this run does not permit another action; nothing was read")
	}
	// The soft deadline is checked before the expensive read, not after it.
	if opt.Budget != nil && opt.Budget.TimeUp(e.Clock) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the run's time slice was spent before this object was re-verified")
	}
	if opt.Budget != nil && opt.Budget.RemainingRead() < obj.Token.Size {
		return res, errs.New(errs.CodeBudgetExhausted, "not enough read budget to re-verify this object")
	}

	// One stable-evidence read: identity and metadata before, hash, then identity
	// and metadata again. A writer that changes an already-consumed block makes
	// the streamed hash match the old content while the file no longer does; the
	// trailing comparison is what notices, and the purge path used to lack it.
	vopt := e.VerifyOpt(obj.Token.Size)
	vopt.Budget = opt.Budget
	ev, err := ReadStable(obj, vopt)
	if err != nil {
		if errors.Is(err, ErrUnstableObject) {
			return res, errs.Newf(errs.CodeHeld,
				"the quarantined object changed while it was being verified: %v", err)
		}
		return res, err
	}
	if string(ev.Content) != *it.ContentHash {
		return res, errs.New(errs.CodeHeld, "the quarantined object's content changed")
	}
	if it.MetadataHash != nil && string(ev.Metadata) != *it.MetadataHash {
		return res, errs.New(errs.CodeHeld, "the quarantined object's metadata changed")
	}

	if opt.Budget != nil && !opt.Budget.CanReserve(obj.Token.Size) {
		return res, errs.New(errs.CodeBudgetExhausted, "the action budget for this run is exhausted")
	}

	// Last look before the unlink. It narrows the window between verification and
	// deletion; it cannot close it, and the design says so rather than implying
	// an atomicity the syscall does not offer.
	if err := ConfirmUnchanged(obj, ev); err != nil {
		return res, err
	}

	// Both limits are re-checked at the last possible moment, because both can
	// expire during the read: a clock that jumped while the object was being
	// verified, and a time slice that ran out. The first check happened before any
	// content was read; this one is immediately before the intent that authorises
	// the unlink.
	if err := opt.Trust(ctx); err != nil {
		return res, err
	}
	if opt.Budget != nil && opt.Budget.TimeUp(e.Clock) {
		return res, errs.New(errs.CodeBudgetExhausted,
			"the run's time slice ran out while this object was being verified; nothing was removed")
	}

	opID := domain.OperationID(domain.NewID("op"))
	contract := plan.OperationContractV1{
		BindingVersion:  plan.BindingVersion,
		ItemID:          string(it.ID),
		PriorState:      domain.StateQuarantined,
		NextState:       domain.StatePIntent,
		Purpose:         plan.PurposeNormal,
		ScopeID:         string(sc.Scope.ID),
		RawRelativeB64:  plan.B64(it.RawRelative),
		ObjectName:      it.ObjectName,
		ExpectedIdent:   capturedID,
		ExpectedToken:   capturedTok,
		ContentHash:     it.ContentHash,
		MetadataHash:    it.MetadataHash,
		Binding:         binding,
		MaxFiles:        e.Config.MaxFiles,
		MaxLogicalBytes: e.Config.MaxBytes,
	}
	if opt.Plan != nil {
		contract.ExecutionPlanID = strPtr(string(opt.Plan.ID))
	}
	contractJSON, err := json.Marshal(contract)
	if err != nil {
		return res, errs.Wrap(errs.CodeStateWriteFailed, err)
	}

	intent := store.Intent{
		OpID:         opID,
		ItemID:       it.ID,
		Action:       "purge",
		PriorState:   domain.StateQuarantined,
		NextState:    domain.StatePIntent,
		ContractJSON: string(contractJSON),
		OccurredAtNS: nowNS,
	}
	if opt.Plan != nil {
		intent.PlanID = &opt.Plan.ID
	}

	// The pause gate also covers purging: a pause request must be able to stop
	// the next permanent deletion, not only the next quarantine.
	if err := e.Gate.CheckAndCommit(func() error { return e.Store.BeginIntent(ctx, intent) }); err != nil {
		return res, err
	}

	if err := fsops.UnlinkAt(sc.Objects, it.ObjectName); err != nil {
		if errors.Is(err, unix.ENOENT) {
			// The object vanished without our unlink; that is not a purge we
			// performed, so it must be reconciled rather than declared success.
			herr := e.Store.HoldIntent(ctx, intent, "OBJECT_VANISHED",
				`{"source":"observed","result":"object was gone before unlink"}`)
			return res, recordOr(herr, errs.New(errs.CodeHeld, "the object was already gone; reconcile this item"))
		}
		aerr := e.Store.AbortIntent(ctx, intent, domain.StateQuarantined,
			`{"reason":"unlink failed","detail":`+quoteJSON(err.Error())+`}`)
		return res, recordOr(aerr,
			errs.Newf(errs.CodeIOError, "unlink failed; the object is still quarantined: %v", err))
	}

	if err := fsops.DurableDir(sc.Objects); err != nil {
		return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
	}
	if sc.AnchorFD() >= 0 {
		if err := fsops.Barrier(sc.AnchorFD(), []byte("purge")); err != nil {
			return res, e.park(ctx, intent, "DURABILITY_FAILED", err)
		}
	}

	if err := runBeforeCompleteHook("purge"); err != nil {
		return res, errs.Newf(errs.CodeStateWriteFailed,
			"the object was deleted but its record could not be closed: %v; the operation stays pending for `recover reconcile`", err)
	}
	if err := e.Store.CompletePurge(ctx, intent, `{"source":"observed"}`); err != nil {
		return res, err
	}
	if err := e.Store.ReleaseItemKeeperPin(ctx, it.ID); err != nil {
		return res, err
	}

	// The handle stays open until the directory is synced; only then is the
	// space actually released for observers.
	_ = obj.Close()

	if opt.Budget != nil {
		opt.Budget.AddAction(obj.Token.Size)
	}

	res.State = domain.StatePurged
	res.Code = string(errs.CodeOK)
	res.Operation = opID
	return res, nil
}

// checkAutomaticAuthority verifies the standing grant that allows unattended
// permanent deletion, and refuses the history that never had one.
func (e *Executor) checkAutomaticAuthority(ctx context.Context, sc *ScopeCtx, it store.Item, b plan.ItemBindingV1) error {
	if it.SourceKind != domain.SourceScheduledRule {
		return errs.New(errs.CodeGrantRequired,
			"this object was isolated by a manual plan; automatic purge does not extend to it")
	}
	if it.AutoPurgeGrantID == nil {
		return errs.New(errs.CodeGrantRequired,
			"this object was isolated without an auto-purge grant; grant it is not retroactive")
	}
	if sc.Scope.Kind != domain.KindManagedDisposable {
		return errs.Newf(errs.CodeGrantRequired,
			"scope %s is %s; automatic purge requires managed_disposable", sc.Scope.ID, sc.Scope.Kind)
	}
	g, err := e.Store.GetGrant(ctx, *it.AutoPurgeGrantID)
	if err != nil {
		return err
	}
	if g.Status != domain.GrantActive {
		return errs.Newf(errs.CodeGrantRequired, "the auto-purge grant is %s", g.Status)
	}
	if g.Capability != domain.CapAutoPurge {
		return errs.New(errs.CodeGrantRequired, "the bound grant is not an auto-purge grant")
	}
	if b.RuleID == nil || b.RuleRevision == nil {
		return errs.New(errs.CodeGrantRequired, "this item has no rule binding")
	}
	if string(g.RuleID) != *b.RuleID || g.RuleRevision != *b.RuleRevision {
		return errs.New(errs.CodeGrantRequired,
			"the grant is bound to a different rule revision than this object")
	}
	if g.ScopeID != sc.Scope.ID {
		return errs.New(errs.CodeGrantRequired, "the grant belongs to a different scope")
	}

	// The rule that produced this object must still be the active revision.
	active, err := e.Store.ActiveRule(ctx, domain.RuleID(*b.RuleID))
	if err != nil {
		return errs.New(errs.CodeGrantRequired, "the rule for this object is no longer active")
	}
	if active.Revision != *b.RuleRevision {
		return errs.New(errs.CodeGrantRequired,
			"the rule was revised after this object was isolated; the old grant does not carry over")
	}
	if b.RuleDigest != nil && string(active.Digest) != *b.RuleDigest {
		return errs.New(errs.CodeGrantRequired, "the rule content changed after this object was isolated")
	}
	if b.BoundRetentionSeconds != it.BoundRetentionSeconds {
		return errs.New(errs.CodeGrantRequired, "the retention bound on this item is inconsistent")
	}
	return nil
}

// purgeBlockedByProtection evaluates protections against the recorded original
// path slot.
//
// The path usually no longer exists, so "ENOENT means no protection" would be
// exactly wrong. Four entries, in this order:
//
//  1. the recorded path compared without touching the filesystem (exact match
//     or full segment prefix), which holds even though nothing is there any
//     more — this is the same judgement planning and quarantine make;
//  2. the very object now in the vault: a protection that was recorded for it
//     still names it, whatever spelling was used;
//  3. the directories above the slot, by kernel identity: on a case-insensitive
//     volume "folder" and "Folder" are one directory, and only the kernel's
//     identity says so. A subtree protection whose root is one of those
//     ancestors protects this slot;
//  4. the same directory with a leaf that cannot be resolved. A different
//     spelling that the kernel might still call the same name — case, or the
//     NFC/NFD pair — is an unresolved alias, not a stranger.
//
// When the original directory itself cannot be resolved, nothing can be
// compared by identity any more. The answer is then PROTECTION_ALIAS_UNRESOLVED
// rather than a guess: freezing a scope is recoverable, deleting a file that a
// protection was meant to keep is not.
func (e *Executor) purgeBlockedByProtection(ctx context.Context, sc *ScopeCtx, it store.Item) (bool, string, error) {
	rel := string(it.RawRelative)

	// The guard is rebuilt from the store, never taken from the executor. An
	// executor that planned earlier in this process can hold a guard built before
	// the protection being asked about existed, and reading that stale answer here
	// deletes a file the user has since protected. The cost is one resolution per
	// protection per purge, which is small next to a permanent deletion.
	guard, err := BuildGuard(ctx, e.Store, sc)
	if err != nil {
		return false, "", err
	}

	// 1. The recorded path, as a name.
	if ok, why := guard.CheckNameOnly(rel); ok {
		return true, "the recorded path is still covered: " + why, nil
	}
	// A project tree protects everything inside it, including for a slot that
	// currently holds nothing. A directory that cannot be inspected is not
	// evidence that no marker is there: the walk returns an error for anything
	// but ENOENT, and this path must not read that as "not protected". A purge is
	// the one action that cannot be undone.
	if inside, why, perr := e.projectProtected(sc, it.RawRelative); perr != nil {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the directories above %q cannot all be inspected, so a project tree cannot be ruled out: %v", rel, perr)
	} else if inside {
		return true, why, nil
	}
	if guard.Empty() {
		return false, "", nil
	}

	var captured domain.FileIdentity
	if it.CapturedIdentityJSON != nil {
		var cerr error
		if captured, cerr = decodeID(*it.CapturedIdentityJSON); cerr != nil {
			return false, "", cerr
		}
	}
	// 2. A protection recorded for the object that now sits in the vault.
	if ok, why := guard.CheckExact(rel, captured); ok {
		return true, why + " (it names the object now in the vault)", nil
	}

	storedParent, err := decodeID(it.SourceParentIdentityJSON)
	if err != nil {
		return false, "", err
	}

	itemParent, err := sc.Root.OpenParent(it.RawRelative)
	if err != nil {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the original directory of %q cannot be resolved, so protections cannot be compared by identity; resolve or remove them before purging", rel)
	}
	defer itemParent.Close()
	itemParentID, _, err := itemParent.Stat()
	if err != nil {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the original directory of %q cannot be inspected: %v", rel, err)
	}
	if !itemParentID.Equal(storedParent) {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the original directory of %q was replaced, so protections cannot be matched by identity", rel)
	}

	// 3. The directories above the slot. A subtree root spelled differently at
	// any level is still found, because the kernel resolved the spelling.
	ancestors, aerr := ancestorIdentities(sc, it.RawRelative)
	if aerr != nil {
		return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
			"the directories above %q cannot all be resolved, so subtree protections cannot be compared by identity: %v", rel, aerr)
	}
	if ok, why := guard.CheckAncestors(ancestors); ok {
		return true, why + " (this slot is inside it)", nil
	}

	// 4. The same directory, and a leaf that has to be resolved by hand.
	return e.slotAliasAmbiguity(ctx, sc, rel, itemParentID, captured)
}

// slotAliasAmbiguity reports whether an exact protection whose parent is the
// slot's own directory leaves this slot unresolved.
//
// A protection with the same leaf spelling protects the slot outright, whatever
// currently occupies it: the protection was recorded for this name in this
// directory, and a new file appearing there does not make the protection
// somebody else's.
//
// A different leaf spelling is reported as PROTECTION_ALIAS_UNRESOLVED when the
// two spellings could name one entry (case, or Unicode normalisation). When the
// protection's entry still exists and is the captured object, the kernel has
// already proven the alias, and the slot is protected.
func (e *Executor) slotAliasAmbiguity(ctx context.Context, sc *ScopeCtx, rel string,
	itemParentID domain.FileIdentity, captured domain.FileIdentity) (bool, string, error) {
	ps, err := e.Store.ListProtections(ctx, sc.Scope.ID, true)
	if err != nil {
		return false, "", err
	}
	base := baseOf(rel)

	for _, p := range ps {
		if p.MatchKind != domain.MatchExact {
			continue
		}
		target := strings.TrimSuffix(string(p.RawRelative), "/")
		if target == "" {
			continue
		}
		parent, perr := sc.Root.OpenParent([]byte(target))
		if perr != nil {
			// This protection's own parent is gone; it says nothing about the
			// slot being purged, so it is not evidence either way.
			continue
		}
		pid, _, serr := parent.Stat()
		if serr != nil {
			parent.Close()
			continue
		}
		if !pid.Equal(itemParentID) {
			parent.Close()
			continue
		}
		targetBase := baseOf(target)
		entryID, _, eerr := parent.StatAtNoFollow()
		parent.Close()

		if targetBase == base {
			return true, "the original path slot is protected by " + p.Source, nil
		}
		if eerr == nil && !captured.IsZero() && entryID.Equal(captured) {
			return true, "an equivalent spelling of this path is protected by " + p.Source, nil
		}
		if policy.SpellingsMayCollide(base, targetBase) {
			return true, "", errs.Newf(errs.CodeProtectionAliasUnresolved,
				"a protection in the same directory (%q) differs only in spelling; resolve or remove it before purging", target)
		}
	}
	return false, "", nil
}

// baseOf returns the last path segment of a relative path.
func baseOf(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
