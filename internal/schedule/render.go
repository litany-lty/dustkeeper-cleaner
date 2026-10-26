// Package schedule renders the operating-system timer templates.
//
// Rendering is not installing. The files are written only where the user asked,
// and the program never invokes systemctl, launchctl or sudo: installing a timer
// is an explicit operator decision that needs its own review.
package schedule

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dustkeeper/internal/errs"
)

// Platform selects the template family.
type Platform string

// Supported platforms.
const (
	Linux  Platform = "linux"
	Darwin Platform = "darwin"
)

// Options describes one render request.
type Options struct {
	Platform   Platform
	Executable string // absolute path to this binary
	StateDir   string
	ConfigFile string
	Hour       int // local time, 0-23
	Minute     int // 0-59
	// Label is the launchd label and systemd unit prefix.
	Label string
}

// File is one rendered template.
type File struct {
	Name    string
	Content []byte
	Mode    os.FileMode
}

// Render produces the templates for one platform.
func Render(opt Options) ([]File, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	if opt.Hour == 0 && opt.Minute == 0 {
		// The documented default is 03:30 local time.
		opt.Hour, opt.Minute = 3, 30
	}
	switch opt.Platform {
	case Linux:
		return renderLinux(opt)
	case Darwin:
		return renderDarwin(opt)
	}
	return nil, errs.Newf(errs.CodeInvalidArgument, "unsupported platform %q", opt.Platform)
}

// Write renders and writes the templates into dir, creating it if needed. It
// never overwrites a file that already differs: a hand-edited unit is the
// operator's, not this program's.
func Write(opt Options, dir string) ([]string, error) {
	files, err := Render(opt)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errs.Wrap(errs.CodeAccessDenied, err)
	}
	var written []string
	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if existing, err := os.ReadFile(path); err == nil {
			if bytes.Equal(existing, f.Content) {
				written = append(written, path)
				continue
			}
			return written, errs.Newf(errs.CodeInvalidArgument,
				"%s already exists with different content; remove it first", path)
		} else if !os.IsNotExist(err) {
			return written, errs.Wrap(errs.CodeAccessDenied, err)
		}
		if err := os.WriteFile(path, f.Content, f.Mode); err != nil {
			return written, errs.Wrap(errs.CodeAccessDenied, err)
		}
		written = append(written, path)
	}
	return written, nil
}

func (o Options) validate() error {
	if !filepath.IsAbs(o.Executable) {
		return errs.Newf(errs.CodeInvalidArgument,
			"the executable path must be absolute, got %q", o.Executable)
	}
	if strings.ContainsAny(o.Executable, "\r\n") {
		return errs.New(errs.CodeInvalidArgument, "the executable path contains a newline")
	}
	if o.Label == "" {
		return errs.New(errs.CodeInvalidArgument, "a unit label is required")
	}
	if strings.ContainsAny(o.Label, " \t\r\n/\\") {
		return errs.Newf(errs.CodeInvalidArgument, "label %q is not a valid unit name", o.Label)
	}
	if o.Hour < 0 || o.Hour > 23 || o.Minute < 0 || o.Minute > 59 {
		return errs.Newf(errs.CodeInvalidArgument,
			"time %02d:%02d is out of range", o.Hour, o.Minute)
	}
	return nil
}

// systemdQuote renders one ExecStart argument. systemd splits on whitespace
// before it expands anything, so a path with a space must be quoted and its own
// quotes and backslashes escaped.
func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func renderLinux(opt Options) ([]File, error) {
	service := fmt.Sprintf(`[Unit]
Description=Dustkeeper bounded cleanup

[Service]
Type=oneshot
ExecStart=%s run
Environment=DUSTKEEPER_STATE_DIR=%s
Environment=DUSTKEEPER_CONFIG_DIR=%s
Nice=10
IOSchedulingClass=idle
UMask=0077
NoNewPrivileges=true
# The program bounds itself with run_soft_seconds. Do not add a short hard
# timeout: killing it in the middle of a durable write only creates work for
# reconciliation.
TimeoutStartSec=infinity
`, systemdQuote(opt.Executable),
		systemdQuote(opt.StateDir),
		systemdQuote(filepath.Dir(opt.ConfigFile)))

	timer := fmt.Sprintf(`[Unit]
Description=Run Dustkeeper daily

[Timer]
# Persistent=true runs one catch-up pass after downtime. It does not replay
# every missed day.
OnCalendar=*-*-* %02d:%02d:00
Persistent=true
RandomizedDelaySec=10m
Unit=%s.service

[Install]
WantedBy=timers.target
`, opt.Hour, opt.Minute, opt.Label)

	notes := fmt.Sprintf(`Dustkeeper timer templates (systemd, user scope)
=================================================

Files
  %[1]s.service    the bounded run
  %[1]s.timer      the daily schedule

Install as the user that owns the state directory:

  mkdir -p ~/.config/systemd/user
  cp %[1]s.service %[1]s.timer ~/.config/systemd/user/
  systemctl --user daemon-reload
  systemctl --user enable --now %[1]s.timer
  systemctl --user list-timers %[1]s.timer

A user unit stops when the user's last session ends. On a server without a
session, decide deliberately whether to enable lingering:

  loginctl enable-linger <user>

That is an operator decision; this program never runs it. A system-wide unit
that runs as a dedicated unprivileged account is also acceptable — install it
explicitly and keep it off root.

Do not add a short TimeoutStartSec and do not run the cleanup core as root.
`, opt.Label)

	return []File{
		{Name: opt.Label + ".service", Content: []byte(service), Mode: 0o600},
		{Name: opt.Label + ".timer", Content: []byte(timer), Mode: 0o600},
		{Name: "README.txt", Content: []byte(notes), Mode: 0o600},
	}, nil
}

func renderDarwin(opt Options) ([]File, error) {
	var esc strings.Builder
	write := func(s string) {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		esc.Write(b.Bytes())
	}

	write(opt.Label)
	label := esc.String()
	esc.Reset()
	write(opt.Executable)
	exe := esc.String()
	esc.Reset()
	write(opt.StateDir)
	state := esc.String()
	esc.Reset()
	write(filepath.Dir(opt.ConfigFile))
	cfg := esc.String()

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>

  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>

  <key>EnvironmentVariables</key>
  <dict>
    <key>DUSTKEEPER_STATE_DIR</key>
    <string>%s</string>
    <key>DUSTKEEPER_CONFIG_DIR</key>
    <string>%s</string>
  </dict>

  <!-- No KeepAlive: a cleanup pass is a oneshot, not a daemon. -->
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key>
    <integer>%d</integer>
    <key>Minute</key>
    <integer>%d</integer>
  </dict>

  <key>ProcessType</key>
  <string>Background</string>
  <key>LowPriorityIO</key>
  <true/>
  <key>Nice</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>%s/run.log</string>
  <key>StandardErrorPath</key>
  <string>%s/run.log</string>
</dict>
</plist>
`, label, exe, state, cfg, opt.Hour, opt.Minute, state, state)

	notes := fmt.Sprintf(`Dustkeeper timer templates (launchd, LaunchAgent)
===================================================

File
  %[1]s.plist

Install as the user that owns the state directory:

  cp %[1]s.plist ~/Library/LaunchAgents/
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/%[1]s.plist
  launchctl print gui/$(id -u)/%[1]s

Paths are written as real absolute paths. launchd does not expand "~", and it
does not run a shell, so ProgramArguments is an array and not a command line.

The LaunchAgent runs with the same user but not necessarily the same privacy
permissions as your terminal. A directory that works interactively may fail
with ACCESS_DENIED in the background. Run "dustkeeper doctor --scope <id>"
again after granting access; this program does not request full disk access for
the whole process.
`, opt.Label)

	return []File{
		{Name: opt.Label + ".plist", Content: []byte(plist), Mode: 0o600},
		{Name: "README.txt", Content: []byte(notes), Mode: 0o600},
	}, nil
}
