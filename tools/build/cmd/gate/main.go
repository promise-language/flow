// Command gate measures one property of this tree and prints what it found.
//
// It is not meant to be run by hand — `bin/run <gate>` is that path. A gate
// answers a runner, and a runner is the only caller that can say what became
// of the run: a gate killed for memory is not alive to report it, and a gate
// that exited cleanly having printed nothing would be believed.
package main

import (
	"encoding/json"
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
	sb.WriteString("gate — measure one property of this tree.\n\n")
	sb.WriteString("Usage:\n  gate <name> --envelope\n\n")
	sb.WriteString("Prints one JSON envelope on stdout and nothing else. Without --envelope it\n")
	sb.WriteString("prints nothing and fails: a bare run that printed measurements and exited 0\n")
	sb.WriteString("would be read as a pass by the first script that wrapped it, and a gate has\n")
	sb.WriteString("no verdict to give. Run `run <name>` for a result meant for a person.\n\n")
	sb.WriteString("Gates:\n")
	for _, n := range common.GateNames() {
		fmt.Fprintf(&sb, "  %-12s %s\n", n, common.GateSummary(n))
	}
	return sb.String()
}

// fail prints to stderr and exits non-zero, leaving stdout untouched. Every
// path out of this program that is not a complete envelope comes through here.
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gate: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	args := common.NormalizeArgs(os.Args[1:])

	// Help goes to STDERR and exits non-zero, where every other tool here
	// prints it to stdout and exits 0. Stdout carries the envelope and nothing
	// else — a caller redirecting stdout to a parser must get an envelope or
	// nothing, and "nothing" must not look like success.
	if common.HasHelpFlag(args) {
		fmt.Fprint(os.Stderr, usage())
		os.Exit(1)
	}

	// An unknown name is refused rather than guessed at: a runner asking for a
	// gate this project does not have must learn that, not receive an empty
	// measurement that reads like a clean result.
	name, envelope, err := common.ParseGateArgs(args)
	if err != nil {
		fail("%v; run `%s -h` for usage", err, os.Args[0])
	}
	if !envelope {
		fail("refusing to measure without --envelope; run `run %s` for a result "+
			"meant for a person", name)
	}

	// Stale logic would measure this tree with yesterday's gates and print a
	// well-formed envelope about it, which is the one failure nothing
	// downstream could detect.
	if reason := common.StaleReason(repoRoot, sourceHash); reason != "" {
		fail("%s — run %s", reason, common.MakeCmd())
	}

	env, err := common.MeasureGate(repoRoot, name)
	if err != nil {
		// Nothing was measured. No envelope, because a partial one is not a
		// measurement and must not parse as one.
		fail("%v", err)
	}

	// The envelope is written whole, in one write. A run killed part-way
	// leaves output that does not parse, which is how a reader tells "measured
	// nothing" from "measured and reported" without asking the gate.
	out, err := json.Marshal(env)
	if err != nil {
		fail("could not encode the envelope: %v", err)
	}
	os.Stdout.Write(append(out, '\n'))
}
