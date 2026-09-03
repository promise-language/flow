# Step handler interface

**Normative.** This document defines what a step handler receives, what it must do, what it may do, and the errors it may return. Every statement is a requirement.

## Handler signature

```go
type StepHandler func(ctx StepCtx) error
```

A handler is dispatched by the SDK for `AddStep` and `AddSignalStep` lifecycle items. `AwaitSignal` items have no handler.

## Identity

Every handler receives a `StepCtx` that identifies where it is running:

| Method | Returns |
|---|---|
| `ctx.Flow()` | The flow name. |
| `ctx.StepName()` | The lifecycle item's human label (the `name` argument to `AddStep`/`AddSignalStep`). |
| `ctx.Result()` | The result identifier — `ArtifactId` or `SignalId` as a string. This is the step's identity for budget keying, park naming, and `status` reporting. |
| `ctx.Item()` | The backend-supplied `Item` snapshot: ID, Type, Title, Body, URL, Flow, Finalized. |
| `ctx.Claim()` | The active `Claim` scoping this invocation. Read-only — owned by the orchestrator. |

## Reading artifacts

### Full record

`ctx.Artifact(id)` returns `(ArtifactRecord, bool)`. `ok=false` when the artifact is missing from state (not seeded, or unknown id).

### Typed accessors

Six typed accessors mirror the six artifact types. Each returns `ok=false` when the artifact is missing, unresolved, or carries a different type:

| Method | Returns |
|---|---|
| `ctx.Flag(id)` | `(set bool, ok bool)` |
| `ctx.CommitHash(id)` | `(sha string, ok bool)` |
| `ctx.Markdown(id)` | `(body string, ok bool)` |
| `ctx.JSON(id)` | `(body json.RawMessage, ok bool)` |
| `ctx.File(id)` | `(name string, content []byte, ok bool)` |
| `ctx.Patch(id)` | `(body PatchBody, ok bool)` |

## Reading signals

`ctx.Signal(id)` returns a boolean — whether the signal is set. Handlers **cannot write signals**; attempting to call any `Resolve*` on a signal step returns `ErrSignalNotWritable`.

## Reading park state

`ctx.ParkedOn()` returns the `ParkRequest` this invocation is resuming from, or nil when the item was not parked. It reads the state the orchestrator already loaded — a handler that needs it (to read the answers to a question it asked last time, for example) does not have to re-load the item.

## Resolving

One `Resolve*` call per artifact type. An artifact step handler **must** call exactly one before returning nil:

| Method | For type |
|---|---|
| `ResolveFlag()` | `ArtifactFlag` |
| `ResolveCommitHash(sha)` | `ArtifactCommitHash` |
| `ResolveMarkdown(body)` | `ArtifactMarkdown` |
| `ResolveJSON(body)` | `ArtifactJSON` |
| `ResolveFile(name, content)` | `ArtifactFile` |
| `ResolvePatch(body)` | `ArtifactPatch` |

Calling the wrong `Resolve*` for the step's declared artifact type returns `ErrTypeMismatch`. A signal step handler **must not** call any `Resolve*`.

## Sentinel returns

A handler may return these sentinels instead of (or before) resolving:

| Method | Error type | Meaning |
|---|---|---|
| `ctx.Skip(reason)` | `ErrSkip` | No progress possible right now. Invocation marked skipped. |
| `ctx.MarkStale(id)` | — | Marks an earlier artifact stale, causing its step to re-run. |
| `ctx.Park(req)` | `ErrPark` | Structured park request forwarded to `Backend.Park`. |
| `ctx.AskQuestions(qs...)` | `ErrQuestion` | One or more questions for the user. Backend persists them; flow parks until at least one is answered. |

`ErrTransient` — returned (via `fmt.Errorf` wrapping) for infrastructure failures the handler observed. The orchestrator parks with `ParkInfraTransient` and skips `BumpInvocations` — a flapping runner does not burn the step's invocation budget.

`ErrRefused` — returned for deterministic failures that cannot change on re-run (a repository guard refused a staged file, a required tool is out of date). The orchestrator parks with `ParkRefused` and skips `BumpInvocations`.

## Error semantics

- **`nil` without `Resolve*`** (artifact step): `ErrStepDidNotResolve`. The step has not done its job.
- **Non-nil** (not a sentinel): failure. The invocation is counted against the budget.
- **Sentinel errors** (`ErrSkip`, `ErrPark`, `ErrQuestion`, `ErrTransient`, `ErrRefused`): handled specially as described above.

## Work in progress

A step that stops without completing may leave what it worked out where its own next invocation finds it:

| Method | Meaning |
|---|---|
| `ctx.WorkInProgress()` | Returns what this step stashed on an earlier invocation, or `""`. Loaded lazily and memoised. |
| `ctx.RecordWorkInProgress(body)` | Stashes work for the next invocation. Returns `ErrWorkInProgressUnsupported` when the backend has no store. |

The record is **scaffolding, not a result**: it does not resolve the step, decides nothing about what runs next, is keyed by item and step, is read only when both match, is never published, and is cleared when the step resolves. See [resolution.md](resolution.md) for the full contract.

## The agent chokepoint

`ctx.Agent()` returns the SDK-metered `Agent`. This is the **only** route to spend agent budget. Budget (cost, invocations, prompts) is metered here. See [agent.md](agent.md) for the interface and permission modes.

## Worktree

`ctx.Worktree()` lazily acquires and caches the orchestrator's `Worktree` for the active claim. The worktree is the local-git surface handlers use for branching, committing, pushing, running gates, and managing pull requests. See [orchestrator.md](orchestrator.md) for the worktree contract.

## Other context

| Method | Returns |
|---|---|
| `ctx.Context()` | The `context.Context` for cancellation/deadline. |
| `ctx.VerifyCmd()` | The project verify command configured on the App, or `""`. |
| `ctx.Notify(step, detail)` | Reports a sub-phase progress event to telemetry. Not a liveness signal. |
| `ctx.RefreshItem()` | Reloads the item snapshot from the backend. |

## Cross-references

- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds and their types.
- [flow-registration.md](flow-registration.md) — how steps are declared.
- [agent.md](agent.md) — the agent interface and permission modes.
- [orchestrator.md](orchestrator.md) — the worktree and orchestrator contract.
- [resolution.md](resolution.md) — the lifecycle that dispatches handlers.
