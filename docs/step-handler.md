# Step handler interface

> **Tag:** `step-handler` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines what a step handler receives, what it must do, what it may do, and what it may return. Every statement is a requirement.

Signatures show types, not parameter names — a name is added only where two parameters share a type and the order matters. Every named type is [orchestrator.md](orchestrator.md)'s.

## Handler signature

```go
type StepHandler func(ctx StepCtx) (StepResult, error)
```

A handler is dispatched by the SDK for `AddStep` and `AddSignalStep` lifecycle items. `AwaitSignal` items have no handler.

A completed handler returns a `StepResult`: the route it elects — a declared successor or a permitted finalization, with the message to the successor — plus, for an artifact step whose capture is `returned`, the artifact payload, and optionally a standing note. The SDK captures the result, appends the journal entry, and the step is complete — result and route land together or not at all ([resolution.md](resolution.md) § The journal).

## Identity

Every handler receives a `StepCtx` that identifies where it is running:

| Method | Returns |
|---|---|
| `ctx.Flow() → string` | The flow name. |
| `ctx.Description() → string` | The lifecycle item's human description (the `description` argument to `AddStep`/`AddSignalStep`). Display only, and deliberately not called a name: it is never an identity, nothing accepts it where a `StepId` belongs, and granting, parking and the journal all key on `ctx.Result()`. |
| `ctx.Result() → StepId` | The result identifier — the step's `ArtifactId` or `SignalId`. This is the step's identity for ledger keying, park naming, and `status` reporting. |
| `ctx.Item() → *Item` | The orchestrator-supplied `Item` snapshot — the ref, type, title, body, creator and the rest of the fields [orchestrator.md](orchestrator.md) § `Item` defines. |
| `ctx.Claim() → *Claim` | The active `Claim` scoping this invocation. Read-only — owned by the orchestrator. |

## Who is here

| Method | Returns |
|---|---|
| `ctx.Runner() → AccountId` | The account this resolution acts as. |
| `ctx.Role() → RoleName` | The declared role this dispatch runs under — always the step's own role tag. |
| `ctx.RoleAccount(RoleName) → (AccountId, error)` | The account of record for a role on this item, derived from the journal — empty when the role has not yet acted. The role must be one the flow declares: an undeclared name is refused (`ErrUnknownRole`), never answered empty — empty means *declared and not yet acted*, and a typo read as that would wait forever. |

The item's creator is on `ctx.Item()`. A handler that routes on the creator's standing asks the orchestrator-detected facts, never the item body's claims about itself.

## Reading the journal

`ctx.Journal() → []JournalEntry` returns the item's journal whole — every completed step execution, in order, each entry carrying its step, result, elected route, message, standing note, principal, role, timestamps, and spend ([resolution.md](resolution.md) § The journal).

Convenience accessors answer the common questions without walking entries:

| Method | Returns |
|---|---|
| `ctx.Transfer() → *JournalEntry` | The entry that routed here: the predecessor and its message — why this step is being run. Nil on the entry step's first execution. |
| `ctx.Notes() → []JournalEntry` | Every entry carrying a standing note, in journal order — the notes addressed past their authors. |
| `ctx.Artifact(ArtifactId) → (JournalEntry, ok bool)` | The **current** value of an artifact: its latest journal entry, or `ok=false` when no execution of that step has completed. |
| `ctx.RunNumber() → int` | Which dispatch of the pending step this is — 1 on the first, counting each resume and retry since the route named it. Several dispatches may serve the one execution that eventually completes. |

### Typed accessors

Six typed accessors mirror the six artifact types, reading the current value as `ctx.Artifact` does. Each returns `ok=false` when the artifact is absent or carries a different type:

| Method | Returns |
|---|---|
| `ctx.Flag(ArtifactId) → (set bool, ok bool)` | Whether the flag's step has completed. |
| `ctx.CommitHash(ArtifactId) → (sha string, ok bool)` | The recorded commit. |
| `ctx.Markdown(ArtifactId) → (body string, ok bool)` | The markdown body. |
| `ctx.JSON(ArtifactId) → (body json.RawMessage, ok bool)` | The payload, in the id's declared shape. |
| `ctx.File(ArtifactId) → (name string, content []byte, ok bool)` | The named bytes. |
| `ctx.Patch(ArtifactId) → (body PatchBody, ok bool)` | The structured diff. |

A handler reads artifacts and writes none: its own artifact is captured from its `StepResult` or from the tree when it completes, and never exists before that ([artifacts-and-signals.md](artifacts-and-signals.md) § Capture).

## Reading signals

`ctx.Signal(SignalId) → bool` reports whether the signal is set. Handlers **cannot write signals**; a signal step's `StepResult` carries no payload, and one that does is refused (`ErrSignalNotWritable`).

## Reading park state

`ctx.ParkedOn() → *ParkRequest` returns the park this dispatch is resuming from, or nil when the step was not parked. It reads the state the orchestrator already loaded — a handler that needs it (to read the answers to a question it asked last time, for example) does not have to re-load the item.

## Electing the route

`StepResult` is built by constructors, and the election must be within the step's declaration ([flow-registration.md](flow-registration.md)):

| Constructor | Election |
|---|---|
| `ctx.Next(StepId, message string) → StepResult` | Route to a declared successor, with the message telling it why it is being run. |
| `ctx.Finalize(Disposition, message string) → StepResult` | End the item — `resolved` or `rejected` — where the step declares `MayFinalize`. |

Either may be extended with `.WithNote(string) → StepResult` — a standing note addressed to every subsequent step — and, on an artifact step whose capture is `returned`, with the payload constructor matching the declared type: `.Flag()`, `.CommitHash(sha string)`, `.Markdown(body string)`, `.JSON(json.RawMessage)`, `.File(name string, content []byte)`, `.Patch(PatchBody)`, each returning the extended `StepResult`. A payload not matching the declared type is refused at capture (`ErrTypeMismatch`); electing a route outside the declaration is refused the same way (`ErrRouteNotDeclared`), and nothing is journaled in either case.

## Sentinel returns

A handler may return these sentinels (as the `error`, with a nil `StepResult`) instead of completing:

| Method | Error type | Meaning |
|---|---|---|
| `ctx.Park(ParkRequest) → error` | `ErrPark` | Structured park request forwarded to `Orchestrator.Park`. **Never `question`:** a question park is answerable only through a question the orchestrator registered, this route registers none, and `answer` has no `QuestionId` to record against — so a `question` kind here **fails the step**, naming `ctx.AskQuestions`. A decision needed **asks**. |
| `ctx.AskQuestions(...AgentQuestion) → error` | `ErrQuestion` | One or more questions for the user. The orchestrator persists them; the flow parks until at least one is answered. |
| `ctx.WaitOnItems(...ItemRef) → error` | `ErrWaitsOnItems` | The step's work waits on those items — ones that exist, or ones it filed. Each reference is recorded as a blocker on the item through the orchestrator's editor, and then the step stops: the invocation reports `blocked`, kind `waits-on-items`, naming the blockers. Nothing is parked, nothing is journaled, and the treasurer does not count the dispatch ([resolution.md](resolution.md) § Blocked on items). At least one reference is required; a reference the orchestrator refuses to record fails the step, naming it, with the references already recorded left in place. A declaration naming only items that have already finished is recorded and then fails the step too, charged as a dispatch: the item reads unblocked, so the step stopped on nothing, and the next advance runs it. |

`ErrTransient` — returned (via `fmt.Errorf` wrapping) for infrastructure failures the handler observed. The orchestrator parks with `ParkInfraTransient`, and the treasurer does not count the attempt — a flapping runner does not spend the resolution's budget.

`ErrRefused` — returned for deterministic failures that cannot change on re-run (a repository guard refused a staged file, a required tool is out of date). The orchestrator parks with `ParkRefused`; the treasurer does not count the attempt.

**There is no skip sentinel.** A dispatched handler that stopped saying only "no progress right now" would be the inert stop [resolution.md](resolution.md) § Every outcome leads somewhere forbids: a state naming nothing that would clear it, re-dispatched into an identical stop. Every case it seems to cover decomposes into an outcome that names its clearing — nothing left to do **completes**, with the finding in its message; a condition that clears itself **parks**, naming the condition; a decision needed **asks**. The `skipped` invocation status survives without it ([resolution.md](resolution.md) § Reporting): it reports an invocation stopped before any dispatch, and no handler produces it, because a skipped invocation is one in which no handler ran.

## Error semantics

- **`(nil, nil)`**: `ErrStepDidNotComplete`. A step that neither elected a route nor stopped for a named reason has not done its job.
- **Non-nil error** (not a sentinel): failure. The dispatch counts in the treasurer's ledger.
- **Sentinel errors** (`ErrPark`, `ErrQuestion`, `ErrWaitsOnItems`, `ErrTransient`, `ErrRefused`): handled specially as described above. None of them appends a journal entry.

## Drafts

A step that stops without completing may keep the artifact it was working toward — and the working state behind it — where its own next dispatch finds it:

| Method | Meaning |
|---|---|
| `ctx.Draft() → string` | Returns what this step kept on an earlier dispatch, or `""`. Loaded lazily and memoised. |
| `ctx.SaveDraft(string) → error` | Keeps work for the next dispatch. Returns `ErrUnsupported` when the orchestrator has no store. |

The draft is **scaffolding, not a result**: it completes nothing, decides nothing, is keyed by item and step, is read only when both match, is never published, and is cleared when the step completes. See [resolution.md](resolution.md) § Drafts for the full contract.

## The agent chokepoint

`ctx.Agent() → Agent` returns the SDK-metered `Agent`. This is the **only** route to spend on agent work, and every expense through it is approved by the treasurer before it is incurred ([resolution.md](resolution.md) § The treasurer). See [agent.md](agent.md) for the interface and permission modes.

## Worktree

`ctx.Worktree() → (Worktree, error)` lazily acquires and caches the orchestrator's `Worktree` for the active claim. The worktree is the local-git surface handlers use for branching, committing, pushing, running gates, and managing pull requests. See [orchestrator.md](orchestrator.md) for the worktree contract.

## Other context

| Method | Returns |
|---|---|
| `ctx.Context() → context.Context` | The context for cancellation/deadline — carrying the time allowance the treasurer set for this dispatch. |
| `ctx.VerifyCmd() → string` | The project verify command configured on the App, or `""`. |
| `ctx.Notify(step, detail string)` | Reports a sub-phase progress event to telemetry. Not a liveness signal, and never state — the journal decides, telemetry observes. |
| `ctx.RefreshItem() → error` | Reloads the item snapshot from the orchestrator. |

## Cross-references

- [resolution.md](resolution.md) — the journal, routing, roles, the treasurer, and drafts.
- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds and their capture.
- [flow-registration.md](flow-registration.md) — how steps and their routes are declared.
- [agent.md](agent.md) — the agent interface and permission modes.
- [orchestrator.md](orchestrator.md) — the identities, structures, worktree and orchestrator contract.
