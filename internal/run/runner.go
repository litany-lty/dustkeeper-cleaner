// Package run performs one bounded background pass.
//
// It lives outside internal/execute because it composes execution and
// reconciliation, and reconciliation already depends on execution. Keeping the
// pipeline in its own package preserves that direction.
package run

import (
	"context"
	"encoding/json"
	"strconv"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/recovery"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/scan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Clock thresholds from section 11.2.
const (
	clockBackwardsTolerance = int64(5 * 60 * 1e9)
	clockJumpThreshold      = int64(48 * 60 * 60 * 1e9)
	clockDriftTolerance     = int64(5 * 60 * 1e9)
)

// Runner performs one bounded run.
type Runner struct {
	Store      *store.Store
	Clock      domain.Clock
	Config     config.Config
	Registrar  *scope.Registrar
	Exec       *execute.Executor
	Reconciler *recovery.Reconciler
}

// ScopeRun reports what happened to one scope.
type ScopeRun struct {
	Scope            domain.ScopeID `json:"scope_id"`
	Skipped          string         `json:"skipped,omitempty"`
	Scanned          int            `json:"scanned_files"`
	ScanComplete     bool           `json:"scan_epoch_complete"`
	Quarantined      int            `json:"quarantined"`
	QuarantinedBytes int64          `json:"quarantined_logical_bytes"`
	Purged           int            `json:"purged"`
	PurgedBytes      int64          `json:"purged_logical_bytes"`
	DueNotAuthorised int            `json:"due_but_not_authorised"`
	Message          string         `json:"message,omitempty"`
}

// Report is the machine-readable result of `run`.
type Report struct {
	RunID          string          `json:"run_id"`
	ClockTrusted   bool            `json:"clock_trusted"`
	ClockNote      string          `json:"clock_note,omitempty"`
	Reconciliation recovery.Report `json:"reconciliation"`
	Scopes         []ScopeRun      `json:"scopes"`
	Quarantined    int             `json:"quarantined_total"`
	Purged         int             `json:"purged_total"`
	ReadBytes      int64           `json:"read_bytes"`
	DiskFreeBefore uint64          `json:"disk_free_bytes_before"`
	DiskFreeAfter  uint64          `json:"disk_free_bytes_after"`
}

// Run executes one pass.
//
// Order: reconcile, then purge what is due and authorised, then scan, then
// quarantine. Purging first prevents a scope that keeps producing garbage from
// starving objects that are already due.
func (r *Runner) Run(ctx context.Context) (Report, error) {
	rep := Report{RunID: domain.NewID("run")}
	nowNS := r.Clock.Wall().UnixNano()
	rep.DiskFreeBefore = r.diskFree()

	trusted, note := r.clockTrust(ctx, nowNS)
	rep.ClockTrusted = trusted
	rep.ClockNote = note

	rec, err := r.Reconciler.Run(ctx)
	rep.Reconciliation = rec
	if err != nil {
		return rep, err
	}

	scopes, err := r.Store.ListScopes(ctx)
	if err != nil {
		return rep, err
	}
	ordered := r.rotate(scopes)

	budget := domain.NewBudget(r.Config.Runtime.RunSoftSeconds,
		r.Config.Runtime.RunReadBudgetBytes, 0, 0, r.Config.Runtime.ReadRateBytesPerSecond)

	for _, sc := range ordered {
		if ctx.Err() != nil {
			break
		}
		if sc.Kind == domain.KindReference {
			continue
		}
		sr := ScopeRun{Scope: sc.ID}
		if sc.Status != domain.ScopeActive {
			sr.Skipped = "scope is " + sc.Status
			rep.Scopes = append(rep.Scopes, sr)
			continue
		}
		if budget.TimeUp(r.Clock) || budget.ReadUp() {
			sr.Skipped = "run budget reached"
			rep.Scopes = append(rep.Scopes, sr)
			continue
		}
		result, runErr := r.runScope(ctx, sc, budget, trusted)
		if runErr != nil {
			result.Message = runErr.Error()
		}
		rep.Scopes = append(rep.Scopes, result)
		rep.Quarantined += result.Quarantined
		rep.Purged += result.Purged
	}

	rep.ReadBytes = budget.Read
	rep.DiskFreeAfter = r.diskFree()
	_ = r.Store.MetaSet(ctx, "last_run_ns", strconv.FormatInt(nowNS, 10))
	_ = r.advanceCursor(ordered)
	return rep, nil
}

func (r *Runner) runScope(ctx context.Context, sc store.Scope, budget *domain.Budget, clockTrusted bool) (ScopeRun, error) {
	sr := ScopeRun{Scope: sc.ID}

	sctx, err := execute.OpenScopeCtx(ctx, r.Registrar, sc.ID)
	if err != nil {
		return sr, err
	}
	defer sctx.Close()

	guard, err := BuildGuard(ctx, r.Store, sctx)
	if err != nil {
		return sr, err
	}

	if clockTrusted {
		purged, bytes, due, perr := r.purgeDue(ctx, sctx, budget)
		sr.Purged, sr.PurgedBytes, sr.DueNotAuthorised = purged, bytes, due
		if perr != nil {
			return sr, perr
		}
	}

	stats, complete, err := ScanScope(ctx, r.Store, r.Clock, r.Config.Runtime.ScanBatchSize, sctx, guard, budget)
	if err != nil {
		return sr, err
	}
	sr.Scanned, sr.ScanComplete = stats.Files, complete

	q, qb, err := r.quarantineGrants(ctx, sctx, guard, budget)
	sr.Quarantined, sr.QuarantinedBytes = q, qb
	if err != nil {
		return sr, err
	}
	return sr, nil
}

// purgeDue creates one scheduled purge plan for the objects that are due.
// Objects without an active auto-purge grant are counted and left alone.
func (r *Runner) purgeDue(ctx context.Context, sc *execute.ScopeCtx, budget *domain.Budget) (int, int64, int, error) {
	nowNS := r.Clock.Wall().UnixNano()
	maxItems := r.Config.Runtime.MaxAutoPurgeFiles
	if maxItems == 0 || r.Config.Runtime.MaxAutoPurgeBytes == 0 {
		return 0, 0, 0, nil
	}
	due, err := r.Store.ItemsDueForPurge(ctx, sc.Scope.ID, nowNS, maxItems)
	if err != nil {
		return 0, 0, 0, err
	}

	authorised := make([]store.Item, 0, len(due))
	refused := 0
	var totalBytes int64
	for _, it := range due {
		if it.AutoPurgeGrantID == nil || it.SourceKind != domain.SourceScheduledRule {
			refused++
			continue
		}
		g, err := r.Store.GetGrant(ctx, *it.AutoPurgeGrantID)
		if err != nil || g.Status != domain.GrantActive {
			refused++
			continue
		}
		size := itemSize(it)
		if totalBytes+size > r.Config.Runtime.MaxAutoPurgeBytes {
			continue
		}
		totalBytes += size
		authorised = append(authorised, it)
	}
	if len(authorised) == 0 {
		return 0, 0, refused, nil
	}

	purgeBudget := budget.SplitFor(1, 2)
	p, err := plan.CreatePurgePlan(ctx, r.Store, sc.Scope, authorised,
		domain.PlanScheduledPurge, nowNS, int64(plan.DefaultTTL))
	if err != nil {
		return 0, 0, refused, err
	}
	res, err := r.Exec.ApplyPurgePlan(ctx, sc, p.ID, p.Digest, purgeBudget, true)
	if err != nil {
		return res.Completed, 0, refused, err
	}

	var bytes int64
	for i, out := range res.Results {
		if out.Code == string(errs.CodeOK) && i < len(authorised) {
			bytes += itemSize(authorised[i])
		}
	}
	return res.Completed, bytes, refused, nil
}

// ScanScope runs one scanning batch and reports whether the epoch completed.
//
// Retiring unseen entries happens only after a complete epoch: a partial pass
// cannot tell "not scanned yet" from "deleted".
func ScanScope(ctx context.Context, st *store.Store, clock domain.Clock, batch int,
	sc *execute.ScopeCtx, guard *policy.Guard, budget *domain.Budget) (scan.Stats, bool, error) {

	epoch, err := CurrentEpoch(ctx, st, sc.Scope.ID)
	if err != nil {
		return scan.Stats{}, false, err
	}
	scanner := scan.New(st, clock, guard, batch)
	stats, err := scanner.Run(ctx, scope.VerifiedScope{
		Scope:  sc.Scope,
		Root:   sc.Root,
		RootID: domain.RootIdentity{Runtime: domain.RuntimeMountIdentity{Device: sc.RootDev}},
	}, epoch, budget)
	if err != nil {
		return stats, false, err
	}

	state, err := st.ScanQueueStateOf(ctx, sc.Scope.ID, epoch)
	if err != nil {
		return stats, false, err
	}
	if state.Pending > 0 {
		return stats, false, nil
	}
	// Only a completed epoch may retire unseen entries.
	if _, err := st.MarkStaleObservations(ctx, sc.Scope.ID, epoch); err != nil {
		return stats, false, err
	}
	return stats, true, nil
}

// CurrentEpoch resumes an unfinished scan epoch, or starts a new one.
func CurrentEpoch(ctx context.Context, st *store.Store, id domain.ScopeID) (int64, error) {
	sc, err := st.GetScope(ctx, id)
	if err != nil {
		return 0, err
	}
	if sc.ScanEpoch > 0 {
		state, err := st.ScanQueueStateOf(ctx, id, sc.ScanEpoch)
		if err != nil {
			return 0, err
		}
		if state.Pending > 0 {
			if _, err := st.RequeueActive(ctx, id, sc.ScanEpoch); err != nil {
				return 0, err
			}
			return sc.ScanEpoch, nil
		}
	}
	return st.StartScanEpoch(ctx, id)
}

// quarantineGrants applies every active rule that holds an auto-quarantine
// grant, one plan per rule.
func (r *Runner) quarantineGrants(ctx context.Context, sc *execute.ScopeCtx, guard *policy.Guard,
	budget *domain.Budget) (int, int64, error) {

	allRules, err := r.Store.ListRules(ctx, sc.Scope.ID)
	if err != nil {
		return 0, 0, err
	}
	planner := plan.Planner{Store: r.Store, Clock: r.Clock}

	total, bytes := 0, int64(0)
	for _, rule := range allRules {
		if rule.Status != domain.RuleActive {
			continue
		}
		qg, err := r.Store.ActiveGrant(ctx, sc.Scope.ID, rule.ID, rule.Revision, domain.CapAutoQuarantine)
		if err != nil {
			continue
		}
		pg, pgErr := r.Store.ActiveGrant(ctx, sc.Scope.ID, rule.ID, rule.Revision, domain.CapAutoPurge)

		parsed, err := rules.Parse([]byte(rule.DSLJSON), map[string]bool{string(sc.Scope.ID): true})
		if err != nil {
			continue
		}

		maxFiles := minInt(r.Config.Runtime.MaxAutoQuarantineFiles, grantFiles(qg))
		maxBytes := minI64(r.Config.Runtime.MaxAutoQuarantineBytes, grantBytes(qg))
		if maxFiles <= 0 || maxBytes <= 0 {
			continue
		}

		res, err := planner.CreateRulePlan(ctx, plan.RulePlanRequest{
			Scope:           sc.Scope,
			Rule:            parsed,
			RuleRef:         domain.RuleRef{ID: rule.ID, Revision: rule.Revision, Digest: rule.Digest},
			Kind:            domain.PlanScheduledRule,
			Guard:           guard,
			MaxFiles:        maxFiles,
			MaxBytes:        maxBytes,
			QuarantineGrant: &qg,
			PurgeGrant:      pickGrant(pg, pgErr),
			Config:          r.Config,
			NowNS:           r.Clock.Wall().UnixNano(),
		})
		if err != nil {
			if errs.CodeOf(err) == errs.CodeNoCandidates {
				continue
			}
			return total, bytes, err
		}

		applied, err := r.Exec.ApplyQuarantinePlan(ctx, sc, res.Plan.ID, res.Plan.Digest, budget)
		total += applied.Completed
		for i, out := range applied.Results {
			if out.Code == string(errs.CodeOK) && i < len(res.Items) {
				bytes += itemSize(res.Items[i])
			}
		}
		if err != nil {
			return total, bytes, err
		}
	}
	return total, bytes, nil
}

// BuildGuard resolves protection identities with the kernel.
//
// A protection recorded as a path is only meaningful once the kernel says which
// object currently sits there. Comparing strings would miss the APFS case alias
// where "Keep/x.zip" and "keep/x.zip" are the same file.
func BuildGuard(ctx context.Context, st *store.Store, sc *execute.ScopeCtx) (*policy.Guard, error) {
	ps, err := st.ListProtections(ctx, sc.Scope.ID, true)
	if err != nil {
		return nil, err
	}
	resolved := make([]policy.ResolvedProtection, 0, len(ps))
	for _, p := range ps {
		rp := policy.ResolvedProtection{RawRelative: p.RawRelative, MatchKind: p.MatchKind, Source: p.Source}
		if parent, perr := sc.Root.OpenParent(p.RawRelative); perr == nil {
			if id, _, serr := parent.StatAtNoFollow(); serr == nil {
				rp.Identity = id
			}
			parent.Close()
		}
		resolved = append(resolved, rp)
	}
	return policy.NewGuard(resolved), nil
}

// clockTrust compares this run with the last good run and with its own
// monotonic clock. Suspicion only ever narrows what the run may do.
func (r *Runner) clockTrust(ctx context.Context, nowNS int64) (bool, string) {
	lastStr, err := r.Store.MetaGet(ctx, "last_run_ns")
	if err == nil && lastStr != "" {
		if last, perr := strconv.ParseInt(lastStr, 10, 64); perr == nil {
			if nowNS < last-clockBackwardsTolerance {
				return false, "the wall clock moved backwards since the last run; purging is suspended"
			}
			if nowNS-last > clockJumpThreshold {
				ackStr, _ := r.Store.MetaGet(ctx, "clock_ack_ns")
				ack, _ := strconv.ParseInt(ackStr, 10, 64)
				if ack <= last {
					return false, "more than 48 hours passed between runs; run doctor clock acknowledge before purging"
				}
			}
		}
	}
	if drift := (nowNS - r.startWallNS()) - int64(r.Clock.Mono()); drift > clockDriftTolerance {
		return false, "the wall clock advanced far more than the monotonic clock during this run"
	}
	return true, ""
}

func (r *Runner) startWallNS() int64 { return r.Clock.Wall().UnixNano() - int64(r.Clock.Mono()) }

// ClockTrust reports whether the wall clock is trustworthy enough to purge.
//
// It is exported because a manual `purge apply` has the same clock requirement
// as the background pass (section 10.4 lists "time is trustworthy" among the
// conditions re-checked for every purge). Only the automatic path used to apply
// it, which meant a manual purge could run on a clock that had just jumped.
func ClockTrust(ctx context.Context, st *store.Store, clock domain.Clock) (bool, string) {
	r := &Runner{Store: st, Clock: clock}
	return r.clockTrust(ctx, clock.Wall().UnixNano())
}

func (r *Runner) rotate(scopes []store.Scope) []store.Scope {
	cursor, _ := r.Store.MetaGet(context.Background(), "rotation_cursor")
	if cursor == "" {
		return scopes
	}
	idx := -1
	for i, sc := range scopes {
		if string(sc.ID) == cursor {
			idx = i
			break
		}
	}
	if idx < 0 {
		return scopes
	}
	out := make([]store.Scope, 0, len(scopes))
	out = append(out, scopes[idx:]...)
	out = append(out, scopes[:idx]...)
	return out
}

func (r *Runner) advanceCursor(scopes []store.Scope) error {
	if len(scopes) == 0 {
		return nil
	}
	next := scopes[0].ID
	for i, sc := range scopes {
		if string(sc.ID) == string(next) {
			next = scopes[(i+1)%len(scopes)].ID
			break
		}
	}
	return r.Store.MetaSet(context.Background(), "rotation_cursor", string(next))
}

func (r *Runner) diskFree() uint64 {
	scopes, err := r.Store.ListScopes(context.Background())
	if err != nil {
		return 0
	}
	var best uint64
	for _, sc := range scopes {
		var st unix.Statfs_t
		if err := unix.Statfs(string(sc.RawRoot), &st); err != nil {
			continue
		}
		free := uint64(st.Bavail) * uint64(st.Bsize)
		if free > best {
			best = free
		}
	}
	return best
}

func itemSize(it store.Item) int64 {
	var tok domain.ChangeToken
	if err := json.Unmarshal([]byte(it.PreTokenJSON), &tok); err != nil {
		return 0
	}
	return tok.Size
}

func grantFiles(g store.Grant) int {
	var b plan.GrantBindingV1
	if err := json.Unmarshal([]byte(g.BindingJSON), &b); err != nil {
		return 0
	}
	return b.ApprovedMaxFiles
}

func grantBytes(g store.Grant) int64 {
	var b plan.GrantBindingV1
	if err := json.Unmarshal([]byte(g.BindingJSON), &b); err != nil {
		return 0
	}
	return b.ApprovedMaxLogicalBytes
}

func pickGrant(g store.Grant, err error) *store.Grant {
	if err != nil || g.ID == "" {
		return nil
	}
	return &g
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
