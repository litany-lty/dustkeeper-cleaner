package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"dustkeeper/internal/debugrun"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/fsops"
)

type debugReferences []string

func (r *debugReferences) String() string { return fmt.Sprint([]string(*r)) }
func (r *debugReferences) Set(value string) error {
	if _, err := fsops.SplitRelative([]byte(value)); err != nil {
		return err
	}
	*r = append(*r, value)
	return nil
}

// cmdDebug deliberately never opens a store, registrar, pause gate, or action
// executor. --execute authorizes model requests only, never filesystem actions.
func cmdDebug(ctx context.Context, a *app, args []string) (output, error) {
	flags := flagset("debug", io.Discard)
	root := flags.String("root", "", "explicit directory to inspect without modifying it")
	scopeID := flags.String("scope-id", "debug", "scope id used by the separate evidence file")
	action := flags.String("action", "purge", "action to simulate: quarantine or purge")
	executeModels := flags.Bool("execute", false, "make bounded real model requests; files and state remain unchanged")
	maxCandidates := flags.Int("max-candidates", 3, "maximum candidates to review, 1..6; at most two model calls each")
	maxEntries := flags.Int("max-entries", 500, "maximum directory entries; incomplete scans do not call models")
	maxRead := flags.Int64("max-read-bytes", 64<<20, "total source plus backup read budget")
	days := flags.Int("days", 30, "recent activity window; age alone is not disposal evidence")
	openFiles := flags.Bool("open-files", false, "check files open by visible processes")
	advisory := flags.Bool("advisory", false, "request opinions without execution evidence; never simulate cleanup")
	modelTimeout := flags.Int("minimax-timeout-seconds", 20, "bounded MiniMax wait for debug only, 1..60 seconds")
	var references debugReferences
	var includes debugReferences
	var confirmed debugReferences
	flags.Var(&references, "reference", "known referenced relative path, repeatable")
	flags.Var(&includes, "include", "review only this exact relative path, repeatable")
	flags.Var(&confirmed, "owner-confirmed-unused", "owner confirmed this exact copy is no longer needed; advisory only")
	if err := parseFlags(flags, args); err != nil {
		return output{}, err
	}
	if *root == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "debug requires --root")
	}
	if *modelTimeout < 1 || *modelTimeout > 60 {
		return output{}, errs.New(errs.CodeInvalidArgument, "--minimax-timeout-seconds must be 1..60")
	}
	if len(confirmed) > 0 && !*advisory {
		return output{}, errs.New(errs.CodeInvalidArgument, "--owner-confirmed-unused requires --advisory and grants no execution permission")
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return output{}, err
	}
	opt := debugrun.Options{Root: abs, ScopeID: *scopeID, Action: *action, Execute: *executeModels, MaxEntries: *maxEntries, MaxCandidates: *maxCandidates, MaxReadBytes: *maxRead, Days: *days, Now: a.clock.Wall(), OpenFiles: *openFiles, References: map[string]bool{}}
	for _, ref := range references {
		opt.References[ref] = true
	}
	opt.Advisory = *advisory
	opt.MiniMaxTimeout = time.Duration(*modelTimeout) * time.Second
	opt.IncludePaths = map[string]bool{}
	opt.OwnerConfirmedUnused = map[string]bool{}
	for _, path := range includes {
		opt.IncludePaths[path] = true
	}
	for _, path := range confirmed {
		opt.OwnerConfirmedUnused[path] = true
	}
	if *executeModels {
		if a.cfg.Review == nil {
			return output{}, errs.New(errs.CodeReviewRequired, "configure dual review before debug --execute")
		}
		settings := *a.cfg.Review
		opt.Service, err = settings.NewService()
		if err != nil {
			return output{}, err
		}
		opt.Service.Now = a.clock.Wall
	}
	report, runErr := debugrun.Run(ctx, opt)
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		runErr = errs.New(errs.CodeCancelled, "debug cancelled; no files moved or removed")
	}
	out := ok()
	if runErr != nil {
		out = fail(runErr)
	}
	out.Data = report
	out.Human = []string{fmt.Sprintf("debug: %d simulated cleanup(s), %d model request(s)", report.SimulatedCleanedFiles, report.ModelCalls), "actual deletion: 0 files / 0 bytes; production state unchanged"}
	for _, entry := range report.Entries {
		out.Human = append(out.Human, fmt.Sprintf("  %q: %s (%s)", entry.File.Facts.Path, entry.Outcome, entry.Reason))
	}
	out.Next = []string{"review this debug report before changing production rules or permissions"}
	return out, runErr
}
