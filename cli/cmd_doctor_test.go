package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

func TestCmdDoctor_OKGlyph(t *testing.T) {
	be := fake.New()
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
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
	be := &failingBackend{Backend: fake.New(), err: errors.New("simulated")}
	app := App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{newDummyFlow("x")},
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
	f.AddStep("step", "plan", func(flow.StepCtx) error { return nil })
	return f
}

// failingBackend wraps the fake backend and forces ListEligible to error so
// cmdDoctor's fallback probe fails.
type failingBackend struct {
	*fake.Backend
	err error
}

func (b *failingBackend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	return nil, b.err
}
