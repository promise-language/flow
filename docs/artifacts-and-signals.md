# Artifacts and signals

> **Tag:** `artifacts-and-signals` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines the two result kinds and the vocabulary that depends on them. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it.

## The distinction

A step produces exactly one result. That result is either an **artifact** or a **signal**. The two differ in who writes them:

| | Artifact | Signal |
|---|---|---|
| **Written by** | A handler, via `ctx.Resolve*` | The orchestrator, via side effect or poll |
| **Payload** | Typed value (one of six shapes) | Boolean — set or unset, no payload |
| **Identity** | `ArtifactId` (named string) | `SignalId` (independent namespace) |

A handler **never** writes a signal. The orchestrator writes signals by observing external state (a pull request merged, a review approved) or as a side effect of a worktree operation (a PR opened). This asymmetry is load-bearing: a signal that a handler could set would be indistinguishable from an artifact with no payload, and the distinction between "the handler says it happened" and "the orchestrator observed it happen" would be lost.

## Artifacts

### Identity and type

`ArtifactId` is a named string, unique within an App's declared set. The `(ArtifactId, ArtifactType)` pair is the stable identity that multiple flows — even across projects — coordinate on.

### The type set

`ArtifactType` is a closed set. Adding a value is an SDK-version change. The six values, ordered from most primitive to most structured:

| Type | Payload | Resolve method |
|---|---|---|
| `ArtifactFlag` | None — "happened" marker | `ResolveFlag()` |
| `ArtifactCommitHash` | 40-char git SHA | `ResolveCommitHash(sha)` |
| `ArtifactMarkdown` | `text/markdown` body | `ResolveMarkdown(body)` |
| `ArtifactJSON` | Arbitrary JSON (`json.RawMessage`) | `ResolveJSON(body)` |
| `ArtifactFile` | Named bytes (`FileBody`) | `ResolveFile(name, content)` |
| `ArtifactPatch` | Unified diff with structured metadata (`PatchBody`) | `ResolvePatch(body)` |

Calling the wrong `Resolve*` for a step's declared type returns `ErrTypeMismatch`.

### Declaration

`ArtifactDef` declares an artifact's identity and type. `Doc` is a one-line description of the artifact's purpose; authoritative on the orchestrator's `SupportedArtifacts` entries.

```go
flow.Artifact("plan", flow.ArtifactMarkdown).WithDoc("Implementation plan.")
```

### The artifact body

`ArtifactBody` is the union written by `Orchestrator.ResolveArtifact`. Exactly one field is populated; which one is determined by the matching `ArtifactType`.

An empty body is a legal call. It is the side-effect pattern: the content was already attached out-of-band (a runner-captured patch, a commit) and the handler is saying "resolved — record me as done." An orchestrator that stores such content elsewhere decides emptiness itself: it verifies the side effect happened and fails with a message naming what is missing.

### The artifact record

`ArtifactRecord` is the durable per-item state returned by `LoadState`. It carries:

- Resolution state: `Resolved`, `Stale`, `Required`.
- The resolved value (when `Resolved`), in the field matching its type. `ArtifactFlag` has no payload — `Resolved && Type==ArtifactFlag` is itself the signal.
- Provenance: `ProducedAt`, `Version`, `ResolvedBy`.
- Budget caps: `GrantedInvocations`, `GrantedPromptsPerInvocation`, `GrantedCostUSD`, `GrantedTimeout`.
- Usage counters: `Invocations`, `PromptsThisInvocation`, `CostUSDSpent`, `LastRunAt`.

### Required vs. optional

Steps are **required by default**. `StepConfig.Optional` opts out. An optional artifact does not block `IsDone` — the flow completes without it.

An operator may also remove a required artifact from the checklist after seeding. When the orchestrator surfaces an `ArtifactRecord` with `Required=false` and the artifact is not yet resolved, the step is skipped: `DeriveNext` moves past it rather than dispatching a handler against an item the operator marked as not-required.

## Signals

### Identity

`SignalId` is a named string in an independent namespace from `ArtifactId`.

### Definition

`SignalDef` declares a signal with an id and a description. The description is for documentation and error messages; it is not load-bearing.

```go
flow.Signal("pr-merged", "The pull request was merged")
```

### State

`SignalState` records whether a signal is set, when it was observed, and by whom (orchestrator-specific principal; display-only).

### Never handler-writable

A handler cannot write a signal. The orchestrator writes signals through two paths:

1. **Side effect** — a worktree operation triggers it. Opening a PR sets `pr-open`; the orchestrator records `ObservedVia: "side-effect"`.
2. **Poll** — `Load` refreshes signals by querying external state. The GitHub orchestrator polls PR state and records `ObservedVia: "poll"`.

A handler that calls any `Resolve*` on a signal step receives `ErrSignalNotWritable`.

### The orchestrator declares supported signals

The orchestrator declares which signals it can observe via `SupportedSignals()`. The SDK validates every signal reference against this list at startup: a flow that references a signal this orchestrator cannot observe is refused at startup (exit 2) rather than stalling at runtime.

## Questions

Questions are the third kind of item-scoped durable state, distinct from both artifacts and signals.

An `AgentQuestion` is emitted via `ctx.AskQuestions`, which records each through `AskQuestion`. The orchestrator persists them; the flow parks until at least one is answered. Questions carry a presentation format (`text`, `yes_no`, `choice`) and optional structured options, but the user can always reply with free text regardless of format.

Answering does not resume the item. Resumption is a separate deliberate act — see [resolution.md](resolution.md).

## How resolution uses them

The next step is derived from which artifacts are resolved and which signals are set — never from execution history. This is what makes resolution resumable: an item picked up by a different worktree derives the same next step. See [resolution.md](resolution.md) for the lifecycle that builds on this vocabulary.

## Cross-references

- [resolution.md](resolution.md) — the lifecycle that consumes artifacts and signals.
- [flow-registration.md](flow-registration.md) — how a flow declares steps that produce artifacts or await signals.
- [step-handler.md](step-handler.md) — what a handler may read and write.
- [github-schema.md](github-schema.md) — how artifacts and signals are stored on a GitHub issue.
