package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dustkeeper/internal/errs"
)

// File names. The page lives at a fixed place so that a notification can point at
// it once and keep pointing at it: a user who is told "查看详情" must not have to
// guess which of yesterday's files is today's answer.
const (
	LatestMarkdown = "latest.md"
	LatestJSON     = "latest.json"
)

// Paths are the files one render produced.
type Paths struct {
	Markdown string `json:"markdown"`
	JSON     string `json:"json"`
	// MarkdownSnapshot is the per-run copy. It is empty when the page carries no
	// run id, because a snapshot named after nothing cannot be found later.
	MarkdownSnapshot string `json:"markdown_snapshot,omitempty"`
	JSONSnapshot     string `json:"json_snapshot,omitempty"`
}

// Write renders the page into dir and returns the paths it wrote.
//
// Everything is written as a temporary file in the same directory and renamed into
// place, so a reader never sees half a page - the notification that points here may
// be opened at any moment.
func Write(dir string, page Page) (Paths, error) {
	var paths Paths
	if dir == "" {
		return paths, errs.New(errs.CodeInvalidArgument, "the result page has no directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return paths, errs.Wrapf(errs.CodeIOError, err, "the result directory %s cannot be created", dir)
	}
	md := Render(page)
	raw, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		return paths, errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	paths.Markdown = filepath.Join(dir, LatestMarkdown)
	paths.JSON = filepath.Join(dir, LatestJSON)
	if err := writeAtomic(paths.Markdown, []byte(md)); err != nil {
		return paths, err
	}
	if err := writeAtomic(paths.JSON, append(raw, '\n')); err != nil {
		return paths, err
	}
	if page.RunID != "" {
		paths.MarkdownSnapshot = filepath.Join(dir, "result-"+page.RunID+".md")
		paths.JSONSnapshot = filepath.Join(dir, "result-"+page.RunID+".json")
		if err := writeAtomic(paths.MarkdownSnapshot, []byte(md)); err != nil {
			return paths, err
		}
		if err := writeAtomic(paths.JSONSnapshot, append(raw, '\n')); err != nil {
			return paths, err
		}
	}
	return paths, nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".report-*")
	if err != nil {
		return errs.Wrapf(errs.CodeIOError, err, "the result page %s cannot be written", path)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errs.Wrapf(errs.CodeIOError, err, "the result page %s cannot be written", path)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errs.Wrapf(errs.CodeDurabilityFailed, err, "the result page %s cannot be flushed", path)
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrapf(errs.CodeIOError, err, "the result page %s cannot be closed", path)
	}
	if err := os.Rename(name, path); err != nil {
		return errs.Wrapf(errs.CodeIOError, err, "the result page %s cannot be put in place", path)
	}
	return nil
}

// Render turns the page into the Markdown a person reads.
//
// The three sections are the whole page, in that order, always: a document that
// sometimes hides an empty section makes the reader check whether it was empty or
// missing, and "missing" is the answer that costs trust.
func Render(page Page) string {
	var b strings.Builder
	b.WriteString("# 本轮清理结果\n\n")
	fmt.Fprintf(&b, "生成时间：%s\n\n", time.Unix(0, page.GeneratedAtNS).Format("2006-01-02 15:04:05 -07:00"))
	fmt.Fprintf(&b, "统计区间：%s 至 %s\n\n",
		time.Unix(0, page.WindowStartNS).Format("2006-01-02 15:04:05 -07:00"),
		time.Unix(0, page.WindowEndNS).Format("2006-01-02 15:04:05 -07:00"))
	if page.RunID != "" {
		fmt.Fprintf(&b, "运行编号：`%s`\n\n", page.RunID)
	}
	writeReviewSummary(&b, page.Summary)

	fmt.Fprintf(&b, "## 已删除（%d）\n\n", page.Deleted.Total)
	if page.Deleted.Total == 0 {
		b.WriteString("本次没有永久删除任何文件。\n\n")
	} else {
		b.WriteString("只有已完成并确认的删除会计入这里。\n\n")
		b.WriteString("| 原路径 | 大小 | 删除完成时间 | 理由 |\n|---|---:|---|---|\n")
		for _, e := range page.Deleted.Entries {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				mdCode(e.Path), humanBytes(e.SizeBytes),
				time.Unix(0, e.AtNS).Format("2006-01-02 15:04:05"), mdCell(reasonOf(e)))
		}
		writeMore(&b, page.Deleted)
	}

	fmt.Fprintf(&b, "\n## 待删除（%d）\n\n", page.Observed.Total)
	if page.Observed.Total == 0 {
		b.WriteString("本次没有文件进入待删除区。\n\n")
	} else {
		b.WriteString("这些文件在观察期内可以原样恢复；到期后仍要经过一次双模型审核才会清空。\n\n")
		b.WriteString("| 原路径 | 大小 | 移动时间 | 最早清空时间 | 理由 | 恢复入口 |\n|---|---:|---|---|---|---|\n")
		for _, e := range page.Observed.Entries {
			earliest := "未记录"
			if e.PurgeAfterNS != nil {
				earliest = time.Unix(0, *e.PurgeAfterNS).Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | `dustkeeper quarantine restore --object %s` |\n",
				mdCode(e.Path), humanBytes(e.SizeBytes),
				time.Unix(0, e.AtNS).Format("2006-01-02 15:04:05"), earliest,
				mdCell(reasonOf(e)), objectOf(e))
		}
		writeMore(&b, page.Observed)
		b.WriteString("\n想留下某个文件，用上面的 `restore` 把它放回原处；它随后会被记为受保护，不会再被这个规则选中。\n\n")
	}

	fmt.Fprintf(&b, "## 保留（%d）\n\n", page.Kept.Total)
	if page.Kept.Total == 0 {
		b.WriteString("本次没有需要保留说明的文件。\n\n")
	} else {
		b.WriteString("| 原路径 | 大小 | 保留时间 | 保留原因 | 接口失败或缺依据 |\n|---|---:|---|---|---|\n")
		for _, e := range page.Kept.Entries {
			why := mdCell(reasonOf(e))
			flag := reviewFlag(e)
			if e.ReviewID != "" {
				// The same id joins the dispatch row, the decision event and the
				// operational log. It is an identifier, not a file path.
				flag = strings.TrimSpace(flag + " · 关联审核 " + mdCell(e.ReviewID))
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				mdCode(e.Path), humanBytes(e.SizeBytes),
				time.Unix(0, e.AtNS).Format("2006-01-02 15:04:05"), why, flag)
		}
		writeMore(&b, page.Kept)
	}
	return b.String()
}

// writeReviewSummary states the reviews that produced no decision. Without it a
// page of keeps reads as "the models said no" even when nothing was ever asked.
func writeReviewSummary(b *strings.Builder, s Summary) {
	if s.Reviewed == 0 && s.NotStarted == 0 && s.UnknownHeld == 0 &&
		s.ReviewFailed == 0 && s.CostUnknown == 0 && s.FactsUnknown == 0 {
		return
	}
	b.WriteString("## 本轮审核\n\n")
	fmt.Fprintf(b, "- 已取得判断：%d\n", s.Reviewed)
	if s.NotStarted > 0 {
		fmt.Fprintf(b, "- 未开始（本轮未发出请求）：%d\n", s.NotStarted)
	}
	if s.ReviewFailed > 0 {
		fmt.Fprintf(b, "- 请求未取得结果（文件未动）：%d\n", s.ReviewFailed)
	}
	if s.UnknownHeld > 0 {
		fmt.Fprintf(b, "- 上次请求结果未知，本轮未重复请求：%d\n", s.UnknownHeld)
	}
	if s.CostUnknown > 0 {
		fmt.Fprintf(b, "- 费用未知（不能按 0 计）：%d\n", s.CostUnknown)
	}
	if s.FactsUnknown > 0 {
		fmt.Fprintf(b, "- 请求事实未记录：%d（未计入下方的 HTTP 尝试数，也不能按 0 理解）\n", s.FactsUnknown)
	}
	fmt.Fprintf(b, "- 本窗口 HTTP 尝试：%d 次\n\n", s.Attempts)
}

// reviewFlag is the one-line explanation of why a file was kept, when the reason
// is a review condition rather than a model opinion.
func reviewFlag(e Entry) string {
	switch e.Reason {
	case "not_started_budget":
		return "预算用完，审核未开始"
	case "review_unknown":
		return "请求已发出但无结果；费用未知"
	case "awaiting_request_check":
		return "上次请求结果未知，未重复请求"
	}
	if !e.ModelFailed {
		return ""
	}
	switch e.Failure {
	case "timeout":
		return "模型请求超时，判断未完成"
	case "auth", "provider":
		return "模型接口拒绝或出错，判断未完成"
	}
	return "模型服务失败，判断未完成"
}

// writeMore prints the remainder line R30 asks for.
func writeMore(b *strings.Builder, s Section) {
	if s.More > 0 {
		fmt.Fprintf(b, "\n共 %d 条，这里显示 %d 条，还有 %d 条未显示。\n", s.Total, len(s.Entries), s.More)
	}
}

// reasonOf prefers the sentence and falls back to the code, so a line is never
// blank when a reason exists.
func reasonOf(e Entry) string {
	if e.ReasonText != "" {
		return e.ReasonText
	}
	return e.Reason
}

// objectOf is the argument the restore command needs. An entry without one is
// labelled rather than filled with the operation id: a command that cannot work
// is worse than a column that says it is unknown.
func objectOf(e Entry) string {
	if e.ItemID != "" {
		return e.ItemID
	}
	return "unknown"
}

// mdCell makes a value safe inside a table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// mdCode renders a path as inline code.
//
// A path is user-controlled text and may contain a backtick, which would end the
// code span and turn the rest of the path into formatting. The padding rule from
// the CommonMark specification is applied rather than escaped characters, because
// escaping inside a code span would show the backslashes to the reader.
func mdCode(s string) string {
	s = mdCell(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || longest > 0 {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// humanBytes renders a size the way the rest of the product does.
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
