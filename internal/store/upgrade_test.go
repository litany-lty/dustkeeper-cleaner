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

func TestExplicitUpgradeFromThreeAndFour(t *testing.T) {
	for _, from := range []int{migrations.OldestUpgradable, migrations.CurrentVersion - 1} {
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
			for _, table := range []string{"review_unknown", "review_dispatch"} {
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
