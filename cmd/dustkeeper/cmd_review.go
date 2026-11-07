package main

import (
	"context"
	"fmt"
	"io"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
)

// cmdReview manages review requests that were sent and never answered.
//
// The list is not a diagnostic nicety: those rows are what stops the next run
// from paying for the same question again. Without a way to read and settle
// them, the only escape would be deleting the state, which is exactly the
// history-erasing move this build forbids.
func cmdReview(ctx context.Context, a *app, args []string) (output, error) {
	if len(args) == 0 {
		return output{}, errs.New(errs.CodeInvalidArgument, "review requires a subcommand: unknown list | unknown resolve")
	}
	if args[0] != "unknown" {
		return output{}, errs.Newf(errs.CodeInvalidArgument, "unknown review subcommand %q", args[0])
	}
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
