package cachetool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/errs"
)

// TestHelperFakeUV acts as a deterministic uv binary when invoked as a child
// process with FakeUVMarkerFile present alongside a wrapper script or symlink.
func buildFakeUV(t *testing.T, defaultCacheDir, defaultToolDir, countFile string) string {
	t.Helper()
	binDir := t.TempDir()
	script := filepath.Join(binDir, "uv")
	body := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then
  echo "uv 0.5.9 (test-fixture)"
  exit 0
fi
if [ "${1:-}" = "cache" ] && [ "${2:-}" = "dir" ]; then
  echo %q
  exit 0
fi
if [ "${1:-}" = "tool" ] && [ "${2:-}" = "dir" ]; then
  echo %q
  exit 0
fi
if [ "${1:-}" = "cache" ] && [ "${2:-}" = "prune" ]; then
  echo "1" >> %q
  target=""
  shift 2
  while [ $# -gt 0 ]; do
    case "$1" in
      --cache-dir) shift; target="$1" ;;
      *) echo "unexpected arg: $1" >&2; exit 2 ;;
    esac
    shift
  done
  if [ -z "$target" ] || [ ! -d "$target" ]; then
    echo "missing cache dir" >&2
    exit 2
  fi
  removed=0
  for f in "$target"/unused-*; do
    if [ -f "$f" ]; then
      rm -f "$f"
      removed=$((removed + 1))
    fi
  done
  if [ "$removed" -gt 0 ]; then
    echo "Removed $removed files (128.0KiB)"
  else
    echo "No unused entries found"
  fi
  exit 0
fi
echo "unknown args: $*" >&2
exit 2
`, defaultCacheDir, defaultToolDir, countFile)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func setupUVFixture(t *testing.T) (stateDir, cacheDir, toolDir, uvBin, countFile string) {
	t.Helper()
	stateDir = t.TempDir()
	cacheDir = t.TempDir()
	toolDir = t.TempDir()
	countFile = filepath.Join(t.TempDir(), "prune-invocations.log")

	// Populate cache with unused wheels and an active entry.
	if err := os.WriteFile(filepath.Join(cacheDir, "unused-1.whl"), bytes.Repeat([]byte("u"), 64*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "unused-2.whl"), bytes.Repeat([]byte("v"), 64*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "active-env.marker"), bytes.Repeat([]byte("k"), 8*1024), 0o600); err != nil {
		t.Fatal(err)
	}

	// Populate protected uv tool environment with executable and symlink.
	envBinDir := filepath.Join(toolDir, "ruff", "bin")
	if err := os.MkdirAll(envBinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realExe := filepath.Join(envBinDir, "ruff-real")
	if err := os.WriteFile(realExe, []byte("#!/bin/sh\necho ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExe, filepath.Join(toolDir, "ruff-cli")); err != nil {
		t.Fatal(err)
	}

	uvBin = buildFakeUV(t, cacheDir, toolDir, countFile)
	return
}

func countInvocations(countFile string) int {
	b, err := os.ReadFile(countFile)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

func TestW4PreviewChangesNothingAndReportsUpperBound(t *testing.T) {
	stateDir, cacheDir, _, uvBin, countFile := setupUVFixture(t)

	prev, err := Preview(context.Background(), Options{
		InstanceID: "inst_test",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}
	if prev.ReclaimLowerBoundKnown || prev.MinimumReclaimGuaranteed != 0 {
		t.Fatalf("expected unknown lower bound for uv, got known=%v min=%d",
			prev.ReclaimLowerBoundKnown, prev.MinimumReclaimGuaranteed)
	}
	if prev.UpperBoundAllocatedBytes <= 0 || prev.Files != 3 {
		t.Fatalf("expected 3 files and positive upper bound, got files=%d bytes=%d",
			prev.Files, prev.UpperBoundAllocatedBytes)
	}
	if !strings.HasPrefix(string(prev.Digest), "sha256:") {
		t.Fatalf("expected sha256 digest, got %q", prev.Digest)
	}
	if len(prev.ProtectedEnvs) == 0 {
		t.Fatalf("expected protected tool environment to be discovered")
	}
	if countInvocations(countFile) != 0 {
		t.Fatalf("preview must not invoke prune")
	}
	entries, _ := os.ReadDir(cacheDir)
	if len(entries) != 3 {
		t.Fatalf("preview modified cache entries: got %d", len(entries))
	}
}

func TestW4PruneReclaimsSpaceKeepsToolEnvAndSecondRunIsZeroAction(t *testing.T) {
	stateDir, _, toolDir, uvBin, countFile := setupUVFixture(t)
	// Ensure ambient UV_CACHE_DIR cannot redirect execution away from the approved cache root.
	decoyDir := t.TempDir()
	t.Setenv("UV_CACHE_DIR", decoyDir)

	now := time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC).UnixNano()
	clockFn := func() int64 { now += int64(time.Second); return now }

	prev, err := Preview(context.Background(), Options{
		InstanceID: "inst_test",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		NowNS:      clockFn,
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}

	// Wrong digest must be rejected without invoking uv.
	if _, err := Prune(context.Background(), Options{
		InstanceID: "inst_test",
		RunID:      "run_bad",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		Digest:     "sha256:0000",
		NowNS:      clockFn,
	}); errs.CodeOf(err) != errs.CodeDigestMismatch {
		t.Fatalf("expected DIGEST_MISMATCH, got %v", err)
	}
	if countInvocations(countFile) != 0 {
		t.Fatalf("rejected digest must not spawn uv")
	}

	// Valid confirmed prune reclaims space and preserves protected tool env.
	rec1, err := Prune(context.Background(), Options{
		InstanceID: "inst_test",
		RunID:      "run_1",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		Digest:     string(prev.Digest),
		NowNS:      clockFn,
	})
	if err != nil {
		t.Fatalf("Prune 1 failed: %v", err)
	}
	if rec1.State != "completed" || rec1.ZeroAction {
		t.Fatalf("expected completed active prune, got %+v", rec1)
	}
	if rec1.ReclaimedLogicalBytes != 128*1024 || rec1.PostFiles != 1 {
		t.Fatalf("expected 128 KiB logical reclaimed and 1 remaining file, got logical=%d files=%d",
			rec1.ReclaimedLogicalBytes, rec1.PostFiles)
	}
	if !rec1.ProtectedEnvsIntact {
		t.Fatalf("expected protected tool envs to remain intact")
	}
	if target, err := filepath.EvalSymlinks(filepath.Join(toolDir, "ruff-cli")); err != nil || !strings.HasSuffix(target, "ruff-real") {
		t.Fatalf("protected tool symlink broken: target=%q err=%v", target, err)
	}
	if countInvocations(countFile) != 1 {
		t.Fatalf("expected 1 uv prune invocation, got %d", countInvocations(countFile))
	}

	// Second normal run with no new cache growth becomes an explainable zero-action without spawning uv prune again.
	prev2, err := Preview(context.Background(), Options{
		InstanceID: "inst_test",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		NowNS:      clockFn,
	})
	if err != nil {
		t.Fatalf("Preview 2 failed: %v", err)
	}
	rec2, err := Prune(context.Background(), Options{
		InstanceID: "inst_test",
		RunID:      "run_2",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		Digest:     string(prev2.Digest),
		NowNS:      clockFn,
	})
	if err != nil {
		t.Fatalf("Prune 2 failed: %v", err)
	}
	if !rec2.ZeroAction || rec2.ZeroActionReason == "" {
		t.Fatalf("expected second run to be explainable zero-action, got %+v", rec2)
	}
	if countInvocations(countFile) != 1 {
		t.Fatalf("second run with unchanged cache must not re-invoke uv prune, got %d", countInvocations(countFile))
	}
}

func TestW4InterruptedPendingIntentBlocksBlindRetryUntilReconciled(t *testing.T) {
	stateDir, _, _, uvBin, countFile := setupUVFixture(t)
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC).UnixNano()
	clockFn := func() int64 { now += int64(time.Second); return now }

	prev, err := Preview(context.Background(), Options{
		InstanceID: "inst_test",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		NowNS:      clockFn,
	})
	if err != nil {
		t.Fatalf("Preview failed: %v", err)
	}

	// Simulate crash right after durable intent is written.
	_, err = Prune(context.Background(), Options{
		InstanceID: "inst_test",
		RunID:      "run_crash",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		Digest:     string(prev.Digest),
		NowNS:      clockFn,
		AfterIntentHook: func(opID string) error {
			return errors.New("simulated process crash before settlement")
		},
	})
	if err == nil {
		t.Fatalf("expected simulated crash error")
	}

	st, err := Status(stateDir)
	if err != nil || st.PendingOp == nil {
		t.Fatalf("expected pending operation recorded, got st=%+v err=%v", st, err)
	}
	pendingID := st.PendingOp.OpID

	// Subsequent prune MUST block with RECOVERY_REQUIRED rather than blind-retrying.
	_, err = Prune(context.Background(), Options{
		InstanceID: "inst_test",
		RunID:      "run_retry",
		StateDir:   stateDir,
		Tool:       ToolUV,
		BinaryPath: uvBin,
		Digest:     string(prev.Digest),
		NowNS:      clockFn,
	})
	if errs.CodeOf(err) != errs.CodeRecoveryRequired {
		t.Fatalf("expected RECOVERY_REQUIRED when pending op exists, got %v", err)
	}
	if countInvocations(countFile) != 0 {
		t.Fatalf("blocked retry must not spawn uv")
	}

	// Explicit reconciliation inspects post-crash state and clears the block.
	rec, err := Reconcile(context.Background(), stateDir, "inst_test", pendingID, "verified untouched after simulated crash", clockFn)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if rec.State != "reconciled" || !rec.ProtectedEnvsIntact {
		t.Fatalf("expected reconciled state with intact envs, got %+v", rec)
	}
}
