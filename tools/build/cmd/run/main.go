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
	"github.com/promise-language/forge/primitives"
)

// Injected by the meta-builder via -ldflags at build time; empty otherwise.
var (
	repoRoot   = ""
	sourceHash = ""
)

func usage() string {
	var sb strings.Builder
	sb.WriteString("run — measure one gate and judge what it measured.\n\n")
	sb.WriteString("Usage:\n  run <gate> [-h | -help]\n  run <gate> --verdict < envelope\n  run --list [--json | --human]\n\n")
	sb.WriteString("Runs bin/gate <gate> --envelope, then prints each measurement beside the\n")
	sb.WriteString("term it was judged on. Exit 0 means every capped measurement is within its\n")
	sb.WriteString("cap; non-zero means one is not, or that nothing could be measured.\n\n")
	sb.WriteString("With --verdict it judges an envelope it is GIVEN, on stdin, and runs no\n")
	sb.WriteString("gate: it prints one JSON verdict on stdout and nothing else. That is the\n")
	sb.WriteString("mode the SDK asks — the SDK spawns the gate, because a judge that ran its\n")
	sb.WriteString("own measurement would be the runner, and the runner comes from outside the\n")
	sb.WriteString("tree.\n\n")
	sb.WriteString("With --list it answers what this project builds and what it can be asked\n")
	sb.WriteString("to measure, as two labelled groups for a person and as one object for\n")
	sb.WriteString("anything reading it. The query takes no argument: it is what the project\n")
	sb.WriteString("builds, never what is currently built, so it answers in a clone where\n")
	sb.WriteString("nothing has been built yet.\n\n")
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
	args := primitives.NormalizeArgs(os.Args[1:])
	if primitives.HasHelpFlag(args) {
		fmt.Print(usage())
		os.Exit(0)
	}
	primitives.CheckStale(repoRoot, sourceHash)

	// The discovery query, answered before the single-name rule below, because
	// it names nothing: refusing it for having no gate would read as a project
	// with nothing to run.
	//
	// After CheckStale, and for the reason every other mode is: the entry point
	// is a built artifact, and a listing printed by a binary built from an
	// earlier tree describes that tree and reads as an answer about this one.
	if asked, of, err := common.ParseListArgs(args); asked {
		if err != nil {
			fmt.Fprintf(os.Stderr, "run: %v; run `%s -h` for usage\n", err, os.Args[0])
			os.Exit(2)
		}
		mode, err := of.Mode()
		if err != nil {
			fmt.Fprintf(os.Stderr, "run: %v\n", err)
			os.Exit(2)
		}
		list, err := common.CollectBuildList(repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run: %v\n", err)
			os.Exit(1)
		}
		if err := common.WriteBuildList(os.Stdout, mode, list); err != nil {
			fmt.Fprintf(os.Stderr, "run: %v\n", err)
			os.Exit(1)
		}
		return
	}

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
