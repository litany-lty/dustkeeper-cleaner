package main

import (
	"strings"
	"testing"
)

// These two cases are the command-line half of D01 and D09. They exist because
// both defects were combinations of two individually reasonable commands: an
// approval that replaced another one without retiring it, and a confirmation line
// that dropped the ceiling the user had just chosen.

// nextSteps returns the follow-up commands an envelope printed.
func nextSteps(t *testing.T, r result) []string {
	t.Helper()
	raw, ok := r.env["next_steps"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("the envelope printed no next_steps: %s", r.stdout)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// runPrintedCommand runs a command the tool printed, exactly as printed, and
// asks for the machine-readable envelope so the assertions can read the result.
// The arguments of the printed line are used unchanged: nothing a test adds may
// stand in for a ceiling the tool forgot to print.
func runPrintedCommand(t *testing.T, line string) result {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "dustkeeper" {
		t.Fatalf("the printed command does not start with the program name: %q", line)
	}
	args := append(fields[1:], "--json")
	return mustCLI(t, args...)
}

// TestSlimEnableCommandCarriesTheApprovedCeilings is D09: the line printed after
// a preview commits the same screen the preview described.
func TestSlimEnableCommandCarriesTheApprovedCeilings(t *testing.T) {
	base, tree := enableTree(t, "managed_disposable")
	setRuntime(t, base, map[string]any{
		"max_auto_quarantine_files": 7,
		"max_auto_purge_files":      3,
	})

	view := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup",
		"--max-files", "5", "--purge-max-files", "10", "--json")

	line := nextSteps(t, view)[0]
	// Without these the printed line recomputes the digest with the configured
	// defaults (7 and 3) and the commit is refused as PLAN_STALE.
	for _, want := range []string{"--max-files 5", "--purge-max-files 10"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the confirmation command dropped %q:\n%s", want, line)
		}
	}

	// The preview's ceilings object must describe the numbers, not two empty
	// objects: `{}` per capability reads as "nothing was approved".
	ceilings, ok := dataOf(t, view)["ceilings"].(map[string]any)
	if !ok {
		t.Fatalf("the preview has no ceilings object: %v", dataOf(t, view))
	}
	q, ok := ceilings["auto_quarantine"].(map[string]any)
	if !ok {
		t.Fatalf("ceilings.auto_quarantine is %T, want an object with numbers", ceilings["auto_quarantine"])
	}
	if got, _ := q["files"].(float64); got != 5 {
		t.Fatalf("ceilings.auto_quarantine.files = %v, want 5", q["files"])
	}
	p, ok := ceilings["auto_purge"].(map[string]any)
	if !ok {
		t.Fatalf("ceilings.auto_purge is %T, want an object with numbers", ceilings["auto_purge"])
	}
	if got, _ := p["files"].(float64); got != 10 {
		t.Fatalf("ceilings.auto_purge.files = %v, want 10", p["files"])
	}

	// The human screen names the directory and shows what the count counted.
	human := cli(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup",
		"--max-files", "5", "--purge-max-files", "10")
	if human.code != 0 {
		t.Fatalf("the human preview exited %d:\n%s\n%s", human.code, human.stdout, human.stderr)
	}
	if !strings.Contains(human.stdout, tree) {
		t.Fatalf("the screen never names the scope root %q:\n%s", tree, human.stdout)
	}
	if !strings.Contains(human.stdout, "old.zip") {
		t.Fatalf("the screen says the files are listed above but lists none:\n%s", human.stdout)
	}
	if !strings.Contains(human.stdout, "*.zip") {
		t.Fatalf("the screen does not explain what the rule matches:\n%s", human.stdout)
	}

	// The printed command is the commit, and it approves the numbers on screen.
	committed := runPrintedCommand(t, line)
	grants, _ := dataOf(t, committed)["grants"].([]any)
	if len(grants) != 2 {
		t.Fatalf("the commit reported %d grant(s), want 2", len(grants))
	}
	seen := map[string]float64{}
	for _, raw := range grants {
		g, _ := raw.(map[string]any)
		capability, _ := g["capability"].(string)
		seen[capability], _ = g["max_files"].(float64)
	}
	if seen["auto_quarantine"] != 5 || seen["auto_purge"] != 10 {
		t.Fatalf("the committed ceilings are %v, want 5 and 10", seen)
	}
}

// TestSlimSmallerApprovalWins is D01 seen from the command line: the sequence is
// the one the review used - approve a large ceiling, then approve a smaller one
// through the retained `rule authorize`, then enable the smaller one through the
// new entry point - and only the last approval may be active afterwards.
func TestSlimSmallerApprovalWins(t *testing.T) {
	base, _ := enableTree(t, "managed_disposable")
	setRuntime(t, base, map[string]any{
		"max_auto_quarantine_files": 100,
		"max_auto_purge_files":      100,
	})

	// 1. The new entry point approves 100 files for permanent deletion.
	big := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--json")
	runPrintedCommand(t, nextSteps(t, big)[0])
	_, _, grants := ruleState(t)
	bigPurge := activeGrant(t, grants, "auto_purge")
	if bigPurge == nil {
		t.Fatalf("the first enable left no active purge grant: %v", grants)
	}

	// 2. The retained single-capability command approves 10. Under the old code
	// this inserted a second active row and left the 100-file approval in force.
	proposal := mustCLI(t, "rule", "authorize", "--id", "r1", "--revision", "1",
		"--auto-purge", "--max-files", "10", "--json")
	approval := dataString(t, proposal, "approval_digest")
	mustCLI(t, "rule", "authorize", "--id", "r1", "--revision", "1",
		"--auto-purge", "--max-files", "10", "--digest", approval, "--json")

	actives := activeGrants(t, ruleStateGrants(t), "auto_purge")
	if len(actives) != 1 {
		t.Fatalf("%d active purge grants after confirming 10 files, want exactly 1: %v", len(actives), actives)
	}
	if got, _ := actives[0]["Digest"].(string); got != approval {
		t.Fatalf("the active purge grant is %v, want the approval just confirmed", actives[0]["ID"])
	}
	if old := grantStatus(t, bigPurge["ID"]); old != "revoked" {
		t.Fatalf("the 100-file approval is %q after a 10-file approval; the larger authority must not survive", old)
	}

	// 3. Re-enabling with the smaller ceiling through the new entry point is
	// idempotent and still leaves one active grant.
	view := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup",
		"--purge-max-files", "10", "--json")
	committed := runPrintedCommand(t, nextSteps(t, view)[0])
	// The enable digest names the whole screen; the grant's own digest names the
	// capability. The active row has to carry the latter.
	wantGrant := ""
	for _, raw := range dataOf(t, committed)["grants"].([]any) {
		g, _ := raw.(map[string]any)
		if g["capability"] == "auto_purge" {
			wantGrant, _ = g["digest"].(string)
		}
	}
	if wantGrant == "" {
		t.Fatalf("the commit reported no purge grant: %s", committed.stdout)
	}
	actives = activeGrants(t, ruleStateGrants(t), "auto_purge")
	if len(actives) != 1 {
		t.Fatalf("%d active purge grants after re-enabling, want exactly 1: %v", len(actives), actives)
	}
	if got, _ := actives[0]["Digest"].(string); got != wantGrant {
		t.Fatalf("the active purge grant does not carry the last confirmed binding")
	}
}

// ruleStateGrants is the grant list from `scope show`.
func ruleStateGrants(t *testing.T) []map[string]any {
	t.Helper()
	_, _, grants := ruleState(t)
	return grants
}

// activeGrants returns every active grant of one capability, which is at most one
// by contract - the count is what the assertions check.
func activeGrants(t *testing.T, grants []map[string]any, capability string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, g := range grants {
		if g["Capability"] == capability && g["Status"] == "active" {
			out = append(out, g)
		}
	}
	return out
}

// grantStatus reads one grant's status through the same envelope a script would
// use.
func grantStatus(t *testing.T, id any) string {
	t.Helper()
	shown := mustCLI(t, "scope", "show", "--id", "exports", "--json")
	raw, _ := dataOf(t, shown)["grants"].([]any)
	want, _ := id.(string)
	for _, v := range raw {
		g, _ := v.(map[string]any)
		if got, _ := g["ID"].(string); got == want {
			status, _ := g["Status"].(string)
			return status
		}
	}
	t.Fatalf("grant %q is not in the scope report", want)
	return ""
}
