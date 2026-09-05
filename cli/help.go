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

// isKnownCommand reports whether cmd is a dispatchable subcommand.
// perCommandUsage is the registry — every case in RunWithArgs's dispatch
// switch must have a matching entry here (and vice versa).
func isKnownCommand(cmd string) bool {
	_, ok := perCommandUsage[cmd]
	return ok
}

// cmdHelp is the per-subcommand help record.
type cmdHelp struct {
	name    string
	syntax  string // text after the command name, e.g. "<item-id>"
	summary string
	detail  string
}

// perCommandUsage drives `<bin> <cmd> --help`. The top-level summary lives in
// usage(); this adds focused detail for a single command.
var perCommandUsage = map[string]cmdHelp{
	"doctor": {name: "doctor", summary: "report whether this environment is fit to be given an item",
		detail: `Checks, and reports every one of them rather than stopping at the first
failure:

  orchestrator   reachable and usable
  agent          can be invoked — established WITHOUT spending a turn

A check the SDK cannot make is reported as skipped and does not affect the
exit code.

doctor spends nothing and mutates nothing: it runs before every item, in CI,
and on machines that are mid-item. It exits 1 if any check failed.
Takes no arguments.`},
	"list": {name: "list", syntax: "[--scope SCOPE] [--tag TAG]…", summary: "list processable items",
		detail: `Lists items this flow can see. Default scope is "processable" (open items
this binary could process).

  --scope SCOPE    how far up the ladder: all|open|processable|workable|free|auto
  --tag TAG        filter by tag (repeatable, conjunctive — item must carry all)

The "availability" column marks which items resolve would auto-select (auto)
versus merely claimable (available) — so the answer to "would resolve pick
this?" belongs in the listing.`},
	"claim": {name: "claim", syntax: "<item-id>", summary: "acquire a claim on an item",
		detail: "Acquires a claim (lease) on <item-id> so this owner can advance it.\nTakes exactly one item id."},
	"release": {name: "release", summary: "drop the active claim",
		detail: "Releases the claim currently held by this owner. Takes no arguments."},
	"reseed": {name: "reseed", syntax: "[--force]", summary: "clear the seed and start fresh",
		detail: `Clears the active claim's artifact records, budget counters, and park
state so the next run-step or resolve re-seeds from the current flow.

This is the escape hatch when a flow's step set changes and an item already
seeded against the old set is stranded — or when a wrongly-resolved artifact
needs to be recovered. Re-seeding discards work that was paid for.

Without --force, prints what would be discarded and refuses.

  --force    actually clear the seed (required)`},
	"status": {name: "status", syntax: "[<item-id>]", summary: "print the lifecycle checklist",
		detail: "Prints the read-only lifecycle checklist. With <item-id>, inspects that\nitem from the backend without claiming it; without, uses the active claim."},
	"run-step": {name: "run-step", summary: "advance one lifecycle item",
		detail: "Advances exactly ONE lifecycle item against the active claim\n(one prompt → one artifact). Requires an active claim; takes no arguments."},
	"resolve": {name: "resolve", syntax: "[<item-id>] [--tag TAG]…", summary: "run all steps to completion",
		detail: "Runs ALL steps until the item is finalized or parked. With <item-id>,\nclaims it first; otherwise uses the active claim.\n\n" +
			"--tag TAG narrows the auto-selectable set to items carrying all given\n" +
			"tags. Mutually exclusive with <item-id> (the id already answers the\n" +
			"question the tag would ask). Selecting nothing is not an error.\n\n" +
			"Progress is narrated on stderr in both output modes. In JSON mode\n" +
			"(--json, FLOW_OUTPUT=json, or a piped/redirected stdout) each step's\n" +
			"InvocationResult is also streamed to stdout, one compact object per\n" +
			"line; in human mode stdout stays empty."},
	"answer": {name: "answer", syntax: "<item-id> <text> [--question ID]",
		summary: "answer a question a step is parked on",
		detail: `Posts a human answer on the item and clears the outstanding-question
marker. Requires no claim — addresses the item by id, like status.

  <item-id>       the item carrying the question
  <text>          the answer text

When the item has more than one outstanding question, --question names the
one being answered. When there is exactly one, it is inferred.

  --question ID   which question to answer (required when multiple are pending)

Answering does not resume the item. Use resolve or run-step to continue.`},
	"stale": {name: "stale", syntax: "<step-id>",
		summary: "mark a resolved artifact stale for re-derivation",
		detail: `Marks one resolved artifact as stale so the next run-step or resolve
re-derives it. Unlike reseed --force, which clears everything, stale
targets a single step and preserves the rest of the item's state.

<step-id> is the artifact id (e.g. "plan") — the first column of
"status". The human label (e.g. "write plan") is not accepted.
Signal steps cannot be marked stale.

The step must be resolved. Pending and skipped steps are refused.
An already-stale step is accepted (idempotent, no backend write).

Marking stale does not reset budget counters or clear a park. If
the step has exhausted its budget, use grant afterward. If the item
is parked on a question, use answer first.`},
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
	h, ok := perCommandUsage[cmd]
	if !ok {
		return usage(bin)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — %s\n\nusage:\n  %s %s\n", bin, h.name, h.summary,
		bin, strings.TrimSpace(h.name+" "+h.syntax))
	if h.detail != "" {
		fmt.Fprintf(&b, "\n%s\n", h.detail)
	}
	return strings.TrimRight(b.String(), "\n")
}
