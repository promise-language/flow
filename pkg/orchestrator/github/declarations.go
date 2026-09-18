package github

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// What this orchestrator can run is READ FROM THE MACHINE, not written down
// here. A hardcoded list is a claim about intentions: it says `verify` is
// available on a checkout that never built it, and `fit` on a machine with no
// gate entry point at all — so every caller that trusted the list discovers the
// truth mid-item, at the point of use, having already spent whatever it took to
// get there. `doctor` in particular could then report a machine fit on the
// strength of a list it printed back to itself.
//
// Derived, the same lists answer honestly: `verify` missing from
// SupportedCommands() means the verify command is not on this machine, and
// `fit` missing from SupportedGates() means the gate entry point is absent or
// could not say what it supports. Nothing needs to hold a second copy of the
// project's layout, and nothing goes stale.
//
// Both are resolved ONCE per orchestrator (the fields on the struct). A
// binary's answer to "what can I run" must not change under it mid-run — the
// refusal a gate-running command meets and `doctor` would otherwise disagree
// about the same machine — and each answer costs a directory read or a process
// spawn.

// commandDir is where this project keeps its commands: one main per command,
// which is what makes the directory listing the answer. bin/ is not that list —
// it holds every built tool, including ones that are not commands at all.
var commandDir = filepath.Join("tools", "build", "cmd")

// gateListArgs asks the gate entry point which gates this project has. The
// entry point is the only party that knows, and asking it is the only way to
// learn that cannot drift from what a run would actually find.
//
// THE MACHINE-READABLE FORM IS ASKED FOR RATHER THAN ASSUMED. A project tool
// renders its result for whoever is reading it — one bare name per line at a
// terminal, JSON when stdout is not one (docs/org/cli-guide.md § 6) — and a
// listing a program depends on must not rest on that detection going its way.
// The human form is a rendering: its labels, its columns, and the very choice
// of one name per line are free to improve whenever they read better. The JSON
// is the interface, and it is the one that promises to evolve additively.
//
// Leaning on the detection is how this broke. The query's stdout is a pipe, so
// an entry point obliging a program it could not see handed back a JSON object
// — and every line of it failed to parse as a gate name, leaving an empty list
// no caller could tell from an entry point that was never built.
var gateListArgs = []string{"--list", "--json"}

// gateListLegacyArgs is the same question without the flag, asked only when
// asking with it failed.
//
// An entry point predating --json refuses the flag and exits non-zero, and
// that refusal must not read as "this machine has no gates": the cost of
// getting this wrong is paid silently, by an operator told to build tools that
// are already built and answering.
//
// Transitional, and its removal is #414: it goes when every project's entry
// point takes --json.
var gateListLegacyArgs = []string{"--list"}

// declarationTimeout bounds the list query. It is short on purpose: this runs
// before any work, and a gate entry point that cannot say what it supports
// within a couple of seconds is not one a step should be dispatched against.
// Timing out reports NO gates, which is the honest reading — nothing was
// learned — and `doctor` says so on the gates line.
//
// It bounds the landing read as well as the startup one, and one constant is
// the point: a check made against a change asks the same question with the same
// patience as the check made before it, or the two answers are not comparable.
const declarationTimeout = 5 * time.Second

// SupportedCommands lists the commands this machine actually has: one
// subdirectory per command under tools/build/cmd, filtered to the three names
// the contract defines. A directory holding anything else is not a command as
// far as this contract is concerned, and is passed over rather than declared.
func (b *Orchestrator) SupportedCommands() []flow.CommandDef {
	b.commandsOnce.Do(func() { b.commandsList = discoverCommands(b.cfg.WorktreeDir) })
	return b.commandsList
}

func discoverCommands(root string) []flow.CommandDef {
	entries, err := os.ReadDir(filepath.Join(root, commandDir))
	if err != nil {
		// No command directory is a machine with no commands. It is not an
		// error to report here: SupportedCommands has no way to return one,
		// and an empty list is exactly what the caller needs to know.
		return nil
	}
	var out []flow.CommandDef
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if name := flow.CommandName(e.Name()); name.Valid() {
			out = append(out, flow.Command(name))
		}
	}
	slices.SortFunc(out, func(a, c flow.CommandDef) int { return strings.Compare(string(a.Name), string(c.Name)) })
	return out
}

// SupportedGates asks the gate entry point what it supports, and reads the
// listing it writes back.
//
// `integration` and `fit` are marked required when they come back — the
// contract requires an orchestrator to have them, and this one reports what it
// found rather than asserting what should be there. When they do not come back,
// they are simply absent from the list, which is what `doctor` reports and what
// `claim`, `run-step` and `resolve` refuse to proceed on.
func (b *Orchestrator) SupportedGates() []flow.GateDef {
	b.gatesOnce.Do(func() { b.gatesList = DiscoverGates(context.Background(), b.cfg.WorktreeDir) })
	return b.gatesList
}

// DiscoverGates asks root's gate entry point which gates it has, and reads the
// listing it writes back. Exported because the read has a SECOND caller and
// must not be reimplemented for it.
//
// SupportedGates answers what this machine could run when the resolution
// started, and memoises it — a binary's answer to "what can I run" must not
// change under it mid-run. But the entry point that answers this query is a
// file in the tree, and a resolution may edit it, so the last state anything
// verified is the state before the change. A caller holding a tree it is about
// to land calls THIS: the same entry point, the same query, the same deadline,
// read again against the tree as it will land, with nothing served from the
// memo (docs/resolution.md § A change that removes a declaration does not land).
//
// It is a plain function over a checkout path because the callers that need it
// are not all orchestrators: a graph step's landing check, and — outside this
// SDK — whatever installs a release into a checkout and must report a checkout
// it has just made undriveable. flow.CheckGatesHeld does the comparison.
//
// The entry point is a BUILT ARTIFACT. A listing read from a binary built from
// the previous tree answers about the previous tree and reads as a pass, so a
// caller checking a change takes this read after that tree's tools are built.
//
// ctx bounds the caller's patience; declarationTimeout bounds this SDK's, and
// whichever expires first ends the query. A cancelled ctx reports no gates,
// which is the same honest reading an entry point that said nothing gets.
func DiscoverGates(ctx context.Context, root string) []flow.GateDef {
	// One deadline covers both attempts. What is bounded is how long this SDK
	// waits to learn what a machine can run, not how many ways it asks.
	ctx, cancel := context.WithTimeout(ctx, declarationTimeout)
	defer cancel()

	out, answered := askGateList(ctx, root, gateListArgs)
	if !answered {
		out, answered = askGateList(ctx, root, gateListLegacyArgs)
	}
	if !answered {
		// An absent, unexecutable or silent entry point is a machine with no
		// gates — which is what a checkout whose tools are not yet built looks
		// like. Every caller reads that correctly: the commands that would run
		// a gate refuse, `doctor` reports it, the read-only commands are
		// unaffected, and nothing dispatches a step that would have failed at
		// its first measurement.
		return nil
	}

	var gates []flow.GateDef
	for _, listed := range listedGates(out) {
		if !listed.Name.Valid() {
			// A name this SDK does not know is not a gate it can ask for. It
			// is skipped rather than refused: the entry point is free to have
			// gates of its own, and this list is what the SDK can address.
			continue
		}
		def := flow.Gate(listed.Name, slices.Contains(flow.RequiredGates(), listed.Name))
		def.HostScope = listed.HostScope && hostScopeAllowed(listed.Name)
		gates = append(gates, def)
	}
	slices.SortFunc(gates, func(a, c flow.GateDef) int { return strings.Compare(string(a.Name), string(c.Name)) })
	return gates
}

// askGateList runs the entry point with one set of arguments and returns what
// it wrote.
//
// An error means it was absent, could not run, refused the arguments, or was
// killed at the deadline. In every one of those it has not said what it
// supports, and reading a list out of a diagnostic would declare gates on the
// strength of an error message.
func askGateList(ctx context.Context, root string, args []string) ([]byte, bool) {
	argv := append(append([]string{}, gateEntryPoint...), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// gateListing is the machine-readable form of the listing. Two fields are read:
// the NAME, which is how a gate is addressed, and HOST_SCOPE, which is where a
// project declares that this gate must hold the host-scope exclusion while it
// runs (docs/gates-and-commands.md § Two scopes). The listing carries a summary
// per gate as well, for a person reading it; anything it grows later is ignored
// here rather than refused, which is what reading an additive interface means.
//
// ABSENT READS AS FALSE, which is what makes the field additive rather than a
// flag day: every entry point already in circulation declares no host scope,
// and that is the correct reading of them — a project that never said its
// suites contend has not said it.
type gateListing struct {
	Gates []listedGate `json:"gates"`
}

// listedGate is one row of the listing, in whichever form it arrived.
type listedGate struct {
	Name      flow.GateName `json:"name"`
	HostScope bool          `json:"host_scope"`
}

// listedGates reads the rows out of whichever form came back.
//
// Both are accepted for as long as both are in circulation, and the parser is
// the half that cannot be dropped on a flag day: an entry point that refuses
// --json may still answer the bare query in JSON, because its own stdout is a
// pipe either way. What retires with gateListLegacyArgs is the second spawn
// (#414), not the ability to read a line.
//
// THE BARE FORM CARRIES NO FIELDS, so every row it yields declares no host
// scope. That is not a loss to work around: a rendering of one name per line
// has nowhere to put a declaration, and inventing one would mean guessing which
// of a project's gates are heavy — the guess the whole mechanism exists to stop
// anyone making.
func listedGates(out []byte) []listedGate {
	var listing gateListing
	if err := json.Unmarshal(out, &listing); err == nil {
		return listing.Gates
	}
	var rows []listedGate
	for _, line := range strings.Split(string(out), "\n") {
		if name := flow.GateName(strings.TrimSpace(line)); name != "" {
			rows = append(rows, listedGate{Name: name})
		}
	}
	return rows
}

// hostScopeAllowed reports whether a gate may declare host scope at all.
//
// `fit` may not, and the refusal is here rather than left to a project's good
// sense because the cost of getting it wrong is silent and total:
// docs/gates-and-commands.md § Two scopes states that `fit` is asked BECAUSE
// the machine may be busy, and before an item is taken. A `fit` that queued
// behind a suite could not answer the question it exists for, and every arena
// on a working machine would wait to be told the machine works — with the wait
// reported as fitness, which is the one reading that cannot be debugged from.
//
// It is keyed on the CONCEPT, so `fit:disk` and `fit:services` are covered:
// instances are the project's and the concept is what carries the meaning.
func hostScopeAllowed(name flow.GateName) bool { return name.Concept() != flow.GateFit }
