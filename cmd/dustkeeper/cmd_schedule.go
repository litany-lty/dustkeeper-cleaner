package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/schedule"
)

// draft.go holds the model seam, so the CLI never talks to HTTP directly.

// cmdSchedule routes schedule render.
func cmdSchedule(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 || args[0] != "render" {
		return output{}, errs.New(errs.CodeInvalidArgument, "schedule needs a subcommand: render")
	}
	fs := flagset("schedule render", io.Discard)
	platform := fs.String("platform", "", "linux or darwin")
	dir := fs.String("output", "", "directory to write the templates into")
	hour := fs.Int("hour", 3, "local hour to run")
	minute := fs.Int("minute", 30, "local minute to run")
	label := fs.String("label", "dustkeeper", "systemd unit prefix and launchd label")
	if err := parseFlags(fs, args[1:]); err != nil {
		return output{}, err
	}
	// An explicit 00:00 is a schedule, not "unset". Only the flags the caller
	// actually passed count, so the documented 03:30 default still applies when
	// nothing was asked for.
	timeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "hour" || f.Name == "minute" {
			timeSet = true
		}
	})
	if *platform == "" || *dir == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--platform and --output are required")
	}

	exe, err := os.Executable()
	if err != nil {
		return output{}, errs.Wrap(errs.CodeInvalidArgument, err)
	}
	if resolved, lerr := filepath.EvalSymlinks(exe); lerr == nil {
		exe = resolved
	}

	written, err := schedule.Write(schedule.Options{
		Platform:   schedule.Platform(*platform),
		Executable: exe,
		StateDir:   a.paths.StateDir,
		ConfigFile: a.paths.ConfigFile,
		Hour:       *hour,
		Minute:     *minute,
		TimeSet:    timeSet,
		Label:      *label,
	}, *dir)
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.Data = map[string]any{
		"platform":   *platform,
		"output_dir": *dir,
		"files":      written,
		"executable": exe,
	}
	out.Human = append(out.Human, "wrote:")
	for _, f := range written {
		out.Human = append(out.Human, "  "+f)
	}
	out.Human = append(out.Human,
		"",
		"these are templates: installing them is a separate, explicit decision",
		"this program never runs systemctl, launchctl or sudo")
	return out, nil
}

var _ = fmt.Sprintf
