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
	sb.WriteString("Usage:\n  gate <name> --envelope\n  gate --list\n\n")
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

// isListArg reports whether the argv asks for the gate list and nothing else.
// Exactly one argument: a request with anything alongside it is ambiguous
// between listing and measuring, and guessing would print a list to a caller
// waiting for an envelope.
func isListArg(args []string) bool {
	return len(args) == 1 && (args[0] == "--list" || args[0] == "-list" || args[0] == "list")
}

// fail prints to stderr and exits non-zero, leaving stdout untouched. Every
// path out of this program that is not a complete envelope comes through here.
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gate: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	args := common.NormalizeArgs(os.Args[1:])

	// --list answers "which gates does this project have?", one name per line
	// on stdout, exit 0.
	//
	// It is the only mode besides a measurement that writes to stdout, and it
	// does not break the envelope rule: a caller that asked for the list did
	// not ask for a measurement, and a name-per-line stream cannot be mistaken
	// for an envelope by anything that parses one.
	//
	// It exists because an orchestrator must not hold a second copy of what
	// this project can measure. Asking the entry point is the only way to
	// learn it that cannot go stale — and an entry point that is absent or
	// cannot answer reports a project with no gates, which is the truth about
	// this machine and exactly what `doctor` needs to say.
	if isListArg(args) {
		for _, n := range common.GateNames() {
			fmt.Println(n)
		}
		return
	}

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
