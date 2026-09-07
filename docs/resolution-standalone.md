# Standalone resolution

> **Tag:** `resolution-standalone` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** How an item is resolved when **no server exists** and the flow binary drives the whole lifecycle itself.

Everything in `resolution.md` applies. This document states only what is specific to the standalone model; a statement true of both drive models belongs there, not here.

## What "standalone" means

The binary is the entire system. There is no scheduler, no dispatcher, no lease service. An operator — or a timer — invokes the binary, and the binary decides what to do next by reading durable state from the orchestrator.

All state lives **in the orchestrator's own artifacts**. There is no separate store to consult, and nothing to reconcile against: the item is the record.

This is what makes the model resumable across machines with no coordination. Any worktree that can reach the orchestrator and hold a claim derives the same next step.

## Claiming without a lease service

Exclusivity is asserted **in the orchestrator's own data** — the claim is visible to anyone looking at the item, and is what another worktree observes before deciding it cannot take it.

Because there is no arbiter, claiming is a race, and the race is resolved by the orchestrator's own consistency rather than by a lock. A claim attempt that loses reports that it lost; it does not partially apply.

A claim never takes an item away from another person. An item already owned or assigned elsewhere is refused unless the operator explicitly overrides, and the override is recorded.

## Ownership is shared with humans

The orchestrator is a system people use directly. The same item carries human activity — comments, assignment, labels — and the flow's own bookkeeping.

Two requirements follow:

- **The flow's bookkeeping is distinguishable from human content.** Anything the flow writes is identifiable as machine-written, so reading answers, history, or state never mistakes one for the other.
- **The flow does not overwrite what humans own.** The item's original description is never modified. Ownership markers belonging to other people are not removed to make room for the flow's own.

## The steps of a resolution

A flow defines its own steps and routes; what follows is the shape a standalone resolution takes **when the work is a change to the tree**, and the properties every such route holds to. Not every item is that — the step that plans is where the work's shape is discovered, and its declared routes are where a different shape goes a different way ([issue-flow.md](issue-flow.md) ships an example). What follows binds the change-shaped route.

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

**A producing step's concern is a requirement to meet, not a state to assess.** No producing step runs after this phase — what follows is proposal and review, which judge the change rather than carry it — so a gap a step describes rather than closes is a gap that ships, handed to nobody. There is no later.

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

A step that consumes the branch declares it as its needed state, and establishment checks it out rather than assuming it is current ([resolution.md](resolution.md) § Steps and the worktree). A worktree left on another item's branch is corrected, not failed on — the alternative turns one stale checkout into a deterministic failure on every retry.

The contributor role terminates in a **pull request**. That is the role's product: the branch is pushed and the request opened, and the contributor's part is complete when the request exists — what happens to the item next is the route's business, a handoff to whoever reviews and integrates, or the same principal continuing where its roles span the boundary ([resolution.md](resolution.md) § Accounts, capabilities and roles).

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

An item that cannot be worked is never selected: one already claimed elsewhere, one waiting on an unfinished dependency, one explicitly disabled — and one awaiting a role this runner cannot assume, or a signal not yet observed. Selection answers *whose move is it* before it answers anything else ([resolution.md](resolution.md) § Whose move it is).

Selecting nothing is a clean outcome. No eligible item means there is no work, not that something failed.

## Declaring what a binary may do

The role model — accounts, capabilities, roles, handoff, and one principal crossing a boundary its capabilities span — is [resolution.md](resolution.md) § Accounts, capabilities and roles. What is standalone-specific is where intent is declared, because with no server there is nowhere else to declare it.

**Carrying an item through to the mainline is explicit.** It is stated by the operator, never inferred from the fact that a principal happens to hold the capabilities. Holding the capability to merge is not the same as intending to, and "this flow may land on the mainline" is a property that must be declared rather than acquired by accident.

It is declared where the flow is decided — with the configuration that builds the binary, not as an argument to a single run. What a resolution can perform is fixed when it is constructed, so a run cannot elect into a role its binary never intended. Declaring the intent and holding the capability are checked against each other, and two things follow — without them "explicit" means only "written down somewhere":

- **The impossible combination is refused at construction.** Intending to integrate without the capability to integrate is a configuration that cannot produce correct behaviour, and it is named as an error before any work begins — not discovered at the merge.
- **A binary that carries through says so at runtime.** It reports it when asked whether it is fit to work, and before a resolution crosses into the integrating role. A declaration made once, in configuration, is invisible to whoever invokes it later; explicit has to mean visible to the person acting on it, not merely present somewhere. That report is also the honest place to state what carrying through does not provide — it is not independent review — at the moment someone could still choose otherwise.

**The gate does not move.** Integration is still where verify is required, and it is required against **what will actually land** — the merge result, not the branch as it stood before it. Carrying through in one run shortens the path to integration; it does not remove anything from it. A change that cannot pass the gate does not merge, whoever is asking.

**What is genuinely lost is worth naming.** A single principal reviewing their own agent's work is not independent review, and nothing in this arrangement makes it so. It buys a second pass with different prompts against a different target, which has real value — but it is not a second opinion, and it should not be relied on as one. Where independent review matters, the roles belong to two principals — the arrangement the role boundary exists to serve, and the one the route performs by handing off.

## Where verify is required

`resolution.md` requires the gate exactly where changes are integrated into trunk. In this model, where that point sits depends on which role's steps the route reaches.

**The contributor role does not integrate — it proposes.** Its product is a pull request. Pushing is not integration here: the branch is pushed, and a branch is not trunk — the push makes the work visible for review, not part of the mainline. (Under a central orchestrator the same act *is* the integration, because there is no branch between the push and trunk. Same verb, different meaning, and the difference is which model you are in.) A contributor step that ran verify before opening the request has not discharged the integration gate; it has only avoided proposing something it already knew was unfit. That is still worth doing, and it is why a step may elect to run verify — but it is a courtesy to the reviewer, not the gate itself, and the flow does not treat its own green run as permission to land.

**Where the route reaches the integrating role's steps, the gate is inside the flow.** The step that merges requires the gate to pass against the merge result — what will actually land, the mainline having moved since the branch was measured — and a change that cannot pass it does not merge, whoever is asking.

**Where it does not** — the route hands the request to a human, or the binary holds no integrating role — the mandatory gate sits *outside* the flow, and the authority over trunk is whatever guards the merge.

The verify command is configured once and reaches both consumers — any step electing to run it, and the prompt describing it to an agent. A default that ships with the SDK is a default that must work in the repository shipping it: a default naming a convention the project has abandoned is a trap, because a consumer who sets nothing gets a configuration that looks valid and cannot run.
