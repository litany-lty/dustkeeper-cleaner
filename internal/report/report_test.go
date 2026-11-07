package report

import (
	"encoding/json"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

func itemWithPath(rel string, size int64, state string) store.Item {
	tok, _ := json.Marshal(domain.ChangeToken{Size: size})
	return store.Item{
		ID:           domain.ItemID("item-" + rel),
		ScopeID:      "exports",
		RawRelative:  []byte(rel),
		PreTokenJSON: string(tok),
		State:        state,
		SourceKind:   domain.SourceScheduledRule,
	}
}

func eventFor(action, phase string, res string) store.Event {
	var payload *string
	if res != "" {
		payload = &res
	}
	return store.Event{
		EventID:      "ev",
		Action:       action,
		Phase:        phase,
		OccurredAtNS: 100,
		ContractJSON: `{}`,
		ResultJSON:   payload,
	}
}

// TestOnlyFinishedActionsCount is R29 as a test: a candidate, a model opinion, an
// intent and a debug simulation are all things that almost happened. Putting one of
// them under "已删除" would tell the user a file is gone when it is not.
func TestOnlyFinishedActionsCount(t *testing.T) {
	cases := []struct {
		action string
		phase  string
		want   bool
		reason string
	}{
		{domain.ActionDelete, domain.PhaseComplete, true, "a completed deletion counts"},
		{domain.ActionPurge, domain.PhaseComplete, true, "a completed clearing counts"},
		{domain.ActionDelete, domain.PhaseIntent, false, "an intent is not a deletion"},
		{domain.ActionDelete, domain.PhasePark, false, "a park is an unfinished attempt"},
		{domain.ActionDelete, domain.PhaseHold, false, "a hold is a refusal, not a deletion"},
		{domain.ActionQuarantine, domain.PhaseComplete, true, "a completed move counts as observed"},
		{domain.ActionQuarantine, domain.PhaseIntent, false, "a planned move is not a move"},
		{"keep", domain.PhaseInfo, true, "a keep is recorded"},
		{"keep", domain.PhaseComplete, false, "a keep has no completion phase"},
		{"review", domain.PhaseInfo, false, "a model opinion is not an outcome"},
		{"debug_simulate", domain.PhaseInfo, false, "a simulation is not an outcome"},
	}
	for _, tc := range cases {
		t.Run(tc.action+"/"+tc.phase, func(t *testing.T) {
			_, ok := sectionOf(eventFor(tc.action, tc.phase, ""))
			if ok != tc.want {
				t.Fatalf("%s: got %v, want %v (%s)", tc.action+"/"+tc.phase, ok, tc.want, tc.reason)
			}
		})
	}
}

// TestTheRecordMustStillAgreeWithTheCompletion: a completion event plus a state
// that has since moved on is not a result. An object restored after its move is not
// waiting to be cleared, and a held one has no verdict to report.
func TestTheRecordMustStillAgreeWithTheCompletion(t *testing.T) {
	ev := eventFor(domain.ActionQuarantine, domain.PhaseComplete, `{}`)
	if _, ok := entryOf(sectionObserved, itemWithPath("a.zip", 10, domain.StateRestored), ev); ok {
		t.Fatal("a restored object was listed as waiting to be cleared")
	}
	if _, ok := entryOf(sectionObserved, itemWithPath("a.zip", 10, domain.StateHeld), ev); ok {
		t.Fatal("a held object was listed as waiting to be cleared")
	}
	if _, ok := entryOf(sectionObserved, itemWithPath("a.zip", 10, domain.StateQuarantined), ev); !ok {
		t.Fatal("a quarantined object was not listed")
	}

	del := eventFor(domain.ActionDelete, domain.PhaseComplete, `{"reason":"dual_review_agrees"}`)
	if _, ok := entryOf(sectionDeleted, itemWithPath("a.zip", 10, domain.StatePIntent), del); ok {
		t.Fatal("an item that is still mid-deletion was reported as deleted")
	}
	if _, ok := entryOf(sectionDeleted, itemWithPath("a.zip", 10, domain.StatePurged), del); !ok {
		t.Fatal("a purged item was not reported as deleted")
	}
}

func TestRenderCarriesEveryFieldTheProductPromises(t *testing.T) {
	purgeAfter := int64(1_800_000_000_000_000_000)
	page := Page{
		GeneratedAtNS: 1_700_000_000_000_000_000,
		WindowStartNS: 1_699_999_000_000_000_000,
		WindowEndNS:   1_700_000_000_000_000_000,
		RunID:         "run_test",
		Deleted: Section{Entries: []Entry{{
			Path: "构建/缓存.tmp", SizeBytes: 4096, AtNS: 1_700_000_000_000_000_000,
			Reason: "dual_review_agrees", ReasonText: "双模型同意永久删除",
		}}, Total: 1},
		Observed: Section{Entries: []Entry{{
			Path: "下载/安装包.dmg", SizeBytes: 8192, AtNS: 1_700_000_000_000_000_000,
			PurgeAfterNS: &purgeAfter, Reason: "safe_to_observe", ReasonText: "可安全移动，进入观察区",
			ItemID: "obj_1", Operation: "op_1",
		}}, Total: 1},
		Kept: Section{Entries: []Entry{{
			Path: "财务/账单.pdf", SizeBytes: 512, AtNS: 1_700_000_000_000_000_000,
			Reason: "review_unavailable", ReasonText: "模型服务不可用，本轮未处理", ModelFailed: true,
		}}, Total: 1},
	}
	md := Render(page)

	// R25, field by field.
	for _, want := range []string{
		"## 已删除（1）", "构建/缓存.tmp", "4.0 KiB", "双模型同意永久删除",
		"## 待删除（1）", "下载/安装包.dmg", "最早清空时间", "可安全移动，进入观察区",
		// The restore command takes an item id. The operation id is a record of
		// one attempt and resolves to nothing, so the page must not offer it.
		"quarantine restore --object obj_1",
		"## 保留（1）", "财务/账单.pdf", "模型服务不可用，本轮未处理", "模型服务失败",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("the page does not contain %q:\n%s", want, md)
		}
	}
}

// TestEmptySectionIsPrintedNotEmpty is about trust: a page that hides an empty
// section makes the reader wonder whether it was empty or missing.
func TestEmptySectionIsPrintedNotEmpty(t *testing.T) {
	md := Render(Page{GeneratedAtNS: 1_700_000_000_000_000_000, WindowStartNS: 1})
	for _, want := range []string{"## 已删除（0）", "## 待删除（0）", "## 保留（0）", "本次没有永久删除任何文件"} {
		if !strings.Contains(md, want) {
			t.Fatalf("the empty page does not contain %q:\n%s", want, md)
		}
	}
}

// TestOverflowShowsTotalAndRemainder is R30.
func TestOverflowShowsTotalAndRemainder(t *testing.T) {
	entries := make([]Entry, 3)
	for i := range entries {
		entries[i] = Entry{Path: "a.zip", AtNS: int64(i), ReasonText: "原因"}
	}
	md := Render(Page{GeneratedAtNS: 1, WindowStartNS: 1,
		Kept: Section{Entries: entries, Total: 250, More: 247}})
	if !strings.Contains(md, "共 250 条") || !strings.Contains(md, "还有 247 条未显示") {
		t.Fatalf("the remainder is not stated:\n%s", md)
	}
}

// TestPathCannotBreakTheTable: file names are user-controlled text.
func TestPathCannotBreakTheTable(t *testing.T) {
	md := Render(Page{GeneratedAtNS: 1, WindowStartNS: 1, Kept: Section{
		Entries: []Entry{{Path: "a|b`c.txt", AtNS: 1, ReasonText: "原因"}},
		Total:   1,
	}})
	if strings.Contains(md, "| a|b") {
		t.Fatalf("a pipe in a path broke the table:\n%s", md)
	}
	if !strings.Contains(md, `a\|b`) {
		t.Fatalf("the pipe was not escaped:\n%s", md)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{1 << 30, "1.0 GiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// reviewEvent builds the settlement record the dispatcher writes with every
// decision. It is the one authoritative record of a prepared request.
func reviewEvent(id, item, eventID string, body string) store.Event {
	it := domain.ItemID(item)
	return store.Event{
		EventID: eventID, ItemID: &it, Action: "review", Phase: domain.PhaseInfo,
		OccurredAtNS: 100, ContractJSON: `{"review_id":"` + id + `"}`, ResultJSON: &body,
	}
}

func keepEvent(item, eventID, body string) store.Event {
	it := domain.ItemID(item)
	return store.Event{
		EventID: eventID, ItemID: &it, Action: "keep", Phase: domain.PhaseInfo,
		OccurredAtNS: 100, ContractJSON: `{}`, ResultJSON: &body,
	}
}

// Successful and failed reviews both belong in the summary, and the key is the
// review id rather than the item: a duplicate settlement event and a keep that
// refers to the same review must not be counted as extra reviews.
func TestEveryReviewIsCountedOnceByItsCorrelationID(t *testing.T) {
	events := []store.Event{
		reviewEvent("rev-1", "item-a", "ev-1", `{"review_id":"rev-1","verdict":"allow","attempts":2,"cost_status":"usage_recorded"}`),
		reviewEvent("rev-1", "item-a", "ev-2", `{"review_id":"rev-1","verdict":"allow","attempts":2,"cost_status":"usage_recorded"}`),
		reviewEvent("rev-2", "item-b", "ev-3", `{"review_id":"rev-2","verdict":"keep","attempts":1,"cost_status":"usage_recorded"}`),
		reviewEvent("rev-3", "item-c", "ev-4", `{"review_id":"rev-3","verdict":"keep","failure":"timeout","attempts":2,"cost_status":"unknown"}`),
		reviewEvent("rev-4", "item-d", "ev-5", `{"review_id":"rev-4","verdict":"not_started","cost_status":"not_sent"}`),
		keepEvent("item-c", "ev-6", `{"review_id":"rev-3","model_error":"timeout","review":{"http_attempts":2,"cost_status":"unknown"}}`),
	}
	var got Summary
	tallyReviews(&got, events)
	want := Summary{Reviewed: 2, ReviewFailed: 1, NotStarted: 1, Attempts: 5, CostUnknown: 1}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

// An unknown count must not be padded with a zero. A review whose request facts
// were not recorded is counted as unknown and excluded from the measured HTTP
// attempt total, rather than added as "0 attempts".
func TestUnknownRequestFactsAreNotZeroFilled(t *testing.T) {
	events := []store.Event{
		reviewEvent("rev-x", "item-x", "ev-x", `{"review_id":"rev-x","verdict":"keep","failure":"transport"}`),
		keepEvent("item-y", "ev-y", `{"review":{"http_attempts":0,"cost_status":"unknown"}}`),
	}
	var got Summary
	tallyReviews(&got, events)
	if got.FactsUnknown != 2 {
		t.Fatalf("unknown request facts = %d, want 2: %+v", got.FactsUnknown, got)
	}
	if got.Attempts != 0 {
		t.Fatalf("an unknown review contributed a measured attempt count: %+v", got)
	}
	if got.CostUnknown != 1 || got.ReviewFailed != 1 {
		t.Fatalf("the failure and the unknown cost were lost: %+v", got)
	}
}

// Pre-correlation records still produce an honest page, de-duplicated by the
// best key they have instead of being silently dropped or merged into one.
func TestLegacyKeepRecordsAreStillTalliedOncePerItem(t *testing.T) {
	body := `{"reason":"kept_by_model","review":{"http_attempts":2,"cost_status":"usage_recorded"}}`
	events := []store.Event{keepEvent("item-old", "ev-1", body), keepEvent("item-old", "ev-2", body)}
	var got Summary
	tallyReviews(&got, events)
	if got.Reviewed != 1 || got.Attempts != 2 {
		t.Fatalf("one legacy review was reported twice: %+v", got)
	}
}

func TestAKeptEntryCarriesItsCorrelationID(t *testing.T) {
	body := `{"review_id":"rev-keep","reason":"not_started_budget","review":{"http_attempts":0,"cost_status":"not_sent"}}`
	ev := keepEvent("item-keep", "ev-keep", body)
	entry, ok := entryOf(sectionKept, itemWithPath("a.zip", 10, domain.StatePlanned), ev)
	if !ok {
		t.Fatal("a keep was not reported")
	}
	if entry.ReviewID != "rev-keep" || entry.ReviewState != domain.ReviewNotStartedBudget {
		t.Fatalf("the keep lost its correlation or its state: %+v", entry)
	}
	md := Render(Page{GeneratedAtNS: 1, WindowStartNS: 1,
		Kept: Section{Entries: []Entry{entry}, Total: 1}})
	if !strings.Contains(md, "rev-keep") {
		t.Fatalf("the page does not expose the review id:\n%s", md)
	}
}

func TestRenderStatesUnknownFactsWithoutPaddingTheAttemptTotal(t *testing.T) {
	md := Render(Page{GeneratedAtNS: 1, WindowStartNS: 1,
		Summary: Summary{Reviewed: 1, FactsUnknown: 2, Attempts: 3}})
	for _, want := range []string{"请求事实未记录：2", "本窗口 HTTP 尝试：3 次", "也不能按 0 理解"} {
		if !strings.Contains(md, want) {
			t.Fatalf("the page does not contain %q:\n%s", want, md)
		}
	}
}
