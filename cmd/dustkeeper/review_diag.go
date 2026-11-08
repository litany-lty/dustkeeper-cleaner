package main

// The review record in the operational log.
//
// One line per prepared request, and one line for a candidate that was held or
// not started. The point is correlation: a person reading yesterday's run can
// follow run_id -> review_id -> the matching review_dispatch row and decision
// event without guessing which of several model calls belongs to which item.
//
// The same "no payload" rule as the rest of the diagnostic log applies. There
// is no prompt, no response body, no credential and no file path here. Budget is
// a short JSON summary of numeric allowances, and the note is a reason code or
// a bounded local error. The logger caps every string value anyway.

import (
	"encoding/json"

	"dustkeeper/internal/execute"
	"dustkeeper/migrations"
)

// reviewDiag adapts one bounded review fact to the structured operational log.
// A nil logger drops the record instead of panicking, which is what debug
// commands and tests without a state directory do.
func (a *app) reviewDiag() func(execute.ReviewDiag) {
	return func(d execute.ReviewDiag) {
		if a.log == nil {
			return
		}
		runID := d.RunID
		if runID == "" {
			runID = a.runID
		}
		kv := []any{
			"phase", "review",
			"version", version,
			"commit", commit,
			"schema", migrations.CurrentVersion,
			"run_id", runID,
			"instance", a.instance,
			"command", a.cmd,
			"review_id", d.ReviewID,
			"item_id", d.ItemID,
			"action", d.Action,
			"state", d.State,
			"journal", d.Journal,
			"attempts", d.Attempts,
			"failure", d.Failure,
			"cost_status", d.CostStatus,
			"elapsed_ms", d.ElapsedMS,
			"http_status", d.HTTPStatus,
			"input_tokens", d.InputTokens,
			"output_tokens", d.OutputTokens,
			"budget", d.Budget,
			"note", d.Note,
		}
		// Timings are emitted only when requests actually happened and as a
		// bounded JSON string of the restricted RequestTiming fields. The line
		// keeps its no-payload rule: no URL, proxy, header, body or credential.
		if len(d.RequestTimings) > 0 {
			if raw, err := json.Marshal(d.RequestTimings); err == nil {
				kv = append(kv, "request_timings", string(raw))
			}
		}
		a.log.Log("review", kv...)
	}
}
