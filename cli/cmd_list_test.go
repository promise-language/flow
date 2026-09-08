package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func (d *discovererBackend) List(ctx context.Context, scope flow.ItemScope, binaryName flow.BinaryName, acceptsType func(flow.ItemType) bool) ([]flow.ItemInfo, error) {
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
		Flows:        []*flow.Flow{makeTestFlow(t)},
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

// TestCmdList_WithDiscoverer_ScopeOpen tests listing at scope=open with a
// Discoverer backend.
func TestCmdList_WithDiscoverer_ScopeOpen(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)}, Title: "task one", Availability: flow.AvailAuto, Tags: []flow.TagId{"type:task"}},
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`)}, Title: "task two", Availability: flow.AvailUnhandled, Tags: []flow.TagId{"bug"}},
		},
	}

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{makeTestFlow(t)},
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
			{Ref: flow.ItemRef{OrchestratorName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`)}, Title: "task two", Availability: flow.AvailUnhandled, Tags: []flow.TagId{"bug"}},
		},
	}

	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{makeTestFlow(t)},
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
		t.Errorf("o/r#2 (unhandled) should not appear at scope=processable; got:\n%s", out.String())
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
		Flows:        []*flow.Flow{makeTestFlow(t)},
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
		Flows:        []*flow.Flow{makeTestFlow(t)},
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
		Flows:        []*flow.Flow{makeTestFlow(t)},
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
		Flows:        []*flow.Flow{makeTestFlow(t)},
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
	f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
		return ctx.ResolveMarkdown("the plan")
	}, flow.StepConfig{Budget: flow.DefaultStepBudget()})
	return f
}
