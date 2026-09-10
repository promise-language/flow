package flow

import (
	"errors"
	"slices"
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

// soloRole is the one role the graph-shape fixtures declare and tag every step
// with. The graph checks are about routes, not roles, so the fixtures carry the
// smallest declaration that satisfies the role checks and says nothing else.
const soloRole RoleName = "solo"

// soloFlow is a flow with soloRole already declared.
func soloFlow(name string) *Flow {
	f := NewFlow(name, nil)
	f.Role(soloRole, CapPush)
	return f
}

// Zero entries is the one entry-count failure that reaches here: a second
// entry is refused at registration, and that is covered in flow_test.go.
func TestValidateGraph_RefusesZeroEntries(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "no entry step", "Entry: true")
}

func TestValidateGraph_RefusesSuccessorNamingNoRegisteredItem(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Entry:       true,
		Next:        []StepId{"typo"},
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "write plan", "plan", "typo", "no registered lifecycle item")
}

func TestValidateGraph_RefusesStepNothingRoutesTo(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	// Registered, routes to the entry, and nothing routes to it.
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Next:    []StepId{"plan"},
	})
	wantGraphError(t, f, "implement", "impl", "not reachable from the entry")
}

// A cycle with no finalizing exit: both steps are reachable from the entry,
// and no sequence of routes from either one ever ends the flow.
func TestValidateGraph_RefusesCycleWithNoFinalization(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Entry:   true,
		Next:    []StepId{"impl"},
	})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Next:    []StepId{"plan"},
	})
	wantGraphError(t, f, "write plan", "plan", "cannot reach finalization")
}

// Finalization existing somewhere is not enough: the branch that cannot reach
// it is refused even though the entry can.
func TestValidateGraph_RefusesBranchThatCannotFinalize(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Entry:       true,
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionRejected},
	})
	// Reachable from the entry, routes nowhere, finalizes nothing.
	f.AddStep("implement", "impl", noopHandler, StepConfig{Prompts: PromptsAgent, Role: soloRole})
	wantGraphError(t, f, "implement", "impl", "cannot reach finalization")
}

func TestValidateGraph_RefusesSignalWaitWithNoSuccessor(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Entry:       true,
		Next:        []StepId{"pr-merged"},
		MayFinalize: []Disposition{DispositionResolved},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{})
	wantGraphError(t, f, "await merge", "pr-merged", "0 successors", "exactly one")
}

func TestValidateGraph_RefusesSignalWaitWithTwoSuccessors(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Entry:   true,
		Next:    []StepId{"pr-merged"},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{Next: []StepId{"impl", "review"}})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		MayFinalize: []Disposition{DispositionResolved},
	})
	f.AddStep("review", "review", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		MayFinalize: []Disposition{DispositionRejected},
	})
	wantGraphError(t, f, "await merge", "pr-merged", "2 successors", "exactly one")
}

func TestValidateGraph_AcceptsEntryThroughStepToFinalization(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Entry:   true,
		Next:    []StepId{"impl"},
	})
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		MayFinalize: []Disposition{DispositionResolved, DispositionRejected},
	})
	wantGraphOK(t, f)
}

// A route through a signal wait: the wait's one successor is what carries
// reachability and finalization across it.
func TestValidateGraph_AcceptsRouteThroughSignalWait(t *testing.T) {
	f := soloFlow("x")
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Entry:   true,
		Next:    []StepId{"pr-merged"},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{Next: []StepId{"merge-commit"}})
	f.AddStep("record merge commit", "merge-commit", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphOK(t, f)
}

// A cycle is legal as long as something in it can end the flow — a handback
// that loops work back for rework is exactly that shape.
func TestValidateGraph_AcceptsCycleWithAFinalizingExit(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("implement", "impl", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Entry:   true,
		Next:    []StepId{"review"},
	})
	f.AddStep("review the work", "review", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionResolved, DispositionRejected},
	})
	wantGraphOK(t, f)
}

// The one-step graph: entry and finalizer at once, routing nowhere.
func TestValidateGraph_AcceptsLoneEntryThatFinalizes(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphOK(t, f)
}

// ---------------------------------------------------------------------------
// Check 6: the roles.
// ---------------------------------------------------------------------------

// The check the declaration surface exists for. A tag outside the declared set
// is a typo that would otherwise match nothing at runtime: the item would await
// a role no runner can assume, and nothing would say why.
func TestValidateGraph_RefusesTagNamingNoDeclaredRole(t *testing.T) {
	f := soloFlow("x")
	f.Role("maintainer", CapPush, CapMerge)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        "contribtuor", // the typo the check is for
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	// The message names the flow, the step, its result id, the role asked for,
	// and what it could have been.
	wantGraphError(t, f, "write plan", "plan", "contribtuor", "not declared", "solo", "maintainer")

	// And it is the typed refusal, so a caller can tell an unknown role from
	// any other graph failure.
	var unknown ErrUnknownRole
	if !errors.As(f.ValidateGraph(), &unknown) {
		t.Fatalf("ValidateGraph() = %v, want an ErrUnknownRole", f.ValidateGraph())
	}
	if unknown.Role != "contribtuor" {
		t.Errorf("ErrUnknownRole.Role = %q, want the tag that named nothing", unknown.Role)
	}
	if !slices.Equal(unknown.Declared, []RoleName{soloRole, "maintainer"}) {
		t.Errorf("ErrUnknownRole.Declared = %v, want the declared set in declaration order", unknown.Declared)
	}
}

// StepConfig.Role is required on steps. An untagged one is nobody's move: no
// runner's assumable set matches it.
func TestValidateGraph_RefusesStepWithNoRole(t *testing.T) {
	f := soloFlow("x")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "write plan", "plan", "declares no Role", "solo")
}

// A role requiring nothing is covered by every account, so the steps it tags
// are performable by an account that can do nothing — a boundary written down
// and enforcing nothing, which is worse than declaring none.
func TestValidateGraph_RefusesRoleWithNoCapability(t *testing.T) {
	f := NewFlow("x", nil)
	f.Role("ghost")
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        "ghost",
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "ghost", "names no capability", string(CapPush))
}

// A signal wait carries no role and is exempt: it has no handler, performs
// nothing, and AwaitSignal already panics on a role given to one.
func TestValidateGraph_AcceptsSignalWaitWithNoRole(t *testing.T) {
	f := soloFlow("x")
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{
		Prompts: PromptsAgent,
		Role:    soloRole,
		Entry:   true,
		Next:    []StepId{"pr-merged"},
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{Next: []StepId{"merge-commit"}})
	f.AddStep("record merge commit", "merge-commit", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphOK(t, f)
}

// The other side of that exemption, and where it stops. A SIGNAL STEP is not a
// wait: it has a handler, it performs the act that raises the signal, and
// AddSignalStep accepts the empty Role where AwaitSignal panics on any — so the
// only thing standing between an untagged one and an item awaiting nobody is
// this refusal. An exemption written on the kind rather than on the wait would
// take both with it and nothing else would notice.
func TestValidateGraph_RefusesSignalStepWithNoRole(t *testing.T) {
	f := soloFlow("x")
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphError(t, f, "create pr", "pr-open", "declares no Role", "solo")
}

// A declared role no step is tagged with is NOT refused. Coverage is the
// runner's business, not the graph's: a flow declaring a role its own steps do
// not perform is over-declared, not broken, and refusing it would make the
// three shipped compositions unable to share one declaration table.
func TestValidateGraph_AcceptsADeclaredRoleNoStepPerforms(t *testing.T) {
	f := soloFlow("x")
	f.Role("reviewer", CapApprove)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsAgent,
		Role:        soloRole,
		Entry:       true,
		MayFinalize: []Disposition{DispositionResolved},
	})
	wantGraphOK(t, f)
}
