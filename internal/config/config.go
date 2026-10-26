// Package config reads the strict runtime configuration from section 4.3.
//
// The configuration file only describes resources and the model endpoint.
// Execution authority lives in the database: editing this file can never grant
// permission to purge anything.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/strictjson"
)

// Byte and time bounds.
const (
	kib = 1 << 10
	mib = 1 << 20
	gib = 1 << 30
	tib = 1 << 40
)

// DefaultScanBatch is the metadata upsert batch size.
const DefaultScanBatch = 500

// Runtime holds resource budgets. Zero has meaning ("forbid this action") for
// the four action limits, so those are never defaulted when missing.
type Runtime struct {
	ScanBatchSize          int   `json:"scan_batch_size"`
	IOWorkers              int   `json:"io_workers"`
	IOBufferBytes          int   `json:"io_buffer_bytes"`
	ReadRateBytesPerSecond int64 `json:"read_rate_bytes_per_second"`
	RunSoftSeconds         int   `json:"run_soft_seconds"`
	RunReadBudgetBytes     int64 `json:"run_read_budget_bytes"`
	MaxAutoQuarantineFiles int   `json:"max_auto_quarantine_files"`
	MaxAutoQuarantineBytes int64 `json:"max_auto_quarantine_bytes"`
	MaxAutoPurgeFiles      int   `json:"max_auto_purge_files"`
	MaxAutoPurgeBytes      int64 `json:"max_auto_purge_bytes"`
	MinFreeBytes           int64 `json:"min_free_bytes"`
	MinFreePercent         int   `json:"min_free_percent"`
}

// ProviderChatCompletionsJSON is the only model wire format phase one speaks:
// one OpenAI-shaped POST per draft, no streaming, no tools.
//
// It lives here, not in the adapter, because the loader validates the name and
// the adapter consumes it. Two literals drifted apart once already, which made
// every configuration the loader approved unusable.
const ProviderChatCompletionsJSON = "chat_completions_json"

// Model configures the single rule-draft HTTP adapter.
type Model struct {
	Enabled               bool   `json:"enabled"`
	Provider              string `json:"provider"`
	BaseURL               string `json:"base_url"`
	Model                 string `json:"model"`
	APIKeyEnv             string `json:"api_key_env"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	MaxResponseBytes      int    `json:"max_response_bytes"`
}

// Config is the whole file.
type Config struct {
	ConfigVersion int     `json:"config_version"`
	Runtime       Runtime `json:"runtime"`
	Model         *Model  `json:"model"`
}

// Default returns the documented defaults.
func Default() Config {
	return Config{
		ConfigVersion: 1,
		Runtime: Runtime{
			ScanBatchSize:          DefaultScanBatch,
			IOWorkers:              1,
			IOBufferBytes:          mib,
			ReadRateBytesPerSecond: 20 * mib,
			RunSoftSeconds:         60,
			RunReadBudgetBytes:     gib,
			MaxAutoQuarantineFiles: 100,
			MaxAutoQuarantineBytes: gib,
			MaxAutoPurgeFiles:      100,
			MaxAutoPurgeBytes:      gib,
			MinFreeBytes:           512 * mib,
			MinFreePercent:         5,
		},
		Model: &Model{
			Enabled:               false,
			Provider:              ProviderChatCompletionsJSON,
			BaseURL:               "https://api.example.invalid/v1",
			Model:                 "explicitly-configured-model",
			APIKeyEnv:             "DUSTKEEPER_MODEL_API_KEY",
			RequestTimeoutSeconds: 20,
			MaxResponseBytes:      64 * kib,
		},
	}
}

// Load reads path, or returns defaults when the file does not exist. A missing
// file is not an error: every command works on defaults until `init` writes one.
func Load(path string) (Config, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), true, nil
	}
	if err != nil {
		return Config{}, false, errs.Wrap(errs.CodeAccessDenied, err)
	}

	var c Config
	if err := strictjson.Unmarshal(data, &c, strictjson.Options{
		MaxBytes:     256 * kib,
		MaxDepth:     8,
		IntegerOnly:  true,
		StrictStruct: true,
	}); err != nil {
		return Config{}, false, fromStrict(err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, false, err
	}
	return c, false, nil
}

// fromStrict maps a structural rejection onto a CLI code.
func fromStrict(err error) error {
	var se *strictjson.Error
	if ok := asStrict(err, &se); ok {
		switch se.Kind {
		case strictjson.KindUnknown:
			return errs.Newf(errs.CodeUnknownField, "config: %s", se.Detail)
		case strictjson.KindDuplicate:
			return errs.Newf(errs.CodeDuplicateKey, "config: %s", se.Detail)
		case strictjson.KindNotInteger:
			return errs.Newf(errs.CodeInvalidArgument, "config: %s", se.Detail)
		}
	}
	return errs.Newf(errs.CodeInvalidArgument, "config: %v", err)
}

func asStrict(err error, target **strictjson.Error) bool {
	se, ok := err.(*strictjson.Error)
	if ok {
		*target = se
	}
	return ok
}

// Validate enforces every documented bound. Out-of-range values are rejected,
// never clamped: a configuration that silently became something else is worse
// than one that refuses to start.
func (c Config) Validate() error {
	if c.ConfigVersion != 1 {
		return errs.Newf(errs.CodeInvalidArgument, "config_version must be 1, got %d", c.ConfigVersion)
	}
	r := c.Runtime
	checks := []struct {
		name string
		ok   bool
		want string
		got  string
	}{
		{"scan_batch_size", r.ScanBatchSize >= 1 && r.ScanBatchSize <= 2000, "1..2000", itoa(r.ScanBatchSize)},
		{"io_workers", r.IOWorkers == 1, "1", itoa(r.IOWorkers)},
		{"io_buffer_bytes", r.IOBufferBytes >= 64*kib && r.IOBufferBytes <= 4*mib, "65536..4194304", itoa(r.IOBufferBytes)},
		{"read_rate_bytes_per_second", r.ReadRateBytesPerSecond >= mib && r.ReadRateBytesPerSecond <= gib, "1048576..1073741824", itoa64(r.ReadRateBytesPerSecond)},
		{"run_soft_seconds", r.RunSoftSeconds >= 5 && r.RunSoftSeconds <= 600, "5..600", itoa(r.RunSoftSeconds)},
		{"run_read_budget_bytes", r.RunReadBudgetBytes >= mib && r.RunReadBudgetBytes <= tib, "1048576..1099511627776", itoa64(r.RunReadBudgetBytes)},
		{"max_auto_quarantine_files", r.MaxAutoQuarantineFiles >= 0 && r.MaxAutoQuarantineFiles <= 10000, "0..10000", itoa(r.MaxAutoQuarantineFiles)},
		{"max_auto_quarantine_bytes", r.MaxAutoQuarantineBytes >= 0 && r.MaxAutoQuarantineBytes <= tib, "0..1099511627776", itoa64(r.MaxAutoQuarantineBytes)},
		{"max_auto_purge_files", r.MaxAutoPurgeFiles >= 0 && r.MaxAutoPurgeFiles <= 10000, "0..10000", itoa(r.MaxAutoPurgeFiles)},
		{"max_auto_purge_bytes", r.MaxAutoPurgeBytes >= 0 && r.MaxAutoPurgeBytes <= tib, "0..1099511627776", itoa64(r.MaxAutoPurgeBytes)},
		{"min_free_bytes", r.MinFreeBytes >= 0 && r.MinFreeBytes <= tib, "0..1099511627776", itoa64(r.MinFreeBytes)},
		{"min_free_percent", r.MinFreePercent >= 1 && r.MinFreePercent <= 50, "1..50", itoa(r.MinFreePercent)},
	}
	for _, ck := range checks {
		if !ck.ok {
			return errs.Newf(errs.CodeInvalidArgument, "runtime.%s must be in %s, got %s", ck.name, ck.want, ck.got)
		}
	}

	if c.Model != nil {
		return c.Model.validate()
	}
	return nil
}

func (m *Model) validate() error {
	if m.Provider != ProviderChatCompletionsJSON {
		return errs.Newf(errs.CodeInvalidArgument, "model.provider must be %s, got %q", ProviderChatCompletionsJSON, m.Provider)
	}
	if m.RequestTimeoutSeconds < 1 || m.RequestTimeoutSeconds > 120 {
		return errs.Newf(errs.CodeInvalidArgument, "model.request_timeout_seconds must be in 1..120, got %d", m.RequestTimeoutSeconds)
	}
	if m.MaxResponseBytes < 1024 || m.MaxResponseBytes > 4*mib {
		return errs.Newf(errs.CodeInvalidArgument, "model.max_response_bytes must be in 1024..4194304, got %d", m.MaxResponseBytes)
	}
	if !m.Enabled {
		return nil
	}
	if m.BaseURL == "" {
		return errs.New(errs.CodeInvalidArgument, "model.base_url is required when the model is enabled")
	}
	if m.APIKeyEnv == "" {
		return errs.New(errs.CodeInvalidArgument, "model.api_key_env is required when the model is enabled")
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil {
		return errs.Newf(errs.CodeInvalidArgument, "model.base_url is not a URL: %v", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return errs.New(errs.CodeInvalidArgument, "model.base_url may use http only for a loopback host")
		}
	default:
		return errs.Newf(errs.CodeInvalidArgument, "model.base_url scheme must be https, got %q", u.Scheme)
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Marshal renders the configuration as indented JSON with a trailing newline.
func Marshal(c Config) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func itoa(v int) string     { return fmt.Sprintf("%d", v) }
func itoa64(v int64) string { return fmt.Sprintf("%d", v) }

// Paths are the per-user locations from section 4.1.
type Paths struct {
	ConfigDir    string
	ConfigFile   string
	StateDir     string
	DBFile       string
	InstanceLock string
	InstanceJSON string
	ControlLock  string
	PauseRequest string
	LogDir       string
}

// ResolvePaths computes the platform default locations. DUSTKEEPER_STATE_DIR
// and DUSTKEEPER_CONFIG_DIR override them for tests and for operators who run
// several instances; scope ownership markers still prevent two state
// directories from managing the same tree.
func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, errs.Wrap(errs.CodeAccessDenied, err)
	}

	var configDir, stateDir string
	if v := os.Getenv("DUSTKEEPER_CONFIG_DIR"); v != "" {
		configDir = v
	} else if runtime.GOOS == "darwin" {
		configDir = filepath.Join(home, "Library", "Application Support", "Dustkeeper")
	} else if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		configDir = filepath.Join(x, "dustkeeper")
	} else {
		configDir = filepath.Join(home, ".config", "dustkeeper")
	}

	if v := os.Getenv("DUSTKEEPER_STATE_DIR"); v != "" {
		stateDir = v
	} else if runtime.GOOS == "darwin" {
		stateDir = filepath.Join(home, "Library", "Application Support", "Dustkeeper")
	} else if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		stateDir = filepath.Join(x, "dustkeeper")
	} else {
		stateDir = filepath.Join(home, ".local", "state", "dustkeeper")
	}

	return Paths{
		ConfigDir:    configDir,
		ConfigFile:   filepath.Join(configDir, "config.json"),
		StateDir:     stateDir,
		DBFile:       filepath.Join(stateDir, "state.db"),
		InstanceLock: filepath.Join(stateDir, "instance.lock"),
		InstanceJSON: filepath.Join(stateDir, "instance.json"),
		ControlLock:  filepath.Join(stateDir, "control.lock"),
		PauseRequest: filepath.Join(stateDir, "pause.request"),
		LogDir:       filepath.Join(stateDir, "logs"),
	}, nil
}

// RegistrationLockPath is the fixed user-level lock used when registering a
// mutable scope. It is derived from the home directory so that changing the
// state directory cannot produce two unrelated locks over one tree.
func RegistrationLockPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errs.Wrap(errs.CodeAccessDenied, err)
	}
	return filepath.Join(home, ".dustkeeper-registry.lock"), nil
}

// IsLoopbackHost is exported for the model provider tests.
func IsLoopbackHost(host string) bool { return isLoopback(host) }

// TrimSpaceJSON removes exactly one trailing newline, never arbitrary space.
func TrimSpaceJSON(b []byte) []byte {
	return []byte(strings.TrimSuffix(string(b), "\n"))
}
