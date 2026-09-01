package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// statusFlowLine is the source of the "flow:" status line. The key invariant
// (the T-bug this guards): "(finalized)" must appear ONLY when the persistent
// Item.Finalized flag is set — never for a not-yet-seeded item. An unseeded
// item with no eligible step must read "(not seeded)", not "(finalized)" and
// not the misleading "(no eligible step)".
func TestStatusFlowLine(t *testing.T) {
	doFlow := flow.NewFlow("do", []flow.ItemType{"task"})

	seeded := map[flow.ArtifactId]flow.ArtifactRecord{
		"plan": {Id: "plan", Required: true},
	}
	unseeded := map[flow.ArtifactId]flow.ArtifactRecord{}

	tests := []struct {
		name      string
		finalized bool
		artifacts map[flow.ArtifactId]flow.ArtifactRecord
		eligible  *flow.Flow
		typeFlow  *flow.Flow
		want      string
	}{
		{
			name:      "finalized flag set",
			finalized: true,
			artifacts: seeded,
			typeFlow:  doFlow,
			want:      "do (finalized)",
		},
		{
			name:      "finalized flag set, no type flow",
			finalized: true,
			artifacts: seeded,
			typeFlow:  nil,
			want:      "finalized",
		},
		{
			// The bug: unseeded item is NOT finalized — must not read "(finalized)".
			name:      "unseeded item is not finalized",
			finalized: false,
			artifacts: unseeded,
			typeFlow:  doFlow,
			want:      "do (not seeded)",
		},
		{
			name:      "seeded but no eligible step",
			finalized: false,
			artifacts: seeded,
			typeFlow:  doFlow,
			want:      "do (no eligible step)",
		},
		{
			name:      "eligible step takes precedence",
			finalized: false,
			artifacts: unseeded,
			eligible:  doFlow,
			typeFlow:  doFlow,
			want:      "do",
		},
		{
			name:      "no matching flow",
			finalized: false,
			artifacts: unseeded,
			typeFlow:  nil,
			want:      "(no matching flow)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &flow.ItemState{
				Item:      flow.Item{Finalized: tt.finalized},
				Artifacts: tt.artifacts,
			}
			got := statusFlowLine(state, tt.eligible, tt.typeFlow)
			if got != tt.want {
				t.Errorf("statusFlowLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

// titleLine feeds the human "title:" line. Item.Title is free backend prose:
// it may be empty, multi-line, or arbitrarily long, and none of those may be
// allowed to break the fixed three-line status header.
func TestTitleLine(t *testing.T) {
	long := strings.Repeat("a", statusTitleMax+10)

	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"empty", "", ""},
		{"whitespace only drops the line", "  \n\t ", ""},
		{"short title passes through", "land: fix the commit guard", "land: fix the commit guard"},
		{"surrounding whitespace trimmed", "  spaced  ", "spaced"},
		{"newlines collapse to single spaces", "first line\nsecond\tline", "first line second line"},
		{"exactly at the cap is not clipped", strings.Repeat("b", statusTitleMax), strings.Repeat("b", statusTitleMax)},
		{"over the cap is clipped with an ellipsis", long, strings.Repeat("a", statusTitleMax) + "…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := titleLine(tt.title); got != tt.want {
				t.Errorf("titleLine(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

// Clipping counts RUNES, not bytes: a byte-based clip would cut a multi-byte
// character mid-sequence and print a replacement glyph.
func TestTitleLine_ClipsRunesNotBytes(t *testing.T) {
	got := titleLine(strings.Repeat("é", statusTitleMax+5))
	want := strings.Repeat("é", statusTitleMax) + "…"
	if got != want {
		t.Errorf("titleLine() = %q, want %q", got, want)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("titleLine() = %q contains a replacement rune — clipped mid-sequence", got)
	}
}

// The human header must name the task, not just its id: "T1566" alone does not
// tell the operator which task the arena is holding.
func TestStatusHuman_PrintsTitle(t *testing.T) {
	env := newParkGrantEnv(t)
	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	if !strings.Contains(out, "title: test#1\n") {
		t.Errorf("status output missing the title line:\n%s", out)
	}
	// Ordering is the point of the placement: the title annotates the id, so
	// it sits between "item:" and "owner:".
	item, title, owner := strings.Index(out, "item:"), strings.Index(out, "title:"), strings.Index(out, "owner:")
	if !(item < title && title < owner) {
		t.Errorf("title line is out of order (item=%d title=%d owner=%d):\n%s", item, title, owner, out)
	}
}

// An item with no title must not print an empty "title:" field.
func TestStatusHuman_OmitsEmptyTitle(t *testing.T) {
	env := newParkGrantEnv(t)
	// Swap the seeded item for an untitled one. AddItem REPLACES the whole
	// record — claim included — so re-claim it, or status finds no active
	// claim and never reaches the header.
	ctx := context.Background()
	env.be.AddItem(flow.Item{ID: "1", Type: "task"})
	ref := flow.ItemRef{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)}
	if _, err := env.be.Claim(ctx, ref, "alice", nil); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	env.out.Reset()
	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	if out := env.out.String(); strings.Contains(out, "title:") {
		t.Errorf("status printed a title line for an untitled item:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// cmdStatus <id> — the StateInspector path introduced by this change
// ---------------------------------------------------------------------------

// When the backend does NOT implement StateInspector, `status <id>` must
// refuse (exit 1) and tell the user to claim first — not panic, not silently
// succeed.
func TestCmdStatus_WithoutStateInspector_RefusesById(t *testing.T) {
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{Budget: flow.DefaultStepBudget()})
	}, &stubAgent{name: "stub"})

	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	code := app.cmdStatus(context.Background(), []string{"1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "cannot inspect") {
		t.Errorf("stderr should explain why; got %q", errBuf.String())
	}
}

// statusInspectBackend wraps the fake backend and implements StateInspector
// and RefResolver — mirroring the github backend where the ref IS the id and
// state is resolvable from just the ref.
type statusInspectBackend struct {
	*fake.Backend
	claim *flow.Claim // set after Claim so LoadStateByRef can delegate
}

func (b *statusInspectBackend) LoadStateByRef(ctx context.Context, ref flow.ItemRef) (*flow.ItemState, error) {
	if b.claim == nil {
		return &flow.ItemState{Item: flow.Item{Type: "task"}}, nil
	}
	return b.Backend.LoadState(ctx, *b.claim)
}

func (b *statusInspectBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	return flow.ItemRef{BackendName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

// When the backend DOES implement StateInspector, `status <id>` must succeed
// (exit 0) and render the state — without claiming. This is the feature the
// change enables.
func TestCmdStatus_WithStateInspector_InspectsById(t *testing.T) {
	be := fake.New(flow.Signal("pr-open", "test"))
	be.AddItem(flow.Item{ID: "1", Type: "task", Title: "inspect me"})

	sib := &statusInspectBackend{Backend: be}
	app := &App{
		Backend: sib,
		Agent:   &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("plan", flow.ArtifactMarkdown),
		},
		Signals: []flow.SignalDef{
			flow.Signal("pr-open", "test"),
		},
		Owner: "alice",
	}
	f := flow.NewFlow("implement", []flow.ItemType{"task"})
	f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
		return ctx.ResolveMarkdown("the plan")
	}, flow.StepConfig{Budget: flow.StepBudget{
		MaxInvocations: 3, MaxPromptsPerInvocation: 1, MaxCostUSD: 10,
		Timeout: 30 * time.Minute,
	}})
	app.Flows = []*flow.Flow{f}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Claim and seed via the underlying fake so LoadStateByRef has state to
	// return, but the claim is NOT the app's active claim — the point is that
	// `status <id>` must NOT need an active claim.
	ctx := context.Background()
	ref := flow.ItemRef{BackendName: "fake", Display: "1", Ref: json.RawMessage(`"1"`)}
	claim, err := be.Claim(ctx, ref, "bob", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	sib.claim = &claim
	if err := be.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out, app.Err = out, errBuf

	code := app.cmdStatus(ctx, []string{"1"})
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, errBuf.String())
	}

	output := out.String()
	// Must show the item's title.
	if !strings.Contains(output, "inspect me") {
		t.Errorf("output should contain the item title; got:\n%s", output)
	}
	// Must show the owner from LookupClaim, not "(unclaimed)".
	if !strings.Contains(output, "bob") {
		t.Errorf("output should show the claim owner 'bob'; got:\n%s", output)
	}
	// Must NOT have claimed via Alice (the app's Owner).
	if strings.Contains(output, "alice") {
		t.Errorf("output should not mention alice (no claim taken); got:\n%s", output)
	}
}
