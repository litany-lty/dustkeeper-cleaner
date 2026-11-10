package report

import (
	"encoding/json"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

func reasonExplanation() review.Explanation {
	return review.Explanation{
		Cause: "model_insufficient_for_isolation",
		Local: &review.LocalSummary{
			Checks:        review.Checks{SingleLink: review.Bool(true), ParentStable: review.Bool(true), OpenByProcess: review.Bool(false)},
			FactsComplete: true,
			Movable:       true,
		},
		JEV: &review.JEVSummary{Choice: "allow_review", GatePassed: true},
		MiniMax: &review.MiniMaxSummary{
			Consulted:       true,
			Decision:        "insufficient",
			Reason:          "insufficient_evidence",
			MissingEvidence: []string{"current_use", "recoverability"},
		},
	}
}

func TestReviewExplanationsAreJoinedOnlyByExactReviewID(t *testing.T) {
	want := reasonExplanation()
	body, err := json.Marshal(map[string]any{
		"review_id":          "rev-right",
		"verdict":            domain.VerdictInsufficient,
		"review_explanation": want,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(body)
	events := []store.Event{reviewEvent("rev-right", "item-a", "ev-right", raw)}
	byID := reviewExplanations(events)
	if byID["rev-right"] == nil || byID["rev-right"].Summary() != want.Summary() {
		t.Fatalf("the exact review lost its explanation: %+v", byID)
	}
	if byID["rev-wrong"] != nil {
		t.Fatalf("an explanation crossed review ids: %+v", byID)
	}
}

func TestKeepEntryReadsEmbeddedExplanationAndLegacyRecordStaysEmpty(t *testing.T) {
	want := reasonExplanation()
	body, err := json.Marshal(map[string]any{
		"review_id":          "rev-keep",
		"reason":             "move_safety_unknown",
		"review_explanation": want,
		"review":             map[string]any{"http_attempts": 2, "cost_status": "usage_recorded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := keepEvent("item-keep", "ev-keep", string(body))
	entry, ok := entryOf(sectionKept, itemWithPath("a.zip", 10, domain.StatePlanned), ev)
	if !ok || entry.ReviewExplanation == nil || entry.ReviewExplanation.Summary() != want.Summary() {
		t.Fatalf("the keep record lost its own explanation: %+v", entry)
	}

	legacy := keepEvent("item-old", "ev-old", `{"reason":"move_safety_unknown"}`)
	oldEntry, ok := entryOf(sectionKept, itemWithPath("old.zip", 10, domain.StatePlanned), legacy)
	if !ok || oldEntry.ReviewExplanation != nil {
		t.Fatalf("a legacy record acquired invented detail: %+v", oldEntry)
	}
}

func TestRenderShowsExplanationSummaryWithoutChangingTheReason(t *testing.T) {
	explanation := reasonExplanation()
	entry := Entry{
		Path:              "a.zip",
		SizeBytes:         10,
		AtNS:              1,
		Reason:            "move_safety_unknown",
		ReasonText:        "无法确认移动是否安全",
		ReviewID:          "rev-1",
		ReviewExplanation: &explanation,
	}
	md := Render(Page{GeneratedAtNS: 1, WindowStartNS: 1,
		Kept: Section{Entries: []Entry{entry}, Total: 1}})
	if !strings.Contains(md, "无法确认移动是否安全") {
		t.Fatalf("the existing reason changed:\n%s", md)
	}
	for _, want := range []string{"模型认为隔离依据不足", "本地必要检查通过", "缺少依据：当前使用情况、可恢复性"} {
		if !strings.Contains(md, want) {
			t.Fatalf("the readable page omits %q:\n%s", want, md)
		}
	}
}
