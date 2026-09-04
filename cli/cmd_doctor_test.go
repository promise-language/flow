package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

func TestCmdDoctor_OKGlyph(t *testing.T) {
	be := fake.New()
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows: []*flow.Flow{
			newDummyFlow("x"),
		},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	code := app.cmdDoctor(context.Background(), nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(out.String(), glyphOK) {
		t.Errorf("doctor OK output should start with %q glyph; got %q", glyphOK, out.String())
	}
}

func TestCmdDoctor_FailGlyph(t *testing.T) {
	be := &failingBackend{Orchestrator: fake.New(), err: errors.New("simulated")}
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	errBuf := &bytes.Buffer{}
	app.Out = &bytes.Buffer{}
	app.Err = errBuf

	code := app.cmdDoctor(context.Background(), nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(errBuf.String(), glyphFail) {
		t.Errorf("doctor fail output should start with %q glyph; got %q", glyphFail, errBuf.String())
	}
}

func newDummyFlow(name string) *flow.Flow {
	f := flow.NewFlow(name, nil)
	f.AddStep("step", "plan", func(flow.StepCtx) error { return nil }, flow.StepConfig{})
	return f
}

// doctor reports the gates and commands the orchestrator declares. They are
// what startup validation checked, and an operator asking why a run refused
// needs to see the same list the check saw.
func TestCmdDoctor_ReportsDeclaredGatesAndCommands(t *testing.T) {
	app := App{
		Orchestrator: fake.New(),
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	if code := app.cmdDoctor(context.Background(), nil); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// The two required gates and the required command, by name.
	for _, want := range []string{"integration", "fit", "verify"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output does not name %q; got %q", want, out.String())
		}
	}
}

// When CarryThrough is set, doctor should print the carry-through caveat.
func TestCmdDoctor_ReportsCarryThrough(t *testing.T) {
	be := fake.New()
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
		CarryThrough: true,
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	code := app.cmdDoctor(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "carry-through: enabled") {
		t.Errorf("doctor output should report carry-through; got %q", out.String())
	}
	if !strings.Contains(out.String(), "not independent review") {
		t.Errorf("doctor output should state the caveat; got %q", out.String())
	}
}

// When CarryThrough is not set, doctor should not mention it.
func TestCmdDoctor_OmitsCarryThroughWhenDisabled(t *testing.T) {
	be := fake.New()
	app := App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	app.Out = out
	app.Err = &bytes.Buffer{}

	code := app.cmdDoctor(context.Background(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(out.String(), "carry-through") {
		t.Errorf("doctor output should not mention carry-through when disabled; got %q", out.String())
	}
}

// failingBackend wraps the fake backend and forces ListEligible to error so
// cmdDoctor's fallback probe fails.
type failingBackend struct {
	*fake.Orchestrator
	err error
}

func (b *failingBackend) ListAutoSelectable(ctx context.Context, _ []flow.TagId) ([]flow.ItemRef, error) {
	return nil, b.err
}
