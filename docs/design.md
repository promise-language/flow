# Flow SDK — Design (`github.com/promise-language/flow`)

## Context

`github.com/promise-language/flow` is an open-source Go SDK (dual-licensed
MIT or Apache-2.0, at the user's option) for declarative, stateless-per-step
automation against task-tracking systems.
The flow author writes a Go binary that:

- Registers **artifacts** (handler-produced durable item state: markdown,
  files, commit hashes, etc.) and **signals** (backend-observed durable
  boolean state: PR open, PR merged, etc.).
- Declares one or more **flows**, each an ordered list of steps that produce
  those artifacts or trigger those signals.
- Embeds the SDK's CLI (`cli.Run`) which dispatches
  `claim | run-step | release | status | grant` against the registered
  flows. A future `run-all` (and a bundled "claim → run-all → release"
  command) are planned — they share the same orchestrator.

The SDK is **backend-pluggable**. Two backend implementations:

- **`pkg/backend/github`** (this repo) — the reference backend. Stores state
  in GitHub Issues (state comment + artifact comments + orphan-branch files
  for large artifacts), uses `gh` CLI for auth and PR ops, no server to host.
- **`pkg/backend/tracker`** — the proprietary tracker backend. Lives in the
  closed-source tracker repo (`~/prog/tracker`); imports this SDK and adds
  the tracker-server-backed `Backend` implementation plus the `do` flow
  registration. Not shipped here.

This doc covers the SDK design (universal — the shape both backends must
satisfy) and the github backend implementation (OSS-specific). The tracker
repo's `docs/flow-sdk-harness.md` covers the tracker-specific pieces and
imports this SDK as its dependency.

**Build order.** The OSS SDK is the canonical artifact: it defines the
`Backend` interface, all shared types (`flow.Flow`, `flow.StepCtx`,
`flow.Agent`, `ArtifactDef`, `SignalDef`, `ItemState`, `Claim`, …), the
`cli.Run` entry point, the budget enforcement, and the github backend.
The tracker repo then imports the SDK and adds only:
1. `pkg/backend/tracker` — the tracker-server `Backend` implementation.
2. `flows/do/main.go` — the do flow's registration (artifacts, signals, two
   flow variants), entirely in terms of SDK primitives.

---

## Locked decisions

| Question | Decision |
|---|---|
| Language | Go |
| Where flows / artifacts / signals are defined | In the flow binary, in Go. No YAML/config for these. The binary is the source of truth. |
| Step types | Handler steps (produce artifacts via `ctx.Resolve*`) AND signal steps (handler does a side-effect action; backend writes the signal as a side effect). A subset of steps may be purely waiting (`AwaitSignal`). |
| Agent integration | First-class in SDK root. `flow.Agent` interface + `flow.AgentRequest`/`AgentResponse`/`AgentFailure` types live in the root `flow` package alongside `flow.Backend` for consistency. Concrete impls live in subpackages (`flow/claude/`) so the root pulls in zero transitive deps. The SDK ships claude as the reference impl; others (codex, etc.) are pluggable. |
| Artifact storage on the issue | All in comments. A single "state comment" carries `<!-- flow:state-v1 ... -->` YAML index (machine-managed, wrapped in `<details>` so humans can fold it). Each handler-resolved artifact is its own comment with an HTML marker. **The issue body is never modified** after the original author posts it. |
| Signal state on the issue | Same state-comment YAML carries a signals section keyed by SignalId, written by the backend when observed. Source-of-truth is GitHub itself (PR state, labels, etc.); the comment holds the cached observation. |
| Large artifact bytes | Orphan branch in the same repo. Committed to `flow/artifacts/issue-N/<key>/<filename>` on a `flow-artifacts` orphan branch; comments link via `raw.githubusercontent.com`. No second repo, no external blob store. |
| GitHub auth | Shell out to `gh` CLI. Hard runtime dep on `gh`. |
| `.flow/` worktree state | Pointer + prefetched cache. Written by `claim` (alias `lease`), kept fresh by `run-step`, deleted by `release`. Contains `active.json` (the serialized `Claim`), `state.yml` (the parsed `ItemState`), and `artifacts/<key>.{md,bytes,json}` (prefetched contents). Handler reads via `ctx.Markdown/File/CommitHash/JSON/Patch(key)` are zero-API. |
| Multi-flow per binary | **Multiple flows per binary, distinguished by `Item.Type` and `RequireSignal` preconditions.** `cli.Run` picks the first flow whose constraints match per dispatch. Replaces the earlier "one binary = one flow" rule — needed to model contributor (open PR) vs maintainer (merge PR) lifecycles on the same issue. |
| Runner concept | Eliminated for github backend. The flow binary is self-contained; one lifecycle item per `./<binary> run-step` invocation. |

---

## User-facing usage

End user installs `gh`, authenticates once (`gh auth login`), then runs the
project-supplied flow binary:

```
$ ./fix doctor                       # verifies gh + auth + repo access
$ ./fix list                         # lists open issues this flow can process
$ ./fix claim 42                     # assigns me + labels for this flow binary
$ ./fix run-step                     # advances ONE lifecycle item on the claimed issue
$ ./fix run-step                     # advances the next item
$ ./fix status [<id>]                # read-only lifecycle checklist
$ ./fix grant <artifact-id> ...      # extend a parked step's budget
$ ./fix release                      # unassign / drop the claim
```

The `<artifact-id>` passed to `grant` is the id from `AddStep` (e.g.
`plan`), NOT the human-readable step name (e.g. `"write plan"`). `run-step`
without `--issue` reads `.flow/active.json` (written by `claim`); falls
back to "the open issue assigned to me with label `flow:<binary>`." If
multiple match, errors and asks for `--issue N`.

The CLI commands (`claim` / alias `lease`, `run-step`, `release`, `status`,
`grant`, `doctor`, `list`) all live in `cli/`; the binary opts in by calling
`cli.Run(app)`.

**Planned commands (not yet implemented):**

- `run-all` — loops `run-step` until the flow reports `done`, parks, or
  asks a question. Same orchestrator, same budget enforcement, same
  invocation result emitted per iteration; just removes the
  one-step-per-process-spawn shell overhead.
- A bundled `auto` / `process` command (name TBD) that wraps
  `claim → run-all → release` so a cron-driven runner can sweep a queue of
  eligible issues with one invocation per issue.

Both build on the same `RunOne` orchestrator the current `run-step` uses;
no shape changes to `flow.Backend` or `flow.StepCtx` are required.

---

## Architecture

### Artifacts and Signals — two distinct concepts

The SDK distinguishes two kinds of item-scoped durable state, with different
identity types, different registration channels, and different read/write
semantics:

| | **Artifact** | **Signal** |
|---|---|---|
| What it is | Something the flow PRODUCES and attaches to the item | Backend's OBSERVATION of external state |
| Identity type | `ArtifactId` (named string) | `SignalId` (named string) |
| Value types | Flag / CommitHash / Markdown / JSON / File / Patch | Boolean (set / unset) |
| Who writes | A flow handler, via `ctx.Resolve*` | The Backend, via side effect or poll |
| Read access | `ctx.Artifact(id)` / typed accessors | `ctx.Signal(id) bool` (read-only) |
| Registered in | `App.Artifacts []ArtifactDef` | `App.Signals []SignalDef` |
| Backend contract | Implementation detail of the Backend | Backend MUST declare each in `SupportedSignals()` |

The two namespaces are independent — an `ArtifactId` and a `SignalId` may have
the same string value without conflict, but for clarity flows should keep
them distinct (e.g. artifact `"implementation"` vs. signal `"pr-open"`).

#### Artifacts

```go
type ArtifactId string

// Item types — a typed alias so flow.NewFlow(..., []ItemType{...}) reads
// distinctly from a free-form string list.
type ItemType string

// Closed set of value types. Ordered from most primitive (no payload) to
// most structured. Adding a type is an SDK-version change.
type ArtifactType int
const (
    ArtifactFlag       ArtifactType = iota + 1  // "happened" marker — no payload
    ArtifactCommitHash                          // 40-char git SHA
    ArtifactMarkdown                            // text/markdown body
    ArtifactJSON                                // arbitrary JSON (json.RawMessage on the wire)
    ArtifactFile                                // named bytes ({Name, Content})
    ArtifactPatch                               // unified diff with structured metadata
)

// PatchBody — used when ArtifactType == ArtifactPatch.
type PatchBody struct {
    Diff       []byte    // unified diff bytes
    BaseSHA    string    // base commit the diff applies against
    BaseBranch string
    RepoURL    string    // origin URL for restoration context
    Untracked  []string  // names of untracked files (content NOT embedded)
}

// ArtifactDef — centralized declaration in App.Artifacts. Referenced by id
// from any number of flows; type is resolved from here at validation time.
type ArtifactDef struct {
    Id   ArtifactId
    Type ArtifactType
}

func Artifact(id ArtifactId, t ArtifactType) ArtifactDef
```

#### Signals

```go
type SignalId string

// SignalDef — centralized declaration in App.Signals. Signals are always
// boolean state; no value-type discriminator.
type SignalDef struct {
    Id          SignalId
    Description string  // for docs / UI / startup error messages; not load-bearing
}

func Signal(id SignalId, description string) SignalDef
```

#### Questions

Questions are a third kind of item-scoped durable state, distinct from
artifacts and signals: an agent or step handler emits one or more
`AgentQuestion`s, the backend persists them with assigned ids, and the flow
parks until at least one is answered. Format/Options are presentation hints
for the UI; the user can always reply with free-text "Other" regardless of
format. Multiple questions can be asked at once — useful when several
clarifications are needed before the next step can proceed.

```go
type QuestionFormat string

const (
    FormatText   QuestionFormat = "text"
    FormatYesNo  QuestionFormat = "yes_no"
    FormatChoice QuestionFormat = "choice"
)

// AgentQuestion is what a handler emits via ctx.AskQuestions.
type AgentQuestion struct {
    Text        string         // the full prompt
    Header      string         // short chip/label
    Format      QuestionFormat // presentation hint; user may always reply "Other"
    Options     []string       // choices for FormatChoice
    MultiSelect bool           // FormatChoice + multi-select allowed
}

// UserAnswer is the user's response. Free-form even for choice questions.
type UserAnswer struct {
    Answer     string
    AnsweredAt *time.Time
}

// Question is the recorded entry on an item: backend-assigned id + the
// agent's AgentQuestion + the user's UserAnswer (empty until answered).
type Question struct {
    ID string
    AgentQuestion
    UserAnswer
}

// Convenience constructors.
func AskText(header, text string) AgentQuestion
func AskYesNo(header, text string) AgentQuestion
func AskChoice(header, text string, options ...string) AgentQuestion
func AskMultiChoice(header, text string, options ...string) AgentQuestion
```

The handler emits questions via the sentinel `ctx.AskQuestions(qs...)`; the
SDK forwards them to `Backend.AskQuestions(ctx, claim, qs)` which assigns
ids and persists them. Subsequent `LoadState` returns them in
`ItemState.Questions`; `state.PendingQuestions()` returns the unanswered
subset.

### `flow.Flow` (registration surface)

A flow is an ordered list of **lifecycle items** — steps that produce
artifacts, steps that trigger signals, and pure signal waits. Each carries:

- `name string` — human-readable **action** ("create pull request", "review
  the implementation"). Unique within the flow.
- a **result** that's either an `ArtifactId` (for handler-produced state) or
  a `SignalId` (for backend-observed state).

```go
// `types` declares which Item.Type values this flow handles. nil or empty
// means "applies to all types — universal." A slice (not variadic) so call
// sites read as `NewFlow("merge", []flow.ItemType{"task", "bug"})` rather
// than the visually-confusing `NewFlow("merge", "task", "bug")`.
//
// Multiple flows may share a Name; cli.App picks the first flow whose
// Types() match item.Type AND whose RequireSignal preconditions are met
// AND has at least one pending lifecycle item.
func NewFlow(name string, types []ItemType) *Flow

// AddStep — handler-produced artifact. The handler MUST call the matching
// ctx.Resolve* (per the artifact's ArtifactType in App.Artifacts) before
// returning nil. Returning nil with an unresolved result is a bug; the SDK
// fails the invocation with ErrStepDidNotResolve.
//
// Duplicate `name` panics. Duplicate `result` (within this flow) panics.
// Unknown `result` (not in App.Artifacts) fails cli.Run validation at startup.
func (f *Flow) AddStep(name string, result ArtifactId, do StepHandler, opts ...StepOption)

// AddSignalStep — handler does a side-effect action (typically a worktree
// call: wt.Open, wt.Merge, wt.ApplyLabel, …) that causes the Backend
// to set `signal`. The handler MUST NOT call any ctx.Resolve* — signals
// are never handler-writable.
//
// Completion semantics: the step is COMPLETE when `signal` is set on the
// item, NOT merely when the handler returned nil.
//   - Synchronous side effect (common): Backend writes the signal as part
//     of the worktree call (Open writes pr-open atomically). Handler
//     returns nil; signal is already set; completion observed in this
//     same invocation.
//   - Asynchronous side effect: handler returns nil but the signal lags
//     (Backend confirms via a later LoadState poll). Step is NOT complete;
//     it stays in the flow's pending list. Subsequent cli.App runs check
//     the signal and DO NOT re-invoke the handler (re-running would
//     re-trigger the side effect — a duplicate).
//
// Failure paths: handler returns an error → step failed for this invocation
// (counts against MaxInvocations); SDK may re-invoke. If signal never sets
// within the step's Timeout, the step parks with BudgetExhausted.
//
// Duplicate `name` panics. Duplicate `signal` (within this flow) panics.
// Unknown `signal` fails cli.Run validation.
func (f *Flow) AddSignalStep(name string, signal SignalId, do StepHandler, opts ...StepOption)

// AwaitSignal — pure wait, no handler. The lifecycle item completes when
// `signal` is set on the item, by whatever means (another flow's
// AddSignalStep, or an external event the Backend observes — e.g. a human
// merging a PR manually). `name` is informational ("waiting for human merge").
func (f *Flow) AwaitSignal(name string, signal SignalId, opts ...StepOption)

// RequireSignal — eligibility precondition (NOT a lifecycle item). The flow
// is only selected by cli.App once this signal is already set on the item.
// Use this to gate one flow on another's completion (maintainer merge flow
// gates on contributor flow's `pr-open`).
func (f *Flow) RequireSignal(signal SignalId)

func (f *Flow) Types() []ItemType
func (f *Flow) Name() string
func (f *Flow) Steps() []string   // ordered registration list (state-independent)

type StepHandler func(ctx StepCtx) error
```

Naming convention:
- `AddStep` ↔ artifact, handler writes via `ctx.Resolve*`.
- `AddSignalStep` ↔ signal, handler does side effect; backend writes.
- `AwaitSignal` ↔ signal, no handler; flow waits.
- `RequireSignal` ↔ signal, eligibility gate; not in lifecycle.

### `flow.StepOption` (typed options)

```go
// Cardinality.
Required, Optional

// Re-run triggers.
StaleAfter(id ArtifactId), StaleOnCommit

// Step budgets (runaway protection). See "Step budgets" section.
MaxInvocations(n int)                           // default 3
MaxPromptsPerInvocation(n int)                  // default 1
MaxCostUSD(d float64)                           // default 10
Timeout(d time.Duration)                        // default 30m
```

Unspecified budget axes inherit the package defaults `{3, 1, $10, 30m}`. All
options compose on a single step. There is no per-step type filter
(type-specific step sets are expressed as separate flows). Budget options on
`AddSignalStep` / `AwaitSignal` are accepted (so a slow PR-create or long
external wait can be capped); `MaxPromptsPerInvocation` is moot on
signal-step handlers since they typically do worktree actions, not agent
prompts.

### `flow.StepCtx` (handler interface)

```go
type StepCtx interface {
    Context() context.Context
    Flow() string
    StepName() string                      // human-readable action label
    Result() ArtifactId                    // result id (or signal id, depending on step kind)
    Item() Item                            // backend-supplied: Type, Title, Body, …

    // Artifact read surface — full record + typed accessors. ok=false if
    // missing, unresolved, or wrong type.
    Artifact(id ArtifactId) (Artifact, bool)
    Flag(id ArtifactId)       (set bool, ok bool)
    CommitHash(id ArtifactId) (sha string, ok bool)
    Markdown(id ArtifactId)   (body string, ok bool)
    JSON(id ArtifactId)       (json.RawMessage, bool)
    File(id ArtifactId)       (name string, content []byte, ok bool)
    Patch(id ArtifactId)      (PatchBody, bool)

    // Signal read surface — handlers cannot write signals.
    Signal(id SignalId) bool

    // Artifact write surface — one Resolve* per ArtifactType. Calling the
    // wrong one for the step's declared result type returns ErrTypeMismatch.
    // Calling any Resolve* on a signal-step lifecycle item returns
    // ErrSignalNotWritable.
    ResolveFlag() error
    ResolveCommitHash(sha string) error
    ResolveMarkdown(body string) error
    ResolveJSON(body json.RawMessage) error
    ResolveFile(name string, content []byte) error
    ResolvePatch(body PatchBody) error

    // Sentinel returns — wrap typed errors the SDK translates to InvocationResult.
    Skip(reason string) error
    MarkStale(id ArtifactId) error
    Park(req ParkRequest) error

    // AskQuestions surfaces one or more questions for the user. Variadic so
    // single-question and multi-question call sites both read naturally.
    // The SDK forwards the questions to Backend.AskQuestions, which assigns
    // ids and persists them; the flow parks until at least one is answered.
    AskQuestions(qs ...AgentQuestion) error

    // Metered agent access — the ONLY spend chokepoint. Wraps cli.App.Agent.
    Agent() Agent

    // Worktree access — lazily acquires (and caches for the invocation)
    // the Backend's Worktree for this claim. Handlers call methods on the
    // returned Worktree directly: wt.Branch(...), wt.Commit(...),
    // wt.Open(...), wt.Validate(...), etc. No duplicate forwarders.
    Worktree() (Worktree, error)

    RefreshItem() error  // re-pull item state mid-handler if needed
}
```

The handler signature is `func(ctx StepCtx) error` with sentinel errors
carrying structured info (`ErrSkip`, `ErrPark`, `ErrQuestion`,
`ErrBudgetExhausted`). The SDK translates these to `InvocationResult` JSON.

### `cli.App` (program-level wiring)

```go
package cli

type App struct {
    Backend   Backend                   // REQUIRED — pluggable persistence/worktree
    Agent     flow.Agent                // REQUIRED — what ctx.Agent() returns
    Artifacts []flow.ArtifactDef        // REQUIRED — handler-resolved artifact defs
    Signals   []flow.SignalDef          // OPTIONAL — backend-observed signal defs
    Flows     []*flow.Flow              // REQUIRED — at least one; order matters
}

func Run(app App) int  // dispatches claim|run-step|release|status|grant against os.Args[1:]
```

**Flow selection.** On each `run-step`, `cli.App` scans `Flows` in order and picks
the **first** flow whose `Types()` either contain `item.Type` or is empty
(universal), whose `RequireSignal` preconditions are satisfied, and that has
at least one pending lifecycle item. Ties resolved by registration order.
Both flows persist as `Item.Flow = flow.Name()` regardless of which variant
ran (e.g. two `do` flows both serialize as `Flow="do"`).

**Validation at startup.** `Run` refuses to start (non-zero exit, named
error) if: `Backend`/`Agent` is nil; `Flows` or `Artifacts` is empty; any
flow has zero lifecycle items; duplicate artifact or signal ids; any
`AddStep` references an unknown `ArtifactId`; any
`AddSignalStep`/`AwaitSignal`/`RequireSignal` references an unknown
`SignalId`; any declared signal is not in `Backend.SupportedSignals()`; two
flows would shadow each other ambiguously (same Name + overlapping non-empty
type sets).

### `flow.Backend` (pluggable storage + worktree boundary)

```go
type Backend interface {
    Name() string

    // SupportedSignals — the set of SignalIds this Backend knows how to
    // observe. cli.Run validates every signal-ref against this list at
    // startup. Github backend returns {pr-open, pr-merged, pr-closed,
    // pr-approved}. Tracker backend returns an empty slice today.
    SupportedSignals() []SignalDef

    // ListEligible returns candidate items in the Backend's scope (github:
    // issues assigned to me with label `flow:<binary>`; tracker: items the
    // current host can work on). cli.App routes by Item.Type +
    // RequireSignal; flows that need finer filtering use ctx.Skip(reason).
    ListEligible(ctx context.Context) ([]ItemRef, error)

    // Claim/release lifecycle. Claim acquires an exclusive claim;
    // subsequent ops require the credentialed handle.
    Claim(ctx context.Context, ref ItemRef, owner string) (Claim, error)
    Release(ctx context.Context, claim Claim) error

    // LookupClaim returns read-only info about the current claim, or
    // (nil, nil) if unclaimed. The returned ClaimInfo cannot be used for
    // write ops.
    LookupClaim(ctx context.Context, ref ItemRef) (*ClaimInfo, error)

    // State load / one-shot seed. LoadState returns artifacts + signals in
    // one snapshot, with signals refreshed by backend-internal polling.
    LoadState(ctx context.Context, claim Claim) (*ItemState, error)
    SeedState(ctx context.Context, claim Claim, artifacts []ArtifactSpec) error

    // Artifact writes — handler-resolved only. No Backend method for
    // writing signals; signals are written by backend-internal side effects
    // (worktree actions) or by the LoadState poll path.
    ResolveArtifact(ctx context.Context, claim Claim, id ArtifactId, body ArtifactBody) error
    MarkStale(ctx context.Context, claim Claim, id ArtifactId) error

    // Budget counters — transactional with the lifecycle-item row.
    BumpInvocations(ctx context.Context, claim Claim, key string) error
    BumpPrompts(ctx context.Context, claim Claim, key string) error
    AddCost(ctx context.Context, claim Claim, key string, usd float64) error
    Grant(ctx context.Context, claim Claim, key string, g Grant) error

    Park(ctx context.Context, claim Claim, req ParkRequest) error

    // AskQuestions records one or more agent-asked questions. The backend
    // assigns each a unique id and persists the AgentQuestion payload;
    // returns the same questions populated with their assigned ids. The
    // flow parks until at least one is answered.
    AskQuestions(ctx context.Context, claim Claim, qs []AgentQuestion) ([]Question, error)

    Worktree(ctx context.Context, claim Claim) (Worktree, error)
}

type ItemState struct {
    Item      Item
    Artifacts map[ArtifactId]Artifact
    Signals   map[SignalId]SignalState
    Questions []Question        // outstanding questions; PendingQuestions() filters unanswered
}

type SignalState struct {
    Set        bool
    ObservedAt time.Time
    By         string     // backend-specific principal; display-only
}

type ClaimInfo struct {
    Owner     string
    ClaimedAt time.Time
}

type ItemRef struct {
    BackendName string          // matches Backend.Name()
    Display     string          // for logs / UI; e.g. "owner/repo#123"
    Ref         json.RawMessage // backend-internal addressing
}

type Claim struct {
    BackendName string          // must match Backend.Name() on reload
    ItemRef     ItemRef
    Owner       string
    ClaimedAt   time.Time
    Token       json.RawMessage // backend-internal credential
}

type Worktree interface {
    // Branch ensures `name` is checked out. If absent, creates off `base`
    // (or current HEAD if base==""). Idempotent: no-op if already current.
    // Errors on dirty tree. Returns true iff newly created.
    Branch(ctx context.Context, name string, base string) (created bool, err error)
    CurrentBranch(ctx context.Context) (string, error)

    Commit(ctx context.Context, msg string) error
    Push(ctx context.Context) error

    // Open / Merge may trigger Backend-internal side effects that set
    // signals (pr-open / pr-merged on github).
    Open(ctx context.Context, base, title, body string) (url string, err error)
    Merge(ctx context.Context, url string) error

    // Validate runs the project's verify command in the worktree (typically
    // bin/verify.sh: lint + vet + full test suite + memory-leak checks).
    // Returns nil iff verify exits 0. Universal contract: no change lands
    // in origin without verify passing.
    Validate(ctx context.Context) error

    CapturePatch(ctx context.Context) (patch []byte, err error)  // for timeout/park
}
```

**Item.Type is a Backend invariant.** Every `Item` the Backend returns MUST
carry a non-empty `Type` string — `cli.App` selects the matching flow by
this field. The github backend derives Type from a label convention (e.g.
label `type:task` → `Item.Type = "task"`); the convention is configurable
per binary.

**Claim semantics.** The "one claim per arena" constraint is enforced at the
**client** layer (`cli.Run`'s `claim` command checks `.flow/active.json`
exists before calling `Backend.Claim`), not at the Backend. The Backend
tracks claims by `(item, owner)`; a single owner holding multiple claims
across different worktrees is legitimate. The constraint is local to the
worktree.

### Derivation (in the SDK, shared by run-step AND status)

`Pending` / `DeriveNext` / `TerminalReason` / `IsReady` / `IsDone` live in
the SDK on `*Flow` and read from the `*ItemState` that `Backend.LoadState`
returns. The parity guarantee ("`status` and `run-step` can never disagree
about what's next") is structural — one derivation, called by both paths.

```go
func (f *Flow) Steps() []string                       // ordered registration list
func (f *Flow) Pending(s *ItemState, key string) bool
func (f *Flow) DeriveNext(s *ItemState) (key string, ok bool)
func (f *Flow) TerminalReason(s *ItemState) string
func (f *Flow) IsReady(s *ItemState) bool             // RequireSignal preconditions
func (f *Flow) IsDone(s *ItemState) bool              // all required items resolved
```

### Step budgets (runaway protection)

Every SDK-owned step is capped on four axes; exhausting any axis parks the
item with `ParkKind=BudgetExhausted`.

| Axis | Default | Bounds | StepOption | Where checked |
|---|---|---|---|---|
| `MaxInvocations` | `3` | Runs of this step over the item's life | `MaxInvocations(n)` | SDK pre-dispatch |
| `MaxPromptsPerInvocation` | `1` | `ctx.Agent().Run` calls within ONE invocation | `MaxPromptsPerInvocation(n)` | SDK metered Agent wrapper |
| `MaxCostUSD` | `$10` | Cumulative `AgentResponse.CostUSD` across all invocations | `MaxCostUSD(d)` | SDK pre-dispatch + per-call |
| `Timeout` | `30m` | Per-invocation wall clock | `Timeout(d)` | SDK `context.WithTimeout` |

**Why both invocations AND prompts-per-invocation.** Distinct failure shapes;
one cannot proxy for the other. Invocations cap catches "many fresh runs
each cleanly exiting without progress" (the original T0423 incident in the
tracker codebase); prompts-per-invocation catches in-step runaway loops.

**Why $ not tokens.** `AgentResponse.CostUSD` is returned directly by the
prompt API. Tokens are unreliable at the call site; $ is also the resource
we actually care about.

**Why metered on `ctx.Agent()` only.** Verify loops, gate runs, git ops,
file IO, and shell commands don't spend prompt budget. The metered Agent
wrapper is the sole spend chokepoint by construction.

**Defaults discourage in-step loops by making them visible.** With
`MaxPromptsPerInvocation=1` as the default, the standard step shape is "one
prompt → write durable artifact → return; SDK re-invokes if more is needed."
A step that needs a verify→prompt→verify loop must declare an override at
registration — making the loop visible in code review.

#### Budget state lives on the Artifact

```go
type ArtifactSpec struct {
    Id       ArtifactId
    Type     ArtifactType
    Required bool
    Budget   StepBudget       // resolved from AddStep options ∪ defaults
}

type Artifact struct {
    Id       ArtifactId
    Type     ArtifactType
    Required bool
    Stale    bool
    Resolved bool

    // Value — exactly one populated when Resolved, matching Type.
    // (Flag has no payload — Resolved && Type==ArtifactFlag is the signal.)
    CommitHash string
    Markdown   string
    JSON       json.RawMessage
    File       FileBody
    Patch      PatchBody

    ProducedAt time.Time
    Version    int
    ResolvedBy string  // backend-specific principal; display-only

    // Budget caps — pre-loaded at SeedState from the flow's StepOption
    // values (or package defaults). User grants ADD to these directly.
    GrantedInvocations          int
    GrantedPromptsPerInvocation int
    GrantedCostUSD              float64
    GrantedTimeout              time.Duration

    // Usage counters.
    Invocations           int
    PromptsThisInvocation int
    CostUSDSpent          float64
    LastRunAt             time.Time
}
```

Effective cap on each axis IS the `Granted*` value; exhaustion is
`usage >= Granted*`. A user grant action increments `GrantedInvocations` /
`GrantedCostUSD` / etc. directly. The flow source's compile-time defaults
exist only in code — once an item is seeded, its caps live entirely in these
fields, frozen against later flow-source changes.

#### Seed semantics: one shot, no re-seed

`Backend.SeedState(claim, spec)` runs exactly once per item, when the
Backend reports zero existing artifacts. The seed reads the flow's
StepOption values (or package defaults) and pre-loads them into
`Artifact.Granted*`. **The Backend MUST refuse a second seed for the same
item.** A flow-source bump (raising `MaxInvocations(3)` to `MaxInvocations(5)`)
does NOT apply to items already mid-flight; new builds only affect items
seeded after they ship. Grants are the only post-seed path.

#### Enforcement (in the SDK, one place each)

```go
func (f *Flow) runOnce(ctx context.Context, b Backend, claim Claim) error {
    state, _ := b.LoadState(ctx, claim)
    if len(state.Artifacts) == 0 {
        _ = b.SeedState(ctx, claim, f.seedSpec(state))   // ONE-shot
        state, _ = b.LoadState(ctx, claim)
    }
    next, ok := f.DeriveNext(state)
    if !ok { return finalize(ctx, b, claim, state) }

    art := state.Artifact(next)
    if art.Invocations >= art.GrantedInvocations {
        return b.Park(ctx, claim, ParkRequest{Kind: BudgetExhausted, Axis: "invocations", ...})
    }
    if art.CostUSDSpent >= art.GrantedCostUSD {
        return b.Park(ctx, claim, ParkRequest{Kind: BudgetExhausted, Axis: "cost", ...})
    }

    stepCtx, cancel := context.WithTimeout(ctx, art.GrantedTimeout)
    defer cancel()
    _ = b.BumpInvocations(stepCtx, claim, next)   // BEFORE dispatch — crash still counts

    err := f.dispatch(stepCtx, b, claim, next)

    if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
        if wt, e := b.Worktree(ctx, claim); e == nil {
            _, _ = wt.CapturePatch(ctx)           // retain spent work
        }
        return b.Park(ctx, claim, ParkRequest{Kind: BudgetExhausted, Axis: "timeout", ...})
    }
    return f.translateSentinel(err, b, claim)
}
```

The metered Agent wrapper:

```go
func (m *meteredAgent) Run(ctx context.Context, req AgentRequest) (*AgentResponse, error) {
    art := m.state.Artifact(m.step)
    if art.PromptsThisInvocation >= art.GrantedPromptsPerInvocation {
        return nil, ErrBudgetExhausted{Axis: "prompts", ...}
    }
    if art.CostUSDSpent >= art.GrantedCostUSD {
        return nil, ErrBudgetExhausted{Axis: "cost", ...}
    }
    _ = m.backend.BumpPrompts(ctx, m.claim, m.step)
    resp, err := m.inner.Run(ctx, req)
    if err == nil && resp.CostUSD > 0 {
        _ = m.backend.AddCost(ctx, m.claim, m.step, resp.CostUSD)
    }
    return resp, err
}
```

#### Park on exhaustion + grant flow

`ParkKind=BudgetExhausted`; `ParkRequest.Axis ∈ {"invocations","prompts","cost","timeout"}`.
**Timeout captures patch** via `Worktree.CapturePatch` before parking,
retaining spent work.

**Grant UI:** OSS variant uses `./<flow> grant <key> --invocations 3 --cost 10`
CLI + GitHub Issue command-comment form (`/flow grant key=plan invocations=3`)
parsed by the github backend. Grant ADDS to `Artifact.Granted*` (does not
replace). **Defaults live in flow source, full stop** — grant is the only
post-seed knob.

### Agent integration

`flow.Agent` is the SDK-root interface; concrete impls live in subpackages:

```go
// In root flow package.
type AgentRequest struct {
    Prompt          string
    ResumeSessionID string  // empty → fresh session; set → resume that session
    PermissionMode  string  // default | acceptEdits | bypassPermissions | plan
    Model           string
    Effort          string  // low | medium | high | max
    Worktree        string  // cwd for the agent process
}

type AgentResponse struct {
    LastText        string
    ToolsUsed       []string
    CostUSD         float64
    DurationSeconds float64
    SessionID       string   // for chaining via Request.ResumeSessionID
    Failure         *AgentFailure
}

type AgentFailure struct {
    Kind    string // no-result | killed | cancelled | exit-error | start-error
    Message string
}

type Agent interface {
    Name() string
    Run(ctx context.Context, req AgentRequest) (*AgentResponse, error)
}
```

`flow/claude/` ships the reference Claude CLI implementation: spawns
`claude --print --input-format stream-json --output-format stream-json`,
streams events, aggregates into `AgentResponse`. Other agent CLIs plug in
the same way.

---

## GitHub backend implementation

The github backend lives in `pkg/backend/github/` and translates the SDK's
backend contract into GitHub Issues + comments + orphan-branch operations.

### Supported signals

`Backend.SupportedSignals()` returns:

| Signal id | Set by backend when |
|---|---|
| `pr-open` | A PR with the claim branch as `head.ref` exists in `open` state |
| `pr-approved` | That PR's review state is `approved` |
| `pr-merged` | That PR has `merged: true` |
| `pr-closed` | That PR is `closed` (merged or not) |

Two resolution mechanisms, transparent to the SDK:

1. **Side-effect resolution.** `Worktree.Open(...)` succeeds → backend
   immediately sets `pr-open` in the same transaction that records the PR
   URL. `Worktree.Merge(...)` does the analog for `pr-merged`.
2. **Polling refresh.** Every `Backend.LoadState(claim)` (one per `run-step`)
   hits `GET /repos/{o}/{r}/pulls/{n}` for the PR linked to this issue (the
   backend identifies it by claim-branch name) and reconciles all four
   signals against GitHub's reported state. No webhook plumbing required;
   `run-step` IS the natural poll cadence.

### Item.Type derivation

Items (GitHub Issues) carry `Type` derived from labels. Default convention:
- Label matching `type:<x>` → `Item.Type = "<x>"` (e.g. `type:task` →
  `"task"`).
- No `type:*` label → `Item.Type = ""` (item excluded from `ListEligible`),
  or mapped to a configurable default like `"task"` via
  `github.Config{DefaultType: "task"}`.

### State storage schema

**Issue body is never modified by the SDK.** All state lives in comments.

#### The state comment (the index)

Posted once at seed; PATCHed in place by the same author on subsequent
updates; superseded by a fresh comment when ownership transfers. GitHub
enforces author-only edit, so there's no body CAS / read-modify-write retry
loop.

```
<!-- flow:state-v1 begin owner=alice -->
<details><summary>📋 Flow state — fix (machine-managed, do not edit)</summary>

```yaml
flow: fix
schema: 1
seeded_at: 2026-05-26T15:00:00Z

artifacts:
  - id: plan
    type: markdown
    required: true
    resolved_by: https://github.com/o/r/issues/42#issuecomment-12345
    produced_at: 2026-05-26T15:10:00Z
    resolved_by_principal: claude-opus-4-7
    version: 1
    stale: false
    granted_invocations: 3
    granted_prompts_per_invocation: 1
    granted_cost_usd: 10
    granted_timeout: 30m
    invocations: 1
    cost_usd_spent: 0.42

  - id: implementation
    type: patch
    required: true
    resolved_by: https://github.com/o/r/issues/42#issuecomment-12350
    # ...

signals:
  - id: pr-open
    set: true
    observed_at: 2026-05-26T15:42:00Z
    observed_via: side-effect    # or "poll"

  - id: pr-merged
    set: false
```

</details>
<!-- flow:state-v1 end -->
```

The `<details>` wrapper keeps the comment human-foldable; the SDK keys off
the HTML-comment markers and parses the fenced YAML inside. The `owner=`
attribute in the begin marker matches the GitHub login that authored the
comment.

**Authoritative-comment rule:** the most recent comment matching
`^<!-- flow:state-v1 begin` is authoritative. Older state comments are
ignored; can be marked off-topic by any maintainer.

#### Artifact comments (the storage)

First line is an HTML marker, content follows:

```
<!-- flow:artifact id=plan type=markdown v=1 by=claude-opus-4-7 step="write plan" ts=2026-05-26T15:10Z -->
## Implementation plan
...
```

Artifacts are **append-only / supersede-only**: re-running a step posts a
new comment with `v=N+1` and the state comment's `resolved_by` pointer
moves to it. The SDK never edits prior artifact comments.

#### Large-artifact escape hatch

Artifacts that would exceed the 64 KiB comment limit (large test/build
output, screenshots, diffs not associated with a PR) are committed to a
`flow-artifacts` orphan branch at `flow/artifacts/issue-N/<id>/<filename>`,
with a marker comment linking to `raw.githubusercontent.com/<owner>/<repo>/flow-artifacts/...`.
The flow author opts in via `ctx.ResolveFile(name, bytes)` or `ctx.ResolvePatch(body)`;
`ctx.ResolveMarkdown` auto-spills to file storage with a
`[truncated, full output at <url>]` marker if the body exceeds ~60 KiB.

#### Labels (closed vocabulary)

| Label | Purpose |
|---|---|
| `flow:seeded` | State comment has been posted |
| `flow:<binary-name>` | This binary owns the issue |
| `flow:owner:<gh-login>` | Current claim holder |
| `flow:claim:<128b-hex>` | Transient race-lock during claim |
| `flow:blocked` | Flow detected a blocker |
| `flow:needs-answer` | Open `flow:question` comment |
| `flow:disabled` | Skip this issue (equivalent of `FlowNone`) |
| `flow:stale:<id>` | Artifact marked stale |

The `flow:seeded` label is the cheap "is this issue flow-managed?" check —
answerable via the labels API alone, keeps `list` fast across hundreds of
issues.

### SDK orchestration commands

#### `claim N` (alias `lease N`)

`claim` is where the SDK does its heavy reads. After it completes, every
subsequent `run-step` has a fully populated `.flow/` directory and can dispatch
handlers without per-artifact API calls.

1. **Resolve token** — `gh auth token`, fall back to `GITHUB_TOKEN`.
2. **Resolve repo** — `git remote get-url origin`, parsed.
3. **Two-phase race-lock**:
   - POST label `flow:claim:<rand-128b-hex>`.
   - GET labels; if multiple `flow:claim:*` present, lexicographically
     smallest hex wins. Losers DELETE their own claim label and abort.
   - Winner: POST self as assignee, POST `flow:owner:<login>`, DELETE
     `flow:claim:*`.
4. **Fetch issue + all comments** — `GET /repos/{o}/{r}/issues/{n}` plus
   paginated `GET /comments`. ~1–5 requests for typical issues.
5. **Locate state comment** — scan newest-first for `<!-- flow:state-v1 begin`.
6. **Preflight** — refuse if label `flow:<other-binary>` set, `flow:disabled`
   present, or state-comment `flow:` differs from this binary's name.
7. **Seed if absent** — render initial state YAML from
   `App.Artifacts`/`App.Signals` + the flows' StepOption-derived budgets,
   POST as a new comment, capture comment ID. POST `flow:seeded` +
   `flow:<binary-name>` labels.
8. **Supersede if owner changed** — POST a new state comment authored by
   the current user, copy state forward, mark the old one off-topic.
9. **Prefetch resolved artifacts** — for each artifact with non-null
   `resolved_by`:
   - Markdown → fetch comment, strip marker, write `.flow/artifacts/<id>.md`.
   - File → `git fetch flow-artifacts && git show ...`, write
     `.flow/artifacts/<id>/<file>`.
   - Patch → same as File but parsed into PatchBody on read.
   - CommitHash / Flag / JSON → small enough to inline in the state YAML;
     no separate prefetch needed.
10. **Write `.flow/active.json`** — serialized `Claim` (with backend Token
    holding `{state_comment_id, claim_id}`). Write `.flow/state.yml` (parsed
    ItemState snapshot for inspection).

After claim: handlers can read any artifact / signal via `ctx.Markdown(id)`
/ `ctx.Signal(id)` / etc. with zero API cost.

#### `run-step`

Pure function of (cached state + fresh state-comment fetch).

1. **Load `.flow/active.json`** — or accept `--issue N` for an unclaimed
   run, which performs an implicit minimal claim first.
2. **Refresh state comment** — `GET /repos/{o}/{r}/issues/comments/{state_comment_id}`.
3. **Poll signals** — `GET /pulls/{n}` for the claim-branch PR (if any);
   reconcile pr-open / pr-approved / pr-merged / pr-closed against
   GitHub state; update state comment YAML if anything changed.
4. **Reconcile artifact cache** — diff cached state vs fresh state; refetch
   any artifacts whose `resolved_by` changed (rare in steady state).
5. **Select flow** — pick first `App.Flows` whose `Types()` match
   `Item.Type` AND `RequireSignal` satisfied AND has pending lifecycle items.
6. **Derive next** — first registered lifecycle item whose result is unresolved.
7. **Budget gate** — check `Invocations` / `CostUSDSpent` against `Granted*`;
   park on exhaustion.
8. **Wrap step ctx** with `context.WithTimeout(GrantedTimeout)`; bump
   `Invocations` BEFORE dispatch.
9. **Invoke handler** (for `AddStep` / `AddSignalStep` items). For
   `AwaitSignal`, no handler — just check if signal is set and complete the
   item if so, else skip.
10. **Resolve = write** (SDK internals):
    - For `AddStep`: POST artifact comment with marker (or commit to orphan
      branch for File/Patch); write `.flow/artifacts/<id>.*`; PATCH the
      state comment YAML to update `resolved_by`, `produced_at`, `version`.
    - For `AddSignalStep`: backend writes the signal as side effect of the
      worktree action; step completes when SDK observes the signal set.
11. **Maybe close** — if all flows are `IsDone()` → PATCH issue
    `state: closed, state_reason: completed`, remove `flow:owner:*`.
12. **Emit `InvocationResult` JSON** to stdout. Exit 0/non-zero.

#### Other commands

- **`status [<id>]`** — load (claimed) or `LookupClaim(ref)` + fresh state
  comment; print artifact + signal checklist with resolution URLs. Read-only.
- **`list`** — `GET /search/issues` with `label:flow:<name> state:open
  assignee:@me` (and bare `label:flow:<name>` for the unclaimed view).
  Cheap; never fetches comments. `LookupClaim` to show current holders.
- **`release`** — DELETE `flow:owner:<me>`, unassign user, delete `.flow/`.
  Does NOT delete the state comment.
- **`grant <id> [--invocations N] [--cost $N] [--prompts N] [--timeout T]`**
  — increments `Granted*` for the artifact on the state comment, clears any
  `flow:budget-exhausted:<id>` label.
- **`doctor`** — verify `gh` installed, authed, repo accessible with
  `permissions.push == true`, write-test by toggling a temporary label.

### Why no body CAS

In a body-fenced design, every artifact resolution would require
read-modify-write of the issue body, with retries on conflict (multiple
users editing simultaneously). In the comment-fenced design, the state
comment is **authored by one user at a time**; GitHub enforces author-only
PATCH; combined with the claim-lock there is exactly one writer at a time.
PATCH succeeds or fails for a deterministic reason (auth, deletion) — never
for "you raced someone." The retry/CAS module is gone.

---

## End-to-end lifecycle — OSS contributor + maintainer flows

Two flows in one binary, both targeting `task`/`bug` items. The contributor
flow plans, implements, reviews, runs coverage, verifies, and opens the PR.
The maintainer flow gates on `pr-open` and adds review, re-verify, and
merge.

```go
package main

import (
    "os"
    "time"

    "github.com/promise-language/flow"
    "github.com/promise-language/flow/claude"
    "github.com/promise-language/flow/cli"
    "github.com/promise-language/flow/pkg/backend/github"
)

func main() {
    artifacts := []flow.ArtifactDef{
        flow.Artifact("plan",            flow.ArtifactMarkdown),
        flow.Artifact("implementation",  flow.ArtifactPatch),       // diff bytes + base metadata
        flow.Artifact("review",          flow.ArtifactMarkdown),
        flow.Artifact("coverage",        flow.ArtifactMarkdown),
        flow.Artifact("verify-impl",     flow.ArtifactMarkdown),
        flow.Artifact("verify-merge",    flow.ArtifactMarkdown),
        flow.Artifact("review-maint",    flow.ArtifactMarkdown),
        flow.Artifact("merge-commit",    flow.ArtifactCommitHash),
    }

    signals := []flow.SignalDef{
        flow.Signal("pr-open",   "set when a PR exists for the claim branch and is open"),
        flow.Signal("pr-merged", "set when that PR has been merged"),
    }

    // Contributor flow: plans, implements, opens the PR.
    fix := flow.NewFlow("fix", []flow.ItemType{"task", "bug"})
    fix.AddStep      ("write plan",           "plan",           stepPlan,           flow.Required)
    fix.AddStep      ("implement the change", "implementation", stepImplementation,
                      flow.Required,
                      flow.MaxPromptsPerInvocation(5),
                      flow.Timeout(60*time.Minute))
    fix.AddStep      ("review the work",      "review",         stepReview,         flow.Required)
    fix.AddStep      ("analyze coverage",     "coverage",       stepCoverage,       flow.Required)
    fix.AddStep      ("verify",               "verify-impl",    stepVerify,         flow.Required)
    fix.AddSignalStep("create pull request",  "pr-open",        stepCreatePR,       flow.Required)

    // Maintainer flow: reviews + re-verifies + merges. Eligible only once PR is open.
    merge := flow.NewFlow("merge", []flow.ItemType{"task", "bug"})
    merge.RequireSignal("pr-open")
    merge.AddStep      ("review the implementation", "review-maint", stepMaintReview, flow.Required)
    merge.AddStep      ("verify",                    "verify-merge", stepVerify,      flow.Required)
    merge.AddSignalStep("merge pull request",        "pr-merged",    stepMerge,       flow.Required)
    merge.AddStep      ("record merge commit",       "merge-commit", stepRecord,      flow.Required)

    os.Exit(cli.Run(cli.App{
        Backend:   github.NewBackend(github.Config{
            VerifyCmd: []string{"bash", "bin/verify.sh"},
        }),
        Agent:     claude.New(),
        Artifacts: artifacts,
        Signals:   signals,
        Flows:     []*flow.Flow{fix, merge},
    }))
}
```

### Handler snippets

```go
func stepVerify(ctx flow.StepCtx) error {
    wt, err := ctx.Worktree()
    if err != nil { return err }
    if err := wt.Validate(ctx.Context()); err != nil {
        return err   // verify failed; step retries up to MaxInvocations
    }
    return ctx.ResolveMarkdown("verify passed at " + time.Now().Format(time.RFC3339))
}

func stepCreatePR(ctx flow.StepCtx) error {
    wt, err := ctx.Worktree()
    if err != nil { return err }
    _, err = wt.Open(ctx.Context(),
        "main",
        "feat: "+ctx.Item().Title,
        "Closes #"+ctx.Item().IDStr())
    return err   // no Resolve* call — pr-open is signal-sourced, backend writes
}
```

### Selection logic walkthrough

`cli.App.Flows` registered in order: `[fix, merge]`. Each `run-step`:

1. Item is `task` — both flows' types match.
2. `fix` has no `RequireSignal` preconditions; check pending:
   - If any of {`plan`, `implementation`, `review`, `coverage`,
     `verify-impl`, `pr-open`} unresolved → `fix` active, dispatch
     next pending.
3. If `fix` done (all six resolved), try `merge`:
   - `RequireSignal("pr-open")` satisfied.
   - If any of {`review-maint`, `verify-merge`, `pr-merged`,
     `merge-commit`} unresolved → `merge` active.
4. Both flows done → close issue (`state_reason: completed`).

### Contributor mode (PR from a fork)

This design covers maintainer mode (v1: user has push to upstream). Contributor
mode (PR from a fork) changes the worktree mechanics but not the flow model
— same `fix` flow, same signals, just `wt.Open(...)` opens a
cross-repo PR via `gh pr create --head forkowner:branch`. Deferred for v1;
nothing in the SDK changes to support it later.

### "Pure-observer" flow variant (optional)

If a binary wants the maintainer to merge by hand and the bot to simply
detect it:

```go
observe := flow.NewFlow("observe", []flow.ItemType{"task", "bug"})
observe.RequireSignal("pr-open")
observe.AwaitSignal("waiting for human merge", "pr-merged")
```

No handler steps; flow completes when an external human merges the PR. The
backend's `pr-merged` polling does the work.

---

## Repo layout

```
github.com/promise-language/flow/
├── go.mod                              module github.com/promise-language/flow
├── doc.go
├── flow.go                             Flow, NewFlow, AddStep/AddSignalStep/AwaitSignal/RequireSignal, Steps/Pending/DeriveNext/IsReady/IsDone
├── step.go                             StepHandler, StepOption (Required/Optional, StaleAfter/StaleOnCommit, MaxInvocations, MaxPromptsPerInvocation, MaxCostUSD, Timeout)
├── stepctx.go                          StepCtx interface; lazily-cached Worktree via ctx.Worktree()
├── artifact.go                         ArtifactId, ArtifactType (6 values), ArtifactDef, ArtifactSpec, Artifact, FileBody, PatchBody, Artifact() constructor
├── signal.go                           SignalId, SignalDef, SignalState, Signal() constructor
├── backend.go                          Backend interface, ItemState, Claim, ClaimInfo, ItemRef, Item, ArtifactBody, Grant, Worktree
├── budget.go                           StepBudget, defaultStepBudget {3, 1, $10, 30m}, resolveBudget, budget-axis park builders, ErrBudgetExhausted
├── agent.go                            Agent interface, AgentRequest, AgentResponse, AgentFailure  (root for parity with Backend)
├── errs.go                             ErrSkip, ErrPark, ErrQuestion, ErrTypeMismatch, ErrSignalNotWritable, ErrStepDidNotResolve
├── wire.go                             InvocationResult, ParkRequest/ParkKind, enums
├── *_test.go                           table-driven tests
├── cli/
│   ├── app.go                          App struct, Run(App) int entry point, startup validation
│   ├── cmd_run.go                      ★ `run-step` orchestrator: one lifecycle item per invocation
│   ├── cmd_claim.go                    ★ claim entry point (alias: lease); primes the local cache
│   ├── cmd_release.go
│   ├── cmd_status.go                   read-only checklist via LookupClaim + LoadState
│   ├── cmd_grant.go                    extend Granted* on a parked item
│   ├── cmd_doctor.go                   verify backend prereqs
│   └── cmd_list.go
├── claude/
│   └── claude.go                       ★ claude CLI driver implementing flow.Agent
├── pkg/backend/github/
│   ├── backend.go                      ★ implements flow.Backend
│   ├── state_comment.go                ★ parse/render <details>+fenced YAML; PATCH-in-place or supersede
│   ├── claim.go                        ★ two-phase claim race-lock
│   ├── seed.go                         idempotent first-run seed
│   ├── artifact.go                     ResolveArtifact, MarkStale, large-artifact spillover
│   ├── signal.go                       PR-state polling + side-effect writes
│   ├── worktree.go                     ★ Worktree impl: Branch/Commit/Push/Open/Merge/Validate via gh + local git
│   ├── artifacts_branch.go             commit/read files on flow-artifacts orphan branch
│   ├── label.go                        label vocabulary helpers
│   ├── type_derivation.go              Item.Type from `type:*` label convention
│   └── *_test.go
├── pkg/backend/fake/                   in-memory backend for SDK tests
├── pkg/auth/
│   └── gh.go                           shell out to `gh auth token`; GITHUB_TOKEN fallback
├── pkg/git/
│   └── git.go                          local git ops via os.exec
├── examples/
│   ├── fix/main.go                     full contributor+maintainer example (binary name `fix`)
│   └── verify/main.go                  minimal "run go test" one-step flow
└── docs/
    ├── design.md                       (this file)
    └── state-block-spec.md             state-v1 YAML reference
```

**Star (★) marks the highest-leverage files to land first.**

---

## Critical files

1. **`flow.go`** — public Flow API. `NewFlow` + `AddStep`/`AddSignalStep`/
   `AwaitSignal`/`RequireSignal`. The handler-facing surface.
2. **`stepctx.go`** — `StepCtx` interface with typed accessors, Resolve
   helpers, `Agent()` metered wrapper, `Worktree()` lazy accessor.
3. **`backend.go`** — `Backend` interface. The pluggable boundary. The
   tracker repo implements this against its server; this repo implements it
   in `pkg/backend/github`.
4. **`budget.go`** — step budget enforcement (the universal runaway-protection
   mechanism). Implemented once in the SDK; both backends inherit.
5. **`cli/cmd_run.go`** — the per-step orchestrator. Flow selection, budget
   gating, ctx-with-timeout wrapping, handler dispatch, sentinel
   translation.
6. **`cli/cmd_claim.go`** — explicit claim entrypoint (alias: `lease`);
   wraps the backend-specific claim flow, seeds state, prefetches resolved
   artifacts.
7. **`pkg/backend/github/state_comment.go`** — parse/render the
   `<details>`-wrapped YAML; PATCH-in-place or supersede. No body CAS.
8. **`pkg/backend/github/claim.go`** — two-phase claim race-lock.
9. **`pkg/backend/github/worktree.go`** — git + gh operations, including
   `Validate` (project verify command exec).
10. **`claude/claude.go`** — spawn `claude --print --input-format stream-json
    --output-format stream-json`, scan JSONL events, aggregate into
    `AgentResponse`.

---

## Migration / build order

1. **SDK foundation (this repo).** Land `flow.go` / `step.go` / `stepctx.go`
   / `artifact.go` / `signal.go` / `backend.go` / `budget.go` / `agent.go`
   / `errs.go` / `wire.go`. Add `pkg/backend/fake/` for SDK tests.
   Verify: `go test ./...`.
2. **Cli + claude.** Land `cli/*` and `claude/claude.go`. Verify against
   the fake backend.
3. **GitHub backend.** Land `pkg/backend/github/*`. Verify with integration
   tests gated by `GH_INTEGRATION=1` against a sandbox repo.
4. **Examples.** `examples/fix/` and `examples/verify/` exercise the
   real github backend end-to-end.
5. **Tracker repo integration** (separate plan, in the tracker repo): the
   tracker imports this SDK, adds `pkg/backend/tracker/`, rewrites
   `flows/do` to register against the new SDK. See the tracker's
   `docs/flow-sdk-harness.md` for the tracker-specific migration order.

---

## Verification

End-to-end against a real repo:
1. `gh auth login`.
2. `cd examples/fix && go build -o fix .`
3. Create a private test repo; push it.
4. Open issue with labels `type:task needs-flow`.
5. `./fix doctor` → green.
6. `./fix claim 1` → state comment posted; labels `flow:seeded`,
   `flow:fix`, `flow:owner:<me>` set; assignee set; `.flow/active.json`
   present.
7. `./fix run-step` ×5 → plan, implementation, review, coverage,
   verify-impl artifacts posted as comments.
8. `./fix run-step` → PR opens via `gh pr create`; backend writes
   `pr-open` signal; `fix` flow complete.
9. `./fix run-step` → maintainer flow starts: review-maint comment posted.
10. `./fix run-step` → verify-merge re-runs against merge target.
11. `./fix run-step` → PR merged via `gh pr merge`; backend writes
    `pr-merged` signal; merge-commit recorded.
12. Issue auto-closes with `state_reason: completed`.

Budget enforcement (regression for the runaway-loop class):
- Stub step with `MaxInvocations(2)`: 3rd run parks with
  `ParkKind=BudgetExhausted, Axis=invocations`; handler NOT entered.
- `MaxPromptsPerInvocation=1`: 2nd `ctx.Agent().Run` in one invocation
  returns `ErrBudgetExhausted{Axis="prompts"}`.
- Mock `AgentResponse.CostUSD=$4`, `MaxCostUSD($10)`: 3rd successful turn
  (total $12) refused with `Axis=cost`.
- `Timeout(1*time.Second)` on a 2s-sleeping handler: deadline fires;
  `Worktree.CapturePatch` writes a patch artifact; park with `Axis=timeout`.
- `grant` flow clears the park and unblocks the next dispatch.

Unit tests (table-driven, in `*_test.go`):
- `wire_test.go` — round-trip JSON for `InvocationResult`, `Claim`, `ItemRef`,
  `AgentRequest`/`AgentResponse`.
- `flow_test.go` — `DeriveNext` / `Pending` / `IsReady` / `IsDone` against
  fixture `ItemState`s.
- `budget_test.go` — full enforcement matrix on all four axes.
- `pkg/backend/github/state_comment_test.go` — parse/render/CAS round-trips.
- `pkg/backend/github/claim_test.go` — two-runner race simulation against a
  mock GH server.
- `claude/claude_test.go` — golden JSONL stdin/stdout fixtures; aggregation;
  SIGINT-on-cancel.

---

## Risks / open questions

1. **`gh` CLI as a hard dep.** `doctor` must give an actionable error
   pointing at `https://cli.github.com/`.
2. **State-comment deletion / hiding.** A maintainer can delete the SDK's
   state comment via the web UI. Mitigation: `run` detects 404 on the
   cached `state_comment_id`, scans comments for a state-v1 marker, treats
   missing as unseeded → re-seeds (idempotent). Document.
3. **Cache vs upstream drift.** `.flow/artifacts/` is a cache. Every `run`
   refreshes the state comment and reconciles cached `resolved_by` URLs;
   changed entries are re-fetched before the handler sees them.
4. **Supersede chain pollution.** Many owner handoffs grow the issue thread.
   Mitigation: on supersede, mark prior state comment off-topic (hidden by
   default in GH UI). Optional `flow:gc` command for DELETE — out of v1.
5. **`claude` CLI stream-json drift.** Event-type names (`assistant`,
   `tool_use`, `result`) are not stable SDK. Recommend a smoke test against
   real `claude --version` in CI.
6. **Artifact-list migrations.** If a binary adds a new artifact after
   issues are seeded, existing issues won't require it (their seeded
   checklist is frozen — same one-shot rule as budget caps). A future
   `./implement migrate` command could PATCH the state comment with new
   artifact rows; out of v1.
7. **One-claim-per-arena enforcement is client-side.** The `.flow/active.json`
   file is the source of truth for "this worktree has a claim." Backend
   doesn't refuse multiple claims by the same owner across worktrees —
   that's intentional and supports multi-worktree workflows. The single
   binary in a single worktree path is enforced by the cli command itself.
8. **Maintainer-mode-only in v1.** Read-only contributors can't claim
   issues (no assignee/label write). `doctor` errors loudly if
   `permissions.push == false`.
9. **Large-artifact spillover correctness.** `ResolveMarkdown` auto-spills
   bodies >~60 KiB to the `flow-artifacts` orphan branch. Edge cases:
   comment-edit history is lost when content is moved to the branch on a
   re-run; `flow-artifacts` branch auto-created on first spillover. Same
   `permissions.push` as the rest of v1.
10. **Reactor server, deliberately deferred.** A central server would buy
    webhook reactivity, cross-repo orchestration, shared agent rate limits
    — none of which v1 needs. The architecture is designed so a reactor
    can later back the `Backend` interface with object storage + a queue
    without changing the handler API. Decision: build comment-backed v1;
    revisit reactor only when a concrete use case demands it.
11. **Gates (structured periodic checks).** Separate from the per-issue
    flow lifecycle: gates emit a stable stdout-JSON envelope (metrics +
    tests + completion-set) that a tracker-style orchestrator consumes for
    ratcheting baselines and health dashboards. v1 of flow ships without
    them; design is in
    [docs/proposals/gates.md](proposals/gates.md). The proposed shape adds
    a `flow/gate` subpackage + optional `cli.App.Gates` + a `gate manifest
    | list | run` cli surface; tracker compatibility is free because the
    wire format mirrors tracker's existing one.
