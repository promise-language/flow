package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// grantTestSetup builds an App + claim using the shared testApp scaffolding,
// seeds the "plan" artifact so Grant has something to bump, and returns the
// loaded state-reader helper. Tests use "plan" as the artifact id because the
// bug being fixed (T0484) is in the flag parser — the specific id is incidental.
func grantTestSetup(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer, func() flow.ArtifactRecord) {
	t.Helper()
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})

	}, a)

	if err := be.SeedState(context.Background(), claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	var out, errBuf bytes.Buffer
	app.Out = &out
	app.Err = &errBuf

	read := func() flow.ArtifactRecord {
		st, err := be.Load(context.Background(), claim.ItemRef)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return st.Artifact("plan")
	}
	return app, &out, &errBuf, read
}

// The bug: flags placed after the positional artifact id must parse, not be
// reported as "unexpected argument".
func TestCmdGrant_FlagsAfterPositional(t *testing.T) {
	app, _, errBuf, read := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{"plan", "--invocations", "3"})
	if code != 0 {
		t.Fatalf("cmdGrant = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := read().GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3", got)
	}
}

// Existing form (flags before positional) must keep working.
func TestCmdGrant_FlagsBeforePositional(t *testing.T) {
	app, _, errBuf, read := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{"--invocations", "3", "plan"})
	if code != 0 {
		t.Fatalf("cmdGrant = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := read().GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3", got)
	}
}

// "--name=value" form after the positional must parse (covers the equals
// branch of the parseArgs walk).
func TestCmdGrant_EqualsFormAfterPositional(t *testing.T) {
	app, _, errBuf, read := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{"plan", "--invocations=3"})
	if code != 0 {
		t.Fatalf("cmdGrant = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := read().GrantedInvocations; got != 3 {
		t.Errorf("GrantedInvocations = %d, want 3", got)
	}
}

// Multiple flags after the positional must all parse — covers the multi-flag
// walk in parseArgs.
func TestCmdGrant_InterspersedMultipleFlags(t *testing.T) {
	app, _, errBuf, read := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{
		"plan",
		"--invocations", "3",
		"--cost", "5",
		"--timeout", "60",
	})
	if code != 0 {
		t.Fatalf("cmdGrant = %d, want 0; stderr=%q", code, errBuf.String())
	}
	rec := read()
	if rec.GrantedInvocations != 3 {
		t.Errorf("GrantedInvocations = %d, want 3", rec.GrantedInvocations)
	}
	if rec.GrantedCostUSD != 5 {
		t.Errorf("GrantedCostUSD = %v, want 5", rec.GrantedCostUSD)
	}
	if rec.GrantedTimeout.Seconds() != 60 {
		t.Errorf("GrantedTimeout = %v, want 60s", rec.GrantedTimeout)
	}
}

// Two raw positionals must still error — the helper must not accidentally
// swallow the second one.
func TestCmdGrant_TooManyPositionals(t *testing.T) {
	app, _, errBuf, _ := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{"plan", "extra"})
	if code != 2 {
		t.Fatalf("cmdGrant = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("stderr = %q, want 'unexpected argument'", errBuf.String())
	}
}

// "--" terminator: subsequent tokens are forced into positionals so a literal
// "--invocations" can be passed as an artifact id. Covers the terminator
// branch in parseArgs.
func TestCmdGrant_DoubleDashTerminator(t *testing.T) {
	app, _, errBuf, _ := grantTestSetup(t)

	// Two positionals after `--` trigger the NArg()>1 path, proving both
	// were collected as positionals (not flags).
	code := app.cmdGrant(context.Background(), []string{"--", "plan", "--invocations"})
	if code != 2 {
		t.Fatalf("cmdGrant = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("stderr = %q, want 'unexpected argument'", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--invocations") {
		t.Errorf("stderr = %q, expected --invocations treated as positional", errBuf.String())
	}
}

// Flag-only invocation with nothing parked: park mode has no park to act on,
// so it refuses (rather than silently sweeping every step) and points at the
// two explicit forms.
func TestCmdGrant_NoPark_RefusesWithRemedy(t *testing.T) {
	app, _, errBuf, _ := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{"--invocations", "3"})
	if code != 2 {
		t.Fatalf("cmdGrant = %d, want 2", code)
	}
	for _, want := range []string{"no park recorded", "grant --all", "grant <step-id>"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("stderr = %q, want %q", errBuf.String(), want)
		}
	}
}

// TestCmdGrant_UnmatchedTypeExitsOne (#25): an item whose type no flow accepts
// must exit 1 (could not complete), not 2 (malformed invocation). The command
// line was valid; the binary simply cannot act on this item type.
func TestCmdGrant_UnmatchedTypeExitsOne(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testAppItem(t,
		flow.Item{Ref: itemRefFor("1"), Type: "chore", Title: "chore#1"},
		[]flow.ItemType{"task"}, // flow accepts only "task"
		func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
				return ctx.ResolveMarkdown("the plan")
			}, flow.StepConfig{})
		}, a)

	if err := be.SeedState(context.Background(), claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	var errBuf bytes.Buffer
	app.Err = &errBuf

	code := app.cmdGrant(context.Background(), []string{"--invocations", "1", "--all"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), `"chore"`) {
		t.Errorf("expected stderr to name the unmatched type; got %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no flow in this binary handles item type") {
		t.Errorf("expected stderr to explain the mismatch; got %q", errBuf.String())
	}
}
