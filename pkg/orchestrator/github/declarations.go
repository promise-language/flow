package github

import (
	"context"
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
// binary's answer to "what can I run" must not change under it mid-run —
// startup validation and `doctor` would otherwise disagree about the same
// machine — and each answer costs a directory read or a process spawn.

// commandDir is where this project keeps its commands: one main per command,
// which is what makes the directory listing the answer. bin/ is not that list —
// it holds every built tool, including ones that are not commands at all.
var commandDir = filepath.Join("tools", "build", "cmd")

// gateListArgs asks the gate entry point which gates this project has. The
// entry point is the only party that knows, and asking it is the only way to
// learn that cannot drift from what a run would actually find.
var gateListArgs = []string{"--list"}

// declarationTimeout bounds the list query. It is short on purpose: this runs
// at startup, before any work, and a gate entry point that cannot say what it
// supports within a couple of seconds is not one a step should be dispatched
// against. Timing out reports NO gates, which is the honest reading — nothing
// was learned — and `doctor` says so on the gates line.
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

// SupportedGates asks the gate entry point what it supports, one name per line.
//
// `integration` and `fit` are marked required when they come back — the
// contract requires an orchestrator to have them, and this one reports what it
// found rather than asserting what should be there. When they do not come back,
// they are simply absent from the list, which is what `doctor` reports and what
// startup validation refuses to proceed on.
func (b *Orchestrator) SupportedGates() []flow.GateDef {
	b.gatesOnce.Do(func() { b.gatesList = discoverGates(b.cfg.WorktreeDir) })
	return b.gatesList
}

func discoverGates(root string) []flow.GateDef {
	ctx, cancel := context.WithTimeout(context.Background(), declarationTimeout)
	defer cancel()

	argv := append(append([]string{}, gateEntryPoint...), gateListArgs...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		// An absent, unexecutable or silent entry point is a machine with no
		// gates. Every caller reads that correctly: startup refuses, `doctor`
		// reports it, and nothing dispatches a step that would have failed at
		// its first measurement.
		return nil
	}

	var gates []flow.GateDef
	for _, line := range strings.Split(string(out), "\n") {
		name := flow.GateName(strings.TrimSpace(line))
		if name == "" || !name.Valid() {
			// A name this SDK does not know is not a gate it can ask for. It
			// is skipped rather than refused: the entry point is free to have
			// gates of its own, and this list is what the SDK can address.
			continue
		}
		gates = append(gates, flow.Gate(name, slices.Contains(flow.RequiredGates(), name)))
	}
	slices.SortFunc(gates, func(a, c flow.GateDef) int { return strings.Compare(string(a.Name), string(c.Name)) })
	return gates
}
