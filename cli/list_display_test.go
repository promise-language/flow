package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// The display half of `list`: the header and the padding, the two orderings,
// the cut, and the work mark. The cell-content rules — how a title or a tag is
// bounded — stay in cmd_list_test.go beside the renderers they exercise.

// listedRef builds a ref the discovererBackend fixture can carry.
func listedRef(display string) flow.ItemRef {
	return flow.ItemRef{OrchestratorName: "fake", Display: display, Ref: json.RawMessage(`"` + display + `"`)}
}

// listedAt builds an auto-available item filed at a given instant.
func listedAt(display string, filed time.Time, priority flow.Priority, urgency flow.Urgency) flow.ItemInfo {
	return flow.ItemInfo{
		Ref:          listedRef(display),
		FiledAt:      filed,
		Availability: flow.AvailAuto,
		Priority:     priority,
		Urgency:      urgency,
	}
}

// listRefsInOrder runs `list` and returns the ref cell of each row, in the
// order printed, with the header dropped.
func listRefsInOrder(t *testing.T, out *bytes.Buffer, app *App, args ...string) []string {
	t.Helper()
	out.Reset()
	if code := app.cmdList(context.Background(), append([]string{"--human"}, args...)); code != 0 {
		t.Fatalf("cmdList %v = %d", args, code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var refs []string
	for _, line := range lines[1:] { // drop the header
		// The closing "(showing n of m matched)" is a note about the listing,
		// not a row in it.
		if strings.HasPrefix(line, "(") {
			continue
		}
		refs = append(refs, strings.Fields(line)[0])
	}
	return refs
}

var (
	jan = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	feb = time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	mar = time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
)

// --sort resolution is the order `resolve` takes work in, and it is
// flow.CompareSelection rather than a second copy of it: urgency outranks
// priority, so a `next`/`low` item starts ahead of a `default`/`critical` one,
// and age breaks what is left with the oldest first.
//
// The fixture returns the items in none of those orders, so a listing that
// merely passed the orchestrator's order through fails here.
func TestCmdList_SortResolutionIsTheSDKsComparison(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		listedAt("newest-critical", mar, flow.PriorityCritical, flow.UrgencyDefault),
		listedAt("oldest-low", jan, flow.PriorityLow, flow.UrgencyDefault),
		listedAt("marked-next", feb, flow.PriorityLow, flow.UrgencyNext),
		listedAt("oldest-critical", jan, flow.PriorityCritical, flow.UrgencyDefault),
	}}
	app, out := selectionApp(t, be)

	got := listRefsInOrder(t, out, app)
	want := []string{"marked-next", "oldest-critical", "newest-critical", "oldest-low"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("resolution order = %v, want %v — urgency, then priority, then oldest first", got, want)
	}
	// It is the DEFAULT, because "what would run next" is what a listing is
	// nearly always being asked.
	if implicit := listRefsInOrder(t, out, app, "--sort", "resolution"); strings.Join(implicit, ",") != strings.Join(got, ",") {
		t.Errorf("explicit --sort resolution = %v, but the default gave %v", implicit, got)
	}
}

// newest and oldest order by the filing time ItemInfo now carries, in the two
// directions, and they are exact inverses over a set with no ties.
func TestCmdList_SortNewestAndOldestInvert(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		listedAt("b", feb, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("c", mar, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("a", jan, flow.PriorityMedium, flow.UrgencyDefault),
	}}
	app, out := selectionApp(t, be)

	if got := listRefsInOrder(t, out, app, "--sort", "newest"); strings.Join(got, ",") != "c,b,a" {
		t.Errorf("newest = %v, want c,b,a", got)
	}
	if got := listRefsInOrder(t, out, app, "--sort", "oldest"); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("oldest = %v, want a,b,c", got)
	}
}

// EVERY ORDER IS TOTAL. Items that tie on the sort key fall back to the order
// the orchestrator returned — the sort is stable — so two runs over one set
// print one order. The fixture is four items identical on every axis and on
// filing time, which is the only case flow.CompareSelection answers 0 for.
func TestCmdList_EveryOrderIsTotal(t *testing.T) {
	var items []flow.ItemInfo
	for _, ref := range []string{"d", "a", "c", "b"} {
		items = append(items, listedAt(ref, jan, flow.PriorityMedium, flow.UrgencyDefault))
	}
	be := &discovererBackend{Orchestrator: fake.New(), items: items}
	app, out := selectionApp(t, be)

	for _, order := range allListOrders() {
		first := listRefsInOrder(t, out, app, "--sort", string(order))
		second := listRefsInOrder(t, out, app, "--sort", string(order))
		if strings.Join(first, ",") != strings.Join(second, ",") {
			t.Errorf("--sort %s is not total: %v then %v", order, first, second)
		}
		// The tiebreak IS the orchestrator's order, which is what makes scope
		// `auto` come out exactly as List returned it.
		if strings.Join(first, ",") != "d,a,c,b" {
			t.Errorf("--sort %s reordered a fully tied set to %v, want the orchestrator's d,a,c,b", order, first)
		}
	}
}

// Both renderings come out in the SAME order. What --json keeps unchanged is
// the values, not the order — a tool and a person reading one report must not
// be reading two.
func TestCmdList_BothRenderingsShareTheOrder(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		listedAt("b", feb, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("c", mar, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("a", jan, flow.PriorityMedium, flow.UrgencyDefault),
	}}
	app, out := selectionApp(t, be)

	human := listRefsInOrder(t, out, app, "--sort", "newest")

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--json", "--sort", "newest"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var machine []string
	for _, it := range payload.Items {
		machine = append(machine, it.Display)
	}
	if strings.Join(human, ",") != strings.Join(machine, ",") {
		t.Errorf("human order %v != json order %v", human, machine)
	}
	if payload.Sort != "newest" {
		t.Errorf("payload sort = %q, want %q", payload.Sort, "newest")
	}
}

// An unknown --sort is REJECTED BY NAME, before the command does anything. A
// listing silently ordered by something other than what was asked for is a
// wrong answer that looks like a right one.
func TestCmdList_UnknownSortIsRejectedByName(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task"})
	app, out := selectionApp(t, be)
	errBuf := &bytes.Buffer{}
	app.Err = errBuf

	if code := app.cmdList(context.Background(), []string{"--human", "--sort", "priority"}); code != 2 {
		t.Fatalf("cmdList = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), `"priority"`) {
		t.Errorf("refusal does not name the value: %q", errBuf.String())
	}
	// The valid set is offered, enumerated from the one copy of it.
	for _, order := range allListOrders() {
		if !strings.Contains(errBuf.String(), string(order)) {
			t.Errorf("refusal does not offer %q: %q", order, errBuf.String())
		}
	}
	if out.Len() != 0 {
		t.Errorf("a rejected invocation listed anyway: %q", out.String())
	}
}

// --limit takes the first n in the chosen order, AFTER --scope and --tag have
// filtered and --sort has ordered. So `--limit 2` is the next two items
// `resolve` would take, and `--sort newest --limit 2` is the two newest.
func TestCmdList_LimitTakesTheFirstNOfTheChosenOrder(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		listedAt("b", feb, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("c", mar, flow.PriorityCritical, flow.UrgencyDefault),
		listedAt("a", jan, flow.PriorityMedium, flow.UrgencyDefault),
	}}
	app, out := selectionApp(t, be)

	if got := listRefsInOrder(t, out, app, "--limit", "2"); strings.Join(got, ",") != "c,a" {
		t.Errorf("--limit 2 = %v, want the two the resolution order leads with: c,a", got)
	}
	if got := listRefsInOrder(t, out, app, "--sort", "newest", "--limit", "2"); strings.Join(got, ",") != "c,b" {
		t.Errorf("--sort newest --limit 2 = %v, want c,b", got)
	}
}

// A CUT LISTING SAYS IT WAS CUT. A limit that silently dropped items makes a
// partial listing read as a complete one, which is the under-reporting
// docs/index.md guards against for `gh issue list`.
func TestCmdList_ACutListingSaysSo(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		listedAt("a", jan, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("b", feb, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("c", mar, flow.PriorityMedium, flow.UrgencyDefault),
	}}
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human", "--limit", "2"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if !strings.Contains(out.String(), "(showing 2 of 3 matched)") {
		t.Errorf("a cut listing did not say so; got:\n%s", out.String())
	}

	// A listing that fit inside its limit has nothing to disclaim.
	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human", "--limit", "9"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if strings.Contains(out.String(), "showing") {
		t.Errorf("an uncut listing disclaimed anyway; got:\n%s", out.String())
	}
}

// Both renderings are limited alike, and --json carries the matching count
// beside the items so a consumer can tell a cut listing from a complete one
// without re-running the query.
func TestCmdList_JSONCarriesTheMatchingCountBesideTheItems(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		listedAt("a", jan, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("b", feb, flow.PriorityMedium, flow.UrgencyDefault),
		listedAt("c", mar, flow.PriorityMedium, flow.UrgencyDefault),
	}}
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--json", "--limit", "2"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Errorf("items = %d, want the limit's 2", len(payload.Items))
	}
	if payload.Matched != 3 {
		t.Errorf("matched = %d, want 3", payload.Matched)
	}
}

// n is a POSITIVE INTEGER. Zero, a negative number and a non-number are each
// rejected by name — zero especially, because a limit of nothing is not a
// listing and is far likelier to be a mistake than a request.
func TestCmdList_LimitMustBeAPositiveInteger(t *testing.T) {
	for _, bad := range []string{"0", "-1", "abc"} {
		t.Run(bad, func(t *testing.T) {
			be := fake.New()
			be.AddItem("1", flow.Item{Type: "task"})
			app, out := selectionApp(t, be)
			errBuf := &bytes.Buffer{}
			app.Err = errBuf

			if code := app.cmdList(context.Background(), []string{"--human", "--limit", bad}); code != 2 {
				t.Fatalf("cmdList --limit %s = %d, want 2", bad, code)
			}
			if !strings.Contains(errBuf.String(), bad) {
				t.Errorf("refusal does not name the value %q: %q", bad, errBuf.String())
			}
			if out.Len() != 0 {
				t.Errorf("a rejected invocation listed anyway: %q", out.String())
			}
		})
	}
}

// The default is UNLIMITED, which is not the same as a limit of zero — the one
// value the flag refuses.
func TestCmdList_DefaultLimitIsUnlimited(t *testing.T) {
	var items []flow.ItemInfo
	for i := range 40 {
		items = append(items, listedAt(fmt.Sprintf("i%02d", i), jan, flow.PriorityMedium, flow.UrgencyDefault))
	}
	be := &discovererBackend{Orchestrator: fake.New(), items: items}
	app, out := selectionApp(t, be)

	if got := listRefsInOrder(t, out, app); len(got) != 40 {
		t.Errorf("rows = %d, want all 40", len(got))
	}
	if strings.Contains(out.String(), "showing") {
		t.Errorf("an unlimited listing disclaimed a cut; got:\n%s", out.String())
	}
}

// One over-wide cell must not push every row. Tags are reported in full, so an
// item carrying many labels keeps them whole — but it stops setting the TAGS
// column for the whole listing, which is what would otherwise shove every other
// title off the screen.
func TestCmdList_OneWideTagsCellDoesNotPushEveryRow(t *testing.T) {
	wide := strings.Repeat("x", tagsColumnMax+40)
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		{Ref: listedRef("a"), Availability: flow.AvailAuto, Title: "first", Tags: []flow.TagId{"cli"}},
		{Ref: listedRef("b"), Availability: flow.AvailAuto, Title: "second", Tags: []flow.TagId{flow.TagId(wide)}},
		{Ref: listedRef("c"), Availability: flow.AvailAuto, Title: "third", Tags: []flow.TagId{"bug"}},
	}}
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want a header and three rows:\n%s", len(lines), out.String())
	}
	// The wide cell is printed WHOLE — nothing is clipped and no second
	// placeholder rule is introduced.
	if !strings.Contains(lines[2], wide) {
		t.Errorf("the wide tags cell was not printed whole:\n%s", lines[2])
	}
	// …and the rows either side of it keep their titles where a listing with a
	// 64-character tags column would not.
	if strings.Index(lines[1], "first") != strings.Index(lines[3], "third") {
		t.Errorf("one wide cell moved the other rows' titles:\n%s", out.String())
	}
	// The claim precisely: the other rows land where they would have if the
	// wide item were not in the listing at all. Measured against that listing
	// rather than against a hand-picked column number, so the assertion stays
	// true when a column is added.
	be.items = []flow.ItemInfo{be.items[0], be.items[2]}
	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	without := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if strings.Index(lines[1], "first") != strings.Index(without[1], "first") {
		t.Errorf("the wide cell widened the column for everyone:\nwith:\n%s\nwithout:\n%s",
			strings.Join(lines, "\n"), strings.Join(without, "\n"))
	}
}

// The columns are padded to a common width so a column can be followed down the
// page, and the width is measured in RUNES: "—" is three bytes, so a byte count
// pads a row carrying one three columns too far — which is exactly the wave
// this replaces.
func TestCmdList_ColumnsAlignAcrossRowsOfDifferentWidths(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		{Ref: listedRef("a"), Availability: flow.AvailAuto, Title: "first", Holder: flow.Holder{Account: "djabi"}},
		{Ref: listedRef("b"), Availability: flow.AvailAvailable, Title: "second"},
		{Ref: listedRef("c"), Availability: flow.AvailBlocked, Title: "third"},
	}}
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want a header and three rows:\n%s", len(lines), out.String())
	}
	// Every title starts at one COLUMN — counted in runes, which is the whole
	// point: the rows carry different numbers of "—", three bytes each, so a
	// byte offset differs on rows that line up perfectly on a terminal. An
	// implementation that padded by byte count would pass a byte-counting
	// assertion and produce the wave this replaces.
	at := []int{
		runeIndex(lines[1], "first"),
		runeIndex(lines[2], "second"),
		runeIndex(lines[3], "third"),
	}
	if at[0] != at[1] || at[1] != at[2] {
		t.Errorf("titles land at %v, want one offset:\n%s", at, out.String())
	}
	if at[0] != runeIndex(lines[0], "TITLE") {
		t.Errorf("the header's TITLE does not sit over the titles:\n%s", out.String())
	}
	// The byte offsets genuinely differ here, so the rune measurement above is
	// doing work rather than agreeing with a byte one by accident.
	if strings.Index(lines[1], "first") == strings.Index(lines[2], "second") {
		t.Skip("fixture no longer varies the multi-byte cells; the rune check is not being exercised")
	}
}

// runeIndex is strings.Index counted in runes, which is how a terminal counts.
func runeIndex(line, sub string) int {
	at := strings.Index(line, sub)
	if at < 0 {
		return -1
	}
	return utf8.RuneCountInString(line[:at])
}

// Nothing is printed above an EMPTY listing: there is nothing to name the
// columns of, and the "(no items…)" line stays exactly as it was.
func TestCmdList_NoHeaderOverAnEmptyListing(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New()}
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if got := out.String(); got != "(no items at scope processable)\n" {
		t.Errorf("empty listing = %q, want the bare note with no header", got)
	}
}

// The row carries the open blockers BY REFERENCE, which is what turns "this is
// blocked" into "go work that one instead", and the block's reason under it.
// The references come from blockerDisplays — the same function that fills the
// payload's blocked_by — so the two renderings cannot report different
// blockers.
func TestCmdList_HumanRowCarriesOpenBlockersAndTheReason(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{{
		Ref:          listedRef("o/r#7"),
		Title:        "waits",
		Availability: flow.AvailBlocked,
		Blocked:      true,
		BlockKind:    flow.WaitsOnItems,
		BlockReason:  "waiting on unfinished dependencies",
		BlockedBy: []flow.Blocker{
			{Ref: listedRef("o/r#3"), Status: flow.StatusOpen},
			{Ref: listedRef("o/r#4"), Status: flow.StatusTerminal},
			{Ref: listedRef("o/r#5"), Status: flow.StatusOpen},
		},
	}}}
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "o/r#3,o/r#5") {
		t.Errorf("row does not carry the open blockers by reference; got:\n%s", got)
	}
	// A blocker that has FINISHED is not named: printing it would send the
	// operator to work something that is already done.
	if strings.Contains(got, "o/r#4") {
		t.Errorf("row names a blocker that has already finished; got:\n%s", got)
	}
	if !strings.Contains(got, "o/r#7: waiting on unfinished dependencies") {
		t.Errorf("row does not carry the block reason; got:\n%s", got)
	}
}

// An item that is NOT blocked carries neither — the dash in the column, and no
// reason line at all — even when blockers were once declared on it and have
// since finished.
func TestCmdList_AnUnblockedRowNamesNoBlockers(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{{
		Ref:          listedRef("o/r#7"),
		Title:        "free now",
		Availability: flow.AvailAuto,
		BlockedBy:    []flow.Blocker{{Ref: listedRef("o/r#3"), Status: flow.StatusTerminal}},
	}}}
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if got := out.String(); strings.Contains(got, "o/r#3") {
		t.Errorf("an unblocked row names a finished blocker; got:\n%s", got)
	}
}

// withRunningRecord points clistate at a temporary arena and writes the
// registration `list` and `status` both read, naming the current process so the
// liveness check passes.
func withRunningRecord(t *testing.T, rec clistate.RunningRecord) {
	t.Helper()
	t.Setenv("FLOW_DIR", t.TempDir())
	if rec.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		abs, err := filepath.Abs(exe)
		if err != nil {
			t.Fatalf("abs: %v", err)
		}
		rec.Exe = abs
	}
	if rec.PID == 0 {
		rec.PID = os.Getpid()
	}
	if err := clistate.SaveRunning(rec); err != nil {
		t.Fatalf("SaveRunning: %v", err)
	}
}

// heldHere builds an item this arena holds, which is the only case a run can be
// observed for.
func listingHeldHere(t *testing.T, be *discovererBackend, display string, park flow.ParkKind) *discovererBackend {
	t.Helper()
	be.items = []flow.ItemInfo{{
		Ref:          listedRef(display),
		Title:        "work",
		Availability: flow.AvailHeld,
		Holder:       flow.Holder{Account: "djabi", Arena: flow.ArenaAt(be.ArenaRoot())},
		ParkKind:     park,
	}}
	return be
}

// `in progress` is reported only when a run is OBSERVED advancing this item: a
// registration naming it, whose holder is alive right now. It is the evidence
// `status` uses for a running step, and for the same reason — a record left
// behind by a process that died reports work that stopped hours ago.
func TestCmdList_WorkMarkInProgressOnlyWhenObserved(t *testing.T) {
	be := listingHeldHere(t, &discovererBackend{Orchestrator: fake.New()}, "o/r#1", "")
	withRunningRecord(t, clistate.RunningRecord{Item: "o/r#1", Step: "plan"})
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if !strings.Contains(out.String(), "in progress here") {
		t.Errorf("a live registration naming this item was not reported in progress; got:\n%s", out.String())
	}
}

// A registration whose holder is NOT ALIVE is not in progress. PID 1 is alive
// but is not this executable, which is the reuse case ProcessAlive exists to
// defeat — so the item falls back to `leased`, which is what its claim still
// says.
func TestCmdList_WorkMarkDeadRegistrationIsLeasedNotInProgress(t *testing.T) {
	be := listingHeldHere(t, &discovererBackend{Orchestrator: fake.New()}, "o/r#1", "")
	withRunningRecord(t, clistate.RunningRecord{Item: "o/r#1", Step: "plan", PID: 1, Exe: "/nonexistent/flow-binary"})
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	got := out.String()
	if strings.Contains(got, "in progress") {
		t.Errorf("a dead registration was reported in progress; got:\n%s", got)
	}
	if !strings.Contains(got, "leased here") {
		t.Errorf("want the claim still reported as a lease; got:\n%s", got)
	}
}

// A live registration for a DIFFERENT item marks neither item in progress — the
// one it names is not in this listing, and the one that is has no run.
func TestCmdList_WorkMarkIgnoresARegistrationForAnotherItem(t *testing.T) {
	be := listingHeldHere(t, &discovererBackend{Orchestrator: fake.New()}, "o/r#1", "")
	withRunningRecord(t, clistate.RunningRecord{Item: "o/r#99", Step: "plan"})
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if strings.Contains(out.String(), "in progress") {
		t.Errorf("a registration naming another item marked this one in progress; got:\n%s", out.String())
	}
}

// IN PROGRESS IS NEVER GUESSED for an item held ELSEWHERE. A run on another
// machine is not observable from here in principle, so such an item reads
// `leased another arena` even while this arena's own registration happens to
// name it — which is the state a stale or coincidental record produces.
func TestCmdList_WorkMarkNeverGuessesARunElsewhere(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{{
		Ref:          listedRef("o/r#1"),
		Title:        "work",
		Availability: flow.AvailHeld,
		// The account, and no arena — what the GitHub orchestrator reports for
		// a holder it can only fingerprint.
		Holder: flow.Holder{Account: "someone-else"},
	}}}
	withRunningRecord(t, clistate.RunningRecord{Item: "o/r#1", Step: "plan"})
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	got := out.String()
	if strings.Contains(got, "in progress") {
		t.Errorf("a run was claimed for an item held elsewhere; got:\n%s", got)
	}
	if !strings.Contains(got, "leased another arena") {
		t.Errorf("want `leased another arena`; got:\n%s", got)
	}
	// The account is always shown, which is the half that IS knowable.
	if !strings.Contains(got, "someone-else") {
		t.Errorf("the holding account is not named; got:\n%s", got)
	}
}

// A claim survives a park, so a parked item is usually also leased and the two
// marks COMBINE. Reporting only one of them would withhold the other.
func TestCmdList_WorkMarkCombinesParkedAndLeased(t *testing.T) {
	be := listingHeldHere(t, &discovererBackend{Orchestrator: fake.New()}, "o/r#1", flow.ParkQuestion)
	t.Setenv("FLOW_DIR", t.TempDir())
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if !strings.Contains(out.String(), "parked: waiting on an answer · leased here") {
		t.Errorf("the two marks did not combine; got:\n%s", out.String())
	}
}

// An unclaimed, unparked item carries the absent marker, not an empty cell: a
// reader must be able to tell an absent value from a dropped column.
func TestCmdList_WorkMarkIsAbsentOnAnIdleItem(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{
		{Ref: listedRef("o/r#1"), Title: "work", Availability: flow.AvailAuto},
	}}
	t.Setenv("FLOW_DIR", t.TempDir())
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if cells := strings.Fields(lines[1]); cells[2] != "—" {
		t.Errorf("work cell = %q, want the absent marker; got:\n%s", cells[2], out.String())
	}
}

// PARKED IS NOT BLOCKED. A blocked item waits on other items or on a condition;
// a parked item waits on what its kind names. Both can show at once, in their
// own columns, and neither is rendered as the other.
func TestCmdList_ParkedAndBlockedAreSeparateColumns(t *testing.T) {
	be := &discovererBackend{Orchestrator: fake.New(), items: []flow.ItemInfo{{
		Ref:          listedRef("o/r#1"),
		Title:        "both",
		Availability: flow.AvailBlocked,
		ParkKind:     flow.ParkTreasurerRefused,
		Blocked:      true,
		BlockKind:    flow.WaitsOnItems,
		BlockReason:  "waiting on unfinished dependencies",
		BlockedBy:    []flow.Blocker{{Ref: listedRef("o/r#3"), Status: flow.StatusOpen}},
	}}}
	t.Setenv("FLOW_DIR", t.TempDir())
	app, out := selectionApp(t, be)

	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "parked: budget exhausted") {
		t.Errorf("the park is missing; got:\n%s", got)
	}
	if !strings.Contains(got, "o/r#3") {
		t.Errorf("the blocker is missing; got:\n%s", got)
	}
}

// Every member of the park vocabulary renders in words. The crosswalk walks
// flow.AllParkKinds(), so a kind added later is a failure here rather than a
// blank cell nobody notices — and the words come from the KIND, never from the
// reason prose, which is a free-text line nothing parses.
func TestParkKindTable_CoversEveryKind(t *testing.T) {
	for _, kind := range flow.AllParkKinds() {
		facts, ok := parkKindTable[kind]
		if !ok {
			t.Errorf("park kind %q has no entry in parkKindTable", kind)
			continue
		}
		if facts.words == "" {
			t.Errorf("park kind %q renders no words", kind)
		}
		if facts.resume == "" {
			t.Errorf("park kind %q names no resuming act", kind)
		}
		if got := parkKindWords(kind); got != facts.words {
			t.Errorf("parkKindWords(%q) = %q, want %q", kind, got, facts.words)
		}
		if got := resumingAct(kind, "bin/issue"); !strings.Contains(got, "bin/issue") {
			t.Errorf("resumingAct(%q) = %q, want it to name the binary", kind, got)
		}
	}
	// An item that is not parked has no kind and renders nothing, so a caller
	// can ask unconditionally.
	if got := parkKindWords(""); got != "" {
		t.Errorf("parkKindWords(\"\") = %q, want the empty string", got)
	}
	if got := resumingAct("", "bin/issue"); got != "" {
		t.Errorf("resumingAct(\"\") = %q, want the empty string", got)
	}
	// A kind this SDK does not recognise renders ITSELF rather than nothing —
	// a value that came from somewhere is worth showing — but names no
	// resuming act, because inventing one would be worse than omitting it.
	if got := parkKindWords("from-the-future"); got != "from-the-future" {
		t.Errorf("parkKindWords(unknown) = %q, want the value itself", got)
	}
	if got := resumingAct("from-the-future", "bin/issue"); got != "" {
		t.Errorf("resumingAct(unknown) = %q, want no line at all", got)
	}
}
