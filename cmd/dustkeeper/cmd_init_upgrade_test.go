package main

// Only `init` may move an older state database forward. A routine read has to
// refuse the mismatch so nobody upgrades a vault by accident while merely
// checking status.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"dustkeeper/migrations"
)

func oldInstanceDB(t *testing.T, path string, version int) {
	t.Helper()
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
	for _, q := range []string{
		fmt.Sprintf("PRAGMA user_version=%d", version),
		"INSERT INTO meta(key,value) VALUES('instance_id','inst-old')",
		fmt.Sprintf("INSERT INTO meta(key,value) VALUES('schema_version','%d')", version),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInitUpgradesExplicitlyButStatusDoesNot(t *testing.T) {
	base := newHome(t)
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldInstanceDB(t, filepath.Join(stateDir, "state.db"), migrations.CurrentVersion-1)

	before := cli(t, "status", "--json")
	if before.code == 0 || !strings.Contains(before.stdout, "MIGRATION_TOO_OLD") {
		t.Fatalf("status silently accepted or migrated an old database: exit %d\n%s", before.code, before.stdout)
	}

	init := cli(t, "init", "--json")
	if init.code != 0 {
		t.Fatalf("explicit init refused the upgrade: exit %d\n%s\n%s", init.code, init.stdout, init.stderr)
	}
	upgrade, ok := dataOf(t, init)["schema_upgrade"].(map[string]any)
	if !ok || upgrade["upgraded"] != true {
		t.Fatalf("init did not report the upgrade: %v", dataOf(t, init)["schema_upgrade"])
	}
	if got := dataOf(t, init)["instance_id"]; got != "inst-old" {
		t.Errorf("instance identity changed: %v", got)
	}

	after := cli(t, "status", "--json")
	if after.code != 0 {
		t.Fatalf("status after an explicit upgrade exited %d\n%s", after.code, after.stdout)
	}
}

func TestInitHumanOutputNamesTheUpgrade(t *testing.T) {
	base := newHome(t)
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldInstanceDB(t, filepath.Join(stateDir, "state.db"), migrations.CurrentVersion-1)

	init := cli(t, "init")
	if init.code != 0 {
		t.Fatalf("init exited %d\n%s\n%s", init.code, init.stdout, init.stderr)
	}
	want := fmt.Sprintf("upgraded state schema %d -> %d explicitly",
		migrations.CurrentVersion-1, migrations.CurrentVersion)
	if !strings.Contains(init.stdout, want) {
		t.Errorf("human output does not name the upgrade %q:\n%s", want, init.stdout)
	}
}
