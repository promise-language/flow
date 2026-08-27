# Resolution

**Normative.** This document defines how an item is resolved, independent of what drives the resolution. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it. Nothing here records progress, status, or history.

Two drive models exist, and they are the only two:

| Model | Who decides what runs next |
|---|---|
| **Standalone** | The flow binary. No server exists. See `resolution-standalone.md`. |
| **Orchestrated** | A server schedules, leases and dispatches; the binary executes one step on command. See `resolution-orchestrated.md`. |

Everything in *this* document is true of both. A statement that is true of only one belongs in that model's document, and a statement true of both belongs **here and nowhere else** — restating it in the model documents is how two copies drift into disagreement.

## The item and its lifecycle

An **item** is a unit of work owned by a backend. A **flow** is an ordered list of steps that resolves items of a given type.

A **step** produces exactly one result: an artifact, or a signal. A step that produces nothing has not run. The result is the step's identity — it is what budget is metered against, what a park names, and what `status` reports.

The lifecycle is: **claim → seed → advance one step at a time → finalize**. An item enters at claim and leaves at finalize; everything between is a sequence of single-step advances.

## Claiming

A claim is **exclusive**: at most one worktree works an item at a time, and at most one item is worked per worktree. The binding is one-to-one in both directions.

A claim is **idempotent for its holder**: re-claiming an item this worktree already holds succeeds and changes nothing.

A claim carries the credential for every subsequent write. Operations that modify an item require it; read-only inspection does not.

Claims are released explicitly, or by finalizing. A claim is never released as a side effect of a step failing, a step parking, or a process exiting — work in progress keeps its claim so it can be resumed.

## Seeding

Seeding records the artifact set and budget caps an item will be resolved against. It happens **once**. A flow definition that changes later does not retroactively re-seed items already in flight, because a step's budget and required-artifact set must not move underneath a run.

Re-seeding is possible only by an explicit operator action, never automatically.

## Advancing

One invocation advances **at most one step**. This holds in both drive models and is what makes a run inspectable: after any invocation, the item is in a state some human can read.

The next step is derived from durable state — which artifacts are resolved, which signals are set — never from a record of what ran last. Two consequences follow, and both are requirements:

- Resolution is **resumable**. An item picked up by a different worktree, after any interruption, derives the same next step.
- Execution history is **telemetry**. It records what happened; it never decides what happens next.

A step runs only when its result is unresolved. A resolved result is not recomputed unless something explicitly marks it stale.

## Budgets

Every step carries caps on what it may consume: invocations, prompts per invocation, cost, and wall-clock time. The set is closed.

A step that reaches a cap **parks** rather than continuing. Parking is not failure — no work is lost and the claim is kept.

Budget is granted additively by an operator. A grant clears the park it satisfies, and only that park: granting an axis that is not the exhausted one leaves the item parked, because the next dispatch would re-park immediately.

Infrastructure failures consume no budget. A step that could not run because the environment was unavailable has not spent an attempt.

## Parking

A park records that a step stopped without completing, and why. Every park names the step it belongs to.

Parks divide into two kinds, and the division is what an operator acts on:

- **Self-clearing** — the condition resolves without human action, and the item resumes when it does.
- **Human-clearing** — nothing changes until a person acts. A budget cap, an unanswered question, or a condition only a human can lift.

A park that advertises a condition **stops advertising it the moment the condition ends**. A marker outliving its condition is worse than no marker, because it is read as current.

## Questions

A step that needs a human decision asks a question and parks. It asks **in the open**, where the humans already are — an answer from anyone counts, not only from whoever filed the item.

Answering does not resume the item. Resumption is a separate deliberate act, because somebody has to judge the answer complete.

Re-running an item with an unanswered question consumes no budget and runs no agent turn. The check happens before dispatch, not inside it.

## The verify command

The project's verify command establishes that a change is fit to integrate.

**It is required at exactly one point: where changes are integrated into trunk.** That is the only place it is mechanically mandatory, and it is mandatory without exception — nothing reaches trunk that has not passed.

**Everywhere else it is elective.** A step may run it while working, as often as it finds useful, to check its own progress or to drive a fix loop. That is a tool the step chose to pick up, not a gate it was made to pass. A step is not required to leave the tree green, and a step that leaves it red has not failed on that account — the change is simply not yet ready to integrate, which the integration point will establish on its own.

Treating verify as a per-step gate costs real work: it forces a step to converge before handing off, when the natural shape is often several steps converging together.

It is configured once and reaches every consumer — the integration gate, and the prompt that tells an agent what to satisfy. **One value, every consumer.** Two settings that both mean "the verify command" will disagree, and the failure is silent: the agent runs one and it passes, the gate runs another and it cannot.

Running the verify command **may modify the worktree**. Anything that runs it re-reads worktree state afterwards and never assumes the tree is unchanged.

## Steps and the worktree

A step declares whether it may modify the worktree, and the declaration is explicit. A step whose product is a report does not acquire the ability to edit by default or by omission.

**Every modification a step makes is either recorded or refused.** A flow does not leave a step's changes uncommitted for a later step to sweep up, and does not complete an item while carrying changes no step recorded. Silent loss and silent inclusion are the same defect seen from two sides.

## Finalizing

Finalizing marks an item's resolution complete and releases the claim. It is terminal: a finalized item is not reprocessed.

Finalizing means **the work was done**. An item that no flow will act on — because no flow accepts its type — is not finalized on that basis. Reporting success for work that was never attempted hides a misconfiguration, and doing it terminally makes the misconfiguration irreversible.

## Reporting

Every invocation reports exactly one status: `done`, `skipped`, `parked`, `blocked`, or `failed`. These five are the vocabulary, and anything mirroring them mirrors all five.

The report says what happened to **one step**, not to the item. An item's overall state is derived from its durable artifacts, never from the last report.
