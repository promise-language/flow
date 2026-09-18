# Claude agent

> **Tag:** `claude-agent` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how `flow/claude` binds the [`Agent`](agent.md) interface to the Claude Code CLI: how a prompt is sent, how the harness's output is read, where its account and allowance are found, and how the guards stand in front of it. Every statement is a requirement. What any substrate must provide is [agent.md](agent.md); this document states only how this one provides it.

## Invocation

**One prompt is one `claude` process.** The binary is `claude`, found on `PATH` unless the client names another. It is spawned in `AgentRequest.Worktree`, never through a shell, with:

| Argument | Carries |
|---|---|
| `--print --verbose` | Non-interactive operation, with the event stream a result can be read from |
| `--input-format stream-json --output-format stream-json` | The prompt in, the events out (§ The event stream) |
| `--model <Model>` | `Model`, when non-empty |
| `--permission-mode <PermissionMode>` | `PermissionMode`, when non-empty (§ Request mapping) |
| `--effort <Effort>` | `Effort`, when non-empty (§ Request mapping) |
| `--max-budget-usd <MaxCostUSD>` | `MaxCostUSD`, when positive, written exactly — never rounded, since rounding up grants more than the treasurer priced |
| `--resume <ResumeSessionID>` | The handle, when the chokepoint stamped one (§ Sessions) |
| `--allowed-tools` / `--disallowed-tools` | The client's tool restrictions, one argument per entry |

**The prompt is one `user` event on stdin, and stdin is closed after it.** Closing it is what tells the harness the prompt is complete; a prompt passed as an argument would be visible to every process listing on the host and bounded by the argument limit.

**The minimum version is 2.1.217**, the first release on which `--max-budget-usd` stops the run at the cap. On anything older the flag is accepted and ignored, so a cost grant stops bounding the prompt — and nothing reports it until the overrun. `Doctor` refuses an older install.

## Request mapping

**`PermissionMode` maps to itself.** The SDK's five spellings are all `--permission-mode` values this harness accepts, each with the meaning [agent.md](agent.md) § Permission modes assigns it; the harness's further modes are not reachable through the interface. `plan` ends at the harness's plan-submission tool (§ The event stream).

**`Effort` is carried verbatim to `--effort`, and `Efforts` declares what may be carried.** For each model, `Efforts(model)` returns the `--effort` values the harness accepts for that model, from least to most — drawn from `low`, `medium`, `high`, `xhigh` and `max` — and a level outside that set is refused at the chokepoint before anything is spawned ([agent.md](agent.md) § Effort levels). Nothing is mapped: another agent's level is refused here, not translated.

**`MaxCostUSD` is enforced**, and `flow/claude` implements `AgentLimits` answering `true`. A stop at the cap is a `result` event with subtype `error_max_budget_usd`, reported as `cost-cap` with the prompt's cost kept.

## The event stream

The harness writes one JSON object per line. Lines that are not JSON are skipped; a line may be long, and the reader's buffer is sized for it.

| Event | Read for |
|---|---|
| `system` | `session_id` — the conversation this prompt is in, announced before any work |
| `assistant` | Content blocks: `text` (the latest non-blank one is kept), `tool_use` (its `name` is added to `ToolsUsed`, once per name) |
| `user` | A `tool_result` answering one of this prompt's **delegations** — a `tool_use` named `Task` or `Agent` — whose text is the delegated agent's output |
| `rate_limit_event` | The harness's own statement that the account refused (§ The agent account) |
| `result` | The end of the prompt: `result`, `session_id`, `total_cost_usd`, `duration_ms`, `is_error`, `subtype` |

**A successful parse requires a `result` event.** A stream that ends without one has produced no prompt, whatever text it carried.

**`LastText` is chosen in this order:** the `result` event's `result`, when non-empty; otherwise `PlanText`; otherwise the last text the prompt produced — an assistant `text` block, or a delegated agent's output, whichever came later. `result` is empty for a prompt that ended on a tool call, which is how a plan-mode prompt ends, and the plan is the deliverable there rather than the preamble before the call.

**A delegation's output is text the prompt produced.** A prompt that hands its work to a delegated agent says one sentence announcing it and nothing afterwards; the deliverable arrives as the delegation's `tool_result`. Every other `tool_result` is a tool's output, not the prompt's, and a file's contents must never become the answer.

**`PlanSubmitted` is set when a `tool_use` named `ExitPlanMode` appears**, whether or not its input decodes, and `PlanText` is its input's `plan`. The pair `PlanSubmitted` with an empty `PlanText` is the transport losing a plan, and is reported as such rather than as a prompt that never planned.

**`CostMeasured` is true whenever a `result` event arrived**, with `CostUSD` its `total_cost_usd` — including a prompt stopped at the cap, which is billed for everything it spent.

### Failures

| Observation | Reported as |
|---|---|
| The context was cancelled | `cancelled`, whatever else happened |
| The process could not be started | `start-error` |
| The stream ended without a `result` event | `no-result`, `Transient`, keeping any `session_id` the stream announced |
| The process failed having produced no session and no text | `exit-error`, `Transient` |
| `result` with `is_error` and subtype `error_max_budget_usd` | `cost-cap`, not transient |
| `rate_limit_event` with `status: "rejected"` | `account-exhausted`, `Transient` (§ The agent account) |
| `result` with `is_error`, any other subtype | `exit-error`, not transient |

Diagnostics combine the parse error, the exit status, and the harness's stderr, capped so one prompt cannot fill a report.

## Sessions

**A handle is resumed with `--resume <id>`. A fresh session is the absence of `--resume` and `--continue`** — which is the clean slate `FreshSession` asks for, so no argument spells it.

**A handle the harness no longer has is declined, and the prompt is sent once more without it.** `--resume` fails outright for a conversation that was pruned, recorded on another machine, or written under another project, and fails identically every time; re-offering it would turn a prompt that could have run into one that never can. The test is narrow: the prompt came back with **no session id, no text, and a `no-result` or `exit-error` failure** — no evidence it ever started. A prompt that announced its session and then went badly **happened**, and is never re-sent without its handle, because that would discard a live conversation over an unrelated fault. The re-sent prompt is not a declared new session ([resolution.md](resolution.md) § The agent session).

## The agent account

**The account is identified by `oauthAccount.accountUuid`**, and read for a person by `oauthAccount.emailAddress`, in the harness's account file: `$CLAUDE_CONFIG_DIR/.claude.json` when that variable is set, and `$HOME/.claude.json` otherwise. Nothing else is read for it. A file that is absent, or holds no `accountUuid`, leaves the account **unidentified** ([environment.md](environment.md) § The agent account): the e-mail is never promoted to an identifier, and no other field or file is tried.

**The credential is read from the one place the harness writes it on the host's operating system**, with no fallback between the two:

| Host | Location | Fields |
|---|---|---|
| macOS | The login Keychain item `Claude Code-credentials` | `claudeAiOauth.accessToken`; `claudeAiOauth.expiresAt`, Unix milliseconds |
| Every other host | `.credentials.json` — dot-prefixed — in `$CLAUDE_CONFIG_DIR` when set, else `$HOME/.claude` | The same |

An absent or zero `expiresAt` is unknown, not expired.

**Published usage is `GET <api base>/api/oauth/usage`**, authorised by that credential, answering `five_hour` and `seven_day` objects, each with `utilization` (percent, 0–100) and `resets_at` (RFC 3339). The API base is `https://api.anthropic.com`. Reading it spends nothing and sends no prompt; it is what the pre-dispatch check consults, and what a refusal falls back to only when the harness's own statement named no reset.

**The refusal is the `rate_limit_event`**, read where it arrives:

```json
{"type": "rate_limit_event",
 "rate_limit_info": {"status": "rejected",
                     "rateLimitType": "seven_day",
                     "resetsAt": 1789153635}}
```

`status: "rejected"` is the condition, `rateLimitType` the window, and `resetsAt` — Unix seconds — the instant it clears. The `result` event that follows a refusal is not the evidence: its `is_error` and `subtype` have been observed to disagree with each other for exactly this case, and an error subtype is not a discriminator ([environment.md](environment.md) § The agent account).

## Guards

**The action guard is a `PreToolUse` hook matching every tool**, whose command runs the guard binary — naming this harness to it, and blocking when the binary could not run ([agent.md](agent.md) § The action guard holds on every substrate) — and refuses the call by exiting 2, with the reason on stderr for the agent to read and adapt to. A `PostToolUse` hook matching every tool runs the same binary to record what happened, and never refuses.

**The tool vocabulary the guard reads** is this harness's: `Bash` is the `run tool` subject ([gates-and-commands.md](gates-and-commands.md) § Gates on what an agent does); `Edit`, `Write` and `NotebookEdit` are `edit file` subjects; `Task` and `Skill` hand work to something that makes tool calls of its own. A tool the guard does not recognise is not thereby allowed to publish or edit around it.

**The hook is wired in the harness's settings**, and what may be trusted to hold that wiring is [disclosure.md](disclosure.md) § The hook's guard must also come from outside the tree.

## Checking without spending

**`Doctor` runs `claude --version`** — the same binary, resolved the same way `Run` resolves it — and refuses a version below the minimum. No model is called. It drains the binary's stderr rather than inheriting it, and quotes it only when explaining a failure.

**The client refuses to spawn the real binary from a test process**, detected by the test binary's own flag registration rather than by importing `testing`, which would put test flags into every production binary. Tests substitute the spawn seam or use a stub `Agent`; there is no environment variable that lifts the refusal ([agent.md](agent.md) § Nothing mechanical may spend).

## Open questions

- **Which levels each model accepts, and `ultracode`.** The harness states that its effort levels depend on the model without publishing which, so § Request mapping's per-model sets are not yet established from the harness itself, and `Efforts` declares no level it has not verified. `ultracode` asks for `xhigh` with further agents orchestrated; whether it is declared depends on that work staying inside the prompt ([agent.md](agent.md) § Long-running work).
- **A relocated API base.** The usage endpoint's base is fixed above. Whether an install pointed elsewhere — a proxy, a gateway — publishes its usage at the base it was pointed at, and where that choice is stated for a reader to find, changes whether pacing reads the right account behind one.
