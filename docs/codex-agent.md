# Codex agent

> **Tag:** `codex-agent` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how `flow/codex` binds the [`Agent`](agent.md) interface to the Codex CLI: how a prompt is sent, how the harness's output is read, where its account is found, and how the guards stand in front of it. Every statement is a requirement. What any substrate must provide is [agent.md](agent.md); this document states only how this one provides it.

## Invocation

**One prompt is one `codex exec` process.** The binary is `codex`, found on `PATH` unless the client names another. It is spawned in `AgentRequest.Worktree`, never through a shell, with:

| Argument | Carries |
|---|---|
| `exec --json` | Non-interactive operation, with one JSON event per line on stdout (§ The event stream) |
| `--cd <Worktree>` | The workspace root, the same directory the process is spawned in |
| `-c projects."<Worktree>".trust_level="trusted"` | The arena's project layer, so its committed hook wiring loads (§ Guards) |
| `--dangerously-bypass-hook-trust` | The committed hook runs without a per-checkout review nobody is present to give (§ Guards) |
| `--model <Model>` | `Model`, when non-empty |
| `-c model_reasoning_effort=<level>` | `Effort`, verbatim, when non-empty (§ Request mapping) |
| `--sandbox <mode> --ask-for-approval never` | `PermissionMode`, mapped (§ Request mapping) |
| `resume <ResumeSessionID>` | The handle, when the chokepoint stamped one (§ Sessions) |

**The prompt reaches the harness whole and unexposed**: never as a shell word, and never as an argument another process on the host can list. How the argument or stream carrying it is spelled is the client's; that it is not visible outside the process is the requirement.

**`--ask-for-approval never` on every prompt.** Nobody is present to approve anything, so an action the sandbox does not permit is refused to the agent at once, which adapts, rather than waiting on a person who will never answer.

**Never `--ephemeral`.** It stops the harness persisting the conversation, which makes every handle this client issues one no later prompt can resume — a session opened on every dispatch that nobody declared ([resolution.md](resolution.md) § The treasurer).

**Never `--skip-git-repo-check`.** The arena is a checkout; a harness that refuses to run outside one is reporting a fault in the arena, and hiding it moves the fault to the commit.

## Request mapping

**`PermissionMode` maps onto a sandbox that restricts at least as much** ([agent.md](agent.md) § Refuse, never substitute):

| `PermissionMode` | Harness settings | What the restriction is |
|---|---|---|
| `plan` | `--sandbox read-only` | Reads; modifies nothing |
| `default` | `--sandbox read-only` | Unattended, nothing is approved, so nothing beyond reading is done |
| `acceptEdits` | `--sandbox workspace-write` | Edits within the worktree |
| `auto` | `--sandbox workspace-write` | Autonomous within the worktree |
| `bypassPermissions` | — | **Refused** as `unsupported` |

**`bypassPermissions` is refused because this substrate's guard depends on a sandbox.** The hook runs without a per-checkout review (§ Guards), and what makes that safe is that no prompt can change the wiring it runs: the sandbox keeps `.codex/` read-only. A prompt with no sandbox could write a hook of its own into the worktree, and the next tool call would run it — unreviewed, and outside any restriction the step declared. Mapping the mode to a sandboxed one would be a substitution; refusing it is the rule.

**`plan` has no plan-submission tool here.** `PlanSubmitted` is always false and the plan is `LastText`. A `todo_list` item — the harness's running checklist — is progress reporting, not a submitted plan, and is never read as `PlanText`.

**`Effort` is carried verbatim to `model_reasoning_effort`, and `Efforts` declares what may be carried.** For each model, `Efforts(model)` returns the reasoning-effort values the harness accepts for that model, from least to most — drawn from `minimal`, `low`, `medium`, `high` and `xhigh` — and a level outside that set is refused at the chokepoint before anything is spawned ([agent.md](agent.md) § Effort levels). Nothing is mapped: a level named for another agent — Claude's `max`, say — is refused here, not rounded to this harness's highest.

**`MaxCostUSD` is not enforced.** The harness accepts no spend limit, so `flow/codex` implements `AgentLimits` answering `false`, and every run that grants a cost allowance through it announces that the allowance does not bound a prompt ([agent.md](agent.md) § MaxCostUSD contract).

## The event stream

The harness writes one JSON object per line. Lines that are not JSON are skipped.

| Event | Read for |
|---|---|
| `thread.started` | `thread_id` — the conversation this prompt is in, announced before any work |
| `item.completed`, item `type: "agent_message"` | `text` — the latest non-blank one is kept |
| `item.started` / `item.completed`, any other item type | The tool the agent used, added to `ToolsUsed` once per name: the item type (`command_execution`, `file_change`, `web_search`), and for `mcp_tool_call` the name `mcp__<server>__<tool>` |
| `turn.completed` | The end of the prompt; `usage` — token counts |
| `turn.failed` | The end of a prompt that failed; `error.message` |
| `error` | A stream-level fault; `message` |

**A successful parse requires `turn.completed`.** A stream that ends without it, or without `turn.failed`, has produced no prompt, whatever text it carried.

**`LastText` is the last `agent_message` the prompt produced.** Reasoning items are the agent's deliberation, not its answer, and are never read as text; command output is a tool's output, and a file's contents must never become the answer.

**`CostMeasured` is false on every response.** The harness reports tokens, not a price, and on a subscription there is no price per prompt to report. `CostUSD` is zero and meaningless beside it; the unmeasured cost is never read as none ([agent.md](agent.md) § A cost that was not measured is not zero).

**`DurationSeconds` is measured by the client**, from spawn to exit.

### Failures

| Observation | Reported as |
|---|---|
| The context was cancelled | `cancelled`, whatever else happened |
| The request carried `bypassPermissions`, or anything else this document refuses | `unsupported`, before anything is spawned |
| The process could not be started | `start-error` |
| The stream ended without `turn.completed` or `turn.failed` | `no-result`, `Transient`, keeping any `thread_id` the stream announced |
| The process failed having produced no thread and no text | `exit-error`, `Transient` |
| The harness's own statement that the account refused | `account-exhausted`, `Transient` (§ The agent account) |
| `turn.failed`, or `error`, for any other reason | `exit-error`, not transient |

Diagnostics combine the parse error, the exit status, the failure message and the harness's stderr, capped so one prompt cannot fill a report.

## Sessions

**The handle is `thread_id`. A handle is resumed with `codex exec resume <thread_id>`; a fresh session is `codex exec` without `resume`**, which is the clean slate `FreshSession` asks for.

**A resumed prompt runs under the request's own settings** — its sandbox, its approval policy, its effort, and the project trust and hook arguments that keep the guard in front of it. The handle is an optimisation ([resolution.md](resolution.md) § The agent session); where a resume cannot be given every argument in § Invocation, the handle is declined and the prompt runs fresh with them. A conversation continued with less restriction, or with no guard, is a substitution, whatever it saves.

**A handle the harness no longer has is declined, and the prompt is sent once more without it** — on the same narrow test [claude-agent.md](claude-agent.md) § Sessions applies, because it is the same question: the prompt came back with **no thread id, no text, and a `no-result` or `exit-error` failure**, no evidence it ever started. A prompt that announced its thread and then went badly happened, and is never re-sent without its handle.

## The agent account

**The harness's home is `$CODEX_HOME` when set, and `$HOME/.codex` otherwise.** Every file below is read from there.

**The account is identified by `tokens.account_id` in `auth.json`.** Nothing else is read for it. A file that is absent, or holds no `tokens.account_id` — as under API-key authentication, where there is no account to name — leaves the account **unidentified** ([environment.md](environment.md) § The agent account). No other field is promoted to an identifier, and none is synthesized.

**The refusal is classified from the harness's own statement that the allowance refused**, carrying the window and the instant it resets, read where it arrives in the stream — never from a `turn.failed` message's wording, which is prose for a person and changes between releases ([environment.md](environment.md) § The agent account).

## Guards

**The guard is the workspace's, and the wiring that reaches it is the project's committed `.codex/hooks.json`** — provisioned by the workspace and committed by the project, exactly as the Claude wiring is ([claude-agent.md](claude-agent.md) § Guards). The binary is built into the checkout's `bin/` by setup; nothing in the project defines what it refuses.

**The wiring fails closed before setup.** A fresh checkout has no built guard, so the committed hook finds nothing to run — and that must block every tool call rather than let it through, because a guard that is absent until someone builds it is absent on every arena that has not. The hook command exits 2, the harness's blocking code, whenever the guard binary could not run; only the guard's own decision allows a call.

**A `PreToolUse` hook matching every tool refuses; a `PostToolUse` hook matching every tool only observes.** The refusal is exit 2 with the reason on stderr, for the agent to read and adapt to. The observing hook stays silent when the binary could not run, since the tool has already run and a failure there blocks nothing.

**The command names this harness to the guard, and pins its own interpreter.** It passes the substrate's name, so the guard reads the payload in this harness's vocabulary ([agent.md](agent.md) § The action guard holds on every substrate); and it runs under a named shell of its own, since this harness has no setting that fixes one, so the fail-closed spelling means the same thing whether the harness hands the command to a shell or splits it into arguments.

**Which hooks exist is stated once per project, not once per harness.** The events wired, and whether each hook gates or only observes, are the project's hook policy; the Codex wiring carries the same policy as the project's other wiring, and differs only in how it reaches the guard.

**The hook finds the project from the checkout it runs in.** This harness sets no project-directory variable; the command locates the checkout root from the directory the hook runs in, and the guard reads the project from the payload's `cwd`. A root that cannot be found leaves no binary to run, which blocks.

**The invocation loads the wiring and skips the review — and both are safe only together with the sandbox.** An untrusted project's `.codex/` layer is ignored, hooks included, and an arena is a checkout at a path nobody has trusted; a hook that is loaded still runs only once a person has reviewed its exact definition, and nobody is present to. So each prompt trusts the arena's project layer and bypasses the per-hook review (§ Invocation). That grants nothing a prompt can exploit, because **no prompt can change what it grants**: the wiring is reviewed, committed content, provisioned identically by every setup; the sandbox keeps `.codex/` read-only under every mode this substrate accepts; and the one mode without a sandbox is refused (§ Request mapping). Remove any of those and the bypass runs whatever an agent wrote.

**The tool vocabulary the guard reads** is this harness's, and it differs from Claude's in the one place it matters:

| `tool_name` | Subject | What the guard reads |
|---|---|---|
| `Bash` | `run tool` ([gates-and-commands.md](gates-and-commands.md) § Gates on what an agent does) — every shell call, however the harness executes it | `tool_input.command` |
| `apply_patch` | `edit file`, for **every path the patch touches** | `tool_input.command` — the patch text, whose `*** Add File:`, `*** Update File:`, `*** Delete File:` and `*** Move to:` lines name the paths |
| `mcp__<server>__<tool>` | Whatever the tool does; publishing through one is publishing | `tool_input` |

**An edit here is a patch, and a patch names its paths inside itself.** A guard that reads a path argument finds none, and treats an edit to anything as if nothing had been proposed. One patch may touch several files; the call is refused if any of them is.

**Project instructions are not step protocol.** The harness reads `AGENTS.md` by convention; no step depends on its having done so ([agent.md](agent.md) § The prompt carries everything the result depends on).

## Checking without spending

**`Doctor` runs `codex --version`** — the same binary, resolved the same way `Run` resolves it — and refuses a version below the minimum. **It also runs `codex login status`**, which exits 0 only when the harness is authenticated and calls no model, so a host with no credential fails here rather than on its first prompt. Neither spends. Both drain stderr rather than inheriting it, and quote it only when explaining a failure.

**The client refuses to spawn the real binary from a test process**, on the same detection and for the same reason as [claude-agent.md](claude-agent.md) § Checking without spending.

## Open questions

- **The minimum version.** The oldest release on which every obligation above holds — `exec --json` with this event vocabulary, `PreToolUse` on `apply_patch`, project trust honoured from `-c`, `--dangerously-bypass-hook-trust`, and a resume that accepts every argument in § Invocation — is not established. It is what `Doctor` enforces, and until it is named `Doctor` can refuse nothing.
- **Which effort levels each model accepts.** § Request mapping draws the levels from the harness's reasoning-effort scale; which of them each model accepts — and whether the scale itself is the harness's documented one rather than a reading of it — is not established, and `Efforts` declares no level it has not verified.
- **How a hook command runs under `exec`.** § Guards requires the command to block whenever the guard could not run. That depends on whether the harness runs the command through a shell, in which directory, and what it does with an exit code other than 2 — each of which decides whether the fail-closed spelling blocks or is silently skipped.
- **Whether a resume accepts every argument.** § Sessions declines the handle otherwise; whether `exec resume` honours the sandbox, trust and hook arguments decides whether this substrate resumes at all.
- **The refusal signal and published usage.** § The agent account requires the harness's own statement with the window and its reset. Whether `exec --json` carries one, what its event is called, and whether the account's usage is published anywhere readable without a prompt decide whether this substrate can be paced before a dispatch or classify a refusal at all; until they are known, an exhausted allowance is indistinguishable from an ordinary failure, which [environment.md](environment.md) forbids.
- **Delegated work.** Whether a prompt that hands its work to a subagent reports the subagent's output in the stream, and as what, decides whether `LastText` can hold the preamble of a delegation — the failure [claude-agent.md](claude-agent.md) § The event stream closes for that harness.
