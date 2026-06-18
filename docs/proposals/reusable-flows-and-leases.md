# Proposal: Reusable flow recipes, pluggable backends, and a generalized sequencing lease

**Status:** draft, not yet implemented
**Author:** initial design (promise + flow collaboration)
**Related:** [docs/design.md](../design.md), [docs/proposals/gates.md](./gates.md),
the reference contributor/maintainer flow in [examples/fix](../../examples/fix/main.go),
and the private `flow-sdk/doflow` + `flow-sdk/pkg/backend/tracker` (the tracker
consumer of this SDK, in the closed superproject).

---

## 1. Goal

Make the flow SDK ship a **reusable, backend-pluggable lifecycle library** — one
place that structures the canonical "resolve an item" flow (plan → implement →
review → coverage → land → summary → inspect) — that any project *configures*
(prompts, budgets, verify command) and *binds* to a backend (GitHub Issues, a
server-backed tracker, anything implementing `flow.Backend`). The flow structure
is fixed and shared; the bindings are swapped.

To make that library safe under concurrency, the SDK also grows **one
generalized sequencing primitive** (`flow.Lease`) that subsumes three needs that
today are either bespoke (the tracker's push-lease and arena ledger) or missing
(host-level exclusion for hardware-intensive steps):

1. the **arena lease** — exclusive, persistent ownership of a worktree;
2. the **merge/push lease** — serialize landings so concurrent merges don't
   chain the same branch head;
3. the **verify/host lease** — exclude concurrent hardware-intensive steps
   (verify, tests) on one machine.

End goal, in a consuming superproject (the promise compiler is the reference
consumer):

```
bin/flow/issue resolve #1
```

drives the full lifecycle against GitHub issue #1 — issue body/comments as
durable artifact storage, a PR as the land vehicle — with **no dependency on the
private tracker**, and *optionally* coordinating with the tracker (arena lease,
merge serialization, live telemetry) when one is reachable.

This proposal is implemented in **two places**:

- **the flow submodule** — the `flow/resolve` recipe library, the `flow.Lease`
  primitive + the host-local (`flock`) and GitHub-native arbiters, the
  `RequestManager` merge-primitive additions, the extendable step deadline, and
  the GitHub-backend bring-up (optional capabilities + correctness fixes +
  widened artifact set);
- **the consuming superproject** — the thin `issue` binary, its project prompts
  / budgets, and the *optional* tracker bindings (a `flow.Backend` decorator, a
  `flow.Telemetry` adapter, a tracker-backed `flow.Lease` arbiter), all
  quarantined so the OSS packages stay tracker-free.

---

## 2. Background: what already exists

The OSS substrate is already in place (see [docs/design.md](../design.md)):

- the universal `flow.Item`, the pluggable `flow.Backend` boundary, `flow.Agent`,
  `flow.Telemetry`, `flow.Worktree` + `flow.RequestManager`, and the optional
  capability interfaces `RefResolver` / `Finalizer` / `StateInspector` /
  `ManualTakeover` ([backend.go](../../backend.go));
- the `cli` surface — `doctor` / `list` / `claim` / `run-step` / **`resolve`** /
  `status` / `grant` / `release` — where `resolve` already loops `RunOne` to
  completion ([cli/cmd_resolve.go](../../cli/cmd_resolve.go));
- a reference **GitHub Issues backend** ([pkg/backend/github](../../pkg/backend/github));
- a **PR-based example flow** ([examples/fix](../../examples/fix/main.go)).

What is **missing** is the reusable recipe (today a flow author copies
`examples/fix` or carries a private equivalent like `flow-sdk/doflow`), and any
sequencing primitive in OSS (the tracker's push-lease/ledger are private and
bespoke).

> **Why not "copy doflow"?** The private `flow-sdk/doflow` *is* this lifecycle,
> but it is hard-bound to `*trackerbackend.Backend`: its step handlers bypass
> `flow.Worktree` and call tracker-only methods (`Status`, `Validate`, `Commit`,
> `Push`, `PushTarget`, `AcquirePushLease`/`ReleasePushLease`, smart-rebase)
> driven through the tracker's remote runner. The recipe must be **re-expressed
> on the OSS capability seams**, not copied. The good news: those tracker
> mechanics are not tracker quirks — serialized, retrying, conflict-resolving
> landing is a *general* property of merging concurrent independent work onto a
> shared origin, and this proposal makes it first-class (Parts 3–4).

---

## 3. Part I — Reusable flow recipes (`flow/resolve`)

### 3.1 Shape: one structure, two seams

`flow/resolve` is a Go package that defines the canonical lifecycle **once** and
exposes two seams:

```go
package resolve

// Config — what a PROJECT configures. No backend types here.
type Config struct {
    Name      string          // cli.App.Name + flow binary name (e.g. "issue")
    VerifyCmd Command         // the gate steps run + reference in prompts; Windows-aware
    FormatCmd Command         // optional pre-commit normalizer

    DefaultStepBudget flow.StepBudget          // per-axis baseline
    Budgets           map[string]flow.StepBudget // per-step overrides, keyed by artifact id

    Prompts    Prompts        // per-project agent prompt builders (see 3.5)
    Guardrails Guardrails     // project-configurable prompt prefix/suffix + resource prose

    CommitMessage func(*flow.Item) string // nil => DefaultCommitMessage

    // StepOverride optionally replaces a single step's handler while keeping the
    // rest of the structure — "replace a binding to fit the project, retain the
    // flow." Keyed by artifact id; nil entries keep the default handler.
    StepOverride map[string]flow.StepHandler
}

// Deps — what a PROJECT binds. All interfaces; swap freely.
type Deps struct {
    Backend   flow.Backend
    Agent     flow.Agent
    Telemetry flow.Telemetry  // nil => no-op
    Leases    LeaseSet        // arbiters per scope (see Part III); zero => degraded/no coordination
}

// BuildApp wires the canonical flows + artifacts + preflight from cfg and deps.
func BuildApp(cfg Config, deps Deps) cli.App
```

This mirrors how the private `doflow` already factors "shared structure" vs
"per-project seam," but with the backend as an injected interface rather than a
concrete type, and with the sequencing arbiters injected too.

### 3.2 The canonical step set (retained from doflow)

```
do-task   (ItemType task|bug):
    create plan            → plan           (markdown)
    implement              → implementation (patch)
    review and fix issues  → review         (markdown)
    fill coverage gaps     → coverage       (markdown)
    land                   → land           (commit-hash)   ← Part II/III
    summarize resolution   → summary        (markdown)
    inspect                → inspection     (json)

do-plan   (ItemType plan):
    create plan            → plan           (markdown)
    review and fix issues  → review         (markdown)
    file phase tasks       → phases         (json)
```

A project can add a maintainer/merge sub-flow gated on a signal (as
[examples/fix](../../examples/fix/main.go) does with `RequireSignal("pr-open")`)
when its land model splits contribution from merge.

### 3.3 Steps are written against the OSS capability seams

The single rule that makes the recipe backend-agnostic: **handlers never name a
concrete backend.** They use only `flow.StepCtx`, `ctx.Worktree()`
(`flow.Worktree`), the nil-safe `flow.Open` / `flow.Merge` over
`flow.RequestManager`, signals, and `ctx.AcquireLock(...)` (`flow.Lease`).

Because both reference backends implement `flow.Worktree`
(github's with a real `Request()`, the tracker's with `Request()==nil`), the
*same* `land` handler adapts itself:

- `Request() != nil` (GitHub): commit → push branch → open PR → drive the merge
  loop → await `pr-merged`;
- `Request() == nil` (tracker / direct push): commit → push to the target ref.

### 3.4 The `land` step is a first-class, smart, coordinated step

Opening a PR does **not** remove the landing problem — it relocates it to the
merge. By the time you merge, another arena may have already landed, so the base
moved underneath you: non-fast-forward, possible conflicts, and stale
verification. The only correct resolution is the same one the tracker uses for
direct pushes:

> **Land loop (shared handler):**
> 1. acquire the **merge lease** for the target ref (Part III) — serialize
>    landings;
> 2. check mergeability; if the base moved → **update-branch / rebase onto the
>    latest base**;
> 3. on conflicts → an **agent conflict-resolution turn** (the rebase prompt);
> 4. **re-verify** the rebased tree (under the host verify lease, Part III);
> 5. **merge** → set the land signal (`pr-merged`) + record the land
>    (commit-hash) artifact;
> 6. on a race (someone landed between verify and merge) → **bounded retry** from
>    step 2;
> 7. release the merge lease on every exit path.

The per-backend part is only the **primitives** the loop calls:

| Primitive | GitHub | Tracker |
|---|---|---|
| merge lease | GitHub Merge Queue if enabled; else a `flow.Lease` arbiter (Part III) | push-lease ledger |
| rebase onto base | `PUT pulls/{n}/update-branch`, or local rebase + force-push | runner smart-rebase |
| mergeability / merge | PR `mergeable_state` + `PUT pulls/{n}/merge` (retry on 405/409) | direct push (head flip) |

If a repo has **GitHub Merge Queue** enabled, the github backend may delegate
steps 1–5 to it (enqueue → GitHub rebases, re-runs required checks, merges) and
the handler simply awaits the outcome. Either way the *step in `flow/resolve`* is
identical.

### 3.5 Prompt composition + project-configurable guardrails

Keep the existing two layers: a domain-agnostic skeleton in
[prompt](../../prompt/prompt.go) (partials, `{{.VerifyCmd}}`, item header, …) and
per-project prompt builders supplied via `Config.Prompts`. Add a
backward-compatible **guardrails** seam so a project can inject prompt
prefix/suffix prose and resource-limit guidance without editing the SDK:

```go
type Guardrails struct {
    Prefix string // prepended to every step prompt
    Suffix string // appended to every step prompt
    // Optional dynamic form, kept in sync with the enforced cap:
    Note func(step string, b flow.StepBudget) string
}
```

`prompt.Context` gains optional fields (`GuardrailsPrefix`, `GuardrailsSuffix`,
`GuardrailsNote`) populated before template execution; existing flows that don't
set them are unaffected.

### 3.6 Artifact vocabulary reconciliation

Retaining doflow's steps means the canonical artifact ids are
`plan, phases, implementation, review, coverage, land, summary, inspection`.
Because `Backend.SupportedArtifacts()` is a **closed set validated at startup**
(declaring an artifact a backend doesn't support is exit 2 — see
[design.md §Backend](../design.md)), the recipe must declare a vocabulary every
target backend supports. Action: the github backend's `SupportedArtifacts` is
**widened to a superset** of the canonical ids (today it lacks `phases` /
`inspection` and names the land artifact `merge-commit`). The recipe owns the
vocabulary; backends list it.

---

## 4. Part II — Pluggable backends

### 4.1 The contract

Backends implement `flow.Backend` (required methods) plus any of the optional
capability interfaces (`RefResolver`, `Finalizer`, `StateInspector`,
`ManualTakeover`) and supply a `flow.Worktree` (with an optional
`flow.RequestManager`). See [design.md §`flow.Backend`](../design.md).

This proposal **extends** the worktree surface with the merge primitives the
land loop needs (additive; existing single-shot `Merge` stays):

```go
type RequestManager interface {
    Open(ctx, base, title, body string) (url string, err error)
    Merge(ctx, url string) error

    // NEW — drive the smart land loop (§3.4). A backend that can't service
    // these returns ErrNotSupported; the land loop then falls back to a single
    // Merge (acceptable only where the base never moves).
    Mergeable(ctx, url string) (ready bool, reason string, err error)
    UpdateBranch(ctx, url string) (conflicts []string, err error)
}
```

### 4.2 GitHub backend — bring-up

The reference backend ([pkg/backend/github](../../pkg/backend/github)) already
implements every required `Backend` method (state in an issue comment; claim in
worktree-local `.flow/active.json`; local-git worktree; PR via `Request()`). For
the `resolve` UX and production trust it needs:

- **`RefResolver.ResolveRef`** — so `resolve #1` resolves any issue number to an
  `ItemRef` directly (today it falls back to list-and-match over the *eligible*
  set). **Required for the `resolve #N` goal.**
- **`Finalizer.Finalize`** — close the issue / set a `flow:done` label + clear
  the claim on completion (otherwise `resolve` ends at "no eligible flow"
  without finalizing).
- **`StateInspector.LoadStateByRef`** — claimless `status #N`.
- **`Mergeable` / `UpdateBranch`** (§4.1) — for the land loop.
- **Widened `SupportedArtifacts`** (§3.6).
- **Correctness fixes** surfaced during review: `AskQuestions` answers are never
  read back into `LoadState` (so parking-for-answer can't resume); `Park`
  reasons are recorded but never re-loaded; `ResolveArtifact` is non-atomic
  (comment then state-patch can orphan); `markSignalSetOnState` can reset all
  signals; the state-comment cache is never invalidated after a delete.
- **Live testing** behind `GH_INTEGRATION` (worktree/PR ops are exec-shelling
  `git`/`gh` with only HTTP mocks today).

### 4.3 Tracker backend — relationship

The private tracker backend stays on `flow-sdk/doflow` for now: its advanced
resilience (push-lease, smart-rebase, committed-ahead reconcile,
remote-unreachable parking) is richer than `flow.Worktree` exposes. It *can*
migrate onto `flow/resolve` later, once those mechanics are either expressed as
`flow.Lease` arbiters (push-lease → merge lease) and `RequestManager`
primitives, or kept as `Config.StepOverride` handlers. The recipe is built
reuse-ready regardless; GitHub is the first consumer.

---

## 5. Part III — A generalized sequencing lease (`flow.Lease`)

### 5.1 One primitive

The three needs differ on only **two axes**, both parameterizable; "persistent
vs transient" is not a different primitive, just a different *liveness rule*, and
"global vs host" is just a different *arbiter*.

```go
// Lease — the one primitive. Injected per arbiter; used uniformly by handlers.
type Lease interface {
    // Acquire blocks until `key` is held exclusively under `policy`, ctx
    // cancels, or (liveness-bound leases) the wait is abandoned. The returned
    // Handle carries a fencing token.
    Acquire(ctx context.Context, key string, policy Policy) (Handle, error)
}

type Handle interface {
    Token() uint64                            // fencing token, monotonic per key
    Heartbeat(ctx context.Context) error      // renew liveness (liveness-bound)
    Valid(ctx context.Context) (bool, error)  // have I been fenced out?
    Release(ctx context.Context) error
}

type Policy struct {
    Scope    Scope         // Global | Host
    Liveness Liveness      // DurableOwner | Heartbeat(ttl) | ProcessBound
    Owner    Owner         // DurableID(arena) | Process{machine, pid, bootID}
    MaxHold  time.Duration // hard ceiling even for a healthy holder (anti-monopoly)
    Reclaim  Reclaim       // Auto (expiry/death) | Manual (explicit break)
    Capacity int           // host semaphore capacity; 0/1 = exclusive
}
```

### 5.2 The three uses, one primitive

| Use | Logical key | Scope / arbiter | Liveness | Reclaim | Natural fence |
|---|---|---|---|---|---|
| **Arena** | arena id | Global store | DurableOwner (no TTL) | Manual break; re-attach on restart | bijection check |
| **Merge** | the **target ref** | Global store | Heartbeat-TTL **+** MaxHold | Auto | git base SHA / non-FF (free) |
| **Verify** | resource name (`verify`) | Host-local (`flock`) | ProcessBound + MaxHold | Auto (kernel releases on exit) | n/a (advisory) |

### 5.3 Keying model

The key is exactly the **unit of mutual exclusion = the mutable resource the
operation advances or consumes** — and *part of the key is implicit in the
arbiter's scope*. You never encode what the arbiter already scopes.

- **Host / verify:** key is just the **resource-type name** (`verify`), a
  constant. The host is implicit — the arbiter (a `flock` file) lives on the
  machine; you don't name the host, you *are* it. Host leases are therefore
  **local-only and not remotely acquirable**: a liveness-bound lock needs a live
  process to hold and heartbeat it, and for a host resource that process must be
  on the host. (If a big host supports K concurrent runs, the host arbiter is a
  counting semaphore of capacity K under the same name; K=1 = exclusive.)
- **Merge:** key is the **target ref being advanced** (`<repo>@<base>`, with repo
  implicit if the arbiter is per-repo). **Two leases on the same repo at
  different bases coexist** — merges into `main` and into `release-1.0` move
  different refs and don't chain the same head, so they run concurrently; only
  two merges into the *same* base serialize. Granularity is the caller's policy:
  default to the finest correct unit (the target ref); coarsen to per-repo only
  if a project's land touches cross-branch shared state. Cross-branch flows
  (land on `release-1.0`, then merge up to `main`) are just two sequential
  lock-takes.
- **Arena:** key is the durable arena id; fully explicit in the global store.

> **Corollary — remote-acquirability is derived, not declared.** A *global-store*
> arbiter is remotely observable/contended (the store outlives processes for the
> durable arena lease, or holds a heartbeat from a live process *anywhere* for
> the merge lease). A *host-local* arbiter is co-located with its holder by
> construction, hence local-only.

### 5.4 The anti-stuck invariant

> A lease is released by an authority **independent of the holder** — never by
> trusting the holder to release it.

Three independent reclaim mechanisms, one per liveness rule:

- **Heartbeat-TTL** (global transient): the holder renews (~10s); stopping —
  crash *or* machine offline, both stop the heartbeat uniformly — expires the
  lease after the TTL, making it reclaimable.
- **OS process-exit** (host-local): `flock` is released by the kernel when the
  fd closes / the process dies — PID-binding and crash-safety for free, no
  network.
- **Explicit break** (persistent arena): never auto-stolen (long work must not be
  yanked), but always operator/policy-recoverable (the tracker's existing
  `CheckLeaseBreakable` / `BreakLeaseSelf` shape); a restarted owner *re-attaches*
  via its durable id (`.flow/active.json`) rather than being locked out.

Transient leases carry **two independent expiries** (both *time-bound* and
*PID-bound*, as required): heartbeat-TTL catches death/offline; **MaxHold** caps
even a healthy-but-monopolizing holder so it cannot keep the lock perpetually.
**Fencing tokens** neutralize the zombie case (a holder stalls past TTL, another
acquires, the stalled one wakes); for merge this is *free* because a moved base
SHA makes the stale push non-fast-forward; for arena it's the bijection check.

### 5.5 Lock ordering

Because the land step holds the arena lease (persistent) and then acquires the
merge lease (global) and the verify lease (host), concurrent arenas could ABBA.
The `Lease` helper enforces a **total order on keys** (rank: host-local before
global, then lexical by key) and acquires in rank order. Handlers that follow
the helper cannot deadlock.

### 5.6 Arbiters (pluggable)

- **Global (durable + transient):** the tracker ledger when present (it already
  has both the arena ledger and the push-lease); a **GitHub-native** arbiter when
  tracker-free — merge → GitHub Merge Queue, or a lock-ref / lock-issue carrying
  TTL + fencing metadata; arena → `.flow/active.json` + a repo-side claim
  (label/assignee) for cross-machine visibility.
- **Host-local:** `flock` on a well-known per-host path — pure OSS, zero
  dependencies, works tracker-free, crash-safe by construction.

Arbiters are injected via `resolve.Deps.Leases`. A binary may wire different
arbiters per scope (e.g. host = flock, global = tracker-or-github-native).

### 5.7 Step timeout must pause during lock-wait

A step's `Budget.Timeout` is meant to bound **work**, but lock-wait is
**queueing** and is unbounded by nature. Today the deadline is a fixed
`context.WithTimeout(ctx, budget.Timeout)` ([cli/orchestrator.go](../../cli/orchestrator.go)).
Change it to an **extendable deadline** and route acquisition through the step
context:

- `ctx.AcquireLock(key, policy)` waits on a *separate* context (not the
  step-deadline context), measures the wait, and on success pushes the step
  deadline out by the measured wait. Net: `step_work = wall − Σ lock_wait`.
- Contention does not need the step timeout to bound it — it is bounded by the
  **current holder's** liveness (TTL / MaxHold / break). The wait is finite
  without inflating the timeout.
- `lock_wait` is surfaced as telemetry (operators see contention) separate from
  the work clock. The heartbeat keeps running while held, independent of the
  step clock. If the *guarded operation itself* (the verify, the rebase)
  overruns the work budget, that is a real, correct timeout.

---

## 6. Part IV — Optional tracker coordination (OSS stays tracker-free)

The OSS `flow/resolve` + `flow/pkg/backend/github` packages never import the
private tracker. Coordination is opt-in and lives entirely in the consuming
**binary** (which may legitimately depend on the private SDK). It uses only
existing OSS seams:

1. **Leasing + park/grant → a `flow.Backend` decorator** in the superproject. It
   wraps the github backend, delegates *every* method to it (github stays the
   source of truth), and additionally, **best-effort**, mirrors to the tracker:
   `Claim → arena lease`, `Release → release`, `Park → tracker.Park`, budget
   bumps / `AddCost → mirror`. Tracker failures log and continue.
2. **Live context → a `flow.Telemetry` adapter** over the tracker client.
   `cli` already forwards `ctx.Notify → App.Telemetry.StepProgress`.
3. **Coordination → a tracker-backed `flow.Lease` arbiter** (§5.6) injected as
   the *global* arbiter, so the arena and merge leases serialize across arenas
   through the tracker ledger when present; otherwise the github-native / flock
   arbiters are used.

**OSS-purity guard:** all tracker imports stay in the binary's `main` + a
superproject-internal package; add a CI grep that fails if `flow/resolve` or
`flow/pkg/backend/github` import the private SDK.

**Prerequisite for *full* park/grant mirroring:** the tracker lease ledger and
Park/Grant APIs key on tracker item ids, not GitHub issue numbers. Lease keys
and notes/cost are representable now (lease keys are free-form strings); full
park/grant *visibility* needs the tracker server to model an **external (GitHub)
work item** (a lightweight item whose source is `owner/repo#N`). Ship lease +
telemetry first; land full park/grant once the tracker can represent an external
item. (This is private-tracker work, owned by the tracker maintainers.)

---

## 7. Part V — Reference consumer: the `issue` binary

In the consuming superproject (the promise compiler), a thin shim — no flow
structure, only config + bindings:

```go
// flows/issue/main.go (superproject)
backend, _ := ghbackend.NewBackend(ghbackend.Config{
    BinaryName: "issue", VerifyCmd: []string{"bin/verify", "--wasm"}, DefaultType: "task",
})

leases := resolve.LeaseSet{Host: flock.New()} // host arbiter always available
if tk := tracker.Detect(); tk != nil {        // optional, env/config-driven
    backend = trackercoord.Wrap(backend, tk)  // decorator (§6.1)
    leases.Global = tk.LeaseArbiter()          // tracker serializes arena+merge
} else {
    leases.Global = ghbackend.LeaseArbiter(backend) // merge queue / lock-ref
}

deps := resolve.Deps{
    Backend: backend, Agent: claude.New(),
    Telemetry: trackercoord.TelemetryOrNil(), Leases: leases,
}
os.Exit(cli.Run(resolve.BuildApp(issueConfig(), deps)))
```

**Build wiring:** the superproject's `make` already auto-discovers any
`flows/<name>/` directory containing `.go` files and builds it to
`bin/flow/<name>` (hash-gated; staleness self-check via the embedded source
hash). Adding `flows/issue/` is therefore zero-config; `bin/flow/issue resolve #1`
results.

---

## 8. What lives where

**Flow submodule (this repo):**

- `flow/resolve/` — the recipe library (`Config`, `Deps`, `BuildApp`, the
  canonical flows, the smart `land` loop, guardrails).
- `flow.Lease` + `Handle` + `Policy` + the lock-ordering helper; the host-local
  `flock` arbiter; the GitHub-native arbiter.
- `flow.RequestManager` additions (`Mergeable`, `UpdateBranch`).
- `cli` orchestrator: extendable step deadline + `StepCtx.AcquireLock` (§5.7).
- GitHub backend bring-up: `RefResolver` / `Finalizer` / `StateInspector`,
  widened `SupportedArtifacts`, the merge primitives, the correctness fixes,
  `GH_INTEGRATION` tests.
- `prompt.Context` guardrail fields.

**Consuming superproject (e.g. promise):**

- `flows/issue/` — the thin shim + project prompts/templates/budgets
  (`issueConfig`).
- a superproject-internal `trackercoord` package — the `flow.Backend` decorator,
  the `flow.Telemetry` adapter, the tracker-backed `flow.Lease` arbiter (all
  importing the private SDK).
- leave `flows/do` untouched (stays on `flow-sdk/doflow`).

---

## 9. Implementation plan

1. **`flow.Lease` core** — interface, `Policy`, the host-local `flock` arbiter,
   the lock-ordering helper, unit tests with a fake arbiter. (Self-contained,
   no backend dependency.)
2. **Extendable step deadline + `StepCtx.AcquireLock`** in the orchestrator;
   tests that lock-wait does not consume the work budget.
3. **GitHub backend bring-up** — `RefResolver` / `Finalizer` /
   `StateInspector`, the merge primitives, widened artifacts, correctness fixes;
   live `GH_INTEGRATION` smoke (`doctor` → seed → run a single issue through to a
   PR on a scratch repo).
4. **GitHub-native lease arbiter** — merge via Merge Queue or lock-ref; arena via
   `.flow/active.json` + claim label.
5. **`flow/resolve`** — the recipe: `Config`/`Deps`/`BuildApp`, the canonical
   flows, the smart `land` loop, guardrails; handler tests against
   [pkg/backend/fake](../../pkg/backend/fake).
6. **`issue` binary** (superproject) — thin shim → `bin/flow/issue resolve #1`,
   tracker-free.
7. **Optional tracker coordination** (superproject) — decorator + telemetry +
   tracker lease arbiter; coordinate the tracker-server external-item work for
   full park/grant.

Submodule discipline throughout: push `flow` before the superproject; stage the
gitlink.

---

## 10. Risks / open questions

- **Land smarts are load-bearing.** The merge loop (serialize + rebase + reverify
  + bounded retry + conflict resolution) is where correctness lives; it must not
  regress to a single `gh pr merge`. GitHub Merge Queue, when available, is the
  most robust path and should be preferred.
- **Untested GitHub worktree/PR surface.** The happy path runs straight through
  exec-shelled `git`/`gh`; gate + fixture behind `GH_INTEGRATION` before trusting
  it in production.
- **Fencing coverage.** Merge is fenced for free by the base SHA; arena by the
  bijection; the host verify lease is advisory (a stale verify holder wastes a
  slot but cannot corrupt state). Confirm that's acceptable, or add a host fence.
- **Heartbeat under load.** A busy holder must still heartbeat (sidecar
  goroutine), or MaxHold/TTL must be sized above worst-case step duration so a
  healthy long step is not reclaimed mid-flight.
- **External-item modeling.** Full tracker park/grant for a GitHub issue needs the
  tracker server to represent an external work item; until then park/grant stays
  github-native.
- **Closed artifact set.** Declaring an artifact outside a backend's
  `SupportedArtifacts` is a startup error; the recipe vocabulary and every target
  backend's set must be kept in sync.
- **Manual recovery on GitHub.** There is no orchestrator auto-grant; a parked
  step needs human action (`issue grant` / a comment). Size
  `MaxPromptsPerInvocation` so healthy steps finish without parking.
