# Artifacts and signals

> **Tag:** `artifacts-and-signals` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines the two result kinds and the vocabulary that depends on them. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it.

## The distinction

A step produces exactly one result. That result is either an **artifact** or a **signal**. The two differ in who writes them:

| | Artifact | Signal |
|---|---|---|
| **Written by** | Captured from the step, at completion | The orchestrator, via side effect or poll |
| **Payload** | Typed value (one of six shapes) | Boolean — set or unset, no payload |
| **Identity** | `ArtifactId` (named string) | `SignalId` (independent namespace) |

A handler **never** writes a signal. The orchestrator writes signals by observing external state (a pull request merged, a review approved) or as a side effect of a worktree operation (a PR opened). This asymmetry is load-bearing: a signal that a handler could set would be indistinguishable from an artifact with no payload, and the distinction between "the handler says it happened" and "the orchestrator observed it happen" would be lost.

## Artifacts

### Identity and type

`ArtifactId` is a named string, unique within an App's declared set. The `(ArtifactId, ArtifactType)` pair is the stable identity that multiple flows — even across projects — coordinate on.

### The type set

`ArtifactType` is a closed set. Adding a value is an SDK-version change. The six values, ordered from most primitive to most structured:

| Type | Payload |
|---|---|
| `ArtifactFlag` | None — "happened" marker |
| `ArtifactCommitHash` | 40-char git SHA |
| `ArtifactMarkdown` | `text/markdown` body |
| `ArtifactJSON` | JSON in the id's declared shape (`json.RawMessage`) |
| `ArtifactFile` | Named bytes (`FileBody`) |
| `ArtifactPatch` | Unified diff with structured metadata (`PatchBody`) |

A result whose payload does not match the step's declared type is refused at capture (`ErrTypeMismatch`); nothing is journaled.

### Declaration

`ArtifactDef` declares an artifact's identity and type. `Doc` is a one-line description of the artifact's purpose; authoritative on the orchestrator's `SupportedArtifacts` entries.

```go
flow.Artifact("plan", flow.ArtifactMarkdown).WithDoc("Implementation plan.")
```

### A JSON artifact's id names one shape

`ArtifactJSON` is the one payload that exists to be read by programs, and a program cannot read whims: a JSON artifact whose shape floats with its producer is consumed by guesswork, and two captures of one id that disagree in shape are two artifacts sharing a name. So the shape is part of the declaration — bound to the `ArtifactId` where the artifact is declared, owned with the rest of the artifact schema ([orchestrator.md](orchestrator.md) § Artifact schema ownership) — and every capture is validated against it: a payload that does not fit the declared shape is refused at capture exactly as a wrong type is (`ErrShapeMismatch`), and nothing journals. How a shape is expressed is the SDK's — the Go type the payload must unmarshal into — but the property is not optional: an id declared `ArtifactJSON` with no shape is a defect of the declaration, not a license to the step.

The other payloads do not carry this rule, and the difference is the audience: markdown is prose for a reader, a file is named bytes opaque by design, a patch already has one structure. JSON alone is a machine interface, and an interface is a shape.

### Capture

**A step reads every artifact and writes none.** Prior results reach it through the journal; its own artifact does not exist until the step completes, and is captured exactly once, in the same act that appends the journal entry carrying it and the route elected. Mid-flight, what a step keeps is its draft — see [resolution.md](resolution.md) § Drafts — which is not an artifact and is never published.

The capture has two sources, and the step's declaration says which:

- **Returned.** The handler ends by returning the payload — the ordinary case for a result that exists only as the step's output: a plan, a briefing, a structured report.
- **From the tree.** The payload is read from the worktree as the step left it — the commit the branch head names, the patch the tree carries. This is the honest source for a result whose subject *is* the worktree: a returned copy of it could disagree with the tree it claims to describe, and the tree would be right.

An orchestrator that stores a captured payload elsewhere verifies the content it expects is present and fails naming what is missing — an empty capture is never silently recorded as a result.

### The record

An artifact's durable record is its step's **journal entries** — see [resolution.md](resolution.md) § The journal. Each **completed** execution of the step appends one entry carrying the captured value — completion is the only appending act, so a dispatch that parks, fails, or is resumed is one execution still under way, and writes nothing. The count of a step's entries is the artifact's version, and the latest entry's value is its current one. Earlier entries are never rewritten: a superseded value stays in the journal as the record of what a later reader — and every step that ran in between — actually saw.

There is no per-artifact status beside the journal: no resolved bit, no checklist, no required flag. Whether a step's artifact exists is whether the journal carries an entry for it; whether the flow is done is whether a step elected finalization.

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

### The orchestrator declares supported signals

The orchestrator declares which signals it can observe via `SupportedSignals()`. The SDK validates every signal reference against this list at startup: a flow that references a signal this orchestrator cannot observe is refused at startup (exit 2) rather than stalling at runtime.

## Questions

Questions are the third kind of item-scoped durable state, distinct from both artifacts and signals.

An `AgentQuestion` is emitted via `ctx.AskQuestions`, which records each through `AskQuestion`. The orchestrator persists them; the flow parks until at least one is answered. Questions carry a presentation format (`text`, `yes_no`, `choice`) and optional structured options, but the user can always reply with free text regardless of format.

Answering does not resume the item. Resumption is a separate deliberate act — see [resolution.md](resolution.md).

## How resolution uses them

The pending step is read from the journal — the route the last completed step elected — and every prior step's result reaches later steps the same way, as journal entries. Signals gate the route where the flow declares a wait, and eligibility where it declares a precondition ([flow-registration.md](flow-registration.md)). This is what makes resolution resumable: an item picked up by a different worktree reads the same journal and derives the same pending step. See [resolution.md](resolution.md) for the lifecycle that builds on this vocabulary.

## Cross-references

- [resolution.md](resolution.md) — the journal, and the lifecycle that consumes artifacts and signals.
- [flow-registration.md](flow-registration.md) — how a flow declares steps that produce artifacts or await signals.
- [step-handler.md](step-handler.md) — what a handler may read, and how its result is captured.
- [github-schema.md](github-schema.md) — how the journal and signals are stored on a GitHub issue.
