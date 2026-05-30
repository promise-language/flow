package main

import (
	"os"

	"github.com/promise-language/flow/tools/build/common"
)

var (
	repoRoot   = ""
	sourceHash = ""
)

const usage = `precommit — the pre-commit gate.

Usage:
  precommit [-h | -help]

Normally invoked by .githooks/pre-commit, not by hand. Rejects staged
binaries under bin/ and any commit whose author or committer email is not a
@users.noreply.github.com address.`

func main() {
	common.MaybeHelp(os.Args[1:], usage)
	// CheckStale first: the commit gate is only meaningful if it runs the
	// current logic. If the tools are out of sync, this refuses the commit and
	// points at ./make — you must rebuild (and therefore fix any broken tool)
	// before committing. That is what keeps a broken bin/precommit from ever
	// being committed into an unrecoverable state. Recovery is ./make (ungated,
	// 'go run'), never a commit, so blocking here is a speed bump, not a trap.
	common.CheckStale(repoRoot, sourceHash)
	if err := common.RunPrecommit(repoRoot); err != nil {
		os.Exit(1)
	}
}
