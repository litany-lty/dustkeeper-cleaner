package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/store"
)

// cmdRule routes the rule subcommands.
func cmdRule(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"rule needs a subcommand: draft, import, show, validate, preview, enable, activate, authorize, revoke, disable")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "draft":
		return ruleDraft(ctx, a, rest)
	case "import":
		return ruleImport(ctx, a, rest)
	case "show":
		return ruleShow(ctx, a, rest)
	case "validate":
		return ruleValidate(ctx, a, rest)
	case "preview":
		return rulePreview(ctx, a, rest)
	case "enable":
		return ruleEnable(ctx, a, rest)
	case "activate":
		return ruleActivate(ctx, a, rest)
	case "authorize":
		return ruleAuthorize(ctx, a, rest)
	case "revoke":
		return ruleRevoke(ctx, a, rest)
	case "disable":
		return ruleDisable(ctx, a, rest)
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown rule subcommand %q", sub)
}

// ruleDraft asks the configured model for a draft. The model produces text that
// must survive the same strict parser as a hand-written file.
func ruleDraft(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule draft", io.Discard)
	text := fs.String("text", "", "natural-language description")
	scopeRef := fs.String("scope", "", "scope ID the rule may reference")
	id := fs.String("id", "", "rule ID to store the draft under")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *text == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--text is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	allowed, err := allowedScopes(ctx, a.store)
	if err != nil {
		return output{}, err
	}
	if len(allowed) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"no scopes are registered; register one before describing a rule")
	}
	if *scopeRef != "" {
		sc, err := resolveScope(ctx, a.store, *scopeRef)
		if err != nil {
			return output{}, err
		}
		allowed = map[string]bool{string(sc.ID): true}
	}

	draft, derr := a.draftRule(ctx, *text, allowed)
	if derr != nil {
		// A model outage never touches an existing rule. The offline path stays
		// fully available.
		out := fail(derr)
		out.Next = []string{"dustkeeper rule import --file <rule.json>"}
		return out, nil
	}
	if draft.NeedsInput {
		out := output{
			Status:  string(errs.StatusNeedsInput),
			Code:    errs.CodeNeedsInput,
			Message: "the description is missing information a rule needs",
			Data:    map[string]any{"questions": draft.Questions},
		}
		for _, q := range draft.Questions {
			out.Human = append(out.Human, "? "+q)
		}
		return out, nil
	}
	if draft.Kind == rules.DraftUnsupported {
		return output{
			Status:  string(errs.StatusNeedsInput),
			Code:    errs.CodeRuleUnsupported,
			Message: draft.Reason,
			Human:   []string{"the model could not express this safely: " + draft.Reason},
			Next:    []string{"dustkeeper rule import --file <rule.json>"},
		}, nil
	}

	parsed, err := rules.Parse([]byte(draft.DSL), allowed)
	if err != nil {
		return output{}, err
	}
	return storeDraft(ctx, a, parsed, *id, "model draft")
}

// ruleImport reads a hand-written rule. This is the complete offline path.
func ruleImport(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule import", io.Discard)
	file := fs.String("file", "", "path to a rule JSON file, or - for stdin")
	id := fs.String("id", "", "rule ID to store the draft under")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *file == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--file is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return output{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	allowed, err := allowedScopes(ctx, a.store)
	if err != nil {
		return output{}, err
	}
	parsed, err := rules.Parse(data, allowed)
	if err != nil {
		return output{}, err
	}
	return storeDraft(ctx, a, parsed, *id, "import")
}

// storeDraft persists one revision in draft state. Storing a draft grants no
// authority: activation and authorisation are separate commands.
func storeDraft(ctx context.Context, a *app, r *rules.Rule, id, source string) (output, error) {
	sc, err := resolveScope(ctx, a.store, r.ScopeID)
	if err != nil {
		return output{}, err
	}
	if id == "" {
		id = domain.NewID("rule")
	}
	ruleID := domain.RuleID(id)
	rev, err := a.store.NextRuleRevision(ctx, ruleID)
	if err != nil {
		return output{}, err
	}
	digest, err := r.Digest()
	if err != nil {
		return output{}, err
	}
	canonical, err := r.CanonicalJSON()
	if err != nil {
		return output{}, err
	}
	rb, _, err := plan.NewReviewBinding(sc, digest)
	if err != nil {
		return output{}, err
	}
	rbJSON, err := json.Marshal(rb)
	if err != nil {
		return output{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	stored := store.Rule{
		ID:                ruleID,
		Revision:          rev,
		ScopeID:           sc.ID,
		DSLJSON:           string(canonical),
		Digest:            digest,
		ReviewBindingJSON: string(rbJSON),
		Status:            domain.RuleDraft,
		CreatedAtNS:       a.clock.Wall().UnixNano(),
	}
	if err := a.store.InsertRule(ctx, stored); err != nil {
		return output{}, err
	}

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"rule_id":  string(ruleID),
		"revision": rev,
		"digest":   string(digest),
		"status":   domain.RuleDraft,
		"source":   source,
		"rule":     r,
		"explain":  r.Explain(),
	}
	out.Human = append(out.Human, fmt.Sprintf("stored draft rule %s revision %d", ruleID, rev))
	for _, line := range r.Explain() {
		out.Human = append(out.Human, "  "+line)
	}
	out.Human = append(out.Human,
		"  a draft does not clean anything; review it, then activate it with the review digest")
	out.Next = []string{
		fmt.Sprintf("dustkeeper rule preview --id %s --revision %d", ruleID, rev),
	}
	return out, nil
}

func ruleShow(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule show", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "revision; 0 means the active one")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	var r store.Rule
	var err error
	if *rev == 0 {
		r, err = a.store.ActiveRule(ctx, domain.RuleID(*id))
	} else {
		r, err = a.store.GetRule(ctx, domain.RuleID(*id), *rev)
	}
	if err != nil {
		return output{}, err
	}
	parsed, perr := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(r.ScopeID): true})
	out := ok()
	out.ScopeID = string(r.ScopeID)
	out.Data = map[string]any{"rule": r, "parsed": parsed}
	out.Human = []string{
		fmt.Sprintf("rule %s revision %d (%s)", r.ID, r.Revision, r.Status),
		"  scope:  " + string(r.ScopeID),
		"  digest: " + string(r.Digest),
	}
	if perr == nil {
		for _, line := range parsed.Explain() {
			out.Human = append(out.Human, "  "+line)
		}
	} else {
		out.Human = append(out.Human, "  stored DSL cannot be re-parsed: "+messageOf(perr))
	}
	return out, nil
}

func ruleValidate(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule validate", io.Discard)
	file := fs.String("file", "", "path to a rule JSON file, or - for stdin")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *file == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--file is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return output{}, errs.Wrap(errs.CodeAccessDenied, err)
	}
	allowed, err := allowedScopes(ctx, a.store)
	if err != nil {
		return output{}, err
	}
	parsed, err := rules.Parse(data, allowed)
	if err != nil {
		return output{}, err
	}
	digest, err := parsed.Digest()
	if err != nil {
		return output{}, err
	}
	canonical, err := parsed.CanonicalJSON()
	if err != nil {
		return output{}, err
	}
	out := ok()
	out.Data = map[string]any{
		"digest":    string(digest),
		"canonical": string(canonical),
		"explain":   parsed.Explain(),
	}
	out.Human = []string{"valid rule; digest " + string(digest)}
	for _, line := range parsed.Explain() {
		out.Human = append(out.Human, "  "+line)
	}
	return out, nil
}

// rulePreview freezes a dry-run plan: it shows exactly what would move and the
// digest the user must repeat to activate the rule.
func rulePreview(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule preview", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "revision; 0 means the active one")
	maxFiles := fs.Int("max-files", 0, "how many objects to show (default from configuration)")
	maxBytes := fs.Int64("max-bytes", 0, "logical byte ceiling for this preview")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	r, err := ruleAt(ctx, a.store, *id, *rev)
	if err != nil {
		return output{}, err
	}
	sc, err := resolveScope(ctx, a.store, string(r.ScopeID))
	if err != nil {
		return output{}, err
	}
	parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
	if err != nil {
		return output{}, err
	}

	files := *maxFiles
	if files <= 0 {
		files = a.cfg.Runtime.MaxAutoQuarantineFiles
	}
	if files <= 0 {
		files = 200
	}
	bytes := *maxBytes
	if bytes <= 0 {
		bytes = a.cfg.Runtime.MaxAutoQuarantineBytes
	}
	if bytes <= 0 {
		bytes = 1 << 40
	}

	sctx, err := execute.OpenScopeCtx(ctx, a.registrar(), sc.ID)
	if err != nil {
		return output{}, err
	}
	defer sctx.Close()
	guard, err := guardFor(ctx, a.store, sctx)
	if err != nil {
		return output{}, err
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
		// The plan compares the same ancestry the executor will compare, so a
		// preview cannot offer an object the run would refuse.
		AncestorsOf: func(dir string) ([]domain.FileIdentity, error) {
			return execute.AncestorIdentities(sctx, []byte(dir))
		},
	})
	if err != nil {
		return output{}, err
	}

	rb, rbDigest, err := plan.NewReviewBinding(sc, r.Digest)
	if err != nil {
		return output{}, err
	}

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"rule_id":        string(r.ID),
		"revision":       r.Revision,
		"matched":        len(res.Items),
		"logical_bytes":  res.Bytes,
		"truncated":      res.Truncated,
		"reasons":        res.Reasons,
		"scanned":        res.Scanned,
		"plan_digest":    string(res.Plan.Digest),
		"review_digest":  string(rbDigest),
		"review_binding": rb,
		"items":          previewItems(res.Items),
	}
	out.Human = []string{
		fmt.Sprintf("preview for rule %s revision %d: %d object(s), %d bytes",
			r.ID, r.Revision, len(res.Items), res.Bytes),
	}
	for _, it := range res.Items {
		out.Human = append(out.Human, fmt.Sprintf("  %s  %s", it.ID, displayPathBytes(it.RawRelative)))
	}
	if res.Truncated {
		out.Human = append(out.Human, "  truncated: the limit stopped the list; raise --max-files to see more")
	}
	out.Human = append(out.Human,
		"  this preview touched nothing",
		"",
		"review digest (repeat it to activate):",
		"  "+string(rbDigest),
	)
	out.Next = []string{
		fmt.Sprintf("dustkeeper rule activate --id %s --revision %d --digest %s", r.ID, r.Revision, rbDigest),
	}
	if sc.Kind == domain.KindManagedDisposable {
		// Activation still grants nothing. On a disposable scope the shortcut that
		// does both is one command with one digest, so it is offered here instead
		// of leaving the operator to discover it from the help text.
		out.Next = append(out.Next, fmt.Sprintf(
			"dustkeeper rule enable --id %s --revision %d --mode quarantine   (or --mode cleanup to approve permanent deletion too)",
			r.ID, r.Revision))
	}
	return out, nil
}

func previewItems(items []store.Item) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"item_id":       string(it.ID),
			"relative_path": displayPathBytes(it.RawRelative),
			"reason":        it.Reason,
			"source_kind":   it.SourceKind,
		})
	}
	return out
}

// ruleActivate pins one revision and invalidates the previous revision's
// grants. The digest must match what preview showed.
func ruleActivate(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule activate", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "revision to activate")
	digest := fs.String("digest", "", "the full review digest shown by preview")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" || *rev == 0 || *digest == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id, --revision and --digest are required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	r, err := a.store.GetRule(ctx, domain.RuleID(*id), *rev)
	if err != nil {
		return output{}, err
	}
	sc, err := resolveScope(ctx, a.store, string(r.ScopeID))
	if err != nil {
		return output{}, err
	}
	rb, rbDigest, err := plan.NewReviewBinding(sc, r.Digest)
	if err != nil {
		return output{}, err
	}
	if domain.Digest(*digest) != rbDigest {
		return output{}, errs.Newf(errs.CodePlanStale,
			"the review digest does not match the current scope state; run `rule preview` again (current digest %s)", rbDigest)
	}
	rbJSON, err := json.Marshal(rb)
	if err != nil {
		return output{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := a.store.ActivateRuleWithReviewBinding(ctx, r.ID, r.Revision, string(rbJSON)); err != nil {
		return output{}, err
	}

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"rule_id":       string(r.ID),
		"revision":      r.Revision,
		"status":        domain.RuleActive,
		"review_digest": string(rbDigest),
	}
	out.Human = []string{
		fmt.Sprintf("rule %s revision %d is now active", r.ID, r.Revision),
		"  activation is not permission: nothing runs in the background until a capability is granted",
	}
	out.Next = []string{
		fmt.Sprintf("dustkeeper rule authorize --id %s --revision %d --auto-quarantine", r.ID, r.Revision),
	}
	return out, nil
}

// ruleAuthorize is the explicit control point for unattended work. Without a
// digest it only proposes; with the matching digest it stores one independent
// capability.
func ruleAuthorize(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule authorize", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "active revision")
	autoQ := fs.Bool("auto-quarantine", false, "grant unattended quarantine")
	autoP := fs.Bool("auto-purge", false, "grant unattended permanent deletion")
	digest := fs.String("digest", "", "the approval digest shown by the proposal")
	maxFiles := fs.Int("max-files", 0, "approved file ceiling (default from configuration)")
	maxBytes := fs.Int64("max-bytes", 0, "approved logical byte ceiling (default from configuration)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" || *rev == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id and --revision are required")
	}
	if *autoQ == *autoP {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"choose exactly one capability: --auto-quarantine or --auto-purge. They are separate switches")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	r, err := a.store.GetRule(ctx, domain.RuleID(*id), *rev)
	if err != nil {
		return output{}, err
	}
	if r.Status != domain.RuleActive {
		return output{}, errs.Newf(errs.CodeGrantRequired,
			"rule %s revision %d is %s; activate it before granting a background capability", r.ID, r.Revision, r.Status)
	}
	sc, err := resolveScope(ctx, a.store, string(r.ScopeID))
	if err != nil {
		return output{}, err
	}
	parsed, err := rules.Parse([]byte(r.DSLJSON), map[string]bool{string(sc.ID): true})
	if err != nil {
		return output{}, err
	}

	capability := domain.CapAutoQuarantine
	if *autoP {
		capability = domain.CapAutoPurge
	}
	if capability == domain.CapAutoPurge && sc.Kind != domain.KindManagedDisposable {
		return output{}, errs.Newf(errs.CodeGrantRequired,
			"scope %s is %s; unattended permanent deletion requires managed_disposable", sc.ID, sc.Kind)
	}

	// The ceiling belongs to the action being approved. Reading the quarantine
	// default for a purge approval would hand permanent deletion a number that
	// was set for moving files into the vault.
	files, bytes := a.ceilingFor(capability, *maxFiles, *maxBytes)
	if files <= 0 || bytes <= 0 {
		return output{}, errs.Newf(errs.CodeInvalidArgument,
			"the %s ceiling is zero, which forbids this action; pass --max-files and --max-bytes explicitly or raise %s",
			capability, ceilingKey(capability))
	}

	rb, _, err := plan.NewReviewBinding(sc, r.Digest)
	if err != nil {
		return output{}, err
	}
	gb, grantDigest, err := plan.NewGrantBinding(rb, r.ID, r.Revision, capability, files, bytes, parsed.RetentionSeconds)
	if err != nil {
		return output{}, err
	}

	existing, gerr := a.store.ActiveGrant(ctx, sc.ID, r.ID, r.Revision, capability)
	if gerr == nil && existing.Digest == grantDigest {
		out := ok()
		out.ScopeID = string(sc.ID)
		out.Data = map[string]any{"grant_id": existing.ID, "digest": existing.Digest, "already_active": true}
		out.Human = []string{fmt.Sprintf("an identical %s grant is already active: %s", capability, existing.ID)}
		return out, nil
	}

	if *digest == "" {
		out := ok()
		out.ScopeID = string(sc.ID)
		out.Data = map[string]any{
			"capability":      capability,
			"rule_id":         string(r.ID),
			"revision":        r.Revision,
			"scope_id":        string(sc.ID),
			"max_files":       files,
			"max_bytes":       bytes,
			"retention":       parsed.RetentionSeconds,
			"retroactive":     false,
			"approval_digest": string(grantDigest),
		}
		out.Human = []string{
			fmt.Sprintf("proposed %s grant for rule %s revision %d", capability, r.ID, r.Revision),
			fmt.Sprintf("  scope:            %s (%s)", sc.ID, sc.Kind),
			"  rule digest:      " + string(r.Digest),
			fmt.Sprintf("  retention:        %d seconds (from the approved DSL)", parsed.RetentionSeconds),
			fmt.Sprintf("  approved ceiling: %d file(s), %d bytes", files, bytes),
			"  effective ceiling is the smaller of this and the local configuration",
			"  objects already isolated: NOT covered — this grant is never retroactive",
			"",
			"approval digest (repeat it to commit):",
			"  " + string(grantDigest),
		}
		out.Next = []string{
			fmt.Sprintf("dustkeeper rule authorize --id %s --revision %d --%s --digest %s",
				r.ID, r.Revision, capabilityFlag(capability), grantDigest),
		}
		return out, nil
	}

	if domain.Digest(*digest) != grantDigest {
		return output{}, errs.Newf(errs.CodeDigestMismatch,
			"the approval digest does not match the current proposal; re-run without --digest (current digest %s)", grantDigest)
	}

	gbJSON, err := json.Marshal(gb)
	if err != nil {
		return output{}, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	grant := store.Grant{
		ID:           domain.GrantID(domain.NewID("grant")),
		ScopeID:      sc.ID,
		RuleID:       r.ID,
		RuleRevision: r.Revision,
		Capability:   capability,
		BindingJSON:  string(gbJSON),
		Digest:       grantDigest,
		Status:       domain.GrantActive,
		ApprovedAtNS: a.clock.Wall().UnixNano(),
	}
	if err := a.store.InsertGrant(ctx, grant); err != nil {
		return output{}, err
	}

	out := ok()
	out.ScopeID = string(sc.ID)
	out.Data = map[string]any{
		"grant_id":   string(grant.ID),
		"capability": capability,
		"digest":     string(grantDigest),
		"max_files":  files,
		"max_bytes":  bytes,
	}
	out.Human = []string{
		fmt.Sprintf("granted %s for rule %s revision %d", capability, r.ID, r.Revision),
		"  grant: " + string(grant.ID),
	}
	if capability == domain.CapAutoPurge {
		out.Human = append(out.Human, "  permanent deletion still requires the retention window to elapse and a fresh verification")
	}
	out.Next = []string{"dustkeeper run"}
	return out, nil
}

func ruleRevoke(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule revoke", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "revision")
	autoQ := fs.Bool("auto-quarantine", false, "revoke the quarantine capability")
	autoP := fs.Bool("auto-purge", false, "revoke the purge capability")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" || *rev == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id and --revision are required")
	}
	if *autoQ == *autoP {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"choose exactly one capability: --auto-quarantine or --auto-purge")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	r, err := a.store.GetRule(ctx, domain.RuleID(*id), *rev)
	if err != nil {
		return output{}, err
	}
	capability := domain.CapAutoQuarantine
	if *autoP {
		capability = domain.CapAutoPurge
	}
	n, err := a.store.RevokeGrants(ctx, r.ID, r.Revision, capability)
	if err != nil {
		return output{}, err
	}
	out := ok()
	out.ScopeID = string(r.ScopeID)
	out.Data = map[string]any{"revoked": n, "capability": capability}
	out.Human = []string{fmt.Sprintf("revoked %d %s grant(s) for rule %s revision %d", n, capability, r.ID, r.Revision)}
	if n == 0 {
		out.Human = append(out.Human, "  there was no active grant to revoke")
	}
	out.Human = append(out.Human, "  revocation takes effect for intents committed after this transaction")
	return out, nil
}

func ruleDisable(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("rule disable", io.Discard)
	id := fs.String("id", "", "rule ID")
	rev := fs.Int64("revision", 0, "revision; 0 means the active one")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *id == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "--id is required")
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	var r store.Rule
	var err error
	if *rev == 0 {
		r, err = a.store.ActiveRule(ctx, domain.RuleID(*id))
	} else {
		r, err = a.store.GetRule(ctx, domain.RuleID(*id), *rev)
	}
	if err != nil {
		return output{}, err
	}
	if err := a.store.DisableRule(ctx, r.ID, r.Revision); err != nil {
		return output{}, err
	}
	out := ok()
	out.ScopeID = string(r.ScopeID)
	out.Data = map[string]any{"rule_id": string(r.ID), "revision": r.Revision, "status": domain.RuleDisabled}
	out.Human = []string{
		fmt.Sprintf("rule %s revision %d disabled; its grants are revoked", r.ID, r.Revision),
		"  objects already quarantined stay recoverable for their full retention window",
	}
	return out, nil
}

// ---- helpers --------------------------------------------------------------

func allowedScopes(ctx context.Context, st *store.Store) (map[string]bool, error) {
	scopes, err := st.ListScopes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(scopes))
	for _, sc := range scopes {
		out[string(sc.ID)] = true
	}
	return out, nil
}

// capabilityFlag turns a stored capability into the CLI flag that grants it.
func capabilityFlag(capability string) string {
	if capability == domain.CapAutoPurge {
		return "auto-purge"
	}
	return "auto-quarantine"
}

func ruleAt(ctx context.Context, st *store.Store, id string, rev int64) (store.Rule, error) {
	if rev == 0 {
		return st.ActiveRule(ctx, domain.RuleID(id))
	}
	return st.GetRule(ctx, domain.RuleID(id), rev)
}

func displayPathBytes(b []byte) string {
	return string(b)
}
