package main

import (
	"os"

	"github.com/promise-language/flow/tools/build/common"
)

// Injected by the meta-builder via -ldflags at build time; empty otherwise.
var (
	repoRoot   = ""
	sourceHash = ""
)

const usage = `verify — the commit gate.

Usage:
  verify [-h | -help]

Runs gofmt, go vet, go build, and go test over the module, printing a
pass/FAIL summary. Exit 0 ("✅ OK to Commit") means safe to commit; non-zero
("❌ Verify FAILED") means not.`

func main() {
	common.MaybeHelp(os.Args[1:], usage)
	common.CheckStale(repoRoot, sourceHash)
	if err := common.RunVerify(repoRoot, common.NormalizeArgs(os.Args[1:])); err != nil {
		// RunVerify already printed the ❌ banner; exit non-zero silently so it
		// stays the last line of output.
		os.Exit(1)
	}
}
