# The environment

**Normative.** What makes a machine fit to be given an item, and what follows from a condition that makes it unfit. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it. Nothing here records progress, status, or history.

`resolution.md` defines what a gate is, and requires that infrastructure failures consume no budget. `gates-and-commands.md` defines the gate contract and names what a project supplies. This document defines the one gate whose subject is not the change, and the rules that follow from a subject that is not the change.

## What an environment condition is

**A property of the machine, of the delivered tree, or of a remote — never of the work.** Two questions decide it, and it is one only when both answer the same way:

- Does the same item succeed on another machine?
- Does a different item here fail identically?

Everything else a resolution can stop on is a property of the item: a budget cap, an unanswered question, a refused write, a failing suite. An environment condition is the one class where the item is incidental. It was in the way, and any item would have been.

**The test is deliberately not "is retrying free".** A machine with no disk left is not a transient failure — retrying costs exactly what the first attempt cost and produces exactly the same answer. Nor is it a deterministic refusal, because the same item on another machine succeeds immediately. A vocabulary that offers only those two has no place to put it, and whichever is chosen is wrong: *transient* sends the item back to the machine that cannot run it, and *refused* blames a change that is fine.

## The classification is two questions, not one

**What would clear it, and what it is a property of.** The second is what the first is derived from, and a classification that answers only the first cannot be acted on.

| Property of | Is | Whose problem |
|---|---|---|
| **The machine** | No space left where the worktree or the build writes; the agent cannot be invoked; anything the project knows its work requires and does not have | A person acting on the machine — or the machine recovering, which is not knowable in advance |
| **The delivered tree** | The gate entry point or the verify command absent or not executable; normative documentation absent | Whoever delivered the tree |
| **A remote** | The backend or a git remote unreachable | The remote returning |

Two of these owners are already the runner's — the host, and whoever declared the gate or delivered the tree — and they are kept apart here for the reason `gates-and-commands.md` gives there: collapsing them costs attribution even where it never costs a wrong retry.

**Retryability is derived, never declared alongside the condition.** A remote clears on its own; a machine does not, until someone acts on it or something on it finishes; a delivered tree never does. Reporting "transient" states the conclusion and discards what it was drawn from, which leaves the next reader unable to check it or to act.

## Nothing is concluded from a failure observed on an unfit machine

**A failure observed on an unfit machine is not a measurement of anything.** It does not resolve a step and it does not fail one. It does not reach an agent. It does not move a baseline, and it does not become a park on the item.

This is the whole of what the classification buys, and each clause forbids something that otherwise happens by default:

- **It does not reach an agent.** No prompt is rendered from it and no turn is spent on it. This does not follow from *infrastructure failures consume no budget* (`resolution.md`): a turn that is not charged against a cap is still a turn that was paid for, and a loop that re-prompts an agent with a full disk's error text spends real money to be told the same thing again. Where a failure is handed back to an agent to work from, the environment is measured **before** the hand-back, not after it.
- **It does not fail a step.** A step that stopped because the machine did has not attempted anything, and reporting a failure against it makes the item look defective on every machine.
- **It does not move a baseline.** `gates-and-commands.md` already requires that a run which measured less than usual must not move one. A run on an unfit machine measured less than usual by definition.

## `fit` measures the machine

`resolution.md` names this gate already: *"Whether a machine is fit to be given work — before work is given."* It is the third row of the gate table there, and it is the only gate whose subject is not the code.

**It is `fit`, and the project provides it.** One of the required set in `gates-and-commands.md`, reached the same way every gate is: `bin/gate fit --envelope`, spawned by the runner, reported as one of the five outcomes, and judged by the project's judge against the project's thresholds.

**The project provides it because only the project knows what its work requires.** How much disk a build needs, which services a suite expects, what toolchain must be present — the SDK cannot know any of it, and a floor compiled into the SDK is a threshold held by the wrong party. This is the same boundary every other gate sits on: the gate measures, and something holding the thresholds decides.

**It divides into instances like any other concept.** `fit:disk`, `fit:toolchain`, `fit:services` — separately runnable, so a wait on one condition re-measures that condition rather than the whole set. The concept is closed and the instances are the project's, exactly as for `tested`.

**It does not appear among the concerns a change is measured against.** `formatted`, `builds`, `checked`, `tested` and `covered` are properties of the code, and a project reads that table to decide what its `integration` is made of. `fit` belongs in neither place: a machine that cannot build is not a change that may not land.

### A `fit` gate may come from the tree, because the tree is not what it judges

`gates-and-commands.md` permits a gate to be a tree artifact because what it reported is checkable against its subject afterwards, by anyone. A machine's free disk at an instant is not checkable afterwards, so that reason does not carry, and `fit` needs its own.

**It has one, and it is stronger: a `fit` gate does not judge the change.** The rule that keeps a judgement out of the reach of what it judges exists because a party under judgement can make itself pass. A `fit` gate reporting falsely buys the tree nothing — claiming *fit* when unfit starts work that then fails visibly, and claiming *unfit* stops work without landing anything. There is no version of lying that gets a change past a gate, which is the risk the rule was written against.

**What its subject not persisting does cost is the ability to settle a disagreement afterwards**, and the answer to that is the section below: the machine, unlike a process that has exited, is still there to be asked again.

## Unfit is a wait, not a verdict

**An environment condition is re-measured, never assumed to persist.** A machine is not a vanished process: it can be asked again, and the answer changes without anyone announcing it — a build elsewhere finishes, a log rotates, a person frees space. Work resumes when `fit` reports clear, and nothing about resuming requires a person to have said so.

This is what `resolution.md` § *Parking* requires of any advertised condition — that it stops being advertised the moment it ends. Re-measuring is how that is achieved for a condition nobody reports the end of.

**The wait is bounded, and exhausting the bound is not a verdict.** `gates-and-commands.md` states this for a queued gate and it holds identically here: exhausting a wait is *still unfit*, which is a condition to report, never a refusal to act on. What the bound protects is the claim — an item held indefinitely on a machine nobody is fixing is an item no other machine can take.

**Who waits is the drive model's, and neither model's answer belongs here.** Standalone drives its own lifecycle and holds; under an orchestrator the binary reports and the server decides, which `resolution-orchestrated.md` already assigns to it. What is required of both is that the condition is re-measured rather than concluded from once.

**Waiting holds the claim and touches nothing else.** `resolution.md` requires that a claim survives a step failing; an environment condition is not even that, so the item is left exactly as it was found — unparked, unmodified, and workable by another machine the moment this one lets go of it.

## The condition is reported, not recorded on the item

**It is reported as `blocked`**, which already means the run stopped and a person must act, and which already exits non-zero. What it carries is the measurement: the gate's own envelope, the verdict the judge returned, and the run the verdict was reached from.

**Nothing about the condition is written to the item.** `resolution.md` requires that every park names the step it belongs to, and that a park stops advertising its condition the moment the condition ends. A full disk satisfies neither — the step did not stop for a reason belonging to it, and the marker would travel with the item to a machine that has disk, where it is read as current and is false.

**The report names the condition, the measurement, and what would clear it.** `12 MB free on /srv/work/promise, floor 2 GB` is actionable; *verify failed* is not, and *infrastructure failure* is not either. A condition reported without the measurement that established it is one an operator has to reproduce before believing, on the machine that is already the problem.

## The set is closed, and one rule keeps it closed

**Every member is both a check made before work is given and a classification made during it.** A condition worth recognising after effort has been spent is worth refusing before it is spent; a condition not worth checking up front is not a member. Adding one means adding it in both places, and that requirement is what stops the set growing by accident into "things that went wrong".

The two points are not alternatives. The first avoids starting work that cannot finish. The second is the load-bearing one: a machine fit when the item started fills up during a step, and no up-front check can see that coming.

| Measured by | Members |
|---|---|
| The SDK | The backend is reachable; the agent can be invoked; the verify command and the gate entry point exist and are executable; normative documentation is present |
| The project's `fit` gate | Whatever the project knows its work requires — disk, toolchain, services |

The SDK's members are `doctor`'s check set, which `cli.md` holds and which is closed there. **`fit` is the seam**: a project extends what fitness means through its own gate and its own thresholds, and never by adding a member to a set everyone else must understand.

**Memory is not a member**, and the reason is worth stating rather than discovering. A gate killed for memory is already `died` under the runner's account, owned by the host and named as such. There is no second mechanism to build, and adding one would give the same condition two names.
