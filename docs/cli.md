# Flow CLI

> **Tag:** `cli` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines what the CLI *is*, not how it got here. Every statement is a requirement. Where the code does not satisfy a statement, an issue is open against it; when the issue closes the code matches this document. Nothing here records progress, status, or history.

Every binary built on `cli.Run` exposes exactly this surface.

## Commands

| Command | Purpose |
|---|---|
| `claim <item-id>` | Acquire an exclusive claim on an item |
| `release` | Drop the active claim |
| `reseed [--force]` | Clear the active claim's seed — artifacts, budgets, park state — for re-seeding |
| `run-step` | Advance the claimed item by exactly one step |
| `resolve [<item-id>]` | Drive an item to completion, one step at a time |
| `status [<item-id>]` | Report an item's lifecycle checklist |
| `list` | Report the work available to this operator |
| `answer <item-id> <text>` | Answer a question a step is parked on |
| `grant <step> <axis> <amount>` | Add budget to a step and clear a matching budget park |
| `stale <step-id>` | Mark one resolved artifact stale so its step re-runs |
| `doctor` | Report whether this environment is fit to be given an item |

No command outside this set exists. A binary that needs project-specific behaviour expresses it as a flow, not as a new command.

The set is closed in both directions: no command is added by a project, and no command answers to a name not listed above.

**There are no aliases.** Each command has exactly one name, and that name is the one this document uses — in help text, in error messages, and in examples.

## Addressing an item

Every command that names an item accepts the backend's own identifier, verbatim.

Resolution is direct: the backend turns the typed string into an `ItemRef` without enumerating candidates. An item that exists but is not currently eligible is still addressable — eligibility governs what is *offered*, never what can be *named*.

Matching is exact. A typed id never resolves to an item whose identifier merely contains it.

> Not yet true of the github backend, which has no direct resolver and falls back to substring matching over the eligible set — [#6](https://github.com/promise-language/flow/issues/6).

## Output

Two modes, `human` and `json`. Selection, highest precedence first:

1. `--json` / `--human`
2. `FLOW_OUTPUT=json|human`
3. whether stdout is a terminal — human if it is, JSON if it is not

Passing both `--json` and `--human` is a usage error, detected before the command takes any action.

Commands fall into two shapes, and the shape determines the streams.

### One-shot reports — `list`, `status`, `grant`, `stale`, `doctor`, `run-step`

The report *is* the output. It goes to **stdout**, rendered in the selected mode.

### Streaming — `resolve`

`resolve` runs for minutes to hours and produces a result per step. The streams split by role:

- **stderr** carries progress narration, in *both* modes. It names each step before running it and reports the outcome after.
- **stdout** carries per-step `InvocationResult` objects and nothing else. In human mode it carries nothing at all.

This is what makes `resolve > steps.json` behave: JSON accumulates in the file while progress stays visible on the terminal, and `resolve --json 2>/dev/null` yields the machine stream alone.

The mode is decided by **stdout**, never by stderr. Bare `resolve 2>/dev/null` on a terminal prints nothing at all, because a terminal stdout selects human mode and human mode writes nothing to stdout.

JSON-mode stdout is a stable interface. Its bytes are consumed by other programs.

Each `InvocationResult` object may carry `duration_seconds` (wall-clock time of the step, measured by the orchestrator around handler dispatch) and `cost_usd` (what the invocation spent, as a sum of agent turns). Both fields are optional with `omitempty`: a consumer that does not know them is unaffected, and JSON output for a run that reports neither is byte-identical to before these fields existed. `cost_usd` is a pointer type: `null`/absent means unknown (the step never dispatched), while `0` means the step ran but spent nothing.

## Invocation errors

An invocation that cannot be understood is rejected before the command takes any action — nothing is claimed, nothing is written, and the exit code is 2.

**Unknown flags are rejected by name.** The message says that the flag is not recognised and which one it was:

```
resolve: use of unknown flag --dry-run
run `~/prog/flow/bin/issue --help` for usage
```

The pointer names the **actual binary**, resolved to an absolute path with `$HOME` abbreviated to `~` — not a placeholder, and not a bare command name. A reader who has several binaries on their machine, or an agent that did not choose the invocation itself, must be able to tell which one produced the error and re-run it without guessing.

The error does **not** dump the flag list. It names the failure and says how to obtain the usage, in one line each. A wall of flag definitions buries the one fact the operator needs — which flag was wrong — and it is the same output whether they mistyped one flag or twenty.

The same shape covers every malformed invocation: an unknown command, a wrong number of positional arguments, contradictory options such as `--json --human`. Each names what was wrong and points at `--help`; none of them print usage unprompted.

Usage is printed when it is asked for, and only then.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The command did what was asked, including "there was nothing to do" |
| 1 | The command could not complete, or stopped on a condition a human must clear |
| 2 | The invocation was malformed — unknown flag, wrong arity, contradictory options |

An empty result is not an error: `list` with nothing to list, and `resolve` with no eligible item, both exit 0.

A step that parks exits 0 — parking is a designed outcome, not a failure. A step that is *blocked* exits 1, because a blocked item makes no progress until someone acts.

## Status

`status` reports an item's lifecycle: which steps are complete, which remain, and **which step is running right now**.

Each step is reported in exactly one state. The set is closed, and it includes a state for a step that is currently executing — a step being actively worked on is not the same thing as a step that has yet to start, and reporting both as pending loses the distinction an operator most often needs.

For a running step, `status` reports what is running: the step, and the identity of the process executing it, so an operator can find it, watch it, or end it.

### Running is observed, never assumed

A step is reported as running only when its process is **observed to be alive at the moment `status` runs**. A record saying a step is running is not evidence that it is.

This matters because the failure it guards against is common and silent: a step whose process died leaves the record behind. Trusting the record reports work in progress that stopped hours ago, and an operator waiting on it waits forever.

Process identity is checked, not just process existence — a process identifier alone is reusable, and an unrelated process that inherited the number must not be mistaken for the step.

`status` never starts, stops, or modifies anything. It reports.

### Why this is required

A step can run for many minutes. Without this, an operator has no way to distinguish a long-running step from a stalled one, or from one that never started — and the only command whose job is to answer "what is happening?" cannot answer it. Nothing else reports it either: the narration from a running command is visible only in the terminal that launched it, and is gone from any other.

### Implementation hint — not normative

One way to satisfy this: when a step starts, record the step name, the process identifier, and the process's executable path alongside the claim state; `status` reads that record and confirms the process is alive and is still the same executable before reporting it as running.

The mechanism is not normative. Only the requirement is: `status` reports the running step, and does so on evidence rather than on a stored claim.

## Answering

A step that needs a human decision asks a question and parks. `answer` is how that decision is given.

**It requires no claim.** The arena holding the item is not the party who answers — a maintainer, the reporter, or a passer-by can all unblock a step, which is the point of asking in the open. `answer` therefore addresses the item by id, like `status`, and works from any machine.

An answer is recorded **against the question it answers**, and the item's outstanding-question marker **clears when the last pending question is answered** — not when the first one is. The marker describes one condition, *a human must answer this*, and that condition stays true while any question is still waiting.

Both directions of getting this wrong are the same mistake. A marker that outlives the condition is read as current when it is not; a marker cleared while two questions still wait stops advertising a state that is still true, and the run resumes on a decision nobody finished giving. Anything advertising the state — a label, a listing, a dashboard query — tracks the condition, not the most recent answer.

Answering does not resume the item. Resumption is a separate, deliberate act — `resolve` or `run-step` — because somebody has to decide the answer is complete and the work should continue.

If the item has more than one outstanding question, the one being answered is named explicitly. Answering is never applied to an unspecified question.

## Claiming

`claim` acquires an exclusive claim and is idempotent for the holder: re-claiming an item this worktree already holds succeeds and changes nothing.

A claim is refused when the item is already held, when the target worktree is unfit, or when the backend's own preconditions are unmet. Every refusal is **typed** — the caller can tell the reasons apart without reading prose — and carries:

- a one-line human reason,
- the failing check's own output, reproduced verbatim and unmodified,
- the flag that would override it, when one exists.

A refusal that another item might survive is distinguished from one that no item would survive, so `resolve`'s auto-selection knows whether trying the next item is meaningful.

> Refusals are currently untyped prose, matched by substring — [#4](https://github.com/promise-language/flow/issues/4).

`claim` never takes an item away from another person. An item assigned to, or owned by, someone else is refused unless the operator explicitly overrides, and the override is recorded.

> The github backend currently takes over silently — [#5](https://github.com/promise-language/flow/issues/5).

Overrides are named and independent. There is one option per thing being overridden, never a single flag meaning "ignore whatever refused".

## Listing

`list` reports **every item this binary could process**, whatever state it is in — not only the ones the operator can pick up right now.

Restricting it to available work would make the listing unable to answer the question that follows it: *why isn't the item I expected here?* An item held by someone else, or explicitly disabled, is absent for a reason the operator needs, and a listing that omits it sends them to the backend's web UI to find out — which is the gap this command exists to close.

Scope is the binary's own remit. An item no flow here handles is not listed: it is not hidden by policy, it simply is not this binary's work, and browsing the backend's full contents is the backend's own job, not this command's.

`list` is a report. It never claims, never mutates, and never starts work.

### Scope

Items nest in six levels, from everything the backend holds down to what runs unattended:

| # | Level | `--scope` |
|---|---|---|
| 1 | Every item the backend holds, open and closed | `all` |
| 2 | Every **open** item | `open` |
| 3 | Open items **this binary could process** — some flow here accepts the type | `processable` *(default)* |
| 4 | …of those, the ones **not blocked** — someone could work them | `workable` |
| 5 | …of those, the ones **free** — this operator could claim one now | `free` |
| 6 | …of those, the ones **opted in** — an unattended `resolve` would pick one | `auto` |

`--scope` names how far up the ladder to report. The levels nest, so each scope includes every level below it. The value set is closed and its names are the level names — there is no second spelling, and no scope that is not a level.

The default is `processable`: the binary's own remit, which is what an operator is nearly always asking about. Wider scopes are available because the question *"where did item X go?"* is a real one and answering it should not require leaving the tool — but they are opt-in, because at levels 1 and 2 the listing includes items this binary can do nothing with, and on a large backend there are many of them.

For open-ended browsing the backend's own interface remains the better tool. `--scope all` exists to answer a question about work, not to be a second issue browser.

### Availability

Each item is reported in exactly one availability state. The set is closed:

| State | Meaning |
|---|---|
| `auto` | Free, and opted in — an unattended `resolve` would pick this |
| `available` | Free, but not opted in — claimable by name; auto-selection will not choose it |
| `held` | Claimed by someone or something else, named in the report |
| `blocked` | Nobody can work it yet, for a stated reason |
| `unhandled` | No flow in this binary accepts this item's type |
| `closed` | Not open; not work |

Each state is exactly the boundary between two adjacent levels: `closed` is in level 1 but not 2, `unhandled` is in 2 but not 3, `blocked` is in 3 but not 4, `held` is in 4 but not 5, `available` is in 5 but not 6, and `auto` is in 6.

**The state set is closed because the ladder is.** There is no seventh place an item can be, and a new state would mean a new level rather than a new label.

#### `blocked` covers every reason nobody can work it

An item deliberately disabled, and an item waiting on another item that has not finished, are both `blocked`: in this binary's remit, and not workable by anyone right now. They differ in the **reason**, which is reported alongside, and which says whether a person must act or whether it will clear on its own.

They are not separate states. Splitting them would put two states on one boundary, and the causes are open-ended — disabled, unmet dependency, unmet prerequisite, whatever a backend adds next — while the boundaries are fixed at six. States enumerate where an item sits; reasons explain why. Only the first can stay closed.

When the reason is an unfinished dependency, the **blocking items are reported by reference** — their identifiers, as data — not named inside the reason text.

Two things follow from that. An operator reading the listing can see that the blocker is itself listed, and whether *it* is workable, which turns "this is blocked" into "go work that one instead". And anything acting on the listing gets the identifiers without parsing them out of prose, which is the same reason references belong in fields everywhere else in this system.

The blocking items are reported as references, each with **whether it has finished**. That much comes free: an item is blocked precisely because some blocker has not finished, so anything able to say the item is blocked already knows which one. Reporting the list without it would answer *these were declared as blockers* when the question is *what am I waiting on* — and would send an operator to look up every entry to find the one still open.

Nothing further is resolved on their behalf. A blocker's title, who holds it, whether it is itself blocked — those are lookups the caller can make, and a reference is enough both to make one and to recognise the blocker elsewhere in the listing. It is a **reference**, not the rendering of one: what an operator reads is that reference displayed, and what anything acting on the listing gets is the reference itself.

A filter is not widened to include blockers. `--tag` and `--scope` mean what they say; a blocker outside the filter is still named, and still addressable.

**The SDK carries dependencies; it does not interpret them.** Whether one item waits on another is the backend's knowledge, and the backend supplies the state, the reason and the references. Nothing here resolves a blocker, walks a graph, or decides when one clears — those answers are the backend's, and this binary only relays what it is told and offers a way to record what a run discovers.

Recording one is supported rather than incidental. A dependency is found part-way through work at least as often as it is known when an item is filed, so a run that discovers one has somewhere to put it. A backend whose store has no dependency notion still answers — it refuses what it cannot represent, which tells a caller something, where silently accepting and forgetting would not. See [orchestrator.md](orchestrator.md).

`auto` and `available` are distinct because `list` and `resolve` draw from different sets. `list` reports work that could be taken; auto-selection draws only from work already opted in. Widening what an operator can see never widens what an unattended `resolve` will start on — and because both commands accept the same `--tag` filter, they read as symmetrical and are not. The state is what makes that visible in the listing rather than a rule the reader has to know.

### Tags

A tag is a free-form label carried by an item. Each backend maps its own vocabulary onto tags — issue labels, tracker tags — and `list` reports an item's tags in full, not only those a flow recognises. Tags are how an operator picks work by area, and how an unattended `resolve` is scoped to a subset of it.

`list --tag <t> [--tag <t>…]` filters conjunctively, matching `resolve`.

## Resolving

`resolve` drives one item to completion, advancing it a step at a time until it finalizes, parks, is skipped, is blocked, or fails.

It selects the item in one of three ways:

| Invocation | Selection |
|---|---|
| `resolve <item-id>` | That item |
| `resolve` | The active claim if there is one; otherwise an item from the auto-selectable set |
| `resolve --tag <t> [--tag <t>…]` | An item from the auto-selectable set carrying **every** given tag |

**Selection is the only difference between them.** Once an item is chosen it is claimed and driven identically, whichever route chose it. There is no auto mode with its own behaviour — only three ways to answer "which item?".

`--tag` is repeated once per tag, and the filter is conjunctive: an item must carry all of them. Naming an item id *and* a tag is a usage error — the id already answers the question the tag would.

Selecting nothing is not an error. No eligible item, or no item carrying the tags, exits 0.

Auto-selection never picks a `blocked` item. An item waiting on an unfinished dependency is not merely undesirable to start — starting it wastes a claim and a run on work that cannot proceed, and the backend that knows about the dependency is the one that keeps it out of the selectable set.

Losing a race for one item means trying the next. Being refused for a reason no other item would satisfy means stopping.

`resolve` bounds attempts, not wall-clock. A slow step is never killed for being slow.

## Reporting an outcome

Every invocation reports exactly one status: `done`, `skipped`, `parked`, `blocked`, or `failed`.

These five are the vocabulary. A backend that mirrors them mirrors all five.

`done` means work completed. An item that no flow will ever act on — because nothing accepts its type — is not `done`, and is never finalized on that basis: reporting success for work that was never attempted hides a misconfiguration, and finalizing makes it irreversible. It is `blocked`, and the reason names the item's type and the registered ones.

The human narration includes duration and cost when present, as a parenthetical after the status: `resolve: write plan → done (1m22s, $0.34)`. At finalization, the total across all artifacts is reported: `resolve: promise-language/flow#46 finalized ✓ (14m02s, $2.71)`. When any artifact's duration is absent (predates tracking), the total is stated as a lower bound (`≥`).

## Startup

A binary refuses to start, with a named error and exit 2, when its configuration cannot produce correct behaviour: an artifact its backend cannot store, a signal its backend cannot observe, a flow with no steps, a missing agent.

Startup validation is exhaustive before any work begins. A misconfiguration is never discovered part-way through an item.

Startup validation covers what can be checked from configuration alone. What requires touching the environment is `doctor`'s job.

**Validation is scoped to the invoked command.** A command is refused only for configuration it needs. Gate declarations are irrelevant to `list`, `status`, `answer` and `release`, and a binary starts for them on a checkout whose project tools have not been built — that is the state between the two halves of bring-up, not a misconfiguration. `claim`, `run-step` and `resolve` meet the check at their own boundary: each refuses when the orchestrator cannot run what a step will ask for — a missing `integration` or `fit` gate, a missing `verify` command, or a declared name outside those closed sets — and the refusal names what to run: the project's build, and `doctor` for the whole environment picture. That refusal is an environment condition, so it exits **1**, not 2.

**Every condition the boundary refuses on is one `doctor` reports.** The refusal sends its reader there for the rest of the picture, so a condition visible to one and not the other leaves that reader with a command that will not run and a report saying everything is fine.

## Doctor

`doctor` answers one question: **is this environment fit to be given an item?** It runs before work is trusted to a machine, and every check it performs is one whose failure would otherwise surface part-way through an item, after effort has been spent.

It checks exactly these things:

| Check | Failure it prevents |
|---|---|
| The orchestrator is reachable and usable | Work is claimed against a store that cannot be read or written |
| The agent can be invoked — established **without spending a turn** | Every step dies on its first turn, reported as an agent fault rather than a broken environment |
| The verify command is available — the orchestrator declares it in `SupportedCommands()` | A step finishes its work, goes to verify it, and fails on a missing command |
| The gates are available — the orchestrator declares them in `SupportedGates()`, `fit` and `integration` included | Every gate reports `could not start`, on every retry, after the budget is spent |
| Normative documentation is present — `docs/` exists and holds at least one document | An agent works without access to what the project defines as correct, and produces something plausible instead of something right |
The set is closed. A check is added only when its failure is one an operator would otherwise diagnose from a mid-item symptom — which is the same rule [environment.md](environment.md) closes the wider set with, because these five are the SDK's half of it.

**Availability is asked of the orchestrator, never resolved by the SDK.** An orchestrator derives what it can run from the machine it runs on — listing the command binaries present, asking the gate entry point which gates it supports — so a declared list is a fact about this environment and not a claim about intentions. `verify` missing from `SupportedCommands()` *is* "the verify command is not on this machine"; `fit` missing from `SupportedGates()` *is* "the gate entry point is absent, or could not answer". The SDK holds no second copy of paths only the orchestrator knows, and there is nothing to go stale.

**`fit` is the seam, and it is why the set stays closed.** A project extends what fitness means for it by extending its own `fit` gate and its own thresholds — never by adding a check here, which every other project would then have to understand. Whether the machine is *currently* fit is measured where it matters, immediately before a claim: `resolve` runs the gate and waits while the answer is no (§ Resolving). `doctor` reports that the gate is there to be run.

**`doctor` spends nothing.** Not a capped turn, not a tool-free one-word turn — nothing. It is mechanical: it runs before every item, in CI, and on every machine an operator touches, with nobody asking for work, so a turn on that path is a standing charge. And the charge is not the worst of it — a preflight that bills the account is one an operator turns off, and a preflight nobody runs prevents nothing. This is [agent.md](agent.md) § Nothing mechanical may spend, and it is enforced by the commit gate, not by convention.

**So the agent check is what can be established for free**, through the agent's own `AgentDoctor` capability: that this SDK can start the binary. The reference implementation spawns it and asks its version, which catches an absent, unexecutable, wrong-architecture or too-old install — the cases that would otherwise surface mid-item as an empty result stream, read as a model failure, and be diagnosed from the wrong end.

**It is not a lookup on `PATH`,** which would prove only that a file exists. It is also not a full invocation: whether a turn would *succeed* — credentials, quota, model availability — is answered only by spending, and the report says what it checked rather than implying the rest. An agent that offers no free check is reported as **skipped**, not failed: the SDK cannot check a black-box `Agent` without spending, which is a fact about that interface and not about this machine.

`doctor` reports what it found — every check, and which failed — rather than stopping at the first failure. An operator fixing an unfit machine wants the whole list in one pass.

`doctor` does not mutate the worktree. It is a diagnosis, and an operator must be able to run it on a machine mid-item without changing anything.

### The verify command may mutate the worktree

Running the verify command can modify the tree — formatting and other auto-fixes are part of what it does. Anything that runs it re-reads worktree state afterwards and never assumes the tree is unchanged.

`doctor` checks that the verify command exists and is executable. It does not run it, precisely because running it may mutate, and `doctor` must stay safe to run on a machine that is mid-item.

### `doctor` runs no gate and no command

A gate modifies nothing, so running one would be safe — but `formatted`, `tested` and the rest answer *is this tree sound*, which is a question about the change and none of `doctor`'s business. The verify command is not even safe: it repairs before it measures.

`fit` is the one gate whose question is `doctor`'s own, and it is still not run here, because running it would add nothing to what the orchestrator has already established by declaring it. The measurement is taken where it can act on the answer — `resolve`, immediately before the claim, waiting while the machine is unfit rather than reporting a number nobody can use.
