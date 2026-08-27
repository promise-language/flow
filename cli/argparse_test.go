package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

func newArgparseApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	be := fake.New()
	app := &App{
		Backend:   be,
		Agent:     &stubAgent{name: "stub"},
		Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:     []*flow.Flow{newDummyFlow("x")},
		Owner:     "alice",
	}
	if err := app.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	app.Out = out
	app.Err = errBuf
	return app, out, errBuf
}

func TestRunWithArgs_UnknownCommand(t *testing.T) {
	app, _, errBuf := newArgparseApp(t)
	code := RunWithArgs(*app, []string{"frobnicate"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Errorf("err = %q, want 'unknown command'", errBuf.String())
	}
}

func TestRunWithArgs_HelpPrintsUsage(t *testing.T) {
	for _, tok := range []string{"help", "--help", "-h"} {
		app, out, _ := newArgparseApp(t)
		code := RunWithArgs(*app, []string{tok})
		if code != 0 {
			t.Errorf("%s: exit code = %d, want 0", tok, code)
		}
		if !strings.Contains(out.String(), "usage:") {
			t.Errorf("%s: out = %q, want usage text", tok, out.String())
		}
	}
}

func TestNoArgsCommands_RejectExtraPositional(t *testing.T) {
	cases := []struct {
		name string
		run  func(app *App, args []string) int
	}{
		{"doctor", func(a *App, args []string) int { return a.cmdDoctor(context.Background(), args) }},
		{"list", func(a *App, args []string) int { return a.cmdList(context.Background(), args) }},
		{"release", func(a *App, args []string) int { return a.cmdRelease(context.Background(), args) }},
		{"run-step", func(a *App, args []string) int { return a.cmdRun(context.Background(), args) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, errBuf := newArgparseApp(t)
			code := tc.run(app, []string{"oops"})
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errBuf.String(), "unexpected argument") {
				t.Errorf("err = %q, want 'unexpected argument'", errBuf.String())
			}
		})
	}
}

// status and resolve take an OPTIONAL single positional (the item id), so one
// positional is valid — but a SECOND positional must still be rejected.
func TestOptionalPositionalCommands_RejectSecondPositional(t *testing.T) {
	cases := []struct {
		name string
		run  func(app *App, args []string) int
	}{
		{"status", func(a *App, args []string) int { return a.cmdStatus(context.Background(), args) }},
		{"resolve", func(a *App, args []string) int { return a.cmdResolve(context.Background(), args) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, errBuf := newArgparseApp(t)
			code := tc.run(app, []string{"T0001", "oops"})
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errBuf.String(), "unexpected argument") {
				t.Errorf("err = %q, want 'unexpected argument'", errBuf.String())
			}
		})
	}
}

func TestCmdClaim_RejectsExtraPositional(t *testing.T) {
	app, _, errBuf := newArgparseApp(t)
	code := app.cmdClaim(context.Background(), []string{"42", "extra"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("err = %q, want 'unexpected argument'", errBuf.String())
	}
}

func TestCmdClaim_RejectsMissingId(t *testing.T) {
	app, _, errBuf := newArgparseApp(t)
	code := app.cmdClaim(context.Background(), nil)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "missing item id") {
		t.Errorf("err = %q, want 'missing item id'", errBuf.String())
	}
}

func TestCmdGrant_RejectsExtraPositional(t *testing.T) {
	app, _, errBuf := newArgparseApp(t)
	code := app.cmdGrant(context.Background(), []string{"--invocations", "1", "plan", "extra"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("err = %q, want 'unexpected argument'", errBuf.String())
	}
}

func TestCmdGrant_RejectsNegativeFlag(t *testing.T) {
	cases := [][]string{
		{"--invocations", "-1", "plan"},
		{"--prompts", "-1", "plan"},
		{"--cost", "-1.5", "plan"},
		{"--timeout", "-10", "plan"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			app, _, errBuf := newArgparseApp(t)
			code := app.cmdGrant(context.Background(), args)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errBuf.String(), "must be >= 0") {
				t.Errorf("err = %q, want 'must be >= 0'", errBuf.String())
			}
		})
	}
}
