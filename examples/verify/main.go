// verify is a minimal flow binary: one step that runs `go test ./...` in
// the worktree and resolves a markdown artifact with the output.
//
// Build:   go build -o verify ./examples/verify
// Use:     ./verify doctor
//          ./verify list
//          ./verify claim 42
//          ./verify run
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
	ghbackend "github.com/promise-language/flow/pkg/backend/github"
)

func main() {
	backend, err := ghbackend.NewBackend(ghbackend.Config{
		BinaryName: "verify",
		VerifyCmd:  []string{"go", "test", "./..."},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify: backend init:", err)
		os.Exit(1)
	}

	verifyFlow := flow.NewFlow("verify", []flow.ItemType{"task"})
	verifyFlow.AddStep("run go test", "test-output", stepRunTests,
		flow.Required,
		flow.Timeout(5*time.Minute),
	)

	os.Exit(cli.Run(cli.App{
		Name:    "verify",
		Backend: backend,
		Agent:   claude.New(), // unused by this flow, but required by cli.App
		Artifacts: []flow.ArtifactDef{
			flow.Artifact("test-output", flow.ArtifactMarkdown),
		},
		Flows: []*flow.Flow{verifyFlow},
	}))
}

func stepRunTests(ctx flow.StepCtx) error {
	out, err := exec.CommandContext(ctx.Context(), "go", "test", "./...").CombinedOutput()
	body := "```\n" + strings.TrimRight(string(out), "\n") + "\n```"
	if err != nil {
		// Resolve with the failure output so the issue gets the evidence,
		// then return an error to mark the step failed for this invocation.
		_ = ctx.ResolveMarkdown(body + "\n\n_go test exited with error: " + err.Error() + "_")
		return fmt.Errorf("go test failed: %w", err)
	}
	return ctx.ResolveMarkdown(body)
}
