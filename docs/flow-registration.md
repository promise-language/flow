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
| `Prompts` | Whether the step may prompt the agent — closed at two: `agent` or `none`. **Required on steps**; absent on signal waits, which dispatch nothing and panic if given one. A step declaring `none` is **mechanical**: a prompt attempted from it is refused before anything is sent, and the item parks naming the step and the call site that tried. |
| `Session` | Whether the step continues the resolution's agent session or begins a new one — closed at two: `continued` or `fresh`. **Defaults to `continued`.** Independent of `Prompts`: a step that never prompts still sits on the route the session travels, and may declare `fresh` to end it. The entry step always begins a new session; declaring `continued` on it is refused, because there is nothing to continue. |

`Needs` and `Leaves` are what make the resolution's branching story a declared, reviewable sequence: read along any route, each step's `Leaves` hands its successor the state its `Needs` requires, and a step that would run outside its declared state is refused before dispatch. There is one way the worktree reaches finalization, and it is written down at registration — not improvised route by route.

**A mechanical step runs no agent prompt.** `Prompts: none` is the whole of it — a restriction the step accepts, in the same shape `Writes` restricts what it may do to the worktree. It is not a different species of step: a step declaring `agent` may do everything a mechanical one does and may also prompt, so the two values are one axis with a restriction at one end, not two categories of work.

> **The restriction is a property of the step, not of a path through it.**

A step that prompts on one branch and not on the others declares `agent`, because that is what it is. There is no partial mechanical, and nothing here forbids such a step: it declares `agent`, it runs, and it simply earns none of the guarantees below. The model accommodates a mixed step; it does not reward one.

**Mechanical work inside a smart step is often the right thing to do.** A step that needs a command run, a file read, or a suite executed should do it itself and hand the agent the output — that costs a subprocess where handing the job to the agent costs a prompt, and the agent needs the output either way. `implement` is built this way: it runs the gate and re-prompts with the gate's own output rather than asking the agent to run the gate ([issue-flow.md](issue-flow.md) § Implement). Doing cheap work cheaply is not a design smell, and no rule here asks such a step to be split.

**Splitting is recommended for one shape, and the discriminator is which part is the deliverable.** What a whole mechanical step earns is three guarantees: it is **free** — no budget applies to it; **deterministic** — the same input gives the same result; and **cheap to retry** — a failure costs seconds rather than a prompt, so rationed backoff stops being load-bearing around it. None of the three survives a branch that prompts. Splitting costs a route, a journal entry, and a name, so it is worth paying only where those guarantees have a reader:

- **Split** when the **mechanical part is the deliverable** and the prompt is the exception — a long mechanical operation with a rare agent repair bolted onto its failure path. The operation is the point, it is what gets retried, and it is what a driver would rather run without waiting for quota; keeping the repair inside it denies the guarantees to the part that needed them, in exchange for saving one route.
- **Leave it whole** when the **prompt is the deliverable** and the mechanical work prepares it. A step whose only output would be an input to the next step is a route added to satisfy a rule nobody consults, and the preparation is cheaper done in the step than delegated.
- **Leave it whole** when nothing would read the guarantees anyway — the mechanical part is quick and retrying it is not a concern.

The recommendation exists because the first shape is easy to miss from inside a handler, where bolting a repair onto a failure path always looks cheaper than adding a step — and it is, for the author, exactly once.

**Required, because every default would be wrong.** `none` as a zero value would publish a guarantee no author wrote, to readers who cannot tell a considered declaration from an unset field. `agent` as a zero value would be safe and useless: the property exists to be relied on, and one that half the graph carries by accident is one nothing can rely on. So a step says which, the way it says its `Role`, and a registration that omits it is a programming error caught at construction rather than a silent answer.

**A declaration nothing checks is not one.** When a step declaring `none` tries to prompt, the attempt is refused **before anything is sent to the agent** — never after an answer has come back, because enforcement that billed for the violation it reports would falsify a mechanical step's guarantee of costing nothing in the one case the guarantee is load-bearing. The step parks rather than failing, so journal position and the claim survive for whoever corrects it, and the park names both the step and the call site: the fix is either that the handler should not be asking or that the step should not have declared `none`, and a reader cannot tell which to argue about without both. The park cannot clear by re-dispatch — a mis-declared step answers identically every time — so it carries a kind that says so ([orchestrator.md](orchestrator.md) § Vocabularies).

**It is not a budget field.** `Prompts` says whether the step may prompt at all; it says nothing about what a prompt may cost, and the paragraph below stands unchanged.

There is no budget field. What a resolution may spend is the treasurer's policy, held and configured with the treasurer — never in the step declarations it constrains ([resolution.md](resolution.md) § The treasurer).

There is no lock field either. Some operations genuinely serialize — landing shares one mainline, a gate may exclude its own kind, a heavy command may exclude others on its host — but the exclusion belongs to the resource, not to the step that happens to touch it, and the orchestrator acquires it inside the operation ([orchestrator.md](orchestrator.md): `Push` may wait, and the gate and landing locks are not the step's to name). Declared per step, a lock is one a new step forgets and two steps order into a deadlock; owned by the operation, no caller can miss it — and the treasurer's ledger records the wait wherever it bites, as waiting, never as work. Nor is any of this a signal: serialization is temporal, a turn arriving inside a running dispatch, not a fact about the world for the orchestrator to observe ([resolution.md](resolution.md) § The treasurer).

## Session continuity

**A resolution is one conversation, and its steps continue it.** The steps of a resolution work one item, on one branch, toward one change; an agent that has read the tree in `plan` has read it for `implement` too. So continuing is the default and the whole graph inherits it: a step is handed the session the resolution has been using, whether the step before it ran in this process or in a different `run-step` invocation on another day.

> **Continuing is the behaviour; a new session is the declaration.**

> **A new session happens only where a step declares `fresh`, and nowhere else.** Not at a process boundary, not on a park or a resume, not on a failure, not when the step before it ran mechanically, not when the resolution moves between `resolve` and `run-step`, and not because time passed. Every one of those carries the session across. The entry step opens the resolution's session because there is not yet one to carry; **every other new session in a resolution is one an author asked for by name.**

That is the whole of the rule, and it is stated as an absolute because the failures are all of one kind: something in the machinery decides, for a locally sensible reason, that this particular moment is special enough to start over. None of them is. A moment that deserves a new conversation is a property of the graph, and the graph says so at registration.

**A graph declares few of them, and the number is checkable.** **A flow whose routes open more than a couple is describing work that is not one piece of work**, and the declarations are where that shows: each one is a place an author decided the agent must stop knowing something.

How many a given resolution should have opened is read off its **journal**, not off the graph: a route may cross the same declaring step more than once — nothing here forbids a cycle, and the rework handback is one — so the graph offers no static total. The journal records one entry per execution, which is what the treasurer compares its count against ([resolution.md](resolution.md) § The treasurer). A count above what the journal accounts for is not an expense; it is something having started a session nobody asked for.

**`fresh` is declared for independence, not for hygiene.** The reason to give up the context is that the step's judgement must not be coloured by the reasoning that produced what it judges — a step inspecting a change should not be holding the deliberations that wrote it, because an agent shown its own rationalisations reliably agrees with them. That is a property of what the step is for, which is why it is declared at registration and not chosen per run. A step that merely does something different does not need `fresh`; it needs a prompt.

**Defaulting is right here, where `Prompts` forbids it.** The two look alike and are not. `Prompts: none` as a zero value would publish a **guarantee** — free, deterministic, cheap to retry — that no author wrote and that other parts of the system are entitled to rely on. `Session: continued` publishes no guarantee at all: nothing relies on a step having inherited context, and a step that wanted a fresh one and did not say so is wrong in a way its own output shows. The costs of forgetting run opposite ways too — an unset `Prompts` would hand out a promise nobody made, while an unset `Session` costs nothing and loses nothing.

**`Prompts` and `Session` are independent axes, and conflating them breaks the graph.** `Prompts: none` says the step runs no prompt. It says **nothing whatever** about the session, and it must not be read as saying anything:

> **A step that does not prompt does not disturb the session. It is carried across untouched.**

This is the rule most easily got wrong, and getting it wrong is worse than not having the mechanism. Mechanical steps sit *between* prompting ones — `open branch` runs between `plan` and `implement`, and the branch is opened precisely so the implementing conversation can continue where the planning one left off. An implementation that read "mechanical" as "no session" and cleared it would sever the conversation at the one point the declaration exists to preserve, and it would do so invisibly: every step would look correctly declared, and every resolution would quietly start over in the middle.

**A step may declare `fresh` without prompting.** Not prompting and not being a boundary are different facts. A mechanical step that marks the end of one line of work and the start of another declares `fresh`, and the next prompting step begins a new session — the declaration says where the conversation ends, and nothing requires the step that says so to be the one talking.

**The entry step always begins a new session**, whatever it declares. Nothing precedes it in this resolution, and what precedes it in the arena belongs to a different item — an entry step continuing whatever the last resolution was discussing would open every item with another item's reasoning, which is the failure [resolution.md](resolution.md) § Drafts closes for drafts by keying them, stated here for the conversation. Declaring `fresh` on it is redundant and true; declaring `continued` is refused, for the reason a signal wait declaring `Prompts` is refused — a declaration nothing could ever act on is a lie written where the truth was expected.

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
