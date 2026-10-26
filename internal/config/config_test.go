package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/errs"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestDefaultsValidate holds the line that matters most: the shipped defaults
// must be a legal configuration. A default outside its own bounds would make
// every fresh installation fail on first use.
func TestDefaultsValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	if c.ConfigVersion != 1 {
		t.Errorf("config_version = %d", c.ConfigVersion)
	}
	if c.Runtime.MaxAutoQuarantineFiles <= 0 || c.Runtime.MaxAutoQuarantineBytes <= 0 {
		t.Error("the default action limits must be positive; zero means forbidden")
	}
	if c.Model != nil && c.Model.Enabled {
		t.Error("the model adapter must be off by default")
	}
}

// TestRoundTrip keeps Marshal and Load in agreement: what init writes must load
// back unchanged, or every restart silently changes the budget.
func TestRoundTrip(t *testing.T) {
	c := Default()
	c.Runtime.ScanBatchSize = 250
	c.Runtime.MaxAutoPurgeFiles = 7
	c.Model.Enabled = true
	c.Model.BaseURL = "http://127.0.0.1:8080/v1"

	body, err := Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Error("Marshal must end with exactly one newline")
	}

	path := writeConfig(t, string(body))
	got, usedDefaults, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if usedDefaults {
		t.Error("a real file must not be reported as defaulted")
	}
	if got.Runtime.ScanBatchSize != 250 || got.Runtime.MaxAutoPurgeFiles != 7 {
		t.Errorf("values changed across a round trip: %+v", got.Runtime)
	}
	if got.Model == nil || got.Model.BaseURL != "http://127.0.0.1:8080/v1" {
		t.Errorf("model section changed across a round trip: %+v", got.Model)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	c, usedDefaults, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if !usedDefaults {
		t.Error("a missing file must be reported as defaulted")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the defaulted configuration does not validate: %v", err)
	}
}

// TestOutOfRangeIsRejectedNotClamped is the invariant: a value that silently
// became something else is worse than one that refuses to start.
func TestOutOfRangeIsRejectedNotClamped(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Config)
		field string
	}{
		{"scan batch below one", func(c *Config) { c.Runtime.ScanBatchSize = 0 }, "scan_batch_size"},
		{"scan batch too large", func(c *Config) { c.Runtime.ScanBatchSize = 5000 }, "scan_batch_size"},
		{"io workers above one", func(c *Config) { c.Runtime.IOWorkers = 4 }, "io_workers"},
		{"buffer too small", func(c *Config) { c.Runtime.IOBufferBytes = 1024 }, "io_buffer_bytes"},
		{"read rate too low", func(c *Config) { c.Runtime.ReadRateBytesPerSecond = 100 }, "read_rate_bytes_per_second"},
		{"soft time below five", func(c *Config) { c.Runtime.RunSoftSeconds = 1 }, "run_soft_seconds"},
		{"read budget zero", func(c *Config) { c.Runtime.RunReadBudgetBytes = 0 }, "run_read_budget_bytes"},
		{"quarantine files negative", func(c *Config) { c.Runtime.MaxAutoQuarantineFiles = -1 }, "max_auto_quarantine_files"},
		{"purge bytes negative", func(c *Config) { c.Runtime.MaxAutoPurgeBytes = -1 }, "max_auto_purge_bytes"},
		{"min free percent zero", func(c *Config) { c.Runtime.MinFreePercent = 0 }, "min_free_percent"},
		{"min free percent above fifty", func(c *Config) { c.Runtime.MinFreePercent = 90 }, "min_free_percent"},
		{"wrong config version", func(c *Config) { c.ConfigVersion = 2 }, "config_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if errs.CodeOf(err) != errs.CodeInvalidArgument {
				t.Fatalf("code = %s, want INVALID_ARGUMENT", errs.CodeOf(err))
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the message does not name the field %q: %v", tc.field, err)
			}
		})
	}
}

// TestZeroActionLimitsAreLegal is the counterpoint to the range checks: zero is
// a legal, meaningful value for the four action limits. It means "forbid this
// action", never "unlimited".
func TestZeroActionLimitsAreLegal(t *testing.T) {
	c := Default()
	c.Runtime.MaxAutoQuarantineFiles = 0
	c.Runtime.MaxAutoQuarantineBytes = 0
	c.Runtime.MaxAutoPurgeFiles = 0
	c.Runtime.MaxAutoPurgeBytes = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("zero action limits must validate: %v", err)
	}
}

func TestModelEndpointRules(t *testing.T) {
	base := func() *Model {
		return &Model{
			Enabled:               true,
			Provider:              "chat_completions_json",
			BaseURL:               "https://api.example.com/v1",
			Model:                 "some-model",
			APIKeyEnv:             "DUSTKEEPER_MODEL_API_KEY",
			RequestTimeoutSeconds: 20,
			MaxResponseBytes:      64 * kib,
		}
	}

	cases := []struct {
		name    string
		mut     func(*Model)
		wantErr bool
	}{
		{"https is fine", func(m *Model) {}, false},
		{"http to loopback is allowed for local adapters", func(m *Model) { m.BaseURL = "http://localhost:9000/v1" }, false},
		{"http to 127.0.0.1 is allowed", func(m *Model) { m.BaseURL = "http://127.0.0.1:9000/v1" }, false},
		{"http to a remote host is refused", func(m *Model) { m.BaseURL = "http://api.example.com/v1" }, true},
		{"plain http to a public IP is refused", func(m *Model) { m.BaseURL = "http://93.184.216.34/v1" }, true},
		{"a non-http scheme is refused", func(m *Model) { m.BaseURL = "file:///etc/passwd" }, true},
		{"an empty base url is refused", func(m *Model) { m.BaseURL = "" }, true},
		{"an unparseable base url is refused", func(m *Model) { m.BaseURL = "://nope" }, true},
		{"an unknown provider is refused", func(m *Model) { m.Provider = "other" }, true},
		{"a zero timeout is refused", func(m *Model) { m.RequestTimeoutSeconds = 0 }, true},
		{"an oversized response cap is refused", func(m *Model) { m.MaxResponseBytes = 8 * mib }, true},
		{"a missing key env is refused when enabled", func(m *Model) { m.APIKeyEnv = "" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			m := base()
			tc.mut(m)
			c.Model = m
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("accepted a configuration that should have been refused")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refused a legal configuration: %v", err)
			}
			if err != nil && errs.CodeOf(err) != errs.CodeInvalidArgument {
				t.Errorf("code = %s, want INVALID_ARGUMENT", errs.CodeOf(err))
			}
		})
	}
}

// TestDisabledModelIsNotValidatedForEndpoint lets an operator leave a half-filled
// model section in place while it is switched off.
func TestDisabledModelIsNotValidatedForEndpoint(t *testing.T) {
	c := Default()
	c.Model.Enabled = false
	c.Model.BaseURL = ""
	c.Model.APIKeyEnv = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("a disabled model section must not be validated for its endpoint: %v", err)
	}
}

// TestStrictParsingRejectsSurprises covers the three structural rejections that
// matter: an unknown field, a duplicate key, and a non-integer number.
func TestStrictParsingRejectsSurprises(t *testing.T) {
	cases := []struct {
		name string
		body string
		code errs.Code
	}{
		{
			name: "unknown top-level field",
			body: `{"config_version":1,"auto_purge":true}`,
			code: errs.CodeUnknownField,
		},
		{
			name: "unknown runtime field",
			body: `{"config_version":1,"runtime":{"max_auto_quarantine_files":5,"god_mode":true}}`,
			code: errs.CodeUnknownField,
		},
		{
			name: "duplicate key",
			body: `{"config_version":1,"config_version":1}`,
			code: errs.CodeDuplicateKey,
		},
		{
			name: "float where an integer is required",
			body: `{"config_version":1,"runtime":{"scan_batch_size":500.5}}`,
			code: errs.CodeInvalidArgument,
		},
		{
			name: "string where a number is required",
			body: `{"config_version":1,"runtime":{"scan_batch_size":"500"}}`,
			code: errs.CodeInvalidArgument,
		},
		{
			name: "trailing data",
			body: `{"config_version":1} {"config_version":1}`,
			code: errs.CodeInvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			_, _, err := Load(path)
			if err == nil {
				t.Fatal("the file was accepted")
			}
			if got := errs.CodeOf(err); got != tc.code {
				t.Fatalf("code = %s, want %s (%v)", got, tc.code, err)
			}
		})
	}
}

// TestPathsFollowTheEnvironment keeps the two overrides working, because tests
// and multi-instance setups both depend on them.
func TestPathsFollowTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DUSTKEEPER_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("DUSTKEEPER_CONFIG_DIR", filepath.Join(dir, "conf"))

	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if p.StateDir != filepath.Join(dir, "state") {
		t.Errorf("StateDir = %q", p.StateDir)
	}
	if p.ConfigDir != filepath.Join(dir, "conf") {
		t.Errorf("ConfigDir = %q", p.ConfigDir)
	}
	if p.DBFile != filepath.Join(dir, "state", "state.db") {
		t.Errorf("DBFile = %q", p.DBFile)
	}
	// Every derived path must live under the state or config directory; nothing
	// may fall back to the working directory.
	for _, path := range []string{p.InstanceLock, p.InstanceJSON, p.ControlLock, p.PauseRequest, p.LogDir} {
		if !strings.HasPrefix(path, filepath.Join(dir, "state")) {
			t.Errorf("%q is outside the state directory", path)
		}
	}

	// Clearing the overrides must not panic and must produce absolute paths.
	t.Setenv("DUSTKEEPER_STATE_DIR", "")
	t.Setenv("DUSTKEEPER_CONFIG_DIR", "")
	def, err := ResolvePaths()
	if err != nil {
		t.Fatalf("resolve default paths: %v", err)
	}
	if !filepath.IsAbs(def.DBFile) {
		t.Errorf("the default database path is not absolute: %q", def.DBFile)
	}
}

func TestTrimSpaceJSONRemovesExactlyOneNewline(t *testing.T) {
	if got := string(TrimSpaceJSON([]byte("{}\n"))); got != "{}" {
		t.Errorf("got %q", got)
	}
	// Interior and repeated whitespace is not this function's business.
	if got := string(TrimSpaceJSON([]byte("{}\n\n"))); got != "{}\n" {
		t.Errorf("got %q; only one trailing newline may be removed", got)
	}
	if got := string(TrimSpaceJSON([]byte("{} "))); got != "{} " {
		t.Errorf("got %q; a trailing space is not a newline", got)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !IsLoopbackHost(host) {
			t.Errorf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"example.com", "10.0.0.1", "192.168.1.1", ""} {
		if IsLoopbackHost(host) {
			t.Errorf("%q must not be treated as loopback", host)
		}
	}
}
