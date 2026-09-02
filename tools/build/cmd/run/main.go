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
	sb.WriteString("Usage:\n  run <gate> [-h | -help]\n  run <gate> --verdict < envelope\n\n")
	sb.WriteString("Runs bin/gate <gate> --envelope, then prints each measurement beside the\n")
	sb.WriteString("term it was judged on. Exit 0 means every capped measurement is within its\n")
	sb.WriteString("cap; non-zero means one is not, or that nothing could be measured.\n\n")
	sb.WriteString("With --verdict it judges an envelope it is GIVEN, on stdin, and runs no\n")
	sb.WriteString("gate: it prints one JSON verdict on stdout and nothing else. That is the\n")
	sb.WriteString("mode the SDK asks — the SDK spawns the gate, because a judge that ran its\n")
	sb.WriteString("own measurement would be the runner, and the runner comes from outside the\n")
	sb.WriteString("tree.\n\n")
	sb.WriteString("Gates:\n")
	for _, n := range common.GateNames() {
		fmt.Fprintf(&sb, "  %-12s %s\n", n, common.GateSummary(n))
	}
	capped := common.CappedMetrics(repoRoot)
	if len(capped) > 0 {
		fmt.Fprintf(&sb, "\nJudged against a cap: %s\n", strings.Join(capped, ", "))
	} else {
		fmt.Fprintf(&sb, "\nThresholds defined in %s\n", common.ManifestFile)
	}
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

	name, verdict, err := common.ParseRunArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: %v; run `%s -h` for usage\n", err, os.Args[0])
		os.Exit(2)
	}

	// The judging mode. Nothing is spawned: the envelope arrives on stdin from
	// whoever ran the gate, and stdout carries one verdict and nothing else.
	// CheckStale has already run above, so stale tooling exits before it can
	// print a verdict rather than answering with terms nobody currently holds.
	if verdict {
		if err := common.JudgeStdin(repoRoot, name, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "run: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := common.RunOneGate(repoRoot, common.GateBinary(repoRoot), name); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
}
