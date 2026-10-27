package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"

	"dustkeeper/migrations"
)

// These two groups are A01 and A02 of the frozen acceptance list.
//
// A01 is about the database on disk: this build reads the schema it writes, a
// current file is opened (including the crash rollback SQLite performs by
// itself), and a file written by another version is refused before a byte of it
// changes.
//
// A02 is about the objects the removed pipeline left behind. The command is gone
// from the CLI, but the rows are not: they are still loadable through the generic
// `plan apply` route and can still be handed to the executor directly. The tests
// below therefore drive both entry points, and they use a fixture that carries
// the historical columns (reason, source_kind, keeper evidence, pin) rather than a
// lifecycle object with a renamed reason.

// TestSlimLegacyCompatibility is A01.
func TestSlimLegacyCompatibility(t *testing.T) {
	t.Run("a hot journal is recovered before the version is trusted", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "state.db")

		// A database written by this build: the test refuses to test recovery on
		// a file whose version nobody has seen.
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		raw, err := sql.Open("sqlite3", dbPath)
		if err != nil {
			t.Fatalf("raw open: %v", err)
		}
		defer raw.Close()
		raw.SetMaxOpenConns(1)
		if _, err := raw.Exec("PRAGMA journal_mode=DELETE"); err != nil {
			t.Fatalf("journal mode: %v", err)
		}
		if _, err := raw.Exec("CREATE TABLE hot(x INTEGER NOT NULL)"); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := raw.Exec("INSERT INTO hot VALUES(1)"); err != nil {
			t.Fatalf("insert: %v", err)
		}

		// An uncommitted transaction, copied out while it is still open. The copy
		// is what a killed process leaves behind: a database file plus a journal
		// that carries pages the file does not have yet.
		// A transaction big enough to spill: SQLite writes modified pages to the
		// database file before the commit, keeping the originals in the journal.
		// That is the state a killed process leaves - a file that already looks
		// changed plus a journal that must be replayed to undo it.
		if _, err := raw.Exec("PRAGMA cache_size=2"); err != nil {
			t.Fatalf("cache size: %v", err)
		}
		tx, err := raw.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		payload := string(make([]byte, 200))
		for i := 0; i < 2000; i++ {
			if _, err := tx.Exec("INSERT INTO hot VALUES(?)", fmt.Sprintf("%04d%s", i, payload)); err != nil {
				t.Fatalf("insert in transaction: %v", err)
			}
		}
		journal := dbPath + "-journal"
		if fi, err := os.Stat(journal); err != nil {
			t.Fatalf("the write transaction left no journal to copy: %v", err)
		} else if fi.Size() < 512 {
			t.Fatalf("the journal is %d bytes, too small to be a hot journal", fi.Size())
		}

		destDir := t.TempDir()
		dest := filepath.Join(destDir, "state.db")
		copyFileBytes(t, dbPath, dest)
		copyFileBytes(t, journal, dest+"-journal")
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		raw.Close()

		// Opening must let SQLite finish the rollback, then read the real version.
		// A header-only decision made before that would be reading a file that is
		// about to change.
		got, err := store.Open(dest)
		if err != nil {
			t.Fatalf("a current-format database with a hot journal was refused: %v", err)
		}
		if err := got.Close(); err != nil {
			t.Fatalf("close recovered: %v", err)
		}
		// Playback removes the journal. Keeping it would mean SQLite read the
		// header, found nothing to undo, and never ran the recovery half.
		if _, err := os.Stat(dest + "-journal"); !os.IsNotExist(err) {
			t.Fatalf("the journal survived the open, so no playback happened: %v", err)
		}

		check, err := sql.Open("sqlite3", dest)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer check.Close()
		var n int
		if err := check.QueryRow("SELECT COUNT(*) FROM hot").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("rows after recovery = %d, want the 1 committed row; the uncommitted ones survived", n)
		}
		var version int
		if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("version: %v", err)
		}
		if version != migrations.CurrentVersion {
			t.Fatalf("user_version after recovery = %d, want %d", version, migrations.CurrentVersion)
		}
	})

	t.Run("a vault from an older install is left byte for byte", func(t *testing.T) {
		e := newEnv(t)
		dir := filepath.Join(e.base, "trees", "legacy")
		vault := filepath.Join(dir, domain.VaultDirName)
		for _, d := range []string{filepath.Join(vault, "objects"), filepath.Join(vault, "manifests")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		// Written by hand: the point is a vault this build did not create, with a
		// marker that belongs to another installation.
		marker := `{"format_version":1,"instance_id":"inst_old","scope_id":"scp_old"}`
		if err := os.WriteFile(filepath.Join(vault, "owner.json"), []byte(marker), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if err := os.WriteFile(filepath.Join(vault, "objects", "obj_old"), []byte("kept"), 0o600); err != nil {
			t.Fatalf("write object: %v", err)
		}
		before := snapshotTree(t, vault)

		_, err := e.reg.Add(context.Background(),
			scope.AddOptions{Path: dir, Kind: domain.KindManagedDisposable, Name: "legacy"})
		if err == nil {
			t.Fatal("a directory with a foreign vault was registered")
		}
		after := snapshotTree(t, vault)
		for name, sum := range before {
			if after[name] != sum {
				t.Fatalf("%s was rewritten by a refused registration", name)
			}
		}
		for name := range after {
			if _, ok := before[name]; !ok {
				t.Fatalf("a refused registration created %s", name)
			}
		}
	})

	t.Run("a schema this build does not write is refused untouched", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			version int
			code    errs.Code
		}{
			{"newer", migrations.CurrentVersion + 1, errs.CodeMigrationTooNew},
			{"older", migrations.CurrentVersion - 1, errs.CodeMigrationTooOld},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				dbPath := filepath.Join(dir, "state.db")
				st, err := store.Open(dbPath)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				if err := st.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
				raw, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("raw open: %v", err)
				}
				if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version=%d", tc.version)); err != nil {
					t.Fatalf("set version: %v", err)
				}
				raw.Close()

				before := readFileBytes(t, dbPath)
				_, err = store.Open(dbPath)
				requireCode(t, err, tc.code)
				after := readFileBytes(t, dbPath)
				if string(before) != string(after) {
					t.Fatal("an unsupported database was modified before being refused")
				}
			})
		}
	})
}

// TestSlimLegacyActionsAreBlocked is A02.
func TestSlimLegacyActionsAreBlocked(t *testing.T) {
	t.Run("a new reference scope is refused", func(t *testing.T) {
		e := newEnv(t)
		dir := filepath.Join(e.base, "trees", "orig")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := e.reg.Add(context.Background(),
			scope.AddOptions{Path: dir, Kind: domain.KindReference, Name: "orig"})
		requireCode(t, err, errs.CodeInvalidArgument)
		if _, err := os.Stat(filepath.Join(dir, domain.VaultDirName)); !os.IsNotExist(err) {
			t.Fatalf("a refused registration created a vault: %v", err)
		}
	})

	t.Run("a duplicate plan cannot be applied", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, p, it := e.legacyDuplicate(t, false)

		sctx := e.scopeCtx(sc.ID)
		e.exec.Guard = e.guard(sctx)
		_, err := e.exec.ApplyQuarantinePlan(ctx, sctx, p.ID, p.Digest,
			domain.NewBudget(300, 1<<30, 10, 1<<30, 0))
		requireCode(t, err, errs.CodePlanStale)

		if got, err := e.st.GetPlan(ctx, p.ID); err != nil {
			t.Fatalf("get plan: %v", err)
		} else if got.Status != domain.PlanReady {
			t.Fatalf("the refused plan was marked %s", got.Status)
		}
		if action := e.lastIntentAction(it.ID); action != "" {
			t.Fatalf("a refused plan wrote a %s intent", action)
		}
		if !e.exists("dups", "one.bin") {
			t.Fatal("the file moved even though the plan was refused")
		}
	})

	t.Run("the executor refuses a historical duplicate directly", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, p, it := e.legacyDuplicate(t, false)

		sctx := e.scopeCtx(sc.ID)
		e.exec.Guard = e.guard(sctx)
		res, err := e.exec.Quarantine(ctx, sctx, p, it, domain.NewBudget(300, 1<<30, 10, 1<<30, 0))
		requireCode(t, err, errs.CodePlanStale)
		if res.State != it.State {
			t.Fatalf("the item state changed to %s", res.State)
		}
		if !e.exists("dups", "one.bin") {
			t.Fatal("the file moved even though the object was refused")
		}
		if action := e.lastIntentAction(it.ID); action != "" {
			t.Fatalf("a refused object wrote a %s intent", action)
		}
	})

	t.Run("a purge plan that contains a duplicate is refused as a whole", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, _, it := e.legacyDuplicate(t, true)

		_, err := plan.CreatePurgePlan(ctx, e.st, sc, []store.Item{it},
			domain.PlanManualPurge, e.clock.Wall().UnixNano(), int64(plan.DefaultTTL))
		requireCode(t, err, errs.CodePlanStale)

		// The same refusal has to exist behind the generic route as well, because
		// a plan can still be loaded by id.
		p := e.insertPurgePlan(t, sc, it)
		sctx := e.scopeCtx(sc.ID)
		e.exec.Guard = e.guard(sctx)
		_, err = e.exec.ApplyPurgePlan(ctx, sctx, p.ID, p.Digest,
			domain.NewBudget(300, 1<<30, 10, 1<<30, 0), trustOK)
		requireCode(t, err, errs.CodePlanStale)

		if got, err := e.st.GetPlan(ctx, p.ID); err != nil {
			t.Fatalf("get plan: %v", err)
		} else if got.Status != domain.PlanReady {
			t.Fatalf("the refused purge plan was marked %s", got.Status)
		}
		if action := e.lastIntentAction(it.ID); action != "" {
			t.Fatalf("a refused purge wrote a %s intent", action)
		}
		if !e.vaultObjectExists(sc, it) {
			t.Fatal("the object was unlinked even though the purge plan was refused")
		}
	})

	t.Run("the purge queue does not offer a duplicate", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, _, it := e.legacyDuplicate(t, true)

		due, err := e.st.ItemsDueForPurge(ctx, sc.ID, e.clock.Wall().UnixNano()+int64(60*24*time.Hour), 10)
		if err != nil {
			t.Fatalf("due: %v", err)
		}
		for _, d := range due {
			if d.ID == it.ID {
				t.Fatal("a historical duplicate was offered to the automatic purge queue")
			}
		}
	})

	t.Run("a quarantined duplicate can still be restored", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, _, it := e.legacyDuplicate(t, true)

		sctx := e.scopeCtx(sc.ID)
		e.exec.Guard = e.guard(sctx)
		if _, err := e.exec.Restore(ctx, sctx, it.ID, execute.RestoreOptions{}); err != nil {
			t.Fatalf("restore of a historical duplicate: %v", err)
		}
		if !e.exists("dups", "one.bin") {
			t.Fatal("the object was not put back")
		}
		if e.vaultObjectExists(sc, it) {
			t.Fatal("the object is still in the vault")
		}
		got, err := e.st.GetItem(ctx, it.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if got.State != domain.StateRestored {
			t.Fatalf("the item is %s", got.State)
		}
		if got.KeeperPinActive {
			t.Fatal("the keeper pin survived a successful restore")
		}
		if inUse, err := e.st.KeeperInUse(ctx, sc.ID, []byte("keep.bin")); err != nil {
			t.Fatalf("keeper in use: %v", err)
		} else if inUse {
			t.Fatal("the keeper is still pinned by an object that is back in the tree")
		}
	})

	t.Run("an adopt pending is reported and left exactly as it was", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, _, it := e.legacyDuplicate(t, true)

		opID := domain.OperationID(domain.NewID("op"))
		if err := e.st.BeginIntent(ctx, store.Intent{
			OpID:         opID,
			ItemID:       it.ID,
			Action:       "adopt",
			PriorState:   domain.StateQuarantined,
			NextState:    domain.StateRIntent,
			ContractJSON: `{"binding_version":1,"purpose":"adopt"}`,
			OccurredAtNS: e.clock.Wall().UnixNano(),
		}); err != nil {
			t.Fatalf("stage the adopt intent: %v", err)
		}
		before := e.itemRow(t, it.ID)

		rep, err := e.rec.Run(ctx)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rep.Blocked != 1 || rep.Completed != 0 || rep.Held != 0 {
			t.Fatalf("an adopt pending was not reported as blocked: %+v", rep)
		}
		after := e.itemRow(t, it.ID)
		if before.State != after.State || before.Pending != after.Pending || before.HoldCode != after.HoldCode {
			t.Fatalf("the historical row was rewritten: %+v -> %+v", before, after)
		}
		if after.Pending != string(opID) {
			t.Fatalf("the pending marker was cleared: %q", after.Pending)
		}
		if got, _ := e.st.GetScope(ctx, sc.ID); got.Status != domain.ScopeHeld {
			t.Fatalf("the scope was not held: %s", got.Status)
		}
		if unresolved, err := e.st.ScopeHasUnresolved(ctx, sc.ID); err != nil {
			t.Fatalf("unresolved: %v", err)
		} else if !unresolved {
			t.Fatal("the scope still looks clean to the shared gate")
		}
	})

	t.Run("a terminal event left with a pending marker is not rewritten", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		sc, p, it := e.legacyDuplicate(t, false)

		opID := domain.OperationID(domain.NewID("op"))
		if err := e.st.BeginIntent(ctx, store.Intent{
			OpID:         opID,
			ItemID:       it.ID,
			PlanID:       &p.ID,
			Action:       "quarantine",
			PriorState:   domain.StatePlanned,
			NextState:    domain.StateQIntent,
			ContractJSON: `{"binding_version":1,"purpose":"normal"}`,
			OccurredAtNS: e.clock.Wall().UnixNano(),
		}); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := e.st.HoldIntent(ctx, store.Intent{
			OpID:         opID,
			ItemID:       it.ID,
			Action:       "quarantine",
			ContractJSON: `{"binding_version":1,"purpose":"normal"}`,
			OccurredAtNS: e.clock.Wall().UnixNano(),
		}, "UNEXPLAINED", `{"source":"reconciled"}`); err != nil {
			t.Fatalf("hold: %v", err)
		}
		// The residue an older build could leave: the verdict is on record and the
		// item still names the operation as pending.
		e.execRaw(t, "UPDATE items SET pending_op_id=?, state='unknown', hold_code=NULL WHERE item_id=?",
			string(opID), string(it.ID))

		terminals := e.terminalEvents(t, opID)
		if terminals != 1 {
			t.Fatalf("the fixture has %d terminal events, want 1", terminals)
		}
		rep, err := e.rec.Run(ctx)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rep.Blocked != 1 {
			t.Fatalf("terminal residue was not reported as blocked: %+v", rep)
		}
		if got := e.terminalEvents(t, opID); got != terminals {
			t.Fatalf("reconciliation wrote another terminal event: %d -> %d", terminals, got)
		}
		if after := e.itemRow(t, it.ID); after.Pending != string(opID) {
			t.Fatalf("the pending marker was cleared: %q", after.Pending)
		}
		if got, _ := e.st.GetScope(ctx, sc.ID); got.Status != domain.ScopeHeld {
			t.Fatalf("the scope was not held: %s", got.Status)
		}
	})
}

// legacyDuplicate builds an object shaped like the ones the removed pipeline
// wrote: an exact duplicate with its keeper evidence and an active pin. It is
// stored through the same plan document the planner uses, so the digest matches
// what the verifier recomputes from the rows.
func (e *env) legacyDuplicate(t *testing.T, quarantined bool) (store.Scope, store.Plan, store.Item) {
	t.Helper()
	ctx := context.Background()
	sc := e.addScope("dups", domain.KindManagedDisposable)
	e.writeFile("dups", "keep.bin", "payload", 40*24*time.Hour)
	e.writeFile("dups", "one.bin", "payload", 40*24*time.Hour)
	e.scan(sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"one.bin"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	real := e.planFor(sc, r, 100)
	base := real.Items[0]

	object := domain.NewID("obj")
	it := base
	it.ID = domain.ItemID(domain.NewID("obj"))
	it.ObjectName = object
	it.Reason = domain.ReasonExactDuplicate
	it.SourceKind = domain.SourceManualDuplicate
	keeper := plan.KeeperEvidenceV1{
		ScopeID:        string(sc.ID),
		RawRelativeB64: plan.B64([]byte("keep.bin")),
		Identity:       decodeIdentity(t, base.PreIdentityJSON),
		Token:          mustToken(t, base.PreTokenJSON),
		ContentHash:    string(domain.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")),
		MetadataHash:   "meta-historical",
		MetadataPolicy: plan.MetadataPolicyVersion,
	}
	keeperJSON, err := json.Marshal(keeper)
	if err != nil {
		t.Fatalf("keeper json: %v", err)
	}
	keeperStr := string(keeperJSON)
	it.KeeperJSON = &keeperStr
	it.KeeperScopeID = &sc.ID
	it.KeeperRawRelative = []byte("keep.bin")
	it.KeeperPinActive = true
	it.PlanID = ""
	it.State = domain.StatePlanned

	if quarantined {
		// The object is moved the way the product moves it, then the row is stored
		// with the evidence of the object that is actually in the vault.
		sctx := e.scopeCtx(sc.ID)
		vaultParent, err := sctx.Objects.ParentOf(object)
		if err != nil {
			t.Fatalf("vault parent: %v", err)
		}
		defer vaultParent.Close()
		src := filepath.Join(e.base, "trees", "dups", "one.bin")
		if err := os.Rename(src, filepath.Join(e.base, "trees", "dups", domain.VaultDirName, "objects", object)); err != nil {
			t.Fatalf("stage the vault object: %v", err)
		}
		identity, token := e.vaultObject(t, sctx, object)
		idJSON, _ := json.Marshal(identity)
		tokJSON, _ := json.Marshal(token)
		idStr, tokStr := string(idJSON), string(tokJSON)
		hash := string(hashFile(t, e.vaultPath("dups", object)))
		meta := "meta-historical"
		nowNS := e.clock.Wall().UnixNano()
		due := nowNS + int64(7*24*time.Hour)
		it.State = domain.StateQuarantined
		it.CapturedIdentityJSON = &idStr
		it.CapturedTokenJSON = &tokStr
		it.ContentHash = &hash
		it.MetadataHash = &meta
		it.QuarantinedAtNS = &nowNS
		it.PurgeNotBeforeNS = &due
	}

	p := store.Plan{
		ID:          domain.PlanID(domain.NewID("plan")),
		Kind:        domain.PlanManualDuplicate,
		HeaderJSON:  real.Plan.HeaderJSON,
		Status:      domain.PlanReady,
		CreatedAtNS: e.clock.Wall().UnixNano(),
		ExpiresAtNS: e.clock.Wall().Add(time.Hour).UnixNano(),
	}
	doc, err := plan.BuildDocFromStore(p, []store.Item{it})
	if err != nil {
		t.Fatalf("plan doc: %v", err)
	}
	digest, err := doc.ComputeDigest()
	if err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	p.Digest = digest
	if err := e.st.InsertPlan(ctx, p, []store.Item{it}); err != nil {
		t.Fatalf("insert the legacy plan: %v", err)
	}
	return sc, p, it
}

// insertPurgePlan stores a purge plan whose only member is the given item,
// bypassing the planner, because the point is a plan that already exists.
func (e *env) insertPurgePlan(t *testing.T, sc store.Scope, it store.Item) store.Plan {
	t.Helper()
	p := store.Plan{
		ID:          domain.PlanID(domain.NewID("plan")),
		Kind:        domain.PlanManualPurge,
		HeaderJSON:  `{"policy_digest":"","config_digest":"","bindings":[]}`,
		Digest:      domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111"),
		Status:      domain.PlanReady,
		CreatedAtNS: e.clock.Wall().UnixNano(),
		ExpiresAtNS: e.clock.Wall().Add(time.Hour).UnixNano(),
	}
	if err := e.st.InsertPlan(context.Background(), p, nil); err != nil {
		t.Fatalf("insert the purge plan: %v", err)
	}
	if err := e.st.InsertPurgeMembers(context.Background(), p.ID, []store.PurgeMember{{
		Ordinal: 0, ItemID: it.ID, VerificationJSON: "{}",
	}}); err != nil {
		t.Fatalf("insert the purge member: %v", err)
	}
	_ = sc
	return p
}

// itemRow is the part of an item row this file compares before and after.
type itemRow struct {
	State    string
	Pending  string
	HoldCode string
}

func (e *env) itemRow(t *testing.T, id domain.ItemID) itemRow {
	t.Helper()
	got, err := e.st.GetItem(context.Background(), id)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	out := itemRow{State: got.State}
	if got.PendingOpID != nil {
		out.Pending = string(*got.PendingOpID)
	}
	if got.HoldCode != nil {
		out.HoldCode = *got.HoldCode
	}
	return out
}

func (e *env) terminalEvents(t *testing.T, op domain.OperationID) int {
	t.Helper()
	rows, err := sql.Open("sqlite3", e.paths.DBFile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	var n int
	if err := rows.QueryRow("SELECT COUNT(*) FROM events WHERE op_id=? AND phase IN ('complete','abort','hold')",
		string(op)).Scan(&n); err != nil {
		t.Fatalf("count terminal events: %v", err)
	}
	return n
}

// execRaw runs one statement against the state database from outside the store.
// It is only used to build states an older build could have left behind; the
// product itself never writes with it.
func (e *env) execRaw(t *testing.T, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", e.paths.DBFile)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatalf("raw statement: %v", err)
	}
}

func copyFileBytes(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// snapshotTree maps every regular file under root to a digest of its bytes.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = string(hashFile(t, path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func mustToken(t *testing.T, s string) domain.ChangeToken {
	t.Helper()
	tok, err := decodeToken(t, s)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return tok
}
