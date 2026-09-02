# Backend contract

**Normative.** This document defines the boundary between the SDK and a backend, including which capabilities are required, which are optional, and what a backend may refuse. Every statement is a requirement.

## What a backend is

A backend is the pluggable storage and worktree boundary the SDK targets. Two exist: the GitHub backend (`pkg/backend/github`) and the tracker backend (closed-source). Both satisfy the same `Backend` interface.

## Required surface

Every backend implements the `Backend` interface. The methods group by concern:

### Declaration

| Method | Contract |
|---|---|
| `Name()` | Returns the backend's name. |
| `SupportedSignals()` | The set of `SignalDef` values this backend knows how to observe. The SDK validates every signal reference against this list at startup. |
| `SupportedArtifacts()` | The backend's canonical artifact schema: a closed, curated set of `ArtifactDef` values it knows how to record. The SDK validates every declared artifact against this set at startup — by id **and** type. See "Artifact schema ownership" below. |

### Discovery

| Method | Contract |
|---|---|
| `ListEligible(ctx)` | Returns candidate items in the backend's scope. Feeds the auto-select path. |

### Claiming

| Method | Contract |
|---|---|
| `Claim(ctx, ref, owner, overrides)` | Acquires an exclusive lease on the item. A claim is one-to-one in both directions (at most one item per worktree, at most one worktree per item). `overrides` names safety checks the operator chose to bypass. |
| `Release(ctx, claim)` | Relinquishes the lease. |
| `LookupClaim(ctx, ref)` | Read-only info about the current claim holder, or nil if unclaimed. |
| `LookupActiveClaim(ctx, owner)` | The backend-authoritative active claim held by `owner` right now, or nil. Single source of truth for "what am I currently working on?" |

### State

| Method | Contract |
|---|---|
| `LoadState(ctx, claim)` | Returns the `ItemState` snapshot: artifacts, signals, questions, and park record in one round. Signals are refreshed by backend-internal polling. |
| `SeedState(ctx, claim, artifacts)` | Pre-loads the artifact set and budget caps. **Must refuse a second seed** for the same item — mid-flight items are frozen against later flow-source changes. |
| `ResetSeed(ctx, claim)` | Clears the existing seed so the next `SeedState` succeeds. Operator-initiated only; the SDK never calls it automatically. May return `ErrResetSeedUnsupported`. |

### Artifacts

| Method | Contract |
|---|---|
| `ResolveArtifact(ctx, claim, id, body)` | Writes a handler-produced artifact value. There is no backend method for writing signals — signals are written by backend-internal side effects or the `LoadState` poll path. An empty body is a legal call (side-effect pattern). |
| `MarkStale(ctx, claim, id)` | Flips the stale bit on an artifact, causing its step to re-run. |

### Budget

| Method | Contract |
|---|---|
| `BumpInvocations(ctx, claim, key)` | Increments the invocation counter and resets prompts-this-invocation. |
| `BumpPrompts(ctx, claim, key)` | Increments the prompt counter. |
| `AddCost(ctx, claim, key, usd)` | Adds cost to the running total. |
| `Grant(ctx, claim, key, grant)` | Adds budget to the artifact record. **Must clear** a `ParkBudgetExhausted` park when the grant raises the parked step's offending axis above its consumption. Parks of any other kind, and grants too small to clear the cap, **must** be left in place. Use `GrantClearsPark` so every backend applies the same rule. |

### Parking and questions

| Method | Contract |
|---|---|
| `Park(ctx, claim, req)` | Records that a step stopped without completing, and why. |
| `AskQuestions(ctx, claim, qs)` | Records agent-asked questions on the item. The backend assigns each a unique id and persists the `AgentQuestion` payload. |

### Worktree

| Method | Contract |
|---|---|
| `Worktree(ctx, claim)` | Returns the local-git surface for the claim. See "Worktree surface" below. |

## Worktree surface

The `Worktree` interface is the local-git boundary handlers use via `ctx.Worktree()`:

| Method | Contract |
|---|---|
| `Branch(ctx, name, base)` | Ensures `name` is checked out. Creates off base (or HEAD). Idempotent. Errors on dirty tree. |
| `CurrentBranch(ctx)` | Returns the current branch name. |
| `Stage(ctx)` | Makes every change (including untracked files) visible to the next `CapturePatch`. The guarantee is the outcome, not a particular mechanism — a backend whose `CapturePatch` already accounts for untracked content legitimately implements this as a no-op. |
| `Commit(ctx, msg)` | Commits the tree whole. Every file not ignored is staged. |
| `Push(ctx)` | Publishes the branch. **May wait** — the backend serializes landing across everything sharing the mainline. Waiting is not failing. |
| `RevParse(ctx, rev)` | Resolves a revision to a commit SHA. Must answer `"HEAD"` and the item's base branch. Anything beyond is best-effort; a backend that cannot resolve arbitrary revisions returns an error rather than falling back to HEAD. |
| `Verify(ctx)` | Runs the project's verify command. Returns nil iff exit 0. **May modify the worktree** — it repairs what has one right answer, then measures. |
| `RunGate(ctx, name)` | Runs the named gate and reports what the runner observed. The SDK is the runner. A non-nil error means no gate was run and no outcome exists. A gate modifies nothing it measures. See [gates-and-commands.md](gates-and-commands.md). |
| `Judge(ctx, run)` | Asks the project whether a measurement is acceptable. The SDK never computes this itself. Only `OutcomeMeasured` may be judged. A non-nil error means no verdict exists and is never a refusal. |
| `CapturePatch(ctx)` | Produces a unified diff. Returning no bytes is legal — the content may live server-side. |
| `Request()` | Returns the `RequestManager` for pull-request operations, or nil when unsupported. |

### RequestManager

The optional pull-request surface exposed via `Worktree.Request()`:

| Method | Contract |
|---|---|
| `Open(ctx, base, title, body)` | Opens a pull request. May trigger backend signals (e.g. `pr-open`). |
| `Merge(ctx, url)` | Merges a pull request. |

Nil-safe helpers `flow.Open` and `flow.Merge` return `ErrRequestNotSupported` instead of panicking when `Request()` is nil.

## Optional capabilities

Each optional capability is a separate interface. A backend that does not support one simply does not implement it:

| Interface | Capability |
|---|---|
| `RefResolver` | Turn a user-supplied item id directly into an `ItemRef` without listing eligible items. |
| `Discoverer` | List items at any scope the operator asks for, with per-item availability, tags, and holder. Feeds `list`. The auto-select path **must never** call `Discover`. |
| `TagFilterer` | List eligible items carrying all given tags. Feeds `resolve --tag`. |
| `StateInspector` | Load an item's flow state read-only by `ItemRef`, with no claim. Feeds `status`. |
| `WorkInProgress` | Save/load/clear per-step work-in-progress records. Keyed by item and step. Never published. See [resolution.md](resolution.md). |
| `Finalizer` | Mark an item's flow run complete and release its claim. |
| `ManualTakeover` | Signal that the operator has taken manual control of an item. |
| `MergeResultPreparer` | Set up the tree to reflect the merge result so the gate measures what will actually land. |
| `PRFinder` | Look up the pull request for the current claim branch. |

## What a backend may refuse

- **A second seed.** `SeedState` must refuse when the item is already seeded. Mid-flight items are frozen against later flow-source changes.
- **An empty artifact body** (when the backend stores the bytes). A backend that carries content elsewhere verifies the side effect happened and fails naming what is missing.
- **A disabled item claim.** An item carrying the disabled label is refused.
- **A claim held by another.** Unless the operator passes `OverrideAlreadyHeld`.

## Artifact schema ownership

`SupportedArtifacts()` returns a closed, curated set. The `(ArtifactId, ArtifactType)` pair is a stable schema that multiple flows — even across projects — must agree on. Owning that schema in the backend keeps it coordinated; letting each flow invent artifacts ad hoc would push schema coordination onto the flows. A backend that can technically store any id (e.g. GitHub) still declares a curated set.

## Cross-references

- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds the backend stores.
- [step-handler.md](step-handler.md) — what a handler may do with the worktree.
- [github-schema.md](github-schema.md) — how the GitHub backend stores state on an issue.
- [resolution.md](resolution.md) — the lifecycle that calls backend methods.
