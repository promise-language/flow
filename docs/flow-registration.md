# Flow registration

> **Tag:** `flow-registration` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how a flow declares its step graph, its roles, the item types it handles, and its signal preconditions. Every statement is a requirement.

## What a flow is

A `Flow` is a **graph of lifecycle items** — steps and signal waits — with one declared entry. It is constructed with `NewFlow(name, types)` and populated by registering lifecycle items; the routes between them are part of each item's declaration, and the order of registration means nothing.

**A binary registers exactly one flow.** What differs by item is the route through the graph, never which graph: heterogeneous processing is an entry step electing routes — on the item's type, the creator's standing, or anything else it reads — recorded in the journal with its reasons like every other decision. A choice of processing made by selecting among graphs would be the same decision made invisibly, before the journal begins, with nothing recording why.

The route an item actually takes through the graph is elected step by step at runtime, within the declared routes — see [resolution.md](resolution.md) § Routing. Registration is where the whole route space is written down, which is what makes the graph reviewable before anything runs: every path an item can take, every handback, every role boundary, and every way the flow can end are all in the declaration.

## Item types

The `types` argument declares the flow's **remit**: which `ItemType` values are this binary's work. An empty or nil slice means **universal** — every type is.

The remit gates listing and selection, and nothing else. It is what makes an item `outside-remit` or `processable` to this binary without dispatching anything — `list`, auto-selection, and claim refusal must answer *is this our work* statically, and a question that had to run a step to be answered could not feed a listing. It does not choose processing; what a type means for an item's route is the entry step's business.

**The remit is consulted before the journal's first entry, and never after.** Once an item has a journal, the route is the authority: editing the item's type mid-resolution redirects nothing — the pending step stays pending, and a step whose declared routes are type-sensitive reads the type when it runs. An operator who means "this item should be processed differently now" is asking for a reset, not a retype.

## Roles

A flow declares its **roles** before its steps: each a name, and the set of backend capabilities it requires.

```go
f.Role("contributor", flow.CapPush)
f.Role("maintainer", flow.CapPush, flow.CapMerge)
```

Every step is tagged with exactly one declared role. The roles a runner may assume are derived from its detected capabilities — see [resolution.md](resolution.md) § Accounts, capabilities and roles; nothing about that derivation is configured here.

A role may also declare **restrictions** — what it must not do regardless of which of its steps is running — and they join the action guard's union for every step the role performs, alongside the general rules and the step's own layer ([resolution.md](resolution.md) § Guards).

## Three kinds of lifecycle item

Every lifecycle item is one of three kinds:

| Kind | Registration | Handler | Result |
|---|---|---|---|
| **Artifact step** | `AddStep(description, result, handler, cfg)` | Required | The artifact, captured at completion |
| **Signal step** | `AddSignalStep(description, signal, handler, cfg)` | Required | The signal, observed by the orchestrator |
| **Signal wait** | `AwaitSignal(description, signal, cfg)` | None | The signal, observed by the orchestrator |

The first argument is a **description**, and the word is chosen against a failure: display text called a name gets treated as an identity, and an operation keyed on it — a grant, a resume — lands on a record that does not exist. The step's identity is its result id, everywhere and only ([orchestrator.md](orchestrator.md) § Identities); the description is for eyes.

An artifact step's handler ends by completing with a result matching the artifact's declared type — returned, or captured from the tree ([artifacts-and-signals.md](artifacts-and-signals.md) § Capture). A signal step's handler performs work whose result the orchestrator observes as its signal; it produces no artifact, because signals are never handler-writable. Both kinds elect their route as part of completing.

A **signal wait** has no handler and elects nothing. Its route is static: it declares exactly one successor, and when the route reaches it, the item waits — nobody's move — until the orchestrator observes the signal set, at which point the wait's journal entry is appended carrying the observation and the declared successor.

## Step configuration

`StepConfig` is a plain data struct — every property a step has is a named field:

| Field | Meaning |
|---|---|
| `Role` | The declared role that performs this step. Required on steps; absent on signal waits, which belong to no role. |
| `Entry` | Exactly one step in the graph carries `true`: where an empty journal starts. |
| `Next` | The step's declared successors — the only steps its handler may elect. |
| `MayFinalize` | The finalization dispositions this step may elect, if any ([resolution.md](resolution.md) § Finalizing). |
| `Capture` | For an artifact step, the result's source — one of two: `returned` by the handler, or read from the `tree` as the step left it ([artifacts-and-signals.md](artifacts-and-signals.md) § Capture). It declares where the one result comes from, never its shape — the payload's shape is the artifact's declared type. |
| `Needs` | The worktree state the step requires — closed at three: `any`, `base`, or `item-branch` (the item's resolution branch, checked out). Established mechanically before dispatch, from durable state, never trusted to be current; a state that cannot be established (the branch does not exist) blocks the item naming the step and the missing state. |
| `Writes` | What the step may do to the worktree while running — branch, commit, edit the tree. Enforced twice: as a layer of the action guard while the step runs, refusing the rest as it is attempted, and checked after the step runs against what actually happened ([resolution.md](resolution.md) § Guards). |
| `Leaves` | The worktree state the step must end in — closed at three: `as-found`, `base`, or `item-branch` — and always clean, which is the commit contract's guarantee rather than a fourth value ([resolution.md](resolution.md) § The commit contract). A step whose tree ends anywhere else has not completed: nothing journals, the item blocks naming the violation, and the changes stay in place for a person to judge. |

`Needs` and `Leaves` are what make the resolution's branching story a declared, reviewable sequence: read along any route, each step's `Leaves` hands its successor the state its `Needs` requires, and a step that would run outside its declared state is refused before dispatch. There is one way the worktree reaches finalization, and it is written down at registration — not improvised route by route.

There is no budget field. What a resolution may spend is the treasurer's policy, held and configured with the treasurer — never in the step declarations it constrains ([resolution.md](resolution.md) § The treasurer).

There is no lock field either. Some operations genuinely serialize — landing shares one mainline, a gate may exclude its own kind, a heavy command may exclude others on its host — but the exclusion belongs to the resource, not to the step that happens to touch it, and the orchestrator acquires it inside the operation ([orchestrator.md](orchestrator.md): `Push` may wait, and the gate and landing locks are not the step's to name). Declared per step, a lock is one a new step forgets and two steps order into a deadlock; owned by the operation, no caller can miss it — and the treasurer's ledger records the wait wherever it bites, as waiting, never as work. Nor is any of this a signal: serialization is temporal, a turn arriving inside a running dispatch, not a fact about the world for the orchestrator to observe ([resolution.md](resolution.md) § The treasurer).

## Signal preconditions

`RequireSignal(signal)` adds an **eligibility precondition**. An item is only begun when all required signals are already set on it. This is a gate on eligibility, not a lifecycle item — it does not appear in the graph and is never routed to.

`IsReady(state)` reports whether all preconditions are satisfied for a given `Item`.

## Routing and completion

The pending lifecycle item is derived from the journal: the route its last entry elected, or the entry step when the journal is empty. The flow is complete when a step elects finalization. There is no completion test beside the route — no checklist of required results, and no way to finish other than a step deciding to.

## Uniqueness invariants

Duplicate step descriptions **panic** at construction — two steps a listing renders identically are broken for the reader. Duplicate result identifiers (an `ArtifactId` or `SignalId` used by more than one lifecycle item) **panic** at construction. Empty descriptions, empty result identifiers, and nil handlers (on steps that require one) also panic. Declaring zero entry steps, or more than one, is refused at construction.

These are programming errors, not runtime conditions.

## Startup validation

At startup, the SDK validates the flow whole, before any item is claimed:

- **The graph.** Every identifier in every `Next` names a registered lifecycle item. Every item is reachable from the entry. Finalization is reachable from every item — a step from which no sequence of declared routes could ever end the flow is refused. Every signal wait declares exactly one successor.
- **Roles.** Every step's `Role` names a declared role, and every declared role names at least one capability. The role vocabulary is open, and that is why the check is closed: matching only ever happens against the declared set ([resolution.md](resolution.md) § Whose move it is), so a reference outside it — a typo in a tag, a lookup by a name nothing declared — is refused here rather than left to match nothing at runtime.
- **Artifacts.** Every `ArtifactId` used by an `AddStep` must appear in `Orchestrator.SupportedArtifacts()`, with a matching `ArtifactType`. A mismatch is refused at startup (exit 2) rather than failing at capture time after a step has run.
- **Signals.** Every `SignalId` used by `AddSignalStep`, `AwaitSignal`, or `RequireSignal` must appear in `Orchestrator.SupportedSignals()`. An unknown signal is refused at startup.

## Cross-references

- [resolution.md](resolution.md) — the journal, routing, roles, and the treasurer.
- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds and their capture.
- [step-handler.md](step-handler.md) — what a handler receives and may do.
