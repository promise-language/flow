# Flow registration

**Normative.** This document defines how a flow declares its steps, the item types it handles, and its signal preconditions. Every statement is a requirement.

## What a flow is

A `Flow` is an ordered list of lifecycle items (steps and signal waits). It is constructed with `NewFlow(name, types)` and populated by registering lifecycle items in the order they should be advanced.

## Item types

The `types` argument declares which `ItemType` values this flow handles. An empty or nil slice means **universal** — the flow applies to all item types. When multiple flows are registered, the first whose `AcceptsType` matches the item's type wins.

## Three kinds of lifecycle item

Every lifecycle item is one of three kinds:

| Kind | Registration | Handler | Result written by |
|---|---|---|---|
| **Artifact step** | `AddStep(name, result, handler, cfg)` | Required — must call matching `ctx.Resolve*` | Handler |
| **Signal step** | `AddSignalStep(name, signal, handler, cfg)` | Required — must NOT call any `ctx.Resolve*` | Orchestrator (side effect or poll) |
| **Signal wait** | `AwaitSignal(name, signal, cfg)` | None | Orchestrator (side effect or poll) |

An artifact step's handler **must** call exactly one `Resolve*` matching the artifact's declared type before returning nil. A signal step's handler **must not** call any `Resolve*` — signals are never handler-writable (see [artifacts-and-signals.md](artifacts-and-signals.md)). An `AwaitSignal` item has no handler; it completes when the signal is set by any means.

## Step configuration

`StepConfig` is a plain data struct — every knob a step has is a named field:

| Field | Default | Meaning |
|---|---|---|
| `Optional` | `false` | When true, the step's result is not required for `IsDone`. |
| `Budget` | Zero value (inherits defaults) | Per-step caps; each zero-valued axis inherits `DefaultStepBudget`. |

## Budget

`StepBudget` carries four axes. Each zero-valued axis in the flow author's `StepConfig.Budget` inherits the package default:

| Axis | Default | Unit |
|---|---|---|
| `MaxInvocations` | 3 | Whole count |
| `MaxPromptsPerInvocation` | 50 | Whole count |
| `MaxCostUSD` | $20 | USD |
| `Timeout` | 30 minutes | Duration |

The set of axes is closed. See [resolution.md](resolution.md) for what happens when a step reaches a cap.

## Signal preconditions

`RequireSignal(signal)` adds an **eligibility precondition**. The flow is only selected for an item when all required signals are already set on that item. This is a gate on flow selection, not a lifecycle item — it does not appear in the step ordering and is not advanced.

`IsReady(state)` reports whether all preconditions are satisfied for a given `Item`.

## Ordering and completion

Steps are advanced in **registration order**. `DeriveNext(state)` returns the first unresolved lifecycle item. The next step is derived from durable state — which artifacts are resolved and which signals are set — never from execution history.

`IsDone(state)` returns true when every **required** lifecycle item is resolved. Optional items that remain unresolved do not prevent completion.

## Uniqueness invariants

Duplicate step names **panic** at construction. Duplicate result identifiers (an `ArtifactId` or `SignalId` used by more than one step) **panic** at construction. Empty names, empty result identifiers, and nil handlers (on steps that require one) also panic.

These are programming errors, not runtime conditions.

## Startup validation

At startup, the SDK validates every artifact and signal reference against the orchestrator:

- Every `ArtifactId` used by an `AddStep` must appear in `Orchestrator.SupportedArtifacts()`, with a matching `ArtifactType`. A mismatch is refused at startup (exit 2) rather than failing at resolve-time after a step has run.
- Every `SignalId` used by `AddSignalStep`, `AwaitSignal`, or `RequireSignal` must appear in `Orchestrator.SupportedSignals()`. An unknown signal is refused at startup.

## Seeding

`SeedSpec(artifactDefs)` returns the `ArtifactSpec` slice the orchestrator pre-loads at seed time. It reads the per-step `StepConfig` values (merged with defaults) for each artifact step. Signal steps and signal waits produce no seed entries — they have no budget record.

## Cross-references

- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds steps produce.
- [step-handler.md](step-handler.md) — what a handler receives and may do.
- [resolution.md](resolution.md) — the lifecycle that advances steps.
