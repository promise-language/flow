// Command run asks one gate for a measurement and reaches a verdict on it.
//
// This is the by-hand path, and it takes the same route a runner takes rather
// than a parallel one: it executes bin/gate as a process and reads what came
// back. Running a single gate is not a lesser case — it is faster than
// everything that blocks a change from landing, and it is what someone
// iterating on one failure actually wants.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/promise-language/flow/tools/build/common"
)

// Injected by the meta-builder via -ldflags at build time; empty otherwise.
var (
	repoRoot   = ""
	sourceHash = ""
)

func usage() string {
	var sb strings.Builder
	sb.WriteString("run — measure one gate and judge what it measured.\n\n")
	sb.WriteString("Usage:\n  run <gate> [-h | -help]\n\n")
	sb.WriteString("Runs bin/gate <gate> --envelope, then prints each measurement beside the\n")
	sb.WriteString("term it was judged on. Exit 0 means every capped measurement is within its\n")
	sb.WriteString("cap; non-zero means one is not, or that nothing could be measured.\n\n")
	sb.WriteString("Gates:\n")
	for _, n := range common.GateNames() {
		fmt.Fprintf(&sb, "  %-12s %s\n", n, common.GateSummary(n))
	}
	fmt.Fprintf(&sb, "\nJudged against a cap: %s\n", strings.Join(common.CappedMetrics(), ", "))
	sb.WriteString("Anything else is reported and not judged.\n")
	return sb.String()
}

func main() {
	args := common.NormalizeArgs(os.Args[1:])
	if common.HasHelpFlag(args) {
		fmt.Print(usage())
		os.Exit(0)
	}
	common.CheckStale(repoRoot, sourceHash)

	var name string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "run: use of unknown flag %q; run `%s -h` for usage\n", a, os.Args[0])
			os.Exit(2)
		case name != "":
			fmt.Fprintf(os.Stderr, "run: unexpected argument %q; one gate at a time\n", a)
			os.Exit(2)
		default:
			name = a
		}
	}
	if name == "" {
		fmt.Fprintf(os.Stderr, "run: no gate named; run `%s -h` for usage\n", os.Args[0])
		os.Exit(2)
	}

	if err := common.RunOneGate(repoRoot, common.GateBinary(repoRoot), name); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
}
