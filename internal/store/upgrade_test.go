package store

// An explicit init is the only allowed upgrade path. These tests build real
// SQLite files at older schema versions (by replaying the migrations this
// product ships), then assert the upgrade adds objects without touching the
// identity or rows already in the file, and that every refusal leaves the file
// at the version it started with.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/migrations"
)

func oldStateDB(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, step := range migrations.Steps {
		if step.Version > version {
			break
		}
		if _, err := db.Exec(step.SQL); err != nil {
			t.Fatalf("replay %03d: %v", step.Version, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO meta(key,value) VALUES('instance_id','inst-from-old-build')",
		"INSERT INTO meta(key,value) VALUES('schema_version','" + strconv.Itoa(version) + "')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestExplicitUpgradeFromThreeFourAndFive(t *testing.T) {
	// Every older schema this build can explain must reach 6 explicitly: 3, 4
	// and 5. Walking the range keeps the set correct if OldestUpgradable moves.
	for from := migrations.OldestUpgradable; from < migrations.CurrentVersion; from++ {
		t.Run(fmt.Sprintf("from_%d", from), func(t *testing.T) {
			path := oldStateDB(t, from)
			got, err := UpgradeExplicit(path)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			if !got.Upgraded || got.From != from || got.To != migrations.CurrentVersion {
				t.Fatalf("result = %+v", got)
			}
			if v := readVersion(t, path); v != migrations.CurrentVersion {
				t.Fatalf("version = %d, want %d", v, migrations.CurrentVersion)
			}
			for _, table := range []string{"review_unknown", "review_dispatch", "review_dispatch_resolution"} {
				if !tableExists(t, path, table) {
					t.Errorf("table %s was not added", table)
				}
			}
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var instance, recorded string
			if err := db.QueryRow("SELECT value FROM meta WHERE key='instance_id'").Scan(&instance); err != nil {
				t.Fatal(err)
			}
			if instance != "inst-from-old-build" {
				t.Errorf("instance identity changed during upgrade: %q", instance)
			}
			if err := db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if recorded != strconv.Itoa(migrations.CurrentVersion) {
				t.Errorf("meta schema_version = %q, want %d", recorded, migrations.CurrentVersion)
			}
			// Running init again is a no-op, not a second migration.
			again, err := UpgradeExplicit(path)
			if err != nil || again.Upgraded {
				t.Fatalf("second explicit init = %+v, %v; want an idempotent no-op", again, err)
			}
		})
	}
}

// TestProveUpgradedSchemaRequiresSchemaSixTable proves the post-migration
// completeness check is schema-6 aware. proveOurDatabase cannot require this
// table: it runs before 006.sql, on a schema-3/4/5 file that does not have it.
func TestProveUpgradedSchemaRequiresSchemaSixTable(t *testing.T) {
	path := oldStateDB(t, migrations.CurrentVersion-1)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := proveUpgradedSchema(db, path); err == nil {
		t.Fatal("a file without review_dispatch_resolution was reported as a complete schema-6 upgrade")
	}
	if _, err := db.Exec(migrations.Schema006); err != nil {
		t.Fatalf("apply 006: %v", err)
	}
	if err := proveUpgradedSchema(db, path); err != nil {
		t.Fatalf("schema-6 table present but rejected: %v", err)
	}
}

func TestExplicitUpgradeRefusesUnknownOrUncleanState(t *testing.T) {
	t.Run("too old", func(t *testing.T) {
		path := oldStateDB(t, migrations.OldestUpgradable-1)
		_, err := UpgradeExplicit(path)
		if err == nil || !strings.Contains(err.Error(), "MIGRATION_TOO_OLD") {
			t.Fatalf("err = %v, want MIGRATION_TOO_OLD", err)
		}
		if v := readVersion(t, path); v != migrations.OldestUpgradable-1 {
			t.Fatalf("a refused upgrade changed the version to %d", v)
		}
	})
	t.Run("too new", func(t *testing.T) {
		path := oldStateDB(t, migrations.CurrentVersion+1)
		_, err := UpgradeExplicit(path)
		if err == nil || !strings.Contains(err.Error(), "MIGRATION_TOO_NEW") {
			t.Fatalf("err = %v, want MIGRATION_TOO_NEW", err)
		}
	})
	t.Run("non-empty journal", func(t *testing.T) {
		path := oldStateDB(t, migrations.CurrentVersion-1)
		if err := os.WriteFile(path+"-journal", []byte("hot"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := UpgradeExplicit(path)
		if err == nil || !strings.Contains(err.Error(), "sidecar") {
			t.Fatalf("err = %v, want a sidecar refusal", err)
		}
		if v := readVersion(t, path); v != migrations.CurrentVersion-1 {
			t.Fatalf("a refused upgrade changed the version to %d", v)
		}
	})
	t.Run("no instance identity", func(t *testing.T) {
		path := oldStateDB(t, migrations.CurrentVersion-1)
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("DELETE FROM meta WHERE key='instance_id'"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err = UpgradeExplicit(path)
		if err == nil || !strings.Contains(err.Error(), "instance identity") {
			t.Fatalf("err = %v, want an identity refusal", err)
		}
	})
}

// TestExplicitUpgradePreservesExistingData proves an explicit -> 6 upgrade is
// additive: the held scope, quarantined object, unresolved request and prepared
// dispatch written by an older binary survive untouched, and the upgrade does
// not invent a manual resolution or a decision event. A routine read still
// refuses the mismatch instead of migrating (see cmd_init_upgrade_test.go).
func TestExplicitUpgradePreservesExistingData(t *testing.T) {
	from := migrations.CurrentVersion - 1 // the latest schema before the 006 table
	path := oldStateDB(t, from)

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	seed := []string{
		`INSERT INTO scopes(scope_id, raw_root, root_identity_json, instance_id,
			kind, generation, status, created_at_ns)
			VALUES('scope-old', X'2F746D70', '{}', 'inst-from-old-build',
			'managed_disposable', 1, 'held', 1)`,
		`INSERT INTO plans(plan_id, kind, header_json, digest, status,
			created_at_ns, expires_at_ns)
			VALUES('plan-old','manual_rule','{}','d','complete', 1, 4000000000000000000)`,
		`INSERT INTO items(item_id, plan_id, ordinal, scope_id, raw_relative,
			object_name, reason, source_kind, binding_json,
			source_parent_identity_json, pre_identity_json, pre_token_json,
			bound_retention_seconds, state)
			VALUES('item-old','plan-old',0,'scope-old', X'612E7A6970','obj-old',
			'lifecycle','manual_rule','{}','{}','{}','{}', 604800, 'quarantined')`,
		`INSERT INTO review_unknown(item_id, action, scope_id, raw_relative_b64,
			identity_json, token_json, created_ns, attempts, failure, cost_status)
			VALUES('item-old','purge','scope-old','YS56aXA=','{}','{}',
			1, 1, 'timeout', 'unknown')`,
		`INSERT INTO review_dispatch(dispatch_id, review_id, run_id, item_id,
			scope_id, action, identity_json, token_json, budget_json,
			created_ns, state, cost_status)
			VALUES('disp-old','rev-old','run-old','item-old','scope-old','purge',
			'{}','{}','{}', 1, 'pending', 'not_sent')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			t.Fatalf("seed old data: %v\n%s", err, q)
		}
	}
	db.Close()

	got, err := UpgradeExplicit(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !got.Upgraded || got.From != from || got.To != migrations.CurrentVersion {
		t.Fatalf("result = %+v", got)
	}
	if !tableExists(t, path, "review_dispatch_resolution") {
		t.Fatal("schema 6 resolution table was not added")
	}

	check, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	countOf := func(q string) int {
		var n int
		if err := check.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	if n := countOf("SELECT count(*) FROM scopes WHERE scope_id='scope-old' AND status='held'"); n != 1 {
		t.Errorf("held scope lost during upgrade: %d", n)
	}
	if n := countOf("SELECT count(*) FROM items WHERE item_id='item-old' AND state='quarantined'"); n != 1 {
		t.Errorf("quarantined object lost during upgrade: %d", n)
	}
	if n := countOf("SELECT count(*) FROM review_unknown WHERE item_id='item-old' AND resolved_ns IS NULL"); n != 1 {
		t.Errorf("unresolved request lost during upgrade: %d", n)
	}
	var state, cost string
	if err := check.QueryRow("SELECT state, cost_status FROM review_dispatch WHERE dispatch_id='disp-old'").
		Scan(&state, &cost); err != nil {
		t.Fatalf("prepared dispatch row disappeared: %v", err)
	}
	if state != "pending" || cost != "not_sent" {
		t.Errorf("upgrade rewrote the original request facts: state=%s cost=%s", state, cost)
	}
	if n := countOf("SELECT count(*) FROM review_dispatch_resolution"); n != 0 {
		t.Errorf("upgrade invented %d manual resolution(s)", n)
	}
	if n := countOf("SELECT count(*) FROM events WHERE action='review_dispatch_resolution'"); n != 0 {
		t.Errorf("upgrade invented %d manual resolution event(s)", n)
	}
}
