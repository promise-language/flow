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

## Gates

A **gate** measures something and may refuse it. Every gate holds four properties, and they are what make one worth trusting:

- **It measures; it never modifies what it measures — including afterwards.** A gate may write elsewhere, a build cache or a report, but the subject it reports on is exactly as it found it and exactly as it leaves it. Measuring faithfully and then tidying up is not a gate: a producing step asks one mid-work, and cleaning behind the answer discards the work the step is in the middle of.
- **Its verdict is pass or refuse.** There is no third answer and no partial one.
- **It may also produce a measurement.** Coverage, size, duration — a number the verdict was derived from, by comparison against a baseline or a threshold. The verdict stays binary; what varies is whether the gate had to measure something to reach it.
- **A refusal carries a reason a person can check.** A refusal that cannot be confirmed or overturned is indistinguishable from the gate having given up.
- **It is reproducible.** The same subject gives the same verdict to anyone who runs it, anywhere.

The non-modification rule is what buys the last one. A measurement that changes its subject cannot be repeated: run it twice and the second answer is about a different thing than the first.

**Gates differ only in what they measure and when.** They are one mechanism, not a family of similar ones:

| Measures | Runs |
|---|---|
| A tree, or a merge result | At the decision to integrate |
| An action an agent proposes to take | Before that action |
| What a step actually did, against what it said it would | After the step |
| Whether a machine is fit to be given work | Before work is given |

Recognising these as one thing is worth more than it first appears. A gate that measures a proposed command is the cheapest place to catch a violation — the agent learns the constraint mid-turn and adapts. A gate that measures the result catches it however it happened, including by routes nobody anticipated. **They are complementary because they differ in position, not in kind**, and neither substitutes for the other: the first fails open when it is absent or bypassed, the second costs a whole turn before it speaks.

**This is a property, not a machinery.** What defines the gates, schedules them, records their results and decides what a refusal means for the work queue belongs to whatever schedules work — not to this SDK. What is stated here is what any of them must be.

## Commands

A **command** does work. It may modify anything it is pointed at, and it **may run gates as part of doing its job**.

**Anyone runs them.** A step invokes a command mechanically; an agent runs one mid-turn to see whether what it has written holds together; a person runs one at a terminal. The same command does the same thing for all three, which is what lets an agent check its own work exactly the way the developer reviewing it will — and what makes a project's own tooling the flow's tooling, rather than the flow needing a parallel set of its own.

The same is true of gates, and there it is the point rather than a convenience: a gate that gave a different answer to the person who ran it than to the step that ran it would not be reproducible, and reproducibility is most of what a gate is for.

Commands and gates are the two kinds of thing that get run, and the difference is not how they are implemented but what may be concluded from them:

| | Gate | Command |
|---|---|---|
| Modifies | never | freely |
| Produces | a verdict, sometimes a measurement | whatever work it did |
| A decision may rest on it | yes | no |

The last row is the whole point. A command's result cannot support a decision about the thing it ran against, because it changed that thing on the way. Asking "did this pass" of a command that repaired what was failing gets an answer about a state that did not exist when the question was asked.

**Formatting is the clearest pair.** `format` is a command: it rewrites the source. *"Is this correctly formatted"* is a gate: it reports and changes nothing. Same subject, same underlying rules, and the two are not interchangeable — one repairs, and only the other can be cited.

Most concerns worth checking have both, and building a new one is usually a question of which you need rather than what to write.

**The verify command composes them.** It formats, applies the other fixes that have one correct answer, and then runs the gate — so it modifies, and anything running it re-reads worktree state afterwards rather than assuming the tree is unchanged.

That composition is exactly what a developer wants: repair what is mechanical, then measure what is left. It is also exactly what a decision cannot rest on, which is why integration runs the gate directly rather than the command that wraps it.

A step should not fail over a misplaced brace it can fix. That is what commands are for. Whether something may be integrated is a decision. That is what gates are for.

### Which is required where

**Integration requires the gate.** That is the point where the answer must be reproducible by someone who was not there: a reviewer, a later bisect, a rebuild on a different machine. A verify run that repaired something on its way to a pass says the tree passes *after being changed*, which is a different claim.

**Producing steps may run either**, and generally want verify — they are working, not deciding, and the repairs are the point.

### One value, every consumer

Whichever a project configures, it is configured **once** and reaches everything that needs it: the gate that must pass, and the prompt that tells an agent what to satisfy. Two settings that both mean "the check" will disagree, and the failure is silent — the agent runs one and it passes, the gate runs another and it cannot.

## Steps and the worktree

A step declares whether it may modify the worktree, and the declaration is explicit. A step whose product is a report does not acquire the ability to edit by default or by omission.

**Every modification a step makes is either recorded or refused.** A flow does not leave a step's changes uncommitted for a later step to sweep up, and does not complete an item while carrying changes no step recorded. Silent loss and silent inclusion are the same defect seen from two sides.

## Finalizing

Finalizing marks an item's resolution complete and releases the claim. It is terminal: a finalized item is not reprocessed.

Finalizing means **the work was done**. An item that no flow will act on — because no flow accepts its type — is not finalized on that basis. Reporting success for work that was never attempted hides a misconfiguration, and doing it terminally makes the misconfiguration irreversible.

## Every outcome leads somewhere

**No step ends in a dead end.** Whatever a step concludes, there is a path from it to a terminal state — the item resolves, or a person acts and it continues.

A step that cannot complete ends in exactly one of:

| Outcome | Means | Cleared by |
|---|---|---|
| **Transient failure** | Something outside the work went wrong | Retrying |
| **Invalidates earlier work** | An earlier step's result is wrong; that step must run again, with the reason why | The flow itself |
| **Waits on a person** | A question, a decision, a permission | An answer |
| **Waits on something else** | Another item, an external condition | That condition clearing |

"Error" is not among them. A step that simply stops, with no route onward and nothing named that would unstick it, leaves an item nobody can finish and nobody can close — and it will sit that way indefinitely, because nothing is watching for it.

That is why every stopping outcome names what would clear it. The name is not documentation; it is the difference between a state someone can act on and one that is merely inert.

## Reporting

Every invocation reports exactly one status: `done`, `skipped`, `parked`, `blocked`, or `failed`. These five are the vocabulary, and anything mirroring them mirrors all five.

The report says what happened to **one step**, not to the item. An item's overall state is derived from its durable artifacts, never from the last report.
