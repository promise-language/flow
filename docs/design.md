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
| Artifact storage on the issue | **Issue body fenced state block + marker comments hybrid.** SDK manages a `<!-- flow:state-v1 ... -->` YAML fence in the body; each artifact is a comment with an HTML marker. Files attached to those comments. |
| GitHub auth | **Shell out to `gh` CLI** (`gh auth token` for the PAT; `gh auth login` to authenticate). Hard runtime dep on `gh`. |
| Per-project config file | **Optional/minimal.** Defaults from the binary itself. A small `.github/flow.yml` may be read if present for machine-level overrides (e.g., repo override, dry-run); v1 may skip it entirely. |
| `.flow/active.json` pointer file | **Kept, slim.** Written by `claim`, deleted by `release`. Records `{flow, repo, issue, claimed_at, claim_id}` only — no tokens (gh handles those), no URLs (computed). One claim per worktree. `run` reads it to know which issue to advance. Reference SDK's heavier `.flow/context.json` shape (tokens, URLs, runner config) is dropped — those concepts don't exist here. |
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

A flow binary in full:

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

    // Artifact checklist. Order matters: first unresolved is the next step.
    // Keys are opaque strings — the SDK never hardcodes any.
    f.Artifact("plan",           flowsdk.Required, flowsdk.AcceptsComment)
    f.Artifact("implementation", flowsdk.Required, flowsdk.AcceptsPR, flowsdk.PRMustBeOpen)
    f.Artifact("review",         flowsdk.Required, flowsdk.AcceptsComment)
    f.Artifact("pr",             flowsdk.Required, flowsdk.AcceptsPR, flowsdk.PRMustBeMerged)

    // Step handlers. Each must resolve its artifact or return an error / skip.
    f.OnStep("plan", func(ctx flowsdk.StepCtx) error {
        resp, err := claude.New().Run(ctx, claude.Request{
            Prompt:         fmt.Sprintf("Issue: %s\n\n%s\n\nProduce an implementation plan.",
                                        ctx.Issue.Title, ctx.Issue.Body),
            Model:          "claude-opus-4-7",
            PermissionMode: "plan",
            Effort:         "high",
        })
        if err != nil { return err }
        return ctx.ResolveComment(resp.LastText)
    })

    f.OnStep("implementation", func(ctx flowsdk.StepCtx) error {
        // Use ctx.Artifact("plan").Content for the plan body.
        // Drive claude through code edits + commits + push.
        // Then open a PR with "Closes #<issue>" in the body.
        prURL, err := ctx.OpenPR("main", "feat: " + ctx.Issue.Title, /* body */ "")
        if err != nil { return err }
        return ctx.ResolvePR(prURL)
    })

    f.OnStep("review", func(ctx flowsdk.StepCtx) error {
        resp, err := claude.New().Run(ctx, claude.Request{
            Prompt: "Review PR " + ctx.Artifact("implementation").URL,
            Model:  "claude-sonnet-4-6",
        })
        if err != nil { return err }
        return ctx.ResolveComment(resp.LastText)
    })

    // Dumb command step — no agent. Resolves with command output as the artifact.
    f.OnStep("verify", func(ctx flowsdk.StepCtx) error {
        out, err := exec.Command("go", "test", "./...").CombinedOutput()
        body := "```\n" + string(out) + "\n```"
        if err != nil {
            return ctx.ResolveCommentWithStatus(body, flowsdk.StatusFailed)
        }
        return ctx.ResolveComment(body)
    })

    // No handler needed for "pr" — its resolution comes from PRMustBeMerged.
    // SDK auto-resolves when the linked PR's state matches; no agent call.

    cli.Run(f)  // exposes: doctor, list, claim, release, status, run [--issue N]
}
```

Key shape choices:
- `flowsdk.NewFlow(name)` — the binary's name is the flow's name; hardcoded by the author.
- `Selection` is exposed as a struct field for direct mutation; no setter ceremony.
- `Artifact(key, opts...)` is variadic; the option helpers (`Required`, `AcceptsComment`, `PRMustBeMerged`) are typed for IDE discoverability.
- `OnStep` registers a handler by artifact key. The handler is what `flow run` invokes when that key is the next unresolved artifact.
- `StepCtx` carries: the parsed `Issue`, prior `Artifact(key)` lookups, `Context()` for cancellation, the GitHub client, and the resolve helpers (`ResolveComment`, `ResolvePR`, `ResolveCommentWithStatus`).
- The SDK auto-resolves artifacts whose resolution is purely external (e.g., PR-merge gate). The handler for such artifacts is optional.

---

## Issue state schema (on GitHub)

**Issue body** — SDK only manages a single fenced block; never touches text outside it:

```
<original user-written description, untouched>

<!-- flow:state-v1 begin -->
```yaml
flow: implement
seeded_at: 2026-05-26T15:00:00Z
schema: 1
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
    resolved_by: null
  - key: pr
    required: true
    accepts: [pr]
    pr_must_be: merged
    resolved_by: null
```
<!-- flow:state-v1 end -->
```

**Artifact comments** — first line is the marker:
```
<!-- flow:artifact key=plan v=1 by=claude-opus-4-7 step=plan ts=2026-05-26T15:10Z -->
## Implementation plan
...
```

**Labels (closed vocabulary):**
| Label | Purpose |
|---|---|
| `flow:seeded` | State block has been written |
| `flow:<flow-name>` | This flow owns the issue |
| `flow:owner:<gh-login>` | Current claim holder |
| `flow:claim:<128b-hex>` | Transient race-lock during claim |
| `flow:blocked` | Flow detected a blocker |
| `flow:needs-answer` | Open `flow:question` comment |
| `flow:disabled` | Equivalent of reference's `FlowNone` |
| `flow:stale:<key>` | Artifact marked stale |

---

## SDK orchestration (what `cli.Run(f)`'s `run` subcommand does)

Pure function of issue state. Each `./<binary> run` does:

1. **Resolve token** — `exec.Command("gh", "auth", "token")` → PAT. Fall back to `GITHUB_TOKEN` env var.
2. **Resolve repo** — from `git remote get-url origin` parsed via `go-git` or `os/exec`.
3. **Resolve target issue** — in priority order:
   - `--issue N` flag,
   - `FLOW_ISSUE` env var,
   - `.flow/active.json` in the worktree (written by `claim`); validate the `flow` field matches this binary,
   - fallback discovery: the single open issue with assignee == me AND label `flow:<flow-name>` (with a warning that no active.json was found). Error if zero or >1.
4. **Fetch issue** — `GET /repos/{o}/{r}/issues/{n}` + parse state block.
5. **Preflight** — refuse if label `flow:<other-flow>` is set, `flow:disabled` present, or state-block `flow:` differs from this binary's name. Mirrors reference `preflight.go:57-69`.
6. **Seed if absent** — if body lacks `<!-- flow:state-v1 begin -->`, render initial state block from `f.Artifacts` (registered in Go), PATCH body, POST `flow:seeded` + `flow:<flow-name>`. Idempotent.
7. **Resolve external-event artifacts first** — for each artifact whose `pr_must_be` is set and not yet resolved, check the linked PR's status; auto-resolve if it matches.
8. **Derive next artifact** — first registered artifact whose `resolved_by` is null OR `stale: true`. Pure function.
9. **Invoke handler** — `f.handlers[key](ctx)`. Handler resolves via `ctx.ResolveComment(...)`, `ctx.ResolvePR(...)`, or returns `flowsdk.ErrSkip{Reason: ...}` to indicate "no progress possible right now."
10. **Resolve = write** — `ctx.ResolveComment(text)` POSTs the artifact comment with marker, then CAS-edits the body state block to set `resolved_by`, `produced_at`, `by`. (Body has no `If-Match`; we read-modify-PATCH and retry on conflict, max 3 attempts. Single-writer is enforced by `claim` taking exclusive ownership via `flow:owner:<me>`.)
11. **Maybe close** — if all `required: true` artifacts are resolved + the `pr` artifact (if present) shows merged, PATCH issue `state: closed, state_reason: completed`, remove `flow:owner:*`. Auto-close from `Closes #N` in the merged PR is a benign no-op.
12. **Emit InvocationResult JSON** — `{flow, invocation_id, issue, step, status: done|skipped|failed, reason?}` to stdout. Exit 0/non-zero.

`claim N` does steps 1, 2, 4, 5, 6 plus the two-phase claim-lock (below), and writes `.flow/active.json`, without invoking a step.
`release N` removes `flow:owner:<me>`, unassigns, and deletes `.flow/active.json`.
`status` does steps 1–7 read-only.

**Claim race-lock** (`pkg/gh/claim.go`):
- POST label `flow:claim:<rand-128b-hex>`.
- GET labels; if multiple `flow:claim:*` are present, the lexicographically smallest hex wins. Losers DELETE their own claim label and retry.
- Winner: POST self as assignee, POST `flow:owner:<login>`, DELETE `flow:claim:*`.

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
- A different state-storage mechanism (the contributor cannot edit the issue body): store the per-issue state block as YAML inside a single contributor-owned tracking comment they edit in place. Claim becomes self-attribution in that comment rather than a label.

Documenting this upfront so v1 doesn't paint itself into a corner: `pkg/gh/state_block.go` is written to accept either "body-fenced" or "comment-fenced" storage; v1 only wires the body path, but the parser is location-agnostic.

---

## Repo layout (proposed paths to create)

```
/Users/djabi/prog/flow-sdk/flow-sdk/
├── go.mod                              module github.com/promise-language/flow-sdk
├── doc.go
├── wire.go                             InvocationResult, FlowName, FlowNone, IssueRef, enums
│                                       (NO ArtifactKey constants — keys are user-defined strings)
├── flow.go                             Flow type + NewFlow + Selection + Artifact + OnStep
├── artifact.go                         Artifact spec + option helpers (Required, AcceptsComment,
│                                       AcceptsPR, AcceptsFile, PRMustBeOpen, PRMustBeMerged, …)
├── stepctx.go                          StepCtx + ResolveComment/ResolvePR/ResolveFile/Skip
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
│   ├── issue.go                        GetIssue, ListOpen, parse state block
│   ├── claim.go                        ★ two-phase claim race-lock
│   ├── seed.go                         idempotent first-run seed from f.Artifacts
│   ├── artifact.go                     ResolveArtifact, MarkStale, ScrapeFileURL
│   ├── state_block.go                  ★ parse/render fenced YAML + body-CAS retry
│   ├── comment.go                      add comment with marker; parse markers
│   ├── pr.go                           OpenPR, PRStatus (open/approved/merged)
│   └── label.go                        label vocabulary helpers
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

1. **`flow.go`** — the public API users see. `NewFlow`, `Artifact`, `OnStep`, `Selection`. Small but the most-touched surface.
2. **`cli/cmd_run.go`** — the orchestrator (steps 1–12 above). The user's "manage steps in the SDK" goal lives here.
3. **`cli/cmd_claim.go`** — explicit claim entrypoint; wraps the race-lock + assign + seed; writes `.flow/active.json`.
4. **`pkg/gh/state_block.go`** — parse/render the fenced YAML; the body-PATCH CAS retry. Hardest module.
5. **`pkg/gh/claim.go`** — two-phase claim-lock; the only thing protecting against multi-runner races.
6. **`pkg/agent/claude/claude.go`** — spawn `claude`, write JSONL prompt to stdin, scan JSONL events from stdout, aggregate into Response. Field shape lines up with reference `wire.go:636-659`.
7. **`stepctx.go`** — `StepCtx` with `ResolveComment/ResolvePR/ResolveFile` helpers; the surface every handler touches.
8. **`wire.go`** — `InvocationResult`, `IssueRef`, `FlowName`, `FlowNone`, enums. Mirror reference's named-constant discipline. **No artifact key constants**.
9. **`preflight.go`** — port of reference `preflight.go`; flow-mismatch check reads labels + state block instead of `item.Flow`.

Reuse / mirror discipline from reference (`~/prog/tracker_flow_sdk/`):
- Item-derivation helpers (`item.go`) → Issue-derivation helpers in `issue.go`.
- InvocationResult shape (`result.go`) → near-identical.

---

## End-to-end lifecycle

```
Issue #42 created with label "needs-flow"
       │
./implement claim 42
       │  POST flow:claim:<rand>;  win race
       │  POST assignee + flow:owner:<me> + flow:implement
       │  Seed body: append <!-- flow:state-v1 ... --> with [plan, implementation, review, pr]
       │  POST flow:seeded
       │  Write .flow/active.json {flow: implement, issue: 42, ...}
       │
./implement run
       │  derive next = plan
       │  handler invokes claude CLI; LastText becomes the plan
       │  POST artifact comment; CAS-edit body resolved_by
       │  emit InvocationResult{step: plan, status: done}
       │
./implement run
       │  derive next = implementation
       │  handler invokes claude CLI for code edits; commits; pushes branch; OpenPR("main", ...)
       │  ResolvePR(prURL) sets resolved_by; PR has "Closes #42" in body
       │  emit InvocationResult{step: implementation, status: done}
       │
./implement run
       │  derive next = review
       │  handler invokes claude (sonnet) with PR URL; ResolveComment(reviewText)
       │  emit InvocationResult{step: review, status: done}
       │
./implement run
       │  derive next = pr (PRMustBeMerged); auto-check: PR not merged
       │  emit InvocationResult{step: pr, status: skipped, reason: awaiting_merge}
       │
[ human merges PR on GitHub ]
       │
./implement run
       │  step 7 auto-resolves pr (PRMustBeMerged satisfied)
       │  all required artifacts resolved → close issue (state_reason: completed)
       │  DELETE flow:owner:<me>; delete .flow/active.json
       │  emit InvocationResult{step: close, status: done}
```

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
2. **Body-edit races.** Single-writer (claim-lock) plus client-side CAS retry covers the common case. A human editing the body in the browser simultaneously could still clobber the state block — mitigation: SDK preserves text outside the fence; next run can repair from comments.
3. **`claude` CLI stream-json contract drift.** Event-type names (`assistant`/`tool_use`/`result`) and field paths are not a stable SDK; Anthropic changes break `pkg/agent/claude`. Recommend a smoke test against a real `claude --version` in CI.
4. **Artifact-list migrations.** If a flow binary adds a new artifact after issues are seeded, those existing issues won't require it (their seeded checklist is frozen). A future `./implement migrate` command could reconcile; out of scope for v1.
5. **`flow.OnStep` for non-handler artifacts.** The `pr` artifact in the example has no handler — SDK auto-resolves it. Document this clearly so users don't write a no-op handler by mistake.
6. **One-binary-one-flow constraint.** If a project needs multiple flows (e.g., `implement` and `triage`), they're separate binaries. Sharing artifact definitions across binaries → a shared Go package the user writes. Acceptable.
7. **Maintainer-mode-only in v1.** Read-only contributors can't claim issues (no assignee/label write), can't seed (no body edit), and need a fork+cross-repo-PR git flow. `doctor` errors loudly if `permissions.push == false`. Contributor mode is a future feature; `pkg/gh/state_block.go` parser is location-agnostic from day one so the contributor path can plug in without a refactor.
