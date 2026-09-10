# Agent integration

> **Tag:** `agent` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines the agent interface, the metered chokepoint, permission modes, and failure kinds. Every statement is a requirement. This is the normative home for the per-step permission requirement described in [#19](https://github.com/promise-language/flow/issues/19).

## The interface

```go
type Agent interface {
    Name() string
    Run(ctx context.Context, req AgentRequest) (*AgentResponse, error)
}
```

Concrete implementations live in subpackages (the reference is `flow/claude`). The root package carries zero transitive dependencies on any agent substrate.

> **A prompt is one `Agent.Run` call.**

The request goes out, the agent works — reading, editing, calling tools, as many model calls as the task takes — and the call returns once with what it produced. This is the unit every statement about agent work is counted in: what the treasurer approves, what `MaxCostUSD` bounds, what the ledger records, and what a step is said to spend. Nothing smaller has an identity here, because a model call inside the run is the substrate's business and no caller can address one. Nothing larger exists either: a step that prompts twice made two calls, and they are two of everything the first one was one of.

**"While the agent works" is the span inside one prompt**: after the request goes out, before the call returns. It is when the agent reads and edits, when tool calls happen, when a guard refuses an action and the agent adapts to the refusal instead of losing what it has done, and when a gate has nothing to say yet because the result does not exist. Wherever this corpus says a thing happens while an agent works, it means inside that span — one dispatch, one prompt, no journal entry yet, and the step's result not written until the call comes back.

**The word is `prompt`, and it is not `turn`.** A turn in this corpus is a position in a queue — whose move it is, an exclusion arriving inside a running dispatch ([resolution.md](resolution.md) § The treasurer) — and a single word carrying both senses left every reader deciding which was meant. `Prompts` is also what a step declares when it says whether it may do this at all ([flow-registration.md](flow-registration.md) § Step configuration).

## The chokepoint

`ctx.Agent()` is the **only** route to spend on agent work. Every expense is metered here, and every expense is approved by the treasurer before it is incurred — allowed, blocked, or priced with an allowance ([resolution.md](resolution.md) § The treasurer). There is no second path. A step that needs agent work calls `ctx.Agent().Run(...)` and nothing else.

The chokepoint also **stamps the request's `Worktree`** with the arena's checkout (`Orchestrator.ArenaRoot()`), overwriting whatever the step put there. Where the agent edits is not a step's choice: it is the tree the commit will be taken in and the gates will measure, and a request that names no directory inherits the directory the binary was started in.

## Nothing mechanical may spend

A prompt is sent **only where somebody asked for work**: a step of resolving an item, against a budget, producing an artifact. Every other path answers by reading, or does not answer.

A mechanical command — `doctor`, `list`, `status` — must never spend. It runs before every item, in CI, and on every machine an operator touches, so a prompt on one of those paths is a standing charge nobody asked for. And the charge is not the worst of it: a preflight that bills the account is one an operator turns off, and a preflight nobody runs prevents nothing. `doctor` carried exactly such a prompt — one tool-free probe, capped at fifty cents, on every run, forever.

This is enforced in three places, because the mistake arrives in three shapes:

| Enforcement | Catches |
|---|---|
| The commit gate's approved list (`tools/build/common/agentturns.go`) | A new call site. The list is exact — file, function, and how many prompts each asks for — and **adding an entry is the maintainer's decision**. Removing one when the call goes away is ordinary upkeep. |
| `App.Agent` refuses `Run` (`cli`) | A prompt requested at runtime from outside a step dispatch, in a binary built from a tree that never passed the gate. The field answers `Name()` and nothing else; the real agent is reached only by the dispatch that builds the metered chokepoint. |
| The reference agent refuses to spawn from a test process (`claude`) | A test that reaches the real binary — which spends on every run, on every machine and in CI, and makes the gate's runtime a function of account state rather than of the tree. |

None of the three is a proof. A request assembled through a helper slips past the scan, and an `Agent` implementation the SDK did not write can do what it likes. They catch the honest case, which is the one that keeps happening.

## Checking the agent without spending

`AgentDoctor` is an optional `Agent` capability: report whether the agent can be invoked, **without sending a prompt**.

```go
type AgentDoctor interface {
    Doctor(ctx context.Context) error
}
```

It is what `doctor`'s agent check calls. The reference implementation spawns the binary and asks its version, which establishes that this SDK can start it — absent, unexecutable, wrong-architecture and too-old installs all fail here — and which costs nothing, because no model is called.

What it does **not** establish is that a prompt would succeed: credentials, quota and model availability are answered only by spending, and nothing mechanical may spend. An implementation must not close that gap by sending a prompt, and a report must not imply more than it checked.

The reference implementation also enforces a **minimum version**: `--max-budget-usd` only stops a run at the cap from claude CLI v2.1.217. On anything older `AgentRequest.MaxCostUSD` is accepted and ignored, so a step's cost grant stops bounding the prompt — a failure that is invisible until an overrun, which is precisely the kind a preflight exists to catch.

An agent with no `AgentDoctor` is reported as **skipped**, not failed. The SDK cannot check a black-box `Agent` for free, and that is a fact about the interface rather than about the machine.

## AgentRequest

`AgentRequest` is the spawn payload for one `Agent.Run` call:

| Field | Type | Meaning |
|---|---|---|
| `Prompt` | `string` | The task prompt. |
| `PermissionMode` | `string` | One of the closed set below. |
| `Model` | `string` | Model identifier. |
| `Effort` | `string` | `low`, `medium`, `high`, or `max`. |
| `MaxCostUSD` | `float64` | Ceiling on what this prompt may spend, in USD. Zero means unbounded. |
| `Worktree` | `string` | Working directory for the agent process. **Set by the SDK at the chokepoint, never by the step**: the arena's checkout (`Orchestrator.ArenaRoot()`), the same tree the commit is taken in and the gates measure. |
| `ResumeSessionID` | `string` | Non-empty resumes that exact session. Empty means "don't actively resume a specific session." |
| `FreshSession` | `bool` | Discard any inherited session state — spawn from a clean slate. |

### MaxCostUSD contract

`MaxCostUSD` is the allowance the treasurer priced for this expense. An implementation that can enforce it passes it to the substrate and reports a stop as `AgentFailure{Kind: FailureCostCap}`. The bound is not exact: the substrate learns what a model call cost only after it returns, so the prompt stops at the **first call that crosses the cap**. The overrun is bounded by one model call, not by a whole prompt — that is the difference this axis provides.

An implementation that cannot enforce it may ignore the field; the treasurer's chokepoint consultations still apply. A caller setting this field is asking for a tighter ceiling; a metered wrapper must narrow it, never widen it.

## Permission modes

The `PermissionMode` field is a closed set:

| Mode | Meaning |
|---|---|
| `default` | Standard permissions — the agent asks before acting. |
| `acceptEdits` | The agent may edit files without confirmation. |
| `bypassPermissions` | All permission prompts are bypassed. |
| `plan` | The agent produces a plan through the harness's plan-submission tool, ending at that tool call rather than in assistant text. |
| `auto` | Fully autonomous operation. |

### Per-step permission requirement (#19)

Each step declares what permission mode it needs. A step calling an agent without the appropriate mode is a misconfiguration. The specification of how steps declare their required mode is [#19](https://github.com/promise-language/flow/issues/19)'s scope; this document is its normative home once written.

## AgentResponse

`AgentResponse` is the aggregated result of one `Agent.Run` call:

| Field | Type | Meaning |
|---|---|---|
| `LastText` | `string` | The **last** assistant text block the agent produced — not every text block joined. An agent that ends on a tool call emits preamble before each one, and concatenating those produces an artifact made entirely of narration. |
| `PlanText` | `string` | What the agent submitted through the plan-submission tool. Empty when the agent did not end that way. |
| `PlanSubmitted` | `bool` | Whether the agent called the plan-submission tool. The pair `(PlanSubmitted=true, PlanText="")` is the case worth failing on: the agent produced a plan and the transport lost it. |
| `ToolsUsed` | `[]string` | Tools the agent invoked. |
| `CostUSD` | `float64` | Cost of this prompt. |
| `DurationSeconds` | `float64` | Wall-clock time. |
| `SessionID` | `string` | For chaining via `Request.ResumeSessionID`. |
| `Failure` | `*AgentFailure` | Nil on success; non-nil carries structured failure info. |

## AgentFailure

`AgentFailure` carries structured failure info when `Failure` is non-nil:

| Field | Type | Meaning |
|---|---|---|
| `Kind` | `string` | One of the six kinds below. |
| `Transient` | `bool` | Infrastructure failure — see below. |
| `Message` | `string` | Human-readable detail. |

### Failure kinds

The `Kind` field is drawn from a closed set:

| Kind | Meaning |
|---|---|
| `no-result` | The agent produced no usable output. |
| `killed` | The agent process was killed. |
| `cancelled` | The context was cancelled. |
| `exit-error` | The agent process exited with a non-zero code. |
| `start-error` | The agent process could not be started. |
| `cost-cap` | The substrate stopped the prompt because it reached `MaxCostUSD`. |

### Transient failures

When `Transient` is true, the failure is infrastructure (remote runner died, network blip, transient 5xx) rather than a real agent-side failure. The orchestrator parks the step with `ParkInfraTransient`, and the treasurer does not count the attempt — a flapping runner must not spend the resolution's budget ([environment.md](environment.md)).

Agent implementations (typically a backend's runner-HTTP wrapper) set `Transient` from substrate-specific signals.

## Cross-references

- [step-handler.md](step-handler.md) — `ctx.Agent()` is how handlers reach the agent.
- [resolution.md](resolution.md) — the treasurer and park semantics.
- [flow-registration.md](flow-registration.md) — step declaration.
