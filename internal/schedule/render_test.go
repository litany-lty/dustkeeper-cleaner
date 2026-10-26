package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linuxOptions() Options {
	return Options{
		Platform:   Linux,
		Executable: "/opt/dustkeeper/bin/dustkeeper",
		StateDir:   "/home/u/.local/state/dustkeeper",
		ConfigFile: "/home/u/.config/dustkeeper/config.json",
		Hour:       3,
		Minute:     30,
		Label:      "dustkeeper",
	}
}

func TestLinuxTemplates(t *testing.T) {
	files, err := Render(linuxOptions())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("expected three files, got %d", len(files))
	}
	service := string(files[0].Content)
	if !strings.Contains(service, "ExecStart=/opt/dustkeeper/bin/dustkeeper run") {
		t.Errorf("service has no absolute ExecStart:\n%s", service)
	}
	if !strings.Contains(service, "TimeoutStartSec=infinity") {
		t.Error("the service must not impose a short hard timeout")
	}
	timer := string(files[1].Content)
	if !strings.Contains(timer, "OnCalendar=*-*-* 03:30:00") {
		t.Errorf("timer schedule is wrong:\n%s", timer)
	}
	if !strings.Contains(timer, "Persistent=true") {
		t.Error("the timer should catch up one missed run")
	}
	if !strings.Contains(timer, "RandomizedDelaySec=10m") {
		t.Error("the timer should spread start times")
	}
}

func TestSystemdQuotingOfSpaces(t *testing.T) {
	opt := linuxOptions()
	opt.Executable = "/Volumes/My Disk/dustkeeper"
	files, err := Render(opt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(files[0].Content), `ExecStart="/Volumes/My Disk/dustkeeper" run`) {
		t.Errorf("a path with a space was not quoted:\n%s", files[0].Content)
	}
}

func TestDarwinTemplateEscapesXML(t *testing.T) {
	opt := linuxOptions()
	opt.Platform = Darwin
	opt.Executable = "/Users/a&b/dustkeeper"
	opt.StateDir = "/Users/a&b/state/<x>"
	files, err := Render(opt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	plist := string(files[0].Content)
	if strings.Contains(plist, "a&b") {
		t.Error("an ampersand was written unescaped")
	}
	if !strings.Contains(plist, "a&amp;b") {
		t.Error("the ampersand was not escaped")
	}
	if !strings.Contains(plist, "&lt;x&gt;") {
		t.Error("angle brackets were not escaped")
	}
	if strings.Contains(plist, "KeepAlive") && strings.Contains(plist, "<key>KeepAlive</key>") {
		t.Error("a oneshot cleanup must not be kept alive")
	}
	if !strings.Contains(plist, "<key>StartCalendarInterval</key>") {
		t.Error("no calendar interval")
	}
	// The array form is what avoids a shell being involved.
	if !strings.Contains(plist, "<string>run</string>") {
		t.Error("ProgramArguments should be an array with one argument per string")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name string
		opt  func(*Options)
	}{
		{"relative executable", func(o *Options) { o.Executable = "dustkeeper" }},
		{"newline in path", func(o *Options) { o.Executable = "/opt/dust\nkeeper" }},
		{"bad label", func(o *Options) { o.Label = "dust keeper" }},
		{"hour out of range", func(o *Options) { o.Hour = 24 }},
		{"minute out of range", func(o *Options) { o.Minute = 60 }},
		{"unknown platform", func(o *Options) { o.Platform = "plan9" }},
	}
	for _, c := range cases {
		opt := linuxOptions()
		opt.Label = "dustkeeper"
		opt.Hour, opt.Minute = 3, 30
		c.opt(&opt)
		if _, err := Render(opt); err == nil {
			t.Errorf("%s: accepted an invalid option", c.name)
		}
	}
}

func TestWriteIsIdempotentAndRefusesToOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	first, err := Write(linuxOptions(), dir)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("wrote %d files", len(first))
	}
	// Second write with identical options succeeds.
	if _, err := Write(linuxOptions(), dir); err != nil {
		t.Fatalf("second write: %v", err)
	}

	// A hand-edited unit is not silently replaced.
	service := filepath.Join(dir, "dustkeeper.service")
	if err := os.WriteFile(service, []byte("# edited by hand\n"), 0o600); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := Write(linuxOptions(), dir); err == nil {
		t.Fatal("a hand-edited unit was overwritten")
	}
	body, _ := os.ReadFile(service)
	if string(body) != "# edited by hand\n" {
		t.Fatalf("the edited file changed: %q", body)
	}
}

func TestDefaultTimeWhenUnset(t *testing.T) {
	opt := linuxOptions()
	opt.Hour, opt.Minute = 0, 0
	files, err := Render(opt)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(files[1].Content), "03:30:00") {
		t.Error("the documented 03:30 default was not applied")
	}
}
