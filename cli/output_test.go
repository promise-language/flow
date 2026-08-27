package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A bytes.Buffer is not a *os.File, so auto-detection must land on human —
// this is what keeps every existing test asserting human text instead of
// silently flipping to JSON.
func TestResolveOutput_BufferIsHuman(t *testing.T) {
	t.Setenv(outputEnv, "")
	app := &App{Out: &bytes.Buffer{}}
	if got := app.resolveOutput(); got != OutputHuman {
		t.Errorf("resolveOutput() = %v, want OutputHuman", got)
	}
}

// A pipe is a *os.File that is not a character device: the piped case, which
// must be JSON.
func TestResolveOutput_PipeIsJSON(t *testing.T) {
	t.Setenv(outputEnv, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	app := &App{Out: w}
	if got := app.resolveOutput(); got != OutputJSON {
		t.Errorf("resolveOutput() = %v, want OutputJSON", got)
	}
}

func TestResolveOutput_ExplicitAndEnv(t *testing.T) {
	t.Setenv(outputEnv, "json")
	app := &App{Out: &bytes.Buffer{}}
	if got := app.resolveOutput(); got != OutputJSON {
		t.Errorf("with FLOW_OUTPUT=json: got %v, want OutputJSON", got)
	}
	// An explicit App.Output beats the environment.
	app.Output = OutputHuman
	if got := app.resolveOutput(); got != OutputHuman {
		t.Errorf("with App.Output set: got %v, want OutputHuman", got)
	}
}

func TestOutputFlags_JSONAndHumanAreExclusive(t *testing.T) {
	env := newParkGrantEnv(t)
	if code := env.grant("--json", "--human"); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(env.err.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want 'mutually exclusive'", env.err.String())
	}
}

// decode is the golden-payload helper: it pins field NAMES, which is the part
// a tool depends on.
func decode(t *testing.T, b *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", b.String(), err)
	}
	return m
}

func TestStatusJSON_Schema(t *testing.T) {
	env := newParkGrantEnv(t)
	if err := env.be.ResolveArtifact(context.Background(), env.claim, "plan",
		flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "done"}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	if err := env.be.BumpInvocations(context.Background(), env.claim, "commit"); err != nil {
		t.Fatalf("BumpInvocations: %v", err)
	}
	env.park(t, budgetExhausted("commit", flow.AxisInvocations))

	if code := env.app.cmdStatus(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	m := decode(t, env.out)

	for _, key := range []string{"item", "title", "owner", "flow", "flow_state", "finalized", "park", "steps", "questions"} {
		if _, ok := m[key]; !ok {
			t.Errorf("payload missing %q: %v", key, m)
		}
	}

	park, ok := m["park"].(map[string]any)
	if !ok {
		t.Fatalf("park = %v, want an object", m["park"])
	}
	if park["kind"] != string(flow.ParkBudgetExhausted) || park["step"] != "commit" || park["axis"] != string(flow.AxisInvocations) {
		t.Errorf("park = %v, want budget-exhausted on commit/invocations", park)
	}

	steps, _ := m["steps"].([]any)
	if len(steps) != 3 {
		t.Fatalf("steps = %v, want 3", steps)
	}
	first, _ := steps[0].(map[string]any)
	if first["id"] != "plan" || first["label"] != "write plan" {
		t.Errorf("first step = %v, want id=plan label='write plan'", first)
	}
	if first["kind"] != kindArtifact || first["state"] != stateResolved {
		t.Errorf("first step kind/state = %v/%v", first["kind"], first["state"])
	}
	budget, ok := first["budget"].(map[string]any)
	if !ok {
		t.Fatalf("budget = %v, want an object", first["budget"])
	}
	for _, key := range []string{"invocations", "cost_usd", "prompts_per_invocation", "timeout_seconds"} {
		if _, ok := budget[key]; !ok {
			t.Errorf("budget missing %q: %v", key, budget)
		}
	}

	// The signal step: budget null is the machine-readable "not a grant target".
	last, _ := steps[2].(map[string]any)
	if last["id"] != "pr-open" || last["kind"] != kindSignal {
		t.Errorf("last step = %v, want the pr-open signal step", last)
	}
	if last["budget"] != nil {
		t.Errorf("signal step budget = %v, want null", last["budget"])
	}
}

// The human checklist leads with the id (the grant target), trails the label,
// and tags steps that cannot take a grant.
func TestStatusHuman_IDFirstChecklist(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.app.cmdStatus(context.Background(), []string{"--human"}); code != 0 {
		t.Fatalf("cmdStatus = %d; stderr=%q", code, env.err.String())
	}
	out := env.out.String()
	for _, want := range []string{"[ ] plan", "write plan", "pr-open", "(signal — no budget)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want %q", out, want)
		}
	}
	planLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "plan") && strings.Contains(line, "write plan") {
			planLine = line
			break
		}
	}
	if planLine == "" {
		t.Fatalf("no plan line in %q", out)
	}
	if strings.Index(planLine, "plan") > strings.Index(planLine, "write plan") {
		t.Errorf("line %q: the id must come before the label", planLine)
	}
}

func TestGrantJSON_Schema(t *testing.T) {
	env := newParkGrantEnv(t)
	for range 3 {
		if err := env.be.BumpInvocations(context.Background(), env.claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	env.park(t, budgetExhausted("plan", flow.AxisInvocations))

	if code := env.grant("--json"); code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, env.err.String())
	}
	m := decode(t, env.out)

	if m["mode"] != grantModePark {
		t.Errorf("mode = %v, want %q", m["mode"], grantModePark)
	}
	if m["unparked"] != true {
		t.Errorf("unparked = %v, want true", m["unparked"])
	}
	if m["dry_run"] != false {
		t.Errorf("dry_run = %v, want false", m["dry_run"])
	}
	granted, _ := m["granted"].([]any)
	if len(granted) != 1 {
		t.Fatalf("granted = %v, want one entry", granted)
	}
	entry, _ := granted[0].(map[string]any)
	if entry["id"] != "plan" {
		t.Errorf("granted id = %v, want plan", entry["id"])
	}
	inv, ok := entry["invocations"].(map[string]any)
	if !ok {
		t.Fatalf("invocations = %v, want an object", entry["invocations"])
	}
	if inv["from"] != float64(3) || inv["to"] != float64(4) {
		t.Errorf("invocations = %v, want from 3 to 4", inv)
	}
	// Axes with no delta are omitted, so a reader never sees a no-op "change".
	if _, present := entry["cost_usd"]; present {
		t.Errorf("cost_usd present with no delta: %v", entry)
	}
}

func TestListJSON_Schema(t *testing.T) {
	env := newParkGrantEnv(t)

	if code := env.app.cmdList(context.Background(), []string{"--json"}); code != 0 {
		t.Fatalf("cmdList = %d; stderr=%q", code, env.err.String())
	}
	m := decode(t, env.out)
	items, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("items = %v, want an array", m["items"])
	}
	if len(items) != 1 {
		t.Fatalf("items = %v, want one", items)
	}
	it, _ := items[0].(map[string]any)
	for _, key := range []string{"display", "owner", "backend"} {
		if _, ok := it[key]; !ok {
			t.Errorf("item missing %q: %v", key, it)
		}
	}
	if it["owner"] != "alice" {
		t.Errorf("owner = %v, want alice", it["owner"])
	}
}

// An empty eligible set is [] in JSON — not the human "(no eligible items)".
func TestListJSON_EmptyIsArray(t *testing.T) {
	app, _, _ := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("x")
		}, flow.StepConfig{})
	}, &stubAgent{name: "stub"})
	out := &bytes.Buffer{}
	app.Out, app.Err = out, &bytes.Buffer{}
	app.Output = OutputJSON
	// The fake lists the one registered item; drop it by pointing the app at a
	// backend whose item set is empty.
	app.Backend = emptyListBackend{app.Backend}

	if code := app.cmdList(context.Background(), nil); code != 0 {
		t.Fatalf("cmdList = %d", code)
	}
	if got := strings.TrimSpace(out.String()); !strings.Contains(got, `"items": []`) {
		t.Errorf("out = %q, want an empty items array", got)
	}
}

// emptyListBackend is the wrapped backend with nothing eligible.
type emptyListBackend struct{ flow.Backend }

func (emptyListBackend) ListEligible(context.Context) ([]flow.ItemRef, error) { return nil, nil }

// A typo in FLOW_OUTPUT must not silently pick the opposite mode.
func TestResolveOutput_UnknownEnvWarns(t *testing.T) {
	t.Setenv(outputEnv, "jsonn")
	errBuf := &bytes.Buffer{}
	app := &App{Out: &bytes.Buffer{}, Err: errBuf}

	if got := app.resolveOutput(); got != OutputHuman {
		t.Errorf("resolveOutput() = %v, want OutputHuman", got)
	}
	if !strings.Contains(errBuf.String(), "not json|human") {
		t.Errorf("stderr = %q, want a warning about the bad value", errBuf.String())
	}
}

// The usage text names the commands that take --json/--human. That sentence is
// a promise about a closed set, and this change is what put `resolve` in it —
// so hold the documented set and the wired set equal. Probing with the
// contradictory pair is what makes the wiring observable: a command that
// registered the flags rejects them ("mutually exclusive"), one that never did
// fails to parse them at all — and both decide before any backend work, so
// nothing here touches the tracker.
func TestUsage_NamesExactlyTheCommandsTakingOutputFlags(t *testing.T) {
	// The output-modes note is the last paragraph of the usage text.
	paras := strings.Split(strings.TrimSpace(usage("flow")), "\n\n")
	note := paras[len(paras)-1]
	if !strings.Contains(note, "--json") {
		t.Fatalf("last usage paragraph is not the output-modes note: %q", note)
	}

	for cmd := range perCommandUsage {
		app, out, errBuf := newArgparseApp(t)
		code := RunWithArgs(*app, []string{cmd, "--json", "--human"})
		if code != 2 {
			t.Errorf("%s --json --human: exit = %d, want 2 (either rejected as contradictory or as undefined flags)", cmd, code)
		}
		if out.Len() != 0 {
			t.Errorf("%s --json --human: stdout = %q, want empty", cmd, out.String())
		}
		takesFlags := strings.Contains(errBuf.String(), "mutually exclusive")
		if !takesFlags && !strings.Contains(errBuf.String(), "use of unknown flag") {
			t.Fatalf("%s --json --human: stderr = %q, want either the mutual-exclusion refusal or an unknown-flag error", cmd, errBuf.String())
		}
		if named := strings.Contains(note, cmd); named != takesFlags {
			t.Errorf("%s: named in the output-modes note = %v, takes --json/--human = %v — the two must agree.\nnote: %q",
				cmd, named, takesFlags, note)
		}
	}
}
