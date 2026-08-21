// issue is the reference contributor+maintainer flow binary. It models the
// full lifecycle of taking a GitHub issue from triage to merged PR:
//
//	fix flow (contributor, no preconditions):
//	  write plan          → plan artifact (markdown)
//	  implement the change → implementation artifact (patch)
//	  review the work     → review artifact (markdown)
//	  analyze coverage    → coverage artifact (markdown)
//	  verify              → verify-impl artifact (markdown)
//	  create pull request → pr-open signal (handler: gh pr create)
//
//	merge flow (maintainer, gated on pr-open):
//	  review the implementation → review-maint artifact (markdown)
//	  verify                    → verify-merge artifact (markdown)
//	  merge pull request        → pr-merged signal (handler: gh pr merge)
//	  record merge commit       → merge-commit artifact (commit hash)
//
// Build:   go build -o issue ./examples/issue
// Use:     ./issue doctor
//
//	./issue claim 42
//	./issue run-step    # one lifecycle item per invocation
//
// Issues must carry `type:task` (or `type:bug`) + the `flow:issue` label +
// an assignee for this binary to claim them.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/claude"
	"github.com/promise-language/flow/cli"
	ghbackend "github.com/promise-language/flow/pkg/backend/github"
)

func main() {
	backend, err := ghbackend.NewBackend(ghbackend.Config{
		BinaryName:  "issue",
		VerifyCmd:   []string{"bash", "bin/verify.sh"},
		DefaultType: "task",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue: backend init:", err)
		os.Exit(1)
	}

	artifacts := []flow.ArtifactDef{
		flow.Artifact("plan", flow.ArtifactMarkdown),
		flow.Artifact("implementation", flow.ArtifactPatch),
		flow.Artifact("review", flow.ArtifactMarkdown),
		flow.Artifact("coverage", flow.ArtifactMarkdown),
		flow.Artifact("verify-impl", flow.ArtifactMarkdown),
		flow.Artifact("review-maint", flow.ArtifactMarkdown),
		flow.Artifact("verify-merge", flow.ArtifactMarkdown),
		flow.Artifact("merge-commit", flow.ArtifactCommitHash),
	}
	signals := []flow.SignalDef{
		flow.Signal("pr-open", "PR for the claim branch is open"),
		flow.Signal("pr-merged", "PR has been merged"),
	}

	// Contributor flow.
	contributor := flow.NewFlow("fix", []flow.ItemType{"task", "bug"})
	contributor.AddStep("write plan", "plan", stepPlan, flow.StepConfig{})
	contributor.AddStep("implement the change", "implementation", stepImplementation, flow.StepConfig{
		Budget: flow.StepBudget{MaxPromptsPerInvocation: 5, Timeout: 60 * time.Minute},
	})
	contributor.AddStep("review the work", "review", stepReview, flow.StepConfig{})
	contributor.AddStep("analyze coverage", "coverage", stepCoverage, flow.StepConfig{})
	contributor.AddStep("verify", "verify-impl", stepVerify, flow.StepConfig{})
	contributor.AddSignalStep("create pull request", "pr-open", stepCreatePR, flow.StepConfig{})

	// Maintainer flow — only eligible once contributor is done.
	maintainer := flow.NewFlow("merge", []flow.ItemType{"task", "bug"})
	maintainer.RequireSignal("pr-open")
	maintainer.AddStep("review the implementation", "review-maint", stepMaintReview, flow.StepConfig{})
	maintainer.AddStep("verify", "verify-merge", stepVerify, flow.StepConfig{})
	maintainer.AddSignalStep("merge pull request", "pr-merged", stepMerge, flow.StepConfig{})
	maintainer.AddStep("record merge commit", "merge-commit", stepRecordMerge, flow.StepConfig{})

	os.Exit(cli.Run(cli.App{
		Name:      "issue",
		Backend:   backend,
		Agent:     claude.New(),
		Artifacts: artifacts,
		Signals:   signals,
		Flows:     []*flow.Flow{contributor, maintainer},
	}))
}

// stepPlan drafts a high-level plan for the issue using the agent.
func stepPlan(ctx flow.StepCtx) error {
	resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
		Prompt: fmt.Sprintf(
			"Issue: %s\n\n%s\n\nProduce an implementation plan as concise markdown.",
			ctx.Item().Title, ctx.Item().Body,
		),
		PermissionMode: "plan",
		Effort:         "high",
	})
	if err != nil {
		return err
	}
	if resp == nil || resp.LastText == "" {
		return fmt.Errorf("agent returned empty plan")
	}
	return ctx.ResolveMarkdown(resp.LastText)
}

// stepImplementation runs the implementation pass: branches off main, lets
// the agent make edits, captures the diff as the artifact.
func stepImplementation(ctx flow.StepCtx) error {
	plan, ok := ctx.Markdown("plan")
	if !ok {
		return fmt.Errorf("plan artifact missing — flow order violated")
	}
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	itemID, err := ctx.Item().IDInt()
	if err != nil {
		return fmt.Errorf("non-numeric item id %q", ctx.Item().ID)
	}
	branch := fmt.Sprintf("flow/issue-%d", itemID)
	if _, err := wt.Branch(ctx.Context(), branch, "main"); err != nil {
		return err
	}

	if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
		Prompt:         fmt.Sprintf("Implement this plan:\n\n%s", plan),
		PermissionMode: "acceptEdits",
	}); err != nil {
		return err
	}

	patch, err := wt.CapturePatch(ctx.Context())
	if err != nil {
		return err
	}
	if len(patch) == 0 {
		return fmt.Errorf("agent made no changes — nothing to attach")
	}
	return ctx.ResolvePatch(flow.PatchBody{
		Diff:       patch,
		BaseSHA:    "HEAD~1", // placeholder; the github backend records the commit when it lands
		BaseBranch: "main",
	})
}

// stepReview asks the agent to critique the current diff (cheaper model).
func stepReview(ctx flow.StepCtx) error {
	resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
		Prompt: "Review the diff in the current branch. Flag correctness bugs, surprising behavior, " +
			"missed edge cases, and unnecessary complexity. Be specific. End with PASS or FAIL.",
		Model: "claude-sonnet-4-6",
	})
	if err != nil {
		return err
	}
	return ctx.ResolveMarkdown(resp.LastText)
}

// stepCoverage analyses test coverage of the change.
func stepCoverage(ctx flow.StepCtx) error {
	resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
		Prompt: "Analyze test coverage of the changes on the current branch. " +
			"List uncovered paths and recommend whether more tests are required.",
		Model: "claude-sonnet-4-6",
	})
	if err != nil {
		return err
	}
	return ctx.ResolveMarkdown(resp.LastText)
}

// stepVerify runs the project's verify command. Used by both flows.
func stepVerify(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Validate(ctx.Context()); err != nil {
		return err
	}
	return ctx.ResolveMarkdown("verify passed at " + time.Now().Format(time.RFC3339))
}

// stepCreatePR opens the PR. Signal pr-open is set by the backend as a
// side-effect of Open succeeding.
func stepCreatePR(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	itemID, _ := ctx.Item().IDInt()
	_, err = flow.Open(
		ctx.Context(),
		wt,
		"main",
		fmt.Sprintf("feat: %s", ctx.Item().Title),
		fmt.Sprintf("Closes #%d", itemID),
	)
	return err
}

// stepMaintReview is the maintainer's second-opinion review of the PR.
func stepMaintReview(ctx flow.StepCtx) error {
	resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
		Prompt: "You are reviewing this PR as a maintainer. Apply a higher bar than the contributor " +
			"review. Identify any regressions or scope creep. End with APPROVE or REQUEST_CHANGES.",
		Model: "claude-opus-4-7",
	})
	if err != nil {
		return err
	}
	return ctx.ResolveMarkdown(resp.LastText)
}

// stepMerge squash-merges the open PR. pr-merged signal is set as a
// side-effect of wt.Merge succeeding.
func stepMerge(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	branch, err := wt.CurrentBranch(ctx.Context())
	if err != nil {
		return err
	}
	// In a real maintainer flow, we'd look up the PR URL via the backend;
	// for the example we pass the claim branch and trust gh CLI's
	// PR-from-branch resolution.
	return flow.Merge(ctx.Context(), wt, branch)
}

// stepRecordMerge captures the merge commit SHA. Runs after pr-merged is
// observed.
func stepRecordMerge(ctx flow.StepCtx) error {
	wt, err := ctx.Worktree()
	if err != nil {
		return err
	}
	sha, err := wt.CurrentBranch(ctx.Context()) // placeholder; production code would query git rev-parse origin/main
	if err != nil {
		return err
	}
	if sha == "" {
		return fmt.Errorf("could not determine merge commit")
	}
	return ctx.ResolveCommitHash(sha)
}
