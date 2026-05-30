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

func main() {
	common.CheckStale(repoRoot, sourceHash)
	if err := common.RunVerify(repoRoot, common.NormalizeArgs(os.Args[1:])); err != nil {
		// RunVerify already printed the ❌ banner; exit non-zero silently so it
		// stays the last line of output.
		os.Exit(1)
	}
}
