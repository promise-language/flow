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
	// Commit that lands nothing (noCommit) leaves it, as git also does. It also
	// blocks a checkout, as git does.
	dirty []byte
	// gatesRun records which gates were asked for; gateOutcome names what the
	// runner observed of the ones that did not simply measure.
	gatesRun    []flow.GateName
	gateOutcome map[flow.GateName]flow.GateOutcome
	// envelope is what the gate printed on stdout — what the judge is handed
	// and what travels with the verdict.
	envelope []byte
	// judgeErr models a judging layer that cannot answer: no verdict exists,
	// which is never a refusal. judgeRefuses models one that answers "no",
	// which is a perfectly good verdict.
	judgeErr     error
	judgeRefuses bool
	judgeDetail  string
	// calls is every worktree operation in order. Some properties here are
	// about ORDER rather than occurrence — the gate must measure the branch as
	// it will be proposed, which is a statement about when it ran.
	calls []string
}

func newFakeWorktree() *fakeWorktree {
	return &fakeWorktree{exists: map[string]bool{}, head: "base", base: "base"}
}

// testBranch is the claim branch for the item every test here uses.
const testBranch = "flow/issue-42"

// resumedWorktree is what every step after "open branch" finds: the item's
// branch exists and is checked out.
func resumedWorktree() *fakeWorktree {
	wt := newFakeWorktree()
	wt.exists[testBranch] = true
	wt.branch = testBranch
	return wt
}

func (w *fakeWorktree) Branch(_ context.Context, name, base string) (bool, error) {
	w.calls = append(w.calls, "branch:"+name)
	if w.branch == name {
		return false, nil // already there; git does not look at the tree
	}
	// git refuses to switch branches over a dirty tree. Modelling it is what
	// makes a failed checkout the branch step's own failure rather than
	// something the next step is blamed for.
	if len(w.dirty) > 0 {
		return false, errors.New("worktree is dirty; cannot switch branches")
	}
	w.branch = name
	if w.exists[name] {
		return false, nil
	}
	w.exists[name] = true
	w.head = w.base // a fresh branch starts at the base branch's tip
	return true, nil
}
func (w *fakeWorktree) CurrentBranch(context.Context) (string, error) { return w.branch, nil }
func (w *fakeWorktree) Commit(context.Context, string) error {
	w.calls = append(w.calls, "commit")
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
func (w *fakeWorktree) Verify(context.Context) error {
	w.validates++
	if w.validates > w.verifyAfter {
		return nil
	}
	return w.verifyErr
}

// RunGate answers for any declared name. gateOutcome names the ones that do
// not measure, so a test can have the integration gate die while the verify
// command passes — which is the whole point of their being different things.
func (w *fakeWorktree) RunGate(_ context.Context, name flow.GateName) (flow.GateRun, error) {
	if !name.Valid() {
		return flow.GateRun{}, fmt.Errorf("fake: %q is not a declared gate name", name)
	}
	w.calls = append(w.calls, "gate:"+string(name))
	w.gatesRun = append(w.gatesRun, name)
	outcome, ok := w.gateOutcome[name]
	if !ok {
		outcome = flow.OutcomeMeasured
	}
	run := flow.GateRun{Gate: name, Outcome: outcome, ExitCode: -1}
	if outcome == flow.OutcomeMeasured {
		run.Stdout = w.envelope
	} else {
		run.Detail = fmt.Sprintf("the runner observed %s", outcome)
	}
	return run, nil
}

// Judge answers about a measured run and refuses anything else — the project
// holds the thresholds, so nothing here computes one.
func (w *fakeWorktree) Judge(_ context.Context, run flow.GateRun) (flow.GateVerdict, error) {
	w.calls = append(w.calls, "judge:"+string(run.Gate))
	if run.Outcome != flow.OutcomeMeasured {
		return flow.GateVerdict{}, fmt.Errorf("fake: %q measured nothing, so there is nothing to judge", run.Gate)
	}
	if w.judgeErr != nil {
		return flow.GateVerdict{}, w.judgeErr
	}
	return flow.GateVerdict{
		Run:        run,
		Acceptable: !w.judgeRefuses,
		Thresholds: []byte("{}"),
		Detail:     w.judgeDetail,
	}, nil
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
	w.calls = append(w.calls, "open")
	w.opened, w.openBody = true, body
	return "https://example.invalid/pr/1", nil
}

// callIndex reports where an operation appears in the ordered log, or -1.
func (w *fakeWorktree) callIndex(op string) int {
	for i, c := range w.calls {
		if c == op {
			return i
		}
	}
	return -1
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
	// wip is this step's work-in-progress record. wipErr / wipSaveErr model a
	// backend that cannot read or cannot write one — the paths that must cost
	// context and nothing more.
	wip        string
	wipErr     error
	wipSaveErr error
	wipSaves   []string
	// resolveErrs is what ResolveMarkdown returns on successive calls, so a
	// test can script a disclosure refusal followed by an acceptance. A shorter
	// slice than the number of calls means every later call succeeds.
	resolveErrs []error
	resolves    []string
	notices     []string
}

func (c *fakeCtx) Context() context.Context { return context.Background() }
func (c *fakeCtx) Flow() string             { return "resolve" }
func (c *fakeCtx) StepName() string         { return "step" }
func (c *fakeCtx) Result() flow.ArtifactId  { return "implementation" }
func (c *fakeCtx) Item() flow.Item          { return c.item }

// Artifact filters unresolved records, as the real StepCtx does: a seeded but
// unresolved entry is not a value a step may read.
func (c *fakeCtx) Artifact(id flow.ArtifactId) (flow.ArtifactRecord, bool) {
	rec, ok := c.arts[id]
	if !ok || !rec.Resolved {
		return flow.ArtifactRecord{}, false
	}
	return rec, ok
}
func (c *fakeCtx) Flag(flow.ArtifactId) (bool, bool) { return false, false }
func (c *fakeCtx) CommitHash(id flow.ArtifactId) (string, bool) {
	rec, ok := c.Artifact(id)
	if !ok || rec.Type != flow.ArtifactCommitHash {
		return "", false
	}
	return rec.CommitHash, true
}
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
func (c *fakeCtx) ResolveFlag() error {
	c.didResolve, c.resolved = true, flow.ArtifactBody{Type: flow.ArtifactFlag}
	return nil
}
func (c *fakeCtx) ResolveCommitHash(sha string) error {
	c.didResolve, c.resolved = true, flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: sha}
	return nil
}
func (c *fakeCtx) ResolveMarkdown(b string) error {
	c.resolves = append(c.resolves, b)
	if n := len(c.resolves) - 1; n < len(c.resolveErrs) && c.resolveErrs[n] != nil {
		// A refused offer leaves the step re-offerable, as the real StepCtx
		// does: writeResolve marks it resolved only when the write lands.
		return c.resolveErrs[n]
	}
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
func (c *fakeCtx) WorkInProgress() (string, error) { return c.wip, c.wipErr }
func (c *fakeCtx) RecordWorkInProgress(body string) error {
	if c.wipSaveErr != nil {
		return c.wipSaveErr
	}
	c.wipSaves = append(c.wipSaves, body)
	c.wip = body
	return nil
}
func (c *fakeCtx) Notify(_, detail string)          { c.notices = append(c.notices, detail) }
func (c *fakeCtx) Agent() flow.Agent                { return c.agent }
func (c *fakeCtx) Worktree() (flow.Worktree, error) { return c.wt, nil }
func (c *fakeCtx) Claim() flow.Claim                { return flow.Claim{} }
func (c *fakeCtx) VerifyCmd() string                { return "make check" }
func (c *fakeCtx) RefreshItem() error               { return nil }

type scriptedAgent struct {
	replies []string
	calls   int
	prompts []string
	// reqs is every request as it arrived, so a test can assert what a
	// re-prompt was allowed to do — a revision that could edit the tree would
	// put work into the branch after the commit meant to carry it.
	reqs []flow.AgentRequest
	// errs is what Run returns on successive calls, so a test can script a
	// substrate that dies part-way through a step. A shorter slice than the
	// number of calls means every later call succeeds.
	errs []error
}

func (a *scriptedAgent) Name() string { return "scripted" }
func (a *scriptedAgent) Run(_ context.Context, req flow.AgentRequest) (*flow.AgentResponse, error) {
	a.prompts = append(a.prompts, req.Prompt)
	a.reqs = append(a.reqs, req)
	reply := "done"
	if a.calls < len(a.replies) {
		reply = a.replies[a.calls]
	}
	var err error
	if a.calls < len(a.errs) {
		err = a.errs[a.calls]
	}
	a.calls++
	if err != nil {
		return nil, err
	}
	// One session per turn, numbered: a re-prompt that resumes the wrong one is
	// asking a session that never saw the text it is being asked to fix.
	return &flow.AgentResponse{LastText: reply, SessionID: fmt.Sprintf("session-%d", a.calls)}, nil
}

func testBuilder(t *testing.T) *builder {
	t.Helper()
	b := &builder{cfg: Config{VerifyCmd: []string{"make", "check"}}, role: RoleContributor}
	base := "main"
	b.base.Store(&base)
	return b
}

// ctxWithPlan is an item whose upstream mechanical and prose results are in
// place: the plan, and the commit the branch was cut from.
func ctxWithPlan(wt *fakeWorktree, agent flow.Agent) *fakeCtx {
	return &fakeCtx{
		item:  flow.Item{ID: "42", Type: "task", Title: "widget is broken"},
		wt:    wt,
		agent: agent,
		arts: map[flow.ArtifactId]flow.ArtifactRecord{
			"plan":   {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "the plan"},
			"branch": {Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "base"},
		},
	}
}

// ---------------------------------------------------------------------------
// stepOpenBranch.
// ---------------------------------------------------------------------------

// The record is what makes "what is this change relative to" answerable later.
// It is the branch's own HEAD, taken after the checkout.
func TestStepOpenBranch_RecordsTheCommitTheBranchWasCutFrom(t *testing.T) {
	wt := newFakeWorktree()
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepOpenBranch(ctx); err != nil {
		t.Fatalf("stepOpenBranch: %v", err)
	}
	if wt.branch != testBranch {
		t.Errorf("worktree is on %q, want the claim branch %q", wt.branch, testBranch)
	}
	if ctx.resolved.Type != flow.ArtifactCommitHash {
		t.Fatalf("resolved a %v, want a commit hash", ctx.resolved.Type)
	}
	if ctx.resolved.CommitHash != "base" {
		t.Errorf("recorded %q, want the commit the branch sits on", ctx.resolved.CommitHash)
	}
	// Mechanical: no prose, and no agent turn.
	if len(ctx.resolves) != 0 {
		t.Errorf("resolved markdown %v — this step produces a commit, not prose", ctx.resolves)
	}
	if agent, ok := ctx.agent.(*scriptedAgent); ok && agent.calls != 0 {
		t.Errorf("agent ran %d times in a mechanical step", agent.calls)
	}
}

// The failure this step exists to relocate. A dirty tree is a branch that could
// not be opened; before this step existed it surfaced as an implement failure
// and sent a reader to look at the agent.
func TestStepOpenBranch_ADirtyTreeFailsHere(t *testing.T) {
	wt := newFakeWorktree()
	wt.dirty = []byte("diff --git a/left-behind.go b/left-behind.go\n")
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepOpenBranch(ctx)
	if err == nil {
		t.Fatal("want the checkout's failure surfaced by this step")
	}
	// It names the branch it could not open and carries the cause, so nothing
	// about the message sends a reader to an agent that never ran.
	if !strings.Contains(err.Error(), testBranch) || !strings.Contains(err.Error(), "dirty") {
		t.Errorf("err = %v, want it to name the branch and the dirty tree", err)
	}
	if ctx.didResolve {
		t.Error("recorded a branch that was never opened")
	}
	if agent, ok := ctx.agent.(*scriptedAgent); ok && agent.calls != 0 {
		t.Errorf("agent ran %d times before the workspace was prepared", agent.calls)
	}
}

// Resumed on a branch an earlier attempt cut and then died on. Its HEAD is the
// base as it stood at the cut, and that is what must be recorded — reading the
// base branch today would record a commit the branch was never cut from.
func TestStepOpenBranch_ResumedBranchRecordsItsOwnHead(t *testing.T) {
	wt := resumedWorktree()
	wt.head = "cut-from"      // where the earlier attempt cut it
	wt.base = "base-moved-on" // the base branch has advanced since
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepOpenBranch(ctx); err != nil {
		t.Fatalf("stepOpenBranch on a resumed branch: %v", err)
	}
	if ctx.resolved.CommitHash != "cut-from" {
		t.Errorf("recorded %q, want the commit the branch actually sits on", ctx.resolved.CommitHash)
	}
}

// ---------------------------------------------------------------------------
// stepImplement.
// ---------------------------------------------------------------------------

func TestStepImplement_RefusesWhenTheBranchGainedNothing(t *testing.T) {
	// Commit is a no-op with nothing staged, so its nil return proves nothing.
	// Catching it here beats failing three agent turns later at `gh pr create`
	// with "No commits between ...".
	wt := resumedWorktree()
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

// The case an equality against today's BASE BRANCH cannot catch: a branch cut by
// an earlier run, still empty, on a base that has moved since. Its HEAD no
// longer equals the base branch's tip, so the old check read it as work and let
// an empty branch travel to "No commits between ...". Against the RECORDED base
// it is what it is: nothing.
func TestStepImplement_RefusesAnEmptyBranchWhoseBaseHasMovedOn(t *testing.T) {
	wt := resumedWorktree()
	wt.head = "cut-from"      // the branch is still where it was cut
	wt.base = "base-moved-on" // ...but the base branch is not
	wt.noCommit = true        // and the agent changed nothing
	ctx := ctxWithPlan(wt, &scriptedAgent{})
	ctx.arts["branch"] = flow.ArtifactRecord{
		Resolved: true, Type: flow.ArtifactCommitHash, CommitHash: "cut-from"}

	err := testBuilder(t).stepImplement(ctx)
	if err == nil || !strings.Contains(err.Error(), "no commits beyond") {
		t.Fatalf("err = %v, want a refusal — the branch carries nothing", err)
	}
	if ctx.didResolve {
		t.Error("resolved an implementation for an empty branch whose base had moved")
	}
}

func TestStepImplement_AcceptsWorkCommittedByAnEarlierRun(t *testing.T) {
	// A previous invocation that committed and then died before resolving
	// leaves a finished tree. Comparing HEAD before/after this commit would
	// read that as "the agent did nothing" and deadlock the step forever, with
	// the change sitting right there in the branch.
	wt := resumedWorktree()
	wt.head = "sha-1"  // an earlier run already committed
	wt.noCommit = true // nothing left to stage
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement = %v, want the already-committed work accepted", err)
	}
	if !ctx.didResolve {
		t.Error("did not resolve despite the branch carrying work")
	}
	// The record names the commit that is there, which is exactly why it is a
	// commit and not a patch: a patch captured on this path is empty, and an
	// empty record cannot be told from "the step did nothing".
	if ctx.resolved.CommitHash != "sha-1" {
		t.Errorf("recorded %q, want the commit the earlier run left", ctx.resolved.CommitHash)
	}
}

func TestStepImplement_RefusesWithoutAPlan(t *testing.T) {
	// A resolved artifact whose body did not load reads as present. Implementing
	// against a blank plan produces something plausible and reports nothing.
	for name, plan := range map[string]flow.ArtifactRecord{
		"missing":    {},
		"unresolved": {Type: flow.ArtifactMarkdown},
		"empty body": {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ctxWithPlan(resumedWorktree(), &scriptedAgent{})
			ctx.arts["plan"] = plan
			err := testBuilder(t).stepImplement(ctx)
			if err == nil || !strings.Contains(err.Error(), "plan") {
				t.Errorf("err = %v, want a refusal naming the plan", err)
			}
		})
	}
}

// The same shape one artifact over. Without the branch step's record there is
// nothing to compare HEAD against, so an empty branch would read as work.
func TestStepImplement_RefusesWithoutTheBranchRecord(t *testing.T) {
	for name, branch := range map[string]flow.ArtifactRecord{
		"missing":    {},
		"unresolved": {Type: flow.ArtifactCommitHash, CommitHash: "base"},
		"wrong type": {Resolved: true, Type: flow.ArtifactMarkdown, Markdown: "base"},
		"empty body": {Resolved: true, Type: flow.ArtifactCommitHash},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ctxWithPlan(resumedWorktree(), &scriptedAgent{})
			ctx.arts["branch"] = branch
			err := testBuilder(t).stepImplement(ctx)
			if err == nil || !strings.Contains(err.Error(), "branch artifact") {
				t.Errorf("err = %v, want a refusal naming the branch record", err)
			}
			if ctx.didResolve {
				t.Error("implemented without knowing what the branch was cut from")
			}
		})
	}
}

// Implement leaves the tree clean, like every other producing step. A step
// returning over work it could not stage hands the next one changes it did not
// make and will be blamed for.
func TestStepImplement_RefusesWhenWorkSurvivesTheCommit(t *testing.T) {
	wt := resumedWorktree()
	wt.noCommit = true                              // nothing lands...
	wt.dirty = []byte("diff --git a/x.go b/x.go\n") // ...but the tree still carries work

	err := testBuilder(t).stepImplement(ctxWithPlan(wt, &scriptedAgent{}))
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err = %v, want a refusal naming the uncommitted work", err)
	}
}

func TestStepImplement_LoopsOnFailingVerifyAndKeepsOneSession(t *testing.T) {
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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

// The deliverable is the commit on the branch, and the record names it. A patch
// would be a copy: it can be legitimately empty, nothing reads it back, and it
// can disagree with the thing it copies.
func TestStepImplement_RecordsTheCommitItProduced(t *testing.T) {
	wt := resumedWorktree()
	ctx := ctxWithPlan(wt, &scriptedAgent{})
	if err := testBuilder(t).stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement: %v", err)
	}
	if ctx.resolved.Type != flow.ArtifactCommitHash {
		t.Fatalf("resolved a %v, want a commit hash", ctx.resolved.Type)
	}
	if ctx.resolved.CommitHash != wt.head {
		t.Errorf("recorded %q, want HEAD after the commit (%q)", ctx.resolved.CommitHash, wt.head)
	}
	if len(ctx.resolved.Patch.Diff) != 0 {
		t.Errorf("resolved a patch as well: %q", ctx.resolved.Patch.Diff)
	}
	// The commit is this handler's job: the prompts tell the agent not to, and
	// nothing else records the work before the request is opened.
	if wt.commits != 1 || !wt.staged {
		t.Errorf("commits = %d, staged = %v, want the work staged and committed once",
			wt.commits, wt.staged)
	}
}

// ---------------------------------------------------------------------------
// The steps that CONSUME the implementation.
// ---------------------------------------------------------------------------

func TestConsumingStepsRefuseAMissingBranch(t *testing.T) {
	// The worktree directory is shared across items. A missing claim branch
	// means the work is not here; cutting a fresh one off the base and carrying
	// on would have implement work on nothing, review and coverage analyse an
	// empty change, and the request propose one. Cutting a branch is the open
	// branch step's job, and only its.
	b := testBuilder(t)
	for name, step := range map[string]func(flow.StepCtx) error{
		"implement":    b.stepImplement,
		"review":       b.stepReview,
		"coverage":     b.stepCoverage,
		"open request": b.stepOpenPR,
	} {
		t.Run(name, func(t *testing.T) {
			wt := newFakeWorktree()
			ctx := ctxWithPlan(wt, &scriptedAgent{})
			err := step(ctx)
			if err == nil || !strings.Contains(err.Error(), "did not exist") {
				t.Errorf("err = %v, want a refusal that the branch is absent", err)
			}
			if ctx.didResolve {
				t.Error("resolved an artifact describing a tree that has no implementation")
			}
			if wt.commits != 0 {
				t.Errorf("committed %d times onto a branch it had just cut", wt.commits)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The ask contract, through a real step.
// ---------------------------------------------------------------------------

func TestAgentQuestionParksThroughAnyStep(t *testing.T) {
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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

// The canonical steps must stay inside the guaranteed RevParse set — "HEAD"
// and the item's base branch. A backend that cannot resolve arbitrary
// revisions is required to error rather than guess, so a step reaching outside
// that pair is a step that will not run on every backend.
func TestStepsOnlyRevParseTheGuaranteedRevisions(t *testing.T) {
	wt := newFakeWorktree()
	wt.strictRevs = map[string]bool{"HEAD": true, "main": true}
	b := testBuilder(t)
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	// The whole mechanical-and-producing sequence over one worktree, because a
	// step reaching outside the pair does so wherever it runs.
	if err := b.stepOpenBranch(ctx); err != nil {
		t.Fatalf("stepOpenBranch: %v", err)
	}
	if err := b.stepImplement(ctx); err != nil {
		t.Fatalf("stepImplement: %v", err)
	}
	if err := b.stepOpenPR(ctx); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
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
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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
	wt := resumedWorktree()
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

// ---------------------------------------------------------------------------
// The gate the request rests on.
// ---------------------------------------------------------------------------

// An ORDERING property, not an occurrence one. The gate must measure the branch
// exactly as it will be proposed — so after the outstanding work is recorded,
// since recording moves the tree — and before the push, since a branch that
// cannot pass must not leave the machine.
func TestStepOpenPR_MeasuresTheBranchAsItWillBeProposed(t *testing.T) {
	wt := resumedWorktree()
	wt.commits = 1 // implement already committed
	wt.dirty = []byte("diff --git a/cli/app.go b/cli/app.go\n")

	if err := testBuilder(t).stepOpenPR(ctxWithPlan(wt, &scriptedAgent{})); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	// One gate, and it is the one a decision may rest on.
	if len(wt.gatesRun) != 1 || wt.gatesRun[0] != flow.GateIntegration {
		t.Fatalf("gates run = %v, want exactly [%s]", wt.gatesRun, flow.GateIntegration)
	}
	commit, gate, open := wt.callIndex("commit"), wt.callIndex("gate:integration"), wt.callIndex("open")
	if commit < 0 || gate < 0 || open < 0 {
		t.Fatalf("calls = %v, want the recording, the gate and the open all to have happened", wt.calls)
	}
	if !(commit < gate) {
		t.Errorf("calls = %v — the gate measured a tree the recording then changed", wt.calls)
	}
	if !(gate < open) {
		t.Errorf("calls = %v — the request was opened before anything was measured", wt.calls)
	}
}

// Four of the five outcomes mean no measurement exists. None of them is the
// change failing, and a message that read that way would send someone looking
// for a defect that is not there.
//
// Derived from AllGateOutcomes rather than listed, so a sixth outcome cannot be
// added without this test failing.
func TestStepOpenPR_DoesNotProposeWhatWasNeverMeasured(t *testing.T) {
	for _, outcome := range flow.AllGateOutcomes() {
		if outcome == flow.OutcomeMeasured {
			continue
		}
		t.Run(string(outcome), func(t *testing.T) {
			wt := resumedWorktree()
			wt.gateOutcome = map[flow.GateName]flow.GateOutcome{flow.GateIntegration: outcome}
			ctx := ctxWithPlan(wt, &scriptedAgent{})

			err := testBuilder(t).stepOpenPR(ctx)
			if err == nil {
				t.Fatal("opened a request over a gate that measured nothing")
			}
			if !strings.Contains(err.Error(), string(outcome)) {
				t.Errorf("err = %v, want it to name the outcome %q", err, outcome)
			}
			if !strings.Contains(err.Error(), "not the change failing") {
				t.Errorf("err = %v, want it to say plainly that nothing was measured "+
					"about the change", err)
			}
			if wt.opened {
				t.Error("opened a pull request that was never measured")
			}
		})
	}
}

// A judge that cannot answer says nothing about the measurement. Reading that
// as "not acceptable" would refuse a sound change because the project's own
// tooling is broken.
func TestStepOpenPR_ABrokenJudgeIsNotARefusal(t *testing.T) {
	wt := resumedWorktree()
	wt.judgeErr = errors.New("bin/run: no such file or directory")
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepOpenPR(ctx)
	if err == nil {
		t.Fatal("opened a request with no verdict about it")
	}
	if !strings.Contains(err.Error(), "not a refusal") {
		t.Errorf("err = %v, want it to say the missing verdict is not a refusal", err)
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v, want the judge's own failure surfaced", err)
	}
	if wt.opened {
		t.Error("opened a pull request with no verdict about it")
	}
}

// A refusal, by contrast, is a perfectly good verdict — and it is the whole
// reason the gate runs here. The maintainer runs the same gate; proposing
// anyway spends a reviewer's attention on a change that cannot land.
func TestStepOpenPR_DoesNotProposeWhatTheJudgeRefuses(t *testing.T) {
	wt := resumedWorktree()
	wt.judgeRefuses = true
	wt.judgeDetail = "coverage 61.2% is below the floor of 70%"
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepOpenPR(ctx)
	if err == nil {
		t.Fatal("proposed a change the maintainer's own gate will reject")
	}
	if !strings.Contains(err.Error(), wt.judgeDetail) {
		t.Errorf("err = %v, want the judge's reason carried", err)
	}
	if wt.opened {
		t.Error("opened a pull request the gate refused")
	}
}

// What was established travels with the request, so a reader knows it rather
// than taking it on trust — including the envelope, so the verdict can be
// recomputed by whoever was not there.
func TestStepOpenPR_BodyCarriesTheGatesResult(t *testing.T) {
	wt := resumedWorktree()
	wt.envelope = []byte(`{"gate":"integration","tests":{"passed":812}}`)
	wt.judgeDetail = "every measurement is within this project's thresholds"
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepOpenPR(ctx); err != nil {
		t.Fatalf("stepOpenPR: %v", err)
	}
	for _, want := range []string{
		"## Gate",
		string(flow.GateIntegration),
		string(flow.OutcomeMeasured),
		"acceptable",
		wt.judgeDetail,
		`"passed":812`,
	} {
		if !strings.Contains(wt.openBody, want) {
			t.Errorf("body missing %q — a reader has to take the change on trust:\n%s",
				want, wt.openBody)
		}
	}
	// Verify is a tool a producing step uses, not a place in the sequence, and
	// there is no artifact recording that it ran.
	if strings.Contains(wt.openBody, "## Verification") {
		t.Errorf("body still carries a verification section:\n%s", wt.openBody)
	}
}

// ---------------------------------------------------------------------------
// stepCloseBranch.
// ---------------------------------------------------------------------------

func TestStepCloseBranch_ReturnsTheWorktreeToTheBase(t *testing.T) {
	wt := resumedWorktree()
	wt.exists["main"] = true
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	if err := testBuilder(t).stepCloseBranch(ctx); err != nil {
		t.Fatalf("stepCloseBranch: %v", err)
	}
	if wt.branch != "main" {
		t.Errorf("worktree is on %q, want the base branch — the arena owes the next "+
			"item a clean starting point", wt.branch)
	}
	if ctx.resolved.Type != flow.ArtifactFlag {
		t.Errorf("resolved a %v, want the flag — this step restores rather than produces",
			ctx.resolved.Type)
	}
	// The branch is the product: it carries the request, which outlives the
	// resolution that opened it.
	if !wt.exists[testBranch] {
		t.Error("deleted the item's branch — the request lives on it")
	}
}

// Branch CREATES when the name is absent, so without the created check a
// worktree missing the base would silently get a branch of that name pointing
// at this item's tip, and every later item would be cut from the wrong place.
func TestStepCloseBranch_RefusesWhenTheBaseIsNotInTheWorktree(t *testing.T) {
	wt := resumedWorktree() // "main" is not among the branches
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepCloseBranch(ctx)
	if err == nil || !strings.Contains(err.Error(), "main") {
		t.Fatalf("err = %v, want a refusal naming the base branch that is missing", err)
	}
	if ctx.didResolve {
		t.Error("recorded the worktree as restored when it was not")
	}
}

func TestStepCloseBranch_RefusesADirtyWorktree(t *testing.T) {
	// Work nobody has seen. Switching away would hide it; the step stops and
	// leaves it where a person can find it.
	wt := resumedWorktree()
	wt.exists["main"] = true
	wt.dirty = []byte("diff --git a/x b/x\n")
	ctx := ctxWithPlan(wt, &scriptedAgent{})

	err := testBuilder(t).stepCloseBranch(ctx)
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("err = %v, want a refusal naming the dirty tree", err)
	}
	if ctx.didResolve {
		t.Error("recorded the worktree as restored over uncommitted work")
	}
	if wt.branch != testBranch {
		t.Errorf("worktree moved to %q over a dirty tree", wt.branch)
	}
}

// ---------------------------------------------------------------------------
// Work in progress: what a step keeps when it stops without completing.
// ---------------------------------------------------------------------------

// The turn that finds the ambiguity is the turn that produced the reasoning
// behind it. Losing it means paying for the same analysis again to arrive at
// the question that has since been answered.
func TestAgentQuestionKeepsTheWorkThatProducedIt(t *testing.T) {
	wt := resumedWorktree()
	const reply = "I read the store and the cache paths.\n" +
		"NEEDS-ANSWER: cache or new store?\n```\nboth are plausible\n```"
	agent := &scriptedAgent{replies: []string{reply}}
	ctx := ctxWithPlan(wt, agent)

	if err := testBuilder(t).stepReview(ctx); err == nil {
		t.Fatal("want the ask sentinel to stop the step")
	}
	if len(ctx.asked) != 1 {
		t.Fatalf("asked %d questions, want 1 — the park must survive the stash", len(ctx.asked))
	}
	if len(ctx.wipSaves) != 1 || ctx.wipSaves[0] != reply {
		t.Errorf("stashed %q, want the agent's whole final message", ctx.wipSaves)
	}
}

// A stash that failed costs a re-derivation — today's behaviour. Turning it
// into a step failure would lose the park as well as the work.
func TestAgentQuestionParksEvenWhenTheStashFails(t *testing.T) {
	wt := resumedWorktree()
	agent := &scriptedAgent{replies: []string{"NEEDS-ANSWER: cache or new store?"}}
	ctx := ctxWithPlan(wt, agent)
	ctx.wipSaveErr = errors.New("nowhere to write")

	if err := testBuilder(t).stepReview(ctx); err == nil {
		t.Fatal("want the ask sentinel to stop the step")
	}
	if len(ctx.asked) != 1 {
		t.Errorf("asked %d questions, want 1 — a failed stash must not swallow the park", len(ctx.asked))
	}
	var reported bool
	for _, n := range ctx.notices {
		if strings.Contains(n, "could not record work in progress") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the failed stash went unreported; notices = %v", ctx.notices)
	}
}

// The record has to reach the prompt, or the resumed step renders the same
// text against the same context and re-derives everything it already paid for.
func TestPromptContextCarriesTheStashedWork(t *testing.T) {
	ctx := ctxWithPlan(newFakeWorktree(), &scriptedAgent{})
	ctx.wip = "what I worked out last time"

	pc, err := testBuilder(t).promptContext(ctx)
	if err != nil {
		t.Fatalf("promptContext: %v", err)
	}
	if pc.WorkInProgress != "what I worked out last time" {
		t.Errorf("PromptContext.WorkInProgress = %q, want the stashed record", pc.WorkInProgress)
	}
	if !strings.Contains(pc.WorkInProgressBlock(), "what I worked out last time") {
		t.Errorf("WorkInProgressBlock() = %q, want it to carry the notes", pc.WorkInProgressBlock())
	}
}

// Best-effort, like the answers: a read that failed costs the prompt some
// context and must not fail a step the gate already cleared.
func TestPromptContextSurvivesAnUnreadableRecord(t *testing.T) {
	ctx := ctxWithPlan(newFakeWorktree(), &scriptedAgent{})
	ctx.wipErr = errors.New("store is unreachable")

	pc, err := testBuilder(t).promptContext(ctx)
	if err != nil {
		t.Fatalf("promptContext: %v", err)
	}
	if pc.WorkInProgress != "" {
		t.Errorf("WorkInProgress = %q, want empty when the read failed", pc.WorkInProgress)
	}
	if pc.WorkInProgressBlock() != "" {
		t.Error("WorkInProgressBlock() is non-empty with nothing stashed")
	}
}

// ---------------------------------------------------------------------------
// The refused-write door.
// ---------------------------------------------------------------------------

// refusal is a disclosure refusal shaped like the one the guard actually
// returns: the act, and the guard's own answer naming what it found.
func refusal(what string) flow.ErrDisclosureRefused {
	return flow.ErrDisclosureRefused{
		Act:    flow.ActArtifactComment,
		Reason: errors.New(what),
	}
}

// docs/disclosure.md: "A refusal is not a failure of the step. The text is
// revised and re-offered." The revision costs one prompt, in the session the
// agent is already holding — not an invocation, and not a re-derivation.
func TestRefusedProseIsRevisedAndPublished(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"the plan mentioning /home/someone/", "the plan, relative"}}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	ctx.resolveErrs = []error{refusal(`an absolute home path names the machine's user`)}

	if err := testBuilder(t).stepPlan(ctx); err != nil {
		t.Fatalf("stepPlan: %v", err)
	}
	if !ctx.didResolve || ctx.resolved.Markdown != "the plan, relative" {
		t.Errorf("resolved %q, want the REVISED text", ctx.resolved.Markdown)
	}
	if agent.calls != 2 {
		t.Errorf("agent ran %d times, want 2 (the plan, then one revision)", agent.calls)
	}
	if ctx.park != nil {
		t.Errorf("parked (%+v) — a refusal is not a failure of the step", ctx.park)
	}
	// The revision fixes a sentence. It must not be able to edit the tree: the
	// wording is what is wrong, and a producing step has already committed by
	// the time it runs.
	rev := agent.reqs[1]
	if rev.PermissionMode != "plan" {
		t.Errorf("revision PermissionMode = %q, want plan", rev.PermissionMode)
	}
	if rev.ResumeSessionID != "session-1" {
		t.Errorf("revision ResumeSessionID = %q, want the session that wrote the text", rev.ResumeSessionID)
	}
	// Carried in the prompt as well as the session, because ResumeSessionID is
	// documented as best-effort.
	if !strings.Contains(rev.Prompt, "the plan mentioning /home/someone/") {
		t.Errorf("revision prompt does not carry the refused text: %q", rev.Prompt)
	}
	if !strings.Contains(rev.Prompt, "an absolute home path names the machine's user") {
		t.Errorf("revision prompt does not carry the guard's reason: %q", rev.Prompt)
	}
}

// The case from the issue, end to end: an agent that keeps producing the same
// refused detail. It parks rather than failing, and the work survives — which
// is what makes the next run differ from this one instead of being the same
// attempt with a bigger budget.
func TestProseRefusedEveryTimeParksAndKeepsTheWork(t *testing.T) {
	// Stands for the fragment a real refusal quotes back — "what it found and
	// where", which is the disclosure itself.
	const guardAnswer = "an absolute home path was found"
	agent := &scriptedAgent{}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	ctx.resolveErrs = []error{
		refusal(guardAnswer), refusal(guardAnswer),
		refusal(guardAnswer), refusal(guardAnswer),
	}

	err := testBuilder(t).stepPlan(ctx)
	if err == nil {
		t.Fatal("want the step to stop when the guard will not take the text")
	}
	if ctx.park == nil {
		t.Fatal("no park recorded — a refusal must not be reported as a failed step")
	}
	if ctx.park.Kind != flow.ParkBlocked {
		t.Errorf("park kind = %q, want %q", ctx.park.Kind, flow.ParkBlocked)
	}
	if !strings.Contains(ctx.park.Reason, "disclosure guard refused") {
		t.Errorf("park reason = %q, want it to name the disclosure refusal", ctx.park.Reason)
	}
	if strings.Contains(ctx.park.Reason, "\n") {
		t.Errorf("park reason spans lines: %q", ctx.park.Reason)
	}
	// A park is PUBLISHED — Backend.Park posts the whole request as an issue
	// comment, through the same guard. A reason repeating what the guard said
	// carries the fragment the guard just refused, so the park record is
	// refused too, Backend.Park errors, and the item never parks at all.
	if strings.Contains(ctx.park.Reason, guardAnswer) {
		t.Errorf("park reason repeats the guard's answer, which is the one text that cannot be published: %q",
			ctx.park.Reason)
	}
	// What it can say instead: the act, which is the SDK's own vocabulary.
	if !strings.Contains(ctx.park.Reason, string(flow.ActArtifactComment)) {
		t.Errorf("park reason = %q, want it to name the refused act", ctx.park.Reason)
	}
	if ctx.didResolve {
		t.Error("resolved the artifact after every offer was refused")
	}
	// The stash is the point: an issue comment is the one place refused text
	// cannot go, so without this the work is gone — and it is where the guard's
	// answer lives, since the park cannot carry it.
	last := ctx.wipSaves[len(ctx.wipSaves)-1]
	if !strings.Contains(last, guardAnswer) {
		t.Errorf("stashed record does not carry the guard's reason: %q", last)
	}
	if !strings.Contains(last, "done") {
		t.Errorf("stashed record does not carry the refused text: %q", last)
	}
	// Bounded, so a loop against the guard cannot eat the step's whole prompt
	// budget: the opening turn plus maxDisclosureRevisions revisions.
	if agent.calls != maxDisclosureRevisions+1 {
		t.Errorf("agent ran %d times, want %d", agent.calls, maxDisclosureRevisions+1)
	}
}

// Anything that is not a refusal is a real failure of the write. Re-prompting
// over it would ask an agent to revise text that was never examined.
func TestANonRefusalFromResolveIsReturnedUnchanged(t *testing.T) {
	agent := &scriptedAgent{}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	boom := errors.New("github: 502 bad gateway")
	ctx.resolveErrs = []error{boom}

	err := testBuilder(t).stepPlan(ctx)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the write's own error unchanged", err)
	}
	if agent.calls != 1 {
		t.Errorf("agent ran %d times, want 1 — nothing should have been revised", agent.calls)
	}
	if ctx.park != nil {
		t.Errorf("parked (%+v) on an error that is not a refusal", ctx.park)
	}
	if len(ctx.wipSaves) != 0 {
		t.Errorf("stashed %v for a write that was never refused", ctx.wipSaves)
	}
}

// An empty revision is not an empty artifact. Recording one would resolve the
// step with nothing in it — the plan section of a pull request, blank.
func TestAnEmptyRevisionIsAnError(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"the plan", "   "}}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	ctx.resolveErrs = []error{refusal("an absolute home path was found")}

	err := testBuilder(t).stepPlan(ctx)
	if err == nil || !strings.Contains(err.Error(), "revise") {
		t.Fatalf("err = %v, want a refusal to record an empty revision", err)
	}
	if ctx.didResolve {
		t.Error("resolved an artifact from an empty revision")
	}
}

// Every step that publishes prose goes through the one revise path. A step
// that resolved directly would fail on a refusal and lose its work — which is
// the defect, reintroduced one step at a time.
func TestEveryProseStepRevisesARefusal(t *testing.T) {
	for name, step := range map[string]func(*builder, flow.StepCtx) error{
		"plan":     (*builder).stepPlan,
		"review":   (*builder).stepReview,
		"coverage": (*builder).stepCoverage,
	} {
		t.Run(name, func(t *testing.T) {
			wt := resumedWorktree()
			agent := &scriptedAgent{replies: []string{"first draft", "revised draft"}}
			ctx := ctxWithPlan(wt, agent)
			ctx.resolveErrs = []error{refusal("an absolute home path was found")}

			if err := step(testBuilder(t), ctx); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !ctx.didResolve || ctx.resolved.Markdown != "revised draft" {
				t.Errorf("resolved %q, want the revised text", ctx.resolved.Markdown)
			}
			if len(ctx.wipSaves) != 1 {
				t.Errorf("stashed %d times, want 1 — the refused text has nowhere else to go", len(ctx.wipSaves))
			}
		})
	}
}

// A refusal that comes back a second time has to produce a different round:
// the agent is asked to fix the text it just wrote, in the session that wrote
// it. A round that re-sent the ORIGINAL text would ask for a change already
// made, and the loop could only run out its rounds and park.
func TestASecondRefusalRevisesTheRevision(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"draft one", "draft two", "draft three"}}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	ctx.resolveErrs = []error{refusal("first refusal"), refusal("second refusal")}

	if err := testBuilder(t).stepPlan(ctx); err != nil {
		t.Fatalf("stepPlan: %v", err)
	}
	if ctx.resolved.Markdown != "draft three" {
		t.Errorf("resolved %q, want the second revision", ctx.resolved.Markdown)
	}
	if agent.calls != 3 {
		t.Fatalf("agent ran %d times, want 3 (the plan, then two revisions)", agent.calls)
	}
	second := agent.reqs[2]
	if !strings.Contains(second.Prompt, "draft two") {
		t.Errorf("the second revision does not carry the text just refused: %q", second.Prompt)
	}
	if strings.Contains(second.Prompt, "draft one") {
		t.Errorf("the second revision re-sends the text the first one already replaced: %q", second.Prompt)
	}
	if !strings.Contains(second.Prompt, "second refusal") {
		t.Errorf("the second revision carries the first refusal's reason, not this one's: %q", second.Prompt)
	}
	if second.ResumeSessionID != "session-2" {
		t.Errorf("second revision resumes %q, want the session that wrote draft two", second.ResumeSessionID)
	}
	// Each round replaces the stash, so what survives a park is the newest
	// refused text — the one the next run should start from.
	if len(ctx.wipSaves) != 2 {
		t.Fatalf("stashed %d times, want one per refusal", len(ctx.wipSaves))
	}
	if !strings.Contains(ctx.wipSaves[1], "draft two") || !strings.Contains(ctx.wipSaves[1], "second refusal") {
		t.Errorf("the last stash is not the last refusal: %q", ctx.wipSaves[1])
	}
}

// The stash is what makes the work survive, but it is not what makes the
// revision possible — that is the session and the prompt. A store that cannot
// take the text must cost the record and nothing else.
func TestRefusedProseIsRevisedEvenWhenTheStashFails(t *testing.T) {
	agent := &scriptedAgent{replies: []string{"the plan, absolute", "the plan, relative"}}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	ctx.resolveErrs = []error{refusal("an absolute home path was found")}
	ctx.wipSaveErr = errors.New("nowhere to write")

	if err := testBuilder(t).stepPlan(ctx); err != nil {
		t.Fatalf("stepPlan: %v", err)
	}
	if ctx.resolved.Markdown != "the plan, relative" {
		t.Errorf("resolved %q, want the revised text", ctx.resolved.Markdown)
	}
	var reported bool
	for _, n := range ctx.notices {
		if strings.Contains(n, "could not record refused text") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the failed stash went unreported; notices = %v", ctx.notices)
	}
}

// The stash happens before the revision is attempted, so a substrate that dies
// between the refusal and the fix still leaves the next run something to start
// from. Stashing after the revision would lose exactly the text that a run
// stopped here can no longer reach — it was never published.
func TestARevisionThatCannotRunKeepsTheRefusedWork(t *testing.T) {
	boom := errors.New("agent substrate is down")
	agent := &scriptedAgent{replies: []string{"the plan"}, errs: []error{nil, boom}}
	ctx := ctxWithPlan(newFakeWorktree(), agent)
	ctx.resolveErrs = []error{refusal("an absolute home path was found")}

	err := testBuilder(t).stepPlan(ctx)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the agent's own error", err)
	}
	if ctx.didResolve {
		t.Error("resolved an artifact after the revision could not run")
	}
	if len(ctx.wipSaves) != 1 {
		t.Fatalf("stashed %d times, want the refused text kept before the revision ran", len(ctx.wipSaves))
	}
	if !strings.Contains(ctx.wipSaves[0], "the plan") ||
		!strings.Contains(ctx.wipSaves[0], "an absolute home path was found") {
		t.Errorf("the stash carries neither the text nor the reason: %q", ctx.wipSaves[0])
	}
}
