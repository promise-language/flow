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

Infrastructure failures consume no budget. A step that could not run because the environment was unavailable has not spent an attempt. What counts as one, and how it is told apart from a failure of the work, is [environment.md](environment.md).

**Spending resources must produce progress.** Every invocation either advances the item or leaves behind something the next invocation starts from — the work done, the answer needed, or the reason the attempt could not be recorded. An invocation that consumes budget and leaves the item exactly as it found it has not failed once; it has established that every remaining invocation will fail the same way, because nothing about the next attempt differs from the last.

That is the shape to check any retry against: **a retry that cannot differ from the attempt before it is not a retry, it is a loop with a budget.** It exhausts the grant, reports the cap as the reason, and names the wrong problem — an operator granted more budget would buy an identical failure.

So an attempt stopped by something correctable hands the correction back. A refused write returns to the step that produced the text, carrying what was refused and why, so the next attempt is answering something the last one did not know.

**A correction round costs a prompt, not an invocation.** An invocation is an attempt at the step; a refused *expression* of finished work is not a failed attempt. Charging one exhausts a three-invocation grant in three sentences and then reports a budget cap — naming the wrong problem, which is exactly what this section is about. It must cost something: a correction that were free is a loop against whatever refused it, with nothing bounding it at all.

## Parking

A park records that a step stopped without completing, and why. Every park names the step it belongs to.

Parks divide into two kinds, and the division is what an operator acts on:

- **Self-clearing** — the condition resolves without human action, and the item resumes when it does.
- **Human-clearing** — nothing changes until a person acts. A budget cap, an unanswered question, or a condition only a human can lift.

A park that advertises a condition **stops advertising it the moment the condition ends**. A marker outliving its condition is worse than no marker, because it is read as current.

## Work in progress

A step that stops without completing may leave **what it worked out** where its own next invocation finds it. That is what makes an invocation that could not finish still produce progress: the work that led to the question, or to the refused sentence, is the expensive part, and it is exactly the part a stop would otherwise discard.

The record is **scaffolding, not a result**:

- **It does not resolve the step.** An unfinished plan stored as the plan artifact would mark the step done and let the resolution proceed against a plan that was never finished — a worse outcome than losing it.
- **It decides nothing.** The next step is still derived from artifacts and signals; a record is read by the step that wrote it and by nothing else. It is not part of what a reviewer reads, and it does not appear in what is proposed.
- **It is keyed by item and step, and read only when both match.** Keying is the correctness property; clearing is hygiene. Every path that skips a cleanup — a crash, a kill, a lost machine, a working directory left by an abandoned run — would otherwise feed one item's reasoning to another item's agent, where it arrives indistinguishable from that agent's own thinking. A missing record costs a re-derivation, which is the cost of not having the mechanism at all; a wrong record costs a plan built on another item's reasoning, published under this item's number. Every ambiguity resolves toward discarding.
- **It is never published.** For a refused write the text to keep *is* the text a guard refused, so a store that could go outward is a store that cannot hold it.
- **It is cleared when the step resolves**, and when the claim is released or finalized. Scaffolding that outlives its work becomes stale prose a later reader mistakes for a record; reasoning left behind after the work is over is a disclosure sitting around for no benefit.
- **It is optional.** A step that does not use it behaves exactly as one would without the mechanism.

Where the record physically lives is the backend's: beside its claim state on a machine that holds one, or with the claim on a server, so that an arena can lose its disk without losing the record.

## Questions

A step that needs a human decision asks a question and parks. It asks **in the open**, where the humans already are — an answer from anyone counts, not only from whoever filed the item.

Answering does not resume the item. Resumption is a separate deliberate act, because somebody has to judge the answer complete.

Re-running an item with an unanswered question consumes no budget and runs no agent turn. The check happens before dispatch, not inside it.

## Gates

A **gate** measures something. Every gate holds four properties, and they are what make one worth trusting:

- **It measures; it never modifies what it measures — including afterwards.** A gate may write elsewhere, a build cache or a report, but the subject it reports on is exactly as it found it and exactly as it leaves it. Measuring faithfully and then tidying up is not a gate: a producing step asks one mid-work, and cleaning behind the answer discards the work the step is in the middle of.
- **It reports what it measured, and does not judge it.** Coverage, size, duration, failure counts — numbers, not a verdict. Whether those numbers are acceptable is decided elsewhere, against thresholds the gate does not hold.
- **Its measurement is complete, or says why it is not.** A run that skipped a suite reports honest numbers that understate what was checked, which is indistinguishable from a regression unless the run says so.
- **It is reproducible.** The same subject gives the same measurement to anyone who runs it, anywhere.

### A gate does not decide

This is the property most easily lost, and losing it is expensive.

The thresholds a measurement is judged against — a coverage floor, a size limit, a baseline that ratchets — are not the gate's. They belong to whoever decides, and they are deliberately **out of reach of whatever the gate is measuring**. A gate that carries its own thresholds can be made to pass by editing the gate, and when the thing being measured is a change written by an agent, the agent can edit it.

So `test_failures: 3` is not a verdict. It is a pass or a failure depending on state the gate does not have and must not be given.

**A gate also does not report whether it finished.** A process killed for memory, or truncated mid-write, is not alive to say so — and one that exits cleanly having measured nothing can say something false. What became of a run is the account of whatever spawned it, which is a third party: the gate measures, a runner observes, and the layer holding the thresholds judges. [gates-and-commands.md](gates-and-commands.md) states the contract.

### Reproducibility has two halves, and one rule covers both

**The measurement half** is why a gate is a program rather than a script. A script inherits whatever the environment hands it — user configuration, path differences, shell dialects — so two hosts can disagree about a textually identical gate for reasons that have nothing to do with the subject.

**The judgement half** is the same requirement applied to what the measurement is compared against: **what a verdict depends on must be a function of the subject.** A threshold that moves on its own schedule fails this — the same tree is judged one way today and another way next month, and neither answer is about the tree. Checking out an old commit and judging it against a threshold that has moved since answers a question about neither.

Note what this does *not* say. A threshold versioned with the tree is the one place it cannot vary: it moves when the subject moves, so a commit carries the terms it was judged on and any machine reaches the same verdict offline. Being *near* the gate is not the problem — being independent of the subject is.

That is why "a gate does not judge" is a separate rule with a separate reason, and not a corollary of this one. It is the artifact rule: the party under judgement must not hold what judges it.

The non-modification rule is what makes any of it repeatable. A measurement that changes its subject cannot be repeated: run it twice and the second answer is about a different thing than the first.

**Gates differ only in what they measure and when.** They are one mechanism, not a family of similar ones:

| Measures | Runs |
|---|---|
| A tree, or a merge result | At the decision to integrate |
| What a step actually did, against what it said it would | After the step |
| Whether a machine is fit to be given work | Before work is given |

Every one of them measures something that already exists, and reports on it.

The third row is the only one whose subject is not the work. What makes a machine fit, and what follows from an answer of unfit, is [environment.md](environment.md).

## Guards

A **guard** is not a gate. It stands on the execution path of an act that has not happened yet, and **prevents it**.

| | Gate | Guard |
|---|---|---|
| **Subject** | **Something that persists independently of the check** | **The act itself, which exists only as a proposal** |
| Position | Beside the work. It observes | On the path. The act cannot occur without passing it |
| Answers | A measurement, judged elsewhere | Allow or refuse, decided there and then |
| Its answer | Is stored, compared across runs, re-judged later | Is consumed once, by the caller about to act |

**The subject is the distinction; everything else follows from it.** A gate's answer is storable and judged elsewhere *because there is something for it to be about afterwards*. A guard's answer is consumed immediately and never stored *because after the decision there is nothing left for it to be about* — the act either happened or it did not.

Prevention is **not** the test, and using it as one misclassifies the ordinary case. A gate that blocks a push prevents something: remove it and the push happens. It is still a gate, because its subject is a tree that exists, its answer is a measurement, and that measurement is judged against a baseline later. Whether a verdict blocks something is a fact about what consumes it, not about what it is.

Two guards exist in a resolution: one over the actions an agent proposes to take, and one over what the flow publishes (`docs/disclosure.md`).

**A guard decides for itself, where a gate must not.** Judging is kept out of a gate because its measurement is re-judged later, so the comparison has to be recomputable by someone who was not there — which is why thresholds are a separate artifact. A guard has no persistent subject to re-judge, so there is no second judgement for separation to keep honest.

**A guard and a gate over the same concern are complementary, and neither substitutes for the other.** A guard on a proposed command is the cheapest place to stop a violation — the agent learns the constraint mid-turn and adapts, rather than losing the whole turn. A gate on the result catches it however it happened, including by routes nobody anticipated. The guard fails open when it is absent or bypassed; the gate costs a whole turn before it speaks.

**A guard must not be authored by the party it constrains**, and this holds for every guard rather than only the ones whose refusals are irreversible.

The reason is that a guard leaves no review window. A weakened gate is caught by review before its answer authorises anything — the measurement persists, and a wrong one can be recomputed and contradicted. **A guard weakened at one step authorises the next step immediately**, before any review exists, and leaves no trace: a run in which the guard was bypassed looks exactly like a run in which it had nothing to refuse.

So a guard's rules come from outside the tree it constrains. Where a resolution runs under an orchestrator, that is the arena applying rules from a companion repository; where it runs standalone, the guard is part of the flow, which is delivered from outside the tree it resolves. A guard configured from inside the worktree is one an `implement` step can edit, and an agent that can edit its own bounds has none.

**This is a property, not a machinery.** What defines the gates, schedules them, records their measurements, holds the thresholds those measurements are judged against and decides what a failure means for the work queue belongs to whatever schedules work — not to this SDK. What is stated here is what any of them must be.

## Commands

A **command** does work. It may modify anything it is pointed at, and it **may run gates as part of doing its job**.

**Anyone runs them.** A step invokes a command mechanically; an agent runs one mid-turn to see whether what it has written holds together; a person runs one at a terminal. The same command does the same thing for all three, which is what lets an agent check its own work exactly the way the developer reviewing it will — and what makes a project's own tooling the flow's tooling, rather than the flow needing a parallel set of its own.

The same is true of gates, and there it is the point rather than a convenience: a gate that gave a different answer to the person who ran it than to the step that ran it would not be reproducible, and reproducibility is most of what a gate is for.

Commands and gates are the two kinds of thing that get run, and the difference is not how they are implemented but what may be concluded from them:

| | Gate | Command |
|---|---|---|
| Modifies | never | freely |
| Produces | a measurement | whatever work it did |
| A decision may rest on it | yes | no |

The last row is the whole point. A command's result cannot support a decision about the thing it ran against, because it changed that thing on the way. Asking "did this pass" of a command that repaired what was failing gets an answer about a state that did not exist when the question was asked.

A gate's measurement *may* support a decision — but it is not itself the decision. Something else holds the thresholds and reaches the verdict; see [gates-and-commands.md](gates-and-commands.md).

**The rule constrains the caller, not the tool.** A repairing command that repairs is behaving correctly; the defect is in what was asked. That is why this cannot be caught by checking tools for misbehaviour — there is nothing misbehaving to catch, and the wrong answer arrives as a green result rather than as an error.

**Formatting is the clearest pair.** `format` is a command: it rewrites the source. *"Is this correctly formatted"* is a gate: it reports and changes nothing. Same subject, same underlying rules, and the two are not interchangeable — one repairs, and only the other can be cited.

Most concerns worth checking have both, and building a new one is usually a question of which you need rather than what to write.

**The verify command composes them.** It formats, applies the other fixes that have one correct answer, and then runs the gate — so it modifies, and anything running it re-reads worktree state afterwards rather than assuming the tree is unchanged.

That composition is exactly what a developer wants: repair what is mechanical, then measure what is left. It is also exactly what a decision cannot rest on, which is why integration runs the gate directly rather than the command that wraps it.

A step should not fail over a misplaced brace it can fix. That is what commands are for. Whether something may be integrated is a decision. That is what gates are for.

### Which is required where

**Integration requires the gate.** That is the point where the measurement must be reproducible by someone who was not there: a reviewer, a later bisect, a rebuild on a different machine. A verify run that repaired something on its way to a pass says the tree passes *after being changed*, which is a different claim.

**Producing steps may run either**, and generally want verify — they are working, not deciding, and the repairs are the point.

### One value, every consumer

Whichever a project configures, it is configured **once** and reaches everything that needs it: the gate that must pass, and the prompt that tells an agent what to satisfy. Two settings that both mean "the check" will disagree, and the failure is silent — the agent runs one and it passes, the gate runs another and it cannot.

## Steps and the worktree

A step declares whether it may modify the worktree, and the declaration is explicit. A step whose product is a report does not acquire the ability to edit by default or by omission.

**Every modification a step makes is either recorded or refused.** A flow does not leave a step's changes uncommitted for a later step to sweep up, and does not complete an item while carrying changes no step recorded. Silent loss and silent inclusion are the same defect seen from two sides.

### The commit contract

**A step commits the tree whole.** Every file the tree carries — everything not already ignored — is staged, and the commit captures all of them or it is rejected. A half-staged tree, some files in and the rest left behind, is not a commit this contract permits: a commit of part of the tree is not the tree the gate measured, so a passing gate would certify a state that never lands.

**So nothing uncommittable may be in the tree.** Not as a third tolerated state beside committed and ignored — this is what makes committing the tree whole safe rather than merely conventional. While such a file is present — a build artifact a repository guard refuses, anything a commit hook rejects — no step can satisfy the requirement above.

**The remedy is deletion.** Not leaving the file uncommitted, and not marking it ignored. An ignore rule that predates the failure is a different thing — build output a project has always excluded is in a settled state, and the contract is already satisfied. What is forbidden is reaching for the ignore list *because* something was refused. The three moves are not stylistic variants:

| Move | Effect |
|---|---|
| **Delete it** | The tree becomes committable. The failure ends. |
| Leave it uncommitted | The file survives; the next step commits the tree whole and is refused identically |
| Mark it ignored | The file survives and stops being reported |

Ignoring is the worst of the three and reads as the most helpful. The gate measures the **working tree**, so it measures a tree containing the file and passes; what lands never contains it, and breaks. A loud, local, immediate failure becomes a silent one that surfaces later, on somebody else's change. Whatever repair a flow offers must name deletion and must not name the other two: an error that volunteers "or ignore it" is teaching the failing move.

**A refusal returns to the step that caused it, in the refusing tool's own words.** The refusal names the offending file and the remedy precisely; a step told only that committing failed has to rediscover both. This is the path a failing gate already takes.

**A refusal that survives repair parks, and costs nothing.** Once retrying is known to be pointless — the same tree, committed again, refused identically — the step parks carrying the refusal's own message, rather than spending invocations on a repetition it cannot change.

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
