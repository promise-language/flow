package cli

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
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
	scopeStr := fs.String("scope", "processable", "listing scope: all|open|processable|actionable|workable|free|auto")
	sortStr := fs.String("sort", string(sortResolution), "order: resolution|newest|oldest")
	limit := fs.Int("limit", 0, "print at most n items (default: unlimited)")
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
		return app.usageError("list: unknown scope %q (valid: all|open|processable|actionable|workable|free|auto)", *scopeStr)
	}

	order := listOrder(*sortStr)
	if !order.valid() {
		return app.usageError("list: unknown sort %q (valid: %s)", *sortStr, joinNames(allListOrders()))
	}

	// Whether the flag was GIVEN, not what it holds: the default is unlimited
	// and 0 is refused, so a sentinel value would be a number an operator could
	// type and be told is invalid while it is also the default.
	limited := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			limited = true
		}
	})
	if limited && *limit < 1 {
		return app.usageError("list: --limit must be a positive integer, got %d", *limit)
	}

	want, ok := app.tagFilter("list", tags)
	if !ok {
		return 1
	}

	// The remit gating a listing, which is what it is for
	// (docs/flow-registration.md § Item types): it answers *is this our work*
	// statically, without dispatching anything.
	items, err := app.Orchestrator.List(ctx, scope, flow.BinaryName(app.Name), app.Flow.InRemit, app.assumesRole(ctx))
	if err != nil {
		fmt.Fprintln(app.Err, "list:", err)
		return 1
	}

	// The run this arena is executing right now, read ONCE for the whole
	// listing rather than per row. It is the same record `status` reads for the
	// same purpose, and the same liveness rule applies: a record naming a dead
	// process is not evidence that anything is running.
	//
	// Best-effort: a read that fails leaves every row without the `in progress`
	// mark, which is what the row printed before this column existed.
	running, _ := clistate.LoadRunning()

	// This arena, as the SDK's one derivation names it — the same value an
	// orchestrator stamps a lease with, so "is this arena the holder?" is a
	// comparison rather than a guess.
	self := arenaKey(flow.ArenaAt(app.Orchestrator.ArenaRoot()))

	payload := listPayload{
		Scope: string(scope),
		Sort:  string(order),
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
			FiledAt:      it.FiledAt,
			Owner:        string(it.Holder.Account),
			Arena:        arenaKey(it.Holder.Arena),
			InProgress:   runObserved(it, self, running),
			ParkKind:     string(it.ParkKind),
			Backend:      string(it.Ref.OrchestratorName),
			Availability: string(it.Availability),
			Tags:         tagStrings(it.Tags),
			Priority:     string(it.Priority),
			Urgency:      string(it.Urgency),
			blockPayload: blockPayloadOf(it.Blocked, it.BlockKind, it.BlockReason, it.BlockedBy),
		})
	}

	// Ordered BEFORE the cut and before either rendering, so `--json` and the
	// human listing come out in one order and are cut alike. What `--json`
	// keeps unchanged is the values, not the order.
	order.sort(payload.Items)
	payload.Matched = len(payload.Items)
	if limited && *limit < len(payload.Items) {
		payload.Items = payload.Items[:*limit]
	}

	return app.emit(mode, payload, func() {
		if len(payload.Items) == 0 {
			fmt.Fprintf(app.Out, "(no items at scope %s)\n", scope)
			return
		}
		// The header names every column, because the fifth cell reading "—" on
		// nearly every row is unreadable without one. It is printed only over a
		// non-empty listing: there is nothing to name the columns of when there
		// are no columns.
		rows := [][]string{listHeader}
		for _, it := range payload.Items {
			rows = append(rows, listRow(it, self))
		}
		alignedRows(app.Out, rows, listColumnBounds)
		// The block's reason is prose for a person, so it gets no column of its
		// own — a free-text cell in the middle of an aligned table sets that
		// column for the whole listing. It goes under its row, through the one
		// renderer `status` uses for the same fact.
		//
		// Printed after the table rather than interleaved, because a
		// continuation line inside the rows would be measured as a row.
		for _, it := range payload.Items {
			if !it.Blocked || it.BlockReason == "" {
				continue
			}
			fmt.Fprintf(app.Out, "%s%s: %s\n", columnGap, it.Display, titleLine(it.BlockReason))
		}
		// A limit that silently dropped items makes a partial listing read as a
		// complete one. Said only when it actually cut: a run that fit inside
		// its limit has nothing to disclaim.
		if len(payload.Items) < payload.Matched {
			fmt.Fprintf(app.Out, "(showing %d of %d matched)\n", len(payload.Items), payload.Matched)
		}
	})
}

// listHeader names the columns of the human listing row, in listRow's order.
var listHeader = []string{"REF", "AVAILABILITY", "WORK", "URGENCY", "PRIORITY", "OWNER", "TAGS", "BLOCKED BY", "TITLE"}

// listColumnBounds caps how wide a cell may make its COLUMN, positionally
// against listHeader. Only TAGS is bounded: tags are reported in full
// (docs/cli.md § Tags), so one item carrying twenty labels would otherwise
// widen the column for the whole listing and shove every title off the screen.
// The over-wide cell is still printed whole — see alignedRows.
var listColumnBounds = []int{0, 0, 0, 0, 0, 0, tagsColumnMax, 0, 0}

// tagsColumnMax is the width beyond which a tags cell stops setting the tags
// column. Not a clip: nothing is shortened at this or any other width.
const tagsColumnMax = 24

// listRow renders one item as the cells of the human listing row.
//
// Ref and availability lead, for addressability and scanning; the title takes
// the flexible tail; the axes and the marks sit between. Every cell filled with
// backend text goes through oneLine or titleLine, which is what keeps a tab in
// a title or a tag from shifting the row.
func listRow(it listItemPayload, self string) []string {
	// An empty availability is a backend defect, not an absent value, so it
	// gets its own marker rather than the absent-cell dash.
	avail := it.Availability
	if avail == "" {
		avail = "?"
	}
	// Both axes are read through OrNeutral, which is the SDK's one definition
	// of what an unset or unrecognised value means: an item nothing has said
	// anything about is `medium` and `default`. A second reading here would let
	// the listing disagree with the order `resolve` takes work in.
	//
	// `default` is what an item has when NO OPERATOR HAS SAID ANYTHING, and it
	// is almost every item — printed, the column is a wall of one word hiding
	// the few rows where somebody did act. docs/cli.md § Priority and urgency:
	// the ordinary case "carries no marker of any kind". Only `next` and
	// `deferred` reach the column.
	//
	// Priority is NOT treated this way and prints on every row, `medium`
	// included: on that axis silence is not an assessment either, but every
	// value ranks the work, and blanking the commonest one would read as
	// unranked rather than as mid-ranked.
	urgency := flow.Urgency(it.Urgency).OrNeutral()
	shown := string(urgency)
	if urgency == flow.UrgencyDefault {
		shown = ""
	}
	return []string{
		it.Display,
		avail,
		orDash(workMark(it, self)),
		orDash(shown),
		string(flow.Priority(it.Priority).OrNeutral()),
		orDash(it.Owner),
		orDash(tagsCell(it.Tags)),
		orDash(strings.Join(it.BlockedBy, ",")),
		orDash(titleLine(it.Title)),
	}
}

// workMark says whether an item is being worked, where, and why it stopped —
// the question an operator otherwise has to open `status` on every row to
// answer. Marks COMBINE, because a claim survives a park: a parked item is
// usually also leased, and reporting only one of the two would withhold the
// other.
//
// A park is not a block. A blocked item waits on other items or on a condition
// and has its own column; a parked item waits on what its kind names, and both
// can show at once.
func workMark(it listItemPayload, self string) string {
	var marks []string
	if words := parkKindWords(flow.ParkKind(it.ParkKind)); words != "" {
		marks = append(marks, "parked: "+words)
	}
	switch {
	case it.InProgress:
		marks = append(marks, "in progress "+arenaWord(it.Arena, self))
	case it.Owner != "" || it.Arena != "":
		marks = append(marks, "leased "+arenaWord(it.Arena, self))
	}
	return strings.Join(marks, " · ")
}

// arenaWord names the holding arena AS FAR AS THE ORCHESTRATOR CAN HONESTLY
// NAME IT, and no further.
//
// Two answers, and no third is invented. An arena this binary can recognise as
// its own is `here`. Anything else is `another arena` — either the orchestrator
// reported an arena that is not this one, or it reported none at all, which is
// what the GitHub orchestrator does for a remote holder: it publishes an arena
// FINGERPRINT and never a machine name, because docs/disclosure.md puts machine
// and arena names among the identifiers that do not travel. Naming a remote
// holder's host and arena is separate work and is not begun here. The OWNER
// column carries the account in either case, which is the half that is always
// knowable.
func arenaWord(arena, self string) string {
	if arena != "" && arena == self {
		return "here"
	}
	return "another arena"
}

// arenaKey renders an arena as the one string both the payload and the
// comparison above use. One spelling, so what `--json` carries and what the
// human row is decided by cannot come apart.
func arenaKey(a flow.Arena) string {
	if a.Empty() {
		return ""
	}
	return string(a.Host) + "/" + string(a.Id)
}

// runObserved reports whether a run is advancing THIS item right now.
//
// IT IS NEVER GUESSED. The registration must name this item and its holder must
// be observed alive at this moment — the evidence `status` uses for a running
// step, and for the same reason: a record left behind by a process that died
// reports work in progress that stopped hours ago.
//
// A registration is arena-local, so this is knowable only for an item THIS
// arena holds. An item held elsewhere reads `leased`, never `in progress`,
// because a run on another machine is not observable from here in principle —
// the rule `status` is under for the same question.
func runObserved(it flow.ItemInfo, self string, running *clistate.RunningRecord) bool {
	if running == nil || arenaKey(it.Holder.Arena) != self || self == "" {
		return false
	}
	if running.Item != it.Ref.Display {
		return false
	}
	return clistate.ProcessAlive(running.PID, running.Exe)
}

// listOrder is the order `list` prints in. The set is CLOSED at three, and an
// unknown value is refused by name rather than falling back to a default — a
// listing silently ordered by something other than what was asked for is a
// wrong answer that looks like a right one (docs/cli.md § Invocation errors).
type listOrder string

const (
	// sortResolution is the order `resolve` takes work in. It is the DEFAULT,
	// because "what would run next" is the question a listing is nearly always
	// being asked.
	sortResolution listOrder = "resolution"
	sortNewest     listOrder = "newest"
	sortOldest     listOrder = "oldest"
)

func allListOrders() []listOrder { return []listOrder{sortResolution, sortNewest, sortOldest} }

func (o listOrder) valid() bool { return slices.Contains(allListOrders(), o) }

// sort orders the listing in place.
//
// EVERY ORDER IS TOTAL, and the tiebreak is the SORT'S STABILITY — the order
// the orchestrator returned. That is not a shortcut; it is the only tiebreak
// that can be right here. flow.CompareSelection returns 0 for two items filed
// at the same instant and says so in as many words: an orchestrator appends its
// own tiebreak "because ItemRef is orchestrator-specific and there is nothing
// here to break the tie on". The CLI is in exactly that position — it holds
// ItemRefs and nothing else — and a tiebreak invented here would be a string
// compare that puts #100 before #99 and would REORDER scope `auto`, which is
// required to come out exactly as List returned it. A stable sort over an
// already-ordered set is a no-op, which is precisely the requirement, and both
// orchestrators return a deterministic order at every scope, so two runs over
// one set print one order.
func (o listOrder) sort(items []listItemPayload) {
	switch o {
	case sortResolution:
		// THE SDK'S COMPARISON, never a second copy of it. What `critical`
		// outranks is one definition (flow.CompareSelection), and a CLI
		// ordering with its own rules would be the rule with two owners that
		// docs/orchestrator.md § Priority and urgency forbids.
		slices.SortStableFunc(items, func(x, y listItemPayload) int {
			return flow.CompareSelection(x.selectionKey(), y.selectionKey())
		})
	case sortNewest:
		slices.SortStableFunc(items, func(x, y listItemPayload) int {
			return y.FiledAt.Compare(x.FiledAt)
		})
	case sortOldest:
		slices.SortStableFunc(items, func(x, y listItemPayload) int {
			return x.FiledAt.Compare(y.FiledAt)
		})
	}
}

// selectionKey is the item's position in the selection order, as the SDK's
// comparison takes it. The axes travel on ItemInfo and the filing time now does
// too, so the key is assembled here rather than asked for a second time.
func (it listItemPayload) selectionKey() flow.SelectionKey {
	return flow.SelectionKey{
		Urgency:  flow.Urgency(it.Urgency),
		Priority: flow.Priority(it.Priority),
		Age:      it.FiledAt,
	}
}

// tagsCell renders an item's tags as one compact cell: every tag, in the
// backend's order, joined with a bare comma so the cell reads as one token and
// the eye lands on the title after it. Nothing is clipped or filtered — the
// listing reports tags in full, not only those a flow recognises. Each tag does
// go through oneLine: the tag floor keeps a tag single-line but not tab-free,
// and backends pass label names through verbatim, so a tag carrying a tab would
// otherwise shift every cell after it.
func tagsCell(tags []string) string {
	cells := make([]string, 0, len(tags))
	for _, t := range tags {
		cells = append(cells, oneLine(t))
	}
	return strings.Join(cells, ",")
}

// orDash renders an absent listing cell as "—", so a row never carries an
// empty column that a reader cannot tell from a dropped one.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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
