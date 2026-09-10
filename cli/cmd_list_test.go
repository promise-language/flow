package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// discovererBackend wraps the fake backend and adds Discoverer capability.
type discovererBackend struct {
	*fake.Orchestrator
	items []flow.ItemInfo
}

func (d *discovererBackend) List(ctx context.Context, scope flow.ItemScope, binaryName flow.BinaryName, acceptsType func(flow.ItemType) bool, assumesRole func(flow.RoleName) bool) ([]flow.ItemInfo, error) {
	var out []flow.ItemInfo
	for _, item := range d.items {
		if item.Availability.InScope(scope) {
			out = append(out, item)
		}
	}
	return out, nil
}

// TestCmdList_DefaultScope tests that `list` with no flags uses processable
// scope and falls back to the legacy path when no Discoverer is available.
func TestCmdList_DefaultScope(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "task one"})

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	code := app.cmdList(context.Background(), []string{"--human"})
	if code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, errBuf.String())
	}
	// The listing line is display, availability, urgency, priority, holder,
	// tags, title — ref and availability lead so a person scanning for
	// something to work can address it, and the title trails so they can tell
	// what it is.
	if !strings.Contains(out.String(), "1\tauto") {
		t.Errorf("output missing the item's display and availability; got:\n%s", out.String())
	}
}

// The human row carries the title and the tags the report already holds, so a
// reader is not sent to the backend's web UI to learn what an item IS. The
// whole line is asserted, not just presence: the column order is the contract
// a `cut -f` reader depends on. Tags print in full, in backend order, joined
// by a bare comma.
func TestCmdList_HumanRowCarriesTitleAndTags(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{
		Type:  "task",
		Title: "Startup validation does not validate the graph",
		Tags:  []flow.TagId{"cli", "bug"},
	})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	want := "1\tauto\tdefault\tmedium\t—\tcli,bug\tStartup validation does not validate the graph\n"
	if got := out.String(); got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
}

// The title cell is bounded the way the status header's is, through the same
// titleLine: a newline or a tab in a title stays ONE row with ONE title cell
// (tab is the column separator, so an uncollapsed one would shift every cell
// after it), and an over-long title is clipped with an ellipsis. One case
// each — the clipping table itself is titleLine's own test.
func TestCmdList_HumanRowBoundsTheTitle(t *testing.T) {
	t.Run("whitespace collapses to one line", func(t *testing.T) {
		be := fake.New()
		be.AddItem("1", flow.Item{Type: "task", Title: "first line\nsecond\tline"})
		app, out := selectionApp(t, be)

		if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
			t.Fatalf("cmdList = %d", code)
		}
		want := "1\tauto\tdefault\tmedium\t—\t—\tfirst line second line\n"
		if got := out.String(); got != want {
			t.Errorf("row = %q, want %q", got, want)
		}
	})
	t.Run("over-long title is clipped", func(t *testing.T) {
		be := fake.New()
		be.AddItem("1", flow.Item{Type: "task", Title: strings.Repeat("a", statusTitleMax+10)})
		app, out := selectionApp(t, be)

		if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
			t.Fatalf("cmdList = %d", code)
		}
		want := "1\tauto\tdefault\tmedium\t—\t—\t" + strings.Repeat("a", statusTitleMax) + "…\n"
		if got := out.String(); got != want {
			t.Errorf("row = %q, want %q", got, want)
		}
	})
}

// The tags cell is bounded the same way. The tag floor keeps a tag single-line
// but not tab-free, and the GitHub backend passes label names through
// verbatim — so a tag carrying a tab must collapse like a title does, or it
// shifts every cell after it and a `cut -f` reader gets the wrong column. The
// tag is NOT clipped, whatever its length: tags are reported in full.
func TestCmdList_HumanRowBoundsTheTags(t *testing.T) {
	be := fake.New()
	long := strings.Repeat("x", statusTitleMax+10)
	be.AddItem("1", flow.Item{
		Type:  "task",
		Title: "t",
		Tags:  []flow.TagId{"needs\treview", flow.TagId(long)},
	})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	want := "1\tauto\tdefault\tmedium\t—\tneeds review," + long + "\tt\n"
	if got := out.String(); got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
}

// An item with no title and no tags still fills every cell: owner, tags and
// title each render as "—", so the row keeps its column count and a reader
// can tell an absent value from a dropped column.
func TestCmdList_HumanRowMarksAbsentTitleAndTags(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task"})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	want := "1\tauto\tdefault\tmedium\t—\t—\t—\n"
	if got := out.String(); got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
}

// A title that is all whitespace is absent, not blank: it renders as "—" like
// a missing one, never as an empty cell. The collapse and the absent marker
// compose in one order only — the marker applies to the collapsed title, so
// whitespace the backend supplied cannot leave the row with an empty column
// the reader cannot tell from a dropped one.
func TestCmdList_HumanRowMarksAWhitespaceOnlyTitleAbsent(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: " \n\t "})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	want := "1\tauto\tdefault\tmedium\t—\t—\t—\n"
	if got := out.String(); got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
}

// Every column of the row filled with a real value, on a held item: the holder
// sits between the axes and the tags, so the owner an operator scans for is
// not displaced by the two cells this listing gained. The absent-cell tests
// pin the column order with dashes; this one pins it with values, so a row
// that printed the tags where the holder belongs — or dropped the holder for
// the dash whatever the report said — fails here and nowhere else.
func TestCmdList_HumanRowFillsEveryColumnOfAHeldItem(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{{
			Ref:          flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)},
			Title:        "Startup validation does not validate the graph",
			Availability: flow.AvailHeld,
			Holder:       flow.Holder{Account: "djabi"},
			Tags:         []flow.TagId{"cli", "bug"},
			Priority:     flow.PriorityHigh,
			Urgency:      flow.UrgencyNext,
		}},
	}
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	want := "o/r#1\theld\tnext\thigh\tdjabi\tcli,bug\tStartup validation does not validate the graph\n"
	if got := out.String(); got != want {
		t.Errorf("row = %q, want %q", got, want)
	}
}

// Human and JSON are two renderings of ONE report. The human row bounds the
// title and collapses the tags because tab is its column separator; JSON has
// no such constraint and carries both exactly as the backend supplied them —
// unclipped, uncollapsed — so nothing that needs the whole string loses it.
// A clip or a collapse applied where the report is built, rather than where
// the human row is rendered, would pass every human-row test and fail here.
func TestCmdList_JSONCarriesTitleAndTagsVerbatim(t *testing.T) {
	title := "first line\nsecond\tline " + strings.Repeat("a", statusTitleMax+10)
	tags := []flow.TagId{"needs\treview", flow.TagId(strings.Repeat("x", statusTitleMax+10))}
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: title, Tags: tags})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode listing: %v (%s)", err, out.String())
	}
	if len(payload.Items) != 1 {
		t.Fatalf("got %d items, want 1: %s", len(payload.Items), out.String())
	}
	it := payload.Items[0]
	if it.Title != title {
		t.Errorf("title = %q, want the backend's string verbatim %q", it.Title, title)
	}
	if want := tagStrings(tags); !slices.Equal(it.Tags, want) {
		t.Errorf("tags = %q, want the backend's tags verbatim %q", it.Tags, want)
	}
}

// TestCmdList_WithDiscoverer_ScopeOpen tests listing at scope=open with a
// Discoverer backend.
func TestCmdList_WithDiscoverer_ScopeOpen(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)}, Title: "task one", Availability: flow.AvailAuto, Tags: []flow.TagId{"type:task"}},
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`)}, Title: "task two", Availability: flow.AvailOutsideRemit, Tags: []flow.TagId{"bug"}},
		},
	}

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	// --scope open should show both items.
	code := app.cmdList(context.Background(), []string{"--scope", "open", "--human"})
	if code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "o/r#1") || !strings.Contains(out.String(), "o/r#2") {
		t.Errorf("expected both items; got:\n%s", out.String())
	}
}

// TestCmdList_WithDiscoverer_ScopeProcessable filters to only processable.
func TestCmdList_WithDiscoverer_ScopeProcessable(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)}, Title: "task one", Availability: flow.AvailAuto, Tags: []flow.TagId{"type:task"}},
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`)}, Title: "task two", Availability: flow.AvailOutsideRemit, Tags: []flow.TagId{"bug"}},
		},
	}

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	code := app.cmdList(context.Background(), []string{"--human"})
	if code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, errBuf.String())
	}
	// Only auto is >= processable level 3.
	if !strings.Contains(out.String(), "o/r#1") {
		t.Errorf("expected o/r#1; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "o/r#2") {
		t.Errorf("o/r#2 (outside the remit) should not appear at scope=processable; got:\n%s", out.String())
	}
}

// TestCmdList_TagFilter tests conjunctive tag filtering.
func TestCmdList_TagFilter(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)}, Title: "both tags", Availability: flow.AvailAuto, Tags: []flow.TagId{"priority:high", "area:api"}},
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`)}, Title: "one tag", Availability: flow.AvailAuto, Tags: []flow.TagId{"priority:high"}},
		},
	}

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	code := app.cmdList(context.Background(), []string{"--tag", "priority:high", "--tag", "area:api", "--human"})
	if code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "o/r#1") {
		t.Errorf("expected o/r#1 with both tags; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "o/r#2") {
		t.Errorf("o/r#2 lacks area:api and should not appear; got:\n%s", out.String())
	}
}

// TestCmdList_UnknownScope rejects an invalid --scope value.
func TestCmdList_UnknownScope(t *testing.T) {
	be := fake.New()
	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	app.Out = newDiscardWriter()
	errBuf := &bytes.Buffer{}
	app.Err = errBuf

	code := app.cmdList(context.Background(), []string{"--scope", "galaxy"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage error)", code)
	}
}

// TestCmdList_JSON_Availability verifies that the JSON output includes the
// availability field with the auto-selectable marker.
func TestCmdList_JSON_Availability(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)}, Title: "auto item", Availability: flow.AvailAuto, Tags: []flow.TagId{"type:task"}},
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`)}, Title: "available item", Availability: flow.AvailAvailable, Tags: []flow.TagId{"type:task"}},
		},
	}

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := &bytes.Buffer{}
	app.Out, app.Err = out, newDiscardWriter()

	code := app.cmdList(context.Background(), []string{"--json", "--scope", "processable"})
	if code != 0 {
		t.Fatalf("cmdList = %d", code)
	}

	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Scope != "processable" {
		t.Errorf("scope = %q, want processable", payload.Scope)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(payload.Items))
	}
	if payload.Items[0].Availability != "auto" {
		t.Errorf("item[0].Availability = %q, want auto", payload.Items[0].Availability)
	}
	if payload.Items[1].Availability != "available" {
		t.Errorf("item[1].Availability = %q, want available", payload.Items[1].Availability)
	}
}

// TestCmdList_RemitGatesTheListing: gating a listing is what the remit is FOR
// (docs/flow-registration.md § Item types) — `list` must answer "is this our
// work" statically, from the flow's declared types and without dispatching
// anything. The predicate `list` hands the orchestrator is what decides that,
// and nothing else asserts it is the remit: a `list` that passed nil, or a
// predicate accepting everything, would report another binary's items as this
// one's work at every scope.
//
// Both directions in one reading, at `open` scope, which is the widest scope
// that still ranks by availability: the item in the remit is auto, the one
// outside it is outside-remit — and that is exactly the rung `processable`
// (the default scope) drops.
func TestCmdList_RemitGatesTheListing(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "in remit"})
	be.AddItem("2", flow.Item{Type: "chore", Title: "outside the remit"})

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t), // remit: {"task"}
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out, app.Err = out, newDiscardWriter()

	if code := app.cmdList(context.Background(), []string{"--json", "--scope", "open"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]string{}
	for _, it := range payload.Items {
		got[it.Display] = it.Availability
	}
	if want := map[string]string{"1": "auto", "2": "outside-remit"}; !maps.Equal(got, want) {
		t.Errorf("availability by item = %v, want %v", got, want)
	}

	// And the default scope drops it, rather than merely labelling it: an
	// operator asking what there is to work on is not offered another binary's
	// item.
	out.Reset()
	if code := app.cmdList(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	payload = listPayload{}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].Display != "1" {
		t.Errorf("processable items = %+v, want only the in-remit item", payload.Items)
	}
}

// TestCmdList_EmptyDiscovery verifies the empty-result message includes the
// scope name, not a generic "no eligible items".
func TestCmdList_EmptyDiscovery(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items:        nil, // empty
	}
	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flow:         makeTestFlow(t),
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out := &bytes.Buffer{}
	app.Out, app.Err = out, newDiscardWriter()

	code := app.cmdList(context.Background(), []string{"--scope", "workable", "--human"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no items at scope workable") {
		t.Errorf("expected scope-specific empty message; got %q", out.String())
	}
}

// makeTestFlow creates a minimal flow for test App validation.
func makeTestFlow(t *testing.T) *flow.Flow {
	t.Helper()
	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	f.Role("contributor", flow.CapPush)
	f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
		return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
	}, flow.StepConfig{Prompts: flow.PromptsAgent, Entry: true, Role: "contributor", MayFinalize: []flow.Disposition{flow.DispositionResolved}})
	return f
}
