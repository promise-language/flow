package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// grantTestSetup builds an App + claim using the shared testApp scaffolding and
// returns a reader for the "plan" step's LEDGER ROW — which is where a grant is
// recorded. Tests use "plan" as the step id because the bug being fixed (T0484)
// is in the flag parser; the specific id is incidental.
//
// The row rather than the effective cap: these tests are about what the parser
// hands to Grant, and the row is that value unmixed with the binary's policy.
func grantTestSetup(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer, func() flow.LedgerRow) {
	t.Helper()
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
			return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
		}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})

	}, a)

	var out, errBuf bytes.Buffer
	app.Out = &out
	app.Err = &errBuf

	read := func() flow.LedgerRow {
		st, err := be.Load(context.Background(), claim.ItemRef)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return st.Ledger.Row("plan")
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
	if got := read().GrantedOn(flow.AxisInvocations); got != 3 {
		t.Errorf("GrantedOn(invocations) = %v, want 3", got)
	}
}

// Existing form (flags before positional) must keep working.
func TestCmdGrant_FlagsBeforePositional(t *testing.T) {
	app, _, errBuf, read := grantTestSetup(t)

	code := app.cmdGrant(context.Background(), []string{"--invocations", "3", "plan"})
	if code != 0 {
		t.Fatalf("cmdGrant = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if got := read().GrantedOn(flow.AxisInvocations); got != 3 {
		t.Errorf("GrantedOn(invocations) = %v, want 3", got)
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
	if got := read().GrantedOn(flow.AxisInvocations); got != 3 {
		t.Errorf("GrantedOn(invocations) = %v, want 3", got)
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
	row := read()
	if got := row.GrantedOn(flow.AxisInvocations); got != 3 {
		t.Errorf("GrantedOn(invocations) = %v, want 3", got)
	}
	if got := row.GrantedOn(flow.AxisCost); got != 5 {
		t.Errorf("GrantedOn(cost) = %v, want 5", got)
	}
	// Timeout grants are recorded in the axis's own unit: seconds.
	if got := row.GrantedOn(flow.AxisTimeout); got != 60 {
		t.Errorf("GrantedOn(timeout) = %v, want 60", got)
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

// TestCmdGrant_TypeOutsideTheRemitStillGrants: the remit gates listing and
// selection, and nothing else (docs/flow-registration.md § Item types). A
// claimed item passed that gate when it was claimed, and a type edited since
// redirects nothing — so `grant` acts on the item's records rather than
// refusing on the type, which would strand exactly the run a person is trying
// to unstick.
//
// Replaces the #25 refusal, which read the remit through a per-type flow lookup
// that no longer exists: with one flow per binary there is no "no flow handles
// this type" state for a claimed item to be in.
func TestCmdGrant_TypeOutsideTheRemitStillGrants(t *testing.T) {
	a := &stubAgent{name: "stub"}
	app, be, claim := testAppItem(t,
		flow.Item{Ref: itemRefFor("1"), Type: "chore", Title: "chore#1"},
		[]flow.ItemType{"task"}, // "chore" is outside the remit
		func(f *flow.Flow) {
			f.AddStep("write plan", "plan", func(ctx flow.StepCtx) (flow.StepResult, error) {
				return ctx.Finalize(flow.DispositionResolved, "done").Markdown("the plan"), nil
			}, flow.StepConfig{Prompts: flow.PromptsAgent, Role: "contributor", Entry: true, MayFinalize: []flow.Disposition{flow.DispositionResolved}})
		}, a)

	var errBuf bytes.Buffer
	app.Err = &errBuf
	app.Out = newDiscardWriter()

	// A named target rather than --all: the sweep only tops up a step that has
	// actually run out, and this item has run nothing. The remit is the subject.
	code := app.cmdGrant(context.Background(), []string{"plan", "--invocations", "1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	state, err := be.Load(context.Background(), claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Ledger.Row("plan").GrantedOn(flow.AxisInvocations); got == 0 {
		t.Error("nothing was granted — the grant must land on the ledger of an item outside the remit")
	}
}

// Every member of the park vocabulary has a remedy written down, including the
// two that take the fall-through. The crosswalk walks flow.AllParkKinds(), so a
// kind added to the vocabulary lands here rather than in `remedyFor`'s default,
// where a wrong instruction reads exactly like a right one (#324).
//
// The answer is recorded as a DISTINGUISHING SUBSTRING rather than the whole
// sentence: these lines are paragraphs an operator reads, and pinning them
// verbatim would make every wording change a test edit while proving nothing
// more than that the arm exists.
func TestRemedyFor_AnswersForEveryParkKind(t *testing.T) {
	// want holds the substring a kind's own arm must carry. A kind mapped to ""
	// takes genericRemedy, deliberately — see below.
	want := map[flow.ParkKind]string{
		// The generic line IS the blocked park's remedy: a person clears what
		// the item waits on, then the step runs again. There is nothing an arm
		// of its own would say differently.
		flow.ParkBlocked: "",
		// Never reached: planPark refuses a non-treasurer park before calling
		// this, so `grant` answers the treasurer-refused kind itself — topping
		// up the parked axis is the whole command. The generic line stands as
		// the answer to a question nothing asks.
		flow.ParkTreasurerRefused:   "",
		flow.ParkQuestion:           "Answer the question on the item",
		flow.ParkStepDidNotComplete: "the step left its job undone",
		flow.ParkInfraTransient:     "Re-run the step once the infrastructure is back",
		// The same arm as infra-transient, shared on purpose: neither consumed
		// budget and neither is cleared by a grant.
		flow.ParkRemoteUnreachable: "Re-run the step once the infrastructure is back",
		flow.ParkRefused:           "the failure is deterministic",
		flow.ParkWriteContract:     "outside its declared contract",
		flow.ParkAccountExhausted:  "the agent account's allowance is spent",
	}
	kinds := flow.AllParkKinds()
	if len(kinds) != len(want) {
		t.Fatalf("AllParkKinds() has %d members, but %d remedies are written down: %v", len(kinds), len(want), kinds)
	}
	for _, kind := range kinds {
		substr, ok := want[kind]
		if !ok {
			t.Errorf("park kind %q has no remedy written down here", kind)
			continue
		}
		got := remedyFor(kind)
		if substr == "" {
			if got != genericRemedy {
				t.Errorf("remedyFor(%q) = %q, want the generic line %q — this kind is recorded as taking the fall-through", kind, got, genericRemedy)
			}
			continue
		}
		if got == genericRemedy {
			t.Errorf("remedyFor(%q) fell through to the generic line; it is recorded as having an arm of its own naming %q", kind, substr)
			continue
		}
		if !strings.Contains(got, substr) {
			t.Errorf("remedyFor(%q) = %q, want it to mention %q", kind, got, substr)
		}
	}
	// A kind this binary does not know — a park written by a newer one — takes
	// the generic line. It is the one instruction that is never actively wrong:
	// whatever stopped the item, a person looking at it is the way forward.
	if got := remedyFor("from-the-future"); got != genericRemedy {
		t.Errorf("remedyFor(unknown kind) = %q, want the generic line %q", got, genericRemedy)
	}
}
