package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the real command line: the same flag parsing, the same
// routing and the same envelopes a script would see. They exist because the
// atomic enable is a promise about what does *not* happen - no write without a
// digest, no half-written approval - and that promise is only visible from
// outside.

const enableRuleDSL = `{"dsl_version":1,"scope_id":"%s","action":"quarantine",` +
	`"match":{"min_mtime_age_seconds":2592000,"min_size_bytes":0,"max_size_bytes":1073741824,` +
	`"relative_dir_prefixes":[],"basename_patterns":["*.zip"]},` +
	`"except":{"relative_dir_prefixes":[],"basename_patterns":[]},"retention_seconds":604800}`

// enableTree registers one scope of the given kind, puts one old matching file
// in it, scans it and imports revision 1 of rule r1. It returns the base
// directory so a test can rewrite the configuration afterwards.
func enableTree(t *testing.T, kind string) (base, tree string) {
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
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	mustCLI(t, "init", "--json")
	mustCLI(t, "scope", "add", "--path", tree, "--kind", kind, "--name", "exports", "--json")
	mustCLI(t, "scan", "--scope", "exports", "--json")

	rulePath := filepath.Join(base, "rule.json")
	if err := os.WriteFile(rulePath, []byte(strings.Replace(enableRuleDSL, "%s", "exports", 1)), 0o600); err != nil {
		t.Fatalf("write rule: %v", err)
	}
	mustCLI(t, "rule", "import", "--file", rulePath, "--id", "r1", "--json")
	return base, tree
}

// setRuntime rewrites runtime keys in the configuration `init` wrote.
func setRuntime(t *testing.T, base string, kv map[string]any) {
	t.Helper()
	cfgPath := filepath.Join(base, "config", "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	runtime, _ := cfg["runtime"].(map[string]any)
	if runtime == nil {
		t.Fatalf("the default config has no runtime block")
	}
	for k, v := range kv {
		runtime[k] = v
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// ruleState reads back the stored revision and every grant of the scope.
func ruleState(t *testing.T) (status, reviewBinding string, grants []map[string]any) {
	t.Helper()
	shown := dataOf(t, mustCLI(t, "scope", "show", "--id", "exports", "--json"))
	rules, _ := shown["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("%d rule revision(s) stored, want 1", len(rules))
	}
	row, _ := rules[0].(map[string]any)
	status, _ = row["Status"].(string)
	reviewBinding, _ = row["ReviewBindingJSON"].(string)
	raw, _ := shown["grants"].([]any)
	for _, g := range raw {
		if m, ok := g.(map[string]any); ok {
			grants = append(grants, m)
		}
	}
	return status, reviewBinding, grants
}

func activeGrant(t *testing.T, grants []map[string]any, capability string) map[string]any {
	t.Helper()
	for _, g := range grants {
		if g["Capability"] == capability && g["Status"] == "active" {
			return g
		}
	}
	return nil
}

// TestSlimEnableIsOneConfirmation is A04 seen from the command line: reading the
// screen writes nothing, a stale digest is refused, and one matching digest
// performs rule activation, the review binding and both capabilities together.
func TestSlimEnableIsOneConfirmation(t *testing.T) {
	base, _ := enableTree(t, "managed_disposable")
	// The two capabilities read two configuration keys. If the purge ceiling came
	// from the quarantine default, the numbers below could not be different.
	setRuntime(t, base, map[string]any{
		"max_auto_quarantine_files": 7,
		"max_auto_purge_files":      3,
	})

	// 1. The preview. Nothing may be written by it: the draft imported above is
	// unchanged, and no capability exists yet.
	beforeStatus, beforeBinding, beforeGrants := ruleState(t)
	view := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--json")
	d := dataOf(t, view)
	if d["mode"] != "cleanup" {
		t.Fatalf("mode = %v, want cleanup", d["mode"])
	}
	if d["scope_kind"] != "managed_disposable" {
		t.Fatalf("scope_kind = %v", d["scope_kind"])
	}
	if got, _ := d["retention_seconds"].(float64); got != 604800 {
		t.Fatalf("retention_seconds = %v, want the 604800 the DSL approved", d["retention_seconds"])
	}
	scan, _ := d["scan"].(map[string]any)
	if scan["complete"] != true {
		t.Fatalf("scan is reported incomplete after a finished scan: %v", scan)
	}
	cand, _ := d["candidates"].(map[string]any)
	if got, _ := cand["matched"].(float64); got != 1 {
		t.Fatalf("candidates.matched = %v, want the one old zip", cand["matched"])
	}
	digest := dataString(t, view, "enable_digest")

	caps, _ := d["capabilities"].(map[string]any)
	rawGrants, _ := caps["grants"].([]any)
	if len(rawGrants) != 2 {
		t.Fatalf("the cleanup mode offered %d capabilities, want quarantine and purge", len(rawGrants))
	}
	// Quarantine first, permanently: the order is part of the digest.
	first, _ := rawGrants[0].(map[string]any)
	if first["capability"] != "auto_quarantine" {
		t.Fatalf("the first capability is %v; quarantine comes first", first["capability"])
	}
	wantCeilings := map[string]float64{"auto_quarantine": 7, "auto_purge": 3}
	for _, raw := range rawGrants {
		g, _ := raw.(map[string]any)
		capability, _ := g["capability"].(string)
		if got, _ := g["approved_max_files"].(float64); got != wantCeilings[capability] {
			t.Fatalf("%s approved_max_files = %v, want %v (each capability reads its own key)",
				capability, g["approved_max_files"], wantCeilings[capability])
		}
	}

	if status, binding, grants := ruleState(t); status != beforeStatus || binding != beforeBinding || len(grants) != len(beforeGrants) {
		t.Fatalf("reading the screen changed state: status %q -> %q, binding changed %v, grants %d -> %d",
			beforeStatus, status, binding != beforeBinding, len(beforeGrants), len(grants))
	}

	// 2. A digest that belongs to another screen is refused and still writes nothing.
	stale := cli(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup",
		"--digest", "sha256:0000000000000000000000000000000000000000000000000000000000000000", "--json")
	if stale.code == 0 {
		t.Fatalf("a wrong enable digest was accepted:\n%s", stale.stdout)
	}
	if status, _, grants := ruleState(t); status != beforeStatus || len(grants) != len(beforeGrants) {
		t.Fatalf("a refused digest wrote state: status=%q grants=%d", status, len(grants))
	}

	// 2b. The same digest in the other mode is not the same approval either:
	// quarantine-only must not be able to commit the cleanup screen.
	wrongMode := cli(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "quarantine",
		"--digest", digest, "--json")
	if wrongMode.code == 0 {
		t.Fatalf("a digest from the cleanup screen committed the quarantine screen:\n%s", wrongMode.stdout)
	}

	// 3. The matching digest commits everything at once.
	committed := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup",
		"--digest", digest, "--json")
	got := dataOf(t, committed)["grants"].([]any)
	if len(got) != 2 {
		t.Fatalf("the commit reported %d grant(s), want 2", len(got))
	}
	status, binding, grants := ruleState(t)
	if status != "active" {
		t.Fatalf("rule status = %q after the commit, want active", status)
	}
	if binding == "" {
		t.Fatal("the commit left no review binding behind")
	}
	q := activeGrant(t, grants, "auto_quarantine")
	p := activeGrant(t, grants, "auto_purge")
	if q == nil || p == nil {
		t.Fatalf("after the commit the active grants are %v", grants)
	}

	// 4. The same confirmation again is a no-op, not a second approval.
	again := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup",
		"--digest", digest, "--json")
	if dataString(t, again, "enable_digest") != digest {
		t.Fatal("the digest of the same approval changed on the second read")
	}
	_, _, grantsAgain := ruleState(t)
	if len(grantsAgain) != 2 {
		t.Fatalf("%d grant row(s) after the retry, want the same 2", len(grantsAgain))
	}
	if q2 := activeGrant(t, grantsAgain, "auto_quarantine"); q2["ID"] != q["ID"] {
		t.Fatalf("the retry created %v instead of reusing %v", q2["ID"], q["ID"])
	}

	// 5. Switching back to quarantine takes the purge authority away in the same
	// single transaction that re-activates the revision.
	qView := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "quarantine", "--json")
	mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "quarantine",
		"--digest", dataString(t, qView, "enable_digest"), "--json")
	_, _, afterSwitch := ruleState(t)
	if activeGrant(t, afterSwitch, "auto_purge") != nil {
		t.Fatal("switching back to quarantine left a purge grant active")
	}
	if activeGrant(t, afterSwitch, "auto_quarantine") == nil {
		t.Fatal("switching back to quarantine lost the quarantine grant")
	}
}

// TestSlimEnableDoesNotExpandOldPurgeAuthority is A05: enabling never widens
// what was already approved, a zero ceiling is a refusal rather than a default,
// and tightening the configuration afterwards does not mint a new approval.
func TestSlimEnableDoesNotExpandOldPurgeAuthority(t *testing.T) {
	base, _ := enableTree(t, "managed_disposable")
	setRuntime(t, base, map[string]any{
		"max_auto_quarantine_files": 7,
		"max_auto_purge_files":      0,
		"max_auto_purge_bytes":      0,
	})

	beforeStatus, beforeBinding, beforeGrants := ruleState(t)
	// A zero ceiling forbids the action. It is not read as "use the default",
	// and the refusal happens before anything is written.
	zero := cli(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--json")
	if zero.code == 0 {
		t.Fatalf("cleanup was offered with a zero purge ceiling:\n%s", zero.stdout)
	}
	if !strings.Contains(zero.stderr+zero.stdout, "max_auto_purge") {
		t.Fatalf("the refusal does not name the key that is zero:\n%s%s", zero.stdout, zero.stderr)
	}
	if status, binding, grants := ruleState(t); status != beforeStatus || binding != beforeBinding || len(grants) != len(beforeGrants) {
		t.Fatalf("the failed cleanup wrote state: status=%q grants=%d", status, len(grants))
	}

	// Quarantine alone is still allowed while its own key is non-zero, and it
	// grants nothing about purge.
	qView := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "quarantine", "--json")
	mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "quarantine",
		"--digest", dataString(t, qView, "enable_digest"), "--json")
	_, _, grants := ruleState(t)
	if len(grants) != 1 || activeGrant(t, grants, "auto_purge") != nil {
		t.Fatalf("quarantine-only produced %v", grants)
	}
	qGrant := activeGrant(t, grants, "auto_quarantine")

	// Raising the purge key to a small number and enabling cleanup approves that
	// number, not the quarantine one.
	setRuntime(t, base, map[string]any{"max_auto_purge_files": 5, "max_auto_purge_bytes": 1 << 30})
	cView := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--json")
	for _, raw := range dataOf(t, cView)["capabilities"].(map[string]any)["grants"].([]any) {
		g, _ := raw.(map[string]any)
		if g["capability"] == "auto_purge" {
			if got, _ := g["approved_max_files"].(float64); got != 5 {
				t.Fatalf("the purge approval is %v files, want 5 - not the quarantine ceiling", g["approved_max_files"])
			}
		}
	}
	cDigest := dataString(t, cView, "enable_digest")
	mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--digest", cDigest, "--json")
	_, _, grants = ruleState(t)
	pGrant := activeGrant(t, grants, "auto_purge")
	if pGrant == nil {
		t.Fatalf("cleanup did not install a purge grant: %v", grants)
	}
	// Turning the purge authority on must not re-grant quarantine: the object
	// that is already isolated runs under the approval that isolated it.
	if activeGrant(t, grants, "auto_quarantine")["ID"] != qGrant["ID"] {
		t.Fatal("enabling cleanup replaced the quarantine approval instead of reusing it")
	}

	// Tightening the local limit afterwards does not rewrite the standing
	// approval. The stored grant keeps the number that was confirmed, and the
	// smaller configured number limits the work at run time, because the
	// effective ceiling is the smaller of the two.
	if got := approvedFiles(t, pGrant); got != 5 {
		t.Fatalf("the stored purge approval is %d files, want the confirmed 5", got)
	}
	setRuntime(t, base, map[string]any{"max_auto_purge_files": 2})
	tight := mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--json")
	// The screen now shows the smaller number, so its digest is different: no
	// screen may be confirmed with the digest of another screen.
	if dataString(t, tight, "enable_digest") == cDigest {
		t.Fatal("the screen shows a smaller ceiling but kept the older digest")
	}
	for _, raw := range dataOf(t, tight)["capabilities"].(map[string]any)["grants"].([]any) {
		g, _ := raw.(map[string]any)
		if g["capability"] == "auto_purge" {
			if got, _ := g["approved_max_files"].(float64); got != 2 {
				t.Fatalf("the preview offers %v purge files, want the 2 the configuration now allows", got)
			}
		}
	}
	// Confirming it narrows the approval; it cannot widen it.
	cTight := dataString(t, tight, "enable_digest")
	mustCLI(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "cleanup", "--digest", cTight, "--json")
	_, _, grants = ruleState(t)
	// The wider approval is revoked, not erased: three rows now, of which exactly
	// one purge grant is active.
	if len(grants) != 3 {
		t.Fatalf("%d grant row(s) after narrowing, want the two earlier rows plus the narrower one", len(grants))
	}
	narrowed := activeGrant(t, grants, "auto_purge")
	if narrowed == nil {
		t.Fatalf("no purge grant is active after narrowing: %v", grants)
	}
	if got := approvedFiles(t, narrowed); got != 2 {
		t.Fatalf("the narrowed purge approval is %d files, want 2", got)
	}
	if narrowed["ID"] == pGrant["ID"] {
		t.Fatal("a narrowed approval reused the id of the wider one, so no object can tell them apart")
	}
	for _, g := range grants {
		if g["ID"] == pGrant["ID"] && g["Status"] != "revoked" {
			t.Fatalf("the wider purge approval is %v, want revoked", g["Status"])
		}
	}
	if activeGrant(t, grants, "auto_quarantine")["ID"] != qGrant["ID"] {
		t.Fatal("narrowing the purge ceiling also replaced the untouched quarantine approval")
	}
}

// approvedFiles reads the number a grant actually approved, from the binding the
// grant was stored with rather than from the local configuration.
func approvedFiles(t *testing.T, g map[string]any) int {
	t.Helper()
	raw, _ := g["BindingJSON"].(string)
	if raw == "" {
		t.Fatalf("the grant has no binding: %v", g)
	}
	var b struct {
		ApprovedMaxFiles int `json:"approved_max_files"`
	}
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("decode the grant binding: %v", err)
	}
	return b.ApprovedMaxFiles
}

// TestSlimRuleLanguageBoundaries is A03: automatic work is only enabled on a
// directory the user marked disposable, and a rule the language cannot express
// exactly never becomes an executable draft.
func TestSlimRuleLanguageBoundaries(t *testing.T) {
	t.Run("an assisted scope keeps its manual route", func(t *testing.T) {
		enableTree(t, "assisted")
		r := cli(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", "quarantine", "--json")
		if r.code == 0 {
			t.Fatalf("an assisted scope was given background authority:\n%s", r.stdout)
		}
		if !strings.Contains(r.stderr, "managed_disposable") && !strings.Contains(r.stdout, "managed_disposable") {
			t.Fatalf("the refusal does not say which kind is required:\n%s%s", r.stdout, r.stderr)
		}
	})

	t.Run("an observe scope never gets automatic work", func(t *testing.T) {
		enableTree(t, "observe")
		for _, mode := range []string{"quarantine", "cleanup"} {
			r := cli(t, "rule", "enable", "--id", "r1", "--revision", "1", "--mode", mode, "--json")
			if r.code == 0 {
				t.Fatalf("an observe scope was given %s authority:\n%s", mode, r.stdout)
			}
		}
		// No grant was written by either attempt.
		if _, _, grants := ruleState(t); len(grants) != 0 {
			t.Fatalf("a refused enable wrote %d grant(s)", len(grants))
		}
	})

	t.Run("the language refuses what it cannot express exactly", func(t *testing.T) {
		base := newHome(t)
		tree := filepath.Join(base, "tree")
		if err := os.MkdirAll(tree, 0o755); err != nil {
			t.Fatalf("mkdir tree: %v", err)
		}
		mustCLI(t, "init", "--json")
		mustCLI(t, "scope", "add", "--path", tree, "--kind", "managed_disposable", "--name", "exports", "--json")

		valid := strings.Replace(enableRuleDSL, "%s", "exports", 1)
		cases := []struct {
			name string
			body string
			code string
		}{
			{"an unknown field", strings.Replace(valid, `"action":"quarantine"`, `"action":"quarantine","dedupe":true`, 1), "UNKNOWN_FIELD"},
			{"a missing required field", strings.Replace(valid, `"retention_seconds":604800`, `"retention":604800`, 1), "UNKNOWN_FIELD"},
			{"a missing size ceiling", strings.Replace(valid, `"max_size_bytes":1073741824,`, ``, 1), "DSL_INVALID"},
			{"purge as an action", strings.Replace(valid, `"action":"quarantine"`, `"action":"purge"`, 1), "RULE_UNSUPPORTED"},
			{"deletion as an action", strings.Replace(valid, `"action":"quarantine"`, `"action":"delete"`, 1), "RULE_UNSUPPORTED"},
			{"a glob that is not in the language", strings.Replace(valid, `"*.zip"`, `"**/*.zip"`, 1), "DSL_INVALID"},
			{"an unregistered scope", strings.Replace(valid, `"scope_id":"exports"`, `"scope_id":"elsewhere"`, 1), "DSL_INVALID"},
			{"a nested size instead of a number", strings.Replace(valid, `"min_size_bytes":0`, `"min_size_bytes":{"any":true}`, 1), "DSL_INVALID"},
		}
		for _, tc := range cases {
			path := filepath.Join(base, "case.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write %s: %v", tc.name, err)
			}
			r := cli(t, "rule", "validate", "--file", path, "--json")
			if r.code == 0 {
				t.Fatalf("%s was accepted:\n%s", tc.name, r.stdout)
			}
			if code := errorCode(r); code != tc.code {
				t.Fatalf("%s: code = %s, want %s\n%s%s", tc.name, code, tc.code, r.stdout, r.stderr)
			}
			// A draft that cannot be expressed must not be storable either: the
			// same bytes through the import route have to fail identically.
			imp := cli(t, "rule", "import", "--file", path, "--id", "bad", "--json")
			if imp.code == 0 {
				t.Fatalf("%s was stored as a draft:\n%s", tc.name, imp.stdout)
			}
		}
	})
}

// errorCode reads the stable code out of an envelope. The envelope is flat, so a
// refusal is a `code` on the object the command printed.
func errorCode(r result) string {
	if r.env == nil {
		return ""
	}
	code, _ := r.env["code"].(string)
	return code
}
