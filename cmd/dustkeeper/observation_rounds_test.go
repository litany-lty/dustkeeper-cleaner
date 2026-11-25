package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/review"
	"dustkeeper/internal/run"
)

func TestO07ScanPartialSetsStatusAndListsFailedDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 still readable as root")
	}
	base := newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init: %s", r.stdout)
	}
	tree := filepath.Join(base, "tree")
	bad := filepath.Join(tree, "unreadable_box")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "ok.tmp"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "hidden.tmp"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, "scope", "add", "--path", tree, "--kind", "managed_disposable", "--name", "o07", "--json"); r.code != 0 {
		t.Fatalf("scope add: %s", r.stdout)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o700) })

	r := cli(t, "scan", "--scope", "o07", "--json")
	if r.code != errs.Exit(errs.CodePartial) {
		t.Fatalf("scan exit=%d, want %d: %s", r.code, errs.Exit(errs.CodePartial), r.stdout)
	}
	if got := r.str(t, "status"); got != string(errs.StatusPartial) {
		t.Fatalf("status=%q, want %q: %s", got, errs.StatusPartial, r.stdout)
	}
	data := r.env["data"].(map[string]any)
	paths, ok := data["failed_dir_paths"].([]any)
	if !ok || len(paths) != 1 || paths[0] != "unreadable_box" {
		t.Fatalf("expected failed_dir_paths=[\"unreadable_box\"], got %+v", data["failed_dir_paths"])
	}
}

func TestO08RulePreviewAndPlanCreateExposeReasonsOnNoCandidates(t *testing.T) {
	base := newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init: %s", r.stdout)
	}
	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "fresh.zip"), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, "scope", "add", "--path", tree, "--kind", "managed_disposable", "--name", "o08", "--json"); r.code != 0 {
		t.Fatalf("scope add: %s", r.stdout)
	}
	ruleFile := filepath.Join(base, "rule.json")
	ruleRaw, _ := json.Marshal(map[string]any{
		"dsl_version": 1, "scope_id": "o08", "action": "quarantine",
		"match": map[string]any{
			"min_mtime_age_seconds": 86400, "min_size_bytes": 0, "max_size_bytes": 1073741824,
			"relative_dir_prefixes": []string{}, "basename_patterns": []string{"*.zip"},
		},
		"except":            map[string]any{"relative_dir_prefixes": []string{}, "basename_patterns": []string{}},
		"retention_seconds": 604800,
	})
	if err := os.WriteFile(ruleFile, ruleRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, "rule", "import", "--file", ruleFile, "--id", "r-o08", "--json"); r.code != 0 {
		t.Fatalf("rule import: %s", r.stdout)
	}

	// Pre-scan: scanned=0, suggests scan.
	pre := cli(t, "rule", "preview", "--id", "r-o08", "--revision", "1", "--json")
	if pre.str(t, "code") != string(errs.CodeNoCandidates) {
		t.Fatalf("expected NO_CANDIDATES, got %s", pre.stdout)
	}
	preData, ok := pre.env["data"].(map[string]any)
	if !ok || int(preData["scanned"].(float64)) != 0 {
		t.Fatalf("expected data.scanned=0 on pre-scan preview, got %s", pre.stdout)
	}
	if next, _ := pre.env["next_steps"].([]any); len(next) == 0 || !strings.Contains(next[0].(string), "scan --scope o08") {
		t.Fatalf("expected scan next step on unscanned preview, got %+v", pre.env["next_steps"])
	}

	// Post-scan with fresh.zip: exposes reasons breakdown on preview & plan create.
	if r := cli(t, "scan", "--scope", "o08", "--json"); r.code != 0 {
		t.Fatalf("scan: %s", r.stdout)
	}
	for _, args := range [][]string{
		{"rule", "preview", "--id", "r-o08", "--revision", "1", "--json"},
		{"plan", "create", "--rule", "r-o08", "--revision", "1", "--json"},
	} {
		res := cli(t, args...)
		if res.str(t, "code") != string(errs.CodeNoCandidates) {
			t.Fatalf("%v expected NO_CANDIDATES, got %s", args, res.stdout)
		}
		d, ok := res.env["data"].(map[string]any)
		if !ok || int(d["scanned"].(float64)) != 1 {
			t.Fatalf("%v missing data.scanned=1: %s", args, res.stdout)
		}
		reasons, _ := d["reasons"].(map[string]any)
		if int(reasons[policy.ReasonTooNew].(float64)) != 1 {
			t.Fatalf("%v missing %s=1 in reasons: %s", args, policy.ReasonTooNew, res.stdout)
		}
	}
}

func TestO09StatusReportsObservationsScanInterruptsAndHealth(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 still readable as root")
	}
	base := newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init: %s", r.stdout)
	}
	tree := filepath.Join(base, "tree")
	bad := filepath.Join(tree, "locked_dir")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "seen.zip"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "x.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, "scope", "add", "--path", tree, "--kind", "managed_disposable", "--name", "o09", "--json"); r.code != 0 {
		t.Fatalf("scope add: %s", r.stdout)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o700) })

	// Incomplete scan leaves scan_interrupts=1 on scope row.
	_ = cli(t, "scan", "--scope", "o09", "--json")

	// Write an unhealthy health.json and an unresolved review_unknown row.
	stateDir := filepath.Join(base, "state")
	if _, err := run.UpdateHealthFile(stateDir, run.Report{
		RunID:        "run_o09",
		Control:      domain.ControlStopRun,
		ClockTrusted: false,
		ClockNote:    "clock jumped backwards",
	}, nil, time.Now().UnixNano()); err != nil {
		t.Fatalf("update health: %v", err)
	}

	stJSON := cli(t, "status", "--json")
	if stJSON.code != 0 {
		t.Fatalf("status --json: %s", stJSON.stdout)
	}
	data := stJSON.env["data"].(map[string]any)
	scopes := data["scopes"].([]any)
	row := scopes[0].(map[string]any)
	if int(row["observation_count"].(float64)) != 1 {
		t.Fatalf("expected observation_count=1, got %+v", row)
	}
	if _, ok := row["scan_interrupts"]; !ok || int(row["scan_interrupts"].(float64)) != 1 {
		t.Fatalf("expected scan_interrupts=1 in status json, got %+v", row)
	}
	if _, ok := data["unresolved_reviews"]; !ok {
		t.Fatalf("expected unresolved_reviews in status json, got keys %v", keys(data))
	}
	hs, _ := data["last_run_health"].(map[string]any)
	if hs == nil || hs["healthy"] != false {
		t.Fatalf("expected unhealthy last_run_health in status json, got %+v", data["last_run_health"])
	}

	stHuman := cli(t, "status")
	if stHuman.code != 0 {
		t.Fatalf("status human: %s", stHuman.stdout)
	}
	for _, want := range []string{"observed=1", "scan_interrupts=1", "unhealthy"} {
		if !strings.Contains(stHuman.stdout, want) {
			t.Fatalf("status human output missing %q:\n%s", want, stHuman.stdout)
		}
	}
}

func TestO10DoctorValidatesReviewConfigReadOnlyWithoutNetwork(t *testing.T) {
	base := newHome(t)
	if r := cli(t, "init", "--json"); r.code != 0 {
		t.Fatalf("init: %s", r.stdout)
	}

	// 1) Without review config, doctor still passes and reports review_config.
	d0 := cli(t, "doctor", "--json")
	if d0.code != 0 {
		t.Fatalf("doctor without review should pass: %s", d0.stdout)
	}
	if !hasDoctorCheck(t, d0, "review_config", true) {
		t.Fatalf("expected ok review_config check when omitted: %s", d0.stdout)
	}

	// Create valid 0600 credentials and PEM CA, plus a symlinked CA alias and 0644 creds.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	canonBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caReal := filepath.Join(canonBase, "ca-real.pem")
	caLink := filepath.Join(canonBase, "ca-link.pem")
	if err := os.WriteFile(caReal, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(caReal, caLink); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(canonBase, "creds.json")
	if err := os.WriteFile(creds, []byte(`{"typesafe_api_key":"k1","minimax_api_key":"k2"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	writeCfg := func(caPath string) {
		cfg := config.Default()
		cfg.Runtime.MinFreePercent = 1
		cfg.Runtime.MinFreeBytes = 0
		cfg.Review = &review.Settings{
			CredentialsFile: creds,
			TrustCAFile:     caPath,
			JEVURL:          "https://127.0.0.1:65432/jev",
			JEVModel:        "jev-test",
			MiniMaxURL:      "https://127.0.0.1:65432/minimax",
			MiniMaxModel:    "minimax-test",
			MaxCandidates:   1,
		}
		raw, err := config.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "config", "config.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 2) Symlink trust_ca_file must fail doctor read-only with MODEL_UNAVAILABLE.
	writeCfg(caLink)
	dBad := cli(t, "doctor", "--json")
	if dBad.code == 0 || dBad.str(t, "code") != string(errs.CodeModelUnavailable) {
		t.Fatalf("expected MODEL_UNAVAILABLE for symlink trust_ca_file, got code=%d: %s", dBad.code, dBad.stdout)
	}
	if !hasDoctorCheck(t, dBad, "review_config", false) {
		t.Fatalf("expected failing review_config check: %s", dBad.stdout)
	}

	// 3) Real non-symlink 0600 CA & credentials must pass without network calls.
	writeCfg(caReal)
	dGood := cli(t, "doctor", "--json")
	if dGood.code != 0 {
		t.Fatalf("expected doctor exit 0 for valid read-only review config, got %d: %s", dGood.code, dGood.stdout)
	}
	if !hasDoctorCheck(t, dGood, "review_config", true) {
		t.Fatalf("expected ok review_config check: %s", dGood.stdout)
	}
}

func hasDoctorCheck(t *testing.T, r result, name string, wantOK bool) bool {
	t.Helper()
	data, _ := r.env["data"].(map[string]any)
	checks, _ := data["checks"].([]any)
	for _, item := range checks {
		c, _ := item.(map[string]any)
		if c["name"] == name {
			return c["ok"] == wantOK
		}
	}
	return false
}
