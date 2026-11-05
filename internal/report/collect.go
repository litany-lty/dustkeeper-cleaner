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
