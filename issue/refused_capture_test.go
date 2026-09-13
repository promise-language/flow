package issue

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/cli"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// A disclosure refusal on an artifact-producing step is revised inside the
// dispatch, and the shipped flow is what has to show it: the plan step reads
// its work in progress back through the prompt, so the round only works if the
// refused record reaches the second prompt with the framing that asks for an
// amendment rather than a fresh plan. A test on the SDK's capture path proves
// the handler re-ran; only this proves what the agent was told when it did.
//
// Driven through the binary's own `run-step` (runStep), for the reason the
// mechanical-request tests are: the lookups a dispatch reads are built by the
// startup gate, and every step here elects an artifact.

// refusingArena is a fake orchestrator whose APPEND answers on a script, so a
// test can refuse the first capture the way the guard does and accept the
// next. Everything else — the journal, the work-in-progress store, the ledger,
// the park, the claim — is the fake's.
type refusingArena struct {
	*fake.Orchestrator
	appendErrs []error
	appends    int
}

func (a *refusingArena) AppendEntry(ctx context.Context, ref flow.ItemRef, e flow.JournalEntry) error {
	a.appends++
	if len(a.appendErrs) > 0 {
		err := a.appendErrs[0]
		a.appendErrs = a.appendErrs[1:]
		if err != nil {
			return err
		}
	}
	return a.Orchestrator.AppendEntry(ctx, ref, e)
}

// planRefusal is the shape a refused plan comment arrives in.
var planRefusal = flow.ErrDisclosureRefused{
	Act:    flow.ActArtifactComment,
	Reason: errors.New("an absolute home path names the machine's user"),
}

// atThePlan positions item 42 at the entry step: an empty journal, which is
// where the plan is derived from.
func atThePlan(t *testing.T, arena *refusingArena, agent flow.Agent) (cli.App, flow.Claim) {
	t.Helper()
	ctx := context.Background()
	app, err := BuildApp(ctx, Config{
		BinaryName: "issue",
		VerifyCmd:  []string{"make", "check"},
		Coverage:   []Role{RoleContributor},
		BaseBranch: "main",
	}, Deps{Orchestrator: arena, Agent: agent})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	claim, err := arena.Claim(ctx, arena.Ref("42"), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return app, claim
}

func TestARefusedPlanIsRevisedInTheSameDispatch(t *testing.T) {
	ctx := context.Background()
	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	arena := &refusingArena{Orchestrator: be, appendErrs: []error{planRefusal}}

	// The fixture reads as a home path without naming anyone: "someone" is a
	// placeholder the real disclosure rules exempt, so a test proving such text
	// is never published can itself be pushed. The refusal is scripted on the
	// arena, so the fixture only has to be recognisable in the second prompt.
	const firstPlan = "the parser lives at /home/someone/src/parser.go"
	const secondPlan = "the parser lives at src/parser.go"
	agent := &scriptedAgent{replies: []string{planned(firstPlan), planned(secondPlan)}}
	app, claim := atThePlan(t, arena, agent)

	res := runStep(t, app)
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("plan: %s: %s (park %+v)", res.Status, res.Reason, res.Park)
	}

	// Two prompts in one dispatch: the plan, and the plan again with the
	// refusal in hand. The second is the one that has to say the right thing.
	if len(agent.prompts) != 2 {
		t.Fatalf("the dispatch sent %d prompt(s), want 2 — the refused turn and its revision", len(agent.prompts))
	}
	second := agent.prompts[1]
	for _, want := range []string{
		planRefusal.Reason.Error(), // what the guard found
		firstPlan,                  // the text it found it in
		refusedResultFraming,       // amend this, do not re-plan
	} {
		if !strings.Contains(second, want) {
			t.Errorf("the revision prompt does not carry %q:\n%s", want, second)
		}
	}
	if strings.Contains(second, "notes you left yourself") {
		t.Errorf("the revision prompt frames the refused plan as notes to continue from:\n%s", second)
	}
	if strings.Contains(agent.prompts[0], planRefusal.Reason.Error()) {
		t.Errorf("the first prompt already carries a refusal nothing had made yet:\n%s", agent.prompts[0])
	}

	// One plan entry, routing on to the branch, carrying the SECOND plan. The
	// refused one was appended and refused; nothing of it is in the record.
	if arena.appends != 2 {
		t.Errorf("appended %d times, want 2 — the refused capture and the revision", arena.appends)
	}
	state, err := arena.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != 1 {
		t.Fatalf("journal = %+v, want the one entry the revision appended", state.Journal)
	}
	last := state.Journal[0]
	if last.Step != flow.StepId(StepPlan) || last.Route.Next != flow.StepId(StepBranch) {
		t.Errorf("journalled %q → %q, want the plan electing the branch", last.Step, last.Route.Next)
	}
	if last.Result.Markdown != planned(secondPlan) {
		t.Errorf("the plan artifact is %q, want the revised plan", last.Result.Markdown)
	}
	if strings.Contains(last.Result.Markdown, firstPlan) {
		t.Errorf("the journalled plan carries the refused text: %q", last.Result.Markdown)
	}
	if state.Park != nil {
		t.Errorf("Park = %+v, want none — the round landed inside the dispatch", state.Park)
	}
	if got := state.Ledger.Row(flow.StepId(StepPlan)).Dispatches; got != 1 {
		t.Errorf("Dispatches = %d, want 1 — a correction round is priced as a round, not as a dispatch", got)
	}
}
