# Standalone resolution

**Normative.** How an item is resolved when **no server exists** and the flow binary drives the whole lifecycle itself.

Everything in `resolution.md` applies. This document states only what is specific to the standalone model; a statement true of both drive models belongs there, not here.

## What "standalone" means

The binary is the entire system. There is no scheduler, no dispatcher, no lease service. An operator — or a timer — invokes the binary, and the binary decides what to do next by reading durable state from the backend.

All state lives **in the backend's own artifacts**. There is no separate store to consult, and nothing to reconcile against: the item is the record.

This is what makes the model resumable across machines with no coordination. Any worktree that can reach the backend and hold a claim derives the same next step.

## Claiming without a lease service

Exclusivity is asserted **in the backend's own data** — the claim is visible to anyone looking at the item, and is what another worktree observes before deciding it cannot take it.

Because there is no arbiter, claiming is a race, and the race is resolved by the backend's own consistency rather than by a lock. A claim attempt that loses reports that it lost; it does not partially apply.

A claim never takes an item away from another person. An item already owned or assigned elsewhere is refused unless the operator explicitly overrides, and the override is recorded.

## Ownership is shared with humans

The backend is a system people use directly. The same item carries human activity — comments, assignment, labels — and the flow's own bookkeeping.

Two requirements follow:

- **The flow's bookkeeping is distinguishable from human content.** Anything the flow writes is identifiable as machine-written, so reading answers, history, or state never mistakes one for the other.
- **The flow does not overwrite what humans own.** The item's original description is never modified. Ownership markers belonging to other people are not removed to make room for the flow's own.

## The steps of a resolution

A flow defines its own steps; what follows is the shape a standalone resolution takes and the properties every such flow holds to.

| Phase | |
|---|---|
| **Plan** | What will be done, before anything is changed |
| **Produce** | Several steps, each carrying the change further under a different concern |
| **Gate** | The verify command, on the tree as it then stands |
| **Propose** | The request |

### Producing is several steps, not one

The producing phase is not "write the change" followed by inspections of it. It is a sequence of steps that each **carry the change further**, differing in what they are looking for: make it work, then make it right, then make it tested.

Naming them as checks would be a mistake with consequences. A step told to *assess* produces an assessment; a step told to *carry the change further* produces a change. The second is what is wanted, and the difference is set by what the step is asked to do rather than by what it is permitted to touch.

### Every producing step may modify the worktree — including the solution

**A producing step is not restricted to its own artifacts.** A step concerned with correctness fixes the fault it finds. A step concerned with coverage writes the missing tests — and where the change cannot be tested as written, **restructures it so that it can be.**

That last license is the one worth stating plainly, because it is the surprising one: a step whose concern is coverage may alter the solution itself. That is intended. Code that cannot be tested is not finished.

**A producing step's concern is a requirement to meet, not a state to assess.** Nothing runs after the producing phase, and the item is resolved when the flow finishes — so a gap a step describes rather than closes is a gap that ships, handed to nobody. There is no later.

Leaving a concern unmet is therefore an exception rather than an outcome, and an exception is justified specifically: what resisted, why, and what would have to change. A reason is accountable to the reader; a list of gaps is a handoff to a step that does not exist.

A step that could only report a fault costs a further agent turn to fix what it had already diagnosed, with everything it needed in front of it. One that fixes it spends the turn it is already paying for.

Nothing is lost by allowing this, because **the gate runs after every producing step and before the proposal**. Whatever any of them changed is verified before anything is proposed. The pipeline establishes correctness; restricting the steps would only restrict usefulness.

### A producing step's artifact is its briefing

Each producing step records what it did. The audience is the person who will review the proposal, so the artifact answers *what did you look for, what did you change, and what should I decide* — not a list of faults, most of which it has already repaired.

The two read differently and the difference matters. A list of faults describes a change that needs work. A briefing describes one that has had work done to it, and directs attention at what remains.

### Every artifact reaches a reader

An artifact that reaches no reader is budget spent on nothing. The producing steps' briefings belong in the request, alongside the plan and the gate's result, because the request is where the reader is.

An artifact filed out of sight is worse than absent when it describes changes that **are** in the diff: the reader meets unexplained work and has to reconstruct why it is there.

## Branch and pull request

Work happens on a **branch of its own per item**, never on the default branch.

A step that consumes the branch checks it out first rather than assuming it is current. A worktree left on another item's branch is corrected, not failed on — the alternative turns one stale checkout into a deterministic failure on every retry.

Resolution terminates in a **pull request**. That is the item's product: the branch is pushed and the request opened as the final step, and the item's completion is the request existing, not the work being merged.

**A pull request is never opened over a worktree carrying uncommitted changes.** The request would describe a branch that does not contain the work, and the omission is invisible in the request itself.

**The branch is pushed before the request is opened, never the other way round.** The ordering matters for recovery: if opening the request fails, the work is already safe on the remote and only the request is missing, so the fix is to open one — not to re-run the flow and redo the work. Opening first would leave a request describing a branch that does not yet exist.

## Committing

The flow commits. The agent does not.

An agent that commits mid-step buries a half-finished round in history, so the prompts instruct against it and the step handler owns it instead.

Commits capture the tree **after** staging and **before** any later step runs, so that the record of what a step produced includes files the step added. A capture taken before staging misses new files while appearing complete; one taken after committing sees a clean tree and reports nothing.

A commit that records nothing is not evidence of success. Whether a branch carries work is answered by comparing it against its base, not by whether a commit call returned without error.

## Selecting work

With no dispatcher, the binary selects its own next item when not given one.

Selection draws **only from items already opted in** — the set an operator has marked as available to be worked automatically. Discovering that an item exists is not the same as consenting to work on it unattended, and the two sets are never merged.

An item that cannot be worked is never selected: one already claimed elsewhere, one waiting on an unfinished dependency, one explicitly disabled.

Selecting nothing is a clean outcome. No eligible item means there is no work, not that something failed.

## Roles, and when one principal holds both

Resolution has two phases and they are performed by different **capabilities**: producing a change, and integrating it. The model separates them because the shape it was designed for separates them — a contributor proposes to a maintainer who is not them, and that gap is what makes the review independent.

**A principal may hold both.** When it does, the separation describes nothing real: running the integration phase as a second act does not recreate independence, it performs it. The check that was doing the work is the integration gate, not the boundary between the roles.

So a principal holding both capabilities **may carry an item through in a single resolution**, ending at a merged change rather than a proposed one. The phases remain distinct — the change is still proposed, the request still exists, the diff and the reasoning are still recorded where a reader can find them — but no second invocation is required to act on a decision already made.

**Carrying through is explicit.** It is stated by the operator, never inferred from the fact that a principal happens to hold both capabilities. Holding the capability to merge is not the same as intending to, and "this flow may land on the mainline" is a property that must be declared rather than acquired by accident.

It is declared where the step set is decided — with the configuration that builds the binary, not as an argument to a single run. The phases a resolution can perform are fixed when it is constructed, so a run cannot elect into an integration phase that was never registered. Declaring the capability and declaring the intent are therefore the same act, made once per binary rather than once per invocation.

Two things follow, and without them "explicit" means only "written down somewhere":

- **The impossible combination is refused at construction.** Intending to integrate without the capability to integrate is a configuration that cannot produce correct behaviour, and it is named as an error before any work begins — not discovered at the merge.
- **A binary that carries through says so at runtime.** It reports it when asked whether it is fit to work, and before a resolution reaches the integration phase. A declaration made once, in configuration, is invisible to whoever invokes it later; explicit has to mean visible to the person acting on it, not merely present somewhere. That report is also the honest place to state what carrying through does not provide — it is not independent review — at the moment someone could still choose otherwise.

**The gate does not move.** Integration is still where verify is required, and it is required against **what will actually land** — the merge result, not the branch as it stood before it. Carrying through in one run shortens the path to integration; it does not remove anything from it. A change that cannot pass the gate does not merge, whoever is asking.

**What is genuinely lost is worth naming.** A single principal reviewing their own agent's work is not independent review, and nothing in this arrangement makes it so. It buys a second pass with different prompts against a different target, which has real value — but it is not a second opinion, and it should not be relied on as one. Where independent review matters, the two phases belong to two principals, and that is the arrangement the separation exists to serve.

## Where verify is required

`resolution.md` requires the verify command exactly where changes are integrated into trunk. **In this model the flow does not integrate — it proposes.** The product is a pull request; a human or a continuous-integration system decides whether it lands.

Pushing is not integration here either. The branch is pushed, and a branch is not trunk — the push makes the work visible for review, not part of the mainline. (Under a central orchestrator the same act *is* the integration, because there is no branch between the push and trunk. Same verb, different meaning, and the difference is which model you are in.)

So the mandatory gate sits *outside* the flow, and the authority over trunk is whatever guards the merge. A flow that ran verify before opening the request has not discharged that gate; it has only avoided proposing something it already knew was unfit.

That is still worth doing, and it is why a step may elect to run verify. But it is a courtesy to the reviewer, not the gate itself, and the flow does not treat its own green run as permission to land.

The verify command is configured once and reaches both consumers — any step electing to run it, and the prompt describing it to an agent. A default that ships with the SDK is a default that must work in the repository shipping it: a default naming a convention the project has abandoned is a trap, because a consumer who sets nothing gets a configuration that looks valid and cannot run.
