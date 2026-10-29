package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/store"
)

// The two modes of one enable. They are strings rather than booleans because the
// screen has to say what "cleanup" means, and a flag called --auto-purge would
// invite enabling deletion by copying a line from a tutorial.
const (
	enableModeQuarantine = "quarantine"
	enableModeCleanup    = "cleanup"
)

// ruleEnable turns a rule revision into standing authority in two calls.
//
// Without --digest it only shows what would be approved and the digest of that
// approval; nothing is written, so an operator who reads the screen and stops has
// changed nothing. With the digest it commits the same approval in one
// transaction: rule activation, review binding and the selected capabilities
// either all happen or none do.
func ruleEnable(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule enable", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "revision to enable")
	mode := fs.String("mode", enableModeQuarantine,
		"quarantine (isolate only) or cleanup (isolate, then permanently delete when the retention period elapses)")
	digest := fs.String("digest", "", "the enable digest shown by the call without --digest")
	maxFiles := fs.Int("max-files", 0, "approved quarantine file ceiling (default from configuration)")
	maxBytes := fs.Int64("max-bytes", 0, "approved quarantine logical byte ceiling (default from configuration)")
	purgeFiles := fs.Int("purge-max-files", 0, "approved permanent-deletion file ceiling (default from configuration)")
	purgeBytes := fs.Int64("purge-max-bytes", 0, "approved permanent-deletion logical byte ceiling (default from configuration)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" || *rev == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id and --revision are required")
	}
	if *mode != enableModeQuarantine && *mode != enableModeCleanup {
		return output{}, errs.Newf(errs.CodeInvalidArgument,
			"--mode must be %s or %s; got %q", enableModeQuarantine, enableModeCleanup, *mode)
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	return a.withInstance(func() (output, error) {
		r, err := a.store.GetRule(ctx, domain.RuleID(*id), *rev)
		if err != nil {
			return output{}, err
		}
		sc, err := resolveScope(ctx, a.store, string(r.ScopeID))
		if err != nil {
			return output{}, err
		}
		// Automatic work is only ever opened on a directory the user marked as
		// disposable. An assisted scope keeps its manual plan route, and nothing
		// else gains background authority through this command.
		if sc.Kind != domain.KindManagedDisposable {
			return output{}, errs.Newf(errs.CodeInvalidArgument,
				"scope %s is %s; automatic work is only enabled for managed_disposable (an assisted scope keeps its manual plan route)",
				sc.ID, sc.Kind)
		}
		parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
		if err != nil {
			return output{}, err
		}

		rb, _, err := plan.NewReviewBinding(sc, r.Digest)
		if err != nil {
			return output{}, err
		}
		grantBindings, grantRows, ceilings, err := enableGrants(a, sc, r, rb, parsed, *mode,
			*maxFiles, *maxBytes, *purgeFiles, *purgeBytes)
		if err != nil {
			return output{}, err
		}
		eb, enableDigest, err := plan.NewEnableBinding(rb, r.ID, r.Revision, grantBindings)
		if err != nil {
			return output{}, err
		}

		// The candidate sample comes from the same index the planner uses, and it
		// is shown together with whether the scan behind it finished. A partial
		// sample must never be presented as "this is the whole directory".
		scanState, matched, truncated, sample, perr := a.enableSample(ctx, sc, r, parsed)
		if perr != nil {
			return output{}, perr
		}
		complete := sc.ScanEpoch > 0 && scanState.Pending == 0 && scanState.Failed == 0

		if *digest == "" {
			out := ok()
			out.ScopeID = string(sc.ID)
			out.Data = map[string]any{
				"mode":                *mode,
				"capabilities":        eb,
				"rule_id":             string(r.ID),
				"revision":            r.Revision,
				"rule_digest":         string(r.Digest),
				"retention_seconds":   parsed.RetentionSeconds,
				"scope_id":            string(sc.ID),
				"scope_kind":          sc.Kind,
				"scope_generation":    sc.Generation,
				"protection_revision": sc.ProtectionRevision,
				"ceilings":            ceilings,
				"scan": map[string]any{
					"epoch":    scanState.Epoch,
					"pending":  scanState.Pending,
					"failed":   scanState.Failed,
					"complete": complete,
				},
				"candidates": map[string]any{
					"matched":   matched,
					"truncated": truncated,
					"items":     sample,
				},
				"enable_digest": string(enableDigest),
			}
			out.Human = []string{
				fmt.Sprintf("enable rule %s revision %d on scope %s (%s)", r.ID, r.Revision, sc.ID, sc.Kind),
				"  root:             " + displayPathBytes(sc.RawRoot),
				"  mode:             " + enableModeLabel(*mode),
				"  rule digest:      " + string(r.Digest),
				fmt.Sprintf("  retention:        %d seconds (%d days)", parsed.RetentionSeconds, parsed.RetentionSeconds/86400),
			}
			for _, g := range eb.Grants {
				out.Human = append(out.Human, fmt.Sprintf("  ceiling (%s): %d file(s), %d bytes",
					g.Capability, g.ApprovedMaxFiles, g.ApprovedMaxLogicalBytes))
			}
			out.Human = append(out.Human,
				"  what the rule matches:",
			)
			for _, line := range parsed.Explain() {
				out.Human = append(out.Human, "    "+line)
			}
			out.Human = append(out.Human,
				"  effective ceiling is the smaller of these and the local configuration; zero forbids the action",
				fmt.Sprintf("  candidates:       %d match(es)%s", matched, truncationNote(truncated)),
			)
			// The sample is the same bounded list the JSON carries. Printing the
			// count without it made the screen say "the files listed above" while
			// listing nothing.
			const humanSample = 5
			for i, item := range sample {
				if i >= humanSample {
					out.Human = append(out.Human, fmt.Sprintf("    ... %d more not shown", len(sample)-humanSample))
					break
				}
				rel, _ := item["relative_path"].(string)
				out.Human = append(out.Human, "    "+rel)
			}
			if len(sample) == 0 {
				out.Human = append(out.Human, "    (no file currently matches)")
			}
			out.Human = append(out.Human,
				scanCompletenessNote(complete, scanState),
				"",
				"this approves the rule and its permissions for future matches, not the files listed above",
				"",
				"enable digest (repeat it to commit):",
				"  "+string(enableDigest),
			)
			out.Next = []string{enableConfirmCommand(r, *mode, ceilings, string(enableDigest))}
			return out, nil
		}

		if domain.Digest(*digest) != enableDigest {
			return output{}, errs.Newf(errs.CodePlanStale,
				"the enable digest does not match the current rule and scope state; run `%s` again (current digest %s)",
				enableConfirmCommand(r, *mode, ceilings, ""), enableDigest)
		}

		rbJSON, err := json.Marshal(rb)
		if err != nil {
			return output{}, errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		stored, err := a.store.EnableRule(ctx, store.EnableRuleRequest{
			RuleID:                     r.ID,
			Revision:                   r.Revision,
			ExpectedRuleDigest:         r.Digest,
			ExpectedScopeID:            sc.ID,
			ExpectedScopeGeneration:    sc.Generation,
			ExpectedProtectionRevision: sc.ProtectionRevision,
			ExpectedRootIdentityJSON:   sc.RootIdentityJSON,
			ReviewBindingJSON:          string(rbJSON),
			Grants:                     grantRows,
		})
		if err != nil {
			return output{}, err
		}

		out := ok()
		out.ScopeID = string(sc.ID)
		grantsOut := make([]map[string]any, 0, len(stored))
		for _, g := range stored {
			grantsOut = append(grantsOut, map[string]any{
				"grant_id":   string(g.ID),
				"capability": g.Capability,
				"status":     g.Status,
				"digest":     string(g.Digest),
				"max_files":  ceilingOf(ceilings, g.Capability).Files,
				"max_bytes":  ceilingOf(ceilings, g.Capability).Bytes,
			})
		}
		out.Data = map[string]any{
			"rule_id":       string(r.ID),
			"revision":      r.Revision,
			"mode":          *mode,
			"scope_id":      string(sc.ID),
			"enable_digest": string(enableDigest),
			"grants":        grantsOut,
		}
		out.Human = []string{
			fmt.Sprintf("enabled rule %s revision %d on scope %s", r.ID, r.Revision, sc.ID),
			"  mode: " + enableModeLabel(*mode),
		}
		for _, g := range stored {
			out.Human = append(out.Human, fmt.Sprintf("  %s grant: %s (%d file(s), %d bytes)",
				g.Capability, g.ID, ceilingOf(ceilings, g.Capability).Files, ceilingOf(ceilings, g.Capability).Bytes))
		}
		out.Human = append(out.Human,
			"  objects already isolated keep their own approval; this enable does not reach back to them")
		out.Next = []string{"dustkeeper run"}
		return out, nil
	})
}

// Ceiling is the approved allowance for one capability, as shown and as stored.
//
// The fields are exported with JSON tags because the preview prints them: an
// unexported struct encoded to `{}` per capability, which read as "no ceiling
// was approved" in a screen whose whole purpose is to show the numbers.
type Ceiling struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// enableConfirmCommand builds the command that commits exactly the screen the
// user just read, ceilings included.
//
// The omissions this replaces were a real failure of the normal path: a preview
// with `--purge-max-files 10` printed a follow-up command without that flag, so
// copying it recomputed the digest with the configured default and the commit was
// refused as stale. The numbers are therefore always passed explicitly; passing a
// ceiling that happens to equal the default produces the same digest.
func enableConfirmCommand(r store.Rule, mode string, ceilings map[string]Ceiling, digest string) string {
	cmd := fmt.Sprintf("dustkeeper rule enable --id %s --revision %d --mode %s",
		shellQuote(string(r.ID)), r.Revision, mode)
	if c, ok := ceilings[domain.CapAutoQuarantine]; ok {
		cmd += fmt.Sprintf(" --max-files %d --max-bytes %d", c.Files, c.Bytes)
	}
	if c, ok := ceilings[domain.CapAutoPurge]; ok {
		cmd += fmt.Sprintf(" --purge-max-files %d --purge-max-bytes %d", c.Files, c.Bytes)
	}
	if digest != "" {
		cmd += " --digest " + digest
	}
	return cmd
}

// enableGrants builds the selected capabilities with their own ceilings.
//
// The two capabilities read their own configuration keys. Reading the quarantine
// default for a purge approval was a real defect: it made permanently deleting
// files inherit a number that was approved for moving them into the vault, which
// is a different action with a different risk.
func enableGrants(a *app, sc store.Scope, r store.Rule, rb plan.ReviewBindingV1, parsed *rules.Rule,
	mode string, maxFiles int, maxBytes int64, purgeFiles int, purgeBytes int64,
) ([]plan.GrantBindingV1, []store.Grant, map[string]Ceiling, error) {

	capabilities := []string{domain.CapAutoQuarantine}
	if mode == enableModeCleanup {
		capabilities = append(capabilities, domain.CapAutoPurge)
	}

	bindings := make([]plan.GrantBindingV1, 0, len(capabilities))
	rows := make([]store.Grant, 0, len(capabilities))
	ceilings := make(map[string]Ceiling, len(capabilities))
	nowNS := a.clock.Wall().UnixNano()

	for _, capability := range capabilities {
		files, bytes := a.ceilingFor(capability, maxFiles, maxBytes)
		if capability == domain.CapAutoPurge {
			files, bytes = a.ceilingFor(capability, purgeFiles, purgeBytes)
		}
		if files <= 0 || bytes <= 0 {
			return nil, nil, nil, errs.Newf(errs.CodeInvalidArgument,
				"the effective %s ceiling is zero, which forbids the action; raise %s or pass an explicit ceiling for %s",
				capability, ceilingKey(capability), capability)
		}
		gb, gd, err := plan.NewGrantBinding(rb, r.ID, r.Revision, capability, files, bytes, parsed.RetentionSeconds)
		if err != nil {
			return nil, nil, nil, err
		}
		gbJSON, err := json.Marshal(gb)
		if err != nil {
			return nil, nil, nil, errs.Wrap(errs.CodeStateWriteFailed, err)
		}
		bindings = append(bindings, gb)
		ceilings[capability] = Ceiling{Files: files, Bytes: bytes}
		rows = append(rows, store.Grant{
			ScopeID:      sc.ID,
			RuleID:       r.ID,
			RuleRevision: r.Revision,
			Capability:   capability,
			BindingJSON:  string(gbJSON),
			Digest:       gd,
			Status:       domain.GrantActive,
			ApprovedAtNS: nowNS,
		})
	}
	return bindings, rows, ceilings, nil
}

// ceilingFor returns the approved ceiling for one capability: the number the
// user typed if any, otherwise that capability's own configured default.
//
// The two capabilities read two keys. Reading the quarantine default for a purge
// approval made permanent deletion inherit a number that was approved for moving
// files into the vault, which is a different action with a different risk; zero
// still means "forbidden" and is never treated as "unset".
func (a *app) ceilingFor(capability string, files int, bytes int64) (int, int64) {
	if capability == domain.CapAutoPurge {
		if files <= 0 {
			files = a.cfg.Runtime.MaxAutoPurgeFiles
		}
		if bytes <= 0 {
			bytes = a.cfg.Runtime.MaxAutoPurgeBytes
		}
		return files, bytes
	}
	if files <= 0 {
		files = a.cfg.Runtime.MaxAutoQuarantineFiles
	}
	if bytes <= 0 {
		bytes = a.cfg.Runtime.MaxAutoQuarantineBytes
	}
	return files, bytes
}

// ceilingKey names the configuration keys behind a capability, so an operator who
// sees a zero knows which two numbers to look at.
func ceilingKey(capability string) string {
	if capability == domain.CapAutoPurge {
		return "max_auto_purge_files / max_auto_purge_bytes"
	}
	return "max_auto_quarantine_files / max_auto_quarantine_bytes"
}

// enableSample plans a dry run so the screen can show what the rule would take,
// and reports what the scan behind that sample actually proved.
func (a *app) enableSample(ctx context.Context, sc store.Scope, r store.Rule, parsed *rules.Rule,
) (store.ScanQueueState, int, bool, []map[string]any, error) {

	var state store.ScanQueueState
	if sc.ScanEpoch > 0 {
		got, err := a.store.ScanQueueStateOf(ctx, sc.ID, sc.ScanEpoch)
		if err != nil {
			return state, 0, false, nil, err
		}
		state = got
	}

	sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), sc.ID)
	if err != nil {
		return state, 0, false, nil, err
	}
	defer sctx.Close()
	guard, err := guardFor(ctx, a.store, sctx)
	if err != nil {
		return state, 0, false, nil, err
	}

	files := a.cfg.Runtime.MaxAutoQuarantineFiles
	if files <= 0 {
		files = 200
	}
	bytes := a.cfg.Runtime.MaxAutoQuarantineBytes
	if bytes <= 0 {
		bytes = 1 << 40
	}
	planner := plan.Planner{Store: a.store, Clock: a.clock}
	res, err := planner.CreateRulePlan(ctx, plan.RulePlanRequest{
		Scope:    sc,
		Rule:     parsed,
		RuleRef:  domain.RuleRef{ID: r.ID, Revision: r.Revision, Digest: r.Digest},
		Kind:     domain.PlanManualRule,
		Guard:    guard,
		MaxFiles: files,
		MaxBytes: bytes,
		Config:   a.cfg,
		NowNS:    a.clock.Wall().UnixNano(),
		DryRun:   true,
		AncestorsOf: func(dir string) ([]domain.FileIdentity, error) {
			return execute.AncestorIdentities(sctx, []byte(dir))
		},
	})
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNoCandidates {
			return state, 0, false, nil, nil
		}
		return state, 0, false, nil, err
	}
	return state, len(res.Items), res.Truncated, previewItems(res.Items), nil
}

func enableModeLabel(mode string) string {
	if mode == enableModeCleanup {
		return "cleanup — isolate now, permanently delete when the retention period elapses"
	}
	return "quarantine — isolate only; nothing is deleted"
}

func scanCompletenessNote(complete bool, st store.ScanQueueState) string {
	if complete {
		return fmt.Sprintf("  scan:             epoch %d complete", st.Epoch)
	}
	return fmt.Sprintf("  scan:             INCOMPLETE (epoch %d, %d directory(ies) not proven, %d failed) — the count above is a lower bound",
		st.Epoch, st.Pending, st.Failed)
}

func truncationNote(truncated bool) string {
	if truncated {
		return ", truncated by the preview limit (the directory may hold more)"
	}
	return ""
}

func ceilingOf(ceilings map[string]Ceiling, capability string) Ceiling {
	return ceilings[capability]
}
