package main

import (
	"dustkeeper/internal/domain"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/review"
)

func TestCommandEnvDoesNotPassAmbientSecrets(t *testing.T) {
	t.Setenv("DUSTKEEPER_AUDIT_SECRET", "must-not-reach-artifact")
	script := filepath.Join(t.TempDir(), "print-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$DUSTKEEPER_AUDIT_SECRET\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (commandEnv{
		artifact: script,
		state:    filepath.Join(root, "state"),
		config:   filepath.Join(root, "config"),
		home:     home,
	}).run("init")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(result.stdout, []byte("must-not-reach-artifact")) {
		t.Fatal("ambient secret reached the artifact")
	}
}

func TestCommandEnvBoundsArtifactOutput(t *testing.T) {
	script := filepath.Join(t.TempDir(), "large-output.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /bin/dd if=/dev/zero bs=1048576 count=2 2>/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (commandEnv{
		artifact: script,
		state:    filepath.Join(root, "state"),
		config:   filepath.Join(root, "config"),
		home:     home,
	}).run("init")
	if err == nil {
		t.Fatal("artifact output beyond 1 MiB was accepted")
	}
	if len(result.stdout) > 1<<20 {
		t.Fatalf("captured %d bytes, want at most 1 MiB", len(result.stdout))
	}
}

func TestCommandEnvTimesOutHungArtifact(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /bin/sleep 12\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := (commandEnv{
		artifact: script,
		state:    filepath.Join(root, "state"),
		config:   filepath.Join(root, "config"),
		home:     home,
	}).run("init")
	if err == nil {
		t.Fatal("hung artifact completed without a timeout error")
	}
	if elapsed := time.Since(started); elapsed >= 11*time.Second {
		t.Fatalf("hung artifact was not stopped promptly: %s", elapsed)
	}
}

func TestCommandEnvRunsArtifactInsideIsolatedHome(t *testing.T) {
	script := filepath.Join(t.TempDir(), "print-working-directory.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/pwd\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (commandEnv{
		artifact: script,
		state:    filepath.Join(root, "state"),
		config:   filepath.Join(root, "config"),
		home:     home,
	}).run("init")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(result.stdout)); got != realHome {
		t.Fatalf("artifact cwd=%q, want isolated home %q", got, realHome)
	}
}

func TestRunSmokeRefusesNonEmptyWorkWithoutChangingIt(t *testing.T) {
	realTemp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(realTemp, "existing")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(work, "marker.txt")
	const body = "belongs to the caller\n"
	if err := os.WriteFile(marker, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if err := runSmoke(filepath.Join(realTemp, "must-not-run"), work); err == nil {
		t.Fatal("non-empty work directory was accepted")
	}
	after, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryNames(before), entryNames(after)) {
		t.Fatalf("work contents changed: before=%v after=%v", entryNames(before), entryNames(after))
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != body {
		t.Fatalf("marker changed: body=%q err=%v", got, err)
	}
}

func TestRunSmokeRefusesSymlinkWork(t *testing.T) {
	realTemp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realTemp, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realTemp, "work-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := runSmoke(filepath.Join(realTemp, "must-not-run"), link); err == nil {
		t.Fatal("symlink work directory was accepted")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v", entryNames(entries))
	}
}

func TestPrepareWorkRefusesSharedWritableDirectory(t *testing.T) {
	work := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWork(work); err == nil {
		t.Fatal("shared-writable work directory was accepted")
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}

func TestFakeTLSModelsRejectThenAllow(t *testing.T) {
	models := &fakeModels{}
	server, caPEM, err := newTLSServer(models)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	realTemp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(realTemp, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	judge, err := review.NewHTTPJudge(
		review.Endpoint{TrustCAFile: caFile, URL: server.URL + "/jev", Model: jevModel, APIKey: jevKey},
		review.Endpoint{TrustCAFile: caFile, URL: server.URL + "/minimax", Model: miniMaxModel, APIKey: miniMaxKey},
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate := review.Facts{Category: "temporary_name_only", Action: "quarantine", ScopeID: "smoke", RelativePath: "old.zip", ContentSHA256: "sha256:test", SizeBytes: 14}
	models.allow.Store(false)
	decision, err := judge.Review(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Verdict == domain.VerdictAllow || models.miniMaxCalls.Load() != 0 {
		t.Fatalf("reject decision=%+v minimax_calls=%d", decision, models.miniMaxCalls.Load())
	}
	models.allow.Store(true)
	decision, err = judge.Review(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Verdict != domain.VerdictAllow || models.miniMaxCalls.Load() != 1 {
		t.Fatalf("allow decision=%+v minimax_calls=%d", decision, models.miniMaxCalls.Load())
	}
}

func TestFindStringReadsNestedEnvelope(t *testing.T) {
	got, ok := findString([]byte(`{"status":"ok","data":{"plan_id":"pln_smoke"}}`), "plan_id")
	if !ok || got != "pln_smoke" {
		t.Fatalf("got %q, %v", got, ok)
	}
}
