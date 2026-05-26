# Flow SDK — Open-Source GitHub-Issues Port

## Context

The repo `~/prog/flow-sdk/flow-sdk/` is a blank Apache-2.0 Go scaffold (LICENSE + .gitignore only; remote `github.com/promise-language/flow-sdk`). It will host an open-source variant of the proprietary `~/prog/tracker_flow_sdk/` (which targets a central `~/prog/tracker` server). The new SDK keeps the reference's stateless-per-step flow model but replaces the central server with **GitHub Issues + the project's own git repo** as the sole backend — no server to host.

**Why this exists.** People want flow-style automated work on regular GitHub projects with no infra. The reference SDK can't be open-sourced because it requires running a tracker server. Users want to declare their own artifacts (not the proprietary plan/review/coverage/commit/push set hardcoded at reference `wire.go:446-457`), get a generic step orchestrator that can drive a coding agent (Claude CLI today, others later) **or** a dumb command (e.g., `go test`), and have all durable state live on the GitHub issue.

**Outcome.** A library plus an embeddable CLI. The user writes a flow as a **Go binary** that imports `flow-sdk` and registers its artifacts and step handlers in code. The binary's CLI (provided by `flow-sdk/cli`) is what end-users run: `./implement run`, `./implement claim 42`, `./implement doctor`. The binary picks an open issue, claims it, seeds the issue body with the registered artifact checklist, advances one step (calling the configured agent or running the configured command), captures output as an artifact comment, and closes the issue once all required artifacts are resolved and the linked PR is merged.

---

## Locked decisions (from user)

| Question | Decision |
|---|---|
| Language | **Go** |
| Where flows, artifacts, prompts are defined | **In the flow binary, in Go.** No YAML/config for these. The binary is the source of truth. |
| Step types | **Agent steps OR command steps (or arbitrary handler).** Some steps just run `go test`; the SDK doesn't assume every step calls an LLM. |
| Agent integration | **First-class in SDK.** SDK ships an `Agent` interface + `pkg/agent/claude` that **spawns the `claude` CLI** with `--input-format stream-json --output-format stream-json` (no API). Other agent CLIs pluggable later. |
| Step registration mechanism | **`flow.OnStep(key, handler)` in Go** — the only way. No YAML alternative. |
| Artifact storage on the issue | **All in comments.** A single "state comment" carries the `<!-- flow:state-v1 ... -->` YAML index (machine-managed, wrapped in `<details>` so humans can fold it). Each artifact is its own comment with an HTML marker. **The issue body is never modified** after the original author posts it. |
| Artifact files (binaries, screenshots, large logs) | **Orphan branch in the same repo.** Committed to `flow/artifacts/issue-N/<key>/<filename>` on a `flow-artifacts` orphan branch; comments link via `raw.githubusercontent.com`. No second repo, no external blob store, no server. GitHub's web-UI attachment endpoint (`uploads.github.com` → user-attachments) is undocumented and not used. |
| GitHub auth | **Shell out to `gh` CLI** (`gh auth token` for the PAT; `gh auth login` to authenticate). Hard runtime dep on `gh`. |
| Per-project config file | **Optional/minimal.** Defaults from the binary itself. A small `.github/flow.yml` may be read if present for machine-level overrides (e.g., repo override, dry-run); v1 may skip it entirely. |
| `.flow/` worktree state | **Pointer + prefetched artifact cache.** Written by `claim`, kept fresh by `run`, deleted by `release`. Contains `active.json` (`{flow, repo, issue, state_comment_id, claimed_at, claim_id}`), `state.yml` (last-seen parsed state block), and `artifacts/<key>.{md,json}` (prefetched content of every resolved artifact). Cache makes `ctx.Artifact(key)` reads zero-API and gives the flow author a predictable on-disk view for debugging. Reference SDK's heavier `.flow/context.json` shape (tokens, URLs, runner config) is dropped — those concepts don't exist here. |
| Default flow / multi-flow per project | **One binary = one flow** with a hardcoded name. Multiple flows per project means multiple binaries. No `default_flow` config. |
| Runner concept | **Eliminated.** The flow binary is self-contained; one step per `./<binary> run` invocation. |

---

## User-facing usage

End user installs `gh`, authenticates once (`gh auth login`), then runs the project-supplied flow binary:

```
$ ./implement doctor                 # verifies gh + auth + repo access
$ ./implement list                   # lists eligible open issues
$ ./implement claim 42               # assigns #42 to me + labels it for this flow
$ ./implement run                    # advances one step on the claimed issue
$ ./implement run                    # advances next step
$ ./implement status                 # read-only artifact checklist
$ ./implement release                # unassign / drop the claim
```

`run` without an explicit `--issue` reads `.flow/active.json` (written by `claim`); falls back to "the open issue assigned to me with label `flow:<name>`". If multiple match, it errors and asks for `--issue N`.

---

## Flow author API (what users of the SDK write)

### Step ↔ Artifact: 1:1, enforced by the registration API

A flow is an **ordered list of steps**. Each step produces exactly one artifact, identified by the step's key. There is no separate artifact registration — `f.AddStep(...)` registers both at once. This is mechanical (not just convention):

- You cannot declare an artifact without a step (the SDK would have nothing to dispatch).
- You cannot declare a step without an artifact (the SDK would have no persistent "did this run?" record).
- The step key is the artifact key — same string, one source of truth, no key-mismatch bugs possible.

The "did this run?" question is answered by the artifact's `resolved_by` field in the state comment. Steps that conceptually "just run a side effect" (deploy, verify, lint) still produce an artifact — a comment recording what was done, when, against what commit, by whom. That comment is the audit trail; without it the SDK has no memory the step happened and would re-dispatch forever.

### A flow binary in full

```go
package main

import (
    "fmt"
    "os/exec"

    flowsdk "github.com/promise-language/flow-sdk"
    "github.com/promise-language/flow-sdk/agent/claude"
    "github.com/promise-language/flow-sdk/cli"
)

func main() {
    f := flowsdk.NewFlow("implement")

    // Which open issues are eligible for this flow.
    f.Selection.LabelsAny = []string{"needs-flow"}

    // Steps. Order matters: first unresolved is what `run` dispatches.
    // f.AddStep(key, handler, opts...) registers the step AND its artifact.
    // Keys are opaque strings — the SDK never hardcodes any.

    f.AddStep("plan",
        func(ctx flowsdk.StepCtx) error {
            resp, err := claude.New().Run(ctx, claude.Request{
                Prompt: fmt.Sprintf("Issue: %s\n\n%s\n\nProduce an implementation plan.",
                                    ctx.Issue.Title, ctx.Issue.Body),
                Model:          "claude-opus-4-7",
                PermissionMode: "plan",
                Effort:         "high",
            })
            if err != nil { return err }
            return ctx.ResolveComment(resp.LastText)
        },
        flowsdk.Required, flowsdk.AcceptsComment)

    f.AddStep("implementation",
        func(ctx flowsdk.StepCtx) error {
            // Read the plan artifact — zero API calls; served from the .flow/ cache
            // populated at claim time. Always returns the latest version.
            plan, ok := ctx.Artifact("plan")
            if !ok {
                return fmt.Errorf("plan artifact missing — flow order violated")
            }

            resp, err := claude.New().Run(ctx, claude.Request{
                Prompt:         fmt.Sprintf("Implement this plan:\n\n%s", plan.Content),
                Model:          "claude-opus-4-7",
                PermissionMode: "acceptEdits",
            })
            if err != nil { return err }
            _ = resp

            // Branch / commit / push / open PR helpers wrap local git + gh.
            if err := ctx.CreateBranch(); err != nil { return err }
            if err := ctx.Commit("implement #" + ctx.Issue.NumStr()); err != nil { return err }
            if err := ctx.Push(); err != nil { return err }
            prURL, err := ctx.OpenPR("main", "feat: "+ctx.Issue.Title, "Closes #"+ctx.Issue.NumStr())
            if err != nil { return err }
            return ctx.ResolvePR(prURL)
        },
        flowsdk.Required, flowsdk.AcceptsPR, flowsdk.PRMustBeOpen)

    f.AddStep("review",
        func(ctx flowsdk.StepCtx) error {
            impl, _ := ctx.Artifact("implementation")  // PR-type, has .URL and .PRStatus
            resp, err := claude.New().Run(ctx, claude.Request{
                Prompt: "Review PR " + impl.URL,
                Model:  "claude-sonnet-4-6",
            })
            if err != nil { return err }
            return ctx.ResolveComment(resp.LastText)
        },
        flowsdk.Required, flowsdk.AcceptsComment)

    // Dumb command step — no agent. Output may be huge; ResolveComment auto-spills
    // to the flow-artifacts branch if it exceeds the comment limit. StaleAfter
    // means: re-run whenever the `implementation` artifact has been updated since
    // this verify ran.
    f.AddStep("verify",
        func(ctx flowsdk.StepCtx) error {
            out, err := exec.Command("go", "test", "./...").CombinedOutput()
            body := "```\n" + string(out) + "\n```"
            if err != nil {
                return ctx.ResolveCommentWithStatus(body, flowsdk.StatusFailed)
            }
            return ctx.ResolveComment(body)
        },
        flowsdk.Required, flowsdk.AcceptsComment,
        flowsdk.StaleAfter("implementation"))

    // No handler for "pr" — its resolution comes from PRMustBeMerged.
    // Pass nil where the handler would go; SDK auto-resolves from PR state.
    f.AddStep("pr", nil,
        flowsdk.Required, flowsdk.AcceptsPR, flowsdk.PRMustBeMerged)

    cli.Run(f)  // exposes: doctor, list, claim, release, status, run [--issue N]
}
```

### Registration signature

```go
type StepHandler func(ctx StepCtx) error

// AddStep registers a step that produces an artifact named `key`. The key serves
// double duty as artifact key and step identifier; the 1:1 relationship is
// enforced by this signature (no separate Artifact() call exists).
//
// Pass nil for `do` when the artifact is resolved by external events
// (e.g., PR merge via PRMustBeMerged). The SDK auto-resolves such steps from
// observed external state; calling AddStep with nil handler explicitly opts in.
//
// Calling AddStep twice with the same key panics at registration time.
func (f *Flow) AddStep(key string, do StepHandler, opts ...StepOption)
```

`StepOption` is the typed option set:
- **Cardinality:** `Required`, `Optional`
- **Artifact type:** `AcceptsComment`, `AcceptsPR`, `AcceptsFile`
- **PR gates:** `PRMustBeOpen`, `PRMustBeMerged`, `PRMustBeApproved`
- **Staleness:** `StaleAfter(otherKey string)` — mark stale if `otherKey`'s artifact was produced more recently than this one. `StaleOnCommit` — mark stale if HEAD has moved since `produced_at`. (v1 ships these as declarative options; the engine compares `produced_at` timestamps in `derive_next`.)

### Staleness and re-runs

By default an artifact is resolved-once-and-done. To re-run a step:

1. **Declarative (preferred).** Attach `StaleAfter("dependency-key")` or `StaleOnCommit` at registration. `derive_next` automatically picks the step up again when the rule fires.
2. **Imperative.** Apply the `flow:stale:<key>` label on the issue (via the web UI, `gh label`, or a future `./<binary> mark-stale <key>`). Next `run` clears the label and re-dispatches the step.
3. **Escape hatch.** Inside another handler, call `ctx.MarkStale("verify")` to invalidate a sibling artifact. Useful when a step's outcome implies another's freshness changed.

A re-dispatched step posts a *new* artifact comment with `v=N+1`; the prior comment stays as history; the state comment's `resolved_by` pointer moves to the new comment. Artifact comments are append-only (never edited), consistent with the permission model.

### Artifact accessor — the clean read surface

`ctx.Artifact(key)` is the **only** way handler code reads prior artifacts. It returns a typed value populated from the local `.flow/artifacts/` cache (refreshed at claim and at the start of every `run`). Handler code never talks to comments, never parses markers, never knows that comments are the underlying storage.

```go
type Artifact struct {
    Key        string
    Type       ArtifactType  // Comment | PR | File
    Resolved   bool          // false if no resolution yet (zero value otherwise)
    Version    int           // 1-indexed; increments on re-resolution after staleness
    Content    string        // markdown body — Comment type only
    URL        string        // comment URL, PR URL, or raw.githubusercontent.com URL
    Filename   string        // File type only
    Bytes      []byte        // File type only; lazy-loaded from .flow/artifacts/<key>/
    ProducedAt time.Time
    By         string        // agent name or gh login that produced it
    PRStatus   string        // "open"|"closed"|"merged" — PR type only
    Stale      bool          // marked stale; handler should re-derive
}

// On StepCtx:
func (c *StepCtx) Artifact(key string) (Artifact, bool)
```

The `bool` is the existence/resolution signal. Idiomatic Go usage:

```go
plan, ok := ctx.Artifact("plan")
if !ok || plan.Stale {
    return flowsdk.Skip("plan not ready")
}
useTheContent(plan.Content)
```

### Resolve helpers — the clean write surface

Each handler resolves **its own step's** artifact (the key the SDK dispatched to it). No need to pass the key:

```go
ctx.ResolveComment(body string) error                   // for AcceptsComment
ctx.ResolveCommentWithStatus(body string, s Status) error
ctx.ResolvePR(url string) error                         // for AcceptsPR
ctx.ResolveFile(filename string, content []byte) error  // for AcceptsFile (orphan-branch commit)
ctx.Skip(reason string) error                           // sentinel: no progress possible
ctx.MarkStale(otherKey string) error                    // invalidate a sibling artifact
```

Under the hood `ResolveComment` (a) POSTs the marker comment, (b) PATCHes the SDK-owned state comment to update the `resolved_by` pointer and bump `version`, and (c) writes the content to `.flow/artifacts/<key>.md` so the next handler's `ctx.Artifact(key)` is served from disk. The flow author sees none of this.

Key shape choices:
- `flowsdk.NewFlow(name)` — the binary's name is the flow's name; hardcoded by the author.
- `Selection` is exposed as a struct field for direct mutation; no setter ceremony.
- `AddStep(key, do, opts...)` is the single registration point. The 1:1 step-artifact relationship is enforced by the signature; there is no public `Artifact()` or `OnStep()` to call out of sync.
- `do == nil` is the explicit "auto-resolved by external event" opt-in; the SDK still dispatches the step in turn but resolves it from observed state (PR merged, etc.) without calling a handler.
- `StepCtx` carries: the parsed `Issue`, the typed `Artifact(key)` accessor, `Context()` for cancellation, the GitHub client, and the resolve/git helpers.
- **The comment-as-storage mechanic is an SDK implementation detail.** Handlers see typed artifact values in and out. A future migration (e.g., to a "reactor" server with object storage) would not change the handler API.

---

## State storage schema (on GitHub)

**Issue body is never modified by the SDK.** The original user description stays as-is for the lifetime of the issue. All flow state lives in comments.

### The state comment (the index)

Posted once at seed; PATCHed in place by the same author on subsequent updates; superseded by a fresh comment when ownership transfers to a different user. Authorship-bound editing is enforced by GitHub itself — no body CAS, no read-modify-write retry loop.

```
<!-- flow:state-v1 begin owner=alice -->
<details><summary>📋 Flow state — implement (machine-managed, do not edit)</summary>

```yaml
flow: implement
schema: 1
seeded_at: 2026-05-26T15:00:00Z
artifacts:
  - key: plan
    required: true
    accepts: [comment]
    resolved_by: https://github.com/o/r/issues/42#issuecomment-12345
    produced_at: 2026-05-26T15:10:00Z
    by: claude-opus-4-7
    stale: false
  - key: implementation
    required: true
    accepts: [pr]
    pr_must_be: open
    resolved_by: https://github.com/o/r/pull/43
  - key: review
    required: true
    accepts: [comment]
    resolved_by: null
  - key: pr
    required: true
    accepts: [pr]
    pr_must_be: merged
    resolved_by: null
```

</details>
<!-- flow:state-v1 end -->
```

The `<details>` wrapper means humans browsing the issue see a folded "📋 Flow state" disclosure they can expand if curious; the SDK keys off the HTML-comment markers and parses the fenced YAML inside. The `owner=` attribute in the begin marker matches the GitHub login that authored the comment (and thus the only login that can PATCH it).

**Authoritative-comment rule:** the most recent comment whose body matches `^<!-- flow:state-v1 begin` is authoritative. Older state comments are stale and can be marked off-topic / hidden by any maintainer; the SDK does not require them gone, just ignores them.

### Artifact comments (the storage)

Same pattern as before — first line is an HTML marker, content follows:

```
<!-- flow:artifact key=plan v=1 by=claude-opus-4-7 step=plan ts=2026-05-26T15:10Z -->
## Implementation plan
...
```

Artifacts are **append-only / supersede-only**: re-running a step posts a new comment with `v=2` and the state comment's `resolved_by` pointer moves to it. The SDK never edits prior artifact comments (which would fail cross-user anyway).

### Large-artifact escape hatch

For artifacts that would exceed the 64 KiB comment limit (captured test/build output, generated reports, screenshots, diffs not associated with a PR), the SDK commits them to a `flow-artifacts` orphan branch in the same repo at `flow/artifacts/issue-N/<key>/<filename>` and posts a marker comment whose body links to `raw.githubusercontent.com/<owner>/<repo>/flow-artifacts/...`. The flow author opts in by calling `ctx.ResolveFile(key, name, bytes)` instead of `ctx.ResolveComment(key, text)`; `ctx.ResolveComment` also auto-spills to file storage with a `[truncated, full output at <url>]` marker if the body exceeds ~60 KiB.

### Labels (closed vocabulary)

| Label | Purpose |
|---|---|
| `flow:seeded` | State comment has been posted |
| `flow:<flow-name>` | This flow owns the issue |
| `flow:owner:<gh-login>` | Current claim holder |
| `flow:claim:<128b-hex>` | Transient race-lock during claim |
| `flow:blocked` | Flow detected a blocker |
| `flow:needs-answer` | Open `flow:question` comment |
| `flow:disabled` | Equivalent of reference's `FlowNone` |
| `flow:stale:<key>` | Artifact marked stale |

The `flow:seeded` label is the cheap "is this issue flow-managed?" check — answerable via the labels API alone, no comment fetch required, which keeps `list` fast across hundreds of issues.

---

## SDK orchestration

### `claim N` — explicit entry; primes the local cache

`claim` is where the SDK does its heavy reads. After this completes, every subsequent `run` has a fully populated `.flow/` directory and can dispatch handlers without per-artifact API calls.

1. **Resolve token** — `gh auth token`, fall back to `GITHUB_TOKEN`.
2. **Resolve repo** — `git remote get-url origin`, parsed.
3. **Two-phase race-lock**:
   - POST label `flow:claim:<rand-128b-hex>`.
   - GET labels; if multiple `flow:claim:*` are present, the lexicographically smallest hex wins. Losers DELETE their own claim label and abort.
   - Winner: POST self as assignee, POST `flow:owner:<login>`, DELETE `flow:claim:*`.
4. **Fetch issue + all comments** — `GET /repos/{o}/{r}/issues/{n}` plus paginated `GET /repos/{o}/{r}/issues/{n}/comments`. Single round of API calls (~1-5 requests for typical issues).
5. **Locate state comment** — scan comments newest-first for `<!-- flow:state-v1 begin`. First match wins (authoritative-comment rule).
6. **Preflight** — refuse if label `flow:<other-flow>` set, `flow:disabled` present, or state-comment `flow:` differs from this binary's name.
7. **Seed if absent** — if no state comment exists, render initial state YAML from `f.Artifacts` (Go-registered), wrap in `<details>` + state-v1 markers, POST as a new comment, capture comment ID. POST `flow:seeded` + `flow:<flow-name>` labels.
8. **Supersede if owner changed** — if state comment exists but its `owner=` attribute (and author) doesn't match the current user, POST a *new* state comment authored by the current user with state copied forward, mark the old one off-topic. From here on, the new comment is "the" state comment for this owner.
9. **Prefetch all resolved artifacts** — for each artifact in the state with non-null `resolved_by`:
   - Comment-type → fetch comment, strip marker, write to `.flow/artifacts/<key>.md`.
   - PR-type → `GET /repos/{o}/{r}/pulls/{n}` for status, write JSON to `.flow/artifacts/<key>.json`.
   - File-type → `git fetch flow-artifacts && git show flow-artifacts:flow/artifacts/issue-N/<key>/<file>` (or HTTP from raw.githubusercontent), write to `.flow/artifacts/<key>/<file>`.
10. **Write `.flow/active.json`** — `{flow, repo, issue, state_comment_id, owner, claimed_at, claim_id}`. Write `.flow/state.yml` (parsed state snapshot for inspection).

After claim: handlers can read any artifact via `ctx.Artifact(key)` with zero API cost.

### `run` — one step per invocation

Pure function of (cached state + fresh state-comment fetch).

1. **Load `.flow/active.json`** — or accept `--issue N` / `FLOW_ISSUE` for an unclaimed run. If unclaimed, run an implicit minimal claim (steps 1–10 above) first.
2. **Refresh state comment** — `GET /repos/{o}/{r}/issues/comments/{state_comment_id}` (one API call). Reparse YAML.
3. **Reconcile cache** — diff cached state vs fresh state; refetch any artifacts whose `resolved_by` changed (rare in steady state; protects against another runner having advanced the issue).
4. **Resolve external-event artifacts** — for each artifact with `pr_must_be:` set and not yet resolved, `GET /pulls/{n}` to check status; if it matches, mark resolved.
5. **Derive next artifact** — first registered artifact whose `resolved_by` is null OR `stale: true`. Pure function over the in-memory state.
6. **Invoke handler** — `f.handlers[key](ctx)`. Handler reads prior artifacts via `ctx.Artifact(key)` (cache), produces a result via `ctx.ResolveComment/PR/File(...)`, or returns `flowsdk.Skip(reason)`.
7. **Resolve = write** (SDK internals):
   - POST artifact comment with marker → capture comment URL.
   - Write content to `.flow/artifacts/<key>.{md,json}` immediately.
   - PATCH the state comment to update `resolved_by`, `produced_at`, `by` for this key.
   - No CAS retry needed: only the state comment's author can PATCH it, and the claim-lock guarantees one author per worktree. If PATCH unexpectedly fails (e.g., 404 — state comment deleted; 403 — auth identity changed), fall back to the supersede path: post a new state comment, update `active.json` with the new ID, continue.
8. **Maybe close** — if all `required: true` artifacts resolved + any `pr` artifact shows merged, PATCH issue `state: closed, state_reason: completed`, remove `flow:owner:*`.
9. **Emit InvocationResult JSON** — `{flow, invocation_id, issue, step, status: done|skipped|failed, reason?}` to stdout. Exit 0/non-zero.

### Other commands

- **`status`** — load `.flow/active.json`, refresh state comment, print the artifact checklist with resolution URLs. Read-only.
- **`list`** — `GET /search/issues` with `label:flow:<name> state:open assignee:@me` (and bare `label:flow:<name>` for the unclaimed view). Cheap; never fetches comments.
- **`release`** — DELETE `flow:owner:<me>` label, unassign user, delete `.flow/`. Does *not* delete the state comment (history stays on the issue).
- **`doctor`** — verify `gh` installed, authed, repo accessible with `permissions.push == true`, write-test by toggling a temporary label.

### Why no body CAS

In the body-fenced design, every artifact resolution required reading the issue body, splicing the YAML inside a fence, PATCHing the whole body, and retrying on conflict — because multiple maintainers (and humans editing in the browser) could all write the body simultaneously.

In the comment-fenced design, the state comment is **authored by one user at a time**. GitHub enforces that only the author can PATCH a comment's content. Combined with the issue-level claim-lock (`flow:owner:<me>` plus assignee), there is exactly one writer at any time. PATCH either succeeds or fails for a deterministic reason (auth, deletion) — never for a "you raced someone" reason. The retry/CAS module is gone, and the parser only has to handle the comment body, not preserve arbitrary user text around a fence.

---

## Agent integration — `pkg/agent/`

Cleaned-up types (per user feedback):

```go
package agent

type Request struct {
    Prompt          string
    ResumeSessionID string  // empty → fresh session; set → resume that session
    PermissionMode  string  // default | acceptEdits | bypassPermissions | plan
    Model           string
    Effort          string  // low | medium | high | max
    Worktree        string  // cwd for the agent process
}

type Response struct {
    LastText        string
    ToolsUsed       []string
    CostUSD         float64
    DurationSeconds float64
    SessionID       string   // for chaining into next Request.ResumeSessionID
    Failure         *Failure // nil → success
}

type Failure struct {
    Kind    string // no-result | killed | cancelled | exit-error | start-error
    Message string
}

type Agent interface {
    Name() string
    Run(ctx context.Context, req Request) (*Response, error)
}
```

Changes vs reference `wire.go:636-659`:
- Drop `Resume bool` and `FreshSession bool`. `ResumeSessionID == ""` means fresh; non-empty means resume.
- Drop `SessionID` from Request (renamed to `ResumeSessionID`); keep `SessionID` in Response as the new session id to chain.
- Drop `Success bool` and `IsError bool`. `Failure == nil` means success.
- Drop `Question`, `RateLimit`, `ErrorText` from Response surface for v1 (can be added under `Failure.Kind == "question"` if needed).

**`pkg/agent/claude/` implementation** — spawns the CLI; pattern inferred from reference (the actual server-side launcher is in the closed `~/prog/tracker` repo):

```go
cmd := exec.CommandContext(ctx, "claude",
    "--print",
    "--input-format", "stream-json",
    "--output-format", "stream-json",
    "--model", req.Model,
    "--permission-mode", req.PermissionMode,
)
if req.ResumeSessionID != "" {
    cmd.Args = append(cmd.Args, "--resume", req.ResumeSessionID)
}
cmd.Dir = req.Worktree
stdin, _ := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
go func() {
    // Write JSONL user-event: {type:"user", message:{role:"user", content:[{type:"text", text:req.Prompt}]}}
    _ = json.NewEncoder(stdin).Encode(userEvent{...})
    stdin.Close()
}()
scanner := bufio.NewScanner(stdout)
for scanner.Scan() {
    // Parse event types:
    //   {type:"assistant", message:{content:[{type:"text", text}]}}    → append to LastText
    //   {type:"tool_use", name}                                        → append to ToolsUsed
    //   {type:"result", session_id, total_cost_usd, duration_ms, ...}  → final aggregation
}
// on ctx.Done(): send SIGINT, then SIGKILL after grace period
```

A `noop` agent is provided for steps that don't call an LLM (used internally by SDK for external-event artifacts).

---

## Access modes and the git/PR workflow

Two distinct user populations want to run a flow binary against a GitHub repo, and they have very different permissions:

| Mode | User has | Can do | Cannot do |
|---|---|---|---|
| **Maintainer** | write access to the upstream repo | assign issues, edit body, add labels, push branches to origin | — |
| **Contributor** | read-only on upstream (any authenticated GH user) | comment on issues, fork the repo, push to their fork, open cross-repo PR | assign, label, edit issue body |

**v1: maintainer mode only.** The full design (state block in issue body, label-based claim, assignee-as-ownership) requires write access. `./<flow> doctor` runs `GET /repos/{o}/{r}` and checks `permissions.push == true`; if false, it errors with an actionable message.

**Git workflow inside maintainer mode** (what the `implementation` step does, regardless of who authored the flow):
1. From the worktree, ensure clean working tree.
2. Create a branch `flow/issue-<N>` off the default branch (configurable via `f.SetBranchPrefix`).
3. Run the handler — it makes edits via the agent + commits via `pkg/git`.
4. Push branch to `origin`.
5. `gh.OpenPR(head: flow/issue-N, base: main, body: "Closes #<N>")`. The PR is the artifact.
6. `ResolvePR(prURL)` records the URL in the state block.

The handler doesn't need to manage branching/pushing directly; `StepCtx` exposes `ctx.CreateBranch()`, `ctx.Commit(msg)`, `ctx.Push()`, `ctx.OpenPR(base, title, body)` so the flow author's code stays terse. Under the hood these are thin wrappers over local `git` + `gh`.

**Contributor mode (future, not in v1)** would require:
- `gh repo fork --remote` to set up the fork as a second remote (idempotent).
- Push the branch to the fork, not origin.
- Open a cross-repo PR (`head: <fork-owner>:<branch>`, `base: <upstream-owner>:main`).
- Claim semantics that don't depend on assignee/label writes (contributors lack those permissions). Self-attribution via the contributor's own state comment plus the supersede rule covers this.

Because the state-storage mechanism is *already* "a comment authored by the current user," the contributor path drops in without changing the parser — same `state_comment.go`, same supersede rule, just a different user identity. No second code path.

---

## Repo layout (proposed paths to create)

```
/Users/djabi/prog/flow-sdk/flow-sdk/
├── go.mod                              module github.com/promise-language/flow-sdk
├── doc.go
├── wire.go                             InvocationResult, FlowName, FlowNone, IssueRef, enums
│                                       (NO ArtifactKey constants — keys are user-defined strings)
├── flow.go                             Flow type + NewFlow + Selection + AddStep
│                                       (single registration call — no separate Artifact/OnStep)
├── step.go                             Step spec + StepOption helpers (Required, AcceptsComment,
│                                       AcceptsPR, AcceptsFile, PRMustBeOpen, PRMustBeMerged,
│                                       StaleAfter, StaleOnCommit, …); duplicate-key panic
├── stepctx.go                          StepCtx with typed Artifact(key) accessor +
│                                       ResolveComment/ResolvePR/ResolveFile/Skip/MarkStale
│                                       + git helpers (CreateBranch/Commit/Push/OpenPR)
├── issue.go                            Issue type + Finalized/IsBlocked/NeedsAnswer/IsClosed
├── preflight.go                        Preflight gate (mirrors reference preflight.go)
├── result.go                           InvocationResult + Emit
├── errs.go                             ErrSkip{Reason}, ErrAwaitExternal, sentinel errors
├── *_test.go                           table-driven tests
├── cli/
│   ├── cli.go                          Run(f *Flow); cobra-style command tree
│   ├── cmd_doctor.go                   verify gh installed, authed, repo access
│   ├── cmd_list.go                     list eligible issues for this flow
│   ├── cmd_claim.go                    ★ claim an issue (race-lock + assign + seed)
│   ├── cmd_release.go                  drop claim
│   ├── cmd_status.go                   read-only artifact checklist
│   └── cmd_run.go                      ★ orchestrator: one step per invocation
├── pkg/config/
│   └── config.go                       parse OPTIONAL .github/flow.yml for machine overrides
│                                       (repo override, dry-run) — v1 may skip
├── pkg/gh/
│   ├── client.go                       go-github wrap; token from pkg/auth
│   ├── issue.go                        GetIssue, ListOpen, list+paginate comments
│   ├── claim.go                        ★ two-phase claim race-lock
│   ├── seed.go                         idempotent first-run seed (post state comment)
│   ├── artifact.go                     ResolveArtifact, MarkStale, large-artifact spillover
│   ├── state_comment.go                ★ parse/render <details>+fenced YAML; locate latest
│   │                                   state comment; PATCH or supersede; NO body CAS
│   ├── comment.go                      add comment with marker; parse markers
│   ├── pr.go                           OpenPR, PRStatus (open/approved/merged)
│   ├── artifacts_branch.go             commit/read files on flow-artifacts orphan branch
│   └── label.go                        label vocabulary helpers
├── pkg/cache/
│   └── cache.go                        ★ .flow/ layout: active.json, state.yml,
│                                       artifacts/<key>.{md,json}; refresh + reconcile
├── pkg/agent/
│   ├── agent.go                        Request/Response/Failure/Agent interface
│   ├── claude/claude.go                ★ claude CLI stream-json driver
│   └── noop/noop.go                    null agent for external-event artifacts
├── pkg/auth/
│   └── gh.go                           shell out to `gh auth token`; GITHUB_TOKEN fallback
├── pkg/git/
│   └── git.go                          status, commit, push, branch via os.exec
├── examples/
│   ├── implement/main.go               full plan→implement→review→PR flow binary
│   └── verify/main.go                  minimal one-step `go test` flow binary
└── docs/
    ├── design.md                       (this file)
    └── state-block-spec.md
```

**Star (★) marks the four highest-leverage modules to land first.**

---

## Critical files

1. **`flow.go`** — the public API users see. `NewFlow`, `AddStep`, `Selection`. The 1:1 step-artifact relationship is enforced by `AddStep`'s signature; there is no separate `Artifact()` or `OnStep()` for callers to drift out of sync. Small but the most-touched surface.
2. **`stepctx.go`** — `StepCtx` with the **typed `Artifact(key) (Artifact, bool)` accessor** and `ResolveComment/ResolvePR/ResolveFile/Skip` helpers. This is the contract handlers see; the comment-storage mechanic is hidden behind it.
3. **`cli/cmd_claim.go`** — explicit claim entrypoint; wraps the race-lock, seeds the state comment, **prefetches every resolved artifact into `.flow/artifacts/`**, writes `.flow/active.json`.
4. **`cli/cmd_run.go`** — the per-step orchestrator. Refresh state comment, derive next, dispatch handler, write artifact, update state comment, maybe close.
5. **`pkg/gh/state_comment.go`** — parse/render the `<details>`-wrapped YAML inside the state-v1 markers; locate the latest state comment by author/timestamp; PATCH-in-place (same author) or supersede (different author). **No body CAS, no fence-around-user-text logic.**
6. **`pkg/gh/claim.go`** — two-phase claim-lock; the only thing protecting against multi-runner races at the issue level.
7. **`pkg/cache/cache.go`** — `.flow/` layout, prefetch routines, reconcile-on-refresh logic. The reason handlers can call `ctx.Artifact(key)` synchronously with no API cost.
8. **`pkg/agent/claude/claude.go`** — spawn `claude`, write JSONL prompt to stdin, scan JSONL events from stdout, aggregate into Response. Field shape lines up with reference `wire.go:636-659`.
9. **`wire.go`** — `InvocationResult`, `IssueRef`, `FlowName`, `FlowNone`, enums. Mirror reference's named-constant discipline. **No artifact key constants**.
10. **`preflight.go`** — port of reference `preflight.go`; flow-mismatch check reads labels + state comment instead of `item.Flow`.

Reuse / mirror discipline from reference (`~/prog/tracker_flow_sdk/`):
- Item-derivation helpers (`item.go`) → Issue-derivation helpers in `issue.go`.
- InvocationResult shape (`result.go`) → near-identical.

---

## End-to-end lifecycle

```
Issue #42 created with label "needs-flow"  (body never modified by SDK)
       │
./implement claim 42
       │  POST flow:claim:<rand>; win race
       │  POST assignee + flow:owner:<me> + flow:implement
       │  GET issue + comments; no state comment found
       │  Seed: POST state comment with <details>+<!-- flow:state-v1 begin owner=me -->
       │        + YAML for [plan, implementation, review, pr] (all resolved_by: null)
       │  POST flow:seeded
       │  Prefetch artifacts: none resolved yet → empty .flow/artifacts/
       │  Write .flow/active.json {issue:42, state_comment_id: 9001, owner:me, ...}
       │
./implement run
       │  GET state comment (1 API call) — no changes; cache fresh
       │  derive next = plan
       │  handler reads no prior artifacts; invokes claude CLI; LastText is the plan
       │  ResolveComment(planText):
       │    POST artifact comment {<!-- flow:artifact key=plan v=1 ... -->\n<plan>}
       │    write .flow/artifacts/plan.md
       │    PATCH state comment: plan.resolved_by = <comment-url>
       │  emit InvocationResult{step: plan, status: done}
       │
./implement run
       │  GET state comment; reconcile cache
       │  derive next = implementation
       │  handler: ctx.Artifact("plan") → served from .flow/artifacts/plan.md (zero API)
       │  invokes claude CLI for code edits; commits; pushes branch; OpenPR("main", ...)
       │  ResolvePR(prURL):
       │    POST artifact comment with PR link + marker
       │    write .flow/artifacts/implementation.json {url, status: open}
       │    PATCH state comment: implementation.resolved_by = <pr-url>
       │  emit InvocationResult{step: implementation, status: done}
       │
./implement run
       │  derive next = review
       │  handler: ctx.Artifact("implementation").URL  (from cache)
       │  invokes claude (sonnet) with PR URL; ResolveComment(reviewText)
       │  emit InvocationResult{step: review, status: done}
       │
./implement run
       │  derive next = pr (PRMustBeMerged); auto-check: PR not merged
       │  emit InvocationResult{step: pr, status: skipped, reason: awaiting_merge}
       │
[ human merges PR on GitHub ]
       │
./implement run
       │  external-event check auto-resolves pr (PRMustBeMerged satisfied)
       │  PATCH state comment: pr.resolved_by = <pr-url>, pr.status = merged
       │  all required artifacts resolved → close issue (state_reason: completed)
       │  DELETE flow:owner:<me>; delete .flow/
       │  emit InvocationResult{step: close, status: done}
```

Note: the state comment is PATCHed in place throughout (same author = the claim holder). If a later claim by a *different* user occurs, that user's first action posts a fresh state comment (supersede), and the new `state_comment_id` is recorded in their `.flow/active.json`.

---

## Verification

End-to-end on a real repo:
1. `gh auth login` (precondition).
2. `cd examples/implement && go build -o implement .`
3. Create a private test repo `flow-sdk-e2e`; push it.
4. Open issue with label `needs-flow` and a simple ask ("add a hello() function").
5. `./implement doctor` → green.
6. `./implement claim 1` → verify body has the state block; labels `flow:seeded`, `flow:implement`, `flow:owner:<me>`; assignee set; `.flow/active.json` exists.
7. `./implement run` → verify a comment with `<!-- flow:artifact key=plan -->` marker is posted; body's `resolved_by` for `plan` points at it.
8. `./implement run` → PR opens with `Closes #1`; body's `implementation.resolved_by` is the PR URL.
9. `./implement run` → review comment posted.
10. `./implement run` → step `pr` returns skipped (awaiting merge).
11. Manually merge the PR.
12. `./implement run` → issue closes with `state_reason: completed`; `.flow/active.json` removed.

Unit tests (table-driven, matching reference style):
- `wire_test.go` — round-trip JSON for InvocationResult, IssueRef, Request/Response.
- `issue_test.go` — Finalized/IsBlocked/NeedsAnswer/IsClosed against fixture issues.
- `pkg/gh/state_block_test.go` — parse/render/CAS-merge round-trips, including hostile concurrent edits.
- `pkg/gh/claim_test.go` — simulate two-runner race against a mock GH server.
- `pkg/agent/claude/claude_test.go` — golden JSONL stdin/stdout fixtures; aggregation correctness; SIGINT-on-cancel.

Integration tests against a sandbox repo are gated behind `GH_INTEGRATION=1` to keep default `go test` fast.

---

## Risks / open questions

1. **`gh` CLI as a hard dep.** Not installed on the dev's current machine. `doctor` must give an actionable error pointing at `https://cli.github.com/`. Acceptable since the user chose this auth path.
2. **State-comment deletion / hiding.** A maintainer can delete the SDK's state comment from the web UI. Mitigation: `run` detects a 404 on the cached `state_comment_id`, falls back to scanning comments for a state-v1 marker; if none found, treats the issue as unseeded and re-seeds (idempotent). Worst case the user re-runs `claim`. Document.
3. **Cache vs upstream drift.** `.flow/artifacts/` is a cache. If another runner advances the issue between `claim` and `run`, the local cache is stale. Mitigation: every `run` refreshes the state comment and diffs against cached `resolved_by` URLs; changed entries are re-fetched before the handler sees them. The cache is never authoritative — GitHub is.
4. **Supersede chain pollution.** Every owner handoff posts a new state comment; over many handoffs the issue thread grows. Mitigation: on supersede, mark the prior state comment off-topic (hides it by default in the UI). Optional `flow:gc` command could DELETE truly-stale state comments — out of scope for v1.
5. **`claude` CLI stream-json contract drift.** Event-type names (`assistant`/`tool_use`/`result`) and field paths are not a stable SDK; Anthropic changes break `pkg/agent/claude`. Recommend a smoke test against a real `claude --version` in CI.
6. **Artifact-list migrations.** If a flow binary adds a new artifact after issues are seeded, those existing issues won't require it (their seeded checklist is frozen). A future `./implement migrate` command could reconcile by PATCHing the state comment with the new artifact rows; out of scope for v1.
7. **Nil-handler steps.** `f.AddStep("pr", nil, ...)` is the explicit "auto-resolved by external event" form. It's the only way a step exists without a handler. Document so users don't write a no-op `func(ctx) error { return ctx.Skip("waiting") }` instead — that would never auto-resolve and the flow would stall.
8. **One-binary-one-flow constraint.** If a project needs multiple flows (e.g., `implement` and `triage`), they're separate binaries. Sharing artifact definitions across binaries → a shared Go package the user writes. Acceptable.
9. **Large-artifact spillover correctness.** `ResolveComment` auto-spills bodies >~60 KiB to the `flow-artifacts` orphan branch. Edge cases: comment-edit history is lost when content is moved to the branch on a re-run; `flow-artifacts` branch may not exist yet on first spillover (auto-create). The `flow-artifacts` branch requires the same `permissions.push` that the rest of v1 requires — no new permission ask.
10. **Maintainer-mode-only in v1.** Read-only contributors can't claim issues (no assignee/label write), can't post the state comment without it being theirs (fine, but they can't be assigned). `doctor` errors loudly if `permissions.push == false`. Contributor mode (future) plugs in trivially because the state-storage mechanism is *already* "a comment authored by the current user" — same as everyone else. The supersede rule means the contributor's state comment is just another link in the chain.
11. **Reactor server, deliberately deferred.** A central server ("reactor") would buy webhook reactivity, cross-repo orchestration, scheduled fan-out, and shared agent rate limits — none of which v1 needs. The architecture is designed so reactor can be added later as an *optional* accelerator without changing the handler API: handlers already read/write via the typed `Artifact` surface, so a future reactor implementation could back that surface with object storage and a queue instead of comments and `.flow/`. Decision: build comment-backed v1; revisit reactor only when a concrete use case demands it.
