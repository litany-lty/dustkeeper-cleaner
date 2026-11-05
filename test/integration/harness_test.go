// Package integration exercises the whole pipeline against a real filesystem.
//
// Every test here uses a temporary directory, the real SQLite store, the real
// kernel calls and a fake clock. Nothing is mocked: the point is to catch the
// cases where the parts disagree with each other.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/notify"
	"dustkeeper/internal/outcome"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/recovery"
	"dustkeeper/internal/review"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/run"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// fixedNow is the wall clock every test starts from.
var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// trustOK is the clock assertion a test supplies when it has already decided the
// clock is fine (the fake clock never moves backwards). Production code goes
// through clocktrust.Guard.Assert instead; the purge path requires an explicit
// assertion so that "nobody checked" can never be mistaken for "trusted".
func trustOK(context.Context) error { return nil }

// trustFails is the assertion a test supplies when it wants the purge path to
// refuse because the clock is not trustworthy.
func trustFails(context.Context) error {
	return errs.New(errs.CodeClockUntrusted, "clock is not trusted in this test")
}

// fakeApproval keeps the pre-review lifecycle integration tests focused on
// filesystem and state-machine behavior. It is explicit test authority, not
// evidence that either production model reviewed a candidate.
type fakeApproval struct {
	itemID       string
	action       string
	verifiedHash string
	reject       error
}

func (a *fakeApproval) Verify(hash string) error {
	if a.reject != nil {
		return a.reject
	}
	if hash == "" {
		return errs.New(errs.CodeReviewRequired, "fake approval received an empty content hash")
	}
	a.verifiedHash = hash
	return nil
}

func (a *fakeApproval) CheckIntent(itemID, action string, _ int64) error {
	if a.reject != nil {
		return a.reject
	}
	if itemID != a.itemID || action != a.action {
		return errs.New(errs.CodeReviewRequired, "fake approval does not bind this item and action")
	}
	if a.verifiedHash == "" {
		return errs.New(errs.CodeReviewRequired, "fake approval was exercised without verifying content first")
	}
	return nil
}

// Disposition reports the action this fixture authorises.
func (a *fakeApproval) Disposition() string { return a.action }

func (a *fakeApproval) AuditJSON() string {
	return `{"source":"integration-lifecycle-fixture"}`
}

func fakeLifecycleReview(_ context.Context, action string, _ store.Scope, it store.Item) (review.Approval, error) {
	return &fakeApproval{itemID: string(it.ID), action: action}, nil
}

// legacyReview adapts the per-item fixtures these integration tests were
// written against to the executor's request-based entry point.
//
// The fixtures only ever look at the action, the scope id and the item, all of
// which the measured request carries, so the adapter reconstructs exactly those
// three. The disposition is derived from the action the fixture authorised -
// "purge" is the irreversible branch - because that is what the executor
// branches on.
func legacyReview(fn func(context.Context, string, store.Scope, store.Item) (review.Approval, error)) execute.ReviewFunc {
	return func(ctx context.Context, r review.Request) (review.Outcome, error) {
		ap, err := fn(ctx, r.Action, store.Scope{ID: domain.ScopeID(r.ScopeID)},
			store.Item{ID: domain.ItemID(r.ItemID), RawRelative: []byte(r.RelativePath)})
		if err != nil {
			return review.Outcome{}, err
		}
		if ap == nil {
			return review.Outcome{Disposition: domain.DispositionKeep, Reason: "fixture kept the item"}, nil
		}
		disp := domain.DispositionQuarantine
		if r.Action == domain.ActionPurge || r.Action == domain.DispositionDelete {
			disp = domain.DispositionDelete
		}
		return review.Outcome{Approval: ap, Disposition: disp, Reason: "integration fixture"}, nil
	}
}

// factGatedReview answers "allow" to the question it was asked, but only when the
// measured facts allow an action at all.
//
// The production review client does exactly this: it asks the models, then puts
// their verdict and the executor's measurements through outcome.Decide. A fixture
// that returns a disposition directly bypasses that mapping, which is fine for
// tests about the filesystem and wrong for a test about what an incomplete fact
// does to the decision - such a test would pass whatever the executor measured.
func factGatedReview(verdict string) execute.ReviewFunc {
	return func(_ context.Context, r review.Request) (review.Outcome, error) {
		disposition, reason := outcome.Decide(outcome.Inputs{
			Action:        r.Action,
			Verdict:       verdict,
			FactsComplete: r.FactsComplete,
			Movable:       r.Movable,
			Authorized:    r.Authorized,
		})
		rejected := disposition == domain.DispositionKeep
		return review.Outcome{
			Disposition: disposition,
			Reason:      reason,
			Approval: &fakeApproval{
				itemID: r.ItemID, action: r.Action, reject: rejectIf(rejected),
			},
			Decision: review.Decision{
				Verdict:  verdict,
				Reason:   reason,
				JEVModel: "jev-fixture", LLMModel: "minimax-fixture",
			},
		}, nil
	}
}

// rejectIf turns "this review refused" into the error an approval carries when it
// may not be used.
func rejectIf(rejected bool) error {
	if !rejected {
		return nil
	}
	return errs.New(errs.CodeReviewRequired, "the review refused this action")
}

// fakeIntentApproval supplies explicit review fields to crash/recovery fixtures// that intentionally begin an intent without executing a file action.
func fakeIntentApproval(itemID domain.ItemID, action string) (review.Approval, string) {
	const hash = "sha256:integration-intent-fixture"
	return &fakeApproval{itemID: string(itemID), action: action, verifiedHash: hash}, hash
}

// env is one isolated installation: state directory, database, clock, scopes.
type env struct {
	t      *testing.T
	base   string
	paths  config.Paths
	cfg    config.Config
	clock  *domain.FakeClock
	st     *store.Store
	reg    *scope.Registrar
	exec   *execute.Executor
	plan   *plan.Planner
	rec    *recovery.Reconciler
	run    *run.Runner
	sender *recordingSender
	// ctxs are the scope contexts this environment has opened, so a test can
	// release the in-tree lock without waiting for the end of the run.
	ctxs []*execute.ScopeCtx
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	home := os.Getenv("DUSTKEEPER_TEST_HOME")
	if home == "" {
		home = filepath.Join(base, "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatalf("mkdir test home: %v", err)
		}
		t.Setenv("DUSTKEEPER_TEST_HOME", home)
	}
	t.Setenv("HOME", home)
	paths := config.Paths{
		ConfigDir:    filepath.Join(base, "config"),
		StateDir:     filepath.Join(base, "state"),
		DBFile:       filepath.Join(base, "state", "state.db"),
		ConfigFile:   filepath.Join(base, "config", "config.json"),
		InstanceLock: filepath.Join(base, "state", "instance.lock"),
		InstanceJSON: filepath.Join(base, "state", "instance.json"),
		ControlLock:  filepath.Join(base, "state", "control.lock"),
		PauseRequest: filepath.Join(base, "state", "pause.request"),
		LogDir:       filepath.Join(base, "log"),
	}
	for _, dir := range []string{paths.ConfigDir, paths.StateDir, paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	cfg := config.Default()
	// The disk floor is a property of the machine, not of the behaviour under
	// test: on a full CI volume every quarantine would stop with DISK_PRESSURE
	// and no other assertion would ever be reached. Tests that are about the
	// floor set it themselves; everything else runs with it off.
	cfg.Runtime.MinFreeBytes = 0
	cfg.Runtime.MinFreePercent = 0
	clock := domain.NewFakeClock(fixedNow)

	st, err := store.Open(paths.DBFile)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetNow(func() int64 { return clock.Wall().UnixNano() })
	if err := st.MetaSet(context.Background(), "instance_id", "inst_test"); err != nil {
		t.Fatalf("meta: %v", err)
	}

	reg := &scope.Registrar{
		Store:      st,
		Paths:      paths,
		InstanceID: "inst_test",
		Now:        func() int64 { return clock.Wall().UnixNano() },
	}
	exec := &execute.Executor{
		Store:  st,
		Clock:  clock,
		Config: executeConfig(cfg),
		Gate:   scope.PauseGate{StateDir: paths.StateDir},
		// Nothing inside a test tree is held open. Saying so explicitly keeps the
		// decision independent of whether this host has lsof and of what else the
		// machine happens to be running; the probe itself is covered by its own
		// test, and a test that wants an open file injects one.
		OpenFiles: func(context.Context, string) (assess.OpenObservation, error) {
			return assess.OpenObservation{Coverage: "visible_processes_only", Paths: map[string]bool{}}, nil
		},
		Review: legacyReview(fakeLifecycleReview),
		// The fake clock never moves backwards, so the run-wide assertion is the
		// same one production supplies; a test that wants a refused clock sets
		// its own function instead of relying on "nobody checked".
		QuarantineTrust: trustOK,
	}
	e := &env{
		t:      t,
		base:   base,
		paths:  paths,
		cfg:    cfg,
		clock:  clock,
		st:     st,
		reg:    reg,
		exec:   exec,
		plan:   &plan.Planner{Store: st, Clock: clock},
		sender: &recordingSender{},
		rec: &recovery.Reconciler{
			Store: st, Clock: clock, Registrar: reg, Exec: exec,
		},
	}
	e.run = &run.Runner{
		Store: st, Clock: clock, Config: cfg, Registrar: reg, Exec: exec, Reconciler: e.rec,
		Notify: e.sender,
	}
	return e
}

func executeConfig(c config.Config) execute.ConfigView {
	return execute.ConfigView{
		BufferBytes:    c.Runtime.IOBufferBytes,
		ReadRatePerSec: 0,
		MaxFiles:       c.Runtime.MaxAutoQuarantineFiles,
		MaxBytes:       c.Runtime.MaxAutoQuarantineBytes,
		MinFreeBytes:   c.Runtime.MinFreeBytes,
		MinFreePercent: c.Runtime.MinFreePercent,
	}
}

// addScope registers a directory under the environment.
func (e *env) addScope(name, kind string) store.Scope {
	e.t.Helper()
	dir := filepath.Join(e.base, "trees", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatalf("mkdir tree: %v", err)
	}
	sc, err := e.reg.Add(context.Background(), scope.AddOptions{Path: dir, Kind: kind, Name: name})
	if err != nil {
		e.t.Fatalf("add scope %s: %v", name, err)
	}
	return sc
}

// writeFile creates a file with a chosen age relative to the fake clock.
func (e *env) writeFile(scopeDir, rel, content string, age time.Duration) {
	e.t.Helper()
	full := filepath.Join(e.base, "trees", scopeDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		e.t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		e.t.Fatalf("write %s: %v", full, err)
	}
	stamp := e.clock.Wall().Add(-age)
	if err := os.Chtimes(full, stamp, stamp); err != nil {
		e.t.Fatalf("chtimes: %v", err)
	}
}

func (e *env) scopeCtx(id domain.ScopeID) *execute.ScopeCtx {
	e.t.Helper()
	sctx, err := execute.OpenScopeCtx(context.Background(), e.reg, id)
	if err != nil {
		e.t.Fatalf("open scope %s: %v", id, err)
	}
	e.ctxs = append(e.ctxs, sctx)
	e.t.Cleanup(sctx.Close)
	return sctx
}

// closeScopeCtxs releases every scope context this environment opened, so the
// in-tree lock is genuinely free. Contexts are normally released at the end of a
// test; a lock-contention scenario needs them released earlier, because the lock
// is re-entrant within one process and a test that holds it is not "another
// task".
func (e *env) closeScopeCtxs() {
	for _, c := range e.ctxs {
		c.Close()
	}
	e.ctxs = nil
}

func (e *env) guard(sctx *execute.ScopeCtx) *policy.Guard {
	e.t.Helper()
	g, err := run.BuildGuard(context.Background(), e.st, sctx)
	if err != nil {
		e.t.Fatalf("guard: %v", err)
	}
	return g
}

// confirmPolicy writes the one-time cleanup confirmation with a chosen
// observation period.
//
// A newly isolated object is held for the longer of what the rule promised and
// what the cleanup policy promises. Tests that are about the retention mechanism
// rather than about the fourteen-day default say so here; everything else runs
// with no policy row and therefore gets the product default.
func (e *env) confirmPolicy(observationDays int, scopeIDs ...domain.ScopeID) {
	e.t.Helper()
	pol := cleanup.Default()
	pol.ObservationDays = observationDays
	for _, id := range scopeIDs {
		pol.Scopes = append(pol.Scopes, string(id))
	}
	e.savePolicy(pol)
}

// savePolicy writes a fully formed confirmation, for tests that need more than
// the observation period decided.
//
// It deliberately does not set Notify: the default is the product's one real
// channel, and a test that needs the channel to fail says so on the sender
// instead. A policy that silently turned notifications off would make the
// reminder gate untestable and untested.
func (e *env) savePolicy(pol cleanup.Policy) {
	e.t.Helper()
	if pol.ConfirmedAtNS == 0 {
		pol.ConfirmedAtNS = e.clock.Wall().UnixNano()
	}
	if err := cleanup.Save(context.Background(), e.st, pol); err != nil {
		e.t.Fatalf("save cleanup policy: %v", err)
	}
}

// recordingSender stands in for the system notification channel.
//
// A real notification is neither reliable nor observable from a test: it needs a
// GUI session, and there is no way to ask the notification centre what it showed.
// The seam is the channel, not the gate, so the gate stays the production one.
type recordingSender struct {
	mu   sync.Mutex
	sent []string
	// fail makes every delivery fail, which is the state an unattended machine
	// is in.
	fail error
}

func (s *recordingSender) Send(_ context.Context, _ string, body string) notify.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return notify.Result{Channel: notify.ChannelMacOS, Attempted: true,
			Delivered: false, Detail: s.fail.Error()}
	}
	s.sent = append(s.sent, body)
	return notify.Result{Channel: notify.ChannelMacOS, Attempted: true, Delivered: true}
}

func (s *recordingSender) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// stableEvidence reproduces the evidence the executor records before a move, so a
// test fixture that simulates a crash writes the same facts the real path writes.
func (e *env) stableEvidence(t *testing.T, sctx *execute.ScopeCtx, rel []byte) (domain.Digest, string) {
	t.Helper()
	parent, err := sctx.Root.OpenParent(rel)
	if err != nil {
		t.Fatalf("open parent: %v", err)
	}
	defer parent.Close()
	f, err := fsops.OpenRegularAt(parent, sctx.RootDev)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer f.Close()
	ev, err := execute.ReadStable(f, e.exec.VerifyOpt(f.Token.Size))
	if err != nil {
		t.Fatalf("stable evidence: %v", err)
	}
	return ev.Content, string(ev.Metadata)
}

// scan runs one complete metadata scan of a scope.
func (e *env) scan(id domain.ScopeID) {
	e.t.Helper()
	sctx := e.scopeCtx(id)
	ledger := domain.NewRunLedger(300, 1<<30, 0)
	if _, _, err := run.ScanScope(context.Background(), e.st, e.clock, 500, sctx, e.guard(sctx), ledger); err != nil {
		e.t.Fatalf("scan: %v", err)
	}
}

// rule stores one active rule revision and returns it.
func (e *env) rule(scopeID domain.ScopeID, id string, namePatterns []string, exceptDirs []string, age time.Duration, retention time.Duration) store.Rule {
	e.t.Helper()
	r := &rules.Rule{
		DSLVersion: 1,
		ScopeID:    string(scopeID),
		Action:     "quarantine",
		Match: rules.Match{
			MinMTimeAgeSeconds: int64(age.Seconds()),
			BasenamePatterns:   namePatterns,
		},
		Except:           rules.Except{RelativeDirPrefixes: exceptDirs},
		RetentionSeconds: int64(retention.Seconds()),
	}
	if err := r.Validate(map[string]bool{string(scopeID): true}); err != nil {
		e.t.Fatalf("rule invalid: %v", err)
	}
	digest, err := r.Digest()
	if err != nil {
		e.t.Fatalf("rule digest: %v", err)
	}
	canonical, err := r.CanonicalJSON()
	if err != nil {
		e.t.Fatalf("canonical: %v", err)
	}
	sc, err := e.st.GetScope(context.Background(), scopeID)
	if err != nil {
		e.t.Fatalf("get scope: %v", err)
	}
	rb, _, err := plan.NewReviewBinding(sc, digest)
	if err != nil {
		e.t.Fatalf("review binding: %v", err)
	}
	rbJSON, _ := json.Marshal(rb)
	rev, err := e.st.NextRuleRevision(context.Background(), domain.RuleID(id))
	if err != nil {
		e.t.Fatalf("revision: %v", err)
	}
	stored := store.Rule{
		ID:                domain.RuleID(id),
		Revision:          rev,
		ScopeID:           scopeID,
		DSLJSON:           string(canonical),
		Digest:            digest,
		ReviewBindingJSON: string(rbJSON),
		Status:            domain.RuleActive,
		CreatedAtNS:       e.clock.Wall().UnixNano(),
	}
	if err := e.st.InsertRule(context.Background(), stored); err != nil {
		e.t.Fatalf("insert rule: %v", err)
	}
	return stored
}

// grant stores one active capability for a rule revision.
func (e *env) grant(r store.Rule, capability string, maxFiles int, maxBytes int64) store.Grant {
	e.t.Helper()
	sc, err := e.st.GetScope(context.Background(), r.ScopeID)
	if err != nil {
		e.t.Fatalf("get scope: %v", err)
	}
	rb, _, err := plan.NewReviewBinding(sc, r.Digest)
	if err != nil {
		e.t.Fatalf("review binding: %v", err)
	}
	var retention int64 = 30 * 86400
	if parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(r.ScopeID): true}); err == nil {
		retention = parsed.RetentionSeconds
	}
	gb, digest, err := plan.NewGrantBinding(rb, r.ID, r.Revision, capability, maxFiles, maxBytes, retention)
	if err != nil {
		e.t.Fatalf("grant binding: %v", err)
	}
	gbJSON, _ := json.Marshal(gb)
	g := store.Grant{
		ID:           domain.GrantID(domain.NewID("grant")),
		ScopeID:      r.ScopeID,
		RuleID:       r.ID,
		RuleRevision: r.Revision,
		Capability:   capability,
		BindingJSON:  string(gbJSON),
		Digest:       digest,
		Status:       domain.GrantActive,
		ApprovedAtNS: e.clock.Wall().UnixNano(),
	}
	if err := e.st.InsertGrant(context.Background(), g); err != nil {
		e.t.Fatalf("insert grant: %v", err)
	}
	return g
}

// planFor creates a manual quarantine plan for one rule.
func (e *env) planFor(sc store.Scope, r store.Rule, maxFiles int) plan.RulePlanResult {
	e.t.Helper()
	res, err := e.planForMayFail(sc, r, maxFiles)
	if err != nil {
		e.t.Fatalf("create plan: %v", err)
	}
	return res
}

// planForMayFail is planFor for cases where producing no plan is the expected
// outcome, such as "every candidate is already excluded".
func (e *env) planForMayFail(sc store.Scope, r store.Rule, maxFiles int) (plan.RulePlanResult, error) {
	e.t.Helper()
	sctx := e.scopeCtx(sc.ID)
	parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
	if err != nil {
		e.t.Fatalf("parse stored rule: %v", err)
	}
	return e.plan.CreateRulePlan(context.Background(), plan.RulePlanRequest{
		Scope:    sc,
		Rule:     parsed,
		RuleRef:  domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:     domain.PlanManualRule,
		Guard:    e.guard(sctx),
		MaxFiles: maxFiles,
		MaxBytes: 1 << 30,
		Config:   e.cfg,
		NowNS:    e.clock.Wall().UnixNano(),
		// The command line supplies this walk; without it here the harness would
		// be testing a planner that decides differently from the one a user runs.
		AncestorsOf: func(dir string) ([]domain.FileIdentity, error) {
			return execute.AncestorIdentities(sctx, []byte(dir))
		},
	})
}

// apply runs one plan and returns the result.
func (e *env) apply(sc store.Scope, p store.Plan, digest domain.Digest) (execute.ApplyResult, error) {
	sctx := e.scopeCtx(sc.ID)
	e.exec.Guard = e.guard(sctx)
	budget := domain.NewBudget(300, 1<<30, 100, 1<<30, 0)
	return e.exec.ApplyQuarantinePlan(context.Background(), sctx, p.ID, digest, budget)
}

// exists reports whether a path exists in a tree.
func (e *env) exists(scopeDir, rel string) bool {
	_, err := os.Stat(filepath.Join(e.base, "trees", scopeDir, rel))
	return err == nil
}

// itemsFor returns the stored items of a plan.
func (e *env) itemsFor(p store.Plan) []store.Item {
	e.t.Helper()
	items, err := e.st.ListItemsByPlan(context.Background(), p.ID)
	if err != nil {
		e.t.Fatalf("list items: %v", err)
	}
	return items
}

// requireCode asserts the error carries a specific code.
func requireCode(t *testing.T, err error, want errs.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got no error", want)
	}
	if got := errs.CodeOf(err); got != want {
		t.Fatalf("expected code %s, got %s (%v)", want, got, err)
	}
}

// decodeIdentity reads a stored identity in a test.
func decodeIdentity(t *testing.T, s string) domain.FileIdentity {
	t.Helper()
	var id domain.FileIdentity
	if err := json.Unmarshal([]byte(s), &id); err != nil {
		t.Fatalf("identity %q: %v", s, err)
	}
	return id
}

// hashFile computes the SHA-256 of a file on disk.
func hashFile(t *testing.T, path string) domain.Digest {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return domain.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// mustParse returns the parsed form of a stored rule.
func mustParse(t *testing.T, r store.Rule) *rules.Rule {
	t.Helper()
	parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(r.ScopeID): true})
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	return parsed
}

// decodeToken reads a stored change token in a test.
func decodeToken(t *testing.T, s string) (domain.ChangeToken, error) {
	t.Helper()
	var tok domain.ChangeToken
	if err := json.Unmarshal([]byte(s), &tok); err != nil {
		return tok, err
	}
	return tok, nil
}
