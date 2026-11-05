package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/report"
)

// cmdCleanup routes the two commands that belong to the one-time confirmation and
// to reading its consequences.
func cmdCleanup(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "cleanup needs a subcommand: setup, report")
	}
	switch args[0] {
	case "setup":
		return cleanupSetup(ctx, a, args[1:])
	case "report":
		return cleanupReport(ctx, a, args[1:])
	default:
		return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown cleanup subcommand %q; use setup or report", args[0])
	}
}

// cleanupSetup confirms the five things the product needs to know once.
//
// It is deliberately two calls. Without --digest it prints the exact policy it
// would save and the digest of that content, and changes nothing; with --digest it
// saves only if the digest still matches what it would save, so a policy edited
// between the preview and the confirmation cannot be confirmed by accident. This is
// the same shape `rule activate` and `plan apply` already use: the digest is the
// thing the operator actually saw.
func cleanupSetup(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("cleanup setup", io.Discard)
	scopes := fs.String("scopes", "", "comma separated scope IDs this confirmation covers")
	categories := fs.String("categories", "", "comma separated categories that may be deleted without an observation period")
	days := fs.Int("observation-days", cleanup.DefaultObservationDays, "how long everything else is observed (7..30)")
	maxFiles := fs.Int("max-files", cleanup.Default().MaxFilesPerRun, "how many files one run may act on")
	maxBytes := fs.Int64("max-bytes", cleanup.Default().MaxBytesPerRun, "how many bytes one run may act on")
	notifyChannel := fs.String("notify", cleanup.Default().Notify, "where the notice appears: macos_notification or none")
	resultDir := fs.String("result-dir", "", "absolute directory for the result page; empty means the state directory")
	keepRetry := fs.Int("keep-retry-days", cleanup.Default().KeepRetryDays, "how long a keep suppresses asking about the same path")
	digest := fs.String("digest", "", "the digest shown by the preview; supplying it confirms the policy")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}

	pol := cleanup.Policy{
		Version:                1,
		Scopes:                 splitComma(*scopes),
		DirectDeleteCategories: splitComma(*categories),
		ObservationDays:        *days,
		MaxFilesPerRun:         *maxFiles,
		MaxBytesPerRun:         *maxBytes,
		Notify:                 *notifyChannel,
		ResultDir:              *resultDir,
		KeepRetryDays:          *keepRetry,
	}
	if err := pol.Validate(); err != nil {
		return output{}, err
	}
	// An empty scope list is a valid policy that covers nothing, which is also the
	// default. It is refused here only when the user asked for direct deletion:
	// approving categories for a set of directories that contains none would read
	// as an approval of everything.
	if len(pol.DirectDeleteCategories) > 0 && len(pol.Scopes) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"a policy that allows direct deletion must name the scopes it covers; --scopes is empty")
	}
	for _, id := range pol.Scopes {
		if _, err := a.store.GetScope(ctx, domain.ScopeID(id)); err != nil {
			return output{}, errs.Newf(errs.CodeInvalidArgument,
				"scope %q is not registered; add it with `scope add` first", id)
		}
	}
	if *digest == "" {
		sum, err := pol.ComputeDigest()
		if err != nil {
			return output{}, err
		}
		out := ok()
		out.Data = map[string]any{
			"policy": pol,
			"digest": sum,
			"confirm_hint": "dustkeeper cleanup setup --digest " + string(sum) + " " +
				setupArgsOf(pol),
		}
		out.Human = []string{
			"this is what would be approved; nothing has been written yet",
			"",
			previewOf(pol),
			"",
			"to confirm exactly this, run:",
			"  dustkeeper cleanup setup --digest " + string(sum) + " " + setupArgsOf(pol),
		}
		return out, nil
	}

	previous, err := cleanup.Load(ctx, a.store)
	if err != nil {
		return output{}, err
	}
	sum, err := pol.ComputeDigest()
	if err != nil {
		return output{}, err
	}
	if domain.Digest(*digest) != sum {
		return output{}, errs.Newf(errs.CodeDigestMismatch,
			"this confirmation does not match the policy it would write (expected %s); preview it again", sum)
	}
	out, err := a.withInstance(func() (output, error) {
		pol.ConfirmedAtNS = a.clock.Wall().UnixNano()
		if err := cleanup.Save(ctx, a.store, pol); err != nil {
			return output{}, err
		}
		saved, err := cleanup.Load(ctx, a.store)
		if err != nil {
			return output{}, err
		}
		res := ok()
		res.Data = map[string]any{
			"policy": saved,
			"digest": saved.Digest,
		}
		res.Human = []string{
			"cleanup policy confirmed",
			"",
			previewOf(saved),
			"",
			"this changes what happens from now on; objects already in the observation area keep the window they were promised",
		}
		if previous.Confirmed() && previous.ConfirmedAtNS != saved.ConfirmedAtNS {
			res.Human = append(res.Human,
				"this replaces the policy confirmed at "+
					time.Unix(0, previous.ConfirmedAtNS).Local().Format("2006-01-02 15:04:05"))
		}
		res.Next = []string{"dustkeeper cleanup report"}
		return res, nil
	})
	return out, err
}

// cleanupReport prints the most recent result page, or says where it is.
//
// It is read-only by construction: it opens the database and the page file, and it
// takes no file action of any kind.
func cleanupReport(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("cleanup report", io.Discard)
	dir := fs.String("dir", "", "directory holding the result page; empty means the confirmed policy's")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	target := *dir
	if target == "" {
		pol, err := cleanup.Load(ctx, a.store)
		if err != nil {
			return output{}, err
		}
		target = a.resultDirOf(pol)
	}
	pagePath := filepath.Join(target, report.LatestMarkdown)
	raw, err := os.ReadFile(pagePath)
	if err != nil {
		if os.IsNotExist(err) {
			return output{}, errs.Newf(errs.CodeNoCandidates,
				"there is no result page in %s yet; it is written when a run finishes", target)
		}
		return output{}, errs.Wrapf(errs.CodeIOError, err, "the result page cannot be read")
	}
	out := ok()
	out.Data = map[string]any{
		"path":    pagePath,
		"dir":     target,
		"content": string(raw),
	}
	if a.json {
		// The machine-readable form is the page as data, not the page as text.
		if pageJSON, jerr := os.ReadFile(filepath.Join(target, report.LatestJSON)); jerr == nil {
			out.Data = map[string]any{
				"path": pagePath,
				"json": filepath.Join(target, report.LatestJSON),
				"page": strings.TrimSpace(string(pageJSON)),
			}
		}
	}
	out.Human = strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	return out, nil
}

// resultDirOf is where this installation's result page lives.
func (a *app) resultDirOf(pol cleanup.Policy) string {
	if pol.ResultDir != "" {
		return pol.ResultDir
	}
	return filepath.Join(a.paths.StateDir, "reports")
}

// previewOf renders the five answers as lines a person can check.
func previewOf(pol cleanup.Policy) string {
	// An empty scope list must not read as "every directory": Covers() answers
	// no for it, so a policy confirmed without scopes governs nothing. Saying
	// just "（无）" leaves the ambiguity that matters most here.
	scopes := "（无：这份策略不覆盖任何目录，运行时不会处理任何文件）"
	if len(pol.Scopes) > 0 {
		scopes = strings.Join(pol.Scopes, ", ")
	}
	categories := "（无：任何文件都不会被直接删除）"
	if len(pol.DirectDeleteCategories) > 0 {
		categories = strings.Join(pol.DirectDeleteCategories, ", ")
	}
	return strings.Join([]string{
		"管理目录: " + scopes,
		"允许直接删除的类别: " + categories,
		fmt.Sprintf("其它候选的观察期: %d 天", pol.ObservationDays),
		fmt.Sprintf("每轮上限: %d 个文件 / %s", pol.MaxFilesPerRun, humanBytesOf(pol.MaxBytesPerRun)),
		"通知出口: " + pol.Notify,
		fmt.Sprintf("结果页目录: %s", displayOr(pol.ResultDir, "状态目录下的 reports/")),
		fmt.Sprintf("保留后重试间隔: %d 天", pol.KeepRetryDays),
	}, "\n")
}

// setupArgsOf renders the same policy as flags, so the confirmation command is one
// copy and paste away from the preview that produced it.
//
// Empty lists are omitted rather than written as `--scopes ”`: the flag's default
// is already empty, so the policy is identical, and a quoted empty string in a
// command meant to be pasted reads like a typo.
func setupArgsOf(pol cleanup.Policy) string {
	parts := make([]string, 0, 8)
	if len(pol.Scopes) > 0 {
		parts = append(parts, "--scopes "+shellQuote(strings.Join(pol.Scopes, ",")))
	}
	if len(pol.DirectDeleteCategories) > 0 {
		parts = append(parts, "--categories "+shellQuote(strings.Join(pol.DirectDeleteCategories, ",")))
	}
	parts = append(parts,
		fmt.Sprintf("--observation-days %d", pol.ObservationDays),
		fmt.Sprintf("--max-files %d", pol.MaxFilesPerRun),
		fmt.Sprintf("--max-bytes %d", pol.MaxBytesPerRun),
		"--notify "+shellQuote(pol.Notify),
		fmt.Sprintf("--keep-retry-days %d", pol.KeepRetryDays),
	)
	if pol.ResultDir != "" {
		parts = append(parts, "--result-dir "+shellQuote(pol.ResultDir))
	}
	return strings.Join(parts, " ")
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func displayOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func humanBytesOf(n int64) string {
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
