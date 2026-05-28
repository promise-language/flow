# flow

A Go SDK for declarative, stateless-per-step automation against
task-tracking systems. Write a flow as a Go binary; the SDK turns each
invocation into one advance-the-state step against a GitHub Issue (or any
backend you plug in).

- **Backends are pluggable.** This repo ships the reference GitHub Issues
  backend. State lives in a single machine-managed comment per issue plus
  per-artifact comments; large blobs spill to an orphan branch in the same
  repo.
- **Agents are pluggable.** Reference impl drives the [`claude`][claude] CLI
  via stream-json. Other CLIs slot in by implementing `flow.Agent`.
- **No server.** A flow binary is a single static `main()` that imports the
  SDK and calls `cli.Run(app)`. End users install `gh`, log in once, and
  run the binary.

[claude]: https://docs.anthropic.com/en/docs/claude-code

```
$ ./fix claim 42             # acquire a claim on issue #42
$ ./fix run-step             # advance ONE lifecycle item (one prompt → one durable artifact)
$ ./fix run-step             # next lifecycle item
$ ./fix status               # read-only lifecycle checklist
$ ./fix grant plan --invocations 3 --cost 10
$ ./fix release              # drop the claim
```

Each `run-step` invocation is a single forward tick: it inspects state,
picks the first pending lifecycle item, runs its handler, persists the
result, and exits. A future `run-all` (and a bundled `auto` / `process`
that wraps `claim → run-all → release` in one call) is planned.

Full architecture in [docs/design.md](docs/design.md).

## Status

v1 in progress. The SDK foundation, the cli orchestrator, the claude
driver, and the github backend are landed; 80 tests pass across five
packages. The two reference example binaries
([`examples/verify`](examples/verify) and
[`examples/fix`](examples/fix)) build cleanly. A `gh`-gated
end-to-end integration test is in place but not yet wired into CI.

## Install

Requires:

- Go 1.26+
- [`gh` CLI](https://cli.github.com/) authenticated against the target
  repository
- `git` on PATH for worktree ops
- Optional: `claude` CLI on PATH for any flow that calls `ctx.Agent()`

The SDK is a library — you embed it in your own flow binary:

```go
import (
    "github.com/promise-language/flow"
    "github.com/promise-language/flow/claude"
    "github.com/promise-language/flow/cli"
    "github.com/promise-language/flow/pkg/backend/github"
)
```

## Concepts

A **flow** is an ordered list of lifecycle items, each producing one of
two kinds of durable state:

- **Artifacts** — handler-produced (`flag` / `commit-hash` / `markdown` /
  `json` / `file` / `patch`). The handler calls `ctx.Resolve*` before
  returning.
- **Signals** — backend-observed booleans (`pr-open`, `pr-merged`, etc.).
  The handler triggers a side effect; the backend writes the signal.

Each artifact carries a per-axis **budget** (`MaxInvocations` /
`MaxPromptsPerInvocation` / `MaxCostUSD` / `Timeout`) seeded once when the
item is first claimed and only mutated via `grant`. A flow can declare
**RequireSignal** preconditions so one flow gates on another's
completion — the SDK uses this to model contributor (open PR) vs.
maintainer (merge PR) lifecycles on the same issue.

## A minimal flow binary

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
        BinaryName: "fix",
        VerifyCmd:  []string{"bash", "bin/verify.sh"},
    })
    if err != nil { panic(err) }

    f := flow.NewFlow("fix", []flow.ItemType{"task"})
    f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
        resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
            Prompt: "Plan implementation of issue: " + ctx.Item().Title,
        })
        if err != nil { return err }
        return ctx.ResolveMarkdown(resp.LastText)
    })

    os.Exit(cli.Run(cli.App{
        Backend: backend,
        Agent:   claude.New(),
        Artifacts: []flow.ArtifactDef{
            flow.Artifact("plan", flow.ArtifactMarkdown),
        },
        Flows: []*flow.Flow{f},
    }))
}
```

A larger contributor + maintainer flow lives at
[examples/fix/main.go](examples/fix/main.go) — two flows (`fix` and
`merge`) in one binary, gated by `RequireSignal("pr-open")`. A one-step
`go test ./...` flow lives at [examples/verify/main.go](examples/verify/main.go).

## CLI surface

| command | behavior |
|---|---|
| `doctor` | verify gh + repo push permissions; ✅ / ❌ on the result |
| `list` | list issues this flow can process |
| `claim <id>` (alias `lease`) | acquire an exclusive claim; seeds the state comment |
| `run-step` | advance ONE lifecycle item; emits an `InvocationResult` JSON. Re-run until the flow reports `done` |
| `status [<id>]` | read-only checklist |
| `grant <artifact-id> --invocations N --cost USD --prompts N --timeout SECONDS` | extend a parked step's budget. `<artifact-id>` is the id passed to `AddStep` (e.g. `plan`), NOT the human step name (`"write plan"`) |
| `release` | drop the claim |

**Planned** (not yet implemented):
- `run-all` — loop `run-step` until the flow reports `done`, parked, or asks a question
- `auto` / `process` (name TBD) — bundle `claim` + `run-all` + `release` into one
  invocation; ideal for `cron`-driven runs against a queue of eligible issues

All commands live in [cli/](cli/); the binary opts in by calling
`cli.Run(cli.App{...})`.

## Repo layout

```
.
├── doc.go                  package doc
├── flow.go                 Flow, NewFlow, AddStep/AddSignalStep/AwaitSignal/RequireSignal, derivation
├── step.go stepctx.go      StepHandler + StepOption + StepCtx interface
├── artifact.go signal.go   ArtifactDef/SignalDef + the six ArtifactTypes + PatchBody/FileBody
├── backend.go              Backend interface; Worktree interface; Item/Claim/ItemRef/ItemState
├── budget.go               StepBudget + defaults
├── agent.go                Agent interface + AgentRequest/Response/Failure
├── errs.go wire.go         sentinel errors + ParkRequest/InvocationResult/Question types
├── cli/                    program-level CLI (cmd_claim, cmd_run, cmd_grant, ...) + orchestrator
├── claude/                 reference Agent impl: spawns the claude CLI
├── pkg/backend/fake/       in-memory backend for SDK tests
├── pkg/backend/github/     GitHub-Issues-backed backend (state_comment, claim race-lock,
│                           worktree, signal polling, orphan-branch artifact spillover)
├── examples/verify/        minimal one-step "run go test" flow
├── examples/fix/           contributor (fix) + maintainer (merge) flows on one issue
└── docs/design.md          full architecture spec
```

## License

Dual-licensed under either:

- [MIT License](LICENSE-MIT)
- [Apache License 2.0](LICENSE-APACHE)

at your option.
