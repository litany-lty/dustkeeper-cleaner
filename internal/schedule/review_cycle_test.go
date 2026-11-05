package schedule

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCycleSystemdKeepsLiteralPaths(t *testing.T) {
	opt := linuxOptions()
	opt.Executable = "/opt/100%H/dustkeeper"
	opt.StateDir = "/home/u/50%n state"
	files, err := Render(opt)
	if err != nil {
		t.Fatal(err)
	}
	service := string(files[0].Content)
	if !strings.Contains(service, "ExecStart=/opt/100%%H/dustkeeper run") || !strings.Contains(service, `Environment="DUSTKEEPER_STATE_DIR=/home/u/50%%n state"`) {
		t.Fatalf("systemd can expand or split literal paths:\n%s", service)
	}
}

func TestReviewCycleScheduleRejectsDirectiveInjection(t *testing.T) {
	for _, mutate := range []func(*Options){
		func(o *Options) { o.StateDir = "/tmp/state\nExecStart=/bin/false" },
		func(o *Options) { o.ConfigFile = "/tmp/config\r\nEnvironment=OTHER=x/config.json" },
	} {
		opt := linuxOptions()
		mutate(&opt)
		if _, err := Render(opt); err == nil {
			t.Fatal("newline-containing configuration path was accepted")
		}
	}
}

func TestReviewCycleLinuxUnitLabelMustBeLoadable(t *testing.T) {
	for _, label := range []string{"x;true", "$(id)", "汉字", strings.Repeat("a", 248)} {
		opt := linuxOptions()
		opt.Label = label
		if _, err := Render(opt); err == nil {
			t.Fatalf("invalid Linux unit prefix accepted: %q", label)
		}
	}
}

func TestReviewCycleLaunchdInstallNotesQuoteLabel(t *testing.T) {
	opt := linuxOptions()
	opt.Platform = Darwin
	opt.Label = "x$(>PWN)"
	files, err := Render(opt)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, f := range files {
		if f.Name == "README.txt" {
			for _, s := range strings.Split(string(f.Content), "\n") {
				if strings.HasPrefix(strings.TrimSpace(s), "cp ") {
					line = strings.TrimSpace(s)
				}
			}
		}
	}
	if line == "" {
		t.Fatal("no install copy command")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cp"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", line)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + bin, "HOME=" + dir}
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install copy: %v %s", err, data)
	}
	if _, err := os.Stat(filepath.Join(dir, "PWN")); !os.IsNotExist(err) {
		t.Fatal("unit label executed shell substitution in installation notes")
	}
}

func TestReviewCycleScheduleWriteDoesNotFollowDanglingLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.service")
	if err := os.Symlink(target, filepath.Join(dir, "dustkeeper.service")); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(linuxOptions(), dir); err == nil {
		t.Fatal("schedule writer followed a dangling output link")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("schedule writer created a file outside its output directory")
	}
}
