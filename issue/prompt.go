package issue

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/prompt"
)

// PromptContext is what a project's prompt template is executed against.
//
// It embeds prompt.Context, so a body can reference the shared partials
// ({{.ItemHeader}}, {{.AskGuidance}}, {{.PlanStepResolution}}, ...) alongside
// its own project-specific text, and can override any partial by pre-setting
// it. See the prompt package for that model.
type PromptContext struct {
	prompt.Context

	// Role is the step set being run, so a body can say something different to
	// a maintainer than to a contributor without needing two templates.
	Role Role

	// Answers carries human replies to questions this step previously asked.
	// Non-empty ONLY on a resume after a park-for-answer; a first run sees nil.
	Answers []Answer

	// VerifyOutput is the tail of the failing verify output. Non-empty ONLY
	// when rendering PromptImplementFix.
	VerifyOutput string

	// WorkInProgress is what THIS step stashed on an earlier invocation that
	// stopped without completing. Non-empty ONLY on a resume that found a
	// record; a first run sees "". Notes, not a result — see WorkInProgressBlock.
	WorkInProgress string

	// Refusal is the disclosure guard's own answer, carried unchanged: it
	// already names what was found, where, and what would satisfy the rule.
	// Non-empty ONLY when rendering PromptRevise.
	Refusal string

	// RefusedText is the body the guard refused, so the re-prompt does not
	// depend on the agent substrate honouring ResumeSessionID — which
	// flow.AgentRequest documents as best-effort. Non-empty ONLY when rendering
	// PromptRevise.
	RefusedText string

	// CommitRefusal is the pre-commit hook's error message, verbatim.
	// Non-empty ONLY when rendering PromptCommitRepair.
	CommitRefusal string

	// StageRefusal is the staging operation's error message, verbatim.
	// Non-empty ONLY when rendering PromptStageRepair.
	StageRefusal string

	// Prior carries upstream artifacts as records rather than strings, so a
	// body cannot silently interpolate a patch into a markdown slot. Read them
	// through PriorMarkdown / PriorPatch / PriorJSON.
	Prior map[StepID]flow.ArtifactRecord
}

// PriorMarkdown returns an upstream markdown artifact's body. ok is false when
// the step did not run, did not resolve, or resolved as another type.
func (c PromptContext) PriorMarkdown(id StepID) (string, bool) {
	rec, ok := c.Prior[id]
	if !ok || !rec.Resolved || rec.Type != flow.ArtifactMarkdown {
		return "", false
	}
	return rec.Markdown, true
}

// PriorPatch returns an upstream patch artifact's body. Note that a resolved
// patch can legitimately carry no bytes — a backend storing diffs server-side
// attaches them out of band — so ok=true does not imply a non-empty diff.
func (c PromptContext) PriorPatch(id StepID) (flow.PatchBody, bool) {
	rec, ok := c.Prior[id]
	if !ok || !rec.Resolved || rec.Type != flow.ArtifactPatch {
		return flow.PatchBody{}, false
	}
	return rec.Patch, true
}

// PriorJSON returns an upstream JSON artifact's body.
func (c PromptContext) PriorJSON(id StepID) ([]byte, bool) {
	rec, ok := c.Prior[id]
	if !ok || !rec.Resolved || rec.Type != flow.ArtifactJSON {
		return nil, false
	}
	return rec.JSON, true
}

// newPromptContext builds the context for one prompt render from the live step.
func newPromptContext(ctx flow.StepCtx, cfg Config, role Role, prior []StepID) (PromptContext, error) {
	item := ctx.Item()
	pc := PromptContext{
		Context: prompt.Context{
			ItemID:          item.ID,
			ItemType:        string(item.Type),
			ItemTitle:       item.Title,
			ItemDescription: item.Body,
			VerifyCmd:       strings.Join(cfg.VerifyCmd, " "),
		},
		Role:  role,
		Prior: map[StepID]flow.ArtifactRecord{},
	}
	for _, id := range prior {
		if rec, ok := ctx.Artifact(flow.ArtifactId(id)); ok {
			pc.Prior[id] = rec
		}
	}
	// Pre-set the partials whose shared versions are written for a tracker
	// backend. Render leaves a non-empty field alone, so this is the prompt
	// package's own override mechanism, not a workaround.
	//
	// This matters more than it looks. The shared AskGuidance tells the agent
	// to call mcp__tracker__ask_user_question "(never ask in plain text)" — a
	// tool that does not exist on this path, and an instruction that directly
	// contradicts the NEEDS-ANSWER sentinel that is the only thing this package
	// can detect. An agent following it would ask into the void and the step
	// would resolve as though nothing had been asked. PlanStepResolution
	// likewise closes items through tracker statuses the GitHub backend has no
	// concept of.
	pc.AskGuidance = askGuidancePartial
	pc.PlanStepResolution = planResolutionPartial

	// Render fills only the partials still empty, so both the overrides above
	// and a project's own survive.
	if err := pc.Context.Render(); err != nil {
		return PromptContext{}, fmt.Errorf("render shared partials: %w", err)
	}
	return pc, nil
}

// askGuidancePartial is this package's AskGuidance: the sentinel contract,
// stated in the terms detectQuestion actually enforces.
//
// The example is INDENTED on purpose. Detection requires the token at column
// zero precisely so that an agent echoing this illustration back cannot park
// the flow on a placeholder question — see AskSentinel.
const askGuidancePartial = `If you need a decision from a human, do not guess and do not work around it.
End your final message with the question flush against the left margin, with no
indentation, optionally followed by a fenced block giving the evidence behind
the decision. The shape (shown indented here; write yours flush left):

    NEEDS-ANSWER: <the decision needed, on one line>
    ` + "```" + `
    <what the decision rests on, and your recommendation>
    ` + "```" + `

The fenced block is what makes the question answerable: whoever reads it cannot
choose between options without seeing what they are choosing about. Ask only
when you genuinely cannot proceed — it stops the flow until a human replies.`

// planResolutionPartial is this package's PlanStepResolution.
//
// The shared version resolves an unnecessary item by setting tracker statuses
// (duplicate / cant_reproduce / works_as_intended / wontfix) through the
// tracker MCP. None of that exists here, so the same decisions route through
// the PLAN-REFUSAL sentinel instead — a human clears the block, which is the
// correct authority for it on a GitHub repository anyway.
const planResolutionPartial = `Producing a plan is the expected outcome: assume the work is real and needed.

If you conclude it is NOT, emit a refusal. A refusal blocks the resolution on a
named reason; you do not close anything yourself. The four refusal kinds are a
closed set — do not invent others:

- already-done — the change exists or the desired state already holds.
- duplicate — the work is pending under another item.
- conflicts — what the item asks for is forbidden by the normative documents.
- not-viable — the item cannot be done as asked. The evidence requirement
  matters most here: a refusal that says only "this cannot be done" is
  indistinguishable from giving up.

Emit the refusal flush against the left margin, with the kind as the first word
after the colon and a one-line summary after it, optionally followed by a fenced
block carrying the evidence. The shape (shown indented here; write yours flush
left):

    PLAN-REFUSAL: <kind> <one-line summary>
    ` + "```" + `
    <the specific evidence: code, commit, issue, or document that makes the case>
    ` + "```" + `

The evidence block is what makes the refusal checkable — without it a reader
cannot tell whether the finding was correct. Not knowing enough to plan is
different and is not a refusal: ask a question instead.`

// repoRelativePaths is carried by every default prompt whose product is
// published on the item.
//
// An absolute path naming a home directory is wrong in an issue comment
// regardless of any guard — it names a person and a machine no reader shares,
// and it is unusable to everyone who reads it. It is also what a disclosure
// guard refuses, and a refusal caught here costs nothing, where one caught on
// the way out costs a revision round against work already finished.
const repoRelativePaths = `Cite files by path relative to the repository root, never by absolute path.
What you write here is published on the item, and no reader shares the
filesystem you are writing on.`

// narrowGateHint is carried by every producing prompt (implement, review,
// coverage) so the agent knows it can iterate on a single failing area without
// re-running the whole gate.
const narrowGateHint = `To iterate on one failing area without re-running everything, use
bin/run <gate> — it measures and judges a single gate. Run bin/run -h to
see the gate names this project defines. Passing one gate is not passing
the whole: only {{.VerifyCmd}} confirms the full set, and only the full
set may be cited.`

// renderPrompt executes the project's body for one slot, falling back to the
// library default when the project supplied none.
//
// The fallback is intentionally generic. It exists so a half-configured binary
// runs at all — useful for bring-up and for the reference shim — not so a
// project can ship without writing its own. A default that tried to be good
// would be a prompt this package cannot possibly write well, because the
// project-specific part is exactly the part it does not know.
func renderPrompt(cfg Config, id PromptID, pc PromptContext) (string, error) {
	src, isOverride := cfg.Prompts[id]
	if !isOverride || strings.TrimSpace(src) == "" {
		src = defaultPrompts[id]
		isOverride = false
		if src == "" {
			return "", fmt.Errorf("no prompt for %q and no library default", id)
		}
	}

	// When a project overrides a slot, append the policy fragments the
	// default would have carried. The project controls its body; the library
	// ensures the invariants it requires are not silently lost.
	if isOverride {
		if frags, ok := requiredFragments[id]; ok {
			src = appendFragments(src, frags)
		}
	}

	tmpl, err := template.New(string(id)).Parse(src)
	if err != nil {
		return "", fmt.Errorf("parse prompt %q: %w", id, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, pc); err != nil {
		return "", fmt.Errorf("execute prompt %q: %w", id, err)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("prompt %q rendered empty", id)
	}
	return out, nil
}

// promptFragments names the policy fragments a prompt slot requires. When a
// project overrides a slot, the library appends exactly these fragments so that
// a project cannot silently end up without them.
type promptFragments struct {
	repoRelativePaths bool
	deferCommit       bool
	workInProgress    bool
	answers           bool
	narrowGateHint    bool
}

// requiredFragments maps each slot that carries policy fragments to the set it
// requires. Slots absent from this map (the in-session re-prompts) get nothing
// appended — they run inside a session whose opening prompt already carried
// them.
var requiredFragments = map[PromptID]promptFragments{
	PromptPlan:      {repoRelativePaths: true, workInProgress: true, answers: true},
	PromptImplement: {deferCommit: true, workInProgress: true, answers: true, narrowGateHint: true},
	PromptReview:    {repoRelativePaths: true, deferCommit: true, workInProgress: true, answers: true, narrowGateHint: true},
	PromptCoverage:  {repoRelativePaths: true, deferCommit: true, workInProgress: true, answers: true, narrowGateHint: true},
}

// appendFragments appends the required policy fragments to a project's override
// body. Order mirrors the defaults: policy constraints first, then conditional
// context blocks.
func appendFragments(body string, frags promptFragments) string {
	var parts []string
	if frags.repoRelativePaths {
		parts = append(parts, repoRelativePaths)
	}
	if frags.deferCommit {
		parts = append(parts, "{{.DeferCommit}}")
	}
	if frags.narrowGateHint {
		parts = append(parts, narrowGateHint)
	}
	if frags.workInProgress {
		parts = append(parts, "{{.WorkInProgressBlock}}")
	}
	if frags.answers {
		parts = append(parts, "{{.AnswersBlock}}")
	}
	if len(parts) == 0 {
		return body
	}
	return body + "\n\n" + strings.Join(parts, "\n\n")
}

// defaultPrompts are the generic fallbacks. See renderPrompt for why they are
// deliberately thin.
var defaultPrompts = map[PromptID]string{
	PromptPlan: `{{.ItemHeader}}

Produce an implementation plan as concise markdown.

` + repoRelativePaths + `

{{.WorkInProgressBlock}}

{{.AnswersBlock}}

{{.PlanStepResolution}}

{{.AskGuidance}}`,

	PromptImplement: `{{.ItemHeader}}

Implement this plan:

{{.PlanBody}}

Make {{.VerifyCmd}} pass. {{.DeferCommit}}

` + narrowGateHint + `

{{.WorkInProgressBlock}}

{{.AnswersBlock}}

{{.AskGuidance}}`,

	PromptImplementFix: `The verify command ({{.VerifyCmd}}) is still failing. Its output ends:

` + "```" + `
{{.VerifyOutput}}
` + "```" + `

Fix the cause. Do not change tests to make them pass unless the test is itself
wrong, and say so explicitly if you conclude that.`,

	PromptReview: `Review the diff on the current branch for correctness bugs, surprising
behavior, missed edge cases, and unnecessary complexity — and FIX what you
find. You have the context loaded; leaving a fault for someone else to repair
costs another turn to rediscover what you already know.

Keep {{.VerifyCmd}} passing. {{.DeferCommit}}

` + narrowGateHint + `

Report what you changed and why, citing file:line, and say plainly what you
left alone and what needs a human decision. Someone will read this to review
the change, so write for them: what you looked for, what you changed, what
they should still judge.

` + repoRelativePaths + `

{{.WorkInProgressBlock}}

{{.AnswersBlock}}`,

	PromptCoverage: `Bring the changes on the current branch up to this project's testing standard.
This is a requirement to MEET, not a state to assess: write the tests.

Nothing runs after you. A gap you describe instead of closing is a gap that
ships, and no one will come back for it — the item will be resolved. You are
the last step holding this context, so "someone should test this later" means
it will not be tested.

Where a change cannot be tested as written, restructure it so that it can be.
Code that cannot be tested is not finished.

Leaving something untested is an exception, not an outcome. If one survives
after you have tried to restructure around it, justify it specifically — what
it is, why it resists testing, and what would have to change. A reason is
accountable; a list is a handoff to nobody.

Keep {{.VerifyCmd}} passing. {{.DeferCommit}}

` + narrowGateHint + `

Report what you added and what you restructured, so the person reviewing the
change can see what you did rather than reconstruct it from the diff.

` + repoRelativePaths + `

{{.WorkInProgressBlock}}

{{.AnswersBlock}}`,

	PromptCommitRepair: `The commit was refused by a pre-commit hook. Its error message:

` + "```" + `
{{.CommitRefusal}}
` + "```" + `

Read the hook's message and fix the problem:

- If the hook refused because of CONTENT in a file (an absolute path, a
  secret, a disclosure), edit the file to fix the offending content. The file
  belongs in the tree; only its content is wrong.
- If the hook refused because a file should not be in the tree at all (a
  binary blob, a build artifact), delete it.

Do NOT unstage files — the next stage will re-add them and the refusal repeats.
Do NOT add files to .gitignore — an ignored file survives in the tree, passes
verify, and breaks the mainline that never receives it.

After your repair the verify gate re-runs to confirm the tree is still sound.`,

	PromptStageRepair: `Staging (git add) was refused. Its error message:

` + "```" + `
{{.StageRefusal}}
` + "```" + `

Delete the offending file(s). That is the ONLY permitted action:
- Do NOT add them to .gitignore — an ignored file survives in the tree, passes
  verify, and breaks the mainline that never receives it.
- Do NOT edit any tracked source file — the gate already passed on what is here,
  and a change after it breaks the invariant that what was verified is what lands.

Delete the file(s) the error named and nothing else.`,

	PromptRevise: `The text you just produced was NOT published. A guard examines everything this
flow writes outward before it is sent, and it refused this:

` + "```" + `
{{.Refusal}}
` + "```" + `

This is not a judgement on the work — the work is done and paid for, and only
its expression is wrong. Express the refused detail another way (a
repository-relative path instead of an absolute one, for instance) rather than
re-deriving anything or dropping the substance.

This is what was refused:

` + "```" + `
{{.RefusedText}}
` + "```" + `

Reply with the FULL revised text and nothing else — no preamble, no
explanation of what you changed. Your reply is recorded verbatim as the step's
result.`,
}

// AnswersBlock renders the human replies this step is resuming on, or "" when
// there are none.
//
// A method rather than leaving `{{range .Answers}}` to every project: this text
// is what closes the park-for-answer loop, and a body that forgets to render it
// re-asks the question it was just answered, re-parks with a fresh timestamp
// that excludes the answer, and stalls forever. Making it one field to
// interpolate is what makes that easy to get right.
func (c PromptContext) AnswersBlock() string {
	if len(c.Answers) == 0 {
		return ""
	}
	var b strings.Builder
	// Deliberately "replied", not "answered": nothing correlates a comment to
	// the question, so a passing remark on the issue reaches this block too.
	// Asking the agent to judge is the only correction available — and it is a
	// real one, since re-asking re-parks past the irrelevant comment.
	b.WriteString("Someone replied on the item after you asked your question. " +
		"If a reply answers it, treat that as the decision and proceed without " +
		"asking again. If none of them do, ask again.\n")
	if q := strings.TrimSpace(c.Answers[0].Text); q != "" {
		fmt.Fprintf(&b, "\nYou asked: %s\n", q)
	}
	for _, a := range c.Answers {
		b.WriteString("\n")
		if a.Author != "" {
			fmt.Fprintf(&b, "%s replied: ", a.Author)
		} else {
			b.WriteString("reply: ")
		}
		b.WriteString(strings.TrimSpace(a.Answer))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// WorkInProgressBlock renders the notes this step left itself on an earlier
// invocation that stopped without completing, or "" when there are none.
//
// A method rather than leaving `{{.WorkInProgress}}` to every project, for the
// same reason as AnswersBlock: this text is what makes a resumed step continue
// rather than restart, and a body that forgets to render it re-derives
// everything the earlier invocation already paid for — which for the plan step
// is the entire step.
//
// The framing is load-bearing. These are notes, not a result: the step still
// has to produce its artifact, and anything the reply supersedes should be
// dropped rather than defended.
func (c PromptContext) WorkInProgressBlock() string {
	notes := strings.TrimSpace(c.WorkInProgress)
	if notes == "" {
		return ""
	}
	return "You stopped part-way through this step on an earlier run, and these " +
		"are the notes you left yourself. They are your own working-out, not a " +
		"result: nothing was recorded and the step still has to produce its " +
		"answer. Continue from them rather than starting over, and discard " +
		"whatever they got wrong.\n\n" + notes
}

// PlanBody is referenced by the default implement prompt. It is a method rather
// than a field so the default template has something to call without every
// project having to populate a field it may not use.
func (c PromptContext) PlanBody() string {
	body, _ := c.PriorMarkdown(StepPlan)
	return body
}
