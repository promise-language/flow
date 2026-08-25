package cli

import (
	"fmt"
	"slices"
	"strings"
)

// isHelpArg reports whether a single token is a help flag, accepting both flag
// prefixes (-help and --help) and the short form (-h / --h). Used both in the
// command position (`<bin> --help`) and as a subcommand flag (`<bin> claim -h`).
func isHelpArg(s string) bool {
	switch s {
	case "-h", "--h", "-help", "--help":
		return true
	}
	return false
}

// wantsHelp reports whether any argument is a help flag.
func wantsHelp(args []string) bool {
	return slices.ContainsFunc(args, isHelpArg)
}

// isKnownCommand reports whether cmd (or its alias) is a dispatchable
// subcommand. perCommandUsage is the registry — every case in RunWithArgs's
// dispatch switch must have a matching entry here (and vice versa).
func isKnownCommand(cmd string) bool {
	_, ok := perCommandUsage[canonicalCommand(cmd)]
	return ok
}

// canonicalCommand folds a command alias onto its canonical name.
func canonicalCommand(cmd string) string {
	switch cmd {
	case "lease":
		return "claim"
	case "run-all":
		return "resolve"
	}
	return cmd
}

// cmdHelp is the per-subcommand help record.
type cmdHelp struct {
	name    string
	aliases []string
	syntax  string // text after the command name, e.g. "<item-id>"
	summary string
	detail  string
}

// perCommandUsage drives `<bin> <cmd> --help`. The top-level summary lives in
// usage(); this adds focused detail for a single command.
var perCommandUsage = map[string]cmdHelp{
	"doctor": {name: "doctor", summary: "verify backend prerequisites",
		detail: "Checks that the backend is reachable and configured (auth, connectivity).\nTakes no arguments."},
	"list": {name: "list", summary: "list processable items",
		detail: "Lists the items this flow can currently process. Takes no arguments."},
	"claim": {name: "claim", aliases: []string{"lease"}, syntax: "<item-id>", summary: "acquire a claim on an item",
		detail: "Acquires a claim (lease) on <item-id> so this owner can advance it.\nTakes exactly one item id."},
	"release": {name: "release", summary: "drop the active claim",
		detail: "Releases the claim currently held by this owner. Takes no arguments."},
	"status": {name: "status", syntax: "[<item-id>]", summary: "print the lifecycle checklist",
		detail: "Prints the read-only lifecycle checklist. With <item-id>, inspects that\nitem from the backend without claiming it; without, uses the active claim."},
	"run-step": {name: "run-step", summary: "advance one lifecycle item",
		detail: "Advances exactly ONE lifecycle item against the active claim\n(one prompt → one artifact). Requires an active claim; takes no arguments."},
	"resolve": {name: "resolve", aliases: []string{"run-all"}, syntax: "[<item-id>]", summary: "run all steps to completion",
		detail: "Runs ALL steps until the item is finalized or parked. With <item-id>,\nclaims it first; otherwise uses the active claim."},
	"grant": {name: "grant", syntax: "[<step-id>] [--all] [--invocations N] [--prompts N] [--cost USD] [--timeout SECONDS] [--dry-run]", summary: "top up a step's budget",
		detail: `With no arguments: reads why the item parked and tops up the axis that
parked it, plus any other axis already at its cap — a step out of both time
and invocations needs both to run again, and topping up one alone just
re-parks on the other. This is the common case — grant is almost always a
reaction to a budget park — and it refuses (without writing) when the item
is parked on anything else, since granting budget would not unpark it.

  grant                     top up the parked step's blocked axes
  grant --all               sweep every pending step to headroom over consumption
  grant <step-id> --cost 5  additive grant to one step

<step-id> is a step's ID — the artifact id from AddStep (e.g. "plan"), which
is what "status" lists first. The human label ("write plan") is not an
identity and is refused. Signal steps own no budget and cannot be granted.

"status" prints every axis under a budget park, tagging the flat ones:

  parked: budget-exhausted on "push" (cost) — spent $11.18 ...
    axes: 3/3 inv (flat) · 2/2 prompts (flat) · $11.18/$10.00 (flat) · 0s/3h0m0s

Any axis tagged "(flat)" can be named by a flag; an axis with headroom is
refused, since granting it would not unpark anything. Bare "grant" tops up
the flat axes that block the next dispatch on its own — prompts and timeout
reset per invocation, so those two are only granted when asked for.

With no id, --invocations/--cost/--prompts/--timeout state the HEADROOM to
leave above what the step has consumed; with an id they are added to the
current caps. All values must be >= 0. --dry-run prints the plan and writes
nothing.`},
}

// commandUsage returns help for a single subcommand. For an unrecognized
// command it falls back to the program-level usage.
func commandUsage(bin, cmd string) string {
	h, ok := perCommandUsage[canonicalCommand(cmd)]
	if !ok {
		return usage(bin)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — %s\n\nusage:\n  %s %s\n", bin, h.name, h.summary,
		bin, strings.TrimSpace(h.name+" "+h.syntax))
	if len(h.aliases) > 0 {
		fmt.Fprintf(&b, "  (alias: %s)\n", strings.Join(h.aliases, ", "))
	}
	if h.detail != "" {
		fmt.Fprintf(&b, "\n%s\n", h.detail)
	}
	return strings.TrimRight(b.String(), "\n")
}
