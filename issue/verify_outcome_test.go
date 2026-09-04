package issue

import (
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// A failing check, a lock timeout and a missing binary used to be ONE value: a
// non-nil error from Verify. Both call sites did a bare `verr != nil`, so a
// step that failed because a lock timed out was indistinguishable from one that
// failed because the code is broken — and the two have different budget
// consequences.
//
// The Outcome is what separates them, and these pin the mapping. Each row is a
// different sentinel because each buys the step a different treatment: a
// timeout parks infra-transient and burns no invocation, a run that never
// happened parks refused and burns none either, and only a measurement is
// allowed to be read as "the tree is bad".
func TestVerifyOutcomeError_SeparatesTheThreeKindsOfFailure(t *testing.T) {
	for _, tc := range []struct {
		outcome  flow.Outcome
		sentinel error
		why      string
	}{
		{flow.OutcomeMeasured, nil, "it ran and reported; the exit code is the measurement"},
		{flow.OutcomeTimedOut, flow.ErrTransient, "the wait, or the host — a retry can resolve it"},
		{flow.OutcomeCouldNotStart, flow.ErrRefused, "nothing ran; a retry loop on a missing binary never terminates"},
		{flow.OutcomeDied, flow.ErrRefused, "no measurement exists, so nothing may be concluded about the tree"},
		{flow.OutcomeBrokeContract, flow.ErrRefused, "the command's own protocol failure is not the change failing"},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			err := verifyOutcomeError(flow.CommandRun{
				Command: flow.CommandVerify, Outcome: tc.outcome, Detail: "the runner's account",
			})
			if tc.sentinel == nil {
				if err != nil {
					t.Fatalf("verifyOutcomeError = %v, want nil — %s", err, tc.why)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyOutcomeError = nil for %s, want %v — %s", tc.outcome, tc.sentinel, tc.why)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("err = %v, want it to wrap %v — %s", err, tc.sentinel, tc.why)
			}
			// The other sentinel must NOT match: that is the whole distinction.
			other := flow.ErrRefused
			if tc.sentinel == flow.ErrRefused {
				other = flow.ErrTransient
			}
			if errors.Is(err, other) {
				t.Errorf("err = %v also wraps %v — the two treatments have collapsed back together", err, other)
			}
			if !strings.Contains(err.Error(), "the runner's account") {
				t.Errorf("err = %q, want the runner's Detail — it is the only account of what happened", err)
			}
		})
	}
}

// Every outcome is classified. A sixth added later must be given a treatment
// rather than silently falling into whichever branch happens to be `default`
// without anyone deciding — enumerated from AllOutcomes so the check cannot go
// stale the way a hand-written list does.
func TestVerifyOutcomeError_ClassifiesEveryDeclaredOutcome(t *testing.T) {
	for _, o := range flow.AllOutcomes() {
		err := verifyOutcomeError(flow.CommandRun{Command: flow.CommandVerify, Outcome: o})
		switch o {
		case flow.OutcomeMeasured:
			if err != nil {
				t.Errorf("%s: got %v, want nil", o, err)
			}
		default:
			if !errors.Is(err, flow.ErrTransient) && !errors.Is(err, flow.ErrRefused) {
				t.Errorf("%s: got %v, which is neither transient nor refused — the step would burn a turn on it", o, err)
			}
		}
	}
}

// A verify that TIMED OUT does not reach an agent. The old shape sent every
// non-nil verify error into the fix loop, so a lock timeout bought a fix round
// and an agent turn spent re-fixing code that was never measured.
func TestStepImplement_ATimedOutVerifyDoesNotSpendAFixRound(t *testing.T) {
	wt := resumedWorktree()
	wt.verifyOutcome = flow.OutcomeTimedOut
	agent := &scriptedAgent{}
	ctx := ctxWithPlan(wt, agent)

	err := testBuilder(t).stepImplement(ctx)
	if !errors.Is(err, flow.ErrTransient) {
		t.Fatalf("stepImplement = %v, want ErrTransient — the wait is not the change failing", err)
	}
	if agent.calls != 1 {
		t.Errorf("agent ran %d times, want 1 (the opening turn only) — a timeout bought a fix round", agent.calls)
	}
}

// A verify that COULD NOT START is refused rather than treated as a failing
// check: the binary is absent, so a fix round asks an agent to repair code
// against no measurement at all, and the retry never terminates.
func TestStepImplement_AVerifyThatNeverRanIsRefusedNotFixed(t *testing.T) {
	wt := resumedWorktree()
	wt.verifyOutcome = flow.OutcomeCouldNotStart
	agent := &scriptedAgent{}
	ctx := ctxWithPlan(wt, agent)

	err := testBuilder(t).stepImplement(ctx)
	if !errors.Is(err, flow.ErrRefused) {
		t.Fatalf("stepImplement = %v, want ErrRefused", err)
	}
	if agent.calls != 1 {
		t.Errorf("agent ran %d times, want 1 — a command that never ran bought a fix round", agent.calls)
	}
}

// A NON-NIL ERROR from Run means no command ran and no outcome exists. It is
// returned as it stands: reading it as a failing check would blame the change
// for a request the runner could not attempt.
func TestStepImplement_ARunThatCouldNotBeAttemptedPropagates(t *testing.T) {
	wt := resumedWorktree()
	wt.runErr = errors.New("this orchestrator declares no verify command")
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepImplement(ctx)
	if err == nil || !strings.Contains(err.Error(), "declares no verify command") {
		t.Fatalf("stepImplement = %v, want the runner's own error unchanged", err)
	}
}
