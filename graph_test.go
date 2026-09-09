package flow

import (
	"strings"
	"testing"
)

// wantGraphError asserts ValidateGraph refused, and that the message says
// enough to act on: the flow, and every fragment the caller names. An error
// that refused for a different reason than the test set up would otherwise
// pass.
func wantGraphError(t *testing.T, f *Flow, fragments ...string) {
	t.Helper()
	err := f.ValidateGraph()
	if err == nil {
		t.Fatalf("ValidateGraph() = nil, want an error mentioning %v", fragments)
	}
	msg := err.Error()
	if !strings.Contains(msg, f.name) {
		t.Errorf("error %q does not name the flow %q", msg, f.name)
	}
	for _, want := range fragments {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func wantGraphOK(t *testing.T, f *Flow) {
	t.Helper()
	if err := f.ValidateGraph(); err != nil {
		t.Fatalf("ValidateGraph() = %v, want nil", err)
	}
}

// Zero entries is the one entry-count failure that reaches here: a second
// entry is refused at registration, and that is covered in flow_test.go.
func TestValidateGraph_RefusesZeroEntries(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "no entry step", "Entry: true")
}

func TestValidateGraph_RefusesSuccessorNamingNoRegisteredItem(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry:       true,
		Next:        []StepId{"typo"},
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "write plan", "plan", "typo", "no registered lifecycle item")
}

func TestValidateGraph_RefusesStepNothingRoutesTo(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	// Registered, routes to the entry, and nothing routes to it.
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Next: []StepId{"plan"},
	})
	wantGraphError(t, f, "implement", "impl", "not reachable from the entry")
}

// A cycle with no finalizing exit: both steps are reachable from the entry,
// and no sequence of routes from either one ever ends the flow.
func TestValidateGraph_RefusesCycleWithNoFinalization(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry: true,
		Next:  []StepId{"impl"},
	})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Next: []StepId{"plan"},
	})
	wantGraphError(t, f, "write plan", "plan", "cannot reach finalization")
}

// Finalization existing somewhere is not enough: the branch that cannot reach
// it is refused even though the entry can.
func TestValidateGraph_RefusesBranchThatCannotFinalize(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry:       true,
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionRejected},
	})
	// Reachable from the entry, routes nowhere, finalizes nothing.
	f.AddStep("implement", "impl", noopHandler, StepConfig{})
	wantGraphError(t, f, "implement", "impl", "cannot reach finalization")
}

func TestValidateGraph_RefusesSignalWaitWithNoSuccessor(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry:       true,
		Next:        []StepId{"pr-merged"},
		MayFinalize: []Disposition{DispositionResolved},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{})
	wantGraphError(t, f, "await merge", "pr-merged", "0 successors", "exactly one")
}

func TestValidateGraph_RefusesSignalWaitWithTwoSuccessors(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry: true,
		Next:  []StepId{"pr-merged"},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{Next: []StepId{"impl", "review"}})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		MayFinalize: []Disposition{DispositionResolved},
	})
	f.AddStep("review", "review", noopHandler, StepConfig{
		MayFinalize: []Disposition{DispositionRejected},
	})
	wantGraphError(t, f, "await merge", "pr-merged", "2 successors", "exactly one")
}

func TestValidateGraph_AcceptsEntryThroughStepToFinalization(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry: true,
		Next:  []StepId{"impl"},
	})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		MayFinalize: []Disposition{DispositionResolved, DispositionRejected},
	})
	wantGraphOK(t, f)
}

// A route through a signal wait: the wait's one successor is what carries
// reachability and finalization across it.
func TestValidateGraph_AcceptsRouteThroughSignalWait(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{
		Entry: true,
		Next:  []StepId{"pr-merged"},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{Next: []StepId{"merge-commit"}})
	f.AddStep("record merge commit", "merge-commit", noopHandler, StepConfig{
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphOK(t, f)
}

// A cycle is legal as long as something in it can end the flow — a handback
// that loops work back for rework is exactly that shape.
func TestValidateGraph_AcceptsCycleWithAFinalizingExit(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Entry: true,
		Next:  []StepId{"review"},
	})
	f.AddStep("review the work", "review", noopHandler, StepConfig{
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionResolved, DispositionRejected},
	})
	wantGraphOK(t, f)
}

// The one-step graph: entry and finalizer at once, routing nowhere.
func TestValidateGraph_AcceptsLoneEntryThatFinalizes(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphOK(t, f)
}
