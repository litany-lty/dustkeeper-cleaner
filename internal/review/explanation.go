package review

import (
	"strings"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/outcome"
)

// Explanation is an additive audit snapshot, never an input to authorization.
// Only measured booleans and allowlisted decision codes belong here. Model
// prose, prompts, paths, headers and raw responses are deliberately excluded.
type Explanation struct {
	Cause   string          `json:"cause,omitempty"`
	Local   *LocalSummary   `json:"local,omitempty"`
	JEV     *JEVSummary     `json:"jev,omitempty"`
	MiniMax *MiniMaxSummary `json:"minimax,omitempty"`
}

type LocalSummary struct {
	FactsComplete bool   `json:"facts_complete"`
	Movable       bool   `json:"movable"`
	Checks        Checks `json:"checks"`
}

type JEVSummary struct {
	Choice     string   `json:"choice,omitempty"`
	GatePassed bool     `json:"gate_passed"`
	Blockers   []string `json:"blockers,omitempty"`
}

type MiniMaxSummary struct {
	Sent      bool   `json:"sent"`
	Consulted bool   `json:"consulted"`
	Decision  string `json:"decision,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// nil means the provider did not supply the field; [] means it supplied an
	// empty list. Do not turn legacy absence into a claim of no missing facts.
	MissingEvidence []string `json:"missing_evidence"`
}

func NewExplanation(r Request, d Decision, reason string) *Explanation {
	x := &Explanation{Local: &LocalSummary{FactsComplete: r.FactsComplete, Movable: r.Movable, Checks: r.Checks}}
	switch {
	case reason == outcome.ReasonFactsMissing:
		x.Cause = "local_facts_missing"
	case reason == outcome.ReasonMoveSafetyUnknown && !r.Movable:
		x.Cause = "local_move_safety_unknown"
	case reason == outcome.ReasonMoveSafetyUnknown && r.Action == domain.DispositionQuarantine && d.Verdict == domain.VerdictInsufficient:
		x.Cause = "model_insufficient_for_isolation"
	}
	if d.JEV != nil {
		x.JEV = &JEVSummary{Choice: safeCode(d.JEV.Choice, "allow_review", "keep", "insufficient"), GatePassed: d.JEV.GatePassed,
			Blockers: safeCodes(d.JEV.Blockers, "model_keep", "model_insufficient", "confidence_below_threshold", "probability_below_threshold")}
	}
	x.MiniMax = &MiniMaxSummary{Sent: d.LLMSent, Consulted: d.LLMConsulted,
		Decision:        safeCode(d.LLMDecision, "allow", "deny", "insufficient"),
		Reason:          safeCode(d.LLMReason, "disposable", "still_needed", "insufficient_evidence"),
		MissingEvidence: safeCodes(d.MissingEvidence, miniMaxMissingEvidenceCategories...)}
	return x
}

func safeCode(v string, allowed ...string) string {
	for _, code := range allowed {
		if v == code {
			return v
		}
	}
	return ""
}

func safeCodes(values []string, allowed ...string) []string {
	if values == nil {
		return nil
	}
	out := []string{}
	// Iterate the fixed vocabulary, avoiding duplicate/unbounded annotations.
	for _, code := range allowed {
		for _, v := range values {
			if v == code {
				out = append(out, code)
				break
			}
		}
	}
	return out
}

// Summary renders only the bounded codes above; it never echoes stored prose.
func (x *Explanation) Summary() string {
	if x == nil {
		return ""
	}
	var parts []string
	switch x.Cause {
	case "local_facts_missing":
		parts = append(parts, "本地必要检查未完成")
	case "local_move_safety_unknown":
		parts = append(parts, "本地检查尚不能确认可安全移动")
	case "model_insufficient_for_isolation":
		parts = append(parts, "模型认为隔离依据不足")
	}
	if l := x.Local; l != nil {
		if l.FactsComplete && l.Movable {
			parts = append(parts, "本地必要检查通过")
		}
		for _, c := range []struct {
			v       *bool
			blocked bool
			text    string
		}{
			{l.Checks.Protected, true, "文件受保护"}, {l.Checks.SingleLink, false, "不是单链接文件"},
			{l.Checks.InUse, true, "仍被保留副本使用"}, {l.Checks.OpenByProcess, true, "可见进程正在打开文件"},
			{l.Checks.ParentStable, false, "父目录稳定性未确认"},
		} {
			if c.v != nil && *c.v == c.blocked {
				parts = append(parts, c.text)
			}
		}
		if !l.FactsComplete && l.Checks.OpenByProcess == nil {
			parts = append(parts, "进程占用检查未完成")
		}
	}
	if j := x.JEV; j != nil {
		switch j.Choice {
		case "allow_review":
			parts = append(parts, "JEV：允许复核")
		case "keep":
			parts = append(parts, "JEV：保留")
		case "insufficient":
			parts = append(parts, "JEV：依据不足")
		}
		for _, b := range j.Blockers {
			switch b {
			case "confidence_below_threshold":
				parts = append(parts, "JEV 置信度未达阈值")
			case "probability_below_threshold":
				parts = append(parts, "JEV 允许复核概率未达阈值")
			}
		}
	}
	if m := x.MiniMax; m != nil {
		if !m.Consulted {
			if m.Sent {
				parts = append(parts, "MiniMax：请求未取得有效判断")
			} else {
				parts = append(parts, "MiniMax：未调用")
			}
		} else {
			switch m.Decision {
			case "allow":
				parts = append(parts, "MiniMax：同意")
			case "deny":
				parts = append(parts, "MiniMax：保留")
			case "insufficient":
				parts = append(parts, "MiniMax：依据不足")
			}
			labels := map[string]string{"purpose_or_disposal_basis": "用途或处置依据", "references": "引用关系", "current_use": "当前使用情况", "recoverability": "可恢复性", "sensitive_content": "敏感内容情况", "provenance": "来源", "action_constraints": "操作约束"}
			var missing []string
			for _, code := range safeCodes(m.MissingEvidence, miniMaxMissingEvidenceCategories...) {
				missing = append(missing, labels[code])
			}
			if len(missing) > 0 {
				parts = append(parts, "缺少依据："+strings.Join(missing, "、"))
			} else if m.Decision == "insufficient" {
				parts = append(parts, "模型未列出具体缺项")
			}
		}
	}
	return strings.Join(parts, "；")
}
