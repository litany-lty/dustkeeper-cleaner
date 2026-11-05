package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/scope"
)

func TestReviewCycleScopeShowDoesNotHideUnreadableAuthority(t *testing.T) {
	for _, fault := range []struct{ table, column string }{
		{"protections", "evidence_json"}, {"rules", "dsl_json"}, {"grants", "binding_json"},
	} {
		t.Run(fault.table, func(t *testing.T) {
			newScopeWithQueue(t, nil)
			paths, err := config.ResolvePaths()
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", paths.DBFile)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("ALTER TABLE " + fault.table + " RENAME COLUMN " + fault.column + " TO broken_column"); err != nil {
				t.Fatal(err)
			}
			r := cli(t, "scope", "show", "--id", "demo", "--json")
			if r.code == 0 || r.str(t, "code") != "DB_ERROR" {
				t.Fatalf("unreadable %s was shown as an empty successful list: %s", fault.table, r.stdout)
			}
		})
	}
}

func TestReviewCycleDoctorDoesNotApproveMalformedClockHistory(t *testing.T) {
	for _, key := range []string{"last_run_ns", "last_trusted_run_ns"} {
		t.Run(key, func(t *testing.T) {
			newHome(t)
			if r := cli(t, "init", "--json"); r.code != 0 {
				t.Fatal(r.stdout)
			}
			st := openStateStore(t)
			if err := st.MetaSet(context.Background(), key, "not-a-timestamp"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			r := cli(t, "doctor", "--json")
			if clock := checkByName(t, r, "clock"); clock["ok"] != false {
				t.Fatalf("malformed clock history was reported as healthy: %+v", clock)
			}
		})
	}
}

func TestReviewCycleQuarantineListHonorsExtendedDeadline(t *testing.T) {
	due := time.Now().Add(3 * 24 * time.Hour).UnixNano()
	newScopeWithQueue(t, []queueItem{{objectName: "retained", reason: "lifecycle", state: "quarantined", notBeforeNano: &due}})
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", paths.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE items SET quarantined_at_ns=?", time.Now().Add(-30*24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	r := cli(t, "quarantine", "list", "--json")
	if r.code != 0 {
		t.Fatal(r.stdout)
	}
	rows := dataOf(t, r)["objects"].([]any)
	row := rows[0].(map[string]any)
	if row["due"] != false || row["due_at_ns"] != float64(due) {
		t.Fatalf("extended retention deadline was ignored: %+v", row)
	}
}

func TestReviewCycleQuarantineListLimitIsGlobal(t *testing.T) {
	newScopeWithQueue(t, []queueItem{
		{objectName: "ready", reason: "lifecycle", state: "quarantined", notBeforeNano: dueAt(t, time.Hour)},
		{objectName: "held", reason: "lifecycle", state: "held", notBeforeNano: dueAt(t, time.Hour)},
	})
	r := cli(t, "quarantine", "list", "--limit", "1", "--json")
	if r.code != 0 {
		t.Fatal(r.stdout)
	}
	if rows := dataOf(t, r)["objects"].([]any); len(rows) != 1 {
		t.Fatalf("limit 1 returned %d objects", len(rows))
	}
}

func TestReviewCycleRecoveryAdviceQuotesRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions require a non-root test process")
	}
	base := newHome(t)
	root := filepath.Join(base, "scope with space")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	_, objects := buildVault(t, root)
	if err := os.Chmod(objects, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(objects, 0700) })
	r := cli(t, "recover", "inspect", "--root", root, "--json")
	commands, ok := r.env["next_steps"].([]any)
	if !ok || len(commands) == 0 {
		t.Fatalf("missing repair instruction: %s", r.stdout)
	}
	if commands[0] != "dustkeeper recover inspect --root "+shellQuote(root) {
		t.Fatalf("repair command splits the path: %v", commands)
	}
}

func TestReviewCycleProtectionRejectsUnusableRelativePaths(t *testing.T) {
	for _, path := range []string{"../secret", "/absolute", ".", "a//b"} {
		t.Run(path, func(t *testing.T) {
			newScopeWithQueue(t, nil)
			r := cli(t, "protect", "add", "--scope", "demo", "--relative", path, "--json")
			if r.code == 0 || r.str(t, "code") != "INVALID_ARGUMENT" {
				t.Fatalf("unusable protection accepted: %s", r.stdout)
			}
		})
	}
}

func TestReviewCycleProtectRemoveRejectsMixedSelectors(t *testing.T) {
	newScopeWithQueue(t, nil)
	first := cli(t, "protect", "add", "--scope", "demo", "--relative", "one", "--json")
	second := cli(t, "protect", "add", "--scope", "demo", "--relative", "two", "--json")
	if first.code != 0 || second.code != 0 {
		t.Fatal("protection fixture failed")
	}
	id := dataOf(t, first)["protection_id"].(string)
	r := cli(t, "protect", "remove", "--id", id, "--scope", "demo", "--relative", "two", "--json")
	remaining := cli(t, "protect", "list", "--scope", "demo", "--json")
	if r.code == 0 || r.str(t, "code") != "INVALID_ARGUMENT" || dataOf(t, remaining)["count"] != float64(2) {
		t.Fatalf("ambiguous selectors removed a protection: result=%s remaining=%s", r.stdout, remaining.stdout)
	}
}

func TestReviewCycleNegativePlanLimitsAreNotDefaults(t *testing.T) {
	for _, flag := range []string{"--max-files", "--max-bytes"} {
		t.Run(flag, func(t *testing.T) {
			clk := domain.NewFakeClock(time.Now())
			scopeWithRule(t, clk)
			r := cliOn(t, clk, "plan", "create", "--rule", "r1", flag, "-1", "--json")
			if r.code == 0 || r.str(t, "code") != "INVALID_ARGUMENT" {
				t.Fatalf("negative limit used configured defaults: %s", r.stdout)
			}
		})
	}
	t.Run("purge limit", func(t *testing.T) {
		newScopeWithQueue(t, nil)
		r := cli(t, "purge", "plan", "--scope", "demo", "--limit", "-1", "--json")
		if r.str(t, "code") != "INVALID_ARGUMENT" {
			t.Fatalf("negative purge limit accepted: %s", r.stdout)
		}
	})
}

func TestReviewCycleHumanPathsDoNotEmitTerminalControls(t *testing.T) {
	newScopeWithQueue(t, []queueItem{{objectName: "plain", reason: "lifecycle", state: "quarantined", notBeforeNano: dueAt(t, time.Hour)}})
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", paths.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE items SET raw_relative=?", []byte("evil\nnext\x1b[2J.zip")); err != nil {
		t.Fatal(err)
	}
	r := cli(t, "quarantine", "list")
	if r.code != 0 {
		t.Fatal("list failed")
	}
	if strings.ContainsRune(r.stdout, '\x1b') || strings.Contains(r.stdout, "evil\nnext") {
		t.Fatal("untrusted filename emitted terminal controls or forged a new output line")
	}
}

func TestReviewCycleClockAcknowledgementHonorsInstanceLock(t *testing.T) {
	newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatal(r.stdout)
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := scope.InstanceLock(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	r := cli(t, "doctor", "--acknowledge-clock", "--json")
	st := openStateStore(t)
	defer st.Close()
	ack, err := st.MetaGet(context.Background(), "clock_ack_ns")
	if err != nil {
		t.Fatal(err)
	}
	if r.code == 0 || r.str(t, "code") != "INSTANCE_BUSY" || ack != "" {
		t.Fatalf("clock acknowledgement ran during another instance's operation: result=%s ack=%s", r.stdout, ack)
	}
}

func TestReviewCycleRuleAuthorityChangesHonorInstanceLock(t *testing.T) {
	for _, command := range []string{"activate", "authorize", "revoke", "disable"} {
		t.Run(command, func(t *testing.T) {
			clk := domain.NewFakeClock(time.Now())
			scopeWithRule(t, clk)
			args := []string{"rule", command, "--id", "r1", "--revision", "1"}
			switch command {
			case "activate":
				proposal := mustCLIOn(t, clk, "rule", "preview", "--id", "r1", "--revision", "1", "--json")
				args = append(args, "--digest", dataString(t, proposal, "review_digest"))
			case "authorize":
				proposal := mustCLIOn(t, clk, "rule", "authorize", "--id", "r1", "--revision", "1", "--auto-quarantine", "--json")
				args = append(args, "--auto-quarantine", "--digest", dataString(t, proposal, "approval_digest"))
			case "revoke":
				args = append(args, "--auto-quarantine")
			}
			paths, err := config.ResolvePaths()
			if err != nil {
				t.Fatal(err)
			}
			lock, err := scope.InstanceLock(paths.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			r := cliOn(t, clk, append(args, "--json")...)
			if r.code == 0 || r.str(t, "code") != "INSTANCE_BUSY" {
				t.Fatalf("%s changed authority while another run held the execution lock: %s", command, r.stdout)
			}
		})
	}
}
