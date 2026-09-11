// issue is the reference consumer of the flow/issue lifecycle library.
//
// It is deliberately thin, and that is the whole demonstration: a project
// adopting this lifecycle supplies configuration and prompt bodies, not step
// handlers. Everything structural — which steps exist, the implement step's
// verify-fix loop, the role coverage gate, park-for-answer — lives in
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
	ghorch "github.com/promise-language/flow/pkg/orchestrator/github"
)

func main() {
	ctx := context.Background()

	backend, err := ghorch.New(ghorch.Config{
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
		// COVERAGE: which of the one graph's steps this binary may perform. It
		// is declared, never derived from what the account can do — holding
		// the capability to merge is not the same as intending to, and anyone
		// running this on their own repository has admin. The contributor's
		// coverage takes an item to a proposal and hands off; add
		// issue.RoleMaintainer to cover both sides and carry an item through to
		// a merge, which `resolve` announces is not independent review before
		// it crosses. A covered role the account cannot back is a handoff, not
		// a misconfiguration, and nothing is probed at startup: `doctor` reports
		// what each role's standing is, and works with a broken token.
		Coverage: []issue.Role{issue.RoleContributor},
		// BaseBranch is left unset: it is detected from the repository, which
		// is right for any repo whose default branch is the merge target. Set
		// it only when cutting from something else.
	}, issue.Deps{
		Orchestrator: backend,
		Agent:        claude.New(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue:", err)
		os.Exit(1)
	}

	os.Exit(cli.Run(app))
}

// verifyCmd is the gate every implement round has to make pass.
var verifyCmd = []string{"bin/verify"}
