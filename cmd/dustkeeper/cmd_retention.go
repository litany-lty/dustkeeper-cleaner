package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/retention"
)

// cmdRetention computes what a host must keep for each declared application.
//
// It is a planning command: it reads one manifest, touches no other file, and
// proposes nothing it could execute. The proposal still has to pass the existing
// gates - model review, identity, protection, grants - before a byte moves, which
// is exactly the order section 4.4 asks for.
func cmdRetention(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 || args[0] != "plan" {
		return output{}, errs.New(errs.CodeInvalidArgument, "retention requires a subcommand: plan")
	}
	fs := flagset("retention plan", io.Discard)
	manifest := fs.String("manifest", "", "absolute path to the application/release manifest")
	if err := parseFlags(fs, args[1:]); err != nil {
		return output{}, err
	}
	if *manifest == "" || !filepath.IsAbs(*manifest) {
		return output{}, errs.New(errs.CodeInvalidArgument, "retention plan needs an absolute --manifest path")
	}
	info, err := os.Stat(*manifest)
	if err != nil {
		return output{}, errs.Newf(errs.CodeAccessDenied, "manifest cannot be read: %v", err)
	}
	if info.IsDir() {
		return output{}, errs.New(errs.CodeInvalidArgument, "--manifest names a directory")
	}
	if info.Size() > retention.MaxManifestBytes {
		return output{}, errs.Newf(errs.CodeInvalidArgument,
			"manifest is larger than %d bytes", retention.MaxManifestBytes)
	}
	raw, err := os.ReadFile(*manifest)
	if err != nil {
		return output{}, errs.Newf(errs.CodeAccessDenied, "manifest cannot be read: %v", err)
	}
	m, err := retention.Parse(raw)
	if err != nil {
		return output{}, errs.Wrap(errs.CodeInvalidArgument, err)
	}
	plan := retention.Build(m)

	out := ok()
	out.Data = plan
	out.Human = renderRetention(plan, *manifest)
	if !plan.GoalMet {
		// Not an error: the plan is the answer, and "one set was not achievable"
		// is a result the operator has to read, not a failure to retry.
		out.Message = "the goal of exactly one restorable set per application is not met; see the notes"
	}
	out.Next = []string{"dustkeeper rule preview", "dustkeeper plan create --scope <scope_id> --rule <rule_id>"}
	return out, nil
}

func renderRetention(plan retention.Plan, manifest string) []string {
	lines := []string{
		"retention plan from " + manifest,
		fmt.Sprintf("  %d application(s); kept files %d; candidate files %d", len(plan.Apps), plan.KeptFiles, plan.CandidateFiles),
	}
	if len(plan.SharedPaths) > 0 {
		lines = append(lines, fmt.Sprintf("  shared path(s) %d; a keep conclusion outranks any proposal to clean them", len(plan.SharedPaths)))
		for _, p := range plan.SharedPaths {
			lines = append(lines, "  shared  "+p)
		}
	}
	for _, ap := range plan.Apps {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("%s: latest qualified set %s, kept sets %d, goal met %v",
			ap.App, orUnknown(ap.LatestQualified), ap.KeptSets, ap.GoalMet))
		for _, k := range ap.Keep {
			release := ""
			if k.Release != "" {
				release = " [" + k.Release + "]"
			}
			detail := ""
			if k.Detail != "" {
				detail = " — " + k.Detail
			}
			lines = append(lines, fmt.Sprintf("  keep    %s%s %s%s", k.Path, release, k.Reason, detail))
		}
		for _, c := range ap.Candidates {
			lines = append(lines, fmt.Sprintf("  propose %s [%s] %s", c.Path, c.Release, c.Why))
		}
		for _, note := range ap.Notes {
			lines = append(lines, "  note    "+note)
		}
		if len(ap.Candidates) == 0 {
			lines = append(lines, "  propose (nothing)")
		}
	}
	return lines
}

func orUnknown(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
