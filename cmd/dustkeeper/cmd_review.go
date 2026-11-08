package main

import (
	"context"
	"fmt"
	"io"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// cmdReview manages review requests that were sent and never answered.
//
// The list is not a diagnostic nicety: those rows are what stops the next run
// from paying for the same question again. Without a way to read and settle
// them, the only escape would be deleting the state, which is exactly the
// history-erasing move this build forbids.
func cmdReview(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument,
			"review requires a subcommand: unknown list | unknown resolve | dispatch list | dispatch resolve")
	}
	switch args[0] {
	case "unknown":
		if len(args) == 1 {
			return output{}, errs.New(errs.CodeInvalidArgument, "review unknown requires list or resolve")
		}
		switch args[1] {
		case "list":
			// Both branches read or write the same state database. withInstance only
			// takes the instance lock and opens the operational log; it does not open
			// the store. Without this call the command dereferences a nil store and
			// crashes instead of reporting a missing or damaged instance.
			if err := a.openStore(); err != nil {
				return output{}, err
			}
			return reviewUnknownList(ctx, a, args[2:])
		case "resolve":
			if err := a.openStore(); err != nil {
				return output{}, err
			}
			return reviewUnknownResolve(ctx, a, args[2:])
		}
		return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown review unknown subcommand %q", args[1])
	case "dispatch":
		if len(args) < 2 {
			return output{}, errs.New(errs.CodeInvalidArgument, "review dispatch requires list or resolve")
		}
		switch args[1] {
		case "list":
			if err := a.openStore(); err != nil {
				return output{}, err
			}
			return reviewDispatchList(ctx, a, args[2:])
		case "resolve":
			// reviewDispatchResolve validates every flag before it opens the
			// state database, so a missing confirmation/note or an unchecked
			// status never touches the database at all.
			return reviewDispatchResolve(ctx, a, args[2:])
		}
		return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown review dispatch subcommand %q", args[1])
	}
	return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown review subcommand %q", args[0])
}

// reviewDispatchList shows prepared requests. The default view is exactly the
// unresolved set - pending without a manual resolution - i.e. the rows that
// still block a re-ask. --all shows every row with the original request facts
// and any independent manual conclusion kept apart.
func reviewDispatchList(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("review dispatch list", io.Discard)
	all := fs.Bool("all", false, "include settled/refused rows and independent manual resolutions")
	limit := fs.Int("limit", 200, "maximum rows to show")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	rows, err := a.store.ListReviewDispatch(ctx, *all, *limit)
	if err != nil {
		return output{}, err
	}
	out := ok()
	out.Data = map[string]any{"requests": rows, "count": len(rows)}
	if len(rows) == 0 {
		if *all {
			out.Human = []string{"no review dispatches"}
		} else {
			out.Human = []string{"no unresolved review dispatches (pending without a manual resolution)"}
		}
		return out, nil
	}
	if *all {
		out.Human = append(out.Human, fmt.Sprintf("%d review dispatch(es) - all states:", len(rows)))
	} else {
		out.Human = append(out.Human, fmt.Sprintf("%d unresolved review dispatch(es) (pending, no manual resolution):", len(rows)))
	}
	for _, r := range rows {
		// The copyable dispatch id comes first so the operator can paste it into
		// the resolve command. These are the facts recorded when the request was
		// prepared (or settled); they are never rewritten by a manual resolution.
		out.Human = append(out.Human, fmt.Sprintf(
			"  dispatch_id=%s  item=%s  action=%s  state=%s  run=%s  review_id=%s",
			r.DispatchID, r.ItemID, r.Action, r.State, r.RunID, r.ReviewID))
		out.Human = append(out.Human, fmt.Sprintf(
			"      recorded when prepared: attempts=%d failure=%s cost=%s",
			r.Attempts, r.Failure, r.CostStatus))
		if r.Resolution != nil {
			// Independent, manual conclusion - deliberately shown as a separate
			// line, never mixed into the preparation-time facts above.
			out.Human = append(out.Human, fmt.Sprintf(
				"      checked by a person later: request_status=%s billing_status=%s resolved_ns=%d note=%q",
				r.Resolution.RequestStatus, r.Resolution.BillingStatus, r.Resolution.ResolvedNS, r.Resolution.Note))
		} else {
			out.Human = append(out.Human,
				"      -> dustkeeper review dispatch resolve --dispatch "+shellQuote(r.DispatchID)+
					" --request-status <not_sent|completed|failed> --billing-status <not_charged|charged> --note \"...\" --confirm")
		}
	}
	out.Next = []string{
		"dustkeeper review dispatch list --all",
		"dustkeeper events --item <item_id>",
	}
	return out, nil
}

// reviewDispatchResolve records a person's reconciliation of exactly one
// prepared request. It lifts that one request's hold; it does not call a model,
// move a file, add allowance or claim the old review approved deletion.
//
// Every flag is validated before the state database is opened, so each refusal
// leaves the database byte-for-byte unchanged. The write itself is one
// transaction in the store (resolution + independent event), under the instance
// lock.
func reviewDispatchResolve(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("review dispatch resolve", io.Discard)
	dispatch := fs.String("dispatch", "", "dispatch id of the pending request (copy it from 'review dispatch list')")
	requestStatus := fs.String("request-status", "", "checked request result: not_sent | completed | failed")
	billingStatus := fs.String("billing-status", "", "checked billing result: not_charged | charged")
	note := fs.String("note", "", "what was checked, the evidence, and why asking again is allowed")
	confirm := fs.Bool("confirm", false, "confirm that a new request may be made for this dispatch")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *dispatch == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "review dispatch resolve needs --dispatch <dispatch_id>")
	}
	if !*confirm {
		return output{}, errs.New(errs.CodeNeedsInput,
			"resolving lets the next run ask again; repeat with --confirm plus the checked --request-status, --billing-status and --note")
	}
	if err := store.ValidateDispatchResolution(*requestStatus, *billingStatus, *note); err != nil {
		return output{}, err
	}
	// Only now, after every input has been checked, is the store opened.
	if err := a.openStore(); err != nil {
		return output{}, err
	}
	return a.withInstance(func() (output, error) {
		now := a.clock.Wall().UnixNano()
		res := store.DispatchResolution{
			ResolvedNS:    now,
			RequestStatus: *requestStatus,
			BillingStatus: *billingStatus,
			Note:          *note,
		}
		ev := store.NewReviewDispatchResolutionEvent(*dispatch, res, now)
		if err := a.store.ResolveReviewDispatch(ctx, *dispatch, res, ev); err != nil {
			return output{}, err
		}
		// The constraints are part of the output in BOTH modes: the human lines
		// and the JSON `data.notice` are the same slice, so a script cannot be
		// told less than the operator at the terminal.
		notice := []string{
			"this does NOT call a model, move files, add allowance, or mean the old review approved deletion",
			"the next run still goes through the normal budget, identity, protection, rule and model gates; a new call produces a new request/review ID",
			"verify the approval to ask again and the remaining allowance yourself before the next run",
		}
		// Other unresolved rows keep their own hold. Say so instead of promising
		// that the next run will definitely be able to ask again.
		others, err := a.store.ListReviewDispatch(ctx, false, 2)
		if err != nil {
			return output{}, err
		}
		unknowns, err := a.store.ListReviewUnknown(ctx, false, 2)
		if err != nil {
			return output{}, err
		}
		if len(others) > 0 || len(unknowns) > 0 {
			notice = append(notice,
				"other unresolved requests still exist; the next run is NOT guaranteed to be able to ask again")
		}
		out := ok()
		out.Data = map[string]any{
			"dispatch_id":    *dispatch,
			"resolved_ns":    now,
			"request_status": *requestStatus,
			"billing_status": *billingStatus,
			"note":           *note,
			"notice":         notice,
		}
		out.Human = append([]string{
			"recorded a manual resolution for dispatch " + *dispatch + "; the original request row is unchanged",
		}, notice...)
		out.Next = []string{
			"dustkeeper review dispatch list --all",
			"dustkeeper status --json",
		}
		return out, nil
	})
}

func reviewUnknownList(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("review unknown list", io.Discard)
	all := fs.Bool("all", false, "include requests that were already resolved")
	limit := fs.Int("limit", 200, "maximum rows to show (1..500)")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	rows, err := a.store.ListReviewUnknown(ctx, *all, *limit)
	if err != nil {
		return output{}, err
	}
	out := ok()
	out.Data = map[string]any{"requests": rows, "count": len(rows)}
	if len(rows) == 0 {
		out.Human = []string{"no unresolved review requests"}
		return out, nil
	}
	out.Human = append(out.Human, fmt.Sprintf("%d unresolved review request(s):", len(rows)))
	for _, r := range rows {
		state := "open"
		if r.ResolvedNS != nil {
			state = "resolved"
		}
		out.Human = append(out.Human, fmt.Sprintf("  %s  %s  action=%s attempts=%d failure=%s cost=%s",
			r.ItemID, r.Path, r.Action, r.Attempts, r.Failure, r.CostStatus))
		out.Human = append(out.Human, "      state="+state+"  -> dustkeeper review unknown resolve --item "+string(r.ItemID)+" --action "+r.Action)
	}
	out.Next = []string{"dustkeeper review unknown resolve --item <item_id> --action <action>"}
	return out, nil
}

func reviewUnknownResolve(ctx context.Context, a *app, args []string) (output, error) {
	fs := flagset("review unknown resolve", io.Discard)
	item := fs.String("item", "", "item id of the unresolved request")
	action := fs.String("action", "", "review action: delete | purge | quarantine")
	note := fs.String("note", "", "what was checked (recorded with the resolution)")
	confirm := fs.Bool("confirm", false, "confirm that a new request may be made for this object and action")
	if err := parseFlags(fs, args); err != nil {
		return output{}, err
	}
	if *item == "" || *action == "" {
		return output{}, errs.New(errs.CodeInvalidArgument, "review unknown resolve needs --item and --action")
	}
	if !*confirm {
		return output{}, errs.New(errs.CodeNeedsInput,
			"resolving allows one more request for this object; repeat with --confirm (and --note describing what was checked)")
	}
	if *note == "" {
		return output{}, errs.New(errs.CodeNeedsInput, "say what was checked with --note; an empty note is not a record")
	}
	return a.withInstance(func() (output, error) {
		now := a.clock.Wall().UnixNano()
		if err := a.store.ResolveReviewUnknown(ctx, domain.ItemID(*item), *action, *note, now); err != nil {
			return output{}, err
		}
		out := ok()
		out.Data = map[string]any{"item_id": *item, "action": *action, "resolved_at_ns": now, "note": *note}
		out.Human = []string{
			"resolved the unresolved review request for " + *item + " (" + *action + ")",
			"the next run may ask again, once; the earlier request stays on record",
		}
		out.Next = []string{"dustkeeper run"}
		return out, nil
	})
}
