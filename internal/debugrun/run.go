// Package debugrun executes bounded, read-only cleanup rehearsals. It never
// creates plans, writes state, or calls a file mutation executor.
package debugrun

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/execute"
	"dustkeeper/internal/fsops"
	"dustkeeper/internal/policy"
	"dustkeeper/internal/review"
)

type Options struct {
	Root, ScopeID, Action           string
	Execute                         bool
	MaxEntries, MaxCandidates, Days int
	MaxReadBytes                    int64
	Now                             time.Time
	References                      map[string]bool
	IncludePaths                    map[string]bool
	OwnerConfirmedUnused            map[string]bool
	Advisory                        bool
	MiniMaxTimeout                  time.Duration
	Service                         *review.Service
	OpenFiles                       bool
}

type Entry struct {
	File            assess.FileReport `json:"file"`
	Outcome         string            `json:"outcome"`
	Reason          string            `json:"reason,omitempty"`
	Decision        *review.Decision  `json:"decision,omitempty"`
	Candidate       *review.Facts     `json:"review_facts,omitempty"`
	Disposition     string            `json:"disposition,omitempty"`
	SourceUnchanged bool              `json:"source_unchanged"`
	AuditStatus     string            `json:"audit_status"`
}

type Report struct {
	Mode                  string        `json:"mode"`
	Execute               bool          `json:"execute"`
	Action                string        `json:"simulated_action"`
	Scan                  assess.Report `json:"scan"`
	Entries               []Entry       `json:"entries"`
	MaxCandidates         int           `json:"max_candidates"`
	MaxModelCalls         int           `json:"max_model_calls"`
	ModelCalls            int           `json:"model_calls"`
	SourceReadBytes       int64         `json:"source_read_bytes"`
	BackupReadBytes       int64         `json:"backup_read_bytes"`
	TotalReadBytes        int64         `json:"total_read_bytes"`
	SimulatedCleanedFiles int           `json:"simulated_cleaned_files"`
	SimulatedLogicalBytes int64         `json:"simulated_logical_bytes"`
	ActualDeletedFiles    int           `json:"actual_deleted_files"`
	ActualFreedBytes      int64         `json:"actual_freed_bytes"`
	ProductionAuthorized  bool          `json:"production_authorized"`
	AdvisoryOnly          bool          `json:"advisory_only"`
	MiniMaxTimeoutSeconds int           `json:"minimax_timeout_seconds"`
	StoppedReason         string        `json:"stopped_reason,omitempty"`
	Notice                string        `json:"notice"`
}

func Run(ctx context.Context, opt Options) (Report, error) {
	r := Report{Mode: "debug", Execute: opt.Execute, Action: opt.Action, AdvisoryOnly: opt.Advisory, Entries: []Entry{}, MaxCandidates: opt.MaxCandidates, MaxModelCalls: 2 * opt.MaxCandidates,
		Notice: "Simulation only. No files are moved or removed and no production state is written. Model agreement requires human audit and grants no production permission."}
	if !filepath.IsAbs(opt.Root) || filepath.Clean(opt.Root) != opt.Root || opt.ScopeID == "" ||
		(opt.Action != "quarantine" && opt.Action != "purge") || opt.MaxCandidates < 1 || opt.MaxCandidates > 6 || opt.MaxReadBytes < 4 || opt.MaxReadBytes > 1<<30 {
		return r, errs.New(errs.CodeInvalidArgument, "debug requires an absolute root, scope-id, action quarantine/purge, max-candidates 1..6 and max-read-bytes 4..1GiB")
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.MiniMaxTimeout == 0 {
		opt.MiniMaxTimeout = 20 * time.Second
	}
	if opt.MiniMaxTimeout < time.Second || opt.MiniMaxTimeout > 60*time.Second {
		return r, errs.New(errs.CodeInvalidArgument, "debug MiniMax timeout must be 1..60 seconds")
	}
	r.MiniMaxTimeoutSeconds = int(opt.MiniMaxTimeout / time.Second)
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	process := assess.OpenObservation{Coverage: "not_requested", Paths: map[string]bool{}}
	if opt.OpenFiles {
		process = assess.ProbeOpenFiles(ctx, opt.Root, nil)
	}
	scan, err := assess.Scan(ctx, opt.Root, assess.Options{Now: opt.Now, Days: opt.Days, MaxEntries: opt.MaxEntries, ReferencePaths: opt.References, OpenPaths: process.Paths, ProcessCoverage: process.Coverage})
	for i := range scan.Files {
		if opt.OwnerConfirmedUnused[scan.Files[i].Facts.Path] {
			scan.Files[i].Facts.OwnerConfirmedUnused = true
		}
		if opt.Advisory {
			scan.Files[i].Assessment = assess.AssessAdvisory(scan.Files[i].Facts, opt.Days, opt.Now)
		} else {
			scan.Files[i].Assessment = assess.Assess(scan.Files[i].Facts, opt.Days, opt.Now)
		}
	}
	r.Scan = scan
	if err != nil {
		return r, err
	}
	opt.Root = scan.Root
	if len(opt.IncludePaths) > 0 {
		visible := []assess.FileReport{}
		for _, file := range scan.Files {
			if opt.IncludePaths[file.Facts.Path] {
				visible = append(visible, file)
			}
		}
		scan.Files = visible
		r.Scan.Files = visible
	}
	if !scan.Complete {
		r.StoppedReason = "incomplete_scan"
		return r, errs.New(errs.CodeReviewRequired, "debug scan incomplete; no model calls started")
	}
	if len(opt.IncludePaths) > 0 {
		if len(scan.Files) != len(opt.IncludePaths) {
			return r, errs.New(errs.CodeReviewRequired, "an explicitly selected file is unavailable; no models called")
		}
	}
	root, err := fsops.OpenRoot(scan.Root)
	if err != nil {
		return r, err
	}
	defer root.Close()
	var judge *review.HTTPJudge
	if opt.Execute {
		if opt.Service == nil || opt.Service.Judge == nil {
			return r, errs.New(errs.CodeReviewRequired, "debug execute requires configured dual review")
		}
		original := opt.Service
		opt.Service = &review.Service{Judge: original.Judge, Now: original.Now, MaxCandidates: opt.MaxCandidates}
		judge, _ = opt.Service.Judge.(*review.HTTPJudge)
		if judge != nil {
			judge, err = judge.DebugSession(r.MaxModelCalls, opt.MiniMaxTimeout)
			if err != nil {
				return r, err
			}
			opt.Service.Judge = judge
		}
	}
	sourceLimit := opt.MaxReadBytes / 2
	if opt.Advisory {
		sourceLimit = opt.MaxReadBytes
	}
	budget := domain.NewBudget(120, sourceLimit, 0, 0, 0)
	sort.SliceStable(scan.Files, func(i, j int) bool {
		if scan.Files[i].Facts.AllocatedBytes == scan.Files[j].Facts.AllocatedBytes {
			return scan.Files[i].Facts.Path < scan.Files[j].Facts.Path
		}
		return scan.Files[i].Facts.AllocatedBytes > scan.Files[j].Facts.AllocatedBytes
	})
	selected := 0
	for index, file := range scan.Files {
		row := Entry{File: file, Outcome: "keep", AuditStatus: "local_retention"}
		protected, _ := policy.GlobalProtected(file.Facts.Path)
		if file.Assessment.Disposition == "keep" || (protected && !(opt.Advisory && policy.AdvisoryOnlyReviewable(file.Facts.Path))) {
			row.Reason = "local protection or recent activity"
			r.Entries = append(r.Entries, row)
			continue
		}
		if opt.Execute && (file.Facts.LogicalBytes < 0 || file.Facts.LogicalBytes > budget.RemainingRead()/2) {
			row.Outcome, row.Reason, row.AuditStatus = "not_reviewed", "source_read_budget", "not_reviewed"
			r.Entries = append(r.Entries, row)
			continue
		}
		if selected >= opt.MaxCandidates {
			row.Outcome = "not_reviewed"
			row.Reason = "candidate_limit"
			r.Entries = append(r.Entries, row)
			continue
		}
		selected++
		row.AuditStatus = "pending_human_review"
		if !opt.Execute {
			row.Outcome = "planned"
			r.Entries = append(r.Entries, row)
			continue
		}
		if err := ctx.Err(); err != nil {
			r.StoppedReason = "deadline"
			return r, err
		}
		row, err = inspect(ctx, root, file, row, opt, budget)
		if row.Decision == nil && row.Outcome == "kept" {
			row.Outcome, row.AuditStatus = "not_reviewed", "not_reviewed"
		}
		if err == nil && !scan.ReferencesUnchanged() {
			row.Outcome, row.Reason = "kept", "reference_observation_stale"
			err = errs.New(errs.CodePlanStale, row.Reason)
		}
		r.SourceReadBytes = budget.BytesRead()
		r.TotalReadBytes = r.SourceReadBytes + r.BackupReadBytes
		if judge != nil {
			r.ModelCalls = judge.Calls()
		}
		r.Entries = append(r.Entries, row)
		if row.Outcome == "simulated_cleanup" {
			r.SimulatedCleanedFiles++
			r.SimulatedLogicalBytes += file.Facts.LogicalBytes
		}
		if err != nil && errs.CodeOf(err) != errs.CodeReviewRequired {
			r.StoppedReason = string(errs.CodeOf(err))
			if ctx.Err() != nil {
				r.StoppedReason = string(errs.CodeCancelled)
			}
			for _, pending := range scan.Files[index+1:] {
				r.Entries = append(r.Entries, Entry{File: pending, Outcome: "not_reviewed", Reason: "run stopped: " + r.StoppedReason, AuditStatus: "not_reviewed"})
			}
			return r, err
		}
	}
	return r, nil
}

func inspect(ctx context.Context, root *fsops.Root, file assess.FileReport, row Entry, opt Options, budget *domain.Budget) (Entry, error) {
	row.Outcome = "kept"
	if !utf8.ValidString(file.Facts.Path) || strings.ContainsRune(file.Facts.Path, '\uFFFD') {
		row.Reason = "ambiguous_path_encoding"
		return row, errs.New(errs.CodeInvalidArgument, row.Reason)
	}
	if file.Facts.LogicalBytes < 0 || file.Facts.LogicalBytes > budget.RemainingRead()/2 {
		row.Reason = "source_read_budget"
		return row, nil
	}
	parent, err := root.OpenParent([]byte(file.Facts.Path))
	if err != nil {
		row.Reason = "source_unavailable"
		return row, err
	}
	defer parent.Close()
	opened, err := fsops.OpenRegularAt(parent, root.Dev())
	if err != nil {
		row.Reason = "source_not_regular"
		return row, err
	}
	defer opened.Close()
	if opened.Identity.Device != file.Facts.Device || opened.Identity.Inode != file.Facts.Inode || opened.Token.Size != file.Facts.LogicalBytes || opened.Token.MTimeNS != file.Facts.MTime.UnixNano() ||
		opened.Token.CTimeNS != file.Facts.CTimeNS || opened.Token.NLink != file.Facts.NLink || opened.Token.Mode != file.Facts.Mode || opened.Token.UID != file.Facts.UID || opened.Token.GID != file.Facts.GID {
		row.Reason = "source_changed_since_scan"
		return row, errs.New(errs.CodePlanStale, row.Reason)
	}
	before, err := execute.ReadStable(opened, execute.VerifyOptions{Budget: budget})
	if err != nil {
		row.Reason = "source_verification_failed"
		return row, err
	}
	ageDays := 0
	if !file.Facts.MTime.IsZero() && opt.Now.After(file.Facts.MTime) {
		ageDays = int(opt.Now.Sub(file.Facts.MTime).Hours() / 24)
	}
	facts := review.Facts{
		Action:       opt.Action,
		FactsSource:  "measured_by_program",
		ScopeID:      opt.ScopeID,
		RelativePath: file.Facts.Path,
		Category:     assess.ClassifyName(filepath.Base(file.Facts.Path)).Category,
		SizeBytes:    before.Token.Size,
		AgeDays:      ageDays,
		Checks: review.Checks{
			Protected:      review.Bool(file.Facts.Protected),
			SingleLink:     review.Bool(file.Facts.NLink == 1),
			OpenByProcess:  review.Bool(file.Facts.OpenByProcess),
			KnownReference: review.Bool(file.Facts.KnownReference),
			ParentStable:   review.Bool(true),
			ContentRead:    review.Bool(true),
		},
		Observation: &review.FileObservation{
			ObservedAtNS:      opt.Now.UnixNano(),
			ModifiedAtNS:      file.Facts.MTime.UnixNano(),
			AccessedAtNS:      file.Facts.ATime.UnixNano(),
			LastUsedStatus:    file.Facts.LastUsedStatus,
			ProcessVisibility: file.Facts.ProcessVisibility,
			ReferenceCoverage: "only explicitly supplied references and symlinks observed inside this scan; no proof of global non-reference",
		},
		Policy:        review.PolicyFacts{ObservationDays: 14},
		ContentSHA256: string(before.Content),
	}
	request := review.Request{
		Action: opt.Action, ScopeID: opt.ScopeID,
		ItemID:       fmt.Sprintf("debug:%d:%d", before.Identity.Device, before.Identity.Inode),
		RelativePath: file.Facts.Path, Category: facts.Category, AgeDays: ageDays,
		Identity: before.Identity, SizeBytes: before.Token.Size,
		Observation: facts.Observation, Checks: facts.Checks, Policy: facts.Policy,
		VerifiedContentSHA256: string(before.Content),
	}
	var d review.Decision
	var approval review.Approval
	var reviewErr error
	if opt.Advisory {
		d, err = opt.Service.Judge.Review(ctx, facts)
		if err != nil {
			reviewErr = errs.New(errs.CodeModelUnavailable, "advisory model request failed; file retained")
		}
		row.AuditStatus = "advisory_only"
	} else {
		d, approval, reviewErr = opt.Service.Review(ctx, request)
		// A model that could not be reached is not a verdict about the file. The
		// Service reports it as a decision with ModelError rather than as an
		// error, so the rehearsal has to recognise it here: continuing would
		// spend the rest of the candidate budget on the same outage and report
		// "kept" for files nobody actually reviewed.
		if reviewErr == nil && d.ModelError != "" {
			reviewErr = errs.New(errs.CodeModelUnavailable, d.ModelError)
		}
	}
	row.Decision = &d
	row.Candidate = &facts
	switch d.Verdict {
	case "allow":
		row.Disposition = "allow"
	case "insufficient":
		row.Disposition = "insufficient"
	default:
		row.Disposition = "keep"
	}
	if err := ctx.Err(); err != nil {
		row.Reason = "review cancelled; no further read started"
		return row, err
	}
	after, err := execute.ReadStable(opened, execute.VerifyOptions{Budget: budget})
	if err != nil {
		row.Reason = "source_recheck_failed"
		return row, err
	}
	if err = execute.EvidenceMatches(after, before.Identity, before.Token, string(before.Content), string(before.Metadata)); err != nil {
		row.Reason = "source_changed_during_review"
		return row, err
	}
	if err = confirmPath(root, file.Facts.Path, before); err != nil {
		row.Reason = "source_path_changed"
		return row, err
	}
	row.SourceUnchanged = true
	if reviewErr != nil {
		row.Reason = reviewErr.Error()
		return row, reviewErr
	}
	if opt.Advisory {
		row.Outcome = "advisory_only"
		row.Reason = "model opinion recorded; recovery and reference evidence are unconfirmed; file retained"
		if !file.Facts.OwnerConfirmedUnused {
			row.Reason = "model opinion recorded; disposal, recovery and reference evidence are unconfirmed; file retained"
		}
		return row, nil
	}
	if approval == nil {
		row.Outcome = "kept"
		row.AuditStatus = "kept_by_review"
		row.Reason = "no reviewer allowed this action; the file stays where it is"
		return row, nil
	}
	if err = approval.Verify(string(after.Content)); err != nil {
		row.Reason = err.Error()
		return row, err
	}
	// The approval is exercised for the action it was asked about, not for the
	// product-level disposition: "purge" and "delete" are both reported as a
	// deletion, but the permission names the action it was given for.
	if err = approval.CheckIntent(request.ItemID, request.Action, serviceNow(opt.Service).UnixNano()); err != nil {
		row.Reason = err.Error()
		return row, err
	}
	row.Outcome = "simulated_cleanup"
	row.Reason = "dual review and local rechecks passed; human audit pending; no deletion performed"
	return row, nil
}

func serviceNow(s *review.Service) time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func confirmPath(root *fsops.Root, relative string, want execute.ObjectEvidence) error {
	fresh, err := fsops.OpenRoot(root.Path())
	if err != nil {
		return err
	}
	defer fresh.Close()
	oldID, _, err := root.Stat()
	if err != nil {
		return err
	}
	newID, _, err := fresh.Stat()
	if err != nil {
		return err
	}
	if !oldID.Equal(newID) {
		return errs.New(errs.CodePlanStale, "debug root changed")
	}
	parent, err := fresh.OpenParent([]byte(relative))
	if err != nil {
		return err
	}
	defer parent.Close()
	id, token, err := parent.StatAtNoFollow()
	if err != nil {
		return err
	}
	if !id.Equal(want.Identity) || token.Size != want.Token.Size || token.MTimeNS != want.Token.MTimeNS || token.CTimeNS != want.Token.CTimeNS || token.NLink != want.Token.NLink {
		return errs.New(errs.CodePlanStale, "debug path no longer names the reviewed file")
	}
	return nil
}
