package issue

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/promise-language/flow"
)

// builder carries what every canonical handler needs. Handlers are methods on
// it so the step set can be assembled without threading five values through
// each closure.
type builder struct {
	cfg     Config
	role    Role
	backend flow.Backend
	// base is resolved lazily, on the first step that needs it — see
	// baseBranch. Resolving it in BuildApp would put a network call between
	// the operator and `doctor`, the command that exists to diagnose exactly
	// the failures that call can hit.
	base atomic.Pointer[string]
	// self is the backend principal, resolved lazily for the same reason.
	self atomic.Pointer[string]
}

// ---------------------------------------------------------------------------
// Contributor step set.
// ---------------------------------------------------------------------------

// stepPlan drafts the implementation plan.
func (b *builder) stepPlan(ctx flow.StepCtx) error {
	pc, err := b.promptContext(ctx)
	if err != nil {
		return err
	}
	body, err := renderPrompt(b.cfg, PromptPlan, pc)
	if err != nil {
		return err
	}
	resp, err := b.runAgent(ctx, flow.AgentRequest{
		Prompt: body,
		// Planning must not edit the tree: the plan is the artifact, and a
		// planner that started implementing would resolve `plan` describing
		// work it had already half-done.
		PermissionMode: "plan",
		Effort:         "high",
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp.LastText) == "" {
		return fmt.Errorf("agent returned an empty plan")
	}
	return b.resolveMarkdown(ctx, pc, resp.SessionID, resp.LastText)
}

// stepImplement makes the change and drives it to a passing gate.
//
// This is a loop, not a single agent turn, and that is the point: an agent's
// first attempt is a draft, and the gate's failing output is the most valuable
// prompt anyone can hand it. The loop ends when the gate goes green, when the
// project's own bound is hit, or when the prompt budget parks the step — which
// surfaces to the operator as a budget park naming every exhausted axis, not as
// a silent give-up.
//
// The artifact resolves ONLY on a green gate. A patch attached after a failing
// verify is unverified work that a resume would apply on top of a broken tree.
func (b *builder) stepImplement(ctx flow.StepCtx) error {
	pc, err := b.promptContext(ctx)
	if err != nil {
		return err
	}
	// Emptiness, not just absence: a resolved artifact whose body did not load
	// reads as present, and implementing against a blank plan is worse than
	// refusing — the agent writes something plausible and nothing reports why.
	if plan, ok := pc.PriorMarkdown(StepPlan); !ok || strings.TrimSpace(plan) == "" {
		return fmt.Errorf("plan artifact missing, unresolved, or empty — refusing to implement without a plan")
	}

	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	base, _, err := b.ensureBranch(ctx, wt)
	if err != nil {
		return err
	}

	prompt, err := renderPrompt(b.cfg, PromptImplement, pc)
	if err != nil {
		return err
	}

	rounds := b.cfg.MaxFixRounds
	if rounds <= 0 {
		rounds = DefaultMaxFixRounds
	}
	// session chains the fix rounds into ONE agent conversation. Without it
	// every round is a fresh process whose entire prompt is a verify tail —
	// no item, no plan, no memory of the edits it just made — so rounds 2..N
	// would be trying to fix code they cannot see the reasoning behind.
	var session string
	// attempt counts FIX rounds: the opening turn is attempt 0, so
	// MaxFixRounds=N buys N re-prompts, which is what the name says.
	for attempt := 0; ; attempt++ {
		ctx.Notify("", fmt.Sprintf("implement round %d", attempt+1))
		resp, err := b.runAgent(ctx, flow.AgentRequest{
			Prompt:          prompt,
			PermissionMode:  "acceptEdits",
			ResumeSessionID: session,
		})
		if err != nil {
			return err
		}
		session = resp.SessionID

		verr := wt.Verify(ctx.Context())
		if verr == nil {
			break
		}
		if attempt >= rounds {
			// An error, not a park. A park here gated nothing — no preflight
			// refuses a ParkBlocked item — so an unattended `resolve` would
			// re-enter this loop and spend the whole round budget again on
			// every cycle. Failing stops the run with the reason attached; the
			// work stays in the worktree either way, so a human who fixes the
			// blocker or raises MaxFixRounds resumes from where it stopped.
			return fmt.Errorf("verify still failing after %d fix attempts: %s",
				attempt, verifyTail(verr))
		}
		// Re-prompt with the failing tail. Rebuilt each round so the context
		// carries THIS round's output, not the first round's.
		pc.VerifyOutput = verifyTail(verr)
		prompt, err = renderPrompt(b.cfg, PromptImplementFix, pc)
		if err != nil {
			return err
		}
	}

	// The commit the diff is taken against. Captured before the commit,
	// because that is what the diff is relative to: pointing BaseSHA at the
	// commit that CONTAINS the hunks would make `checkout BaseSHA && apply`
	// fail, since they are already there.
	baseSHA, err := wt.RevParse(ctx.Context(), "HEAD")
	if err != nil {
		return err
	}

	// Stage, capture, then commit — in that order, and the order is the whole
	// point. CapturePatch is a diff against HEAD: before staging it cannot see
	// untracked files, so it would attach a diff missing every file the change
	// ADDED while looking complete; after committing it sees a clean tree and
	// returns nothing at all. Between the two it sees everything.
	if err := wt.Stage(ctx.Context()); err != nil {
		return err
	}
	patch, err := wt.CapturePatch(ctx.Context())
	if err != nil {
		return err
	}
	// The prompts tell the agent NOT to commit — committing mid-loop would bury
	// a half-finished round in history — which makes it this handler's job.
	// Nothing else in the flow commits, so without this the branch pushed at PR
	// time carries no commits and `gh pr create` fails with "No commits
	// between ...".
	if err := wt.Commit(ctx.Context(), b.commitMessage(ctx)); err != nil {
		return err
	}
	// Commit is a deliberate no-op when nothing is staged, so a nil return is
	// not evidence that anything was recorded. Ask the question that actually
	// matters instead: does this branch carry anything the base does not?
	//
	// Comparing HEAD before and after the commit would only cover the
	// invocation that made it — a resumed branch whose work was committed by an
	// earlier run that then died would read as "the agent did nothing" and
	// deadlock the step with the change sitting right there. Comparing against
	// the base covers both, and catches an empty branch here rather than three
	// agent turns later at "No commits between ...".
	//
	// One case slips through: a branch cut by an EARLIER run, still empty, on a
	// base that has advanced since. Its HEAD is the old base, which no longer
	// equals the current one, so this reads as work. Closing that needs an
	// ahead-count (`rev-list --count base..HEAD`) rather than an equality, and
	// the Worktree surface has no such call. The failure is the pre-existing
	// one — `gh pr create` refusing an empty branch — not a wrong result.
	head, err := wt.RevParse(ctx.Context(), "HEAD")
	if err != nil {
		return err
	}
	baseBranchSHA, err := wt.RevParse(ctx.Context(), base)
	if err != nil {
		return err
	}
	if head == baseBranchSHA {
		return fmt.Errorf("branch %q carries no commits beyond %q — the agent changed nothing, "+
			"so there is nothing to open a pull request from", b.branchName(ctx), base)
	}

	// An empty diff here is legal and expected on one path: a resumed branch
	// whose work an earlier run already committed has a clean tree, so there is
	// nothing left to capture. The branch-vs-base check above has already
	// established the work exists; the artifact records that it lives in the
	// branch rather than in these bytes.
	return ctx.ResolvePatch(flow.PatchBody{
		Diff:       patch,
		BaseSHA:    baseSHA,
		BaseBranch: base,
	})
}

// recordStepWork commits whatever the calling step changed and refuses to
// return with the worktree still dirty.
//
// Every step leaves the tree clean, which makes each step boundary a state the
// resolution can be resumed from: a run that dies loses at most the step that
// was running, never everything since implement. It also attributes the work —
// history shows which step made which change, rather than one commit at the
// end carrying whatever accumulated.
//
// Commit is a deliberate no-op when nothing is staged, so a step that changed
// nothing records nothing and costs one call.
func (b *builder) recordStepWork(ctx flow.StepCtx, wt flow.Worktree, label string) error {
	if err := wt.Stage(ctx.Context()); err != nil {
		return err
	}
	if err := wt.Commit(ctx.Context(), fmt.Sprintf("%s: %s", label, b.itemLabel(ctx))); err != nil {
		return err
	}
	// Not redundant with the commit: staging cannot pick up what a backend
	// refuses to stage, and a step returning over a dirty tree hands the next
	// one work it did not make and will be blamed for.
	patch, err := wt.CapturePatch(ctx.Context())
	if err != nil {
		return err
	}
	if len(patch) > 0 {
		return fmt.Errorf(
			"%s left %d bytes of uncommitted work in the worktree; every step must "+
				"leave it clean, so the next step starts from a known state",
			label, len(patch))
	}
	return nil
}

// itemLabel names the item in a commit subject, without the closes-reference:
// only the implement commit carries that, or every step would read as its own
// resolution of the item.
func (b *builder) itemLabel(ctx flow.StepCtx) string {
	item := ctx.Item()
	if item.Title == "" {
		return "#" + item.ID
	}
	return item.Title
}

// producingMarkdownStep runs a producing step whose artifact is prose: the
// agent works, its changes are recorded, and only then does the artifact
// resolve. That order is deliberate — resolving first would mark the step done
// with its work still uncommitted, which is the state that loses it.
func (b *builder) producingMarkdownStep(ctx flow.StepCtx, id PromptID, label string) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	pc, err := b.promptContext(ctx)
	if err != nil {
		return err
	}
	body, err := renderPrompt(b.cfg, id, pc)
	if err != nil {
		return err
	}
	resp, err := b.runAgent(ctx, flow.AgentRequest{Prompt: body})
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp.LastText) == "" {
		return fmt.Errorf("agent returned nothing for %q", id)
	}
	if err := b.recordStepWork(ctx, wt, label); err != nil {
		return err
	}
	return b.resolveMarkdown(ctx, pc, resp.SessionID, resp.LastText)
}

// stepReview asks for a critique of the change.
func (b *builder) stepReview(ctx flow.StepCtx) error {
	if err := b.onClaimBranch(ctx); err != nil {
		return err
	}
	return b.producingMarkdownStep(ctx, PromptReview, "review")
}

// stepCoverage analyses test coverage of the change.
func (b *builder) stepCoverage(ctx flow.StepCtx) error {
	if err := b.onClaimBranch(ctx); err != nil {
		return err
	}
	return b.producingMarkdownStep(ctx, PromptCoverage, "coverage")
}

// stepVerifyImpl records the passing verification for the pull request.
//
// The gate is re-run here rather than trusted from the implement step: review
// and coverage may have changed the tree in between, and this artifact is the
// evidence a reviewer reads.
func (b *builder) stepVerifyImpl(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	if err := b.onClaimBranch(ctx); err != nil {
		return err
	}
	if verr := wt.Verify(ctx.Context()); verr != nil {
		// Same reasoning as the implement loop: a ParkBlocked nothing refuses
		// would just be re-dispatched next cycle.
		return fmt.Errorf("verify failed on the branch as it now stands: %s", verifyTail(verr))
	}
	pc, err := b.promptContext(ctx)
	if err != nil {
		return err
	}
	body, err := renderPrompt(b.cfg, PromptVerifyImpl, pc)
	if err != nil {
		return err
	}
	resp, err := b.runAgent(ctx, flow.AgentRequest{Prompt: body})
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp.LastText) == "" {
		// The gate passing is the fact worth recording; the summary is a
		// convenience. Don't fail the step over prose.
		return b.resolveMarkdown(ctx, pc, resp.SessionID,
			fmt.Sprintf("verify passed (%s)", strings.Join(b.cfg.VerifyCmd, " ")))
	}
	return b.resolveMarkdown(ctx, pc, resp.SessionID, resp.LastText)
}

// stepOpenPR opens the pull request. The pr-open signal is set by the backend
// as a side effect of Open succeeding, not by this handler.
func (b *builder) stepOpenPR(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	item := ctx.Item()
	title := item.Title
	if title == "" {
		title = fmt.Sprintf("Resolve #%s", item.ID)
	}
	// Check the branch back out, like every other consuming step. Relying on
	// Open's guard instead would turn a worktree left on another item's branch
	// into a deterministic failure on every retry, rather than something the
	// step simply corrects.
	if err := b.onClaimBranch(ctx); err != nil {
		return err
	}
	base, err := b.baseBranch(ctx.Context())
	if err != nil {
		return err
	}
	// flow.Open, not wt.Request() directly: a plain `rq == nil` misses the
	// typed-nil interface a backend can hand back, and would panic on the call
	// instead of reporting that the backend has no pull-request surface.
	//
	// It also pushes, and only AFTER checking the worktree is on the claim
	// branch. Pushing here first would defeat that guard: a tree left on the
	// default branch — or on another item's branch — would be force-tracked to
	// origin before anything noticed it was the wrong one.
	// Record anything the steps AFTER implement produced, before the push.
	//
	// Review and coverage may edit — deliberately, since a reviewer that can
	// only report a fault costs an extra turn to fix one it could have fixed
	// in place. But implement is the only other step that commits, so without
	// this their work is pushed nowhere: the request would describe a branch
	// that does not contain it, and nothing would say so. The change is simply
	// absent, and only `git status` in the arena afterwards shows it.
	//
	// Its own commit rather than amending implement's: the history should read
	// "implement" then "what review changed", because that is what happened.
	if err := b.recordOutstanding(ctx, wt); err != nil {
		return err
	}

	body, err := b.pullRequestBody(ctx)
	if err != nil {
		return err
	}
	_, err = flow.Open(ctx.Context(), wt, base, title, body)
	return err
}

// recordOutstanding commits whatever the post-implement steps left in the tree,
// and refuses to continue if anything survives that.
//
// One commit for all of them, not one per step: a commit per step would record
// steps that changed nothing, and the unit an operator reads is "what happened
// after the change was written", singular.
func (b *builder) recordOutstanding(ctx flow.StepCtx, wt flow.Worktree) error {
	before, err := wt.RevParse(ctx.Context(), "HEAD")
	if err != nil {
		return err
	}
	if err := wt.Stage(ctx.Context()); err != nil {
		return err
	}
	// Commit is a deliberate no-op when nothing is staged, so a clean tree
	// costs one call and records nothing.
	if err := wt.Commit(ctx.Context(), b.followUpCommitMessage(ctx)); err != nil {
		return err
	}
	after, err := wt.RevParse(ctx.Context(), "HEAD")
	if err != nil {
		return err
	}
	if after != before {
		ctx.Notify("", "recorded changes made after the implement step")
	}

	// The guard, and it is not redundant with the commit above. Staging cannot
	// pick up what a backend refuses to stage, and a request opened over a tree
	// that still carries work would describe a branch missing it — the exact
	// silent loss this function exists to prevent, one layer down.
	patch, err := wt.CapturePatch(ctx.Context())
	if err != nil {
		return err
	}
	if len(patch) > 0 {
		return fmt.Errorf(
			"worktree still carries uncommitted changes after recording them, so a "+
				"pull request would describe a branch that does not contain the work "+
				"(%d bytes of diff remain); resolve the worktree by hand before retrying",
			len(patch))
	}
	return nil
}

// followUpCommitMessage names the commit that records post-implement work. It
// deliberately does NOT carry the closes-reference: the implement commit
// already does, and repeating it would make a second commit look like a second
// resolution of the same item.
func (b *builder) followUpCommitMessage(ctx flow.StepCtx) string {
	return fmt.Sprintf("Review and coverage on #%s", ctx.Item().ID)
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// runAgent is the single chokepoint every canonical step goes through.
//
// It exists so the ask contract is enforced in ONE place: an agent that ends
// its turn with AskSentinel needs a human decision, and every agent-driven step
// honors that, not just planning. A step that called ctx.Agent().Run directly
// would silently opt out of it.
//
// The returned error is the sentinel ctx.AskQuestions produces; the SDK
// translates it into a backend-posted question comment and a ParkQuestion
// stamped with the ask time, which is what the answer gate later reads.
func (b *builder) runAgent(ctx flow.StepCtx, req flow.AgentRequest) (*flow.AgentResponse, error) {
	resp, err := ctx.Agent().Run(ctx.Context(), req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("agent returned no response")
	}
	if header, body, ok := detectQuestion(resp.LastText); ok {
		// The turn that found the ambiguity is the turn that produced the
		// reasoning behind it: a step does not ask at the start, it asks once it
		// has read enough to find the question. Stashing the whole final message
		// is what makes the resumed step continue instead of paying for the same
		// analysis again to arrive at the question it now has an answer to.
		//
		// Best-effort: a stash that failed costs a re-derivation, which is the
		// old behaviour, and turning it into a step failure would lose the park
		// as well as the work.
		if err := ctx.RecordWorkInProgress(resp.LastText); err != nil {
			ctx.Notify("", "could not record work in progress: "+err.Error())
		}
		// Header is the question; Text is the evidence behind it. The step's
		// own identity is not repeated here — the park record already names the
		// step, and a header that led with it would push the actual question
		// off the first line a human reads.
		return nil, ctx.AskQuestions(flow.AgentQuestion{Header: header, Text: body})
	}
	return resp, nil
}

// agentMarkdownStep is the shape shared by the read-only analysis steps: render
// a prompt, run the agent, record what it said.
func (b *builder) agentMarkdownStep(ctx flow.StepCtx, id PromptID) error {
	pc, err := b.promptContext(ctx)
	if err != nil {
		return err
	}
	body, err := renderPrompt(b.cfg, id, pc)
	if err != nil {
		return err
	}
	resp, err := b.runAgent(ctx, flow.AgentRequest{Prompt: body})
	if err != nil {
		return err
	}
	if strings.TrimSpace(resp.LastText) == "" {
		return fmt.Errorf("agent returned nothing for %q", id)
	}
	return b.resolveMarkdown(ctx, pc, resp.SessionID, resp.LastText)
}

// resolveMarkdown records prose as this step's artifact, re-prompting the agent
// to revise when the disclosure guard refuses to publish it.
//
// docs/disclosure.md: "A refusal is not a failure of the step. The text is
// revised and re-offered." What was refused is an expression of work already
// done and paid for, so the agent is asked to fix the SENTENCE in the session
// it is already holding — not re-run to re-derive the plan. A revision costs a
// prompt, not an invocation, which is what stops three refused sentences from
// exhausting a three-invocation grant and then reporting a budget cap — naming
// the wrong problem entirely.
//
// Every step that publishes prose goes through this one copy. A step that
// called ctx.ResolveMarkdown directly would fail on a refusal instead, and lose
// the work that produced the text.
func (b *builder) resolveMarkdown(ctx flow.StepCtx, pc PromptContext, session, body string) error {
	for round := 0; ; round++ {
		err := ctx.ResolveMarkdown(body)
		if err == nil {
			return nil
		}
		var refused flow.ErrDisclosureRefused
		if !errors.As(err, &refused) {
			// Anything else is a real failure of the write. Returning it
			// unchanged keeps a broken backend from being reported as a
			// disclosure problem.
			return err
		}
		// Stash BEFORE anything else can go wrong. This is the case the store
		// earns itself on: the text was refused, so an issue comment is the one
		// place it cannot go, and losing it here would spend the whole step's
		// cost again to reach the same sentence.
		if werr := ctx.RecordWorkInProgress(refusedRecord(refused, body)); werr != nil {
			ctx.Notify("", "could not record refused text: "+werr.Error())
		}
		if round >= maxDisclosureRevisions {
			// A guard's answer may run to several lines; a park reason is read
			// as one. The whole answer is in the stashed record, which is where
			// the next invocation reads it from anyway.
			last, _, _ := strings.Cut(refused.Error(), "\n")
			// A park, not a failure: the work is sound and a person has to
			// decide. And a re-run after this park is not the identical retry —
			// it starts from the stashed draft and the refusal, which is exactly
			// what the previous attempt did not have.
			return ctx.Park(flow.ParkRequest{
				Kind: flow.ParkBlocked,
				Reason: fmt.Sprintf(
					"the disclosure guard refused this step's text %d times; last refusal: %s",
					round+1, last),
			})
		}
		ctx.Notify("", fmt.Sprintf("disclosure refused — revising (round %d)", round+1))
		rpc := pc
		rpc.Refusal = refused.Error()
		rpc.RefusedText = body
		prompt, rerr := renderPrompt(b.cfg, PromptRevise, rpc)
		if rerr != nil {
			return rerr
		}
		resp, rerr := b.runAgent(ctx, flow.AgentRequest{
			Prompt: prompt,
			// The wording is what is wrong, not the tree — and by this point a
			// producing step has already committed. A revision that edited
			// files would put work into the branch after the commit that was
			// supposed to carry it.
			PermissionMode:  "plan",
			ResumeSessionID: session,
		})
		if rerr != nil {
			return rerr
		}
		if strings.TrimSpace(resp.LastText) == "" {
			return fmt.Errorf("agent returned nothing when asked to revise refused text")
		}
		session = resp.SessionID
		body = resp.LastText
	}
}

// refusedRecord is what a refused offer leaves behind for the next invocation:
// the text, and the guard's answer about it. Both, because the text alone would
// be re-offered unchanged and refused identically.
func refusedRecord(refused flow.ErrDisclosureRefused, body string) string {
	return fmt.Sprintf(
		"An earlier run produced this text and the disclosure guard refused to publish it.\n\n"+
			"The refusal:\n\n%s\n\nThe text that was refused:\n\n%s",
		refused.Error(), body)
}

// promptContext builds the render context, exposing every upstream contributor
// artifact so a project's template can reference whichever it needs.
func (b *builder) promptContext(ctx flow.StepCtx) (PromptContext, error) {
	pc, err := newPromptContext(ctx, b.cfg, b.role, []StepID{
		StepPlan, StepImplement, StepReview, StepCoverage, StepVerifyImpl,
	})
	if err != nil {
		return PromptContext{}, err
	}
	// Answers from a previous park reach the step that asked. Without this the
	// resumed step renders the SAME prompt, asks the SAME question, and parks
	// again with a fresh timestamp that excludes the answer just given — a
	// stall no amount of answering can clear.
	pc.Answers = b.answersFor(ctx)
	// What this step left itself last time it stopped short. Without it the
	// resumed step renders the same prompt against the same context and
	// re-derives the reasoning it already paid for — worst for the plan step,
	// which changes no files and so keeps nothing else at all.
	pc.WorkInProgress = b.workInProgressFor(ctx)
	return pc, nil
}

// workInProgressFor returns what this step stashed on an earlier invocation.
//
// Best-effort for the same reason as answersFor: a failure here costs the
// prompt some context — the step re-derives, which is what it did before this
// existed — and must not fail a step that is otherwise ready to run.
func (b *builder) workInProgressFor(ctx flow.StepCtx) string {
	wip, err := ctx.WorkInProgress()
	if err != nil {
		ctx.Notify("", "could not read work in progress: "+err.Error())
		return ""
	}
	return wip
}

// answersFor returns the human replies to a question this item is parked on.
//
// Best-effort by design: the answer gate has already decided the step may run,
// so a failure here costs the prompt some context but must not fail a step the
// gate cleared.
//
// The park comes from ctx, not a fresh load. Re-loading would pay a full state
// fetch — and, since bodies hydrate on load, a second comment listing — on
// EVERY step, to read a field the orchestrator already had in hand.
func (b *builder) answersFor(ctx flow.StepCtx) []Answer {
	park := ctx.ParkedOn()
	if park == nil || park.Kind != flow.ParkQuestion {
		return nil
	}
	reader, ok := b.backend.(AnswerReader)
	if !ok {
		return nil
	}
	answers, err := reader.ReadAnswers(ctx.Context(), ctx.Item(),
		flow.QuestionAskedAt(park), b.principal(ctx.Context()))
	if err != nil {
		return nil
	}
	// Carry the question with the replies. Nothing correlates a comment to a
	// question — an issue thread has no threading — so anything a human posts
	// after the ask reads as an answer, including "+1" or "any update?". The
	// agent is the only thing that can judge relevance, and it cannot judge
	// without seeing what was asked.
	//
	// park.Reason is the one-line summary the orchestrator built, so strip its
	// "question: " prefix. For a multi-question ask it summarises rather than
	// enumerates — the canonical steps only ever ask one at a time, so that
	// only bites a project supplying its own handler.
	asked := strings.TrimSpace(strings.TrimPrefix(park.Reason, "question: "))
	for i := range answers {
		if answers[i].Text == "" {
			answers[i].Text = asked
		}
	}
	return answers
}

// ensureBranch puts the worktree on this item's claim branch and reports
// whether it had to create it.
//
// Every step that reads or runs against the tree needs this, not just the one
// that writes to it. The worktree directory is shared across items, so a tree
// left on another item's branch would have review, coverage and — worst — the
// VERIFICATION artifact all describing code from a different issue, with
// nothing noticing until the pull request step refused the branch.
func (b *builder) ensureBranch(ctx flow.StepCtx, wt flow.Worktree) (base string, created bool, err error) {
	base, err = b.baseBranch(ctx.Context())
	if err != nil {
		return "", false, err
	}
	created, err = wt.Branch(ctx.Context(), b.branchName(ctx), base)
	if err != nil {
		return "", false, err
	}
	return base, created, nil
}

// onClaimBranch is ensureBranch for the steps that read the implementation
// rather than produce it.
//
// It REFUSES when the branch had to be created. These steps run after the
// implement step, so a missing claim branch means the work is not here — a
// re-cloned or reset worktree, most likely. Cutting a fresh branch off the base
// and carrying on would have review and coverage analyse an empty change, and
// verify-impl resolve a "verify passed" artifact that is evidence for nothing.
func (b *builder) onClaimBranch(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	_, created, err := b.ensureBranch(ctx, wt)
	if err != nil {
		return err
	}
	if created {
		return fmt.Errorf("branch %q did not exist — the implementation is not in this worktree, "+
			"so there is nothing here to inspect", b.branchName(ctx))
	}
	return nil
}

// commitMessage is the subject for the verified implementation commit.
func (b *builder) commitMessage(ctx flow.StepCtx) string {
	item := ctx.Item()
	if item.Title == "" {
		return fmt.Sprintf("Resolve #%s\n\n%s", item.ID, closesRef(item.ID))
	}
	return fmt.Sprintf("%s\n\n%s", item.Title, closesRef(item.ID))
}

// closesRef renders GitHub's issue-closing reference.
//
// The "#" matters: GitHub links and auto-closes on "Closes #123" and does
// nothing at all with a bare id. This backend implements no Finalizer, so the
// pull request body is the ONLY thing that closes the issue — get the syntax
// wrong and every run leaves its issue open with no error anywhere.
func closesRef(id string) string {
	return "Closes #" + id
}

// branchName is the working branch for an item. Kept deterministic so a resumed
// step lands on the branch its earlier invocation created.
//
// This MUST match what the backend considers the claim branch — the github
// backend's Open refuses to raise a pull request from any other branch, so a
// divergence here fails every run at the last step with a confusing message.
// The coupling is unfortunate but real: no interface exposes the backend's
// naming, so the two are kept in the same format by hand.
func (b *builder) branchName(ctx flow.StepCtx) string {
	return "flow/issue-" + ctx.Item().ID
}

// pullRequestBody assembles the PR description from what the flow produced.
func (b *builder) pullRequestBody(ctx flow.StepCtx) (string, error) {
	var sb strings.Builder
	// Emit a heading only when there is something under it. A resolved
	// artifact can still carry an empty body — the backend stores bodies out
	// of line — and a PR opening with a bare "## Plan" reads as though the
	// flow produced nothing.
	section := func(title string, id StepID) bool {
		body, ok := ctx.Markdown(flow.ArtifactId(id))
		if !ok || strings.TrimSpace(body) == "" {
			return false
		}
		fmt.Fprintf(&sb, "## %s\n\n%s\n\n", title, strings.TrimSpace(body))
		return true
	}
	// The plan is the one section that must be there. A resolved plan reading
	// as empty means its body did not load — the state comment is an index and
	// bodies are fetched separately — and opening a pull request that says only
	// "Closes #N" would present a silent read failure as a finished change.
	if !section("Plan", StepPlan) {
		return "", fmt.Errorf("plan artifact is resolved but its body did not load — " +
			"refusing to open a pull request with no plan in it")
	}
	// Review and coverage reach the reader, not just the state comment. The
	// only audience a proposed change has is the person who reviews it, and an
	// artifact stored where they will not look is budget spent on nothing —
	// worse when it describes work that IS in the diff, leaving them to infer
	// unaided why it is there.
	section("Review", StepReview)
	section("Coverage", StepCoverage)
	section("Verification", StepVerifyImpl)
	fmt.Fprintln(&sb, closesRef(ctx.Item().ID))
	return sb.String(), nil
}

// verifyTail reduces a failing verify error to the part worth re-prompting on.
//
// flow.Worktree.Validate returns only an error, with the combined output
// embedded in its message, so this reads the message rather than a separate
// output channel. The TAIL is what matters: a build log's failure is at the
// end, and the head is setup noise that would crowd out the actual error.
func verifyTail(err error) string {
	if err == nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(err.Error(), "\n"), "\n")
	if len(lines) > verifyTailLines {
		lines = lines[len(lines)-verifyTailLines:]
	}
	return strings.Join(lines, "\n")
}
