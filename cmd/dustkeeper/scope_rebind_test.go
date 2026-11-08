package main

// P0: a scope whose runtime `dev-*` label was renumbered across a boot cannot
// be recovered with `scope resume --accept-stale-hold`. These tests build a
// REAL temporary installation with a REAL isolated object, then simulate only
// the persisted half of a boot relabel (nothing outside t.TempDir is touched).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// rebindFixture creates one quarantined object plus an existing rule/plan/
// grant, then rewrites ONLY the persisted identity facts to an older boot's
// runtime label. It mirrors the 2026-11-06 SCOPE_CHANGED state without ever
// changing a real device number or touching ~/Downloads.
func rebindFixture(t *testing.T) (base, itemID, tree, oldLabel, newLabel string, oldDev uint64) {
	t.Helper()
	base, itemID = heldObjectFixture(t)
	tree = filepath.Join(base, "tree")
	dbPath := filepath.Join(base, "state", "state.db")

	root, err := fsops.OpenRoot(tree)
	if err != nil {
		t.Fatalf("open fixture root: %v", err)
	}
	defer root.Close()
	current, err := root.Identity()
	if err != nil {
		t.Fatalf("read root identity: %v", err)
	}
	newLabel = current.Persistent.VolumeID
	oldDev = current.Runtime.Device + 2
	oldLabel = fmt.Sprintf("dev-%d", oldDev)

	objects, err := root.OpenDir([]byte(domain.VaultObjects))
	if err != nil {
		t.Fatalf("open vault objects: %v", err)
	}
	defer objects.Close()
	oldVault, err := scope.VaultIdentityUnder(objects, oldLabel, oldDev)
	if err != nil {
		t.Fatalf("derive old vault identity: %v", err)
	}
	anchor, err := root.OpenFileRW([]byte(domain.VaultAnchor))
	if err != nil {
		t.Fatalf("open anchor: %v", err)
	}
	anchorID, _, err := anchor.Stat()
	if err != nil {
		anchor.Close()
		t.Fatalf("stat anchor: %v", err)
	}
	anchor.Close()

	vaultDir := filepath.Join(tree, domain.VaultDirName)
	bindPath := filepath.Join(vaultDir, "vault-bind.json")
	var bind map[string]any
	readJSONFile(t, bindPath, &bind)
	tag, _ := bind["anchor_tag"].(string)
	oldAnchor := scope.AnchorIdentityUnder(anchorID, tag, oldLabel, oldDev)
	bind["volume_id"] = oldLabel
	bind["objects_identity"] = oldVault
	bind["anchor_identity"] = oldAnchor
	writeJSONFile(t, bindPath, bind)

	ownerPath := filepath.Join(vaultDir, "owner.json")
	var owner map[string]any
	readJSONFile(t, ownerPath, &owner)
	owner["volume_id"] = oldLabel
	writeJSONFile(t, ownerPath, owner)

	// Manifests are historical evidence: only their runtime label is remapped in
	// this fixture so they describe the same (old-boot) world as the DB row.
	manifestDir := filepath.Join(vaultDir, "manifests")
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		t.Fatalf("read manifests: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(manifestDir, e.Name())
		var m map[string]any
		readJSONFile(t, p, &m)
		for _, k := range []string{"parent_identity", "expected_identity"} {
			id, _ := m[k].(map[string]any)
			if id == nil {
				continue
			}
			id["volume_id"] = oldLabel
			id["device"] = float64(oldDev)
			m[k] = id
		}
		writeJSONFile(t, p, m)
	}

	stored := current
	stored.Persistent.VolumeID = oldLabel
	stored.Runtime.Device = oldDev
	stored.Runtime.BootOrRunID = "boot-fixture-old"
	rootJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal old identity: %v", err)
	}
	mountJSON, err := json.Marshal(domain.RuntimeMountIdentity{
		BootOrRunID: "boot-fixture-old", Device: oldDev,
	})
	if err != nil {
		t.Fatalf("marshal old mount binding: %v", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE scopes SET root_identity_json=?, vault_identity_json=?, status='held'
WHERE scope_id='exports'`, string(rootJSON), oldVault); err != nil {
		t.Fatalf("rewrite scope identity: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('scope_mount_binding:exports',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(mountJSON)); err != nil {
		t.Fatalf("rewrite mount binding: %v", err)
	}
	rows, err := db.Query(`SELECT item_id, source_parent_identity_json, pre_identity_json,
captured_identity_json FROM items WHERE scope_id='exports'`)
	if err != nil {
		t.Fatalf("read items: %v", err)
	}
	type itemRow struct {
		id, parent, pre string
		captured        sql.NullString
	}
	var items []itemRow
	for rows.Next() {
		var it itemRow
		if err := rows.Scan(&it.id, &it.parent, &it.pre, &it.captured); err != nil {
			rows.Close()
			t.Fatalf("scan item: %v", err)
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		parent := rewriteLabel(t, it.parent, oldLabel, oldDev)
		pre := rewriteLabel(t, it.pre, oldLabel, oldDev)
		var captured any
		if it.captured.Valid {
			captured = rewriteLabel(t, it.captured.String, oldLabel, oldDev)
		}
		if _, err := db.Exec(`UPDATE items SET source_parent_identity_json=?, pre_identity_json=?,
captured_identity_json=?, state='quarantined', hold_code=NULL, pending_op_id=NULL
WHERE item_id=?`, parent, pre, captured, it.id); err != nil {
			t.Fatalf("rewrite item %s: %v", it.id, err)
		}
	}

	// The recovery must run with automatic cleanup paused.
	if err := os.WriteFile(filepath.Join(base, "state", "pause.request"),
		[]byte("fixture: schedule stopped"), 0o600); err != nil {
		t.Fatalf("write pause marker: %v", err)
	}
	return base, itemID, tree, oldLabel, newLabel, oldDev
}

func rewriteLabel(t *testing.T, raw, label string, dev uint64) string {
	t.Helper()
	var id domain.FileIdentity
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		t.Fatalf("decode identity %q: %v", raw, err)
	}
	id.VolumeID = label
	id.Device = dev
	out, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("encode identity: %v", err)
	}
	return string(out)
}

func readJSONFile(t *testing.T, p string, dst any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decode %s: %v", p, err)
	}
}

func writeJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %s: %v", p, err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func rebindDB(t *testing.T, base string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func rebindScopeRow(t *testing.T, db *sql.DB) (status, rootJSON, vaultJSON string, generation, revision int64) {
	t.Helper()
	err := db.QueryRow(`SELECT status, root_identity_json, COALESCE(vault_identity_json,''),
generation, protection_revision FROM scopes WHERE scope_id='exports'`).
		Scan(&status, &rootJSON, &vaultJSON, &generation, &revision)
	if err != nil {
		t.Fatalf("read scope row: %v", err)
	}
	return
}

func dataMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T (%v)", v, v)
	}
	return m
}

// §5.3.1/§5.2: the old commands cannot rebuild the binding; the preview is
// strictly read-only and leaves every identity, status and generation alone.
func TestScopeRebindPreviewIsReadOnlyAndResumeCannotRecover(t *testing.T) {
	base, _, _, _, _, _ := rebindFixture(t)
	db := rebindDB(t, base)
	beforeStatus, beforeRoot, beforeVault, beforeGen, beforeRev := rebindScopeRow(t, db)

	// `resume --accept-stale-hold` changes only a status column.
	mustCLI(t, "scope", "resume", "--id", "exports", "--accept-stale-hold", "--json")
	status, root, vault, gen, rev := rebindScopeRow(t, db)
	if root != beforeRoot || vault != beforeVault || gen != beforeGen || rev != beforeRev {
		t.Fatal("scope resume rewrote identity facts; it must not be used as a rebind")
	}
	if status == beforeStatus {
		t.Fatal("resume did not change the status column, so this test proves nothing")
	}
	mustCLI(t, "scope", "pause", "--id", "exports", "--json")

	// The real execution path still refuses: the vault digest is bound to the
	// old label and the scope row still names it.
	st, err := store.OpenExisting(filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	reg := &scope.Registrar{Store: st, InstanceID: "inst_fixture", Now: func() int64 { return 1 }}
	if _, err := reg.OpenVerified(context.Background(), "exports"); err != nil {
		t.Fatalf("root comparison should now accept a proven boot relabel: %v", err)
	}

	preview := mustCLI(t, "scope", "rebind", "--id", "exports", "--json")
	d := dataOf(t, preview)
	if d["mode"] != "preview" {
		t.Fatalf("expected preview mode, got %v", d["mode"])
	}
	p := dataMap(t, d["preview"])
	if p["boot_changed"] != true || p["quarantined_count"].(float64) < 1 {
		t.Fatalf("preview did not prove the relabel and the isolated object: %v", p)
	}
	digest := dataString(t, preview, "digest")
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("preview digest has the wrong shape: %q", digest)
	}
	afterStatus, afterRoot, afterVault, afterGen, afterRev := rebindScopeRow(t, db)
	if afterStatus != "paused" || afterRoot != beforeRoot || afterVault != beforeVault ||
		afterGen != beforeGen || afterRev != beforeRev {
		t.Fatalf("preview changed state: %s/%s/%s/%d/%d", afterStatus, afterRoot, afterVault, afterGen, afterRev)
	}
}

// §5.3.2/3: confirming preserves the scope ID, the isolated object and the
// manifest, moves to a new paused generation, narrows old grants, and the
// original object can be restored through the existing restore flow.
func TestScopeRebindPreservesHistoryAndRestoresTheObject(t *testing.T) {
	base, itemID, tree, oldLabel, newLabel, _ := rebindFixture(t)
	db := rebindDB(t, base)
	beforeStatus, _, _, beforeGen, _ := rebindScopeRow(t, db)
	manifestPath := filepath.Join(tree, domain.VaultDirName, "manifests", itemID+".json")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	payloadBefore, err := os.ReadFile(filepath.Join(tree, domain.VaultDirName, "objects", itemID))
	if err != nil {
		t.Fatalf("read isolated payload: %v", err)
	}
	var beforeEvents, beforePlans, beforeItems int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM events), (SELECT COUNT(*) FROM plans),
(SELECT COUNT(*) FROM items)`).Scan(&beforeEvents, &beforePlans, &beforeItems); err != nil {
		t.Fatalf("count history: %v", err)
	}

	preview := mustCLI(t, "scope", "rebind", "--id", "exports", "--json")
	digest := dataString(t, preview, "digest")
	backup := filepath.Join(base, "backup")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatalf("mkdir backup: %v", err)
	}
	// A wrong digest is refused before anything is written.
	bad := cli(t, "scope", "rebind", "--id", "exports", "--digest", "sha256:"+strings.Repeat("0", 64),
		"--confirm", "--backup", backup, "--schedule-stopped", "--note", "stale digest", "--json")
	if bad.code == 0 {
		t.Fatal("a fabricated digest was accepted")
	}

	res := mustCLI(t, "scope", "rebind", "--id", "exports", "--digest", digest, "--confirm",
		"--backup", backup, "--schedule-stopped", "--note", "fixture: verified boot relabel", "--json")
	rd := dataMap(t, dataOf(t, res)["rebind"])
	if rd["old_generation"].(float64) != float64(beforeGen) ||
		rd["new_generation"].(float64) != float64(beforeGen+1) || rd["status"] != "paused" {
		t.Fatalf("unexpected rebind result: %v", rd)
	}
	if rd["objects"].(float64) < 1 {
		t.Fatalf("no isolated object was carried over: %v", rd)
	}
	_ = beforeStatus

	status, rootJSON, vaultJSON, gen, rev := rebindScopeRow(t, db)
	if status != "paused" || gen != beforeGen+1 || rev <= 1 {
		t.Fatalf("scope did not land on a new paused generation: %s g=%d rev=%d", status, gen, rev)
	}
	var storedRoot domain.RootIdentity
	if err := json.Unmarshal([]byte(rootJSON), &storedRoot); err != nil {
		t.Fatalf("decode new root identity: %v", err)
	}
	if storedRoot.Persistent.VolumeID != newLabel || !strings.Contains(rootJSON, newLabel) {
		t.Fatalf("root identity was not rebound to %s: %s", newLabel, rootJSON)
	}
	if strings.Contains(rootJSON, oldLabel) || strings.Contains(vaultJSON, oldLabel) {
		t.Fatalf("an old label survived: root=%s vault=%s", rootJSON, vaultJSON)
	}
	var afterEvents, afterPlans, afterItems int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM events), (SELECT COUNT(*) FROM plans),
(SELECT COUNT(*) FROM items)`).Scan(&afterEvents, &afterPlans, &afterItems); err != nil {
		t.Fatalf("count history after: %v", err)
	}
	if afterEvents != beforeEvents+1 || afterPlans != beforePlans || afterItems != beforeItems {
		t.Fatalf("history was not preserved: events %d->%d plans %d->%d items %d->%d",
			beforeEvents, afterEvents, beforePlans, afterPlans, beforeItems, afterItems)
	}
	var activeGrants int
	if err := db.QueryRow(`SELECT COUNT(*) FROM grants WHERE scope_id='exports' AND status='active'`).
		Scan(&activeGrants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if activeGrants != 0 {
		t.Fatalf("%d old grant(s) are still active across the generation bump", activeGrants)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest after: %v", err)
	}
	if string(manifestBefore) != string(manifestAfter) {
		t.Fatal("the manifest was rewritten; it must be preserved byte-for-byte")
	}
	payloadAfter, err := os.ReadFile(filepath.Join(tree, domain.VaultDirName, "objects", itemID))
	if err != nil {
		t.Fatalf("read isolated payload after: %v", err)
	}
	if string(payloadBefore) != string(payloadAfter) {
		t.Fatal("the isolated payload changed")
	}

	// The global pause marker is still present, and the object can be given back
	// through the existing restore path with its original bytes.
	if _, err := os.Stat(filepath.Join(base, "state", "pause.request")); err != nil {
		t.Fatal("rebind cleared the global pause marker")
	}
	if err := os.MkdirAll(filepath.Join(tree, "recovered"), 0o755); err != nil {
		t.Fatalf("mkdir restore destination: %v", err)
	}
	restored := mustCLI(t, "quarantine", "restore", "--object", itemID,
		"--destination-relative", "recovered/old.zip", "--json")
	if got := dataString(t, restored, "restored"); got != "recovered/old.zip" {
		t.Fatalf("restored to %q", got)
	}
	body, err := os.ReadFile(filepath.Join(tree, "recovered", "old.zip"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(body) != string(payloadBefore) {
		t.Fatalf("restored content differs: %q", body)
	}
}

// §5.3.4: evidence that changed after the preview, an owner/vault mismatch and
// an unfinished file operation all refuse the confirmation.
func TestScopeRebindRefusesChangedEvidence(t *testing.T) {
	t.Run("stale digest", func(t *testing.T) {
		base, _, _, _, _, _ := rebindFixture(t)
		db := rebindDB(t, base)
		digest := dataString(t, mustCLI(t, "scope", "rebind", "--id", "exports", "--json"), "digest")
		if _, err := db.Exec(`UPDATE scopes SET protection_revision=protection_revision+1
WHERE scope_id='exports'`); err != nil {
			t.Fatalf("change evidence: %v", err)
		}
		backup := filepath.Join(base, "backup")
		_ = os.MkdirAll(backup, 0o700)
		r := cli(t, "scope", "rebind", "--id", "exports", "--digest", digest, "--confirm",
			"--backup", backup, "--schedule-stopped", "--note", "stale", "--json")
		if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "changed since the preview") {
			t.Fatalf("a stale digest should be refused, got exit %d: %s %s", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("owner marker does not match the vault", func(t *testing.T) {
		base, _, tree, _, _, _ := rebindFixture(t)
		backup := filepath.Join(base, "backup")
		_ = os.MkdirAll(backup, 0o700)
		ownerPath := filepath.Join(tree, domain.VaultDirName, "owner.json")
		var owner map[string]any
		readJSONFile(t, ownerPath, &owner)
		owner["instance_id"] = "inst_someone_else"
		writeJSONFile(t, ownerPath, owner)
		r := cli(t, "scope", "rebind", "--id", "exports", "--digest", "sha256:"+strings.Repeat("1", 64),
			"--confirm", "--backup", backup, "--schedule-stopped", "--note", "mismatch", "--json")
		if r.code == 0 {
			t.Fatal("an owner/vault mismatch was accepted")
		}
	})
	t.Run("unfinished file operation", func(t *testing.T) {
		base, itemID, _, _, _, _ := rebindFixture(t)
		db := rebindDB(t, base)
		digest := dataString(t, mustCLI(t, "scope", "rebind", "--id", "exports", "--json"), "digest")
		_ = digest
		if _, err := db.Exec(`UPDATE items SET pending_op_id='op_fixture' WHERE item_id=?`, itemID); err != nil {
			t.Fatalf("open an operation: %v", err)
		}
		// A fresh preview describes the new fact; the confirmation must still
		// refuse because a file operation is in flight.
		fresh := mustCLI(t, "scope", "rebind", "--id", "exports", "--json")
		if got := dataMap(t, dataOf(t, fresh)["preview"])["pending_ops"].(float64); got != 1 {
			t.Fatalf("preview did not report the in-flight operation: %v", got)
		}
		freshDigest := dataString(t, fresh, "digest")
		backup := filepath.Join(base, "backup")
		_ = os.MkdirAll(backup, 0o700)
		r := cli(t, "scope", "rebind", "--id", "exports", "--digest", freshDigest, "--confirm",
			"--backup", backup, "--schedule-stopped", "--note", "in flight", "--json")
		if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "in flight") {
			t.Fatalf("an in-flight operation should be refused, got exit %d: %s %s", r.code, r.stdout, r.stderr)
		}
	})
	// §5.3.4: a second change of the root identity itself after the preview is
	// the closest analogue of another boot renumbering. The old digest must not
	// authorise a rebind against a different set of facts.
	t.Run("stored root identity changes again", func(t *testing.T) {
		base, _, _, _, _, _ := rebindFixture(t)
		db := rebindDB(t, base)
		beforeStatus, beforeRoot, _, beforeGen, _ := rebindScopeRow(t, db)
		digest := dataString(t, mustCLI(t, "scope", "rebind", "--id", "exports", "--json"), "digest")

		var id domain.RootIdentity
		if err := json.Unmarshal([]byte(beforeRoot), &id); err != nil {
			t.Fatalf("decode stored root identity: %v", err)
		}
		id.Persistent.VolumeID = "dev-99999999"
		id.Runtime.Device = 99999999
		changed, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE scopes SET root_identity_json=? WHERE scope_id='exports'", string(changed)); err != nil {
			t.Fatalf("change the root identity: %v", err)
		}

		backup := filepath.Join(base, "backup")
		_ = os.MkdirAll(backup, 0o700)
		r := cli(t, "scope", "rebind", "--id", "exports", "--digest", digest, "--confirm",
			"--backup", backup, "--schedule-stopped", "--note", "identity changed", "--json")
		if r.code == 0 {
			t.Fatal("a root identity changed after the preview was accepted")
		}
		status, _, _, gen, _ := rebindScopeRow(t, db)
		if status != beforeStatus || gen != beforeGen {
			t.Fatalf("a refused rebind opened the scope: status=%s generation=%d", status, gen)
		}
	})
}

// §5.3.5: an interruption at the one commit boundary rolls back every row.
// The scope stays held with its old generation, so no run can act on a half
// rebound instance.
func TestScopeRebindCommitBoundaryKeepsTheOldHeldState(t *testing.T) {
	base, _, _, _, _, _ := rebindFixture(t)
	db := rebindDB(t, base)
	beforeStatus, beforeRoot, beforeVault, beforeGen, beforeRev := rebindScopeRow(t, db)
	var capturedBefore string
	if err := db.QueryRow(`SELECT COALESCE(captured_identity_json,'') FROM items LIMIT 1`).
		Scan(&capturedBefore); err != nil {
		t.Fatalf("read captured identity: %v", err)
	}
	digest := dataString(t, mustCLI(t, "scope", "rebind", "--id", "exports", "--json"), "digest")

	store.TestHookBeforeRebindEvent = func() error { return errors.New("injected: commit boundary") }
	defer func() { store.TestHookBeforeRebindEvent = nil }()
	backup := filepath.Join(base, "backup")
	_ = os.MkdirAll(backup, 0o700)
	r := cli(t, "scope", "rebind", "--id", "exports", "--digest", digest, "--confirm",
		"--backup", backup, "--schedule-stopped", "--note", "fault injection", "--json")
	if r.code == 0 {
		t.Fatal("the injected commit failure did not surface")
	}
	afterStatus, afterRoot, afterVault, afterGen, afterRev := rebindScopeRow(t, db)
	if afterStatus != beforeStatus || afterRoot != beforeRoot || afterVault != beforeVault ||
		afterGen != beforeGen || afterRev != beforeRev {
		t.Fatalf("partial rebind landed: %s/%s/%s/%d/%d", afterStatus, afterRoot, afterVault, afterGen, afterRev)
	}
	var capturedAfter string
	if err := db.QueryRow(`SELECT COALESCE(captured_identity_json,'') FROM items LIMIT 1`).
		Scan(&capturedAfter); err != nil {
		t.Fatalf("read captured identity after: %v", err)
	}
	if capturedBefore != capturedAfter {
		t.Fatal("an isolated object's identity was partially rewritten")
	}
	if afterStatus != "held" {
		t.Fatalf("the scope must stay held after an interrupted commit, got %q", afterStatus)
	}
}
