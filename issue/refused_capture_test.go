package issue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// askingArena is refusingArena with the QUESTION surface scripted too, and a
// human on the other end of it: the question path keeps a refusal record of its
// own, so the two surfaces have to be drivable in one test to show that neither
// is read as the other.
type askingArena struct {
	*refusingArena
	askErrs []error
	// answers is what a person has replied, read by the gate that lets a
	// question-parked step run again and by the prompt that carries the reply.
	answers []Answer
}

func (a *askingArena) AskQuestion(ctx context.Context, ref flow.ItemRef, q flow.AgentQuestion) (flow.Question, error) {
	if len(a.askErrs) > 0 {
		err := a.askErrs[0]
		a.askErrs = a.askErrs[1:]
		if err != nil {
			return flow.Question{}, err
		}
	}
	return a.refusingArena.AskQuestion(ctx, ref, q)
}

func (a *askingArena) ReadAnswers(context.Context, flow.Item, time.Time, string) ([]Answer, error) {
	return a.answers, nil
}

// planRefusal is the shape a refused plan comment arrives in.
var planRefusal = flow.ErrDisclosureRefused{
	Act:    flow.ActArtifactComment,
	Reason: errors.New("an absolute home path names the machine's user"),
}

// testArena is what atThePlan needs of a backend: everything a flow does, plus
// the fake's own way of naming an item. An interface so the two arenas here —
// one scripting the RESULT surface, one the QUESTION surface — can both be
// positioned by the same helper.
type testArena interface {
	flow.Orchestrator
	Ref(string) flow.ItemRef
}

// atThePlan positions item 42 at the entry step: an empty journal, which is
// where the plan is derived from.
func atThePlan(t *testing.T, arena testArena, agent flow.Agent) (cli.App, flow.Claim) {
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

// The other surface, through the same store, and the case that says the two are
// told apart by what was refused rather than by the fact of a refusal.
//
// A QUESTION carrying a rooted path is refused, revised inside the invocation,
// accepted, and the step parks awaiting an answer. A person answers, the step
// runs again — and what it must NOT be told is to produce its plan from the
// question. The question is spent by then: it was revised, then answered, and
// the plan is still owed.
//
// Both halves are load-bearing and both are asserted. The stash must hold the
// accepted turn rather than the refusal it replaced (resolveQuestion), and the
// prompt must read whatever it holds as working-out rather than as a refused
// result (WorkInProgressBlock). Either one alone leaves the other free to
// regress into the failure this is here about.
func TestAnAnsweredQuestionDoesNotResumeAsARefusedResult(t *testing.T) {
	ctx := context.Background()
	be := fake.New(resolveSignals()...)
	be.SetSupportedArtifacts(resolveArtifacts()...)
	be.AddItem("42", flow.Item{Type: "task", Title: "widget is broken"})
	const guardAnswer = "an absolute home path names the machine's user"
	arena := &askingArena{
		refusingArena: &refusingArena{Orchestrator: be},
		askErrs:       []error{flow.ErrDisclosureRefused{Act: flow.ActQuestion, Reason: errors.New(guardAnswer)}},
	}

	// "someone" is a placeholder the real disclosure rules exempt, so a test
	// proving such text is never published can itself be pushed.
	const disclosingQuestion = "The checkout is at /home/someone/src.\n" +
		"NEEDS-ANSWER: which base branch?\n```\nmain or trunk\n```"
	const cleanQuestion = "The checkout is this repository.\n" +
		"NEEDS-ANSWER: which base branch?\n```\nmain or trunk\n```"
	const thePlan = "the parser lives at src/parser.go"
	agent := &scriptedAgent{replies: []string{disclosingQuestion, cleanQuestion, planned(thePlan)}}
	app, claim := atThePlan(t, arena, agent)

	// One dispatch: the question, the refusal, the revision, the park.
	res := runStep(t, app)
	if res.Park == nil || res.Park.Kind != flow.ParkQuestion {
		t.Fatalf("plan: %s: %s (park %+v), want a park awaiting an answer", res.Status, res.Reason, res.Park)
	}
	if agent.calls != 2 {
		t.Fatalf("the agent ran %d times, want 2 — the question and its revision", agent.calls)
	}

	// What the dispatch left behind is the turn the question was ACCEPTED on.
	// The refusal record written on the way there was about a question that no
	// longer exists, and a record that outlived it is what hands the next run a
	// refusal to answer.
	wip, err := arena.LoadWorkInProgress(ctx, claim.ItemRef, flow.StepId(StepPlan))
	if err != nil {
		t.Fatalf("LoadWorkInProgress: %v", err)
	}
	if flow.IsRefusedResultRecord(wip) {
		t.Errorf("the question path stashed a record that reads as the step's refused RESULT:\n%s", wip)
	}
	if strings.Contains(wip, guardAnswer) {
		t.Errorf("the refusal outlived the question it was about — the stash still carries it:\n%s", wip)
	}
	if !strings.Contains(wip, cleanQuestion) {
		t.Errorf("the stash does not carry the turn the question was accepted on:\n%s", wip)
	}

	// A person answers, and the step runs again.
	arena.answers = []Answer{{Answer: "main", Author: "reporter"}}
	res = runStep(t, app)
	if res.Status != string(flow.StatusDone) {
		t.Fatalf("the answered step: %s: %s (park %+v)", res.Status, res.Reason, res.Park)
	}
	if agent.calls != 3 {
		t.Fatalf("the agent ran %d times overall, want 3 — the question, its revision, and the plan", agent.calls)
	}
	resumed := agent.prompts[2]
	if strings.Contains(resumed, refusedResultFraming) {
		t.Errorf("the resumed step is told to produce its result from the question it already had answered:\n%s", resumed)
	}
	if strings.Contains(resumed, reviseGuidance) {
		t.Errorf("the resumed step is handed the result-revision guidance over a spent question:\n%s", resumed)
	}
	if !strings.Contains(resumed, "not a result") {
		t.Errorf("the resumed step is not told its stashed turn is working-out:\n%s", resumed)
	}

	// And it planned: the journalled plan is the plan, not the question.
	state, err := arena.Load(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Journal) != 1 {
		t.Fatalf("journal = %+v, want the one plan entry", state.Journal)
	}
	if got := state.Journal[0].Result.Markdown; !strings.Contains(got, thePlan) {
		t.Errorf("the plan artifact is %q, want the plan", got)
	}
}
