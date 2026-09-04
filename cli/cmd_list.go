package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

// stringSliceFlag accumulates repeated --tag values.
type stringSliceFlag []string

func (f *stringSliceFlag) String() string { return strings.Join(*f, ",") }
func (f *stringSliceFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func (app *App) cmdList(ctx context.Context, args []string) int {
	fs := app.newFlagSet("list")
	of := addOutputFlags(fs)
	scopeStr := fs.String("scope", "processable", "listing scope: all|open|processable|workable|free|auto")
	var tags stringSliceFlag
	fs.Var(&tags, "tag", "filter by tag (repeatable, conjunctive)")
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 0 {
		return app.usageError("list: unexpected argument %q (this command takes no arguments)", fs.Arg(0))
	}
	mode, ok := of.mode(app, "list")
	if !ok {
		return 2
	}

	scope := flow.ItemScope(*scopeStr)
	if !flow.ValidScope(scope) {
		return app.usageError("list: unknown scope %q (valid: all|open|processable|workable|free|auto)", *scopeStr)
	}

	want, ok := app.tagFilter("list", tags)
	if !ok {
		return 1
	}

	acceptsType := func(t flow.ItemType) bool { return flowForType(app, t) != nil }
	items, err := app.Orchestrator.List(ctx, scope, flow.BinaryName(app.Name), acceptsType)
	if err != nil {
		fmt.Fprintln(app.Err, "list:", err)
		return 1
	}

	payload := listPayload{
		Scope: string(scope),
		Items: make([]listItemPayload, 0, len(items)),
	}
	for _, it := range items {
		// flow.TagsMatch is THE comparison — the same one the orchestrator's own
		// auto-selection post-filters with. Anything looser here and one --tag
		// value means two different things across `list` and `resolve`, which
		// are meant to read as symmetrical.
		if !flow.TagsMatch(it.Tags, want) {
			continue
		}
		payload.Items = append(payload.Items, listItemPayload{
			Display:      it.Ref.Display,
			Title:        it.Title,
			Owner:        string(it.Holder.Account),
			Backend:      string(it.Ref.OrchestratorName),
			Availability: string(it.Availability),
			Tags:         tagStrings(it.Tags),
			Blocked:      it.Blocked,
			BlockKind:    string(it.BlockKind),
			BlockReason:  it.BlockReason,
			BlockedBy:    blockerDisplays(it.BlockedBy),
		})
	}

	return app.emit(mode, payload, func() {
		if len(payload.Items) == 0 {
			fmt.Fprintf(app.Out, "(no items at scope %s)\n", scope)
			return
		}
		for _, it := range payload.Items {
			owner := it.Owner
			if owner == "" {
				owner = "—"
			}
			avail := it.Availability
			if avail == "" {
				avail = "?"
			}
			fmt.Fprintf(app.Out, "%s\t%s\t%s\n", it.Display, avail, owner)
		}
	})
}

// tagFilter validates the operator's --tag values into TagIds.
//
// A value below the floor is REFUSED rather than interpolated: a tag reaches
// the orchestrator's own query, where a value carrying a space does not fail —
// it silently becomes a different query, and the operator gets a plausible
// wrong answer instead of an error.
func (app *App) tagFilter(cmd string, tags []string) ([]flow.TagId, bool) {
	out := make([]flow.TagId, 0, len(tags))
	for _, t := range tags {
		id := flow.TagId(t)
		if !id.Valid() {
			fmt.Fprintf(app.Err, "%s: %q is not a valid tag — a tag is non-empty, single-line, and carries no leading or trailing whitespace\n", cmd, t)
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

func tagStrings(tags []flow.TagId) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, string(t))
	}
	return out
}

// blockerDisplays renders the blockers still OPEN for the listing line.
//
// BlockedBy carries every blocker ever declared, so a non-empty list on an
// unblocked item simply means they have all finished — printing those would
// tell an operator to go work something that is already done.
func blockerDisplays(blockers []flow.Blocker) []string {
	var out []string
	for _, b := range blockers {
		if b.Status != flow.StatusTerminal {
			out = append(out, b.Ref.Display)
		}
	}
	return out
}
