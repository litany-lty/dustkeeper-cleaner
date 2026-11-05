package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestA17LoadDoesNotReadUnboundedSpecialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.pipe")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = f.Write(bytes.Repeat([]byte{'x'}, 256*kib+1))
		time.Sleep(2 * time.Second)
		_ = f.Close()
	}()
	started := time.Now()
	_, _, err := Load(path)
	if err == nil {
		t.Fatal("FIFO config was accepted")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Load waited %v for EOF after the configured size boundary", elapsed)
	}
}

func TestA18EnabledModelRequiresUsableModelName(t *testing.T) {
	for _, name := range []string{"", "   ", "model\nname"} {
		cfg := Default()
		cfg.Model.Enabled = true
		cfg.Model.BaseURL = "https://example.invalid/v1/chat/completions"
		cfg.Model.APIKeyEnv = "DUSTKEEPER_TEST_KEY"
		cfg.Model.Model = name
		if err := cfg.Validate(); err == nil {
			t.Errorf("enabled model accepted unusable name %q", name)
		}
	}
}

func TestA19EnabledModelRejectsImpossibleEnvironmentVariableName(t *testing.T) {
	for _, name := range []string{"BAD=NAME", "BAD\x00NAME"} {
		cfg := Default()
		cfg.Model.Enabled = true
		cfg.Model.BaseURL = "https://example.invalid/v1/chat/completions"
		cfg.Model.APIKeyEnv = name
		if err := cfg.Validate(); err == nil {
			t.Errorf("impossible environment variable name accepted: %q", name)
		}
	}
}

func TestA20ResolvePathsRejectsRelativeOrNoncanonicalOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		value string
	}{
		{"relative config", "DUSTKEEPER_CONFIG_DIR", "relative/config"},
		{"relative state", "DUSTKEEPER_STATE_DIR", "relative/state"},
		{"noncanonical config", "DUSTKEEPER_CONFIG_DIR", "/private/tmp/../tmp/dustkeeper-config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DUSTKEEPER_CONFIG_DIR", t.TempDir())
			t.Setenv("DUSTKEEPER_STATE_DIR", t.TempDir())
			t.Setenv(tc.env, tc.value)
			if _, err := ResolvePaths(); err == nil {
				t.Fatalf("unsafe path override accepted: %s=%q", tc.env, tc.value)
			}
		})
	}
}

func TestA09ConfigEndpointMatchesProviderBoundary(t *testing.T) {
	for _, raw := range []string{
		"https:///v1/chat/completions",
		"https://user:secret@example.com/v1/chat/completions",
		"https://example.com/v1/chat/completions?token=secret",
		"https://example.com/v1/chat/completions#fragment",
		"https://example.com:70000/v1/chat/completions",
	} {
		cfg := Default()
		cfg.Model.Enabled = true
		cfg.Model.BaseURL = raw
		if err := cfg.Validate(); err == nil {
			t.Errorf("config accepted provider endpoint rejected by adapter: %q", raw)
		}
	}
}
