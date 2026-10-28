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
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/recovery"
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

// env is one isolated installation: state directory, database, clock, scopes.
type env struct {
	t     *testing.T
	base  string
	paths config.Paths
	cfg   config.Config
	clock *domain.FakeClock
	st    *store.Store
	reg   *scope.Registrar
	exec  *execute.Executor
	plan  *plan.Planner
	rec   *recovery.Reconciler
	run   *run.Runner
	// ctxs are the scope contexts this environment has opened, so a test can
	// release the in-tree lock without waiting for the end of the run.
	ctxs []*execute.ScopeCtx
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
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
		// The fake clock never moves backwards, so the run-wide assertion is the
		// same one production supplies; a test that wants a refused clock sets
		// its own function instead of relying on "nobody checked".
		QuarantineTrust: trustOK,
	}
	e := &env{
		t:     t,
		base:  base,
		paths: paths,
		cfg:   cfg,
		clock: clock,
		st:    st,
		reg:   reg,
		exec:  exec,
		plan:  &plan.Planner{Store: st, Clock: clock},
		rec: &recovery.Reconciler{
			Store: st, Clock: clock, Registrar: reg, Exec: exec,
		},
	}
	e.run = &run.Runner{
		Store: st, Clock: clock, Config: cfg, Registrar: reg, Exec: exec, Reconciler: e.rec,
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
