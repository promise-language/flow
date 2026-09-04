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

## The chokepoint

`ctx.Agent()` is the **only** route to spend agent budget. Budget is metered here — cost, invocations, and prompts per invocation. There is no second path. A step that needs agent work calls `ctx.Agent().Run(...)` and nothing else.

## Nothing mechanical may spend

A turn happens **only where somebody asked for work**: a step of resolving an item, against a budget, producing an artifact. Every other path answers by reading, or does not answer.

A mechanical command — `doctor`, `list`, `status` — must never spend. It runs before every item, in CI, and on every machine an operator touches, so a turn on one of those paths is a standing charge nobody asked for. And the charge is not the worst of it: a preflight that bills the account is one an operator turns off, and a preflight nobody runs prevents nothing. `doctor` carried exactly such a turn — one tool-free probe, capped at fifty cents, on every run, forever.

This is enforced in three places, because the mistake arrives in three shapes:

| Enforcement | Catches |
|---|---|
| The commit gate's approved list (`tools/build/common/agentturns.go`) | A new call site. The list is exact — file, function, and how many turns each asks for — and **adding an entry is the maintainer's decision**. Removing one when the call goes away is ordinary upkeep. |
| `App.Agent` refuses `Run` (`cli`) | A turn requested at runtime from outside a step dispatch, in a binary built from a tree that never passed the gate. The field answers `Name()` and nothing else; the real agent is reached only by the dispatch that builds the metered chokepoint. |
| The reference agent refuses to spawn from a test process (`claude`) | A test that reaches the real binary — which spends on every run, on every machine and in CI, and makes the gate's runtime a function of account state rather than of the tree. |

None of the three is a proof. A request assembled through a helper slips past the scan, and an `Agent` implementation the SDK did not write can do what it likes. They catch the honest case, which is the one that keeps happening.

## Checking the agent without spending

`AgentDoctor` is an optional `Agent` capability: report whether the agent can be invoked, **without starting a turn**.

```go
type AgentDoctor interface {
    Doctor(ctx context.Context) error
}
```

It is what `doctor`'s agent check calls. The reference implementation spawns the binary and asks its version, which establishes that this SDK can start it — absent, unexecutable, wrong-architecture and too-old installs all fail here — and which costs nothing, because no model is called.

What it does **not** establish is that a turn would succeed: credentials, quota and model availability are answered only by spending, and nothing mechanical may spend. An implementation must not close that gap by running a turn, and a report must not imply more than it checked.

The reference implementation also enforces a **minimum version**: `--max-budget-usd` only stops a run at the cap from claude CLI v2.1.217. On anything older `AgentRequest.MaxCostUSD` is accepted and ignored, so a step's cost grant stops bounding the turn — a failure that is invisible until an overrun, which is precisely the kind a preflight exists to catch.

An agent with no `AgentDoctor` is reported as **skipped**, not failed. The SDK cannot check a black-box `Agent` for free, and that is a fact about the interface rather than about the machine.

## AgentRequest

`AgentRequest` is the spawn payload for one `Agent.Run` call:

| Field | Type | Meaning |
|---|---|---|
| `Prompt` | `string` | The task prompt. |
| `PermissionMode` | `string` | One of the closed set below. |
| `Model` | `string` | Model identifier. |
| `Effort` | `string` | `low`, `medium`, `high`, or `max`. |
| `MaxCostUSD` | `float64` | Ceiling on what this turn may spend, in USD. Zero means unbounded. |
| `Worktree` | `string` | Working directory for the agent process. |
| `ResumeSessionID` | `string` | Non-empty resumes that exact session. Empty means "don't actively resume a specific session." |
| `FreshSession` | `bool` | Discard any inherited session state — spawn from a clean slate. |

### MaxCostUSD contract

`MaxCostUSD` is the headroom left in the step's cost grant. An implementation that can enforce it passes it to the substrate and reports a stop as `AgentFailure{Kind: FailureCostCap}`. The bound is not exact: the substrate learns what a model call cost only after it returns, so the turn stops at the **first call that crosses the cap**. The overrun is bounded by one model call, not by a whole turn — that is the difference this axis provides.

An implementation that cannot enforce it may ignore the field; the caller's pre-dispatch and pre-prompt budget gates still apply. A caller setting this field is asking for a tighter ceiling; a metered wrapper must narrow it, never widen it.

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
| `LastText` | `string` | The **last** assistant text block of the turn — not every text block joined. A turn that ends on a tool call emits preamble before each one, and concatenating those produces an artifact made entirely of narration. |
| `PlanText` | `string` | What the agent submitted through the plan-submission tool. Empty when the turn did not end that way. |
| `PlanSubmitted` | `bool` | Whether the agent called the plan-submission tool. The pair `(PlanSubmitted=true, PlanText="")` is the case worth failing on: the agent produced a plan and the transport lost it. |
| `ToolsUsed` | `[]string` | Tools the agent invoked. |
| `CostUSD` | `float64` | Cost of this turn. |
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
| `cost-cap` | The substrate stopped the turn because it reached `MaxCostUSD`. |

### Transient failures

When `Transient` is true, the failure is infrastructure (remote runner died, network blip, transient 5xx) rather than a real agent-side failure. The orchestrator:

1. Parks the step with `ParkInfraTransient`.
2. **Skips** the `BumpInvocations` call — a flapping runner must not burn the step's invocation budget.

Agent implementations (typically a backend's runner-HTTP wrapper) set `Transient` from substrate-specific signals.

## Cross-references

- [step-handler.md](step-handler.md) — `ctx.Agent()` is how handlers reach the agent.
- [resolution.md](resolution.md) — budget metering and park semantics.
- [flow-registration.md](flow-registration.md) — step declaration and budget configuration.
