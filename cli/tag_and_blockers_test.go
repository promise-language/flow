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

// A --tag value below the floor is REFUSED rather than interpolated.
//
// The floor is load-bearing: the value reaches the orchestrator's own query,
// where a tag carrying a space does not fail — it silently becomes a different
// query, and the operator gets a plausible wrong answer instead of an error.
// Both commands read the same values, so both refuse them the same way.

func TestCmdList_RefusesATagBelowTheFloor(t *testing.T) {
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{{
			Ref:          flow.ItemRef{OrchestratorName: "fake", Display: "o/r#1", Ref: json.RawMessage(`"1"`)},
			Title:        "a task",
			Availability: flow.AvailAuto,
		}},
	}
	for _, tag := range []string{"", "   ", " leading", "trailing ", "two\nlines"} {
		t.Run(tag, func(t *testing.T) {
			app := &App{
				Orchestrator: be,
				Agent:        &stubAgent{name: "stub"},
				Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
				Flows:        []*flow.Flow{makeTestFlow(t)},
			}
			if err := app.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
			app.Out, app.Err = out, errBuf

			if code := app.cmdList(context.Background(), []string{"--tag", tag, "--human"}); code != 1 {
				t.Fatalf("cmdList = %d, want 1 — the tag is not one", code)
			}
			// Nothing was listed: an answer computed from a filter the operator
			// did not write is worse than no answer.
			if out.Len() != 0 {
				t.Errorf("a listing was printed for an invalid tag:\n%s", out.String())
			}
			if !strings.Contains(errBuf.String(), "not a valid tag") {
				t.Errorf("stderr = %q, want it to say the tag is not one", errBuf.String())
			}
		})
	}
}

func TestCmdResolve_RefusesATagBelowTheFloorWithoutSelecting(t *testing.T) {
	inner := fake.New()
	inner.AddItem("1", flow.Item{Type: "task", Title: "1"})
	be := &tagFilterBackend{
		Orchestrator: inner,
		taggedRefs:   []flow.ItemRef{{OrchestratorName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)}},
	}
	app, _, errBuf := resolveTestApp(t, be)

	if code := app.cmdResolve(context.Background(), []string{"--tag", " priority:high"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; err=%q", code, errBuf.String())
	}
	// The orchestrator was never asked. A refusal that had already selected
	// against the mangled filter would have claimed the wrong item.
	if be.calledTags != nil {
		t.Errorf("ListAutoSelectable was called with %v for an invalid tag", be.calledTags)
	}
}

// The listing reports the blockers still OPEN, not every blocker ever declared.
//
// BlockedBy keeps a satisfied entry until someone retracts it — a set that
// quietly dropped them could not be edited, because nothing could see what was
// there to remove — so the listing has to do the filtering. Printing a finished
// blocker would tell an operator to go work something that is already done.
func TestCmdList_ReportsOnlyTheBlockersStillOpen(t *testing.T) {
	ref := func(display string) flow.ItemRef {
		return flow.ItemRef{OrchestratorName: "fake", Display: display, Ref: json.RawMessage(`"x"`)}
	}
	be := &discovererBackend{
		Orchestrator: fake.New(),
		items: []flow.ItemInfo{{
			Ref:          ref("o/r#1"),
			Title:        "waiting",
			Availability: flow.AvailBlocked,
			Blocked:      true,
			BlockKind:    flow.WaitsOnItems,
			BlockReason:  "waiting on unfinished dependencies",
			BlockedBy: []flow.Blocker{
				{Ref: ref("o/r#7"), Status: flow.StatusTerminal},
				{Ref: ref("o/r#8"), Status: flow.StatusOpen},
			},
		}},
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
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	if code := app.cmdList(context.Background(), []string{"--scope", "processable", "--json"}); code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, errBuf.String())
	}
	var payload listPayload
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode listing: %v (%s)", err, out.String())
	}
	if len(payload.Items) != 1 {
		t.Fatalf("got %d items, want 1: %s", len(payload.Items), out.String())
	}
	it := payload.Items[0]
	if len(it.BlockedBy) != 1 || it.BlockedBy[0] != "o/r#8" {
		t.Errorf("blocked_by = %v, want just the blocker still open (o/r#8)", it.BlockedBy)
	}
	// The machine-readable facts travel beside the reference; the reason is
	// prose and never names an item.
	if !it.Blocked || it.BlockKind != string(flow.WaitsOnItems) {
		t.Errorf("blocked=%v kind=%q, want blocked on items", it.Blocked, it.BlockKind)
	}
}
