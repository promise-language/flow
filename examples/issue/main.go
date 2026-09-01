// issue is the reference consumer of the flow/issue lifecycle library.
//
// It is deliberately thin, and that is the whole demonstration: a project
// adopting this lifecycle supplies configuration and prompt bodies, not step
// handlers. Everything structural — which steps exist, the implement step's
// verify-fix loop, role-based step selection, park-for-answer — lives in
// github.com/promise-language/flow/issue and is shared with every other
// consumer, so a fix there reaches all of them instead of being re-forked.
//
// Copy this file as the starting point for a real binary. The part worth
// replacing is prompts.go: the bodies here are generic on purpose, and a
// project's own build commands, pipeline stages and policies are exactly what
// this library cannot supply for it.
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
	"context"
	"fmt"
	"os"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/claude"
	"github.com/promise-language/flow/cli"
	"github.com/promise-language/flow/issue"
	ghbackend "github.com/promise-language/flow/pkg/backend/github"
)

func main() {
	ctx := context.Background()

	backend, err := ghbackend.NewBackend(ghbackend.Config{
		BinaryName:  "issue",
		VerifyCmd:   verifyCmd,
		DefaultType: "task",
		// No guard, so this binary publishes NOTHING: `list`, `status` and
		// `doctor` work, and the first write refuses. That is the fail-closed
		// rule in docs/disclosure.md working, not a gap to patch here — the
		// SDK ships no implementation on purpose, because a guard living in
		// the tree it constrains is rebuildable by the party it refuses. A
		// real binary is handed one from wherever it is supplied.
		Guard: nil,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue: backend init:", err)
		os.Exit(1)
	}

	app, err := issue.BuildApp(ctx, issue.Config{
		BinaryName:  "issue",
		VerifyCmd:   verifyCmd,
		DefaultType: "task",
		Prompts:     prompts,
		Budgets: map[issue.StepID]flow.StepBudget{
			// The implement step is the expensive one: it loops against the
			// gate, so it needs a longer deadline than a single agent turn.
			// The other steps take the package defaults.
			issue.StepImplement: {Timeout: 60 * time.Minute},
		},
		// Pinned to the contributor set rather than detected. Detection would
		// route anyone with admin — which is anyone running this on their own
		// repository — to the maintainer set, and that set is not implemented
		// yet, so every run would refuse. Pinning also means no probe at
		// startup, so `doctor` works with a broken token, which is the whole
		// point of `doctor`.
		//
		// Drop this line to detect the role once the maintainer set lands.
		Role: issue.RoleContributor,
		// BaseBranch is left unset: it is detected from the repository, which
		// is right for any repo whose default branch is the merge target. Set
		// it only when cutting from something else.
	}, issue.Deps{
		Backend: backend,
		Agent:   claude.New(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue:", err)
		os.Exit(1)
	}

	os.Exit(cli.Run(app))
}

// verifyCmd is the gate every implement round has to make pass.
var verifyCmd = []string{"bin/verify"}
