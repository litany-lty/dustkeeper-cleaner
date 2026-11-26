package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"dustkeeper/internal/cachetool"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/scope"
)

// cmdCache routes bounded official package-cache inspection and reclamation.
func cmdCache(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "cache needs a subcommand: preview, prune, status, reconcile")
	}
	switch args[0] {
	case "preview":
		return cmdCachePreview(ctx, a, args[1:])
	case "prune":
		return cmdCachePrune(ctx, a, args[1:])
	case "status":
		return cmdCacheStatus(ctx, a, args[1:])
	case "reconcile":
		return cmdCacheReconcile(ctx, a, args[1:])
	default:
		return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown cache subcommand %q; use preview, prune, status or reconcile", args[0])
	}
}

func cmdCachePreview(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("cache preview", io.Discard)
	tool := fs.String("tool", cachetool.ToolUV, "package cache tool (uv)")
	bin := fs.String("bin", "", "trusted binary path (default: resolved on PATH)")
	cacheDir := fs.String("cache-dir", "", "explicit cache root (default: queried from the tool)")
	toolDir := fs.String("tool-dir", "", "explicit installed-tool root (default: queried from the tool)")
	maxBytes := fs.Int64("max-bytes", 0, "optional upper-bound impact cap in bytes (0 means whole cache)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	// Bind to instance_id when initialised; preview still works before init with an empty instance tag
	// only if no DB exists, though `prune` requires an initialised instance.
	_ = a.openStore()

	prev, err := cachetool.Preview(ctx, cachetool.Options{
		InstanceID: a.instance,
		StateDir:   a.paths.StateDir,
		Tool:       *tool,
		BinaryPath: *bin,
		CacheDir:   *cacheDir,
		ToolDir:    *toolDir,
		MaxBytes:   *maxBytes,
		NowNS:      func() int64 { return a.clock.Wall().UnixNano() },
	})
	if err != nil {
		return output{}, err
	}

	confirmCmd := buildCachePruneCommand(*tool, string(prev.Digest), *bin, *cacheDir, *toolDir, *maxBytes)
	res := ok()
	res.Data = map[string]any{
		"preview":      prev,
		"digest":       prev.Digest,
		"confirm_hint": confirmCmd,
	}
	res.Human = []string{
		fmt.Sprintf("cache preview (%s): read-only, nothing changed", prev.ToolVersion),
		fmt.Sprintf("  binary:         %s", prev.BinaryPath),
		fmt.Sprintf("  cache root:     %s (%d files)", prev.CacheRoot, prev.Files),
		fmt.Sprintf("  upper bound:    %s allocated (%s logical); minimum reclaim guaranteed: unknown",
			humanBytesOf(prev.UpperBoundAllocatedBytes), humanBytesOf(prev.LogicalBytes)),
		fmt.Sprintf("  volume avail:   %s", humanBytesOf(prev.VolumeAvailBytes)),
		fmt.Sprintf("  protected envs: %d verified entry point(s)", len(prev.ProtectedEnvs)),
		"",
		"to execute this exact cache reclamation once, run:",
		"  " + confirmCmd,
	}
	res.Next = []string{confirmCmd}
	return res, nil
}

func cmdCachePrune(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("cache prune", io.Discard)
	tool := fs.String("tool", cachetool.ToolUV, "package cache tool (uv)")
	bin := fs.String("bin", "", "trusted binary path (default: resolved on PATH)")
	cacheDir := fs.String("cache-dir", "", "explicit cache root (default: queried from the tool)")
	toolDir := fs.String("tool-dir", "", "explicit installed-tool root (default: queried from the tool)")
	digest := fs.String("digest", "", "confirmation digest produced by `cache preview`")
	maxBytes := fs.Int64("max-bytes", 0, "optional upper-bound impact cap in bytes")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	return a.withInstance(func() (output, error) {
		gate := scope.PauseGate{StateDir: a.paths.StateDir}
		if paused, perr := gate.Paused(); perr != nil {
			return output{}, errs.Wrap(errs.CodeStateWriteFailed, perr)
		} else if paused {
			return output{}, errs.New(errs.CodePaused, "instance is paused; run `dustkeeper resume` before mutating caches")
		}

		rec, err := cachetool.Prune(ctx, cachetool.Options{
			InstanceID: a.instance,
			RunID:      a.runID,
			StateDir:   a.paths.StateDir,
			Tool:       *tool,
			BinaryPath: *bin,
			CacheDir:   *cacheDir,
			ToolDir:    *toolDir,
			Digest:     *digest,
			MaxBytes:   *maxBytes,
			NowNS:      func() int64 { return a.clock.Wall().UnixNano() },
		})
		if err != nil {
			return output{}, err
		}

		res := ok()
		res.ObjectID = rec.OpID
		res.Data = rec
		if rec.ZeroAction {
			res.Code = errs.CodeNoCandidates
			res.Message = rec.ZeroActionReason
			res.Human = []string{
				fmt.Sprintf("cache prune (%s): zero action (%s)", rec.Tool, rec.ZeroActionReason),
				fmt.Sprintf("  cache root: %s (%s allocated, %d files)", rec.CacheRoot, humanBytesOf(rec.PostAllocatedBytes), rec.PostFiles),
			}
			return res, nil
		}

		toolSummary := rec.ToolReportedSummary
		if toolSummary == "" {
			toolSummary = "none"
		}
		res.Human = []string{
			fmt.Sprintf("cache prune completed (%s, op %s)", rec.ToolVersion, rec.OpID),
			fmt.Sprintf("  cache root:     %s", rec.CacheRoot),
			fmt.Sprintf("  reclaimed:      %s allocated (%s logical; files %d -> %d)",
				humanBytesOf(rec.ReclaimedAllocatedBytes), humanBytesOf(rec.ReclaimedLogicalBytes), rec.PreFiles, rec.PostFiles),
			fmt.Sprintf("  tool reported:  %s", toolSummary),
			fmt.Sprintf("  protected envs: intact=%v (%d checked)", rec.ProtectedEnvsIntact, len(rec.ProtectedEnvs)),
		}
		res.Next = []string{"dustkeeper cache status"}
		return res, nil
	})
}

func cmdCacheStatus(_ context.Context, a *app, args []string) (output, error) {
	fs := flagset("cache status", io.Discard)
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	st, err := cachetool.Status(a.paths.StateDir)
	if err != nil {
		return output{}, err
	}
	res := ok()
	res.Data = st
	lines := []string{fmt.Sprintf("cache operations recorded: %d", len(st.Records))}
	if st.PendingOp != nil {
		lines = append(lines, fmt.Sprintf("  unresolved pending op: %s (%s on %s)",
			st.PendingOp.OpID, st.PendingOp.Tool, st.PendingOp.CacheRoot))
		res.Next = []string{fmt.Sprintf("dustkeeper cache reconcile --op %s --confirm", st.PendingOp.OpID)}
	} else if len(st.Records) > 0 {
		last := st.Records[len(st.Records)-1]
		lines = append(lines, fmt.Sprintf("  latest: %s [%s] reclaimed %s allocated (zero_action=%v)",
			last.OpID, last.State, humanBytesOf(last.ReclaimedAllocatedBytes), last.ZeroAction))
	}
	res.Human = lines
	return res, nil
}

func cmdCacheReconcile(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("cache reconcile", io.Discard)
	opID := fs.String("op", "", "pending cache operation ID")
	confirm := fs.Bool("confirm", false, "confirm reconciling the interrupted cache operation")
	note := fs.String("note", "", "optional operator note")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if !*confirm {
		return output{}, errs.New(errs.CodeNeedsInput, "pass --confirm to mark the interrupted cache operation reconciled after checking post-state")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	return a.withInstance(func() (output, error) {
		rec, err := cachetool.Reconcile(ctx, a.paths.StateDir, a.instance, *opID, *note,
			func() int64 { return a.clock.Wall().UnixNano() })
		if err != nil {
			return output{}, err
		}
		res := ok()
		res.ObjectID = rec.OpID
		res.Data = rec
		res.Human = []string{
			fmt.Sprintf("reconciled cache operation %s (%s)", rec.OpID, rec.State),
			fmt.Sprintf("  observed delta: %s allocated reclaimed, protected envs intact=%v",
				humanBytesOf(rec.ReclaimedAllocatedBytes), rec.ProtectedEnvsIntact),
		}
		return res, nil
	})
}

func buildCachePruneCommand(tool, digest, bin, cacheDir, toolDir string, maxBytes int64) string {
	parts := []string{
		"dustkeeper cache prune",
		"--tool " + shellQuote(tool),
		"--digest " + shellQuote(digest),
	}
	if strings.TrimSpace(bin) != "" {
		parts = append(parts, "--bin "+shellQuote(bin))
	}
	if strings.TrimSpace(cacheDir) != "" {
		parts = append(parts, "--cache-dir "+shellQuote(cacheDir))
	}
	if strings.TrimSpace(toolDir) != "" {
		parts = append(parts, "--tool-dir "+shellQuote(toolDir))
	}
	if maxBytes > 0 {
		parts = append(parts, fmt.Sprintf("--max-bytes %d", maxBytes))
	}
	return strings.Join(parts, " ")
}
