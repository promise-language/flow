package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// discovererBackend wraps the fake backend and adds Discoverer capability.
type discovererBackend struct {
	*fake.Backend
	items []flow.DiscoveryItem
}

func (d *discovererBackend) Discover(ctx context.Context, scope flow.DiscoveryScope, binaryName string) ([]flow.DiscoveryItem, error) {
	var out []flow.DiscoveryItem
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
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "task one"})

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{makeTestFlow(t)},
		Owner:     "alice",
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
	if !strings.Contains(out.String(), "task one") {
		t.Errorf("output missing item display; got:\n%s", out.String())
	}
}

// TestCmdList_WithDiscoverer_ScopeOpen tests listing at scope=open with a
// Discoverer backend.
func TestCmdList_WithDiscoverer_ScopeOpen(t *testing.T) {
	be := &discovererBackend{
		Backend: fake.New(),
		items: []flow.DiscoveryItem{
			{BackendName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`), Title: "task one", Availability: flow.AvailAuto, Tags: []string{"type:task"}},
			{BackendName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`), Title: "task two", Availability: flow.AvailUnhandled, Tags: []string{"bug"}},
		},
	}

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{makeTestFlow(t)},
		Owner:     "alice",
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
		Backend: fake.New(),
		items: []flow.DiscoveryItem{
			{BackendName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`), Title: "task one", Availability: flow.AvailAuto, Tags: []string{"type:task"}},
			{BackendName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`), Title: "task two", Availability: flow.AvailUnhandled, Tags: []string{"bug"}},
		},
	}

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{makeTestFlow(t)},
		Owner:     "alice",
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
		Backend: fake.New(),
		items: []flow.DiscoveryItem{
			{BackendName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`), Title: "both tags", Availability: flow.AvailAuto, Tags: []string{"priority:high", "area:api"}},
			{BackendName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`), Title: "one tag", Availability: flow.AvailAuto, Tags: []string{"priority:high"}},
		},
	}

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{makeTestFlow(t)},
		Owner:     "alice",
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
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{makeTestFlow(t)},
		Owner:     "alice",
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
		Backend: fake.New(),
		items: []flow.DiscoveryItem{
			{BackendName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`), Title: "auto item", Availability: flow.AvailAuto, Tags: []string{"type:task"}},
			{BackendName: "fake", Display: "o/r#2", Ref: json.RawMessage(`"2"`), Title: "available item", Availability: flow.AvailAvailable, Tags: []string{"type:task"}},
		},
	}

	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{makeTestFlow(t)},
		Owner:     "alice",
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

// makeTestFlow creates a minimal flow for test App validation.
func makeTestFlow(t *testing.T) *flow.Flow {
	t.Helper()
	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
		return ctx.ResolveMarkdown("the plan")
	}, flow.StepConfig{Budget: flow.DefaultStepBudget()})
	return f
}
