package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// `list` and `status` report both selection axes — the fields that decide which
// item an unattended `resolve` takes, and in what order.

func selectionApp(t *testing.T, be flow.Orchestrator) (*App, *bytes.Buffer) {
	t.Helper()
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
	return app, out
}

// Both keys are on every item, with no omitempty: an item nothing has said
// anything about reports medium and default rather than dropping the field. A
// stable key set is the machine contract.
func TestCmdList_JSONCarriesBothAxesOnEveryItem(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "nothing set"})
	be.AddItem("2", flow.Item{Type: "task", Title: "both set", Priority: flow.PriorityCritical, Urgency: flow.UrgencyNext})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(payload.Items))
	}
	if got := payload.Items[0]; got.Priority != "medium" || got.Urgency != "default" {
		t.Errorf("unset item = %q/%q, want medium/default", got.Priority, got.Urgency)
	}
	if got := payload.Items[1]; got.Priority != "critical" || got.Urgency != "next" {
		t.Errorf("set item = %q/%q, want critical/next", got.Priority, got.Urgency)
	}
	// The keys are present as keys, not merely as zero values a decoder filled
	// in — an omitempty on either would drop them for the ordinary item.
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, item := range raw.Items {
		for _, key := range []string{"priority", "urgency"} {
			if _, ok := item[key]; !ok {
				t.Errorf("item %d has no %q key: %v", i, key, item)
			}
		}
	}
}

// The human line carries them too: display, availability, urgency, priority,
// owner.
func TestCmdList_HumanLineShowsBothAxes(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "both set", Priority: flow.PriorityHigh, Urgency: flow.UrgencyNext})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if !strings.Contains(out.String(), "1\tauto\tnext\thigh\t") {
		t.Errorf("output does not carry the two axes; got:\n%s", out.String())
	}
}

// At scope `auto` the listing IS the selectable set, and the CLI prints it in
// the order the orchestrator returned. The fixture's selection order is the
// reverse of its display order, so a CLI-side re-sort fails here.
func TestCmdList_ScopeAutoPrintsTheOrchestratorsOrder(t *testing.T) {
	be := fake.New()
	// Filed oldest first, so age cannot be what produces the expected order —
	// priority is.
	be.SetClock(func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) })
	be.AddItem("a", flow.Item{Type: "task", Title: "later work", Priority: flow.PriorityLow})
	be.SetClock(func() time.Time { return time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC) })
	be.AddItem("b", flow.Item{Type: "task", Title: "first work", Priority: flow.PriorityCritical})
	app, out := selectionApp(t, be)

	if code := app.cmdList(context.Background(), []string{"--scope", "auto", "--human"}); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want two items", lines)
	}
	if !strings.HasPrefix(lines[0], "b\t") || !strings.HasPrefix(lines[1], "a\t") {
		t.Errorf("output is not in the order the orchestrator returned; got:\n%s", out.String())
	}
}

// `status` reports the same pair through the same fields, so one item cannot
// read one way through `list` and another through `status`.
func TestCmdStatus_ReportsBothAxes(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "inspect me", Priority: flow.PriorityLow, Urgency: flow.UrgencyDeferred})
	app, out := selectionApp(t, be)

	if code := app.cmdStatus(context.Background(), []string{"--json", "1"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	var payload statusPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Priority != "low" || payload.Urgency != "deferred" {
		t.Errorf("status = %q/%q, want low/deferred", payload.Priority, payload.Urgency)
	}

	// And in the human header, under owner.
	app, out = selectionApp(t, be)
	if code := app.cmdStatus(context.Background(), []string{"--human", "1"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	for _, want := range []string{"priority: low", "urgency: deferred"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("header missing %q; got:\n%s", want, out.String())
		}
	}
}

// An item nothing has said anything about reports the neutral pair here too,
// never an empty field.
func TestCmdStatus_ReportsTheNeutralValuesWhenNothingIsSet(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "inspect me"})
	app, out := selectionApp(t, be)

	if code := app.cmdStatus(context.Background(), []string{"--json", "1"}); code != 0 {
		t.Fatalf("cmdStatus = %d", code)
	}
	var payload statusPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Priority != "medium" || payload.Urgency != "default" {
		t.Errorf("status = %q/%q, want medium/default", payload.Priority, payload.Urgency)
	}
}
