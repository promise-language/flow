package main

import "github.com/promise-language/flow/issue"

// prompts is the half of a flow binary that is genuinely project-specific.
//
// Each entry is a text/template executed against issue.PromptContext, which
// embeds prompt.Context — so {{.ItemHeader}}, {{.AskGuidance}},
// {{.PlanStepResolution}} and {{.DeferCommit}} are the shared partials, already
// rendered, and everything else here is this project talking about itself.
//
// A real project replaces the bodies below with its own: which build and test
// commands, which pipeline stages a change has to respect, which policies make
// a change unacceptable regardless of whether the tests pass. That content
// cannot live in the library, which is why this seam exists — see the issue
// package docs.
//
// Slots left out of this map fall back to the library's generic defaults, so a
// binary runs before its prompts are written. Shipping on the defaults is not
// the intent.
var prompts = map[issue.PromptID]string{
	issue.PromptPlan: `{{.ItemHeader}}

Produce an implementation plan as concise markdown. Name the files you expect to
change and why. Do not write code yet.

{{.PlanStepResolution}}

If this item conflicts with a project constraint you cannot resolve on your own,
do not guess and do not quietly plan around it — ask, following the guidance
below, and quote the conflicting statement in the block.

{{.WorkInProgressBlock}}

{{.AnswersBlock}}

{{.AskGuidance}}`,

	issue.PromptImplement: `{{.ItemHeader}}

Implement this plan:

{{.PlanBody}}

Rules for this project:
  - {{.VerifyCmd}} must pass before the change is done.
  - Match the surrounding code's idioms rather than importing your own.
  - {{.DeferCommit}}

{{.WorkInProgressBlock}}

{{.AnswersBlock}}

{{.AskGuidance}}`,

	// The re-prompt. VerifyOutput is the tail of the failing gate, and is
	// populated only for this slot.
	issue.PromptImplementFix: `{{.VerifyCmd}} is still failing. The tail of its output:

` + "```" + `
{{.VerifyOutput}}
` + "```" + `

Fix the underlying cause. Do not weaken or delete a test to make it pass — if
you believe the test itself is wrong, say so explicitly and explain why rather
than changing it silently.`,

	issue.PromptReview: `Review the diff on the current branch as a maintainer would.

Flag correctness bugs, surprising behavior, missed edge cases, and complexity
that is not carrying its weight. Cite file:line for each point. Be specific
enough that someone could act on the finding without rediscovering it.

End with PASS or FAIL on its own line.

{{.WorkInProgressBlock}}

{{.AnswersBlock}}

{{.AskGuidance}}`,

	issue.PromptCoverage: `Analyze test coverage of the changes on the current branch.

List the paths a reviewer would expect to be covered and are not, and say
whether each gap is worth closing now or is acceptable. Recommend PASS or MORE
TESTS NEEDED on its own line.

{{.WorkInProgressBlock}}

{{.AnswersBlock}}

{{.AskGuidance}}`,

	issue.PromptVerifyImpl: `Summarize the verification run for the pull request body: what was run, what
passed, and anything a reviewer should still check by hand. Keep it to a short
markdown section — it is read by a human deciding whether to merge.

{{.WorkInProgressBlock}}

{{.AnswersBlock}}`,
}
