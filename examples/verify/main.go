// verify is a minimal flow binary: one step that runs `go test ./...` in
// the worktree and completes with a markdown artifact carrying the output.
//
// Build:   go build -o verify ./examples/verify
// Use:     ./verify doctor
//
//	./verify list
//	./verify claim 42
//	./verify run
//
// Issues must carry a `type:task` label and `flow:verify` label (see
// docs/design.md for the full label vocabulary).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/claude"
	"github.com/promise-language/flow/cli"
	ghorch "github.com/promise-language/flow/pkg/orchestrator/github"
)

func main() {
	backend, err := ghorch.New(ghorch.Config{
		BinaryName: "verify",
		VerifyCmd:  []string{"go", "test", "./..."},
		// No guard, so this binary publishes nothing: `doctor` and `list`
		// work, and `claim` — the first write — refuses. See examples/issue
		// and docs/disclosure.md.
		Guard: nil,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify: backend init:", err)
		os.Exit(1)
	}

	verifyFlow := flow.NewFlow("verify", []flow.ItemType{"task"})
	// One step, so it is the entry and the only way the item can end: it
	// finalizes. A handler elects its route, and a graph with no election that
	// ends it is a graph that never finishes.
	verifyFlow.AddStep("run go test", "test-output", stepRunTests, flow.StepConfig{
		Entry:       true,
		MayFinalize: []flow.Disposition{flow.DispositionResolved},
	})

	os.Exit(cli.Run(cli.App{
		Name:         "verify",
		Orchestrator: backend,
		Agent:        claude.New(), // unused by this flow, but required by cli.App
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("test-output", flow.ArtifactMarkdown),
		},
		Flow: verifyFlow,
		// What the run may spend is the binary's policy, keyed by step id —
		// never a step declaration. Everything unnamed takes the defaults.
		StepBudgets: map[flow.StepId]flow.StepBudget{
			"test-output": {Timeout: 5 * time.Minute},
		},
	}))
}

func stepRunTests(ctx flow.StepCtx) (flow.StepResult, error) {
	out, err := exec.CommandContext(ctx.Context(), "go", "test", "./...").CombinedOutput()
	body := "```\n" + strings.TrimRight(string(out), "\n") + "\n```"
	if err != nil {
		// A step completes with its result and its route TOGETHER, so there is
		// no "record the artifact, then fail": a run that failed has no result
		// to record. The evidence travels in the failure instead, which is
		// where a reader looks for it.
		return flow.StepResult{}, fmt.Errorf("go test failed: %w\n%s", err, body)
	}
	return ctx.Finalize(flow.DispositionResolved, "go test passes on the item's branch").
		Markdown(body), nil
}
