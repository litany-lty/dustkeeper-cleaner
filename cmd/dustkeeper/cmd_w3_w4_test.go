package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dustkeeper/migrations"
)

func runCLITest(t *testing.T, cfgDir, stateDir string, args ...string) (int, map[string]any, string) {
	t.Helper()
	t.Setenv("DUSTKEEPER_CONFIG_DIR", cfgDir)
	t.Setenv("DUSTKEEPER_STATE_DIR", stateDir)
	var out, errBuf bytes.Buffer
	fullArgs := append([]string{"--json"}, args...)
	code := dispatchWith(context.Background(), fullArgs, &out, &errBuf, nil)
	var parsed map[string]any
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid JSON output (%v): %s", err, out.String())
		}
	}
	return code, parsed, errBuf.String()
}

func TestW3AndW4CLIEndToEndPreservesSchema6AndZeroScopeMutation(t *testing.T) {
	cfgDir := t.TempDir()
	stateDir := t.TempDir()

	// 1. Initialise instance and verify schema stays at 6.
	code, initEnv, _ := runCLITest(t, cfgDir, stateDir, "init")
	if code != 0 {
		t.Fatalf("init failed (exit %d): %+v", code, initEnv)
	}
	if migrations.CurrentVersion != 6 {
		t.Fatalf("expected schema version 6 preserved, got %d", migrations.CurrentVersion)
	}

	// 2. W3 CLI: resumable read-only inventory without creating any scopes or grants.
	scanRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scanRoot, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scanRoot, "omega"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scanRoot, "alpha", "a.txt"), bytes.Repeat([]byte("a"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scanRoot, "omega", "big.dmg"), bytes.Repeat([]byte("z"), 128*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	code, inv1, _ := runCLITest(t, cfgDir, stateDir, "inventory", "--root", scanRoot, "--state-file", "auto", "--max-entries", "1")
	if code != 9 || inv1["code"] != "PARTIAL" {
		t.Fatalf("expected exit 9 PARTIAL on first bounded inventory call, got code=%d env=%+v", code, inv1)
	}
	code, inv2, _ := runCLITest(t, cfgDir, stateDir, "inventory", "--root", scanRoot, "--state-file", "auto", "--max-entries", "20")
	if code != 0 || inv2["code"] != "OK" {
		t.Fatalf("expected exit 0 OK on resumed inventory call, got code=%d env=%+v", code, inv2)
	}

	// Verify 0 scopes, 0 rules, 0 items in state.db after inventory.
	code, stEnv, _ := runCLITest(t, cfgDir, stateDir, "status")
	if code != 0 {
		t.Fatalf("status failed: %+v", stEnv)
	}
	stData := stEnv["data"].(map[string]any)
	stats := stData["stats"].(map[string]any)
	if int(stats["scopes"].(float64)) != 0 || int(stats["rules"].(float64)) != 0 || int(stats["items"].(float64)) != 0 {
		t.Fatalf("inventory must not register scopes/rules/items, got stats=%+v", stats)
	}

	// 3. W4 CLI: uv cache preview -> digest-confirmed prune -> second run NO_CANDIDATES zero-action.
	cacheDir := t.TempDir()
	toolDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "unused-wheel.whl"), bytes.Repeat([]byte("w"), 64*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "keep.marker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	realTool := filepath.Join(toolDir, "black-bin")
	if err := os.WriteFile(realTool, []byte("#!/bin/sh\necho ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTool, filepath.Join(toolDir, "black")); err != nil {
		t.Fatal(err)
	}

	fakeUV := filepath.Join(t.TempDir(), "uv")
	uvScript := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  --version) echo "uv 0.5.9 (cli-test)"; exit 0 ;;
  cache)
    if [ "${2:-}" = "dir" ]; then echo %q; exit 0; fi
    if [ "${2:-}" = "prune" ]; then
      rm -f %q/unused-*
      echo "Removed 1 file (64.0KiB)"
      exit 0
    fi
    ;;
  tool)
    if [ "${2:-}" = "dir" ]; then echo %q; exit 0; fi
    ;;
esac
exit 2
`, cacheDir, cacheDir, toolDir)
	if err := os.WriteFile(fakeUV, []byte(uvScript), 0o700); err != nil {
		t.Fatal(err)
	}

	code, prevEnv, _ := runCLITest(t, cfgDir, stateDir, "cache", "preview", "--tool", "uv", "--bin", fakeUV)
	if code != 0 {
		t.Fatalf("cache preview failed (exit %d): %+v", code, prevEnv)
	}
	digest := prevEnv["data"].(map[string]any)["digest"].(string)

	code, pruneEnv, _ := runCLITest(t, cfgDir, stateDir, "cache", "prune", "--tool", "uv", "--bin", fakeUV, "--digest", digest)
	if code != 0 || pruneEnv["code"] != "OK" {
		t.Fatalf("cache prune failed (exit %d): %+v", code, pruneEnv)
	}
	pruneData := pruneEnv["data"].(map[string]any)
	if int64(pruneData["reclaimed_logical_bytes"].(float64)) != 64*1024 {
		t.Fatalf("expected 64 KiB reclaimed, got %+v", pruneData)
	}

	// Second preview + prune on unchanged cache returns NO_CANDIDATES (exit 0) zero-action.
	_, prev2Env, _ := runCLITest(t, cfgDir, stateDir, "cache", "preview", "--tool", "uv", "--bin", fakeUV)
	digest2 := prev2Env["data"].(map[string]any)["digest"].(string)
	code, prune2Env, _ := runCLITest(t, cfgDir, stateDir, "cache", "prune", "--tool", "uv", "--bin", fakeUV, "--digest", digest2)
	if code != 0 || prune2Env["code"] != "NO_CANDIDATES" {
		t.Fatalf("expected NO_CANDIDATES zero-action on second run, got code=%d env=%+v", code, prune2Env)
	}
}
