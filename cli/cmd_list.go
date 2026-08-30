package cli

import (
	"context"
	"fmt"
	"slices"
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

	scope := flow.DiscoveryScope(*scopeStr)
	if !flow.ValidScope(scope) {
		return app.usageError("list: unknown scope %q (valid: all|open|processable|workable|free|auto)", *scopeStr)
	}

	// When the backend implements Discoverer, use it for the full listing.
	if disc, ok := app.Backend.(flow.Discoverer); ok {
		return app.listViaDiscoverer(ctx, disc, scope, tags, mode)
	}

	// Fallback: legacy ListEligible path (no --scope, no --tag, no tags).
	if scope != flow.ScopeProcessable && scope != flow.ScopeAuto {
		fmt.Fprintln(app.Err, "list: this backend does not support --scope (no Discoverer capability)")
		return 1
	}
	if len(tags) > 0 {
		fmt.Fprintln(app.Err, "list: this backend does not support --tag (no Discoverer capability)")
		return 1
	}
	return app.listViaEligible(ctx, mode, scope)
}

// listViaDiscoverer uses the Discoverer interface.
func (app *App) listViaDiscoverer(ctx context.Context, disc flow.Discoverer, scope flow.DiscoveryScope, tags []string, mode OutputMode) int {
	items, err := disc.Discover(ctx, scope, app.Name)
	if err != nil {
		fmt.Fprintln(app.Err, "list:", err)
		return 1
	}

	// Apply tag filter (conjunctive).
	if len(tags) > 0 {
		items = filterByTags(items, tags)
	}

	payload := listPayload{
		Scope: string(scope),
		Items: make([]listItemPayload, 0, len(items)),
	}
	for _, di := range items {
		payload.Items = append(payload.Items, listItemPayload{
			Display:      di.Display,
			Title:        di.Title,
			Owner:        di.Holder,
			Backend:      di.BackendName,
			Availability: string(di.Availability),
			Tags:         di.Tags,
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

// listViaEligible is the legacy path for backends without Discoverer.
func (app *App) listViaEligible(ctx context.Context, mode OutputMode, scope flow.DiscoveryScope) int {
	refs, err := app.Backend.ListEligible(ctx)
	if err != nil {
		fmt.Fprintln(app.Err, "list:", err)
		return 1
	}

	payload := listPayload{
		Scope: string(scope),
		Items: make([]listItemPayload, 0, len(refs)),
	}
	for _, r := range refs {
		owner := ""
		if info, _ := app.Backend.LookupClaim(ctx, r); info != nil {
			owner = info.Owner
		}
		payload.Items = append(payload.Items, listItemPayload{
			Display:      r.Display,
			Owner:        owner,
			Backend:      r.BackendName,
			Availability: string(flow.AvailAuto), // ListEligible only returns auto-selectable items
		})
	}

	return app.emit(mode, payload, func() {
		if len(payload.Items) == 0 {
			fmt.Fprintln(app.Out, "(no eligible items)")
			return
		}
		for _, it := range payload.Items {
			owner := it.Owner
			if owner == "" {
				owner = "—"
			}
			fmt.Fprintf(app.Out, "%s\t%s\n", it.Display, owner)
		}
	})
}

// filterByTags returns items carrying ALL given tags.
func filterByTags(items []flow.DiscoveryItem, tags []string) []flow.DiscoveryItem {
	out := make([]flow.DiscoveryItem, 0, len(items))
	for _, di := range items {
		if hasAllTags(di.Tags, tags) {
			out = append(out, di)
		}
	}
	return out
}

func hasAllTags(itemTags, wanted []string) bool {
	for _, w := range wanted {
		if !slices.Contains(itemTags, w) {
			return false
		}
	}
	return true
}
