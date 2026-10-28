package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/store"
)

// crashFixture is the state a crash between rename and commit leaves behind.
type crashFixture struct {
	env      *env
	scope    store.Scope
	item     store.Item
	plan     *store.Plan
	sctx     *execute.ScopeCtx
	vaultRaw string
	srcPath  string

	manifest execute.ManifestV1
	contract plan.OperationContractV1
	// writeManifest is false when a test wants the manifest to be absent.
	writeManifest bool
	// corruptManifest replaces the manifest body on disk when non-empty.
	corruptManifest string
}

// stageCrashedQuarantine builds a durable manifest, a committed intent and an
// already-moved file, then lets the caller adjust the records before they land.
func (e *env) stageCrashedQuarantine(t *testing.T, adjust func(*crashFixture)) *crashFixture {
	return e.stageCrashedQuarantineAt(t, "a.zip", adjust)
}

// stageCrashedQuarantineAt is stageCrashedQuarantine with a chosen relative path,
// so a case that needs the object to live in a subdirectory - "the source parent
// cannot even be opened" - does not have to rebuild the fixture.
func (e *env) stageCrashedQuarantineAt(t *testing.T, rel string, adjust func(*crashFixture)) *crashFixture {
	t.Helper()
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", rel, "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 30*24*time.Hour)
	res := e.planFor(sc, r, 100)
	it := res.Items[0]

	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)

	preID := decodeIdentity(t, it.PreIdentityJSON)
	parentID := decodeIdentity(t, it.SourceParentIdentityJSON)
	content, meta := e.stableEvidence(t, sctx, it.RawRelative)
	preTok, err := decodeToken(t, it.PreTokenJSON)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	var binding plan.ItemBindingV1
	if err := json.Unmarshal([]byte(it.BindingJSON), &binding); err != nil {
		t.Fatalf("binding: %v", err)
	}
	planIDStr := string(res.Plan.ID)
	contentStr := string(content)

	f := &crashFixture{
		env:           e,
		scope:         sc,
		item:          it,
		plan:          &res.Plan,
		sctx:          sctx,
		srcPath:       filepath.Join(e.base, "trees", "exports", rel),
		vaultRaw:      filepath.Join(e.base, "trees", "exports", ".dustkeeper", "objects", it.ObjectName),
		writeManifest: true,
		manifest: execute.ManifestV1{
			FormatVersion: 1, InstanceID: "inst_test", ScopeID: string(sc.ID),
			ObjectID: it.ObjectName, RawRelativeB64: plan.B64(it.RawRelative),
			ParentIdentity: parentID, ExpectedIdentity: preID,
			ContentHash: contentStr, MetadataHash: meta,
			PlanDigest: string(res.Plan.Digest), CreatedAtNS: e.clock.Wall().UnixNano(),
		},
		contract: plan.OperationContractV1{
			BindingVersion:  plan.BindingVersion,
			ItemID:          string(it.ID),
			ExecutionPlanID: &planIDStr,
			PriorState:      domain.StatePlanned,
			NextState:       domain.StateQIntent,
			Purpose:         plan.PurposeNormal,
			ScopeID:         string(sc.ID),
			RawRelativeB64:  plan.B64(it.RawRelative),
			ObjectName:      it.ObjectName,
			ParentIdentity:  parentID,
			ExpectedIdent:   preID,
			ExpectedToken:   preTok,
			ContentHash:     &contentStr,
			MetadataHash:    &meta,
			Binding:         binding,
			MaxFiles:        1,
			MaxLogicalBytes: 1 << 20,
		},
	}
	if adjust != nil {
		adjust(f)
	}

	if f.writeManifest {
		if err := sctx.WriteManifest(f.manifest); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	if f.corruptManifest != "" {
		path := filepath.Join(e.base, "trees", "exports", ".dustkeeper", "manifests", it.ObjectName+".json")
		if err := os.WriteFile(path, []byte(f.corruptManifest), 0o600); err != nil {
			t.Fatalf("corrupt manifest: %v", err)
		}
	}

	body, err := json.Marshal(f.contract)
	if err != nil {
		t.Fatalf("contract: %v", err)
	}
	if err := e.st.BeginIntent(context.Background(), store.Intent{
		OpID: domain.OperationID(domain.NewID("op")), ItemID: it.ID, PlanID: &res.Plan.ID,
		Action: "quarantine", PriorState: domain.StatePlanned, NextState: domain.StateQIntent,
		ContractJSON: string(body), OccurredAtNS: e.clock.Wall().UnixNano(),
	}); err != nil {
		t.Fatalf("begin intent: %v", err)
	}
	if err := os.Rename(f.srcPath, f.vaultRaw); err != nil {
		t.Fatalf("rename into the vault: %v", err)
	}
	return f
}

// V12: a crashed quarantine whose manifest is missing must not be completed from
// whatever happens to be on disk. The object stays put and stays visible.
func TestReconcileRefusesAMissingManifest(t *testing.T) {
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, func(f *crashFixture) { f.writeManifest = false })

	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 || rep.Held != 1 {
		t.Fatalf("a missing manifest was treated as a completion: %+v", rep)
	}
	if _, err := os.Stat(f.vaultRaw); err != nil {
		t.Fatalf("the quarantined object was moved or removed: %v", err)
	}
	got, err := e.st.GetItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.State == domain.StateQuarantined {
		t.Fatalf("the item was completed without cross-checkable evidence: %s", got.State)
	}
	// A missing manifest will not reappear by itself, so this is a verdict rather
	// than a retry: the item is held, which is visible, which keeps the object in
	// the vault, and which `recover adopt` can converge. Clearing the pending
	// marker without a verdict is what used to leave the object unreachable.
	if got.State != domain.StateHeld {
		t.Fatalf("the item is %s; an unprovable move must be held rather than dropped or completed", got.State)
	}
	if got.PendingOpID != nil {
		t.Fatal("a held item still claims an open operation")
	}
	if got.HoldCode == nil || *got.HoldCode == "" {
		t.Fatal("the hold records no reason")
	}
}

// V12: a manifest that cannot be parsed is the same refusal as a missing one.
func TestReconcileRefusesACorruptManifest(t *testing.T) {
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, func(f *crashFixture) { f.corruptManifest = "{not json" })

	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 {
		t.Fatalf("a corrupt manifest was treated as a completion: %+v", rep)
	}
	if _, err := os.Stat(f.vaultRaw); err != nil {
		t.Fatalf("the quarantined object was moved or removed: %v", err)
	}
}

// V13: a manifest whose content hash disagrees with the intent contract is a
// contradiction, not something to average out.
func TestReconcileRefusesDisagreeingEvidence(t *testing.T) {
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, func(f *crashFixture) {
		f.manifest.ContentHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	})

	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 {
		t.Fatalf("two disagreeing content hashes produced a completion: %+v", rep)
	}
	if _, err := os.Stat(f.vaultRaw); err != nil {
		t.Fatalf("the quarantined object was moved or removed: %v", err)
	}
}

// V10: an intent contract that never recorded the evidence cannot be completed.
// R08 A wrote an empty hash in this case, which made the object permanently
// unpurgeable because purge recomputes a non-empty digest.
func TestReconcileRefusesAContractWithoutEvidence(t *testing.T) {
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, func(f *crashFixture) {
		f.contract.ContentHash = nil
		f.contract.MetadataHash = nil
		f.manifest.ContentHash = ""
		f.manifest.MetadataHash = ""
	})

	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 0 {
		t.Fatalf("a contract with no recorded evidence produced a completion: %+v", rep)
	}
	got, err := e.st.GetItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.ContentHash != nil && *got.ContentHash == "" {
		t.Fatal("an empty content hash was written as if it were evidence")
	}
}

// V12: once the records are intact, reconciliation completes and the object
// continues on to a real, authorised purge at the end of the retention window.
func TestReconciledQuarantineStillReachesPurge(t *testing.T) {
	e := newEnv(t)
	f := e.stageCrashedQuarantine(t, nil)

	rep, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.Completed != 1 {
		t.Fatalf("expected one completion, got %+v", rep)
	}
	got, err := e.st.GetItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.PurgeNotBeforeNS == nil {
		t.Fatal("reconciliation did not set a retention deadline")
	}

	// A second pass must be a no-op: reconciliation is idempotent.
	again, err := e.rec.Run(context.Background())
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if again.Examined != 0 {
		t.Fatalf("the second pass still saw pending work: %+v", again)
	}

	e.clock.Advance(31 * 24 * time.Hour)
	due, err := e.st.ItemsDueForPurge(context.Background(), f.scope.ID, e.clock.Wall().UnixNano(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("the reconciled object never became due: %d", len(due))
	}
	p, err := plan.CreatePurgePlan(context.Background(), e.st, f.scope, due, domain.PlanManualPurge,
		e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
	if err != nil {
		t.Fatalf("purge plan: %v", err)
	}
	out, err := e.exec.ApplyPurgePlan(context.Background(), f.sctx, p.ID, p.Digest,
		domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
	if err != nil {
		t.Fatalf("purge after reconciliation: %v", err)
	}
	if out.Completed != 1 {
		t.Fatalf("the reconciled object was not purged: %+v", out)
	}
	if _, err := os.Stat(f.vaultRaw); !os.IsNotExist(err) {
		t.Fatalf("the purged object is still on disk: %v", err)
	}
}
