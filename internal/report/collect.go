// Package report answers the question a user actually asks after a run: what
// happened to my files.
//
// It reads the same two tables everything else writes to - items and events - and
// it counts only actions that finished and were confirmed. A plan, an intent, a
// model opinion and a debug simulation are all things that almost happened; none
// of them belongs on a page that says "deleted".
package report

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/outcome"
	"dustkeeper/internal/review"
	"dustkeeper/internal/store"
)

// DefaultLimit is how many entries one section shows before it says how many more
// there are. R30 asks for the total and the remainder, not for a longer page.
const DefaultLimit = 200

// Entry is one line of one section.
type Entry struct {
	// ItemID is the object identifier the restore command takes.
	//
	// It is not the operation: `quarantine restore --object` resolves an item,
	// and an operation is the record of one attempt on it. The page used to
	// print the operation id there, so the recovery command it offered could
	// never work - and a test that only compared the string kept it that way.
	ItemID string `json:"item_id,omitempty"`
	// Path is the original relative path, as the user knew it.
	Path string `json:"path"`
	// SizeBytes is the logical size the operation recorded, not a measurement of
	// what was on disk at page-render time.
	SizeBytes int64  `json:"size_bytes"`
	ScopeID   string `json:"scope_id"`
	// AtNS is when the action finished: the deletion completion, the move, or the
	// moment the item was left alone.
	AtNS int64 `json:"at_ns"`
	// PurgeAfterNS is the earliest instant the object may be cleared. It is
	// present only for the observation section.
	PurgeAfterNS *int64 `json:"purge_after_ns,omitempty"`
	// Reason is the machine code; ReasonText is the sentence. Both are kept
	// because a person reads one and a script greps the other.
	Reason     string `json:"reason,omitempty"`
	ReasonText string `json:"reason_text,omitempty"`
	// ModelFailed separates "the reviewers could not answer" from "the reviewers
	// said keep". They are different promises and the page must not blur them.
	ModelFailed bool `json:"model_failed,omitempty"`
	// Operation is the intent that produced this line, so a reader can trace one
	// row back to its record.
	Operation string `json:"operation,omitempty"`
	// ReviewID is the correlation id of the one prepared request behind this
	// line. It is the key that joins the result page, the review_dispatch row,
	// the decision event and the operational log.
	ReviewID string `json:"review_id,omitempty"`
	// Review facts, copied from the record. A line that says "kept" without
	// saying whether anything was asked is the ambiguity this field removes.
	Failure     string `json:"review_failure,omitempty"`
	Attempts    int    `json:"http_attempts,omitempty"`
	CostStatus  string `json:"cost_status,omitempty"`
	ReviewState string `json:"review_state,omitempty"`
	// RequestTimings are the measured, payload-free timings of the prepared
	// request correlated by ReviewID. They come from the settlement event, not
	// from the keep/completion record, and an unrecorded timing is never
	// invented as a zero.
	RequestTimings []review.RequestTiming `json:"request_timings,omitempty"`
}

// Summary counts what the three sections cannot: reviews that produced no
// decision. They are not keeps and they are not failures of a file action.
type Summary struct {
	Reviewed int `json:"reviewed"`
	// NotStarted counts reviews that never sent a request: a spent allowance or
	// a local refusal before the first provider call.
	NotStarted   int `json:"review_not_started"`
	UnknownHeld  int `json:"review_unknown_held"`
	ReviewFailed int `json:"review_failed_kept"`
	Attempts     int `json:"http_attempts"`
	CostUnknown  int `json:"cost_unknown"`
	// FactsUnknown counts review records whose request facts are not measured.
	// They are excluded from Attempts rather than added as zero: a zero would
	// read as "we measured and nothing was sent", which is exactly what cannot
	// be claimed for these records.
	FactsUnknown int `json:"review_facts_unknown"`
}

// Section is one of the three lists, with what did not fit.
type Section struct {
	Entries []Entry `json:"entries"`
	// Total is how many entries the run really produced, before the display limit.
	Total int `json:"total"`
	// More is Total minus what is shown; it is what the page prints as "还有 N 条".
	More int `json:"more"`
}

// Page is the whole result page.
type Page struct {
	GeneratedAtNS int64 `json:"generated_at_ns"`
	// WindowStartNS and WindowEndNS say which slice of history this page covers.
	// A page without a window would be a description of everything that ever
	// happened, which is not what "this run" means.
	WindowStartNS int64   `json:"window_start_ns"`
	WindowEndNS   int64   `json:"window_end_ns"`
	RunID         string  `json:"run_id,omitempty"`
	Deleted       Section `json:"deleted"`
	Observed      Section `json:"to_be_deleted"`
	Kept          Section `json:"kept"`
	Summary       Summary `json:"review_summary"`
}

// Window selects the slice of history one page describes.
type Window struct {
	StartNS int64
	EndNS   int64
	RunID   string
	// Limit is the per-section display cap. Zero means DefaultLimit.
	Limit int
}

func (w Window) limit() int {
	if w.Limit <= 0 {
		return DefaultLimit
	}
	return w.Limit
}

// Collect reads the results of one window out of the store.
//
// The filter is the whole point of this function: an event counts only when its
// phase is a completion, and an item counts only when the state the record holds
// still agrees with that completion. A quarantine that was later restored is not
// "waiting to be deleted", and a deletion whose completion event is missing is not
// a deletion.
func Collect(ctx context.Context, st *store.Store, w Window) (Page, error) {
	page := Page{GeneratedAtNS: w.EndNS, WindowStartNS: w.StartNS, WindowEndNS: w.EndNS, RunID: w.RunID}
	if w.EndNS == 0 {
		page.GeneratedAtNS = time.Now().UnixNano()
		page.WindowEndNS = page.GeneratedAtNS
	}

	events, err := eventsInWindow(ctx, st, w.StartNS, page.WindowEndNS)
	if err != nil {
		return page, err
	}
	// The review summary is computed over the whole window before the sections
	// are built. Successful and failed reviews both count, even when the answer
	// led to a move or a deletion rather than a keep; the settlement event is
	// the one authoritative record of each prepared request, keyed by review id.
	tallyReviews(&page.Summary, events)
	timingsByReview := reviewTimings(events)

	// One entry per item per section: several events can describe the same object
	// (a retry, a reconciliation), and a page that lists a file twice reads like
	// two files were deleted.
	seen := map[sectionKey]struct{}{}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.ItemID == nil {
			continue
		}
		section, ok := sectionOf(ev)
		if !ok {
			continue
		}
		key := sectionKey{section: section, item: *ev.ItemID}
		if _, dup := seen[key]; dup {
			continue
		}
		item, err := st.GetItem(ctx, *ev.ItemID)
		if err != nil {
			return page, errs.Wrapf(errs.CodeItemNotFound, err, "the result page cannot read item %s", *ev.ItemID)
		}
		entry, ok := entryOf(section, item, ev)
		if !ok {
			// The record no longer agrees with the completion: a restored object is
			// not waiting to be cleared, and a held one has no verdict.
			continue
		}
		seen[key] = struct{}{}
		// Timings are recorded on the settlement event, not on the
		// keep/completion record. Correlate them by the actual review id so a
		// section that names that id exposes the same measured rows.
		if t, ok := timingsByReview[entry.ReviewID]; ok {
			entry.RequestTimings = t
		}
		appendEntry(&page, section, entry)
	}

	// Oldest first inside each section: a page reads like a log, and the oldest
	// entry is the one the user is most likely to have forgotten.
	for _, s := range []*Section{&page.Deleted, &page.Observed, &page.Kept} {
		sort.SliceStable(s.Entries, func(i, j int) bool { return s.Entries[i].AtNS < s.Entries[j].AtNS })
		s.Total = len(s.Entries)
		if limit := w.limit(); s.Total > limit {
			s.More = s.Total - limit
			s.Entries = s.Entries[:limit]
		}
	}
	return page, nil
}

// eventsInWindow reads the audit log in pages and keeps the events whose recorded
// instant falls inside the window.
//
// Paging by sequence rather than by time is deliberate: the store exposes a keyset
// cursor, and sequence order is the order the operations happened. The instant is
// filtered afterwards rather than used as the cursor, because the wall clock can
// jump and a cursor built from it would then skip or repeat rows.
func eventsInWindow(ctx context.Context, st *store.Store, startNS, endNS int64) ([]store.Event, error) {
	const pageSize = 1000
	var out []store.Event
	var after int64
	for {
		page, err := st.ListEvents(ctx, pageSize, after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return out, nil
		}
		for _, ev := range page {
			after = ev.Sequence
			if ev.OccurredAtNS < startNS || ev.OccurredAtNS > endNS {
				continue
			}
			out = append(out, ev)
		}
		if len(page) < pageSize {
			return out, nil
		}
	}
}

type sectionKind int

const (
	sectionDeleted sectionKind = iota
	sectionObserved
	sectionKept
)

type sectionKey struct {
	item    domain.ItemID
	section sectionKind
}

// sectionOf decides which of the three lists an event belongs to, and whether it
// is a finished action at all.
func sectionOf(ev store.Event) (sectionKind, bool) {
	switch {
	case ev.Action == domain.ActionDelete || ev.Action == domain.ActionPurge:
		// Only a completion is a deletion. An intent, a park and a hold are all
		// stages of an attempt, and a debug record is a simulation.
		return sectionDeleted, ev.Phase == domain.PhaseComplete
	case ev.Action == domain.ActionQuarantine:
		return sectionObserved, ev.Phase == domain.PhaseComplete
	case ev.Action == "keep":
		return sectionKept, ev.Phase == domain.PhaseInfo
	}
	return 0, false
}

// entryOf builds one line, and reports whether the record still supports it.
func entryOf(section sectionKind, item store.Item, ev store.Event) (Entry, bool) {
	entry := Entry{
		ItemID:    string(item.ID),
		Path:      string(item.RawRelative),
		ScopeID:   string(item.ScopeID),
		SizeBytes: sizeOf(item),
		AtNS:      ev.OccurredAtNS,
	}
	if ev.OpID != nil {
		entry.Operation = string(*ev.OpID)
	}
	switch section {
	case sectionDeleted:
		// A deletion that the record does not call purged is not one: a hold, a
		// park and a replaced source all leave the item somewhere else.
		if item.State != domain.StatePurged {
			return Entry{}, false
		}
		entry.Reason, entry.ReasonText = reasonFromEvent(ev)
	case sectionObserved:
		if item.State != domain.StateQuarantined {
			// Restored, held or already cleared: it is no longer waiting.
			return Entry{}, false
		}
		entry.PurgeAfterNS = item.PurgeNotBeforeNS
		entry.Reason, entry.ReasonText = reasonFromEvent(ev)
	case sectionKept:
		entry.Reason, entry.ReasonText, entry.ModelFailed = keepReason(ev)
		entry.ReviewState = reviewStateOf(entry.Reason)
		fillReview(&entry, ev)
	}
	// A completed action also carries the review it was approved under when the
	// record says so, so the same id can be followed from any section.
	if entry.ReviewID == "" {
		entry.ReviewID = reviewIDOf(ev)
	}
	return entry, true
}

// reasonFromEvent reads the reason the operation recorded for itself.
func reasonFromEvent(ev store.Event) (string, string) {
	if ev.ResultJSON == nil {
		return "", ""
	}
	var payload struct {
		Reason     string `json:"reason"`
		ReasonText string `json:"reason_text"`
	}
	if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
		return "", ""
	}
	text := payload.ReasonText
	if text == "" {
		text = outcome.ReasonText(payload.Reason)
	}
	return payload.Reason, text
}

// keepReason reads a keep record, including whether the reviewers failed.
func keepReason(ev store.Event) (code, text string, modelFailed bool) {
	if ev.ResultJSON == nil {
		return "", "", false
	}
	var payload struct {
		Reason     string `json:"reason"`
		ReasonText string `json:"reason_text"`
		ModelError string `json:"model_error"`
	}
	if err := json.Unmarshal([]byte(*ev.ResultJSON), &payload); err != nil {
		return "", "", false
	}
	text = payload.ReasonText
	if text == "" {
		text = outcome.ReasonText(payload.Reason)
	}
	return payload.Reason, text, payload.ModelError != ""
}

// reviewFacts is the settlement record of one prepared request. It is written
// in the same transaction as the decision event, so one row here is one review,
// whether the answer was a keep, a move, a deletion or a failure.
type reviewFacts struct {
	ReviewID   string `json:"review_id"`
	Verdict    string `json:"verdict"`
	Failure    string `json:"failure"`
	Attempts   int    `json:"attempts"`
	CostStatus string `json:"cost_status"`
}

// reviewIDOf finds the correlation id in either half of an event. Old records
// have none; they are de-duplicated by item and action instead of being dropped
// or, worse, merged into one invented review.
func reviewIDOf(ev store.Event) string {
	var body struct {
		ReviewID string `json:"review_id"`
	}
	if ev.ResultJSON != nil && json.Unmarshal([]byte(*ev.ResultJSON), &body) == nil && body.ReviewID != "" {
		return body.ReviewID
	}
	if ev.ContractJSON != "" && json.Unmarshal([]byte(ev.ContractJSON), &body) == nil {
		return body.ReviewID
	}
	return ""
}

// reviewTimings indexes the per-request timings that settlement events carry
// under the actual review id. A keep/completion record only stores request
// counts and cost, so timings must be read from the settlement event that the
// dispatcher wrote in the same transaction. Only events that really recorded
// timings contribute: a missing field stays absent, never a zero-filled row.
func reviewTimings(events []store.Event) map[string][]review.RequestTiming {
	out := map[string][]review.RequestTiming{}
	for _, ev := range events {
		if ev.Action != "review" || ev.Phase != domain.PhaseInfo || ev.ResultJSON == nil {
			continue
		}
		var body struct {
			RequestTimings []review.RequestTiming `json:"request_timings"`
		}
		if json.Unmarshal([]byte(*ev.ResultJSON), &body) != nil || len(body.RequestTimings) == 0 {
			continue
		}
		if id := reviewIDOf(ev); id != "" {
			out[id] = body.RequestTimings
		}
	}
	return out
}

// tallyReviews counts every review of the window once.
//
// Pass one reads settlement events ("review"/info), which cover successful and
// failed reviews alike and are keyed by review_id. Pass two reads the older
// per-item state events so a page built from an older database still says
// something true. A keep whose review_id was already counted in pass one is
// skipped, otherwise one review would be reported twice.
func tallyReviews(sum *Summary, events []store.Event) {
	byReviewID := map[string]struct{}{}

	for _, ev := range events {
		if ev.Action != "review" || ev.Phase != domain.PhaseInfo {
			continue
		}
		id := reviewIDOf(ev)
		key := id
		if key == "" {
			// A settlement without a correlation id is still one record. Its
			// event id keeps it distinct without merging it into another review.
			key = "event:" + ev.EventID
		}
		if _, dup := byReviewID[key]; dup {
			continue
		}
		byReviewID[key] = struct{}{}

		var f reviewFacts
		if ev.ResultJSON != nil {
			_ = json.Unmarshal([]byte(*ev.ResultJSON), &f)
		}
		switch {
		case f.Verdict == "not_started" || (f.CostStatus == "" && f.Attempts == 0 && f.Failure == ""):
			// A local refusal before any provider call, or a settlement whose
			// request facts were never recorded. Nothing was sent, so this is
			// not a provider failure and must not be counted as a judgment.
			sum.NotStarted++
		case f.CostStatus == "" && f.Attempts == 0:
			// A failed review whose request facts are missing. Adding zero to
			// the attempt total would turn "unknown" into "measured as none".
			sum.FactsUnknown++
			sum.ReviewFailed++
		default:
			if f.Attempts > 0 {
				sum.Attempts += f.Attempts
			}
			if f.CostStatus == "unknown" {
				sum.CostUnknown++
			}
			if f.Failure != "" {
				sum.ReviewFailed++
			} else {
				sum.Reviewed++
			}
		}
	}

	seenLegacy := map[string]struct{}{}
	for _, ev := range events {
		if ev.ItemID == nil {
			continue
		}
		key := string(*ev.ItemID) + "\x00" + ev.Action
		switch ev.Action {
		case "review":
			continue // settled in pass one
		case "review_not_started", "review_hold", "review_unknown":
			if _, dup := seenLegacy[key]; dup {
				continue
			}
			seenLegacy[key] = struct{}{}
			switch ev.Action {
			case "review_not_started":
				sum.NotStarted++
				var payload struct {
					Review struct {
						Attempts int `json:"http_attempts"`
					} `json:"review"`
				}
				if ev.ResultJSON != nil && json.Unmarshal([]byte(*ev.ResultJSON), &payload) == nil && payload.Review.Attempts > 0 {
					sum.Attempts += payload.Review.Attempts
				}
			case "review_hold":
				sum.UnknownHeld++
			case "review_unknown":
				sum.CostUnknown++
			}
		case "keep":
			if ev.Phase != domain.PhaseInfo || ev.ResultJSON == nil {
				continue
			}
			if id := reviewIDOf(ev); id != "" {
				if _, counted := byReviewID[id]; counted {
					continue
				}
				if _, dup := byReviewID["keep:"+id]; dup {
					continue
				}
				byReviewID["keep:"+id] = struct{}{}
			} else {
				if _, dup := seenLegacy[key]; dup {
					continue
				}
				seenLegacy[key] = struct{}{}
			}
			var payload struct {
				ModelError string `json:"model_error"`
				Review     struct {
					Attempts   int    `json:"http_attempts"`
					CostStatus string `json:"cost_status"`
				} `json:"review"`
			}
			if json.Unmarshal([]byte(*ev.ResultJSON), &payload) != nil {
				continue
			}
			if payload.Review.Attempts > 0 {
				sum.Attempts += payload.Review.Attempts
			} else if payload.Review.CostStatus == "unknown" {
				// Cost is unknown but the attempt count was not recorded. Do not
				// pad the measured total with a zero.
				sum.FactsUnknown++
			}
			if payload.Review.CostStatus == "unknown" {
				sum.CostUnknown++
			}
			if payload.ModelError != "" {
				sum.ReviewFailed++
			} else {
				sum.Reviewed++
			}
		}
	}
}

// reviewStateOf names the three "no conclusion" outcomes from the reason the
// keep record itself states. It is a rendering of a recorded reason, not an
// inference from a verdict.
func reviewStateOf(reason string) string {
	switch reason {
	case "not_started_budget":
		return domain.ReviewNotStartedBudget
	case "review_unknown":
		return domain.ReviewUnknownResult
	case "awaiting_request_check":
		return domain.ReviewUnknownHold
	}
	return ""
}

// fillReview copies the request facts and the correlation id a keep record
// carries. It intentionally does not read timings: a keep record stores only
// request counts and cost. The measured timings are joined by review id from
// the settlement event in Collect, so adding Decision.RequestTimings alone
// would not have populated this view.
func fillReview(entry *Entry, ev store.Event) {
	entry.ReviewID = reviewIDOf(ev)
	if ev.ResultJSON == nil {
		return
	}
	var payload struct {
		Review struct {
			Failure    string `json:"failure"`
			Attempts   int    `json:"http_attempts"`
			CostStatus string `json:"cost_status"`
		} `json:"review"`
	}
	if json.Unmarshal([]byte(*ev.ResultJSON), &payload) != nil {
		return
	}
	entry.Failure = payload.Review.Failure
	entry.Attempts = payload.Review.Attempts
	entry.CostStatus = payload.Review.CostStatus
}

// sizeOf is the logical size the item recorded when it was observed.
func sizeOf(item store.Item) int64 {
	var tok domain.ChangeToken
	if err := json.Unmarshal([]byte(item.PreTokenJSON), &tok); err != nil {
		return 0
	}
	if tok.Size < 0 {
		return 0
	}
	return tok.Size
}

func appendEntry(page *Page, section sectionKind, entry Entry) {
	switch section {
	case sectionDeleted:
		page.Deleted.Entries = append(page.Deleted.Entries, entry)
	case sectionObserved:
		page.Observed.Entries = append(page.Observed.Entries, entry)
	case sectionKept:
		page.Kept.Entries = append(page.Kept.Entries, entry)
	}
}
