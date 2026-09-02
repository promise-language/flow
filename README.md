# flow

A Go SDK for **declarative, stateless-per-step automation against
task-tracking systems.** You write a flow as an ordinary Go binary; the SDK
turns each invocation into *one* advance-the-state step against a backend
item (a GitHub Issue out of the box, or any backend you plug in).

- **No server.** A flow binary is a single static `main()` that imports the
  SDK and calls `cli.Run(app)`. End users install the binary, authenticate
  their backend once, and run it.
- **Backends are pluggable.** This repo ships the reference GitHub Issues
  backend (`pkg/backend/github`). Implement `flow.Backend` to target any
  store — a server-backed tracker, a database, a queue. The flow-author API
  never changes.
- **Agents are pluggable.** The reference impl drives the [`claude`][claude]
  CLI via stream-json. Any other LLM CLI slots in by implementing
  `flow.Agent`.
- **Stateless per step.** Each `run-step` reads durable state from the
  backend, runs exactly one step, writes one durable artifact, and exits.
  All progress lives in the backend; the process keeps nothing. This is what
  makes the model survive restarts, crashes, and host churn.

[claude]: https://docs.anthropic.com/en/docs/claude-code

```
$ ./issue doctor             # verify backend prerequisites
$ ./issue list               # list items this flow can process
$ ./issue claim 42           # acquire a claim on item #42
$ ./issue run-step           # advance ONE lifecycle item (one prompt → one durable artifact)
$ ./issue run-step           # next lifecycle item
$ ./issue status             # read-only lifecycle checklist
$ ./issue grant              # item parked on a budget cap? top up that axis
$ ./issue release            # drop the claim
```

Each `run-step` is a single forward tick: inspect state, pick the first
pending lifecycle item, run its handler, persist the result, exit. Re-run
until the flow reports `done`, parks, or asks a question.

Architecture docs: [docs/](docs/).

---

## Contents

1. [Install](#install)
2. [Key concepts](#key-concepts) — the entities and how they relate
3. [Build a flow for a new project](#build-a-flow-for-a-new-project)
4. [The flow-author API](#the-flow-author-api) — `Flow`, `StepCtx`, artifacts, signals, budgets, questions
5. [Blueprint: a reliable working flow](#blueprint-a-reliable-working-flow) — the do-task lifecycle, validation gates, the commit↔push loop, parking, failure-loud invariants
6. [Implement a custom backend](#implement-a-custom-backend) — the `Backend` interface and optional capabilities
7. [Build the binary with forge](#build-the-binary-with-forge)
8. [CLI surface](#cli-surface)
9. [Repo layout](#repo-layout)
10. [License](#license)

---

## Install

Requires:

- Go 1.26+
- For the reference GitHub backend: the [`gh` CLI](https://cli.github.com/)
  authenticated against the target repository, and `git` on PATH for
  worktree ops
- Optional: `claude` CLI on PATH for any flow that calls `ctx.Agent()`

The SDK is a library — you embed it in your own flow binary:

```go
import (
    "github.com/promise-language/flow"
    "github.com/promise-language/flow/claude"
    "github.com/promise-language/flow/cli"
    ghbackend "github.com/promise-language/flow/pkg/backend/github"
)
```

---

## Key concepts

A flow is built from a small set of strictly-separated entities. Keeping
them separate is what makes the model reliable; conflating them is the root
of most bugs.

| Entity | Lifetime | What it is |
|---|---|---|
| **Item** | persistent | The unit of work (a GitHub Issue, a tracker task). Carries a `Type` (routes flow selection), a title/body, durable **artifacts**, **signals**, and questions. The backend supplies it; it is opaque to the SDK beyond these fields. |
| **Flow** | code | An ordered list of **lifecycle items** (steps) selected for an item by its `Type` and `RequireSignal` preconditions. The binary *is* the source of truth — no YAML. |
| **Step / lifecycle item** | code | One entry in a flow. Three kinds: an **artifact step** (`AddStep`, runs a handler that produces one artifact), a **signal step** (`AddSignalStep`, runs a handler whose side effect makes the backend set a signal), or a **pure wait** (`AwaitSignal`, no handler). |
| **Artifact** | persistent | A durable product of one step — a plan, a patch, a commit hash, a review summary. The handler calls `ctx.Resolve*`. A flow is *complete* when every required artifact is attached. |
| **Signal** | persistent | A backend-*observed* boolean (`pr-open`, `pr-merged`). Never handler-writable — the backend sets it from a side effect or a poll. |
| **Claim (lease)** | persistent | An exclusive binding **item ↔ arena**. "This item's work lives in this worktree." One arena holds at most one claim; one item is claimed by at most one arena. |
| **Arena** | long-lived | A worktree plus a stable identity. Where work physically happens. Must survive at least one item's full lifecycle (across any restarts), then may be reclaimed. For the GitHub backend the arena is simply the local checkout; `.flow/active.json` records the claim. |
| **Runner** | transient | (Server-backed backends only.) The process serving an arena. URL/port/PID churn on every restart; **resolve it from the backend every time** — never store it durably. The GitHub backend has no runner: the flow binary is self-contained. |
| **Agent** | per-call | The SDK's metered handle on an LLM CLI (`ctx.Agent()`). The single spend chokepoint. |
| **Budget** | persistent | Per-step caps on four axes (invocations, prompts/invocation, cost, timeout). Seeded once; only `grant` mutates them. |

### Cardinal rules

- **A claim is `item ↔ arena`** — never `item ↔ runner`, never `item ↔ user`.
  The owner is attribution only. The runner has *nothing* to do with the
  claim: releasing a claim does not change how a flow reaches a runner, and a
  runner is reachable with no claim at all.
- **One arena, at most one claim.** The worktree is dedicated to that one
  item until release; in-flight work (uncommitted changes between steps)
  lives in the tree.
- **The runner is transient — resolve it live.** Nothing durable stores a
  runner URL/port/token. A claim survives any number of runner restarts.
- **State lives in the backend, not the process.** Every `run-step` re-derives
  what to do next from `Backend.LoadState`. There is no in-memory step
  machine to lose.

### Three orthogonal item flags — don't conflate them

- **`status`** (backend domain state: `open` / `done` / `wontfix` / …) is the
  *outcome* of the work. It does **not** mean "no more flow work needed."
- **`finalized`** (`Item.Finalized`) is the single authoritative "this item is
  fully resolved, no more work of any kind." A finalized item is ineligible
  for all work; **running a flow on a finalized item is an error.** It is set
  exactly two ways: (1) a flow runs to completion — every required artifact
  attached — or (2) the item is abandoned (`wontfix`/`not_feasible`/…). Set
  via the optional `Finalizer.Finalize` backend capability.
- **`manual`** (backend-specific) means "not eligible for *automatic*
  dispatch, but a human may still run flows by hand."

> **The key decoupling:** "done for good" is the **`finalized` flag**, *not*
> the `status`. `status == done` asserts only that **the work is real and
> merged to origin** — it is set at the push step, and the flow keeps running
> (summary, inspect) afterward until it finalizes.

---

## Build a flow for a new project

A flow binary is ~100 lines: pick a backend, declare your artifacts, register
your steps as handlers, and hand the whole thing to `cli.Run`.

### 1. The smallest possible flow

```go
package main

import (
    "os"

    "github.com/promise-language/flow"
    "github.com/promise-language/flow/claude"
    "github.com/promise-language/flow/cli"
    ghbackend "github.com/promise-language/flow/pkg/backend/github"
)

func main() {
    backend, err := ghbackend.NewBackend(ghbackend.Config{
        BinaryName: "issue",
        VerifyCmd:  []string{"bin/verify"}, // your project's gate
        // Guard is what lets this publish anything at all. With none, reads
        // work and the first write refuses — see docs/disclosure.md.
        Guard: nil,
    })
    if err != nil {
        panic(err)
    }

    f := flow.NewFlow("fix", []flow.ItemType{"task"})
    f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
        resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
            Prompt: "Plan implementation of: " + ctx.Item().Title,
        })
        if err != nil {
            return err
        }
        return ctx.ResolveMarkdown(resp.LastText)
    })

    os.Exit(cli.Run(cli.App{
        Name:      "issue",
        Backend:   backend,
        Agent:     claude.New(),
        Artifacts: []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
        Flows:     []*flow.Flow{f},
    }))
}
```

Build it (`go build -o issue .`), then `./issue doctor` and `./issue list`.
`./issue claim <id>` is the first thing that writes, so it — and every step
after it — refuses until a guard is injected above.

### 2. The `cli.App` you assemble

```go
type App struct {
    Name      string             // binary name (defaults from os.Args[0])
    Backend   flow.Backend       // REQUIRED — storage + worktree boundary
    Agent     flow.Agent         // REQUIRED — what ctx.Agent() returns
    Artifacts []flow.ArtifactDef // REQUIRED — every artifact id a step resolves
    Signals   []flow.SignalDef   // optional — every signal id a step references
    Flows     []*flow.Flow       // REQUIRED — at least one; registration order matters
    Telemetry flow.Telemetry     // optional — sink for ctx.Notify progress events
    Preflight flow.PreflightFunc // optional — cross-flow gate run before every dispatch
    Owner     string             // optional — claim attribution; defaults to $USER
    Out, Err  io.Writer          // optional — output streams
}

func Run(app App) int // os.Exit(cli.Run(app))
```

`cli.Run` validates the whole wiring at startup and refuses to start (named
error, non-zero exit) on: nil `Backend`/`Agent`; empty `Flows`/`Artifacts`;
a flow with zero steps; duplicate artifact/signal ids; an `AddStep`
referencing an unknown `ArtifactId`; an
`AddSignalStep`/`AwaitSignal`/`RequireSignal` referencing a signal not in
`Backend.SupportedSignals()`; or two flows that would ambiguously shadow each
other.

### 3. How a step is dispatched (`run-step` → `RunOne`)

Every `run-step` runs the same orchestrator (`cli/cmd_run.go` → `RunOne`):

1. **Resolve the active claim** via `Backend.LookupActiveClaim(owner)` — the
   single source of truth for "what am I working on." Never a local cache.
2. **`LoadState`** — artifacts + signals + questions in one snapshot
   (signals refreshed by backend polling).
3. **Preflight** (if configured) — a non-nil error short-circuits the
   invocation as `skipped`, no budget spent.
4. **Select the flow** — the first flow whose `Types()` match `Item.Type`,
   whose `RequireSignal` preconditions are all set, and that has a pending
   step. No pending step anywhere → **finalize** (via `Finalizer`, if the
   backend implements it) and report `done`.
5. **Seed once** — on an item with no artifacts yet, `SeedState` pre-loads
   the artifact set and per-step budget caps. Frozen thereafter (see
   [budgets](#step-budgets)).
6. **Budget gate** — refuse + park if invocations or cost are already
   exhausted.
7. **Dispatch** the handler under a `context.WithTimeout(step.Timeout)`,
   bumping the invocation counter *before* dispatch (a crash still counts).
8. **Translate the result** — the handler's (sentinel) error becomes an
   `InvocationResult{Status: done|skipped|failed|parked}` printed as JSON to
   stdout.

---

## The flow-author API

### Declaring flows and steps

```go
// types declares which Item.Type values this flow handles; nil/empty = universal.
func NewFlow(name string, types []flow.ItemType) *flow.Flow

// Artifact step: handler MUST call the matching ctx.Resolve* before returning nil.
func (f *Flow) AddStep(name string, result flow.ArtifactId, do StepHandler, opts ...StepOption)

// Signal step: handler does a side effect; the backend sets `signal`. Handler
// MUST NOT call Resolve*. Step completes when the signal is observed set.
func (f *Flow) AddSignalStep(name string, signal flow.SignalId, do StepHandler, opts ...StepOption)

// Pure wait: no handler. Completes when `signal` is set by any means
// (another flow's signal step, or an external event the backend observes).
func (f *Flow) AwaitSignal(name string, signal flow.SignalId, opts ...StepOption)

// Eligibility precondition (NOT a lifecycle item): this flow is only selected
// once `signal` is already set. Gate one flow on another's completion.
func (f *Flow) RequireSignal(signal flow.SignalId)
```

Multiple flows can live in one binary, distinguished by `Item.Type` and
`RequireSignal`. The canonical example is a contributor flow (plan → … → open
PR) and a maintainer flow (`RequireSignal("pr-open")` → review → merge) on the
same item — see [examples/issue/main.go](examples/issue/main.go).

### `StepHandler` and `StepCtx`

```go
type StepHandler func(ctx flow.StepCtx) error
```

The `StepCtx` is the handler's whole world:

```go
ctx.Context()        // the per-step context (carries the Timeout deadline)
ctx.Item()           // Item: ID, Type, Title, Body, URL, Finalized
ctx.Result()         // this step's result id

// Read prior artifacts (typed; ok=false if missing/unresolved/wrong type):
ctx.Markdown(id) / ctx.Patch(id) / ctx.CommitHash(id) / ctx.JSON(id) /
ctx.File(id) / ctx.Flag(id) / ctx.Artifact(id)
ctx.Signal(id) bool  // read a signal (handlers can't write signals)

// Write THIS step's artifact (exactly one, matching its declared type):
ctx.ResolveMarkdown(body) / ctx.ResolvePatch(body) / ctx.ResolveCommitHash(sha) /
ctx.ResolveJSON(raw) / ctx.ResolveFile(name, bytes) / ctx.ResolveFlag()

// Non-completion outcomes (sentinel errors the SDK translates to InvocationResult):
ctx.Skip(reason)                 // no progress possible right now
ctx.Park(req)                    // blocked / waiting; structured reason
ctx.AskQuestions(q1, q2, ...)    // park until the user answers (see below)
ctx.MarkStale(id)                // force a prior artifact to re-run

// Work in progress — what THIS step keeps when it stops without completing,
// so the next dispatch continues rather than restarts. Scaffolding, not a
// result: it resolves nothing, no other step reads it, it is never published,
// and it is cleared when the step resolves. Needs a backend implementing
// WorkInProgress; without one, reads are empty and the write returns
// ErrWorkInProgressUnsupported.
ctx.WorkInProgress()             // (body, error) — "" when nothing was stashed
ctx.RecordWorkInProgress(body)   // stash it for the next invocation

ctx.Agent()          // the metered LLM handle — the ONLY spend chokepoint
ctx.Worktree()       // lazily-acquired git surface for this claim
ctx.Notify(step, detail) // progress telemetry (NOT a liveness signal)
ctx.Claim()          // the active claim (read-only; pass to backend extras)
ctx.RefreshItem()    // re-pull item state mid-handler
```

The cardinal handler contract: an artifact step **must** call its `Resolve*`
before returning `nil`, or the SDK fails the invocation with
`ErrStepDidNotResolve`. Calling the wrong `Resolve*` returns `ErrTypeMismatch`;
calling any `Resolve*` on a signal step returns `ErrSignalNotWritable`.

### Artifacts: the six value types

Declared centrally in `App.Artifacts`, referenced by id from any flow:

| Type | Carries | Use for |
|---|---|---|
| `ArtifactFlag` | nothing (a "happened" marker) | "this step ran" |
| `ArtifactCommitHash` | a git SHA | recorded commits/merges |
| `ArtifactMarkdown` | text | plans, reviews, summaries |
| `ArtifactJSON` | `json.RawMessage` | structured results |
| `ArtifactFile` | named bytes | logs, screenshots |
| `ArtifactPatch` | `PatchBody` (diff + base SHA/branch, repo URL, untracked names) | the implementation diff — the **recovery record** |

```go
flow.Artifact("plan", flow.ArtifactMarkdown)   // ArtifactDef constructor
```

### Signals

```go
flow.Signal("pr-open", "a pull request for this item is open")
```

Signals are boolean, backend-observed, and read-only to handlers. A backend
declares the set it can observe in `SupportedSignals()`; `cli.Run` validates
every signal reference against it at startup.

### Step budgets

Every step is capped on four axes; exhausting any one parks the item with
`ParkBudgetExhausted` and the offending `BudgetAxis`.

| Axis | Default | `StepOption` | Where checked |
|---|---|---|---|
| invocations | `3` | `MaxInvocations(n)` | SDK pre-dispatch |
| prompts / invocation | `50` | `MaxPromptsPerInvocation(n)` | metered `Agent` wrapper |
| cost (USD) | `$20` | `MaxCostUSD(d)` | pre-dispatch + per `Agent.Run` + in-turn via `--max-budget-usd` |
| timeout | `30m` | `Timeout(d)` | `context.WithTimeout` |

The cost cap is not exact: a substrate learns what a model call cost only once
that call has returned, so a turn stops at the first call that crosses the
remaining grant. The overrun is bounded by one model call, not by a whole turn.

```go
f.AddStep("implement", "implementation", stepImpl,
    flow.Required,
    flow.MaxPromptsPerInvocation(5),
    flow.Timeout(60*time.Minute))
```

Other options: `Required` / `Optional` (cardinality for `IsDone`),
`StaleAfter(id)` and `StaleOnCommit` (re-run triggers when a dependency moves
or HEAD changes). Unspecified axes inherit the package defaults
`{3, 50, $20, 30m}` (`flow.DefaultStepBudget()`).

A park records the step by its **result id** (`plan`), not its label (`"write
plan"`), and `Backend.LoadState` surfaces it as `ItemState.Park` — that is what
lets `grant` with no arguments top up exactly the axis that parked the step.
`Backend.Grant` must clear a budget park once the grant gives that axis
headroom (see `flow.GrantClearsPark`); a grant too small to clear the cap
leaves the park in place and says so.

**Budgets are seeded once and frozen.** `SeedState` records the caps on the
item the first time it's processed; bumping `MaxInvocations` in your source
does **not** retroactively re-budget items already in flight. The only
post-seed knob is `grant` (additive). This is deliberate runaway protection:
the invocations cap catches "many fresh runs each exiting without progress";
the prompts/invocation cap catches in-step loops. The 50-prompt default
is a runaway backstop, not a tight budget — what a step actually costs is
bounded by `MaxCostUSD` and `Timeout`, and any in-step loop declared via
`MaxPromptsPerInvocation` overrides it visibly.

### Asking the user a question

A handler that needs a decision emits one or more questions and parks — it
does **not** print a question and stall:

```go
return ctx.AskQuestions(
    flow.AskChoice("DB", "Which datastore should this use?", "postgres", "sqlite"),
    flow.AskText("Deadline", "Any hard deadline I should know about?"),
)
```

Constructors: `AskText`, `AskYesNo`, `AskChoice`, `AskMultiChoice` (format and
options are presentation hints; the user can always reply free-text). The SDK
forwards them to `Backend.AskQuestions`, which assigns ids and persists them;
the flow parks until at least one is answered, then the step is **re-run from
scratch** with the answer available in `ItemState.Questions`. Because the step
re-runs from the top, **ask early** — before doing expensive work a re-run
would redo.

### Parking, skipping, and transient failures

The handler's return value drives the `InvocationResult.Status`:

- `nil` (after `Resolve*`) → `done`
- `ctx.Skip(reason)` / `ErrSkip` → `skipped` (no budget beyond the invocation)
- `ctx.Park(req)` / `ErrPark` → `parked` with a `ParkKind`
  (`blocked` / `question` / `budget-exhausted` / `step-did-not-resolve` /
  `infra-transient`)
- any other error → `failed`
- `flow.ErrTransient` (or an `Agent` failure with `Transient: true`) →
  `parked` as `infra-transient`, and the invocation counter is **not** bumped
  — a flapping runner must never burn a step's budget.

---

## Blueprint: a reliable working flow

This is the agreed model for driving an item from `open` to fully resolved,
distilled to the parts that any flow author should follow. It is what the
reference `do-task` flow implements.

### The lifecycle of a code-change flow

A flow selected for a `task`/`bug` item produces one artifact per step and
advances to the first unresolved one. The validation gate is your project's
verify command (`Worktree.Validate`, e.g. `bin/verify`); a passing
validation is always tied to the **exact tree/commit it ran against** —
changing the tree or rebasing invalidates it.

| # | Step | Kind | Produces | Notes |
|---|---|---|---|---|
| 1 | **plan** | agent | `plan` | Understand the task; or resolve as abandonment (sets `finalized`, ends the flow). |
| 2 | **implement** | agent | `implementation` (patch) | Write the change **in the worktree only** — no commit. **Validation must pass on the worktree.** Capture the diff immediately as the recovery record. |
| 3 | **review & fix** | agent | `review` | Real second pass: fix issues inline, file follow-ups. **If the tree changed, validation must pass again.** |
| 4 | **coverage** | agent | `coverage` | Aim for a coverage target (aspirational, not a hard gate). Keep validation green if tests change. |
| 5 | **commit** | agent | `commit` (hash) | Commit locally, then **fetch + rebase onto origin's head**, resolve conflicts. **If the tree changed or the rebase pulled commits, re-validate.** |
| 6 | **push** | command | `push` (hash) | **Precondition:** a passing validation on the exact current local head. Push. **Sets `status=done`** ("real and merged"). |
| 7 | **summary** | agent | `summary` | Read-only TLDR of what was done, in the **work session's lineage**. Runs after push so it can't drift. |
| 8 | **inspect** | agent | `inspection` | Independent **fresh session** with no memory of the work; judges only what landed in origin. Proposes follow-ups (never auto-files). |

On step 8 completing (all required artifacts attached) → the flow **finalizes**.

Two principles make this sequence robust:

- **Implementation is the most expensive step**, so it attaches the patch diff
  *immediately*. The work survives a lost worktree/arena without re-running.
- **Only the push step reaches origin, and only behind validation.** Nothing
  is merged until verify passes on the exact head being pushed.

### The self-healing commit↔push loop

Origin advances while you work. The push step's precondition is "validation
passed on *this* head." On a **push rejection** (someone landed commits
between your commit and your push), mark the `commit` artifact **stale** —
`StaleOnCommit` / `ctx.MarkStale("commit")` — and let the flow re-run it: a
fresh fetch + rebase + re-validate, then push again. This loop is the entire
reconciliation strategy; no special-casing in the push handler.

### Multi-repo (submodules)

Tasks routinely span a superproject and its submodules. Each repo has its own
verify gate; the superproject's gate calls each submodule's. Rules:

- **A submodule change is gated by its OWN verify**, not the superproject's —
  submodules are independent.
- **Commit depth-first** (innermost submodule → … → superproject): verify →
  commit → rebase per changed submodule, then bump the gitlink and commit the
  superproject.
- **Push is all-or-nothing, best-effort:** pre-flight every repo that needs a
  push (confirm each is fast-forwardable), then push **submodules first,
  superproject last**, so a pushed parent pointer always references a SHA that
  already exists on the submodule's origin.

### Stopping early — only for the right reasons

A step ends the flow early **only** on abandonment, blocked, or a pending
question — *never* because `status == done`. ("Done for good" is the
`finalized` flag, not the status.) Surfacing this wrong is a classic bug: if
your `IsTerminal` treats `done` as closed and your post-step check skips on
it, setting `done` at implement-time strands the work uncommitted. Set `done`
only at push, and gate "stop" on `finalized` / abandonment.

### Failure-loud invariants

Hold these and a flow self-corrects instead of silently stalling:

- **A claim that cannot acquire the lease fails loudly** (strict bijection:
  one arena ↔ one item). A refused acquire is an error, never a false
  "claimed."
- **A released claim stays released** across restarts — no stale on-disk
  snapshot may resurrect it.
- **Nothing reaches origin without passing validation** (the push gate).
- **Running a flow on a `finalized` item is an error.**
- **A claim starts only on a clean arena**, and **release leaves the arena
  clean** — worktree free of changes and not ahead of origin. The next claim
  begins from a known-clean state. (`--force` overrides the start check for
  recovery.)

### No hidden timeouts

Every time-related constant must be named, configurable, defaulted next to its
definition, logged when it fires, and surfaced in error messages by name.
Prefer waiting for work to finish over killing it: a false-positive kill that
strands a multi-wave pipeline is worse than a slow completion. The SDK follows
this — the step `Timeout` is an explicit per-step budget axis, captured as a
patch before parking — and your backend should too.

---

## Implement a custom backend

A backend is the pluggable storage + worktree boundary. Implement
`flow.Backend` and the SDK gives you the entire CLI, the orchestrator, budget
enforcement, and the flow-author API for free. Both the GitHub backend
(`pkg/backend/github`) and the proprietary tracker backend satisfy the same
interface.

### The required interface

```go
type Backend interface {
    Name() string
    SupportedSignals() []SignalDef               // validated against signal refs at startup

    // Discovery + claim lifecycle.
    ListEligible(ctx context.Context) ([]ItemRef, error)
    Claim(ctx context.Context, ref ItemRef, owner string) (Claim, error)
    Release(ctx context.Context, claim Claim) error
    LookupClaim(ctx context.Context, ref ItemRef) (*ClaimInfo, error)
    LookupActiveClaim(ctx context.Context, owner string) (*Claim, error) // source of truth for "what am I on?"

    // State load + one-shot seed.
    LoadState(ctx context.Context, claim Claim) (*ItemState, error)
    SeedState(ctx context.Context, claim Claim, artifacts []ArtifactSpec) error
    ResetSeed(ctx context.Context, claim Claim) error // the ONLY escape from "frozen after first seed"

    // Writes — artifacts only; signals are written by side effects / polling.
    ResolveArtifact(ctx context.Context, claim Claim, id ArtifactId, body ArtifactBody) error
    MarkStale(ctx context.Context, claim Claim, id ArtifactId) error

    // Budget counters — transactional with the artifact record.
    BumpInvocations(ctx context.Context, claim Claim, key string) error
    BumpPrompts(ctx context.Context, claim Claim, key string) error
    AddCost(ctx context.Context, claim Claim, key string, usd float64) error
    Grant(ctx context.Context, claim Claim, key string, g Grant) error

    Park(ctx context.Context, claim Claim, req ParkRequest) error
    AskQuestions(ctx context.Context, claim Claim, qs []AgentQuestion) ([]Question, error)

    Worktree(ctx context.Context, claim Claim) (Worktree, error)
}
```

Key contracts:

- **`Item.Type` must be non-empty** on every item you return — `cli.Run`
  selects the flow by it. (The GitHub backend derives it from a `type:<x>`
  label convention.)
- **`LookupActiveClaim` is authoritative.** The CLI never falls back to a
  local cache. If your store is offline, return an error rather than a stale
  read — that's what keeps "released stays released."
- **`SeedState` runs exactly once.** Refuse a second seed for the same item;
  mid-flight items are frozen against later flow-source changes. `ResetSeed`
  is the only operator-initiated escape (a backend with no seed concept may
  return `ErrResetSeedUnsupported`).
- **Signals are never handler-written.** Set them from a worktree side effect
  (e.g. opening a PR sets `pr-open`) or from a poll inside `LoadState`.

### The `Worktree` surface

```go
type Worktree interface {
    Branch(ctx, name, base string) (created bool, err error) // errors on dirty tree
    CurrentBranch(ctx) (string, error)
    Commit(ctx, msg string) error
    Push(ctx) error
    Validate(ctx) error                  // run the project verify command; nil iff it passes
    CapturePatch(ctx) (patch []byte, err error) // handler-driven; see below
    Request() RequestManager             // optional PR surface; nil if unsupported
}

type RequestManager interface { // pull-request operations
    Open(ctx, base, title, body string) (url string, err error)
    Merge(ctx, url string) error
}
```

Backends with no pull-request concept (e.g. one that commits straight to
main) return `nil` from `Request()`. Handlers use the nil-safe helpers
`flow.Open(ctx, wt, …)` / `flow.Merge(ctx, wt, …)`, which return
`ErrRequestNotSupported` instead of panicking.

`CapturePatch` is called by handlers only — the orchestrator never captures a
patch when a step's deadline fires, so a timeout park writes no patch artifact
(the spent work stays in the worktree for the rerun). Returning **no bytes is
legal**: a backend whose patches live server-side attaches the diff out-of-band
and has nothing client-side to return, and a tree whose work is already
committed has an empty `git diff HEAD` by definition. Those handlers still
resolve their patch artifact, passing an **empty `PatchBody`** — the SDK hands
it straight to `Backend.ResolveArtifact`, which verifies the evidence wherever
it actually lives and fails with its own message when it is missing. `cli`
never second-guesses an empty body.

### The `Agent` surface

```go
type Agent interface {
    Name() string
    Run(ctx context.Context, req AgentRequest) (*AgentResponse, error)
}
```

`AgentResponse.CostUSD` feeds the cost budget; set
`AgentResponse.Failure.Transient = true` for infrastructure failures so the
orchestrator parks-without-bumping. The reference `flow/claude` impl spawns
`claude --print --input-format stream-json --output-format stream-json` and
aggregates the event stream.

### Optional capabilities (interface assertions)

The CLI feature-detects these — implement the ones that fit your store:

| Capability | Method | Buys you |
|---|---|---|
| `RefResolver` | `ResolveRef(ctx, id) (ItemRef, error)` | `claim <id>` builds the ref directly (no `ListEligible` scan), and can claim any item regardless of current status. Implement when your ref *is* the id. |
| `StateInspector` | `LoadStateByRef(ctx, ref) (*ItemState, error)` | `status <id>` inspects any item read-only, with no claim. |
| `Finalizer` | `Finalize(ctx, claim) error` | On flow completion (or a completed manual run), the SDK marks the item finalized and releases the claim — instead of leaving it un-finalized with the lease held. |
| `WorkInProgress` | `Save/Load/ClearWorkInProgress(ctx, claim, step[, body])` | A step that parks — on a question, or on a refused write — keeps what it worked out, and its next dispatch continues from it. Key by **item and step**, read only when both match, and never publish it: a record that crossed items would feed one item's reasoning to another item's agent. |

### Preflight

Use `App.Preflight` (a `flow.PreflightFunc`, composable via
`flow.ChainPreflight`) for cross-flow, binary-wide gates that may change
between scheduling and dispatch — an operator flipping a "manual" flag, the
item closed mid-flight, claim-vs-state divergence. It runs after `LoadState`
and the terminal-done check, before seed/dispatch; a non-nil error marks the
invocation `skipped` with no budget spent. It is **not** the place to decide
"this item is terminal" — that's owned by flow selection.

### A reference: the fake backend

`pkg/backend/fake` is an in-memory `Backend` used by the SDK's own tests. Read
it as the minimal, correct implementation before writing your own.

---

## Build the binary with forge

A flow binary is a dev tool, and dev tools rot when they're hand-built with
scattered shell scripts. The companion **[forge][forge]** blueprint is the
recommended way to build, keep fresh ("updatable"), and gate a flow binary —
one in-repo Go module that compiles every tool into `bin/` and only recompiles
when its source changes.

[forge]: https://github.com/promise-language/forge

Scaffold it once into your project:

```bash
go run github.com/promise-language/forge/cmd/init@latest
./make          # compiles every cmd/<tool> (your flow binary included) into bin/
bin/verify      # the main quality gate
```

The pattern, and why it fits flow:

1. **One `./make`, all tools.** `./make` discovers each `cmd/<tool>/main.go`
   (including `cmd/issue/` — your flow binary) and compiles it into `bin/`.
   No per-tool config; discovery is by directory convention.
2. **Hash-based staleness = "updatable" binaries.** `./make` hashes each
   tool's source (FNV-128a) and recompiles **only** when it changed, so the
   binary in `bin/` is always the latest build of the source with near-zero
   overhead on repeat runs. After scaffolding, the project owns every line —
   forge is not a runtime dependency unless you import its `primitives/`
   helper lib.
3. **`bin/verify` is the gate your flow already calls.** Point the backend's
   `VerifyCmd` at it (`[]string{"bin/verify"}`) so the flow's `Validate` step
   and your pre-commit hook run the *same* check. forge's verify ratchets
   committed quality baselines (test count, coverage, leak count in
   `.baselines.json`) so metrics can only move the right way.
4. **Deterministic root resolution.** forge injects the repo root at link
   time (ldflags), so a flow binary run from any CWD — including a remote
   runner's worktree — reliably finds project files.

See the forge [blueprint][forge-blueprint] for the full file layout
(`tools/build/` with its own `go.mod`, the `make`/`make.cmd` wrappers, the
`.githooks/pre-commit`), the staleness check, and the ratchet system.

[forge-blueprint]: https://github.com/promise-language/forge/blob/main/docs/blueprint.md

---

## CLI surface

`cli.Run` dispatches these against `os.Args[1:]`:

| command | behavior |
|---|---|
| `doctor` | verify backend prerequisites (e.g. `gh` auth + repo push permissions); ✅ / ❌ |
| `list` | list items this flow can process |
| `claim <id>` (alias `lease`) | acquire an exclusive claim; resolves the ref via `RefResolver` or a `ListEligible` substring match |
| `run-step` | advance ONE lifecycle item; emit an `InvocationResult` JSON. Re-run until `done` |
| `resolve [<id>]` (alias `run-all`) | drive the FULL lifecycle: loop `run-step` until the item finalizes or the run stops (parked, skipped, or failed). With `<id>` claims it first; with no claim and no id, auto-selects `ListEligible()[0]`. Narrates progress on stderr; in JSON mode streams each step's `InvocationResult` to stdout |
| `status [<id>]` | read-only lifecycle checklist (uses `StateInspector` when there's no claim) |
| `grant` | read the item's park and top up **exactly** the axis that parked it. Refuses (writing nothing) when the park is not a budget cap — a question park is cleared by answering, not by granting |
| `grant --all [--invocations N] [--cost USD] [--prompts N]` | sweep every pending step, raising each axis to at least *consumed + headroom*. Steps that already have headroom are not written at all |
| `grant <step-id> --invocations N --cost USD --prompts N --timeout SECONDS` | additively extend one step's budget. `<step-id>` is the id passed to `AddStep` (e.g. `plan`) — the first column of `status`. The human label (`"write plan"`) is **refused**, as is a signal id (signal steps own no budget) |
| `release` | drop the claim |

`grant` validates its target against the item's flow before writing anything:
an unknown id, a label, a signal id, or an unseeded step is rejected with exit
2 and the list of legal ids. `--dry-run` prints the plan and writes nothing.

### Output modes

`status`, `list`, `grant`, and `resolve` render **human-readable text on a
terminal and JSON when stdout is piped or redirected** — so a tool driving a
flow binary never parses text meant for a person. `--json` / `--human` force
one for a single command; `FLOW_OUTPUT=json|human` forces it for the process.
Errors are always plain text on stderr with the exit code as the signal.

`resolve` is a stream, not a one-shot report, so it splits the two: its human
output is the per-step progress narration on **stderr**, which it prints in
both modes, and its stdout carries per-step `InvocationResult` objects and
nothing else — in human mode, nothing at all. That is why `resolve >
steps.json` fills the file while the terminal still shows progress, and why
`resolve --json 2>/dev/null` yields the machine stream alone. The mode is
still decided by stdout, never by stderr: bare `resolve 2>/dev/null` on a
terminal prints nothing at all, because a terminal stdout selects human.
`run-step` always emits its single `InvocationResult` JSON on stdout.

`status --json` is the machine view of the checklist: each step carries its
`id`, `label`, `kind` (`artifact|signal|await`), `state`
(`pending|resolved|stale|skipped`), and either a `budget` object or `null` —
`null` being the machine-readable "not a grant target". The item's `park`, when
set, names the step and axis, which is exactly what bare `grant` acts on.

`cli.Run` also handles help automatically, in both flag prefixes and the short
form: `<bin> --help` / `-help` / `-h` (or `<bin> help`) prints the command list,
and `<bin> <command> --help` (likewise `-help` / `-h`) prints that command's
usage and exits 0 without running it.

**Planned** (share the same `RunOne` orchestrator; not yet implemented):
- `auto` / `process` — bundle `claim` + `resolve` + `release` for cron-driven
  sweeps over a queue of eligible items.

---

## Repo layout

```
.
├── doc.go                  package doc
├── flow.go                 Flow, NewFlow, AddStep/AddSignalStep/AwaitSignal/RequireSignal, DeriveNext/IsDone/IsReady
├── step.go                 StepHandler + StepOption (Required/Optional, StaleAfter/StaleOnCommit, Max*/Timeout)
├── stepctx.go              StepCtx interface — typed read/Resolve* surface, Agent(), Worktree(), AskQuestions
├── artifact.go             ArtifactDef/ArtifactType (the six types), ArtifactRecord, PatchBody/FileBody
├── signal.go               SignalDef + SignalState
├── backend.go              Backend, Worktree, RequestManager, Item, Claim, ItemRef, ItemState; RefResolver/StateInspector/Finalizer/WorkInProgress
├── budget.go               StepBudget + defaults {3, 50, $20, 30m}
├── agent.go                Agent interface + AgentRequest/Response/Failure
├── preflight.go            PreflightFunc + ChainPreflight
├── errs.go wire.go         sentinel errors + ParkRequest/InvocationResult/Question/AgentQuestion
├── telemetry.go            Telemetry sink for ctx.Notify
├── cli/                    program CLI (app.go, cmd_claim/run/status/grant/release/doctor/list) + RunOne orchestrator
├── claude/                 reference Agent impl: spawns the claude CLI via stream-json
├── pkg/backend/fake/       in-memory backend for SDK tests (read this first when writing your own)
├── pkg/backend/github/     GitHub-Issues backend: state-comment index, claim race-lock, worktree, signal polling, orphan-branch artifact spillover
├── examples/verify/        minimal one-step "run go test" flow
├── examples/issue/         contributor (fix) + maintainer (merge) flows on one issue
└── docs/                   architecture docs
```

The reference **GitHub backend** stores all item state in comments (a single
machine-managed state comment carrying a YAML index, plus one append-only
comment per artifact), spills large artifacts to a `flow-artifacts` orphan
branch, and races claims via a two-phase label lock — no server, no body
read-modify-write. See [docs/archive/design.md](docs/archive/design.md) for the storage schema (archived; no normative document covers it yet — see #22).

## License

Dual-licensed under either:

- [MIT License](LICENSE-MIT)
- [Apache License 2.0](LICENSE-APACHE)

at your option.
