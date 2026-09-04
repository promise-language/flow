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
	backend flow.Orchestrator
	// base is resolved lazily, on the first step that needs it — see
	// baseBranch. Resolving it in BuildApp would put a network call between
	// the operator and `doctor`, the command that exists to diagnose exactly
	// the failures that call can hit.
	base atomic.Pointer[flow.BranchName]
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
		// The agent asked a question (or failed), but may have produced a
		// plan first — a plan-mode turn submits the plan and THEN continues
		// to reason, so PlanText and NEEDS-ANSWER can coexist.
		//
		// The plan is saved as WIP, not resolved: resolution.md § "Work in
		// progress" requires that a step stopped by a question does not
		// resolve its artifact. Resolving would let the next derive skip
		// plan and run implement — and the answer to the question, which
		// may need to change the plan, would be silently ignored.
		//
		// runAgent already saved resp.LastText as WIP (the reasoning and
		// the question). When a PlanText also exists, overwrite with a
		// combined record so the resumed step has both the submitted plan
		// and the reasoning that led to the question.
		if resp != nil && strings.TrimSpace(resp.PlanText) != "" {
			combined := "## Submitted plan\n\n" + resp.PlanText +
				"\n\n---\n\n## Agent reasoning\n\n" + resp.LastText
			if wipErr := ctx.RecordWorkInProgress(combined); wipErr != nil {
				ctx.Notify("", "could not persist plan as work in progress: "+wipErr.Error())
			}
		}
		return err
	}
	// A refusal is the step's work — the agent read enough to conclude the
	// item should not be done. It blocks rather than resolves.
	if kind, summary, _, ok := detectRefusal(resp.LastText); ok {
		// Save the full agent output as WIP so the reasoning survives if
		// the refusal is cleared and the step resumes.
		combined := resp.LastText
		if strings.TrimSpace(resp.PlanText) != "" {
			combined = "## Submitted plan\n\n" + resp.PlanText +
				"\n\n---\n\n## Agent reasoning\n\n" + resp.LastText
		}
		if wipErr := ctx.RecordWorkInProgress(combined); wipErr != nil {
			ctx.Notify("", "could not record refusal reasoning: "+wipErr.Error())
		}
		return ctx.Park(flow.ParkRequest{
			Kind:    flow.ParkBlocked,
			Reason:  fmt.Sprintf("plan refused (%s): %s", kind, summary),
			Details: fmt.Sprintf("refusal=%s", kind),
		})
	}

	// The plan is what the agent SUBMITTED, when it submitted one. A plan-mode
	// turn ends at the submission tool, so preferring it here is preferring the
	// deliverable over whatever the agent happened to say on the way.
	plan := resp.LastText
	if resp.PlanText != "" {
		plan = resp.PlanText
	}
	// An emptiness check is not enough, and this is the case that proves it: an
	// agent that submitted a plan the transport dropped leaves behind the
	// one-line preambles it emitted before each tool call. That is not empty,
	// so it resolved — and implement, review and coverage each rendered seven
	// lines of narration into their prompts as though it were the design.
	//
	// Failing here rather than resolving is the point: "the agent planned and
	// we lost it" is a defect in this program, and publishing narration under
	// the plan's name buries it where the next three steps pay for it.
	if resp.PlanSubmitted && strings.TrimSpace(resp.PlanText) == "" {
		return fmt.Errorf(
			"agent submitted a plan and none was captured — refusing to resolve "+
				"the plan artifact from the turn's narration instead (tools used: %v)",
			resp.ToolsUsed)
	}
	if strings.TrimSpace(plan) == "" {
		return fmt.Errorf("agent returned an empty plan")
	}
	// Emptiness is the wrong property to test, and this is the case that proves
	// THAT: a turn that delegates its planning to a subagent leaves the parent's
	// narration as the only text the stream carries, and one sentence announcing
	// the delegation is not empty. Two observed artifacts resolved that way —
	// "Now let me write the final plan." and a longer preamble naming the agent
	// it was about to launch — and both passed every emptiness check between the
	// turn and the implement step.
	//
	// So the floor is structural rather than a byte count. It refuses only text
	// that is BOTH unstructured and short, which is what narration is and what a
	// plan is not: a plan carries headings, and a plan without headings is at
	// least long. Deliberately permissive — the point is to catch the sentence,
	// not to adjudicate plan quality, which is the review step's job.
	//
	// This fails rather than parking refused: the plan step keeps its remaining
	// invocations and a retry re-runs planning, which may well produce a real
	// plan, since whether the agent delegates is its own choice each turn. The
	// artifact never resolves, so nothing downstream reads narration.
	if !looksLikePlan(plan) {
		return fmt.Errorf(
			"plan artifact is %d bytes with no headings — refusing to resolve what "+
				"looks like the turn's narration rather than a plan (plan submitted: %t, "+
				"tools used: %v): %q",
			len(strings.TrimSpace(plan)), resp.PlanSubmitted, resp.ToolsUsed,
			firstLine(plan))
	}
	return b.resolveMarkdown(ctx, pc, resp.SessionID, plan)
}

// planProseFloor is the length above which unstructured text is accepted as a
// plan. A plan with no heading at all is unusual but not impossible; a plan
// that is also shorter than a paragraph is narration. The two observed
// narration artifacts were 32 and 135 bytes, and every real plan produced by
// this flow has been thousands, so the gap either side of this is wide.
const planProseFloor = 400

// looksLikePlan reports whether text is structurally a plan rather than the
// sentence an agent said on its way to writing one. A markdown heading is
// sufficient on its own — writing one is an act of structuring, which
// narration does not do — and unstructured text passes on length alone.
func looksLikePlan(plan string) bool {
	for _, line := range strings.Split(plan, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			return true
		}
	}
	return len(strings.TrimSpace(plan)) >= planProseFloor
}

// firstLine returns the first non-blank line, trimmed, for use in an error that
// has to show what was rejected without reproducing a whole document.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// stepOpenBranch puts the worktree on this item's branch and records the commit
// it was cut from.
//
// Mechanical, and its own step for three reasons. A branch that fails to open —
// a dirty tree, a name already taken, a base that cannot be resolved — fails
// HERE, naming its own cause, rather than surfacing as an implement failure that
// sends a reader to look at the agent. Implement's concern stays "make it work"
// rather than "prepare a workspace and then make it work". And every step after
// this one commits, so the branch has to exist before any of them runs.
//
// It records HEAD after the checkout, not the base branch's tip. On a resume the
// two differ: a branch cut by an earlier attempt that died before resolving is
// still empty, and its HEAD is the base as it stood when the branch was cut.
// Reading the base branch today would record a commit the branch was never cut
// from — and that record is exactly what makes "what is this change relative to"
// answerable later against a base that has since moved.
func (b *builder) stepOpenBranch(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	if _, _, err := b.ensureBranch(ctx, wt); err != nil {
		// Named, so the message stands on its own wherever it is read: what
		// could not be opened, and why. That is the whole of "fails as itself".
		return fmt.Errorf("could not open branch %q: %w", b.branchName(ctx), err)
	}
	head, err := wt.RevParse(ctx.Context(), "HEAD")
	if err != nil {
		return err
	}
	return ctx.ResolveCommitHash(string(head))
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
// The artifact resolves ONLY on a green gate. A commit recorded after a failing
// verify would name work that does not build.
func (b *builder) stepImplement(ctx flow.StepCtx) error {
	pc, err := b.promptContext(ctx)
	if err != nil {
		return err
	}
	// Emptiness, not just absence: a resolved artifact whose body did not load
	// reads as present, and implementing against a blank plan is worse than
	// refusing — the agent writes something plausible and nothing reports why.
	plan, ok := pc.PriorMarkdown(StepPlan)
	if !ok || strings.TrimSpace(plan) == "" {
		return fmt.Errorf("plan artifact missing, unresolved, or empty — refusing to implement without a plan")
	}
	// And structure, not just emptiness, by the same reasoning one step further:
	// a single sentence of narration is not empty either, and dispatching
	// against it costs an invocation to produce work anchored to nothing. The
	// predicate is stepPlan's, deliberately — one floor, applied at both ends.
	// Not redundant with it: this catches an artifact resolved by an older
	// binary, by a hand edit, or by any future path that does not run stepPlan.
	//
	// Separate from the refusal above, and it quotes what it rejected, because
	// the two send a reader somewhere different: an empty artifact is a load
	// that failed, while this one is a plan step that resolved something that
	// was never a plan — and the operator cannot tell which without seeing it.
	if !looksLikePlan(plan) {
		return fmt.Errorf(
			"plan artifact is %d bytes with no headings — refusing to implement against "+
				"what looks like the planning turn's narration rather than a plan: %q",
			len(strings.TrimSpace(plan)), firstLine(plan))
	}
	// The commit the branch was cut from, as the branch step recorded it. Same
	// shape as the plan refusal above, and for the same reason: without it the
	// empty-branch check below has nothing to compare against.
	baseSHA, ok := ctx.CommitHash(flow.ArtifactId(StepBranch))
	if !ok || strings.TrimSpace(baseSHA) == "" {
		return fmt.Errorf("branch artifact missing, unresolved, or empty — " +
			"refusing to implement without the commit the branch was cut from")
	}

	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	// The branch must already exist. Cutting one here is the failure the open
	// branch step exists to report as itself.
	if err := b.onClaimBranch(ctx); err != nil {
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

		ctx.Notify("", "running the verify command")
		run, rerr := wt.Run(ctx.Context(), flow.CommandVerify)
		if rerr != nil {
			return rerr // no command ran and no outcome exists
		}
		if err := verifyOutcomeError(run); err != nil {
			return err
		}
		if run.ExitCode == 0 {
			break
		}
		// A failure observed on an unfit machine does not reach an agent:
		// re-prompting with "No space left on device" burns a turn on a
		// condition no edit can change. The orchestrator's ErrUnfit branch
		// reports blocked.
		if fitErr := flow.CheckFit(ctx.Context(), wt); fitErr != nil {
			return fitErr
		}
		if attempt >= rounds {
			// An error, not a park. A park here gated nothing — no preflight
			// refuses a ParkBlocked item — so an unattended `resolve` would
			// re-enter this loop and spend the whole round budget again on
			// every cycle. Failing stops the run with the reason attached; the
			// work stays in the worktree either way, so a human who fixes the
			// blocker or raises MaxFixRounds resumes from where it stopped.
			return fmt.Errorf("verify still failing after %d fix attempts: %s",
				attempt, verifyTail(run))
		}
		// Re-prompt with the failing tail. Rebuilt each round so the context
		// carries THIS round's output, not the first round's.
		pc.VerifyOutput = verifyTail(run)
		prompt, err = renderPrompt(b.cfg, PromptImplementFix, pc)
		if err != nil {
			return err
		}
	}

	// The prompts tell the agent NOT to commit — committing mid-loop would bury
	// a half-finished round in history — which makes it this handler's job.
	// Nothing else in the flow commits before the request, so without this the
	// branch pushed at PR time carries no commits and `gh pr create` fails with
	// "No commits between ...".
	//
	// The same helper every other producing step records through, so implement
	// also gets the dirty-tree guard: the invariant is the same one step later,
	// and one copy of it is what keeps the two from drifting.
	ctx.Notify("", "staging and committing")
	if err := b.recordStepWork(ctx, wt, "implement", b.commitMessage(ctx)); err != nil {
		return err
	}
	// Commit is a deliberate no-op when nothing is staged, so a nil return is
	// not evidence that anything was recorded. Ask the question that actually
	// matters instead: does this branch carry anything the commit it was cut
	// from does not?
	//
	// Comparing HEAD before and after the commit would only cover the
	// invocation that made it — a resumed branch whose work was committed by an
	// earlier run that then died would read as "the agent did nothing" and
	// deadlock the step with the change sitting right there. Comparing against
	// the RECORDED base covers both, and catches an empty branch here rather
	// than three agent turns later at "No commits between ...".
	//
	// Recorded rather than re-read, which is what closes the case a base-branch
	// lookup misses: a branch cut by an earlier run, still empty, on a base that
	// has advanced since. Its HEAD is the base as it stood at the cut, which no
	// longer equals the base branch's tip — so comparing against today's base
	// would read an empty branch as work.
	head, err := wt.RevParse(ctx.Context(), "HEAD")
	if err != nil {
		return err
	}
	if string(head) == baseSHA {
		return fmt.Errorf("branch %q carries no commits beyond %s, the commit it was cut from — "+
			"the agent changed nothing, so there is nothing to open a pull request from",
			b.branchName(ctx), baseSHA)
	}

	// The commit, not a copy of it. A resumed branch whose work an earlier run
	// already committed has a clean tree, so a patch captured here would be
	// legitimately empty — a record that may be empty, that nothing reads back,
	// and that can disagree with what it copies. A commit names exactly one
	// state and is never ambiguous.
	return ctx.ResolveCommitHash(string(head))
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
//
// The message is the caller's rather than built here: only the implement commit
// carries the closes-reference, and a second commit repeating it would read as a
// second resolution of the same item.
func (b *builder) recordStepWork(ctx flow.StepCtx, wt flow.Worktree, label, msg string) error {
	if err := b.commitWithRepair(ctx, wt, msg); err != nil {
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

// commitWithRepair stages, commits, and on a pre-commit hook refusal runs a
// bounded content-aware repair loop. A content refusal (absolute path, secret)
// asks the agent to edit the offending file in place; a presence refusal
// (binary blob, build artifact) asks it to delete the file. Each repair round
// re-runs verify — mandatory for content edits, cheap for deletions — and
// re-stages before retrying the commit. The loop is bounded by
// maxDisclosureRevisions.
//
// On exhaustion the handler parks directly with a safe reason that names
// neither the file nor the fragment the hook quoted. The hook's messages are
// stashed locally via RecordWorkInProgress so the next run can act on them.
func (b *builder) commitWithRepair(ctx flow.StepCtx, wt flow.Worktree, msg string) error {
	firstStageErr := wt.Stage(ctx.Context())
	if firstStageErr != nil {
		// A staging failure on an unfit machine is not a refusal — it is
		// ENOSPC or similar. Do not spend an agent turn on it.
		if fitErr := flow.CheckFit(ctx.Context(), wt); fitErr != nil {
			return fitErr
		}
		ctx.Notify("", fmt.Sprintf("staging refused: %s", firstStageErr))

		pc := PromptContext{StageRefusal: firstStageErr.Error()}
		prompt, err := renderPrompt(b.cfg, PromptStageRepair, pc)
		if err != nil {
			return err
		}
		_, err = ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         prompt,
			PermissionMode: "acceptEdits",
		})
		if err != nil {
			return err // infrastructure failure, NOT ErrRefused
		}

		if secondStageErr := wt.Stage(ctx.Context()); secondStageErr != nil {
			return fmt.Errorf("staging refused twice — first: %s — second: %s: %w",
				firstStageErr, secondStageErr, flow.ErrRefused)
		}
	}
	firstErr := wt.Commit(ctx.Context(), msg)
	if firstErr == nil {
		return nil
	}

	// Bounded content-aware repair loop. A revision costs a prompt, not an
	// invocation, so a few rounds cannot exhaust an invocation grant.
	lastErr := firstErr
	for round := 0; round <= maxDisclosureRevisions; round++ {
		ctx.Notify("", fmt.Sprintf(
			"commit refused by pre-commit hook (round %d): %s", round+1, lastErr))

		pc := PromptContext{CommitRefusal: lastErr.Error()}
		prompt, err := renderPrompt(b.cfg, PromptCommitRepair, pc)
		if err != nil {
			return err
		}
		_, err = ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         prompt,
			PermissionMode: "acceptEdits",
		})
		if err != nil {
			return err // infrastructure failure
		}

		// The agent may have edited files → re-verify unconditionally.
		// For a delete-only repair this is cheap (fewer files, still valid).
		// For a content repair it is mandatory.
		run, rerr := wt.Run(ctx.Context(), flow.CommandVerify)
		if rerr != nil {
			return rerr // no command ran and no outcome exists
		}
		if err := verifyOutcomeError(run); err != nil {
			return err
		}
		if run.ExitCode != 0 {
			// Verify RAN and reported failures. Stash context and park.
			ctx.RecordWorkInProgress(fmt.Sprintf(
				"commit repair round %d: verify failed after editing.\n\n"+
					"Hook refusal:\n%s\n\nVerify failure:\n%s",
				round+1, lastErr, verifyTail(run)))
			return ctx.Park(flow.ParkRequest{
				Kind: flow.ParkBlocked,
				Reason: "the pre-commit hook refused this step's commit and " +
					"the repair broke verify; details are kept with the step",
			})
		}

		if err := wt.Stage(ctx.Context()); err != nil {
			return err
		}
		commitErr := wt.Commit(ctx.Context(), msg)
		if commitErr == nil {
			return nil
		}
		lastErr = commitErr
	}

	// Exhausted. Stash the hook's messages locally — never published.
	ctx.RecordWorkInProgress(fmt.Sprintf(
		"commit repair exhausted after %d rounds.\n\n"+
			"First hook refusal:\n%s\n\nLast hook refusal:\n%s",
		maxDisclosureRevisions+1, firstErr, lastErr))

	return ctx.Park(flow.ParkRequest{
		Kind: flow.ParkBlocked,
		Reason: fmt.Sprintf(
			"the pre-commit hook refused this step's commit %d times; "+
				"what it refused is kept with the step for the next run",
			maxDisclosureRevisions+2),
	})
}

// itemLabel names the item in a commit subject, without the closes-reference:
// only the implement commit carries that, or every step would read as its own
// resolution of the item.
func (b *builder) itemLabel(ctx flow.StepCtx) string {
	item := ctx.Item()
	if item.Title == "" {
		return "#" + itemNumber(item)
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
	if err := b.recordStepWork(ctx, wt, label,
		fmt.Sprintf("%s: %s", label, b.itemLabel(ctx))); err != nil {
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

// stepOpenPR measures the branch and, if the measurement is acceptable, opens
// the pull request. The pr-open signal is set by the backend as a side effect of
// Open succeeding, not by this handler.
func (b *builder) stepOpenPR(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	item := ctx.Item()
	title := item.Title
	if title == "" {
		title = fmt.Sprintf("Resolve #%s", itemNumber(item))
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
	ctx.Notify("", "recording post-implement changes")
	if err := b.recordOutstanding(ctx, wt); err != nil {
		return err
	}

	// The gate, after recording and before the push. After, because it must
	// measure the branch exactly as it will be proposed — recording moves the
	// tree, so measuring first would establish something about a state nobody
	// will review. Before, because a branch that cannot pass must not leave the
	// machine: the maintainer runs the same gate, and a request that fails it
	// spends a reviewer's attention on a change that was never going to land.
	verdict, err := b.runIntegrationGate(ctx, wt, "the branch as it will be proposed")
	if err != nil {
		return err
	}

	body, err := b.pullRequestBody(ctx, verdict)
	if err != nil {
		return err
	}
	ctx.Notify("", "pushing and opening pull request")
	_, err = flow.Open(ctx.Context(), wt, flow.BranchName(base), title, body)
	if err == nil {
		return nil
	}

	// Only recover ActPush refusals. An ActPullRequest refusal (PR body/title)
	// is a different surface — return it as-is.
	var refused flow.ErrDisclosureRefused
	if !errors.As(err, &refused) || refused.Act != flow.ActPush {
		return err
	}

	// Stash before anything else can fail.
	ctx.RecordWorkInProgress(fmt.Sprintf(
		"The disclosure guard refused the push.\n\nThe refusal:\n\n%s",
		refused.Error()))

	ctx.Notify("", "disclosure refused the push — asking the agent to rewrite history")

	pc := PromptContext{PushRefusal: refused.Error()}
	prompt, rerr := renderPrompt(b.cfg, PromptPushRepair, pc)
	if rerr != nil {
		return rerr
	}
	_, rerr = b.runAgent(ctx, flow.AgentRequest{
		Prompt:         prompt,
		PermissionMode: "acceptEdits",
	})
	if rerr != nil {
		return rerr
	}

	// One retry.
	ctx.Notify("", "retrying push after history rewrite")
	_, err = flow.Open(ctx.Context(), wt, flow.BranchName(base), title, body)
	if err == nil {
		return nil
	}

	// Second failure: park, don't fail.
	// Infrastructure errors on retry are also parked — the work is done,
	// and a transient push failure after a successful rebase should not
	// discard the rebase.
	var refused2 flow.ErrDisclosureRefused
	if errors.As(err, &refused2) {
		ctx.RecordWorkInProgress(fmt.Sprintf(
			"The disclosure guard refused the push a second time.\n\nThe refusal:\n\n%s",
			refused2.Error()))
	}

	return ctx.Park(flow.ParkRequest{
		Kind: flow.ParkBlocked,
		Reason: "the disclosure guard refused this step's push twice; " +
			"what it refused and why are kept with the step for the next run",
	})
}

// runIntegrationGate measures subject and asks the project whether the
// measurement is acceptable. subject names what is being measured — "the
// branch as it will be proposed" for the contributor gate, "the merge result"
// for the integration phase — and appears in every notification and error so
// a reader knows which context the gate ran in.
//
// Each way this can stop returns an ERROR rather than a park, for the reason
// already recorded on the implement loop: no preflight refuses a ParkBlocked
// item, so a park here is re-dispatched next cycle and spends the gate again.
//
// The three failures are deliberately worded apart, because they belong to
// different people and only one of them is about the change:
//
//   - RunGate errored: no gate ran and no outcome exists. Never read as the
//     change failing.
//   - The run measured nothing: the gate timed out, could not start, died, or
//     broke its contract. Still nothing about the change.
//   - Judge errored: no verdict exists. Explicitly not a refusal — an
//     unanswerable judge says nothing about the measurement, and reading it as
//     "not acceptable" would refuse a sound change over the project's own
//     broken tooling.
//
// Only the fourth — a verdict that is a refusal — is about the change, and it
// is a perfectly good answer arriving with a nil error.
func (b *builder) runIntegrationGate(ctx flow.StepCtx, wt flow.Worktree, subject string) (flow.GateVerdict, error) {
	ctx.Notify("", fmt.Sprintf("running the %s gate on %s", flow.GateIntegration, subject))
	run, err := wt.RunGate(ctx.Context(), flow.GateIntegration)
	if err != nil {
		return flow.GateVerdict{}, fmt.Errorf(
			"no %s gate ran on %s, so nothing was measured — this is not the "+
				"change failing: %w: %w", flow.GateIntegration, subject, err, flow.ErrTransient)
	}
	if run.Outcome != flow.OutcomeMeasured {
		return flow.GateVerdict{}, fmt.Errorf(
			"the %s gate reports %q on %s, so nothing was measured — this is not "+
				"the change failing%s: %w", flow.GateIntegration, run.Outcome, subject,
			detailSuffix(run.Detail), flow.ErrTransient)
	}
	verdict, err := wt.Judge(ctx.Context(), run)
	if err != nil {
		return flow.GateVerdict{}, fmt.Errorf(
			"the %s gate measured %s but no verdict exists, which is not a refusal — "+
				"the project's judging layer could not answer: %w: %w", flow.GateIntegration, subject, err, flow.ErrTransient)
	}
	if !verdict.Acceptable {
		return flow.GateVerdict{}, fmt.Errorf(
			"the %s gate's verdict on %s is not acceptable%s",
			flow.GateIntegration, subject, detailSuffix(verdict.Detail))
	}
	return verdict, nil
}

// detailSuffix appends prose a runner or a judge wrote for a person, when there
// is any. Nothing keys on it, so an empty one costs the message nothing.
func detailSuffix(detail string) string {
	if d := strings.TrimSpace(detail); d != "" {
		return ": " + d
	}
	return ""
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
	if err := b.commitWithRepair(ctx, wt, b.followUpCommitMessage(ctx)); err != nil {
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
	return fmt.Sprintf("Review and coverage on #%s", itemNumber(ctx.Item()))
}

// stepCloseBranch returns the worktree to the base branch.
//
// It runs only when the resolution completed: DeriveNext dispatches the first
// PENDING step in registration order, and this one is registered after the
// request, so a run that parked, was blocked or failed never reaches it. That is
// the point — the state a stopped run left is what someone resumes from or
// diagnoses.
//
// It does NOT delete the item's branch. The branch carries the request, and the
// request outlives the resolution that opened it.
func (b *builder) stepCloseBranch(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return fmt.Errorf("close branch: worktree unavailable: %v: %w", err, flow.ErrTransient)
	}
	base, err := b.baseBranch(ctx.Context())
	if err != nil {
		return fmt.Errorf("close branch: base branch lookup failed: %v: %w", err, flow.ErrTransient)
	}
	// Branch CREATES when the name is absent, so the created flag is the check
	// rather than decoration: without it, a worktree missing the base branch
	// silently gets a new branch of that name pointing at this item's tip, and
	// every later item would be cut from the wrong place.
	created, err := wt.Branch(ctx.Context(), flow.BranchName(base), "")
	if err != nil {
		return fmt.Errorf("close branch: checkout %s: %v: %w", base, err, flow.ErrTransient)
	}
	if created {
		return fmt.Errorf("base branch %q is not in this worktree, so the worktree cannot be "+
			"returned to it — a branch of that name now points at this item's work and has "+
			"to be resolved by hand: %w", base, flow.ErrRefused)
	}
	return ctx.ResolveFlag()
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
	ctx.Notify("", "awaiting the agent")
	resp, err := ctx.Agent().Run(ctx.Context(), req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("agent returned no response")
	}
	if header, body, ok := detectQuestion(resp.LastText); ok {
		return b.resolveQuestion(ctx, resp, header, body)
	}
	return resp, nil
}

// resolveQuestion handles a question detected in the agent's response, routing
// it through a disclosure-revision loop that mirrors resolveMarkdown. On a
// refusal the agent is asked to reframe the question in the session it already
// holds; on exhaustion the step parks instead of failing.
func (b *builder) resolveQuestion(ctx flow.StepCtx, resp *flow.AgentResponse, header, body string) (*flow.AgentResponse, error) {
	// The turn that found the ambiguity is the turn that produced the
	// reasoning behind it: stashing the whole final message is what makes the
	// resumed step continue instead of re-deriving the question.
	//
	// Best-effort: a stash that failed costs a re-derivation, which is the
	// old behaviour, and turning it into a step failure would lose the park
	// as well as the work.
	if err := ctx.RecordWorkInProgress(resp.LastText); err != nil {
		ctx.Notify("", "could not record work in progress: "+err.Error())
	}

	session := resp.SessionID
	questionText := header + "\n" + body

	for round := 0; ; round++ {
		err := ctx.AskQuestions(flow.AgentQuestion{Header: header, Text: body})
		if err == nil {
			// Should not happen — AskQuestions always returns a sentinel or
			// error — but if it does, the question posted and we are done.
			return resp, nil
		}
		var refused flow.ErrDisclosureRefused
		if !errors.As(err, &refused) {
			// Either the ErrQuestion sentinel (the question posted and the
			// orchestrator should park) or a real backend error. Both
			// propagate unchanged.
			return resp, err
		}

		// Stash the refused text before anything else can go wrong.
		if werr := ctx.RecordWorkInProgress(refusedRecord(refused, questionText)); werr != nil {
			ctx.Notify("", "could not record refused text: "+werr.Error())
		}
		if round >= maxDisclosureRevisions {
			// A park, not a failure: the work is sound and a person has to
			// decide. The reason carries NOTHING the guard said — a park is
			// published through the same guard, and a reason repeating the
			// guard's answer carries the refused fragment itself.
			return resp, ctx.Park(flow.ParkRequest{
				Kind: flow.ParkBlocked,
				Reason: fmt.Sprintf(
					"the disclosure guard refused this step's question %d times (%s); "+
						"what it refused and why are kept with the step for the next run",
					round+1, refused.Act),
			})
		}
		ctx.Notify("", fmt.Sprintf("question disclosure refused — revising (round %d)", round+1))

		pc, perr := b.promptContext(ctx)
		if perr != nil {
			return nil, perr
		}
		rpc := pc
		rpc.Refusal = refused.Error()
		rpc.RefusedText = questionText
		prompt, rerr := renderPrompt(b.cfg, PromptRevise, rpc)
		if rerr != nil {
			return nil, rerr
		}
		// Call ctx.Agent().Run directly — NOT b.runAgent, which detects
		// questions and calls resolveQuestion, creating infinite recursion.
		newResp, rerr := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:          prompt,
			PermissionMode:  "plan",
			ResumeSessionID: session,
		})
		if rerr != nil {
			return nil, rerr
		}
		if newResp == nil {
			return nil, fmt.Errorf("agent returned no response when asked to revise refused question")
		}

		session = newResp.SessionID
		resp = newResp

		// Did the agent drop the question on revision?
		newHeader, newBody, ok := detectQuestion(newResp.LastText)
		if !ok {
			// The agent chose not to ask — the step continues without a question.
			return newResp, nil
		}
		header = newHeader
		body = newBody
		questionText = header + "\n" + body
	}
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
			// A park, not a failure: the work is sound and a person has to
			// decide. And a re-run after this park is not the identical retry —
			// it starts from the stashed draft and the refusal, which is exactly
			// what the previous attempt did not have.
			//
			// The reason carries NOTHING the guard said. A park is published:
			// Backend.Park posts the whole request as an issue comment, through
			// the same guard. A refusal names what it found and quotes it
			// (docs/disclosure.md), so a reason repeating the guard's answer
			// carries the refused fragment itself — the guard refuses the park
			// record too, Backend.Park errors, and the item never parks at all:
			// the run dies with an error and no person is told anything. The
			// act is the SDK's own closed vocabulary and is safe to publish;
			// the guard's answer stays in the stashed record, which is local
			// and never published.
			return ctx.Park(flow.ParkRequest{
				Kind: flow.ParkBlocked,
				Reason: fmt.Sprintf(
					"the disclosure guard refused this step's text %d times (%s); "+
						"what it refused and why are kept with the step for the next run",
					round+1, refused.Act),
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
	// The branch step is not here: implement reads the commit it recorded
	// through ctx.CommitHash, and no prompt has anything to say about it.
	pc, err := newPromptContext(ctx, b.cfg, b.role, []StepID{
		StepPlan, StepImplement, StepReview, StepCoverage,
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
// left on another item's branch would have review and coverage analyse code
// from a different issue, and — worst — the gate the request rests on measure
// it. Nothing would notice: the request would be proposed carrying a
// measurement of somebody else's change.
func (b *builder) ensureBranch(ctx flow.StepCtx, wt flow.Worktree) (base flow.BranchName, created bool, err error) {
	base, err = b.baseBranch(ctx.Context())
	if err != nil {
		return "", false, err
	}
	created, err = wt.Branch(ctx.Context(), flow.BranchName(b.branchName(ctx)), base)
	if err != nil {
		return "", false, err
	}
	return base, created, nil
}

// onClaimBranch is ensureBranch for the steps that read the implementation
// rather than produce it.
//
// It REFUSES when the branch had to be created. These steps run after the open
// branch step, so a missing claim branch means the work is not here — a
// re-cloned or reset worktree, most likely. Cutting a fresh branch off the base
// and carrying on would have review and coverage analyse an empty change, and
// the request propose one.
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
	num := itemNumber(item)
	if item.Title == "" {
		return fmt.Sprintf("Resolve #%s\n\n%s", num, closesRef(num))
	}
	return fmt.Sprintf("%s\n\n%s", item.Title, closesRef(num))
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

// itemNumber is the orchestrator's own number for the item.
//
// Item carries no store id — docs/orchestrator.md removed it, because the
// orchestrator's key IS a projection of ItemRef — so the number is read back
// out of the display, which for the github orchestrator is `owner/repo#123`.
// Reading a projection is not reconstructing a ref from one: nothing here
// addresses the item with the result, it only spells it the way GitHub does.
//
// Two things this package writes are GitHub's syntax and not flow's, and both
// need the bare number: the claim branch (`flow/issue-<N>`, which Open refuses
// to raise a pull request from any other branch) and the closing reference
// (`Closes #<N>`, the only thing that closes the issue). Handing either the
// whole display produces `flow/issue-owner/repo#123` and `Closes
// #owner/repo#123` — a branch the orchestrator never looks for, and a reference
// GitHub does nothing with.
//
// A display carrying no "#" is used whole: that is already the item's own name.
func itemNumber(item flow.Item) string {
	d := item.Ref.Display
	if i := strings.LastIndexByte(d, '#'); i >= 0 {
		return d[i+1:]
	}
	return d
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
	return "flow/issue-" + itemNumber(ctx.Item())
}

// pullRequestBody assembles the PR description from what the flow produced and
// what the gate established.
func (b *builder) pullRequestBody(ctx flow.StepCtx, verdict flow.GateVerdict) (string, error) {
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
	// The gate's result travels with the request. This is the whole of
	// "recorded, so a reader knows what was established rather than taking it on
	// trust": the body is where a reader meets it, and it carries both inputs
	// the verdict was computed from as well as the answer, so the verdict can be
	// recomputed by whoever was not there.
	sb.WriteString(gateSection(verdict))
	fmt.Fprintln(&sb, closesRef(itemNumber(ctx.Item())))
	return sb.String(), nil
}

// gateSection renders what the gate established: which gate, what the runner
// observed, what the project's judge answered, and BOTH inputs it answered
// from.
//
// Both, because either one alone leaves a reader exactly where a reader with
// neither stands. The envelope says what was measured and the thresholds say
// what it was judged against, and recomputing the verdict needs the pair — so
// a body carrying only the measurement publishes an answer nobody can check.
// That is not a presentation detail: recomputability is the whole reason a
// judge is allowed to live in the tree it judges, and a verdict whose terms
// were discarded is as unfalsifiable as a lying runner. See
// docs/gates-and-commands.md § "Where the verdict is made".
func gateSection(v flow.GateVerdict) string {
	var sb strings.Builder
	sb.WriteString("## Gate\n\n")
	fmt.Fprintf(&sb, "- gate: `%s`\n", v.Run.Gate)
	fmt.Fprintf(&sb, "- outcome: `%s`\n", v.Run.Outcome)
	fmt.Fprintf(&sb, "- acceptable: `%t`\n", v.Acceptable)
	if d := strings.TrimSpace(v.Detail); d != "" {
		fmt.Fprintf(&sb, "- verdict: %s\n", d)
	}
	// Labelled, because two unlabelled fenced blocks are two blobs of JSON a
	// reader has to tell apart by guessing which is which.
	block := func(label string, body []byte) {
		if b := strings.TrimSpace(string(body)); b != "" {
			fmt.Fprintf(&sb, "\n%s:\n\n```\n%s\n```\n", label, b)
		}
	}
	block("measurement", v.Run.Stdout)
	block("thresholds", v.Thresholds)
	sb.WriteString("\n")
	return sb.String()
}

// verifyTail reduces a failing verify error to the part worth re-prompting on.
//
// flow.Worktree.Validate returns only an error, with the combined output
// embedded in its message, so this reads the message rather than a separate
// output channel. The TAIL is what matters: a build log's failure is at the
// end, and the head is setup noise that would crowd out the actual error.
// verifyOutcomeError translates a verify run whose outcome is NOT a
// measurement into the sentinel that says what a retry is worth.
//
// The three cases are not the same and used to be one `err != nil`:
//
//   - measured — it ran and reported. Exit 0 proceeds; a non-zero exit is a
//     real result and costs the round it takes to fix. Not this function's
//     business.
//   - timed_out — the wait is the problem, not the change. ErrTransient, so the
//     orchestrator parks WITHOUT burning an invocation: a lock that timed out
//     is worth retrying unchanged.
//   - could_not_start / died / broke_contract — re-running changes nothing a
//     retry can fix. ErrRefused, so again no invocation is burned: charging a
//     missing binary would drain the budget on identical no-op failures and
//     then park on a budget message describing the clock rather than the cause.
func verifyOutcomeError(run flow.CommandRun) error {
	switch run.Outcome {
	case flow.OutcomeMeasured:
		return nil
	case flow.OutcomeTimedOut:
		return fmt.Errorf("the verify command timed out (%s): %w", run.Detail, flow.ErrTransient)
	default:
		return fmt.Errorf("the verify command reported %s and produced no measurement (%s): %w",
			run.Outcome, run.Detail, flow.ErrRefused)
	}
}

// verifyTail is the last few lines of what verify printed, for a re-prompt.
func verifyTail(run flow.CommandRun) string {
	if len(run.Stdout) == 0 {
		return run.Detail
	}
	lines := strings.Split(strings.TrimRight(string(run.Stdout), "\n"), "\n")
	if len(lines) > verifyTailLines {
		lines = lines[len(lines)-verifyTailLines:]
	}
	return strings.Join(lines, "\n")
}
