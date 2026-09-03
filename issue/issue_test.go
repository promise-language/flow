package issue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
	ghbackend "github.com/promise-language/flow/pkg/backend/github"
)

// ---------------------------------------------------------------------------
// Role selection.
// ---------------------------------------------------------------------------

func TestRoleFromPermissions(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   flow.RepoPermissions
		want Role
	}{
		// GitHub reports these cumulatively, so an admin also carries push.
		// Testing push first would call every admin a contributor.
		{"admin carries push too", flow.RepoPermissions{Admin: true, Maintain: true, Push: true, Triage: true, Pull: true}, RoleMaintainer},
		{"maintain", flow.RepoPermissions{Maintain: true, Push: true, Pull: true}, RoleMaintainer},
		{"push only", flow.RepoPermissions{Push: true, Pull: true}, RoleContributor},
		{"triage without push", flow.RepoPermissions{Triage: true, Pull: true}, RoleContributor},
		{"read only", flow.RepoPermissions{Pull: true}, RoleContributor},
		{"nothing", flow.RepoPermissions{}, RoleContributor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleFromPermissions(tc.in); got != tc.want {
				t.Errorf("roleFromPermissions(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// stubBackend implements just enough of flow.Backend to be passed around; the
// capability interfaces are what the tests actually exercise.
type stubBackend struct {
	flow.Backend // nil — these tests never call the base methods
	perms        flow.RepoPermissions
	permsErr     error
	branch       string
	branchErr    error
	answers      []flow.Answer
	answersErr   error
	sawSince     time.Time
	sawSelf      string
}

func (s *stubBackend) RepoPermissions(context.Context) (flow.RepoPermissions, error) {
	return s.perms, s.permsErr
}
func (s *stubBackend) DefaultBranch(context.Context) (string, error) {
	return s.branch, s.branchErr
}
func (s *stubBackend) ReadAnswers(_ context.Context, _ flow.Item, since time.Time, self string) ([]flow.Answer, error) {
	s.sawSince, s.sawSelf = since, self
	return s.answers, s.answersErr
}

// bareBackend implements none of the optional capabilities.
type bareBackend struct{ flow.Backend }

func TestResolveRole_ExplicitConfigWins(t *testing.T) {
	// A maintainer deliberately running their own change through the
	// contributor set is the case this exists for: the probe would say
	// maintainer, and the operator's choice has to beat it.
	be := &stubBackend{perms: flow.RepoPermissions{Admin: true}}
	got, err := resolveRole(context.Background(), Config{Role: RoleContributor}, be)
	if err != nil {
		t.Fatalf("resolveRole: %v", err)
	}
	if got != RoleContributor {
		t.Errorf("role = %q, want %q — explicit config must beat the probe", got, RoleContributor)
	}
}

func TestResolveRole_DetectsFromBackend(t *testing.T) {
	be := &stubBackend{perms: flow.RepoPermissions{Maintain: true, Push: true}}
	got, err := resolveRole(context.Background(), Config{}, be)
	if err != nil {
		t.Fatalf("resolveRole: %v", err)
	}
	if got != RoleMaintainer {
		t.Errorf("role = %q, want %q", got, RoleMaintainer)
	}
}

func TestResolveRole_RefusesWhenUndetectable(t *testing.T) {
	// Guessing here would route a contributor into merge steps they cannot
	// perform, and the failure would not surface until the merge call.
	_, err := resolveRole(context.Background(), Config{}, &bareBackend{})
	if err == nil {
		t.Fatal("want an error when the backend cannot report permissions")
	}
	if !strings.Contains(err.Error(), "Config.Role") {
		t.Errorf("error = %q, want it to name the field that fixes it", err)
	}
}

func TestResolveRole_RejectsUnknownRole(t *testing.T) {
	_, err := resolveRole(context.Background(), Config{Role: "admin"}, &stubBackend{})
	if err == nil || !strings.Contains(err.Error(), "unknown Config.Role") {
		t.Errorf("err = %v, want an unknown-role refusal", err)
	}
}

// ---------------------------------------------------------------------------
// Base branch.
// ---------------------------------------------------------------------------

func TestResolveBaseBranch(t *testing.T) {
	t.Run("config wins", func(t *testing.T) {
		be := &stubBackend{branch: "main"}
		got, err := resolveBaseBranch(context.Background(), Config{BaseBranch: "develop"}, be)
		if err != nil || got != "develop" {
			t.Errorf("got (%q, %v), want develop", got, err)
		}
	})
	t.Run("detects non-main defaults", func(t *testing.T) {
		// The whole point: "main" is not a safe literal.
		be := &stubBackend{branch: "trunk"}
		got, err := resolveBaseBranch(context.Background(), Config{}, be)
		if err != nil || got != "trunk" {
			t.Errorf("got (%q, %v), want trunk", got, err)
		}
	})
	t.Run("refuses when undetectable", func(t *testing.T) {
		_, err := resolveBaseBranch(context.Background(), Config{}, &bareBackend{})
		if err == nil || !strings.Contains(err.Error(), "Config.BaseBranch") {
			t.Errorf("err = %v, want a refusal naming the field that fixes it", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Prompt seam — the reason this package exists.
// ---------------------------------------------------------------------------

func TestRenderPrompt_ProjectBodyBeatsDefault(t *testing.T) {
	cfg := Config{Prompts: map[PromptID]string{
		PromptPlan: "project-specific: {{.ItemTitle}}",
	}}
	pc := PromptContext{}
	pc.ItemTitle = "widget is broken"

	got, err := renderPrompt(cfg, PromptPlan, pc)
	if err != nil {
		t.Fatalf("renderPrompt: %v", err)
	}
	if !strings.HasPrefix(got, "project-specific: widget is broken") {
		t.Errorf("got %q, want the project's body rendered at the start", got)
	}
	// The library appends required fragments even on an override.
	if !strings.Contains(got, "never by absolute path") {
		t.Errorf("override lost repoRelativePaths fragment:\n%s", got)
	}
}

// Probes the fallback MECHANISM, not the prompt's wording. An earlier version
// asserted on a phrase in the review prompt and broke the moment that prompt
// was rewritten -- which tested the text, not the behaviour it was named for.
func TestRenderPrompt_FallsBackToDefault(t *testing.T) {
	got, err := renderPrompt(Config{}, PromptReview, PromptContext{})
	if err != nil {
		t.Fatalf("renderPrompt: %v", err)
	}
	// A second call with the same empty config must produce the same output —
	// the fallback is deterministic.
	again, err := renderPrompt(Config{}, PromptReview, PromptContext{})
	if err != nil {
		t.Fatalf("renderPrompt(second): %v", err)
	}
	if got != again {
		t.Errorf("two renders of the fallback differ:\ngot  %q\nwant %q", got, again)
	}
	if got == "" {
		t.Error("fallback rendered empty")
	}
}

func TestRenderPrompt_EmptyProjectBodyFallsBack(t *testing.T) {
	// Whitespace is not a prompt. Treating it as an override would silently
	// send the agent an empty instruction.
	cfg := Config{Prompts: map[PromptID]string{PromptReview: "   \n  "}}
	got, err := renderPrompt(cfg, PromptReview, PromptContext{})
	if err != nil {
		t.Fatalf("renderPrompt: %v", err)
	}
	want, err := renderPrompt(Config{}, PromptReview, PromptContext{})
	if err != nil {
		t.Fatalf("renderPrompt(no override): %v", err)
	}
	if got != want {
		t.Errorf("a blank override did not fall back:\ngot  %q\nwant %q", got, want)
	}
}

func TestRenderPrompt_SurfacesTemplateErrors(t *testing.T) {
	cfg := Config{Prompts: map[PromptID]string{PromptPlan: "{{.NoSuchField}}"}}
	if _, err := renderPrompt(cfg, PromptPlan, PromptContext{}); err == nil {
		t.Fatal("want an error for a template referencing an unknown field")
	}
}

func TestPromptContext_PriorDiscriminatesType(t *testing.T) {
	// The reason Prior carries records rather than strings: a body must not be
	// able to interpolate a patch into a markdown slot.
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{
		StepPlan:      {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "the plan"},
		StepImplement: {Resolved: true, Type: flow.ArtifactPatch, Patch: flow.PatchBody{Diff: []byte("diff")}},
	}}

	if body, ok := pc.PriorMarkdown(StepPlan); !ok || body != "the plan" {
		t.Errorf("PriorMarkdown(plan) = (%q, %v), want the plan", body, ok)
	}
	if _, ok := pc.PriorMarkdown(StepImplement); ok {
		t.Error("PriorMarkdown must refuse a patch artifact")
	}
	if _, ok := pc.PriorPatch(StepPlan); ok {
		t.Error("PriorPatch must refuse a markdown artifact")
	}
	if p, ok := pc.PriorPatch(StepImplement); !ok || string(p.Diff) != "diff" {
		t.Errorf("PriorPatch(implementation) = (%v, %v), want the diff", p, ok)
	}
}

func TestPromptContext_PriorSkipsUnresolved(t *testing.T) {
	// A seeded-but-unresolved step has a record with no body. Handing its
	// empty string to a template would read as "the plan is blank".
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{
		StepPlan: {Resolved: false, Type: flow.ArtifactMarkdown},
	}}
	if _, ok := pc.PriorMarkdown(StepPlan); ok {
		t.Error("PriorMarkdown must not report an unresolved artifact as present")
	}
}

// ---------------------------------------------------------------------------
// Park-for-answer.
// ---------------------------------------------------------------------------

func TestQuestionMarkerRoundTrip(t *testing.T) {
	at := time.Date(2026, 8, 25, 14, 30, 0, 0, time.UTC)
	park := &flow.ParkRequest{Details: MarkQuestionAsked(at)}
	if got := QuestionAskedAt(park); !got.Equal(at) {
		t.Errorf("questionAskedAt = %v, want %v", got, at)
	}
}

func TestQuestionAskedAt_MissingMarkerIsZero(t *testing.T) {
	// Failing to the zero time reads every comment, which can only
	// over-report answers. Over-reporting resumes a step that asks again;
	// under-reporting strands it forever. Fail toward the recoverable one.
	if got := QuestionAskedAt(&flow.ParkRequest{Details: "something-else=1"}); !got.IsZero() {
		t.Errorf("got %v, want the zero time when no marker is present", got)
	}
	if got := QuestionAskedAt(&flow.ParkRequest{Details: "asked-at=not-a-timestamp"}); !got.IsZero() {
		t.Errorf("got %v, want the zero time for an unparsable marker", got)
	}
}

func TestAnswerGate_IgnoresNonQuestionParks(t *testing.T) {
	gate := answerGate(&stubBackend{}, self("flowbot"))
	// A budget park is the budget system's business; this gate must not touch it.
	state := &flow.ItemState{Park: &flow.ParkRequest{Kind: flow.ParkBudgetExhausted, Step: "plan"}}
	if err := gate(context.Background(), state); err != nil {
		t.Errorf("gate returned %v on a budget park, want nil", err)
	}
	if err := gate(context.Background(), &flow.ItemState{}); err != nil {
		t.Errorf("gate returned %v on an unparked item, want nil", err)
	}
}

func TestAnswerGate_BlocksWhenUnanswered(t *testing.T) {
	gate := answerGate(&stubBackend{answers: nil}, self("flowbot"))
	state := &flow.ItemState{Park: &flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "which database?",
	}}
	err := gate(context.Background(), state)
	if err == nil {
		t.Fatal("want a refusal when nobody has answered")
	}
	// It must be ErrBlocked, not a bare error: a plain preflight error is a
	// skip, which exits 0 and reads as "nothing to do" to whoever re-ran it.
	if !errors.Is(err, flow.ErrBlocked) {
		t.Errorf("err = %v, want it to wrap flow.ErrBlocked", err)
	}
	if !strings.Contains(err.Error(), "which database?") {
		t.Errorf("err = %q, want it to carry the question", err)
	}
}

func TestAnswerGate_PassesOnceAnswered(t *testing.T) {
	be := &stubBackend{answers: []flow.Answer{{Answer: "postgres", Author: "maintainer"}}}
	gate := answerGate(be, self("flowbot"))
	asked := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	state := &flow.ItemState{Park: &flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Details: MarkQuestionAsked(asked),
	}}
	if err := gate(context.Background(), state); err != nil {
		t.Fatalf("gate = %v, want nil once answered", err)
	}
	if !be.sawSince.Equal(asked) {
		t.Errorf("reader saw since=%v, want the ask time %v", be.sawSince, asked)
	}
	if be.sawSelf != "flowbot" {
		t.Errorf("reader saw self=%q, want the flow principal", be.sawSelf)
	}
}

func TestAnswerGate_BlocksWhenAnswersUnreadable(t *testing.T) {
	// Neither guess is safe, so refuse to guess: "unanswered" strands a step
	// somebody already answered, "answered" burns an invocation re-asking.
	be := &stubBackend{answersErr: errors.New("api down")}
	gate := answerGate(be, self("flowbot"))
	state := &flow.ItemState{Park: &flow.ParkRequest{Kind: flow.ParkQuestion, Step: "plan"}}
	err := gate(context.Background(), state)
	if err == nil || !errors.Is(err, flow.ErrBlocked) {
		t.Fatalf("err = %v, want a blocked refusal", err)
	}
	if !strings.Contains(err.Error(), "api down") {
		t.Errorf("err = %q, want it to surface the underlying cause", err)
	}
}

func TestAnswerGate_NilWhenBackendCannotRead(t *testing.T) {
	// A gate here would strand every question park permanently, since such a
	// backend can never observe the answer that would clear it.
	if gate := answerGate(&bareBackend{}, self("")); gate != nil {
		t.Error("want no gate when the backend cannot read answers")
	}
}

// An item whose park was cleared but whose questions were never formally
// answered must not pass: dispatching a handler re-discovers and re-asks,
// discarding whatever work it produced. This is the pre-flight half of
// the spend-before-terminal-check fix.
func TestAnswerGate_BlocksWhenParkClearedButQuestionsRemain(t *testing.T) {
	gate := answerGate(&stubBackend{}, self("flowbot"))
	state := &flow.ItemState{
		// Park is nil — cleared by a manual takeover or backend resolution.
		Questions: []flow.Question{
			{ID: "q1", AgentQuestion: flow.AgentQuestion{Text: "cache or store?"}},
		},
	}
	err := gate(context.Background(), state)
	if err == nil {
		t.Fatal("want a refusal when questions remain after park cleared")
	}
	if !errors.Is(err, flow.ErrBlocked) {
		t.Errorf("err = %v, want it to wrap flow.ErrBlocked", err)
	}
	if !strings.Contains(err.Error(), "unanswered question") {
		t.Errorf("err = %q, want it to name the pending questions", err)
	}
}

// Same check with a non-question park: the park is budget-exhausted, but
// questions from an earlier run remain unanswered.
func TestAnswerGate_BlocksWhenNonQuestionParkButQuestionsRemain(t *testing.T) {
	gate := answerGate(&stubBackend{}, self("flowbot"))
	state := &flow.ItemState{
		Park: &flow.ParkRequest{Kind: flow.ParkBudgetExhausted, Step: "plan"},
		Questions: []flow.Question{
			{ID: "q1", AgentQuestion: flow.AgentQuestion{Text: "cache or store?"}},
		},
	}
	err := gate(context.Background(), state)
	if err == nil {
		t.Fatal("want a refusal when questions remain alongside a budget park")
	}
	if !errors.Is(err, flow.ErrBlocked) {
		t.Errorf("err = %v, want it to wrap flow.ErrBlocked", err)
	}
}

// An item with no park and no pending questions passes cleanly.
func TestAnswerGate_PassesWhenNoParkAndNoQuestions(t *testing.T) {
	gate := answerGate(&stubBackend{}, self("flowbot"))
	if err := gate(context.Background(), &flow.ItemState{}); err != nil {
		t.Errorf("gate = %v, want nil for a clean item", err)
	}
}

// Answered questions (Answer field populated) do not block.
func TestAnswerGate_PassesWhenAllQuestionsAnswered(t *testing.T) {
	gate := answerGate(&stubBackend{}, self("flowbot"))
	state := &flow.ItemState{
		Questions: []flow.Question{
			{ID: "q1", AgentQuestion: flow.AgentQuestion{Text: "which db?"}, UserAnswer: flow.UserAnswer{Answer: "postgres"}},
		},
	}
	if err := gate(context.Background(), state); err != nil {
		t.Errorf("gate = %v, want nil when all questions are answered", err)
	}
}

// A mix of answered and unanswered questions with no park: PendingQuestions
// filters to the unanswered subset, and even one remaining blocks.
func TestAnswerGate_BlocksOnMixedQuestionsWithNoPark(t *testing.T) {
	gate := answerGate(&stubBackend{}, self("flowbot"))
	state := &flow.ItemState{
		Questions: []flow.Question{
			{ID: "q1", AgentQuestion: flow.AgentQuestion{Text: "which db?"}, UserAnswer: flow.UserAnswer{Answer: "postgres"}},
			{ID: "q2", AgentQuestion: flow.AgentQuestion{Text: "cache layer?"}},
		},
	}
	err := gate(context.Background(), state)
	if err == nil {
		t.Fatal("want a refusal when one question remains unanswered")
	}
	if !errors.Is(err, flow.ErrBlocked) {
		t.Errorf("err = %v, want it to wrap flow.ErrBlocked", err)
	}
	if !strings.Contains(err.Error(), "1 unanswered") {
		t.Errorf("err = %q, want it to report the count of pending questions", err)
	}
}

// ---------------------------------------------------------------------------
// Verify tail.
// ---------------------------------------------------------------------------

func TestVerifyTail_KeepsTheEnd(t *testing.T) {
	// A build log's useful part is the failure at the end; the head is setup
	// noise that would crowd the real error out of the re-prompt.
	var b strings.Builder
	for i := 0; i < verifyTailLines*2; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	got := verifyTail(errors.New(b.String()))
	lines := strings.Split(got, "\n")
	if len(lines) != verifyTailLines {
		t.Fatalf("kept %d lines, want %d", len(lines), verifyTailLines)
	}
	if !strings.HasSuffix(got, fmt.Sprintf("line %d", verifyTailLines*2-1)) {
		t.Errorf("tail ends %q, want the last line", got[len(got)-20:])
	}
}

func TestVerifyTail_ShortOutputUnchanged(t *testing.T) {
	if got := verifyTail(errors.New("boom")); got != "boom" {
		t.Errorf("got %q, want %q", got, "boom")
	}
	if got := verifyTail(nil); got != "" {
		t.Errorf("got %q, want empty for a nil error", got)
	}
}

// ---------------------------------------------------------------------------
// BuildApp.
// ---------------------------------------------------------------------------

func TestBuildApp_RequiresVerifyCmd(t *testing.T) {
	// Without a gate the implement step's loop has nothing to loop against,
	// which quietly turns it back into a one-shot draft.
	_, err := BuildApp(context.Background(), Config{Role: RoleContributor, BaseBranch: "main"},
		Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err == nil || !strings.Contains(err.Error(), "VerifyCmd") {
		t.Errorf("err = %v, want a refusal naming VerifyCmd", err)
	}
}

// The maintainer step set refuses at DISPATCH, not at construction. Failing
// BuildApp would take status / list / grant / doctor down with it — the
// commands a maintainer most needs to see what a contributor's run left.
func TestBuildApp_MaintainerBuildsButRefusesOnDispatch(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		Role: RoleMaintainer, BaseBranch: "main", VerifyCmd: []string{"true"},
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err != nil {
		t.Fatalf("BuildApp = %v, want an app that still serves read-only commands", err)
	}
	if len(app.Flows) != 1 {
		t.Fatalf("got %d flows, want the stand-in", len(app.Flows))
	}
	// Silently running the contributor set would have a maintainer opening a
	// pull request against their own review, so the step must refuse.
	li, ok := app.Flows[0].Item("review the implementation")
	if !ok {
		t.Fatal("stand-in flow has no maintainer step")
	}
	err = li.Handler(nil)
	if err == nil || !strings.Contains(err.Error(), "not implemented yet") {
		t.Errorf("handler err = %v, want an explicit not-yet-implemented refusal", err)
	}
}

func TestBuildApp_ContributorSliceWiresUp(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		BinaryName: "issue",
		VerifyCmd:  []string{"bin/verify", "--wasm"},
		Role:       RoleContributor,
		BaseBranch: "main",
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	if len(app.Flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(app.Flows))
	}
	if app.VerifyCmd != "bin/verify --wasm" {
		t.Errorf("VerifyCmd = %q, want the joined display form", app.VerifyCmd)
	}
	// The answer gate must be wired, or park-for-answer is inert.
	if app.Preflight == nil {
		t.Error("Preflight is nil — the answer gate was not wired")
	}
	// Ids AND types. The type is what catches `implementation` reverting to a
	// patch: the deliverable is the commit on the branch, and a copy of it can
	// be empty, is read back by nothing, and can disagree with what it copies.
	wantArtifacts := []flow.ArtifactDef{
		{Id: "plan", Type: flow.ArtifactMarkdown},
		{Id: "branch", Type: flow.ArtifactCommitHash},
		{Id: "implementation", Type: flow.ArtifactCommitHash},
		{Id: "review", Type: flow.ArtifactMarkdown},
		{Id: "coverage", Type: flow.ArtifactMarkdown},
		{Id: "branch-closed", Type: flow.ArtifactFlag},
	}
	if len(app.Artifacts) != len(wantArtifacts) {
		t.Fatalf("got %d artifacts, want %d", len(app.Artifacts), len(wantArtifacts))
	}
	for i, want := range wantArtifacts {
		if app.Artifacts[i].Id != want.Id || app.Artifacts[i].Type != want.Type {
			t.Errorf("artifact[%d] = (%q, %v), want (%q, %v)",
				i, app.Artifacts[i].Id, app.Artifacts[i].Type, want.Id, want.Type)
		}
	}
}

// The registered step set IS the document's step set: seven steps, in order,
// each producing the result that is its identity.
//
// docs/issue-flow.md § "Contributor steps". The two branch steps and the
// request are mechanical; there is no verification step, because verify is a
// command a producing step uses while working rather than a place in a
// sequence.
func TestContributorStepSetMatchesTheDocument(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		BinaryName: "issue", VerifyCmd: []string{"true"},
		Role: RoleContributor, BaseBranch: "main",
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	want := []struct {
		name   string
		result string
		kind   flow.LifecycleKind
	}{
		{"write plan", "plan", flow.LifecycleArtifact},
		{"open branch", "branch", flow.LifecycleArtifact},
		{"implement the change", "implementation", flow.LifecycleArtifact},
		{"review the work", "review", flow.LifecycleArtifact},
		{"analyze coverage", "coverage", flow.LifecycleArtifact},
		{"create pull request", "pr-open", flow.LifecycleSignal},
		{"close branch", "branch-closed", flow.LifecycleArtifact},
	}
	items := app.Flows[0].Items()
	if len(items) != len(want) {
		t.Fatalf("got %d steps %v, want %d", len(items), app.Flows[0].Steps(), len(want))
	}
	for i, w := range want {
		got := items[i]
		result := string(got.ArtifactId)
		if got.Kind == flow.LifecycleSignal {
			result = string(got.SignalId)
		}
		if got.Name != w.name || result != w.result || got.Kind != w.kind {
			t.Errorf("step[%d] = (%q, %q, %v), want (%q, %q, %v)",
				i, got.Name, result, got.Kind, w.name, w.result, w.kind)
		}
	}
	// Closing the branch has no "did the resolution complete" test of its own:
	// DeriveNext returns the first PENDING step in registration order, so a run
	// that stopped never reaches a step registered after the request.
	if items[len(items)-1].Name != "close branch" {
		t.Error("close branch is not last, so a parked or failed run would still restore the worktree")
	}
}

// A prompt slot that outlives its step is a Config.Prompts key a project can
// set, that BuildApp accepts, and that nothing will ever render. Removing a
// step means removing its slot from three files, and missing one is silent in
// every direction: the override is simply never used.
//
// Derived from defaultPrompts rather than listed, so a slot cannot be kept by
// keeping it out of a hand-written list.
func TestEveryPromptSlotBelongsToARegisteredStep(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		BinaryName: "issue", VerifyCmd: []string{"true"},
		Role: RoleContributor, BaseBranch: "main",
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	steps := map[string]bool{}
	for _, it := range app.Flows[0].Items() {
		steps[string(it.ArtifactId)] = true
		steps[string(it.SignalId)] = true
	}
	// The re-prompts are the exclusions, and for the reason they are excluded
	// everywhere else: each runs inside another step's invocation rather than
	// being a step of its own.
	inSessionReprompts := map[PromptID]bool{
		PromptImplementFix: true,
		PromptRevise:       true,
		PromptCommitRepair: true,
		PromptStageRepair:  true,
		PromptPushRepair:   true,
	}
	for id := range defaultPrompts {
		if inSessionReprompts[id] {
			continue
		}
		if !steps[string(id)] {
			t.Errorf("prompt slot %q names no registered step — a project overriding "+
				"it would see nothing happen", id)
		}
	}
}

// Three ids move across two closed sets in this change — the flow's and the
// backend's — and today nothing but a live cli.Run notices when they drift.
// cli.App refuses at startup on a mismatch, which is a failure every operator
// sees and no test does.
func TestContributorArtifactsAreInTheGitHubBackendsSchema(t *testing.T) {
	recordable := map[flow.ArtifactId]flow.ArtifactDef{}
	for _, def := range (*ghbackend.Backend)(nil).SupportedArtifacts() {
		recordable[def.Id] = def
	}
	for _, declared := range contributorArtifacts() {
		def, ok := recordable[declared.Id]
		if !ok {
			t.Errorf("artifact %q is declared by the contributor flow but the github "+
				"backend cannot record it — cli.Run refuses at startup", declared.Id)
			continue
		}
		if def.Type != declared.Type {
			t.Errorf("artifact %q is declared as %v but the backend records it as %v",
				declared.Id, declared.Type, def.Type)
		}
	}
}

type stubAgent struct{}

func (stubAgent) Name() string { return "stub" }
func (stubAgent) Run(context.Context, flow.AgentRequest) (*flow.AgentResponse, error) {
	return &flow.AgentResponse{LastText: "ok"}, nil
}

// ---------------------------------------------------------------------------
// The ask side: without it every reader-side piece is machinery with no writer.
// ---------------------------------------------------------------------------

func TestDetectQuestion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         string
		wantHeader string
		wantBody   string
		wantOK     bool
	}{
		{"plain", "I looked at this.\nNEEDS-ANSWER: Cache or new store?", "Cache or new store?", "Cache or new store?", true},
		{"trailing whitespace", "NEEDS-ANSWER:   Which one?   ", "Which one?", "Which one?", true},
		{"no sentinel", "All done, no questions.", "", "", false},
		// An agent that reasons about the mechanism before using it must not
		// trip it; the operative line is its last word on the matter.
		{
			"last occurrence wins",
			"I could emit NEEDS-ANSWER: something vague\nbut instead:\nNEEDS-ANSWER: the real question",
			"the real question", "the real question", true,
		},
		// A bare token asks nothing — parking on it would strand the step on a
		// question nobody can answer.
		{"bare token ignored", "NEEDS-ANSWER:", "", "", false},
		{"bare token then real one", "NEEDS-ANSWER:\nNEEDS-ANSWER: a real one", "a real one", "a real one", true},
		// Matched at line start, so prose mentioning it inline does not trip.
		{"mid-line mention", "the convention is to write NEEDS-ANSWER: at line start", "", "", false},
		{"case sensitive", "needs-answer: lowercase", "", "", false},
		// Column zero only. A prompt body shows the agent an INDENTED example
		// of the convention; the agent echoing that example back must not park
		// the flow on a placeholder question.
		{"indented example does not trip", "    NEEDS-ANSWER: <the decision needed>", "", "", false},
		{
			"indented example ignored, real one honored",
			"    NEEDS-ANSWER: <placeholder from the prompt>\nNEEDS-ANSWER: the real one",
			"the real one", "the real one", true,
		},

		// The evidence block: a choice is unanswerable without what it rests on.
		{
			"fenced evidence becomes the body",
			"NEEDS-ANSWER: amend, adjust, or reject?\n```\n§3 states: \"No macros.\"\nRecommendation: reject.\n```",
			"amend, adjust, or reject?",
			"§3 states: \"No macros.\"\nRecommendation: reject.",
			true,
		},
		{
			"blank lines before the fence are tolerated",
			"NEEDS-ANSWER: which?\n\n\n```\nevidence\n```",
			"which?", "evidence", true,
		},
		{
			"language tag on the fence",
			"NEEDS-ANSWER: which?\n```text\nevidence\n```",
			"which?", "evidence", true,
		},
		// Losing the evidence is the failure that matters; a few extra lines
		// cost nothing next to an unanswerable question.
		{
			"unterminated fence keeps the remainder",
			"NEEDS-ANSWER: which?\n```\nevidence that never closes",
			"which?", "evidence that never closes", true,
		},
		// Prose after the question means the agent moved on — not a block.
		{
			"prose before a fence is not a block",
			"NEEDS-ANSWER: which?\nsome trailing prose\n```\nnot evidence\n```",
			"which?", "which?", true,
		},
		{
			"empty fence falls back to the header",
			"NEEDS-ANSWER: which?\n```\n```",
			"which?", "which?", true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, body, ok := detectQuestion(tc.in)
			if ok != tc.wantOK || header != tc.wantHeader || body != tc.wantBody {
				t.Errorf("detectQuestion(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, header, body, ok, tc.wantHeader, tc.wantBody, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The refusal side: plan-step refusals.
// ---------------------------------------------------------------------------

func TestDetectRefusal(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           string
		wantKind     RefusalKind
		wantSummary  string
		wantEvidence string
		wantOK       bool
	}{
		{
			"already-done with evidence",
			"I checked the tree.\nPLAN-REFUSAL: already-done The fix is already in main\n```\ncommit abc123 added the check\n```",
			RefusalAlreadyDone, "The fix is already in main", "commit abc123 added the check", true,
		},
		{
			"duplicate without evidence block",
			"PLAN-REFUSAL: duplicate Covered by issue #12",
			"", "", "", false,
		},
		{
			"conflicts with evidence",
			"PLAN-REFUSAL: conflicts Normative doc forbids this\n```\ndocs/design.md §3: No macros.\n```",
			RefusalConflicts, "Normative doc forbids this", "docs/design.md §3: No macros.", true,
		},
		{
			"not-viable with evidence",
			"PLAN-REFUSAL: not-viable Cannot be done without breaking the API\n```\nThe public API has no extension point.\n```",
			RefusalNotViable, "Cannot be done without breaking the API", "The public API has no extension point.", true,
		},
		{
			"bare sentinel with no kind",
			"PLAN-REFUSAL:",
			"", "", "", false,
		},
		{
			"unknown kind skipped",
			"PLAN-REFUSAL: wontfix Not worth doing",
			"", "", "", false,
		},
		{
			"sentinel not at column zero",
			"    PLAN-REFUSAL: already-done Something",
			"", "", "", false,
		},
		{
			"multiple sentinels — last one wins",
			"PLAN-REFUSAL: duplicate Earlier guess\n```\nitem #7 covers this\n```\nPLAN-REFUSAL: already-done The real finding\n```\ncommit def456 already landed the fix\n```",
			RefusalAlreadyDone, "The real finding", "commit def456 already landed the fix", true,
		},
		{
			"empty text",
			"",
			"", "", "", false,
		},
		{
			"kind with no summary",
			"PLAN-REFUSAL: already-done",
			"", "", "", false,
		},
		{
			"kind with whitespace-only summary",
			"PLAN-REFUSAL: already-done   ",
			"", "", "", false,
		},
		{
			"mid-line mention does not trip",
			"the convention is PLAN-REFUSAL: already-done at line start",
			"", "", "", false,
		},
		{
			"indented example ignored, real one honored",
			"    PLAN-REFUSAL: already-done <placeholder>\nPLAN-REFUSAL: not-viable Real reason here\n```\nThe API has no extension point for this.\n```",
			RefusalNotViable, "Real reason here", "The API has no extension point for this.", true,
		},
		{
			"blank lines before fence tolerated",
			"PLAN-REFUSAL: already-done Already there\n\n\n```\nevidence\n```",
			RefusalAlreadyDone, "Already there", "evidence", true,
		},
		{
			"unterminated fence keeps remainder",
			"PLAN-REFUSAL: duplicate Covered by #5\n```\nevidence that never closes",
			RefusalDuplicate, "Covered by #5", "evidence that never closes", true,
		},
		{
			"later invalid kind falls through to earlier valid",
			"PLAN-REFUSAL: already-done Real finding\n```\nline 42 of cmd/main.go already does this\n```\nPLAN-REFUSAL: wontfix Not a real kind",
			RefusalAlreadyDone, "Real finding", "line 42 of cmd/main.go already does this", true,
		},
		{
			"later sentinel without block falls through to earlier with block",
			"PLAN-REFUSAL: duplicate Covered by #5\n```\nissue #5 tracks the same defect\n```\nPLAN-REFUSAL: already-done No block here",
			RefusalDuplicate, "Covered by #5", "issue #5 tracks the same defect", true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, summary, evidence, ok := detectRefusal(tc.in)
			if ok != tc.wantOK || kind != tc.wantKind || summary != tc.wantSummary || evidence != tc.wantEvidence {
				t.Errorf("detectRefusal(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tc.in, kind, summary, evidence, ok,
					tc.wantKind, tc.wantSummary, tc.wantEvidence, tc.wantOK)
			}
		})
	}
}

// The sentinel has to survive a round trip through the whole contract: agent
// text -> question park (stamped by the SDK) -> gate reads the marker.
func TestAskAndGateComposeEndToEnd(t *testing.T) {
	header, body, ok := detectQuestion(
		"NEEDS-ANSWER: amend the doc, adjust the item, or reject?\n" +
			"```\n§3 forbids macros; this item asks for them.\n```")
	if !ok {
		t.Fatal("sentinel not detected")
	}
	if body == header {
		t.Fatal("evidence block was dropped — the question is unanswerable without it")
	}
	asked := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	park := &flow.ParkRequest{
		Kind:    flow.ParkQuestion,
		Step:    "plan",
		Reason:  header,
		Details: MarkQuestionAsked(asked),
	}

	// Unanswered: blocked, and the question travels with the refusal.
	gate := answerGate(&stubBackend{}, self("flowbot"))
	err := gate(context.Background(), &flow.ItemState{Park: park})
	if !errors.Is(err, flow.ErrBlocked) {
		t.Fatalf("err = %v, want blocked", err)
	}
	if !strings.Contains(err.Error(), "amend the doc") {
		t.Errorf("err = %q, want it to carry the question", err)
	}

	// Answered: passes, and the reader is scoped to after the ask.
	be := &stubBackend{answers: []flow.Answer{{Answer: "amend the doc", Author: "reporter"}}}
	if err := answerGate(be, self("flowbot"))(context.Background(), &flow.ItemState{Park: park}); err != nil {
		t.Fatalf("gate = %v, want nil once answered", err)
	}
	if !be.sawSince.Equal(asked) {
		t.Errorf("reader scoped to %v, want the ask time %v", be.sawSince, asked)
	}
}

// self adapts a fixed login to the resolver answerGate takes. The resolver is a
// function so BuildApp does not have to make a network call before every
// command — including the one meant to diagnose network failures.
func self(login string) func(context.Context) string {
	return func(context.Context) string { return login }
}

// ---------------------------------------------------------------------------
// Regressions from the review. Each of these shipped broken once.
// ---------------------------------------------------------------------------

// DefaultType names what an UNTYPED item becomes. It is not the set of types
// the flow accepts, and conflating the two drops every item carrying a third
// type — with the run reporting success having selected no flow at all.
func TestItemTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want []flow.ItemType
	}{
		{"default set", Config{}, []flow.ItemType{"task", "bug"}},
		// DefaultType names what an UNTYPED item becomes; it must not replace
		// the accepted set, or every item of some third type is dropped with
		// the run reporting success.
		{"default type is folded in, not substituted", Config{DefaultType: "chore"},
			[]flow.ItemType{"task", "bug", "chore"}},
		{"no duplicate when it repeats one", Config{DefaultType: "bug"},
			[]flow.ItemType{"task", "bug"}},
		{"explicit set wins", Config{ItemTypes: []string{"feature", "task"}},
			[]flow.ItemType{"feature", "task"}},
		{"explicit set plus default type", Config{ItemTypes: []string{"feature"}, DefaultType: "task"},
			[]flow.ItemType{"feature", "task"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := itemTypes(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The library's prompt defaults must not carry tracker-specific instructions.
//
// The shared AskGuidance partial tells the agent to call
// mcp__tracker__ask_user_question "(never ask in plain text)". On this backend
// that tool does not exist, and the instruction contradicts the one signal
// detectQuestion can see — an agent obeying it would ask into the void and the
// step would resolve as though nothing had been asked.
func TestPromptOverridesAreNotTrackerSpecific(t *testing.T) {
	pc := PromptContext{}
	pc.AskGuidance = askGuidancePartial
	pc.PlanStepResolution = planResolutionPartial
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for name, body := range map[string]string{
		"AskGuidance":        pc.AskGuidance,
		"PlanStepResolution": pc.PlanStepResolution,
	} {
		if strings.Contains(body, "mcp__tracker") {
			t.Errorf("%s still names a tracker MCP tool this backend has no access to", name)
		}
		if strings.Contains(body, "works_as_intended") || strings.Contains(body, "wontfix") {
			t.Errorf("%s still sets tracker statuses the GitHub backend has no concept of", name)
		}
	}
	if !strings.Contains(pc.AskGuidance, AskSentinel) {
		t.Error("AskGuidance must teach the sentinel that detectQuestion actually enforces")
	}
	// The illustration has to be indented, or the agent's echo of it parks the
	// flow on a placeholder question.
	for _, line := range strings.Split(pc.AskGuidance, "\n") {
		if strings.HasPrefix(line, AskSentinel) {
			t.Errorf("AskGuidance shows the sentinel at column zero: %q — an echo would self-trigger", line)
		}
	}
	// PlanStepResolution must teach the refusal sentinel the handler enforces.
	if !strings.Contains(pc.PlanStepResolution, RefusalSentinel) {
		t.Error("PlanStepResolution must teach the refusal sentinel that detectRefusal actually enforces")
	}
	// The illustration has to be indented, or the agent's echo of it triggers
	// a refusal on a placeholder.
	for _, line := range strings.Split(pc.PlanStepResolution, "\n") {
		if strings.HasPrefix(line, RefusalSentinel) {
			t.Errorf("PlanStepResolution shows the refusal sentinel at column zero: %q — an echo would self-trigger", line)
		}
	}
}

// Every default prompt must render against a bare context. A default that
// referenced a field the library does not populate would fail at run time on
// the one path meant to work before a project writes its own bodies.
func TestDefaultPromptsRender(t *testing.T) {
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{}}
	pc.VerifyCmd = "make check"
	pc.VerifyOutput = "FAIL"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for id := range defaultPrompts {
		t.Run(string(id), func(t *testing.T) {
			if _, err := renderPrompt(Config{}, id, pc); err != nil {
				t.Errorf("default prompt %q does not render: %v", id, err)
			}
		})
	}
}

// Every prompt slot the canonical steps request must have a default, or a
// binary with no project prompts fails at dispatch rather than at startup.
func TestEveryPromptSlotHasADefault(t *testing.T) {
	for _, id := range []PromptID{
		PromptPlan, PromptImplement, PromptImplementFix,
		PromptReview, PromptCoverage, PromptCommitRepair,
		PromptStageRepair, PromptPushRepair,
	} {
		if _, ok := defaultPrompts[id]; !ok {
			t.Errorf("no library default for %q", id)
		}
	}
}

// The answers have to reach the prompt, or park-for-answer never converges:
// the resumed step renders a byte-identical prompt, re-asks, and re-parks with
// a fresh timestamp that excludes the answer just given.
func TestAnswersReachEveryResumableDefaultPrompt(t *testing.T) {
	pc := PromptContext{
		Prior: map[StepID]flow.ArtifactRecord{},
		Answers: []Answer{{
			Answer: "amend the document",
			Author: "maintainer",
		}},
	}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Every slot a canonical step can park from, derived rather than listed:
	// this test once named the exact failure it guards against and then
	// omitted one of the slots from its own hand-written list, which parked
	// like any other.
	//
	// The two re-prompts are the exclusions: each runs inside a single
	// invocation of the step it belongs to, resuming that session, where the
	// answer already reached the opening prompt. Repeating it would re-state
	// the answer to an agent that has it in context, and the revise prompt in
	// particular asks for one thing — the revised text — so anything else in it
	// is competing with that.
	inSessionReprompts := map[PromptID]bool{
		PromptImplementFix: true,
		PromptRevise:       true,
		PromptCommitRepair: true,
		PromptStageRepair:  true,
		PromptPushRepair:   true,
	}
	for id := range defaultPrompts {
		if inSessionReprompts[id] {
			continue
		}
		t.Run("default/"+string(id), func(t *testing.T) {
			got, err := renderPrompt(Config{}, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "amend the document") {
				t.Errorf("default prompt %q does not render the answer — a resumed step would re-ask it", id)
			}
		})
	}
	// A project override must also receive the answers, because the library
	// appends the AnswersBlock fragment for every slot that requires it.
	for id, frags := range requiredFragments {
		if !frags.answers {
			continue
		}
		t.Run("override/"+string(id), func(t *testing.T) {
			cfg := Config{Prompts: map[PromptID]string{id: "project body"}}
			got, err := renderPrompt(cfg, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "amend the document") {
				t.Errorf("override prompt %q does not render the answer — a resumed step would re-ask it", id)
			}
		})
	}
}

func TestAnswersBlock(t *testing.T) {
	t.Run("empty when nothing was answered", func(t *testing.T) {
		if got := (PromptContext{}).AnswersBlock(); got != "" {
			t.Errorf("got %q, want empty on a first run", got)
		}
	})
	t.Run("names the authors and carries the question", func(t *testing.T) {
		pc := PromptContext{Answers: []Answer{
			{Answer: "postgres", Author: "alice", Text: "which database?"},
			{Answer: "and use the existing pool", Author: "bob"},
		}}
		got := pc.AnswersBlock()
		for _, want := range []string{"alice", "postgres", "bob", "existing pool"} {
			if !strings.Contains(got, want) {
				t.Errorf("AnswersBlock() = %q, want it to contain %q", got, want)
			}
		}
		// The question travels with the replies: nothing correlates a comment
		// to a question, so the agent is the only thing that can judge whether
		// a reply actually answers — and it cannot judge without the question.
		if !strings.Contains(got, "which database?") {
			t.Errorf("AnswersBlock() = %q, want it to restate the question", got)
		}
		// It must not assert the reply IS the decision: a "+1" reaches this
		// block too, and re-asking is the available correction.
		if !strings.Contains(got, "ask again") {
			t.Errorf("AnswersBlock() = %q, want it to permit re-asking", got)
		}
	})
	t.Run("survives a missing author", func(t *testing.T) {
		got := PromptContext{Answers: []Answer{{Answer: "yes"}}}.AnswersBlock()
		if !strings.Contains(got, "yes") {
			t.Errorf("got %q, want the answer even with no author", got)
		}
	})
}

// GitHub links and auto-closes on "Closes #123" and does nothing at all with a
// bare id. This backend implements no Finalizer, so the pull request body is
// the only thing that closes the issue.
func TestClosesRefUsesGitHubSyntax(t *testing.T) {
	if got := closesRef("123"); got != "Closes #123" {
		t.Errorf("closesRef = %q, want %q", got, "Closes #123")
	}
}

// The maintainer stand-in must not SEED the item. Seeding is one-shot, so a
// required maintainer artifact would permanently checklist the issue with a
// step set that does not exist — and switching to the contributor role
// afterwards would never re-seed, leaving every step dead on "not seeded".
func TestMaintainerStandInDoesNotSeed(t *testing.T) {
	app, err := BuildApp(context.Background(), Config{
		Role: RoleMaintainer, BaseBranch: "main", VerifyCmd: []string{"true"},
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	for _, spec := range app.Flows[0].SeedSpec(nil) {
		if spec.Required {
			t.Errorf("stand-in seeds required artifact %q — this permanently "+
				"checklists the issue for a step set that does not exist", spec.Id)
		}
	}
}

// A misspelled prompt key compiles (PromptID is a string type) and would
// silently fall back to the generic library default — running the one thing
// this package exists to let a project replace.
func TestBuildApp_RejectsUnknownPromptKey(t *testing.T) {
	_, err := BuildApp(context.Background(), Config{
		Role: RoleContributor, BaseBranch: "main", VerifyCmd: []string{"true"},
		Prompts: map[PromptID]string{"implementaion": "oops"},
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err == nil || !strings.Contains(err.Error(), "implementaion") {
		t.Errorf("err = %v, want a refusal naming the bad key", err)
	}
}

func TestBuildApp_AcceptsEveryRealPromptKey(t *testing.T) {
	prompts := map[PromptID]string{}
	for id := range defaultPrompts {
		prompts[id] = "body"
	}
	if _, err := BuildApp(context.Background(), Config{
		Role: RoleContributor, BaseBranch: "main", VerifyCmd: []string{"true"},
		Prompts: prompts,
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}}); err != nil {
		t.Errorf("BuildApp = %v, want every real slot accepted", err)
	}
}

// The stashed work has to reach the prompt of every step that can stop short,
// for the same reason the answers do: a body that renders neither re-derives
// what the earlier invocation already paid for — and for the plan step, which
// changes no files, that is the entire step.
func TestWorkInProgressReachesEveryResumableDefaultPrompt(t *testing.T) {
	pc := PromptContext{
		Prior:          map[StepID]flow.ArtifactRecord{},
		WorkInProgress: "what I worked out before I stopped",
	}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The two re-prompts are excluded for the same reason as in the answers
	// test: each resumes the session that produced the text, which already has
	// the working-out in context.
	inSessionReprompts := map[PromptID]bool{
		PromptImplementFix: true,
		PromptRevise:       true,
		PromptCommitRepair: true,
		PromptStageRepair:  true,
		PromptPushRepair:   true,
	}
	for id := range defaultPrompts {
		if inSessionReprompts[id] {
			continue
		}
		t.Run("default/"+string(id), func(t *testing.T) {
			got, err := renderPrompt(Config{}, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "what I worked out before I stopped") {
				t.Errorf("default prompt %q does not render the stashed work — a resumed step would re-derive it", id)
			}
		})
	}
	// A project override must also receive the stashed work.
	for id, frags := range requiredFragments {
		if !frags.workInProgress {
			continue
		}
		t.Run("override/"+string(id), func(t *testing.T) {
			cfg := Config{Prompts: map[PromptID]string{id: "project body"}}
			got, err := renderPrompt(cfg, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "what I worked out before I stopped") {
				t.Errorf("override prompt %q does not render the stashed work — a resumed step would re-derive it", id)
			}
		})
	}
}

func TestWorkInProgressBlock(t *testing.T) {
	t.Run("empty on a first run", func(t *testing.T) {
		if got := (PromptContext{}).WorkInProgressBlock(); got != "" {
			t.Errorf("got %q, want empty when nothing was stashed", got)
		}
	})
	t.Run("says the notes are not a result", func(t *testing.T) {
		got := PromptContext{WorkInProgress: "half a plan"}.WorkInProgressBlock()
		if !strings.Contains(got, "half a plan") {
			t.Errorf("block does not carry the notes: %q", got)
		}
		// The framing is load-bearing: an agent that read them as a finished
		// result would defend them instead of finishing the step.
		if !strings.Contains(got, "not a result") {
			t.Errorf("block does not say the notes are scaffolding: %q", got)
		}
	})
}

// The revise prompt has one job, and its reply is recorded verbatim. Anything
// that invites commentary produces an artifact that opens with an explanation
// of what changed.
func TestRevisePromptCarriesTheRefusalAndTheText(t *testing.T) {
	pc := PromptContext{
		Prior:       map[StepID]flow.ArtifactRecord{},
		Refusal:     `disclosure refused (artifact-comment): found "/home/someone/"`,
		RefusedText: "the plan, mentioning /home/someone/",
	}
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got, err := renderPrompt(Config{}, PromptRevise, pc)
	if err != nil {
		t.Fatalf("renderPrompt: %v", err)
	}
	for _, want := range []string{
		`found "/home/someone/"`,              // what the guard caught
		"the plan, mentioning /home/someone/", // what it caught it in
		"FULL revised text",                   // what the reply must be
	} {
		if !strings.Contains(got, want) {
			t.Errorf("revise prompt is missing %q:\n%s", want, got)
		}
	}
}

// The prompts whose product is published on the item have to ask for
// repository-relative paths. An absolute home path is unusable to every reader
// of the issue — nobody shares that filesystem — and it is what the disclosure
// guard refuses on the way out, which then costs a revision round against work
// that was already finished.
func TestPublishedProsePromptsAskForRepositoryRelativePaths(t *testing.T) {
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{}}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, id := range []PromptID{PromptPlan, PromptReview, PromptCoverage} {
		t.Run("default/"+string(id), func(t *testing.T) {
			got, err := renderPrompt(Config{}, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "never by absolute path") {
				t.Errorf("default prompt %q does not ask for repository-relative paths:\n%s", id, got)
			}
		})
	}
	// A project override must also receive the fragment.
	for id, frags := range requiredFragments {
		if !frags.repoRelativePaths {
			continue
		}
		t.Run("override/"+string(id), func(t *testing.T) {
			cfg := Config{Prompts: map[PromptID]string{id: "project body"}}
			got, err := renderPrompt(cfg, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "never by absolute path") {
				t.Errorf("override prompt %q does not ask for repository-relative paths:\n%s", id, got)
			}
		})
	}
}

// An override for a producing prompt must carry the required fragments, even
// though the project body does not mention them.
func TestRenderPrompt_OverrideCarriesRequiredFragments(t *testing.T) {
	pc := PromptContext{
		Prior:          map[StepID]flow.ArtifactRecord{},
		WorkInProgress: "stashed notes",
		Answers:        []Answer{{Answer: "yes", Author: "alice"}},
	}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for id, frags := range requiredFragments {
		t.Run(string(id), func(t *testing.T) {
			cfg := Config{Prompts: map[PromptID]string{id: "minimal project body"}}
			got, err := renderPrompt(cfg, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.HasPrefix(got, "minimal project body") {
				t.Errorf("project body not at the start:\n%s", got)
			}
			if frags.repoRelativePaths && !strings.Contains(got, "never by absolute path") {
				t.Errorf("missing repoRelativePaths:\n%s", got)
			}
			if frags.deferCommit && !strings.Contains(got, "later step") {
				t.Errorf("missing DeferCommit:\n%s", got)
			}
			if frags.narrowGateHint && !strings.Contains(got, "bin/run") {
				t.Errorf("missing narrowGateHint:\n%s", got)
			}
			if frags.workInProgress && !strings.Contains(got, "stashed notes") {
				t.Errorf("missing WorkInProgressBlock:\n%s", got)
			}
			if frags.answers && !strings.Contains(got, "yes") {
				t.Errorf("missing AnswersBlock:\n%s", got)
			}
		})
	}
}

// A default prompt already contains its fragments in the template source;
// renderPrompt must not double-append them.
func TestRenderPrompt_DefaultDoesNotDoubleAppend(t *testing.T) {
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{}}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, id := range []PromptID{PromptPlan, PromptReview, PromptCoverage} {
		t.Run(string(id), func(t *testing.T) {
			got, err := renderPrompt(Config{}, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if n := strings.Count(got, "never by absolute path"); n != 1 {
				t.Errorf("repoRelativePaths appears %d times in default %q, want exactly 1", n, id)
			}
		})
	}
}

// The three producing prompts (implement, review, coverage) must name the
// narrow gate instrument. The plan prompt must not.
func TestProducingPromptsNameNarrowGate(t *testing.T) {
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{}}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, id := range []PromptID{PromptImplement, PromptReview, PromptCoverage} {
		t.Run("default/"+string(id), func(t *testing.T) {
			got, err := renderPrompt(Config{}, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "bin/run") {
				t.Errorf("default prompt %q does not name bin/run:\n%s", id, got)
			}
		})
		t.Run("override/"+string(id), func(t *testing.T) {
			cfg := Config{Prompts: map[PromptID]string{id: "project body"}}
			got, err := renderPrompt(cfg, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if !strings.Contains(got, "bin/run") {
				t.Errorf("override prompt %q does not name bin/run:\n%s", id, got)
			}
		})
	}
	t.Run("plan does not name narrow gate", func(t *testing.T) {
		got, err := renderPrompt(Config{}, PromptPlan, pc)
		if err != nil {
			t.Fatalf("renderPrompt: %v", err)
		}
		if strings.Contains(got, "bin/run") {
			t.Errorf("plan prompt should not name the narrow gate instrument:\n%s", got)
		}
	})
}

// A whitespace-only override is treated as absent: the default is used, and
// no fragments are appended (since the default already carries them).
func TestRenderPrompt_WhitespaceOnlyOverrideFallsBack(t *testing.T) {
	pc := PromptContext{Prior: map[StepID]flow.ArtifactRecord{}}
	pc.VerifyCmd = "make check"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Whitespace-only override → same result as no override.
	cfg := Config{Prompts: map[PromptID]string{PromptPlan: "   \n\t  "}}
	got, err := renderPrompt(cfg, PromptPlan, pc)
	if err != nil {
		t.Fatalf("renderPrompt: %v", err)
	}
	want, err := renderPrompt(Config{}, PromptPlan, pc)
	if err != nil {
		t.Fatalf("renderPrompt(default): %v", err)
	}
	if got != want {
		t.Errorf("whitespace-only override did not fall back to default:\ngot  %q\nwant %q", got, want)
	}
}

// BuildApp must reject an override whose body, once the library appends its
// required fragments, does not parse as a valid template. The body alone may
// parse fine — the validation must include the fragments.
func TestBuildApp_RejectsOverrideThatBreaksWithFragments(t *testing.T) {
	// An unclosed action: "{{" parses on its own as literal text in some
	// template modes, but once fragments append "{{.DeferCommit}}" the
	// combined source has a real action and the unclosed "{{" becomes a parse
	// error. Use a body that is unambiguously broken once fragments land.
	_, err := BuildApp(context.Background(), Config{
		Role: RoleContributor, BaseBranch: "main", VerifyCmd: []string{"true"},
		Prompts: map[PromptID]string{
			PromptImplement: "body with bad template {{ .Nonexistent | bad_func }}",
		},
	}, Deps{Backend: &stubBackend{}, Agent: stubAgent{}})
	if err == nil || !strings.Contains(err.Error(), string(PromptImplement)) {
		t.Errorf("err = %v, want a parse error naming the slot", err)
	}
}

// Every producing default prompt must already contain every fragment its
// requiredFragments entry declares. If a future edit adds a slot to the map
// but forgets to put the fragment in the default body, the default would
// double-append on an override path while silently missing it on the default
// path.
func TestRequiredFragments_DefaultsContainDeclaredFragments(t *testing.T) {
	for id, frags := range requiredFragments {
		src, ok := defaultPrompts[id]
		if !ok {
			t.Errorf("requiredFragments names %q, which has no default prompt", id)
			continue
		}
		t.Run(string(id), func(t *testing.T) {
			if frags.repoRelativePaths && !strings.Contains(src, repoRelativePaths) {
				t.Error("default missing repoRelativePaths fragment")
			}
			if frags.deferCommit && !strings.Contains(src, "{{.DeferCommit}}") {
				t.Error("default missing {{.DeferCommit}}")
			}
			if frags.narrowGateHint && !strings.Contains(src, narrowGateHint) {
				t.Error("default missing narrowGateHint")
			}
			if frags.workInProgress && !strings.Contains(src, "{{.WorkInProgressBlock}}") {
				t.Error("default missing {{.WorkInProgressBlock}}")
			}
			if frags.answers && !strings.Contains(src, "{{.AnswersBlock}}") {
				t.Error("default missing {{.AnswersBlock}}")
			}
		})
	}
}

// In-session re-prompts get no fragments appended, even when overridden.
func TestRenderPrompt_RepromptsGetNoFragments(t *testing.T) {
	pc := PromptContext{
		Prior:          map[StepID]flow.ArtifactRecord{},
		WorkInProgress: "stashed notes",
		Answers:        []Answer{{Answer: "yes"}},
	}
	pc.VerifyCmd = "make check"
	pc.VerifyOutput = "FAIL"
	if err := pc.Context.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, id := range []PromptID{PromptImplementFix, PromptRevise, PromptCommitRepair, PromptStageRepair, PromptPushRepair} {
		t.Run(string(id), func(t *testing.T) {
			cfg := Config{Prompts: map[PromptID]string{id: "reprompt body"}}
			got, err := renderPrompt(cfg, id, pc)
			if err != nil {
				t.Fatalf("renderPrompt: %v", err)
			}
			if got != "reprompt body" {
				t.Errorf("re-prompt %q got unexpected content appended: %q", id, got)
			}
		})
	}
}
