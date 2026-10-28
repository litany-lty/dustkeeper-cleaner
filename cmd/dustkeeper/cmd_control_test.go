package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// These tests cover the last metre of the command line.
//
// A batch can stop for a reason that is not a Go error: the per-item result
// carries a code and a control value, the batch summarises them, and the command
// turns the summary into an exit status. Each of those three steps used to be
// able to lose the reason - the item row was appended with an empty code, or the
// caller read "err == nil" as "keep going". The assertions below are written in
// terms of what a script sees: the exit status, `code`, `data.control`, and the
// per-item `results[].code`.

// cliOn runs one command with a controlled business clock.
//
// It is identical to cli except for the clock: the flag parsing, the routing, the
// instance lock and the JSON envelope are the production ones. Nothing a user can
// type reaches this seam - there is no flag and no environment variable for the
// clock - so a test can move the date without giving the product that ability.
func cliOn(t *testing.T, clk domain.Clock, args ...string) result {
	t.Helper()
	return cliClock(t, clk, args...)
}

func mustCLIOn(t *testing.T, clk domain.Clock, args ...string) result {
	t.Helper()
	r := cliOn(t, clk, args...)
	if r.code != 0 {
		t.Fatalf("%v exited %d\nstdout:\n%s\nstderr:\n%s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

// scopeWithRule registers one managed_disposable scope, puts one old zip in it,
// scans it, and leaves rule r1 active. Everything happens at the caller's date.
func scopeWithRule(t *testing.T, clk domain.Clock) (base, tree string) {
	t.Helper()
	base = newHome(t)
	tree = filepath.Join(base, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	victim := filepath.Join(tree, "old.zip")
	if err := os.WriteFile(victim, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// Forty days old against a thirty day rule: the object is a candidate at the
	// moment the plan is made.
	old := clk.Wall().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	mustCLIOn(t, clk, "init", "--json")
	mustCLIOn(t, clk, "scope", "add", "--path", tree, "--kind", "managed_disposable",
		"--name", "exports", "--json")
	mustCLIOn(t, clk, "scan", "--scope", "exports", "--json")

	rulePath := filepath.Join(base, "rule.json")
	if err := os.WriteFile(rulePath,
		[]byte(strings.Replace(enableRuleDSL, "%s", "exports", 1)), 0o600); err != nil {
		t.Fatalf("write rule: %v", err)
	}
	mustCLIOn(t, clk, "rule", "import", "--file", rulePath, "--id", "r1", "--json")
	prev := mustCLIOn(t, clk, "rule", "preview", "--id", "r1", "--revision", "1", "--json")
	mustCLIOn(t, clk, "rule", "activate", "--id", "r1", "--revision", "1",
		"--digest", dataString(t, prev, "review_digest"), "--json")
	return base, tree
}

// quarantineOne runs the manual quarantine of the tree's only file and returns
// the vaulted object's name.
func quarantineOne(t *testing.T, clk domain.Clock) (tree, itemID, objectName string) {
	t.Helper()
	_, tree = scopeWithRule(t, clk)
	created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
	mustCLIOn(t, clk, "plan", "apply",
		"--id", dataString(t, created, "plan_id"),
		"--digest", dataString(t, created, "digest"), "--json")

	listed := mustCLIOn(t, clk, "quarantine", "list", "--json")
	objects, _ := dataOf(t, listed)["objects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("the vault holds %d object(s), want the one that was just quarantined", len(objects))
	}
	row, _ := objects[0].(map[string]any)
	itemID, _ = row["item_id"].(string)
	objectName, _ = row["object_name"].(string)
	if itemID == "" || objectName == "" {
		t.Fatalf("the list row does not identify the object: %v", row)
	}
	return tree, itemID, objectName
}

// vaultPathOf is where the payload of a quarantined object really lives.
func vaultPathOf(tree, objectName string) string {
	return filepath.Join(tree, filepath.FromSlash(domain.VaultObjects), objectName)
}

// itemResults returns the per-item rows of an apply envelope.
func itemResults(t *testing.T, r result) []map[string]any {
	t.Helper()
	raw, ok := dataOf(t, r)["results"].([]any)
	if !ok {
		t.Fatalf("the envelope carries no results list: %s", r.stdout)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		out = append(out, m)
	}
	return out
}

// countsOf returns the plan-result counters of an apply envelope. Section 5.5
// puts them at the top level, so a script does not have to know about a nested
// object.
func countsOf(t *testing.T, r result) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, k := range []string{"planned_count", "completed_count", "skipped_count", "failed_count"} {
		v, ok := r.env[k]
		if !ok {
			t.Fatalf("the envelope has no %q: %s", k, r.stdout)
		}
		out[k] = v
	}
	return out
}

// TestSlimControlPropagation is A08's reporting half: a batch
// that stopped has to say so three times, in the exit status, on the batch, and
// on the item, and the three have to agree.
func TestSlimControlPropagation(t *testing.T) {
	t.Run("a file that changed under the plan stops the quarantine batch", func(t *testing.T) {
		clk := domain.NewFakeClock(time.Now())
		_, tree := scopeWithRule(t, clk)
		created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
		planID := dataString(t, created, "plan_id")
		digest := dataString(t, created, "digest")

		// Same size, new bytes, modification time put back: the plan no longer
		// describes the object, and the batch must stop rather than move it.
		path := filepath.Join(tree, "old.zip")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if err := os.WriteFile(path, []byte("replace"), 0o644); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatalf("restore mtime: %v", err)
		}

		applied := cliOn(t, clk, "plan", "apply", "--id", planID, "--digest", digest, "--json")
		if applied.code == 0 {
			t.Fatalf("the batch stopped but the command exited 0:\n%s", applied.stdout)
		}
		if applied.code != 4 {
			t.Fatalf("exit = %d, want 4 for %s\n%s", applied.code, errs.CodePlanStale, applied.stdout)
		}
		if got := applied.str(t, "code"); got != string(errs.CodePlanStale) {
			t.Fatalf("code = %q, want %q", got, errs.CodePlanStale)
		}
		if got := applied.str(t, "status"); got != string(errs.StatusError) {
			t.Fatalf("status = %q, want %q", got, errs.StatusError)
		}
		// The control axis is what a caller switches on. A stop at the plan level
		// means "plan again", not "the scope is frozen".
		if got, _ := dataOf(t, applied)["control"].(string); got != domain.ControlStopPlan {
			t.Fatalf("control = %q, want %q", got, domain.ControlStopPlan)
		}
		if c := countsOf(t, applied); c["completed_count"] != float64(0) || c["failed_count"] != float64(1) {
			t.Fatalf("counts = %v, want one failed and nothing completed", c)
		}
		rows := itemResults(t, applied)
		if len(rows) != 1 {
			t.Fatalf("%d result row(s), want 1", len(rows))
		}
		if rows[0]["code"] != string(errs.CodePlanStale) {
			t.Fatalf("the item row does not carry the reason: %v", rows[0])
		}
		if rows[0]["control"] != domain.ControlStopPlan {
			t.Fatalf("the item row carries control %v, want %q", rows[0]["control"], domain.ControlStopPlan)
		}
		if msg, _ := rows[0]["message"].(string); msg == "" {
			t.Fatal("the item row has no message, so the operator is told which item failed but not why")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the file was moved although the batch stopped: %v", err)
		}
	})

	t.Run("a quarantined object that cannot be verified stops the purge batch", func(t *testing.T) {
		clk := domain.NewFakeClock(time.Now())
		tree, _, objectName := quarantineOne(t, clk)
		clk.Advance(8 * 24 * time.Hour)

		due := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
		if got, _ := dataOf(t, due)["objects"].(float64); got != 1 {
			t.Fatalf("the purge plan covers %v object(s), want the expired one", got)
		}
		// The payload disappears behind the database's back. The plan is still
		// valid, so this is not a planning problem - the object itself can no
		// longer be proven to be the one that was isolated.
		if err := os.Remove(vaultPathOf(tree, objectName)); err != nil {
			t.Fatalf("remove the vault object: %v", err)
		}

		applied := cliOn(t, clk, "purge", "apply",
			"--id", dataString(t, due, "plan_id"),
			"--digest", dataString(t, due, "digest"), "--json")
		if applied.code != 6 {
			t.Fatalf("exit = %d, want 6 for %s\n%s", applied.code, errs.CodeHeld, applied.stdout)
		}
		if got := applied.str(t, "code"); got != string(errs.CodeHeld) {
			t.Fatalf("code = %q, want %q", got, errs.CodeHeld)
		}
		// A held object freezes the scope: no further object may be started on it,
		// so the control is stronger than "plan again".
		if got, _ := dataOf(t, applied)["control"].(string); got != domain.ControlStopScope {
			t.Fatalf("control = %q, want %q", got, domain.ControlStopScope)
		}
		if c := countsOf(t, applied); c["completed_count"] != float64(0) {
			t.Fatalf("counts = %v, want nothing completed", c)
		}
		rows := itemResults(t, applied)
		if len(rows) != 1 || rows[0]["code"] != string(errs.CodeHeld) {
			t.Fatalf("the item row does not carry the reason: %v", rows)
		}
	})
}

// TestSlimClockSeamDrivesRetention is the reason the clock is injectable.
//
// "Retention elapsed" is the one condition that can only be demonstrated by
// moving time. Asserting that the code calls Clock.Wall() would pass with a clock
// that is never advanced and with a window that never opens; driving the real
// command line at a controlled date shows that the window really does open, and
// that it stays shut before. Production cannot reach any of this: `dispatch`
// passes nil and therefore uses the system clock, which the last check in this
// file pins down.
func TestSlimClockSeamDrivesRetention(t *testing.T) {
	t.Run("the window opens only after the retention period", func(t *testing.T) {
		clk := domain.NewFakeClock(time.Now())
		tree, _, objectName := quarantineOne(t, clk)

		// Same day: nothing is due, and the command says so without failing.
		early := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
		if early.str(t, "code") != string(errs.CodeNoCandidates) {
			t.Fatalf("code = %q at the quarantine instant, want %q", early.str(t, "code"), errs.CodeNoCandidates)
		}
		if got, _ := dataOf(t, early)["due"].(float64); got != 0 {
			t.Fatalf("%v object(s) are due one day into a seven day retention", got)
		}

		// One hour short of the window: still nothing.
		clk.Advance(7*24*time.Hour - time.Hour)
		still := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
		if got, _ := dataOf(t, still)["due"].(float64); got != 0 {
			t.Fatalf("%v object(s) are due one hour before the window elapses", got)
		}

		// One hour past it: due, and the purge is allowed to run.
		clk.Advance(2 * time.Hour)
		due := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
		if got, _ := dataOf(t, due)["objects"].(float64); got != 1 {
			t.Fatalf("the purge plan covers %v object(s) after the window elapsed", got)
		}
		applied := mustCLIOn(t, clk, "purge", "apply",
			"--id", dataString(t, due, "plan_id"),
			"--digest", dataString(t, due, "digest"), "--json")
		if c := countsOf(t, applied); c["completed_count"] != float64(1) {
			t.Fatalf("counts = %v, want one permanent deletion", c)
		}
		if _, err := os.Stat(vaultPathOf(tree, objectName)); !os.IsNotExist(err) {
			t.Fatalf("the payload is still in the vault: %v", err)
		}
	})

	t.Run("a jumped clock stops the purge and an acknowledgement resumes it", func(t *testing.T) {
		clk := domain.NewFakeClock(time.Now())
		_, tree := scopeWithRule(t, clk)
		// A background run writes the trusted instant down. Without it there is no
		// baseline to compare a jump against.
		mustCLIOn(t, clk, "run", "--json")

		created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
		mustCLIOn(t, clk, "plan", "apply",
			"--id", dataString(t, created, "plan_id"),
			"--digest", dataString(t, created, "digest"), "--json")
		listed := mustCLIOn(t, clk, "quarantine", "list", "--json")
		objects, _ := dataOf(t, listed)["objects"].([]any)
		if len(objects) != 1 {
			t.Fatalf("%d object(s) in the vault", len(objects))
		}
		row, _ := objects[0].(map[string]any)
		objectName, _ := row["object_name"].(string)

		// Eight days pass without any run observing them. Whether that is "the
		// machine was off" or "the clock moved" cannot be told apart, so the
		// retention proof is not available.
		clk.Advance(8 * 24 * time.Hour)
		due := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")

		refused := cliOn(t, clk, "purge", "apply",
			"--id", dataString(t, due, "plan_id"),
			"--digest", dataString(t, due, "digest"), "--json")
		if refused.code != 6 {
			t.Fatalf("exit = %d, want 6 for %s\n%s", refused.code, errs.CodeClockUntrusted, refused.stdout)
		}
		if got := refused.str(t, "code"); got != string(errs.CodeClockUntrusted) {
			t.Fatalf("code = %q, want %q", got, errs.CodeClockUntrusted)
		}
		if got, _ := dataOf(t, refused)["control"].(string); got != domain.ControlStopRun {
			t.Fatalf("control = %q, want %q for an untrusted clock", got, domain.ControlStopRun)
		}
		if _, err := os.Stat(vaultPathOf(tree, objectName)); err != nil {
			t.Fatalf("the object was removed on an untrusted clock: %v", err)
		}

		// The operator acknowledges the clock. That is not a rubber stamp: every
		// window that could not be proven restarts from the acknowledgement.
		mustCLIOn(t, clk, "doctor", "--acknowledge-clock", "--json")
		afterAck := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
		if got, _ := dataOf(t, afterAck)["due"].(float64); got != 0 {
			t.Fatalf("%v object(s) became instantly deletable after an acknowledgement", got)
		}

		// A full window from the acknowledgement: now the deletion is provable.
		//
		// Time has to advance the way the product sees it. A run is what writes a
		// trusted instant down, and a gap of more than 48 hours between trusted
		// runs is itself an anomaly, so the scheduler's daily call is simulated
		// rather than jumped over.
		for i := 0; i < 8; i++ {
			clk.Advance(24 * time.Hour)
			mustCLIOn(t, clk, "run", "--json")
		}
		again := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
		if got, _ := dataOf(t, again)["objects"].(float64); got != 1 {
			t.Fatalf("the purge plan covers %v object(s) after the restarted window", got)
		}
		done := mustCLIOn(t, clk, "purge", "apply",
			"--id", dataString(t, again, "plan_id"),
			"--digest", dataString(t, again, "digest"), "--json")
		if c := countsOf(t, done); c["completed_count"] != float64(1) {
			t.Fatalf("counts = %v, want one permanent deletion after the acknowledgement", c)
		}
	})
}

// purgeOutcome is everything the two purge routes are compared on.
type purgeOutcome struct {
	exit      int
	code      string
	status    string
	control   string
	planned   float64
	completed float64
	failed    float64
	itemCodes []string
	states    []string
	inVault   int
	listed    int
}

// runOnePurge builds a fresh fixture, quarantines one object, lets its window
// elapse, and executes the resulting purge plan through the named route.
//
// Each call builds its own fixture from the same steps, so neither route is ever
// handed a plan the other has already consumed: a consumed plan would make the
// second route fail for a reason that has nothing to do with the route.
func runOnePurge(t *testing.T, route string) purgeOutcome {
	t.Helper()
	clk := domain.NewFakeClock(time.Now())
	_, tree := scopeWithRule(t, clk)

	created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
	mustCLIOn(t, clk, "plan", "apply",
		"--id", dataString(t, created, "plan_id"),
		"--digest", dataString(t, created, "digest"), "--json")
	clk.Advance(8 * 24 * time.Hour)
	due := mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")

	id := dataString(t, due, "plan_id")
	digest := dataString(t, due, "digest")

	var applied result
	switch route {
	case "purge apply":
		applied = cliOn(t, clk, "purge", "apply", "--id", id, "--digest", digest, "--json")
	case "plan apply":
		applied = cliOn(t, clk, "plan", "apply", "--id", id, "--digest", digest, "--json")
	default:
		t.Fatalf("unknown route %q", route)
	}

	out := purgeOutcome{
		exit:      applied.code,
		code:      applied.str(t, "code"),
		status:    applied.str(t, "status"),
		control:   stringOr(dataOf(t, applied)["control"]),
		planned:   numberOr(countsOf(t, applied)["planned_count"]),
		completed: numberOr(countsOf(t, applied)["completed_count"]),
		failed:    numberOr(countsOf(t, applied)["failed_count"]),
	}
	for _, row := range itemResults(t, applied) {
		out.itemCodes = append(out.itemCodes, stringOr(row["code"]))
		out.states = append(out.states, stringOr(row["state"]))
	}
	entries, err := os.ReadDir(filepath.Join(tree, filepath.FromSlash(domain.VaultObjects)))
	if err != nil {
		t.Fatalf("read the vault: %v", err)
	}
	for _, en := range entries {
		// The objects directory carries one nonce file of its own; anything else
		// is a payload that survived.
		if en.Name() != domain.VaultNonceName {
			out.inVault++
		}
	}
	listed := mustCLIOn(t, clk, "quarantine", "list", "--json")
	out.listed = int(numberOr(dataOf(t, listed)["count"]))
	return out
}

// TestSlimTwoPurgeRoutesAgree is A09's last clause. `plan apply` and
// `purge apply` are two spellings of one action, and they used to differ: the
// generic route loaded members by plan id, which is empty for a purge plan, and
// the dedicated route skipped checks the generic one made. For the same plan and
// the same object they must produce the same result, not merely both succeed.
func TestSlimTwoPurgeRoutesAgree(t *testing.T) {
	viaPurge := runOnePurge(t, "purge apply")
	viaPlan := runOnePurge(t, "plan apply")

	if viaPurge.exit != 0 || viaPlan.exit != 0 {
		t.Fatalf("exit statuses differ or failed: purge %d, plan %d", viaPurge.exit, viaPlan.exit)
	}
	if !reflect.DeepEqual(viaPurge, viaPlan) {
		t.Fatalf("the two routes disagree on the same plan:\n purge apply: %+v\n  plan apply: %+v",
			viaPurge, viaPlan)
	}
	// And the agreed answer is the right one, not a shared mistake.
	if viaPurge.code != string(errs.CodeOK) || viaPurge.completed != 1 || viaPurge.failed != 0 {
		t.Fatalf("both routes completed %v of %v: %+v", viaPurge.completed, viaPurge.planned, viaPurge)
	}
	if viaPurge.inVault != 0 || viaPurge.listed != 0 {
		t.Fatalf("the payload survived: %d in the vault, %d listed", viaPurge.inVault, viaPurge.listed)
	}
	if len(viaPurge.itemCodes) != 1 || viaPurge.itemCodes[0] != string(errs.CodeOK) {
		t.Fatalf("the item rows say %v", viaPurge.itemCodes)
	}
	if len(viaPurge.states) != 1 || viaPurge.states[0] != domain.StatePurged {
		t.Fatalf("the item ended in %v, want %s", viaPurge.states, domain.StatePurged)
	}
	if viaPurge.control != domain.ControlContinue {
		t.Fatalf("a completed purge reported control %q", viaPurge.control)
	}
}

// protectedSubdirFixture builds a scope whose only candidate sits in a
// subdirectory, quarantines it, and leaves a purge plan covering it. One
// protection exists elsewhere in the scope, because a purge only compares
// identities when there is something to compare.
func protectedSubdirFixture(t *testing.T, clk *domain.FakeClock) (tree, sub, objectName string, due result) {
	t.Helper()
	base := newHome(t)
	tree = filepath.Join(base, "tree")
	sub = filepath.Join(tree, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	victim := filepath.Join(sub, "old.zip")
	if err := os.WriteFile(victim, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	old := clk.Wall().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	mustCLIOn(t, clk, "init", "--json")
	mustCLIOn(t, clk, "scope", "add", "--path", tree, "--kind", "managed_disposable",
		"--name", "exports", "--json")
	mustCLIOn(t, clk, "protect", "add", "--scope", "exports", "--relative", "keep/a.zip", "--json")
	mustCLIOn(t, clk, "scan", "--scope", "exports", "--json")

	rulePath := filepath.Join(base, "rule.json")
	if err := os.WriteFile(rulePath,
		[]byte(strings.Replace(enableRuleDSL, "%s", "exports", 1)), 0o600); err != nil {
		t.Fatalf("write rule: %v", err)
	}
	mustCLIOn(t, clk, "rule", "import", "--file", rulePath, "--id", "r1", "--json")
	prev := mustCLIOn(t, clk, "rule", "preview", "--id", "r1", "--revision", "1", "--json")
	mustCLIOn(t, clk, "rule", "activate", "--id", "r1", "--revision", "1",
		"--digest", dataString(t, prev, "review_digest"), "--json")

	created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
	mustCLIOn(t, clk, "plan", "apply",
		"--id", dataString(t, created, "plan_id"),
		"--digest", dataString(t, created, "digest"), "--json")
	listed := mustCLIOn(t, clk, "quarantine", "list", "--json")
	objects, _ := dataOf(t, listed)["objects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("%d object(s) in the vault", len(objects))
	}
	row, _ := objects[0].(map[string]any)
	objectName, _ = row["object_name"].(string)

	clk.Advance(8 * 24 * time.Hour)
	due = mustCLIOn(t, clk, "purge", "plan", "--scope", "exports", "--json")
	return tree, sub, objectName, due
}

// TestSlimUninspectableProtectionRefusesThePurge is the other half of A08's
// protection clause: "no protection covers this slot" and "the protection could
// not be compared" are different answers, and only the first one permits a
// permanent deletion.
//
// A permission bit cannot express this portably - the suite also runs as root in
// a container, where every directory is readable - so the directory above the
// slot is replaced by something that is not a directory at all. That is the same
// fact from the product's side: the ancestry cannot be inspected, and it is not
// ENOENT, which is the one error the walk deliberately steps over.
func TestSlimUninspectableProtectionRefusesThePurge(t *testing.T) {
	t.Run("an ancestry that cannot be inspected refuses the deletion", func(t *testing.T) {
		clk := domain.NewFakeClock(time.Now())
		tree, sub, objectName, due := protectedSubdirFixture(t, clk)

		if err := os.Remove(sub); err != nil {
			t.Fatalf("remove the original directory: %v", err)
		}
		if err := os.WriteFile(sub, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("replace it with a file: %v", err)
		}

		refused := cliOn(t, clk, "purge", "apply",
			"--id", dataString(t, due, "plan_id"),
			"--digest", dataString(t, due, "digest"), "--json")
		if refused.code != 6 {
			t.Fatalf("exit = %d, want 6 for %s\n%s", refused.code,
				errs.CodeProtectionAliasUnresolved, refused.stdout)
		}
		if got := refused.str(t, "code"); got != string(errs.CodeProtectionAliasUnresolved) {
			t.Fatalf("code = %q, want %q", got, errs.CodeProtectionAliasUnresolved)
		}
		if got, _ := dataOf(t, refused)["control"].(string); got != domain.ControlStopScope {
			t.Fatalf("control = %q, want %q", got, domain.ControlStopScope)
		}
		if _, err := os.Stat(vaultPathOf(tree, objectName)); err != nil {
			t.Fatalf("the object was removed although the protection could not be checked: %v", err)
		}
	})

	t.Run("the same purge is allowed when the ancestry is intact", func(t *testing.T) {
		clk := domain.NewFakeClock(time.Now())
		tree, _, objectName, due := protectedSubdirFixture(t, clk)

		done := mustCLIOn(t, clk, "purge", "apply",
			"--id", dataString(t, due, "plan_id"),
			"--digest", dataString(t, due, "digest"), "--json")
		if c := countsOf(t, done); c["completed_count"] != float64(1) {
			t.Fatalf("counts = %v, want the unrelated protection not to block a due purge", c)
		}
		if _, err := os.Stat(vaultPathOf(tree, objectName)); !os.IsNotExist(err) {
			t.Fatalf("the payload is still in the vault: %v", err)
		}
	})
}

// TestSlimProductionUsesTheRealClock closes the seam from the other side.
//
// The injectable clock exists for tests. If production ever reached it, every
// retention decision would be computed from a clock the operator controls. The
// check is behavioural: a background run through the production entry point
// records the trusted instant, and that instant has to be now.
func TestSlimProductionUsesTheRealClock(t *testing.T) {
	newHome(t)
	mustCLI(t, "init", "--json")
	mustCLI(t, "run", "--json")

	st := openStateStore(t)
	defer st.Close()

	raw, err := st.MetaGet(t.Context(), "last_run_ns")
	if err != nil {
		t.Fatalf("read the run ledger: %v", err)
	}
	ns := int64(0)
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			t.Fatalf("last_run_ns = %q", raw)
		}
		ns = ns*10 + int64(raw[i]-'0')
	}
	if ns == 0 {
		t.Fatal("the run recorded no instant")
	}
	if d := time.Since(time.Unix(0, ns)); d < -time.Minute || d > 5*time.Minute {
		t.Fatalf("the production entry point recorded %s away from now, so it is not using the system clock",
			d)
	}
}

func stringOr(v any) string {
	s, _ := v.(string)
	return s
}

func numberOr(v any) float64 {
	f, _ := v.(float64)
	return f
}
