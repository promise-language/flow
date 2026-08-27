package issue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

// The canonical steps carried every severe bug found in review, and a
// source-text grep is not coverage. These drive the real handlers against a
// worktree that can model the states that actually break them: a branch that
// does not exist, a commit that lands nothing, a verify that keeps failing.

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

type fakeWorktree struct {
	branch      string
	exists      map[string]bool // branches that already exist
	head        string          // current commit
	base        string          // the base commit every branch starts from
	commits     int
	noCommit    bool  // Commit lands nothing, as git does with nothing staged
	verifyErr   error // nil once verifyAfter rounds have run
	validates   int
	verifyAfter int
	staged      bool
	revsAsked   []string
	strictRevs  map[string]bool // when set, any other revision errors
	pushed      bool
	opened      bool
	openBody    string
	captured    []byte
	// dirty models work appearing in the tree AFTER a commit — what the review
	// and coverage steps do. A Commit that lands clears it, as git does; a
	// Commit that lands nothing (noCommit) leaves it, as git also does.
	dirty []byte
}

func newFakeWorktree() *fakeWorktree {
	return &fakeWorktree{exists: map[string]bool{}, head: "base", base: "base"}
}

func (w *fakeWorktree) Branch(_ context.Context, name, base string) (bool, error) {
	w.branch = name
	if w.exists[name] {
		return false, nil
	}
	w.exists[name] = true
	w.head = w.base // a fresh branch starts at the base
	return true, nil
}
func (w *fakeWorktree) CurrentBranch(context.Context) (string, error) { return w.branch, nil }
func (w *fakeWorktree) Commit(context.Context, string) error {
	if w.noCommit {
		return nil // git's own behavior with nothing staged
	}
	w.commits++
	w.head = fmt.Sprintf("sha-%d", w.commits)
	w.dirty = nil
	return nil
}
func (w *fakeWorktree) Stage(context.Context) error { w.staged = true; return nil }
func (w *fakeWorktree) Push(context.Context) error  { w.pushed = true; return nil }
func (w *fakeWorktree) Validate(context.Context) error {
	w.validates++
	if w.validates > w.verifyAfter {
		return nil
	}
	return w.verifyErr
}
func (w *fakeWorktree) CapturePatch(context.Context) ([]byte, error) {
	// Models `git diff HEAD`: untracked work is invisible until staged, and
	// nothing is left to see once it has been committed.
	if len(w.dirty) > 0 {
		return w.dirty, nil
	}
	if !w.staged || w.commits > 0 {
		return nil, nil
	}
	return w.captured, nil
}
func (w *fakeWorktree) RevParse(_ context.Context, rev string) (string, error) {
	w.revsAsked = append(w.revsAsked, rev)
	// A backend that cannot resolve a revision must error rather than fall
	// back to HEAD, or a caller comparing a branch against its base gets the
	// same SHA twice and concludes the branch is empty.
	if w.strictRevs != nil && !w.strictRevs[rev] {
		return "", fmt.Errorf("fake: cannot resolve %q", rev)
	}
	if rev == "HEAD" {
		return w.head, nil
	}
	return w.base, nil
}
func (w *fakeWorktree) Request() flow.RequestManager { return w }
func (w *fakeWorktree) Open(_ context.Context, base, title, body string) (string, error) {
	w.opened, w.openBody = true, body
	return "https://example.invalid/pr/1", nil
}
func (w *fakeWorktree) Merge(context.Context, string) error             { return nil }
func (w *fakeWorktree) Mergeable(context.Context, string) (bool, error) { return true, nil }

type fakeCtx struct {
	item       flow.Item
	arts       map[flow.ArtifactId]flow.ArtifactRecord
	wt         *fakeWorktree
	agent      flow.Agent
	park       *flow.ParkRequest
	resolved   flow.ArtifactBody
	didResolve bool
	asked      []flow.AgentQuestion
}

func (c *fakeCtx) Context() context.Context { return context.Background() }
func (c *fakeCtx) Flow() string             { return "resolve" }
func (c *fakeCtx) StepName() string         { return "step" }
func (c *fakeCtx) Result() flow.ArtifactId  { return "implementation" }
func (c *fakeCtx) Item() flow.Item          { return c.item }
func (c *fakeCtx) Artifact(id flow.ArtifactId) (flow.ArtifactRecord, bool) {
	rec, ok := c.arts[id]
	return rec, ok
}
func (c *fakeCtx) Flag(flow.ArtifactId) (bool, bool)         { return false, false }
func (c *fakeCtx) CommitHash(flow.ArtifactId) (string, bool) { return "", false }
func (c *fakeCtx) Markdown(id flow.ArtifactId) (string, bool) {
	rec, ok := c.arts[id]
	if !ok || rec.Type != flow.ArtifactMarkdown {
		return "", false
	}
	return rec.Markdown, true
}
func (c *fakeCtx) JSON(flow.ArtifactId) (json.RawMessage, bool) { return nil, false }
func (c *fakeCtx) File(flow.ArtifactId) (string, []byte, bool)  { return "", nil, false }
func (c *fakeCtx) Patch(flow.ArtifactId) (flow.PatchBody, bool) { return flow.PatchBody{}, false }
func (c *fakeCtx) Signal(flow.SignalId) bool                    { return false }
func (c *fakeCtx) ParkedOn() *flow.ParkRequest                  { return c.park }
func (c *fakeCtx) ResolveFlag() error                           { c.didResolve = true; return nil }
func (c *fakeCtx) ResolveCommitHash(string) error               { c.didResolve = true; return nil }
func (c *fakeCtx) ResolveMarkdown(b string) error {
	c.didResolve, c.resolved = true, flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: b}
	return nil
}
func (c *fakeCtx) ResolveJSON(json.RawMessage) error { c.didResolve = true; return nil }
func (c *fakeCtx) ResolveFile(string, []byte) error  { c.didResolve = true; return nil }
func (c *fakeCtx) ResolvePatch(b flow.PatchBody) error {
	c.didResolve, c.resolved = true, flow.ArtifactBody{Type: flow.ArtifactPatch, Patch: b}
	return nil
}
func (c *fakeCtx) Skip(string) error               { return nil }
func (c *fakeCtx) MarkStale(flow.ArtifactId) error { return nil }
func (c *fakeCtx) Park(req flow.ParkRequest) error {
	c.park = &req
	return errors.New("parked")
}
func (c *fakeCtx) AskQuestions(qs ...flow.AgentQuestion) error {
	c.asked = append(c.asked, qs...)
	return errors.New("asked")
}
func (c *fakeCtx) Notify(string, string)            {}
func (c *fakeCtx) Agent() flow.Agent                { return c.agent }
func (c *fakeCtx) Worktree() (flow.Worktree, error) { return c.wt, nil }
func (c *fakeCtx) Claim() flow.Claim                { return flow.Claim{} }
func (c *fakeCtx) VerifyCmd() string                { return "make check" }
func (c *fakeCtx) RefreshItem() error               { return nil }

type scriptedAgent struct {
	replies []string
	calls   int
	prompts []string
}

func (a *scriptedAgent) Name() string { return "scripted" }
func (a *scriptedAgent) Run(_ context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	a.prompts = append(a.prompts, req.Prompt)
	reply := "done"
	if a.calls < len(a.replies) {
		reply = a.replies[a.calls]
	}
	a.calls++
	return &flow.AgentResponse{LastText: reply, SessionID: "session-1"}, nil
}

func testBuilder(t *testing.T) *builder {
	t.Helper()
	b := &builder{cfg: Config{VerifyCmd: []string{"make", "check"}}, role: RoleContributor}
	base := "main"
	b.base.Store(&base)
	return b
}

func ctxWithPlan(wt *fakeWorktree, agent flow.Agent) *fakeCtx {
	return &fakeCtx{
		item:  flow.Item{ID: "42", Type: "task", Title: "widget is broken"},
		wt:    wt,
		agent: agent,
		arts: map[flow.ArtifactId]flow.ArtifactRecord{
			"plan": {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "the plan"},
		},
	}
}

// ---------------------------------------------------------------------------
// stepImplement.
// ---------------------------------------------------------------------------

func TestStepImplement_RefusesWhenTheBranchGainedNothing(t *testing.T) {
	// Commit is a no-op with nothing staged, so its nil return proves nothing.
	// Catching it here beats failing three agent turns later at `gh pr create`
	// with "No commits between ...".
	wt := newFakeWorktree()
	wt.noCommit = true
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepImplement(ctx)
	if err == nil || !strings.Contains(err.Error(), "no commits beyond") {
		t.Fatalf("err = %v, want a refusal naming the empty branch", err)
	}
	if ctx.didResolve {
		t.Error("resolved an implementation artifact for a branch with no commits")
	}
}

func TestStepImplement_AcceptsWorkCommittedByAnEarlierRun(t *testing.T) {
	// A previous invocation that committed and then died before resolving
	// leaves a finished tree. Comparing HEAD before/after this commit would
	// read that as "the agent did nothing" and deadlock the step forever, with
	// the change sitting right there in the branch.
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true // resumed, not created
	wt.head = "sha-1"                 // an earlier run already committed
	wt.noCommit = true                // nothing left to stage
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement = %v, want the already-committed work accepted", err)
	}
	if !ctx.didResolve {
		t.Error("did not resolve despite the branch carrying work")
	}
	// An empty diff is correct here and not an oversight: the tree is clean
	// because an earlier run committed the work, so there is nothing left to
	// capture. The branch-vs-base check is what established the work exists.
	if len(ctx.resolved.Patch.Diff) != 0 {
		t.Errorf("Diff = %q, want empty on a tree whose work is already committed",
			ctx.resolved.Patch.Diff)
	}
}

func TestStepImplement_RefusesWithoutAPlan(t *testing.T) {
	// A resolved artifact whose body did not load reads as present. Implementing
	// against a blank plan produces something plausible and reports nothing.
	for name, arts := range map[string]map[flow.ArtifactId]flow.ArtifactRecord{
		"missing":    {},
		"unresolved": {"plan": {Type: flow.ArtifactMarkdown}},
		"empty body": {"plan": {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "  "}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ctxWithPlan(newFakeWorktree(), &scriptedAgent{})
			ctx.arts = arts
			err := testBuilder(t).stepImplement(ctx)
			if err == nil || !strings.Contains(err.Error(), "plan") {
				t.Errorf("err = %v, want a refusal naming the plan", err)
			}
		})
	}
}

func TestStepImplement_LoopsOnFailingVerifyAndKeepsOneSession(t *testing.T) {
	wt := newFakeWorktree()
	wt.verifyErr = errors.New("verify failed:\nFAIL pkg/x")
	wt.verifyAfter = 2 // fails twice, then passes
	agent := &scriptedAgent{}
	ctx := ctxWithPlan(wt, agent)

	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement = %v", err)
	}
	if agent.calls != 3 {
		t.Errorf("agent ran %d times, want 3 (initial + 2 fix rounds)", agent.calls)
	}
	// The fix rounds must carry the failing output, or the agent is asked to
	// fix something it cannot see.
	if !strings.Contains(agent.prompts[1], "FAIL pkg/x") {
		t.Errorf("fix prompt = %q, want the failing verify tail", agent.prompts[1])
	}
}

func TestStepImplement_StopsAfterMaxFixRounds(t *testing.T) {
	// An error, not a park: no preflight refuses a blocked park, so an
	// unattended run would re-enter the loop and re-spend the whole round
	// budget every cycle.
	wt := newFakeWorktree()
	wt.verifyErr = errors.New("verify failed:\nstill broken")
	wt.verifyAfter = 99
	b := testBuilder(t)
	b.cfg.MaxFixRounds = 2
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := b.stepImplement(ctx)
	if err == nil || !strings.Contains(err.Error(), "still failing after 2 fix attempts") {
		t.Fatalf("err = %v, want a bounded failure", err)
	}
	// MaxFixRounds counts FIX attempts, so N buys the opening turn plus N
	// re-prompts. Counting the opening turn as a round would silently give a
	// project one fewer attempt than it asked for.
	if agent, ok := ctx.agent.(*scriptedAgent); ok && agent.calls != 3 {
		t.Errorf("agent ran %d times, want 3 (opening turn + 2 fix attempts)", agent.calls)
	}
	if ctx.park != nil {
		t.Error("parked instead of failing — a park nothing gates just re-runs the loop")
	}
}

func TestStepImplement_RecordsTheCommitOnThePatch(t *testing.T) {
	wt := newFakeWorktree()
	ctx := ctxWithPlan(wt, &scriptedAgent{})
	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement: %v", err)
	}
	// BaseSHA must name the commit the diff applies AGAINST, not the one that
	// contains it: `checkout BaseSHA && apply` has to work.
	if ctx.resolved.Patch.BaseSHA != "base" {
		t.Errorf("BaseSHA = %q, want the pre-commit HEAD the diff was taken against",
			ctx.resolved.Patch.BaseSHA)
	}
	if ctx.resolved.Patch.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", ctx.resolved.Patch.BaseBranch)
	}
}

// ---------------------------------------------------------------------------
// The steps that CONSUME the implementation.
// ---------------------------------------------------------------------------

func TestConsumingStepsRefuseAMissingBranch(t *testing.T) {
	// The worktree directory is shared across items. A missing claim branch
	// means the work is not here; cutting a fresh one off the base and carrying
	// on would have review and coverage analyse an empty change, and
	// verify-impl resolve a "verify passed" artifact that proves nothing.
	b := testBuilder(t)
	for name, step := range map[string]func(flow.StepCtx) error{
		"review":      b.stepReview,
		"coverage":    b.stepCoverage,
		"verify-impl": b.stepVerifyImpl,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ctxWithPlan(newFakeWorktree(), &scriptedAgent{})
			err := step(ctx)
			if err == nil || !strings.Contains(err.Error(), "did not exist") {
				t.Errorf("err = %v, want a refusal that the branch is absent", err)
			}
			if ctx.didResolve {
				t.Error("resolved an artifact describing a tree that has no implementation")
			}
		})
	}
}

func TestStepVerifyImpl_FailsRatherThanRecordingAPassingArtifact(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	wt.verifyErr = errors.New("verify failed:\nboom")
	wt.verifyAfter = 99
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepVerifyImpl(ctx)
	if err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("err = %v, want the failing verify surfaced", err)
	}
	if ctx.didResolve {
		t.Error("recorded a verification artifact for a tree that does not verify")
	}
}

// ---------------------------------------------------------------------------
// The ask contract, through a real step.
// ---------------------------------------------------------------------------

func TestAgentQuestionParksThroughAnyStep(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	agent := &scriptedAgent{replies: []string{
		"I cannot decide this.\nNEEDS-ANSWER: cache or new store?\n```\nboth are plausible\n```",
	}}
	ctx := ctxWithPlan(wt, agent)

	err := testBuilder(t).stepReview(ctx)
	if err == nil {
		t.Fatal("want the ask sentinel to stop the step")
	}
	if len(ctx.asked) != 1 {
		t.Fatalf("asked %d questions, want 1", len(ctx.asked))
	}
	// The question is the scannable header; the evidence is the body.
	if ctx.asked[0].Header != "cache or new store?" {
		t.Errorf("Header = %q, want the question", ctx.asked[0].Header)
	}
	if !strings.Contains(ctx.asked[0].Text, "both are plausible") {
		t.Errorf("Text = %q, want the evidence block", ctx.asked[0].Text)
	}
	if ctx.didResolve {
		t.Error("resolved an artifact despite needing a human decision")
	}
}

// ---------------------------------------------------------------------------
// The pull request.
// ---------------------------------------------------------------------------

func TestStepOpenPR_RefusesAPullRequestWithNoPlanInIt(t *testing.T) {
	// A resolved plan reading as empty means its body did not load. Opening a
	// PR that says only "Closes #N" would present a silent read failure as a
	// finished change.
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	ctx := ctxWithPlan(wt, &scriptedAgent{})
	ctx.arts["plan"] = flow.ArtifactRecord{Resolved: true, Type: flow.ArtifactMarkdown}

	err := testBuilder(t).stepOpenPR(ctx)
	if err == nil || !strings.Contains(err.Error(), "no plan") {
		t.Fatalf("err = %v, want a refusal to open an empty pull request", err)
	}
	if wt.opened {
		t.Error("opened a pull request with no plan in its body")
	}
}

func TestStepOpenPR_BodyClosesTheIssue(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepOpenPR(ctx); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	if !strings.Contains(wt.openBody, "Closes #42") {
		t.Errorf("body = %q, want GitHub's closing syntax — the backend has no "+
			"Finalizer, so this is the only thing that closes the issue", wt.openBody)
	}
	if !strings.Contains(wt.openBody, "the plan") {
		t.Errorf("body = %q, want the plan section", wt.openBody)
	}
	// Push is the RequestManager's job, and it guards the branch first;
	// pushing here would defeat that guard.
	if wt.pushed {
		t.Error("stepOpenPR pushed directly, bypassing the branch check in Open")
	}
}

// The question travels with the replies so the agent can judge whether any of
// them actually answers it — nothing correlates a comment to a question, so a
// "+1" reaches the prompt too. The orchestrator's "question: " prefix is
// presentation and must not reach the agent as part of the question.
func TestAnswersCarryTheQuestionWithoutItsPrefix(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	ctx := ctxWithPlan(wt, &scriptedAgent{})
	ctx.park = &flow.ParkRequest{
		Kind:    flow.ParkQuestion,
		Reason:  "question: cache or new store?",
		Details: flow.MarkQuestionAsked(time.Now().Add(-time.Hour)),
	}

	b := testBuilder(t)
	b.backend = &answeringBackend{answers: []flow.Answer{{Answer: "the cache", Author: "alice"}}}

	got := b.answersFor(ctx)
	if len(got) != 1 {
		t.Fatalf("got %d answers, want 1", len(got))
	}
	if got[0].Text != "cache or new store?" {
		t.Errorf("Text = %q, want the bare question", got[0].Text)
	}
}

// A step that is not parked on a question must not go looking for answers.
func TestAnswersSkippedWhenNotParkedOnAQuestion(t *testing.T) {
	ctx := ctxWithPlan(newFakeWorktree(), &scriptedAgent{})
	b := testBuilder(t)
	be := &answeringBackend{answers: []flow.Answer{{Answer: "irrelevant"}}}
	b.backend = be

	if got := b.answersFor(ctx); got != nil {
		t.Errorf("got %v on an unparked item, want none", got)
	}
	ctx.park = &flow.ParkRequest{Kind: flow.ParkBudgetExhausted}
	if got := b.answersFor(ctx); got != nil {
		t.Errorf("got %v on a budget park, want none", got)
	}
	if be.calls != 0 {
		t.Errorf("read answers %d times without a question park, want 0", be.calls)
	}
}

type answeringBackend struct {
	flow.Backend
	answers []flow.Answer
	calls   int
}

func (b *answeringBackend) ReadAnswers(context.Context, flow.Item, time.Time, string) ([]flow.Answer, error) {
	b.calls++
	return b.answers, nil
}

// The patch artifact must carry the actual diff. CapturePatch is a diff against
// HEAD, so it has to run after staging (untracked files are invisible before
// it) and before committing (nothing is left to see after). Getting the order
// wrong attaches either an incomplete diff or an empty one, and both look fine.
func TestStepImplement_AttachesACompletePatch(t *testing.T) {
	wt := newFakeWorktree()
	wt.captured = []byte("diff --git a/new.go b/new.go\n+package new\n")
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement: %v", err)
	}
	if !wt.staged {
		t.Error("did not stage before capturing — a diff against HEAD cannot see untracked files")
	}
	if len(ctx.resolved.Patch.Diff) == 0 {
		t.Error("patch artifact carries no diff")
	}
	if !strings.Contains(string(ctx.resolved.Patch.Diff), "new.go") {
		t.Errorf("diff = %q, want the added file", ctx.resolved.Patch.Diff)
	}
}

// The canonical steps must stay inside the guaranteed RevParse set — "HEAD"
// and the item's base branch. A backend that cannot resolve arbitrary
// revisions is required to error rather than guess, so a step reaching outside
// that pair is a step that will not run on every backend.
func TestStepsOnlyRevParseTheGuaranteedRevisions(t *testing.T) {
	wt := newFakeWorktree()
	wt.strictRevs = map[string]bool{"HEAD": true, "main": true}
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement: %v", err)
	}
	for _, rev := range wt.revsAsked {
		if !wt.strictRevs[rev] {
			t.Errorf("resolved %q, outside the guaranteed {HEAD, base} set", rev)
		}
	}
	if len(wt.revsAsked) == 0 {
		t.Error("no revisions resolved — the empty-branch guard did not run")
	}
}

// ---------------------------------------------------------------------------
// Recording what the checking steps produced (#19) and surfacing it (#26).
// ---------------------------------------------------------------------------

// Review and coverage may edit — that is the design — but implement is the only
// other step that commits. Without recording their work at the proposal, the
// request describes a branch that does not contain it and nothing says so.
// Observed on three consecutive real runs before this existed.
func TestStepOpenPR_RecordsWhatTheCheckingStepsChanged(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	wt.commits = 1 // implement already committed
	wt.dirty = []byte("diff --git a/cli/app.go b/cli/app.go\n")

	before := wt.commits
	if err := testBuilder(t).stepOpenPR(ctxWithPlan(wt, &scriptedAgent{})); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	if wt.commits != before+1 {
		t.Errorf("commits = %d, want %d — the checking steps' work was not recorded, "+
			"so the pull request describes a branch missing it", wt.commits, before+1)
	}
	if !wt.opened {
		t.Error("no pull request opened")
	}
}

// The corollary: a clean tree costs nothing. Committing per step regardless
// would record steps that changed nothing.
func TestStepOpenPR_CleanTreeRecordsNothing(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	wt.commits = 1
	wt.noCommit = true // git lands nothing with nothing staged

	if err := testBuilder(t).stepOpenPR(ctxWithPlan(wt, &scriptedAgent{})); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	if wt.commits != 1 {
		t.Errorf("commits = %d, want 1 — an empty commit was recorded", wt.commits)
	}
}

// The guard, one layer below the commit: if work survives recording, opening
// the request would propose a branch that does not carry it. Refuse instead —
// silently proposing an incomplete change is the failure being prevented.
func TestStepOpenPR_RefusesWhenWorkSurvivesRecording(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	wt.commits = 1
	wt.noCommit = true                        // nothing lands...
	wt.dirty = []byte("diff --git a/x b/x\n") // ...but the tree still carries work

	err := testBuilder(t).stepOpenPR(ctxWithPlan(wt, &scriptedAgent{}))
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err = %v, want a refusal naming the uncommitted work", err)
	}
	if wt.opened {
		t.Error("opened a pull request over a tree still carrying work")
	}
}

// The other half of the same defect: a checking step's product reaching no
// reader is budget spent on nothing, and worse when it describes changes that
// ARE in the diff — the reader sees unexplained work and must reconstruct why.
func TestStepOpenPR_BodyCarriesTheCheckingStepsBriefings(t *testing.T) {
	wt := newFakeWorktree()
	wt.exists["flow/issue-42"] = true
	ctx := ctxWithPlan(wt, &scriptedAgent{})
	ctx.arts["review"] = flow.ArtifactRecord{
		Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "routed grant through usageError"}
	ctx.arts["coverage"] = flow.ArtifactRecord{
		Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "added the arity cases"}

	if err := testBuilder(t).stepOpenPR(ctx); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	for _, want := range []string{"## Review", "routed grant through usageError", "## Coverage", "added the arity cases"} {
		if !strings.Contains(wt.openBody, want) {
			t.Errorf("body missing %q — it reaches only the state comment, where no reviewer looks:\n%s",
				want, wt.openBody)
		}
	}
}
