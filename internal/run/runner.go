// Package run performs one bounded background pass.
//
// It lives outside internal/execute because it composes execution and
// reconciliation, and reconciliation already depends on execution. Keeping the
// pipeline in its own package preserves that direction.
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/clocktrust"
	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/notify"
	"dustkeeper/internal/plan"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/recovery"
	"dustkeeper/internal/rules"
	"dustkeeper/internal/scan"
	"dustkeeper/internal/scope"
	"dustkeeper/internal/store"
)

// Runner performs one bounded run.
type Runner struct {
	Store      *store.Store
	Clock      domain.Clock
	Config     config.Config
	Registrar  *scope.Registrar
	Exec       *execute.Executor
	Reconciler *recovery.Reconciler
	// Notify is the one channel the product tells the user through.
	//
	// It is required for automatic clearing and may be nil only for an
	// installation that never clears anything: a nil channel reports itself as
	// "not configured", and an object whose reminder could not be delivered is
	// left in the observation area. A nil default that quietly succeeded is
	// exactly the shape R27 forbids.
	Notify notify.Sender
}

// ScopeRun reports what happened to one scope.
type ScopeRun struct {
	Scope   domain.ScopeID `json:"scope_id"`
	Skipped string         `json:"skipped,omitempty"`
	// PurgeSkipped and QuarantineSkipped are separate because the two phases no
	// longer share one gate: disk pressure stops new quarantine but must not
	// stop a purge that is already due and authorised.
	PurgeSkipped      string `json:"purge_skipped,omitempty"`
	QuarantineSkipped string `json:"quarantine_skipped,omitempty"`
	Scanned           int    `json:"scanned_files"`
	ScanComplete      bool   `json:"scan_epoch_complete"`
	ScanInterrupted   bool   `json:"scan_interrupted"`
	DeepScanRequired  bool   `json:"deep_scan_required"`
	Quarantined       int    `json:"quarantined"`
	QuarantinedBytes  int64  `json:"quarantined_logical_bytes"`
	Purged            int    `json:"purged"`
	PurgedBytes       int64  `json:"purged_logical_bytes"`
	DueNotAuthorised  int    `json:"due_but_not_authorised"`
	// ExpirySkipped names the due objects the expiry review refused, with the
	// reason each one was refused. It is a list rather than a count because "three
	// objects were skipped" cannot be acted on: "the place is in use again" and
	// "the object is protected" call for different decisions.
	ExpirySkipped []string `json:"expiry_skipped,omitempty"`
	// Notification is what the summary message achieved at the end of this
	// scope's work. A delivery that failed is reported as failed, and the counts
	// above stand on their own: the message is how the user hears about them, not
	// what makes them true.
	Notification *notify.Result `json:"notification,omitempty"`
	IdentityWeak string         `json:"root_identity_weak,omitempty"`
	Control           string `json:"control,omitempty"`
	Message           string `json:"message,omitempty"`
	// FailureCode names why this scope's work stopped, on the scope's own row.
	// The run-level failure code only carries the first one, so with two failing
	// scopes the second reason had nowhere to appear.
	FailureCode errs.Code `json:"failure_code,omitempty"`
}

// Report is the machine-readable result of `run`.
type Report struct {
	RunID           string          `json:"run_id"`
	ClockTrusted    bool            `json:"clock_trusted"`
	ClockNote       string          `json:"clock_note,omitempty"`
	Reconciliation  recovery.Report `json:"reconciliation"`
	Scopes          []ScopeRun      `json:"scopes"`
	Quarantined     int             `json:"quarantined_total"`
	Purged          int             `json:"purged_total"`
	ReadBytes       int64           `json:"read_bytes"`
	DiskFreeBefore  uint64          `json:"disk_free_bytes_before"`
	DiskFreeAfter   uint64          `json:"disk_free_bytes_after"`
	DiskPressure    bool            `json:"disk_pressure"`
	PressureNote    string          `json:"disk_pressure_note,omitempty"`
	Control         string          `json:"control"`
	UntouchedScopes int             `json:"scopes_not_reached"`
	// ClockReassessed says the verdict changed between the start and the end of
	// the run. It is reported so that "the clock looked fine at 09:00" cannot be
	// read as "the clock was fine all morning".
	ClockReassessed bool `json:"clock_reassessed"`
	// Failures lists the scopes whose work did not finish because of a real
	// error. A budget stop is not one: it is reported as control plus a note, so
	// that a scheduler can tell "ran out of its slice" from "could not write".
	Failures    []string  `json:"failures,omitempty"`
	FailureCode errs.Code `json:"failure_code,omitempty"`
}

// Run executes one pass.
//
// Order: reconcile, then purge what is due and authorised, then scan, then
// quarantine. Purging first prevents a scope that keeps producing garbage from
// starving objects that are already due.
//
// Resource accounting is three ledgers, not one: a monotonic run ledger for time
// and reads, and one action ledger per action class shared by every scope and
// rule. That is what makes "several rules must not each use the full limit"
// true, and what makes a zero limit mean "forbidden" rather than "unlimited".
func (r *Runner) Run(ctx context.Context) (Report, error) {
	rep := Report{RunID: domain.NewID("run"), Control: domain.ControlContinue}
	cfg := r.Config.Runtime
	rep.DiskFreeBefore = r.diskFree()

	guard := clocktrust.Start(r.Store, r.Clock)
	verdict, err := guard.Assess(ctx)
	if err != nil {
		return rep, err
	}
	rep.ClockTrusted = verdict.Trusted
	rep.ClockNote = verdict.Reason

	rec, err := r.Reconciler.Run(ctx)
	rep.Reconciliation = rec
	if err != nil {
		return rep, err
	}
	// A reconciliation that could not write down what it decided has not
	// finished, and it says so in RecordFailures rather than with a Go error.
	// The objects are where that pass left them and their pending markers are
	// intact, so planning new work on top of an unknown bookkeeping state is how
	// a second attempt gets scheduled against a scope whose first outcome was
	// never recorded. Read the counter, or the run calls itself a success.
	bookkeepingBroken := rec.RecordFailures > 0
	if bookkeepingBroken {
		rep.Control = domain.ControlStopRun
		rep.FailureCode = errs.CodeStateWriteFailed
		rep.Failures = append(rep.Failures, fmt.Sprintf(
			"reconcile: %d outcome(s) could not be recorded, so no scope was started; the objects were left as they are and their pending markers are intact",
			rec.RecordFailures))
	}

	scopes, err := r.Store.ListScopes(ctx)
	if err != nil {
		return rep, err
	}
	ordered := r.rotate(scopes)

	runLedger := domain.NewRunLedger(cfg.RunSoftSeconds, cfg.RunReadBudgetBytes, cfg.ReadRateBytesPerSecond)
	qLedger := domain.NewActionLedger(cfg.MaxAutoQuarantineFiles, cfg.MaxAutoQuarantineBytes)
	pLedger := domain.NewActionLedger(cfg.MaxAutoPurgeFiles, cfg.MaxAutoPurgeBytes)

	if bookkeepingBroken {
		// Every schedulable scope is named, not silently omitted: a scheduler that
		// sees no rows reads that as "nothing to do" rather than "the pass never
		// started".
		for _, sc := range ordered {
			if sc.Kind == domain.KindReference {
				continue
			}
			rep.Scopes = append(rep.Scopes, ScopeRun{
				Scope:       sc.ID,
				Control:     domain.ControlStopRun,
				Skipped:     "reconciliation could not record its outcomes",
				FailureCode: errs.CodeStateWriteFailed,
			})
			rep.UntouchedScopes++
		}
	}

	for _, sc := range ordered {
		// The guard sits at the top so that the loop below is byte-for-byte the
		// normal path: one reason to start nothing new, and no second way to
		// decide that a scope was skipped.
		if bookkeepingBroken {
			break
		}
		if ctx.Err() != nil {
			rep.Control = domain.ControlStopRun
			break
		}
		if sc.Kind == domain.KindReference {
			continue
		}
		sr := ScopeRun{Scope: sc.ID, Control: domain.ControlContinue}
		if sc.Status != domain.ScopeActive {
			sr.Skipped = "scope is " + sc.Status
			rep.Scopes = append(rep.Scopes, sr)
			continue
		}
		if runLedger.TimeUp(r.Clock.Mono()) || runLedger.ReadUp() {
			sr.Skipped = "run budget reached"
			rep.UntouchedScopes++
			rep.Scopes = append(rep.Scopes, sr)
			continue
		}

		result, runErr := r.runScope(ctx, sc, runLedger, qLedger, pLedger, guard, verdict)
		if runErr != nil {
			result.Message = runErr.Error()
			result.FailureCode = errs.CodeOf(runErr)
			rep.Failures = append(rep.Failures, string(sc.ID)+": "+runErr.Error())
			if rep.FailureCode == "" {
				rep.FailureCode = errs.CodeOf(runErr)
			}
			rep.Control = strongerControl(rep.Control, ControlOfError(runErr))
		}
		// The one message the user gets about this scope. It is sent after the
		// work, from the same numbers the report carries, so "what the user was
		// told" and "what the record says" cannot drift apart.
		r.tellUser(ctx, &result)
		rep.Scopes = append(rep.Scopes, result)
		rep.Quarantined += result.Quarantined
		rep.Purged += result.Purged
		rep.Control = strongerControl(rep.Control, result.Control)
		if rep.Control == domain.ControlStopRun {
			break
		}
	}

	rep.ReadBytes = runLedger.Read
	rep.DiskFreeAfter = r.diskFree()
	if pressure, note := r.diskPressure(); pressure {
		rep.DiskPressure = true
		rep.PressureNote = note
	}
	// Re-assess before recording. The verdict captured at the start describes the
	// start; a clock that jumped during the run is exactly the case the start
	// verdict cannot see, and recording the start verdict would write the
	// anomalous wall clock into the trusted baseline.
	final, ferr := guard.Assess(ctx)
	if ferr != nil {
		return rep, ferr
	}
	if final.Trusted != verdict.Trusted || final.Reason != verdict.Reason {
		rep.ClockTrusted = final.Trusted
		rep.ClockNote = final.Reason
		rep.ClockReassessed = true
	}
	// Recording the run is deliberately last: a run that could not finish its
	// bookkeeping must not advance the trusted clock baseline.
	//
	// A failed pass in the sense of RecordFailures does not skip this, though.
	// That pass still observed the wall clock, and an untrusted verdict has to be
	// latched here - dropping the record because unrelated outcome writes failed
	// would lose the anomaly and leave the next run free to start from a fresh
	// baseline. What it may not do is call itself a success: that is FailureCode,
	// which the CLI turns into a non-zero exit.
	if err := guard.RecordRun(ctx, final); err != nil {
		return rep, err
	}
	_ = r.advanceCursor(ordered)
	return rep, nil
}

func (r *Runner) runScope(ctx context.Context, sc store.Scope, runLedger *domain.RunLedger,
	qLedger, pLedger *domain.ActionLedger, guard *clocktrust.Guard, verdict clocktrust.Verdict) (ScopeRun, error) {

	sr := ScopeRun{Scope: sc.ID, Control: domain.ControlContinue}

	sctx, err := execute.OpenScopeCtx(ctx, r.Registrar, sc.ID)
	if err != nil {
		return sr, err
	}
	defer sctx.Close()

	guardSet, err := BuildGuard(ctx, r.Store, sctx)
	if err != nil {
		return sr, err
	}

	// One decision, two consequences.
	//
	// A suspect clock or thin root evidence stops both new permanent deletion and
	// new quarantine: an object created under a clock that cannot be proven has a
	// retention window nobody can defend later. Disk pressure is a different
	// thing - it stops new quarantine, because quarantining copies bytes onto a
	// full volume, and it does not stop a purge, because the purge is precisely
	// what frees the space.
	pressure, pressureNote := r.diskPressure()
	blocked := ""
	switch {
	case !verdict.Trusted:
		blocked = "clock is not trustworthy: " + verdict.Reason
	case sctx.IdentityWeak != "":
		// R02: thin root evidence is enough to keep observing but not enough to
		// start an automatic action. Nothing is lost by waiting; re-registering
		// restores the automatic path.
		blocked = "root identity evidence is too thin for automatic actions: " + sctx.IdentityWeak
		sr.IdentityWeak = sctx.IdentityWeak
	}

	if blocked != "" {
		sr.Skipped = blocked
		sr.PurgeSkipped = blocked
		sr.QuarantineSkipped = blocked
	} else {
		purge := r.purgeDue(ctx, sctx, runLedger, pLedger, guard)
		sr.Purged, sr.PurgedBytes, sr.DueNotAuthorised = purge.Purged, purge.Bytes, purge.Refused
		sr.ExpirySkipped = purge.Skipped
		if purge.Note != "" {
			sr.PurgeSkipped = purge.Note
		}
		sr.Control = strongerControl(sr.Control, purge.Control)
		if purge.Err != nil {
			return sr, purge.Err
		}
		if purge.Control == domain.ControlStopScope || purge.Control == domain.ControlStopRun {
			return sr, nil
		}
	}

	stats, scanState, err := ScanScope(ctx, r.Store, r.Clock, r.Config.Runtime.ScanBatchSize,
		sctx, guardSet, runLedger)
	if err != nil {
		return sr, err
	}
	sr.Scanned = stats.Files
	sr.ScanComplete = scanState.Complete
	sr.ScanInterrupted = scanState.Interrupted
	sr.DeepScanRequired = scanState.DeepScanRequired

	if blocked != "" {
		sr.Message = "no file was moved: " + blocked
		return sr, nil
	}
	if pressure {
		sr.QuarantineSkipped = "disk pressure: " + pressureNote
		sr.Message = "new quarantine paused: " + pressureNote
		return sr, nil
	}
	// The scan may have spent the time slice. Starting a quarantine after it has
	// would be a new business action past the deadline.
	if runLedger.TimeUp(r.Clock.Mono()) {
		sr.QuarantineSkipped = "run time slice spent during the scan"
		return sr, nil
	}
	// An unfinished scan is a partial picture of the scope, and a rule evaluated
	// against a partial picture is a rule evaluated against the wrong tree: the
	// files it has not reached are not "not matching", they are unknown. Due
	// purges were already handled above and a restore never depends on a scan, so
	// only new quarantine waits for the next round.
	if !scanState.Complete {
		if scanState.FailedDirs > 0 {
			sr.QuarantineSkipped = fmt.Sprintf(
				"the scan of this scope is not complete: %d directory(ies) could not be read", scanState.FailedDirs)
		} else {
			sr.QuarantineSkipped = "the scan of this scope is not complete yet"
		}
		sr.Message = sr.QuarantineSkipped
		return sr, nil
	}

	q, qb, control, err := r.quarantineGrants(ctx, sctx, guardSet, runLedger, qLedger)
	sr.Quarantined, sr.QuarantinedBytes = q, qb
	sr.Control = strongerControl(sr.Control, control)
	return sr, err
}

// purgeOutcome is what one scope's clearing phase produced.
//
// It is a struct rather than five return values because a run report has to
// explain itself: the count of objects that were left alone and the reasons they
// were left alone belong to the same answer.
type purgeOutcome struct {
	Purged  int
	Bytes   int64
	Refused int
	Skipped []string
	// Note explains a phase-level refusal, such as a reminder that could not be
	// delivered. It is not a per-object reason, so it is not in Skipped.
	Note    string
	Control string
	Err     error
}

// purgeDue creates one scheduled purge plan for the objects that are due.
//
// The queue is read with a keyset cursor and a bounded page size. Objects that
// cannot be processed automatically are counted and skipped, and the scan
// continues: a first page full of un-authorised objects used to hide every
// authorised object behind it, every run, for ever.
//
// Each candidate passes the expiry review before it is counted against any
// allowance. That review is what makes "this object was due and was left alone"
// an answer with a reason rather than a silent subtraction from the budget.
func (r *Runner) purgeDue(ctx context.Context, sc *execute.ScopeCtx, runLedger *domain.RunLedger,
	ledger *domain.ActionLedger, guard *clocktrust.Guard) purgeOutcome {

	cfg := r.Config.Runtime
	if cfg.MaxAutoPurgeFiles <= 0 || cfg.MaxAutoPurgeBytes <= 0 {
		return purgeOutcome{Control: domain.ControlContinue}
	}

	nowNS := r.Clock.Wall().UnixNano()
	pageSize := cfg.MaxAutoPurgeFiles
	if pageSize > 200 {
		pageSize = 200
	}

	var (
		authorised []store.Item
		refused    int
		skipped    []string
		totalBytes int64
		afterDue   int64
		afterItem  string
	)

	// The run ledger is a ceiling for the whole run; a grant is a ceiling for one
	// approval. Both apply, and the smaller one wins. Spending ten because the
	// configuration allows ten, when the user approved one for this grant, is how
	// "the approved maximum" turned into a number nobody enforced.
	quotas := map[domain.GrantID]grantQuota{}
	spent := map[domain.GrantID]*grantSpend{}

collect:
	for len(authorised) < cfg.MaxAutoPurgeFiles {
		if runLedger.TimeUp(r.Clock.Mono()) {
			break
		}
		page, err := r.Store.PurgeQueuePage(ctx, sc.Scope.ID, nowNS, afterDue, afterItem, pageSize)
		if err != nil {
			return purgeOutcome{Refused: refused, Skipped: skipped, Control: domain.ControlStopRun, Err: err}
		}
		if len(page) == 0 {
			break
		}
		for _, it := range page {
			afterDue = purgeDueKey(it)
			afterItem = string(it.ID)
			quota, ok, authorityErr := r.automaticAuthority(ctx, it, quotas)
			if authorityErr != nil {
				return purgeOutcome{Refused: refused, Skipped: skipped, Control: ControlOfError(authorityErr), Err: authorityErr}
			}
			if !ok {
				refused++
				continue
			}
			// The expiry review comes next, and still before any allowance is
			// counted. An object that must not be cleared - restored, protected, a
			// place in use again, a vault object that changed - leaves with a
			// reason and costs the run nothing.
			//
			// It runs after the authority check on purpose: the authority is a
			// database read that can fail in a way the run has to hear about, and
			// a review that ran first would hide that failure behind a refusal
			// about the object.
			if why, rerr := r.Exec.ReviewExpiry(ctx, sc, it); rerr != nil {
				return purgeOutcome{Refused: refused, Skipped: skipped, Control: ControlOfError(rerr), Err: rerr}
			} else if why != "" {
				refused++
				skipped = append(skipped, string(it.ID)+": "+why)
				continue
			}
			size := itemSize(it)
			if totalBytes+size > cfg.MaxAutoPurgeBytes {
				refused++
				continue
			}
			if gid := grantIDOf(it); gid != "" {
				used := spent[gid]
				if used == nil {
					used = &grantSpend{}
					spent[gid] = used
				}
				if used.files+1 > quota.files || used.bytes+size > quota.bytes {
					// This grant has spent what it approved. Another object under
					// the same grant waits for the next run, when a new plan is
					// approved against the same ceiling.
					refused++
					continue
				}
				used.files++
				used.bytes += size
			}
			if !ledger.CanReserve(size) {
				// The run's purge allowance is spent. Stop looking: the ledger
				// cannot grow again in this run.
				break collect
			}
			totalBytes += size
			authorised = append(authorised, it)
			if len(authorised) >= cfg.MaxAutoPurgeFiles {
				break collect
			}
		}
	}

	if len(authorised) == 0 {
		return purgeOutcome{Refused: refused, Skipped: skipped, Control: domain.ControlContinue}
	}

	// The reminder before an irreversible act, and the gate it carries.
	//
	// R27 is explicit that "the log says so" is not notification, so the only
	// thing that lets the clearing proceed is a delivery the channel reported as
	// successful. When it was not, nothing is cleared: the objects stay in the
	// observation area, which costs the user nothing but a later date, and the
	// run says why.
	if note, ok := r.remindBeforeClearing(ctx, authorised); !ok {
		return purgeOutcome{Refused: refused, Skipped: skipped, Note: note, Control: domain.ControlContinue}
	}

	p, err := plan.CreatePurgePlan(ctx, r.Store, sc.Scope, authorised,
		domain.PlanScheduledPurge, nowNS, int64(plan.DefaultTTL))
	if err != nil {
		return purgeOutcome{Refused: refused, Skipped: skipped, Control: domain.ControlStopScope, Err: err}
	}

	// The purge phase may spend at most what is left in the shared purge ledger,
	// and its reads land in the same run ledger as everything else.
	phase := domain.NewPhaseBudget(runLedger, ledger, ledger.RemainingFiles(), ledger.RemainingBytes())
	res, err := r.Exec.ApplyPurgePlan(ctx, sc, p.ID, p.Digest, phase, guard.Assert)
	if err != nil {
		return purgeOutcome{Purged: res.Completed, Refused: refused, Skipped: skipped,
			Control: ControlOfError(err), Err: err}
	}

	var bytes int64
	for _, out := range res.Results {
		if out.Code != string(errs.CodeOK) {
			continue
		}
		for _, it := range authorised {
			if it.ID == out.Item {
				bytes += itemSize(it)
				break
			}
		}
	}
	if stopErr := batchStop(res); stopErr != nil {
		return purgeOutcome{Purged: res.Completed, Bytes: bytes, Refused: refused, Skipped: skipped,
			Control: strongerControl(res.Control, ControlOfError(stopErr)), Err: stopErr}
	}
	return purgeOutcome{Purged: res.Completed, Bytes: bytes, Refused: refused, Skipped: skipped,
		Control: res.Control}
}

// tellUser sends the one message the product promises after a scope's work, and
// records what it achieved.
//
// The text is built from the numbers on the report row, so the message and the
// record are the same fact told twice rather than two calculations that can
// disagree. Nothing is sent for a run that touched nothing: a message that says
// "0 deleted, 0 moved" every morning is how a user learns to ignore the one that
// matters.
func (r *Runner) tellUser(ctx context.Context, sr *ScopeRun) {
	if sr == nil || (sr.Quarantined == 0 && sr.Purged == 0) {
		return
	}
	body := summaryMessage(sr.Purged, sr.Quarantined, r.observationDays(ctx))
	res := r.deliver(ctx, body)
	sr.Notification = &res
	// The outcome is written down whichever way it went. A record that only exists
	// when delivery worked cannot answer "was the user told", which is the only
	// question anyone asks of it.
	if err := notify.Record(ctx, r.Store, r.Clock.Wall().UnixNano(), body, res); err != nil {
		sr.Message = joinNote(sr.Message, "the notification outcome could not be recorded: "+err.Error())
	}
	if !res.Delivered {
		sr.Message = joinNote(sr.Message, "the user was not told: "+res.Detail)
	}
}

// remindBeforeClearing sends the short warning that precedes a permanent
// deletion, and reports whether the clearing may proceed.
//
// A user who turned notifications off in the confirmed policy has already said
// they do not want to be told, so the reminder is not a gate for them; what
// remains is the details page, which is still written. Everyone else gets the
// gate: no delivery, no deletion this run.
func (r *Runner) remindBeforeClearing(ctx context.Context, due []store.Item) (string, bool) {
	pol, err := cleanup.Load(ctx, r.Store)
	if err != nil {
		return "notify_unchecked: the cleanup policy could not be read, so the reminder could not be decided: " + err.Error(), false
	}
	if pol.Notify == cleanup.NotifyNone {
		return "", true
	}
	var bytes int64
	for _, it := range due {
		bytes += itemSize(it)
	}
	body := fmt.Sprintf("%d 个文件已到期，本次将永久删除（%s）。删除前可在详情页查看并恢复。",
		len(due), humanBytes(bytes))
	res := r.deliver(ctx, body)
	if err := notify.Record(ctx, r.Store, r.Clock.Wall().UnixNano(), body, res); err != nil {
		return "notify_unrecorded: the reminder was sent but could not be recorded, so it cannot be shown to have been delivered: " + err.Error(), false
	}
	if !res.Delivered {
		return "notify_failed: " + res.Detail + "; nothing was cleared this run", false
	}
	return "", true
}

// deliver sends one message through the configured channel.
//
// A nil channel is not "the system one": it is "this installation has no way of
// telling anyone", and it reports itself as not delivered. Defaulting a nil to a
// real channel would make an installation that was never configured look like one
// that delivered.
func (r *Runner) deliver(ctx context.Context, body string) notify.Result {
	if r.Notify == nil {
		return notify.Result{Attempted: false, Delivered: false,
			Detail: "no notification channel is configured"}
	}
	return r.Notify.Send(ctx, "Dustkeeper", body)
}

// observationDays is how long a newly isolated object is observed, for the
// message text. A policy that cannot be read yields zero, which the message
// omits rather than guessing at.
func (r *Runner) observationDays(ctx context.Context) int {
	pol, err := cleanup.Load(ctx, r.Store)
	if err != nil {
		return 0
	}
	return pol.ObservationDays
}

// summaryMessage is the sentence R26 asks for: what was deleted, what was moved
// and when the earliest of them can go, and where to look.
func summaryMessage(deleted, observed, days int) string {
	parts := make([]string, 0, 3)
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("本次删除 %d 个文件", deleted))
	}
	if observed > 0 {
		if days > 0 {
			parts = append(parts, fmt.Sprintf("%d 个文件进入待删除区，最早 %d 天后清空", observed, days))
		} else {
			parts = append(parts, fmt.Sprintf("%d 个文件进入待删除区", observed))
		}
	}
	if len(parts) == 0 {
		return "本次没有文件被删除或移动。"
	}
	return strings.Join(parts, "；") + "。查看详情／恢复请打开本机结果页。"
}

// humanBytes renders a size for a one-line message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// joinNote appends one explanation to another without losing either.
func joinNote(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return existing + "; " + extra
}

// batchStop turns a stopped batch back into the reason it stopped.
//
// Apply reports a per-item stop condition as a code on the result, not as a Go
// error, so a runner that only checked err printed "control: stop_scope" with an
// empty message: an operator could see that something stopped but not what. The
// condition is turned back into an error only when it stops the scope or the run;
// a dead plan belongs to its own rule and must not stop the others.
func batchStop(res execute.ApplyResult) error {
	switch execute.ControlOf(res.Code) {
	case domain.ControlStopScope, domain.ControlStopRun:
		message := res.Message
		if message == "" {
			message = "the batch stopped with " + res.Code
		}
		return errs.Newf(errs.Code(res.Code), "%s", message)
	}
	return nil
}

// grantQuota is the ceiling one approval carries.
type grantQuota struct {
	files int
	bytes int64
}

// grantSpend is what this run has already spent under one approval.
type grantSpend struct {
	files int
	bytes int64
}

// grantIDOf returns the grant an item was authorised by, or "" when it has none.
func grantIDOf(it store.Item) domain.GrantID {
	if it.AutoPurgeGrantID == nil {
		return ""
	}
	return *it.AutoPurgeGrantID
}

// automaticAuthority re-checks the standing authority for one object and returns
// the ceilings that authority carries.
//
// Reading the binding here is the point: the purge path used to check only that
// a grant existed, not what it approved, so the number the user saw in the
// approval prompt never reached the executor.
func (r *Runner) automaticAuthority(ctx context.Context, it store.Item,
	cache map[domain.GrantID]grantQuota) (grantQuota, bool, error) {

	if it.AutoPurgeGrantID == nil || it.SourceKind != domain.SourceScheduledRule {
		return grantQuota{}, false, nil
	}
	id := *it.AutoPurgeGrantID
	if q, ok := cache[id]; ok {
		return q, q.files > 0 && q.bytes > 0, nil
	}
	g, err := r.Store.GetGrant(ctx, id)
	if err != nil {
		if errs.CodeOf(err) != errs.CodeGrantRequired {
			return grantQuota{}, false, err
		}
		cache[id] = grantQuota{}
		return grantQuota{}, false, nil
	}
	if g.Status != domain.GrantActive || g.Capability != domain.CapAutoPurge {
		cache[id] = grantQuota{}
		return grantQuota{}, false, nil
	}
	var b plan.GrantBindingV1
	if err := json.Unmarshal([]byte(g.BindingJSON), &b); err != nil {
		// An unreadable binding is not an unlimited approval.
		cache[id] = grantQuota{}
		return grantQuota{}, false, nil
	}
	q := grantQuota{files: b.ApprovedMaxFiles, bytes: b.ApprovedMaxLogicalBytes}
	cache[id] = q
	return q, q.files > 0 && q.bytes > 0, nil
}

// ScanState reports how complete one scanning batch was.
//
// Complete is the only outcome that permits retiring unseen entries. Every other
// field exists so the caller can say why the epoch is still open, without guessing
// from a single boolean:
//
//	Interrupted  the batch stopped at a safe boundary because its budget ran out
//	Incomplete   a directory could not be read (FailedDirs says how many)
//	Interrupts   consecutive rounds this scope has been left unfinished
//
// Incomplete became a separate field from Interrupted because the two used to be
// one: a failed directory was recorded as "done", so the run that failed it had
// nothing to report and the next run had nothing to resume.
type ScanState struct {
	Complete         bool  `json:"complete"`
	Interrupted      bool  `json:"interrupted"`
	Incomplete       bool  `json:"incomplete"`
	FailedDirs       int   `json:"failed_dirs"`
	DeepScanRequired bool  `json:"deep_scan_required"`
	Interrupts       int64 `json:"interrupts_consecutive"`
	// Pending and Epoch are reported so that "did this pass finish the tree?"
	// can be checked from the envelope alone: a complete epoch with pending work
	// is a contradiction, and a reviewer should not need the database to see it.
	Pending int64 `json:"queue_pending"`
	Epoch   int64 `json:"epoch"`
}

// ScanScope runs one scanning batch and reports whether the epoch completed.
//
// Retiring unseen entries happens only after a complete epoch: a partial pass
// cannot tell "not scanned yet" from "deleted". Interrupted is a first-class
// outcome, not a silent success: the previous code returned nil from a budget
// stop and then declared the epoch complete.
func ScanScope(ctx context.Context, st *store.Store, clock domain.Clock, batch int,
	sc *execute.ScopeCtx, guard *policy.Guard, runLedger *domain.RunLedger) (scan.Stats, ScanState, error) {

	epoch, err := CurrentEpoch(ctx, st, sc.Scope.ID)
	if err != nil {
		return scan.Stats{}, ScanState{}, err
	}
	scanner := scan.New(st, clock, guard, batch)
	stats, err := scanner.Run(ctx, scope.VerifiedScope{
		Scope:  sc.Scope,
		Root:   sc.Root,
		RootID: domain.RootIdentity{Runtime: domain.RuntimeMountIdentity{Device: sc.RootDev}},
	}, epoch, runLedger)
	if err != nil {
		return stats, ScanState{}, err
	}

	state, err := st.ScanQueueStateOf(ctx, sc.Scope.ID, epoch)
	if err != nil {
		return stats, ScanState{}, err
	}
	// A directory the filesystem refused is pending work, not finished work:
	// ScanQueueStateOf counts 'failed' rows in Pending. One condition therefore
	// covers both halves of R11 — work the budget did not reach, and work the
	// filesystem would not give up. Neither may retire an observation.
	if stats.Interrupted || state.Pending > 0 {
		// Keep the dirs that were not finished in the queue. The next run
		// resumes the same epoch instead of starting a new one that would never
		// reach the tail of a large directory.
		if _, err := st.RequeueActive(ctx, sc.Scope.ID, epoch); err != nil {
			return stats, ScanState{}, err
		}
		// The counter is the persistent half of "the last full pass did not
		// finish". The run that observed the interruption is gone by the next
		// one, and a report has to be able to say it without a live process.
		interrupts, err := st.BumpScanInterrupts(ctx, sc.Scope.ID)
		if err != nil {
			return stats, ScanState{}, err
		}
		return stats, ScanState{
			Interrupted:      stats.Interrupted,
			Incomplete:       state.Failed > 0,
			FailedDirs:       int(state.Failed),
			DeepScanRequired: state.Pending > 0,
			Interrupts:       interrupts,
			Pending:          state.Pending,
			Epoch:            epoch,
		}, nil
	}
	// Only a completed epoch may retire unseen entries.
	if _, err := st.MarkStaleObservations(ctx, sc.Scope.ID, epoch); err != nil {
		return stats, ScanState{}, err
	}
	if err := st.ResetScanInterrupts(ctx, sc.Scope.ID); err != nil {
		return stats, ScanState{}, err
	}
	return stats, ScanState{Complete: true, Epoch: epoch}, nil
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
	runLedger *domain.RunLedger, ledger *domain.ActionLedger) (int, int64, string, error) {

	allRules, err := r.Store.ListRules(ctx, sc.Scope.ID)
	if err != nil {
		return 0, 0, domain.ControlStopScope, err
	}
	planner := plan.Planner{Store: r.Store, Clock: r.Clock}

	total, bytes := 0, int64(0)
	control := domain.ControlContinue

	for _, rule := range allRules {
		if ctx.Err() != nil {
			return total, bytes, domain.ControlStopRun, nil
		}
		if control == domain.ControlStopScope || control == domain.ControlStopRun {
			return total, bytes, control, nil
		}
		// Planning and applying one rule is a new business action. Past the soft
		// deadline it does not start, and the rules after it do not either.
		if runLedger.TimeUp(r.Clock.Mono()) {
			return total, bytes, strongerControl(control, domain.ControlStopScope), nil
		}
		if rule.Status != domain.RuleActive {
			continue
		}
		// GRANT_REQUIRED is "this rule has no authority for this action", and it
		// is skipped: a rule with only an auto_quarantine grant is a normal setup.
		// Everything else - including the ambiguous multiple-active-grant refusal,
		// which carries RECOVERY_REQUIRED - is a record this scope cannot act on,
		// so the scope stops and the reason reaches the run report instead of
		// being read as "nothing to do here".
		qg, qerr := r.Store.ActiveGrant(ctx, sc.Scope.ID, rule.ID, rule.Revision, domain.CapAutoQuarantine)
		if qerr != nil {
			if errs.CodeOf(qerr) == errs.CodeGrantRequired {
				continue
			}
			return total, bytes, domain.ControlStopScope, qerr
		}
		if err := plan.CheckAutomaticGrant(qg, sc.Scope, rule, domain.CapAutoQuarantine); err != nil {
			return total, bytes, domain.ControlStopScope, err
		}
		pg, pgErr := r.Store.ActiveGrant(ctx, sc.Scope.ID, rule.ID, rule.Revision, domain.CapAutoPurge)
		if pgErr != nil && errs.CodeOf(pgErr) != errs.CodeGrantRequired {
			return total, bytes, domain.ControlStopScope, pgErr
		}

		// The rule's own grant and the run's shared ledger both apply. Taking
		// only the grant let two rules each spend the configured maximum.
		maxFiles := minInt(ledger.RemainingFiles(), minInt(grantFiles(qg), r.Config.Runtime.MaxAutoQuarantineFiles))
		maxBytes := minI64(ledger.RemainingBytes(), minI64(grantBytes(qg), r.Config.Runtime.MaxAutoQuarantineBytes))
		if maxFiles <= 0 || maxBytes <= 0 {
			continue
		}

		parsed, err := rules.Parse([]byte(rule.DSLJSON), map[string]bool{string(sc.Scope.ID): true})
		if err != nil {
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
			// Same ancestry as the executor will walk, so a plan never offers an
			// object the execution then refuses as protected.
			AncestorsOf: func(dir string) ([]domain.FileIdentity, error) {
				return execute.AncestorIdentities(sc, []byte(dir))
			},
		})
		if err != nil {
			if errs.CodeOf(err) == errs.CodeNoCandidates {
				continue
			}
			return total, bytes, domain.ControlStopScope, err
		}

		phase := domain.NewPhaseBudget(runLedger, ledger, maxFiles, maxBytes)
		applied, err := r.Exec.ApplyQuarantinePlan(ctx, sc, res.Plan.ID, res.Plan.Digest, phase)
		total += applied.Completed
		control = strongerControl(control, applied.Control)
		for _, out := range applied.Results {
			if out.Code != string(errs.CodeOK) {
				continue
			}
			for _, it := range res.Items {
				if it.ID == out.Item {
					bytes += itemSize(it)
					break
				}
			}
		}
		if err != nil {
			return total, bytes, strongerControl(control, ControlOfError(err)), err
		}
		if stopErr := batchStop(applied); stopErr != nil {
			return total, bytes, strongerControl(control, applied.Control), stopErr
		}
	}
	return total, bytes, control, nil
}

// BuildGuard resolves protection identities with the kernel. The work is done in
// the executor so that the purge path can build the same guard for a historical
// slot; this wrapper keeps the runner's call sites unchanged.
func BuildGuard(ctx context.Context, st *store.Store, sc *execute.ScopeCtx) (*policy.Guard, error) {
	return execute.BuildGuard(ctx, st, sc)
}

// ClockTrust is kept for callers that only need the verdict, such as `doctor`.
func ClockTrust(ctx context.Context, st *store.Store, clock domain.Clock) (bool, string) {
	guard := clocktrust.Start(st, clock)
	v, err := guard.Assess(ctx)
	if err != nil {
		return false, err.Error()
	}
	return v.Trusted, v.Reason
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

// diskStatus reports free and total bytes for the volume holding a path.
func diskStatus(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

func (r *Runner) diskFree() uint64 {
	scopes, err := r.Store.ListScopes(context.Background())
	if err != nil {
		return 0
	}
	var best uint64
	for _, sc := range scopes {
		if free, _, err := diskStatus(string(sc.RawRoot)); err == nil && free > best {
			best = free
		}
	}
	return best
}

// diskPressure reports whether the configured free-space floor has been reached.
//
// The thresholds were in the configuration, in the plan digest and in the
// executor's view struct, and no code read them. A setting that looks present and
// does nothing is worse than an absent one: it invites an operator to believe the
// volume is protected.
func (r *Runner) diskPressure() (bool, string) {
	cfg := r.Config.Runtime
	if cfg.MinFreeBytes <= 0 && cfg.MinFreePercent <= 0 {
		return false, ""
	}
	scopes, err := r.Store.ListScopes(context.Background())
	if err != nil {
		return true, "the volume could not be inspected: " + err.Error()
	}
	for _, sc := range scopes {
		if !domain.MutableKind(sc.Kind) {
			continue
		}
		free, total, err := diskStatus(string(sc.RawRoot))
		if err != nil {
			return true, "scope " + string(sc.ID) + " could not be inspected: " + err.Error()
		}
		if cfg.MinFreeBytes > 0 && free < uint64(cfg.MinFreeBytes) {
			return true, "free space is below min_free_bytes on the volume holding " + string(sc.ID)
		}
		if cfg.MinFreePercent > 0 && total > 0 {
			if free*100/total < uint64(cfg.MinFreePercent) {
				return true, "free space is below min_free_percent on the volume holding " + string(sc.ID)
			}
		}
	}
	return false, ""
}

func itemSize(it store.Item) int64 {
	var tok domain.ChangeToken
	if err := json.Unmarshal([]byte(it.PreTokenJSON), &tok); err != nil {
		return 0
	}
	return tok.Size
}

// purgeDueKey is the cursor value for the due queue.
func purgeDueKey(it store.Item) int64 {
	if it.PurgeNotBeforeNS == nil {
		return 0
	}
	return *it.PurgeNotBeforeNS
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

func strongerControl(a, b string) string {
	rank := func(s string) int {
		switch s {
		case domain.ControlStopRun:
			return 3
		case domain.ControlStopScope:
			return 2
		case domain.ControlStopPlan:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func ControlOfError(err error) string {
	switch errs.CodeOf(err) {
	case errs.CodeReviewRequired, errs.CodeModelUnavailable, errs.CodePaused, errs.CodeHeld, errs.CodeScopeChanged, errs.CodeProtectionAliasUnresolved:
		return domain.ControlStopScope
	case errs.CodePlanStale, errs.CodePlanExpired:
		return domain.ControlStopPlan
	case errs.CodeDurabilityFailed, errs.CodeIOError, errs.CodeStateWriteFailed,
		errs.CodeDBError, errs.CodeClockUntrusted:
		return domain.ControlStopRun
	}
	return domain.ControlContinue
}
