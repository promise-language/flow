# Agent integration

> **Tag:** `agent` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines the agent interface, the metered chokepoint, permission modes, effort levels, failure kinds, and the lifecycle of a prompt — where it runs, what contains it, how it ends, and what it may leave behind. Every statement is a requirement. This is the normative home for the per-step permission requirement described in [#19](https://github.com/promise-language/flow/issues/19).

## The interface

```go
type Agent interface {
    Name() string
    Efforts(model string) []EffortDef
    Run(ctx context.Context, req AgentRequest) (*AgentResponse, error)
}
```

Concrete implementations live in subpackages (the reference is `flow/claude`). The root package carries zero transitive dependencies on any agent substrate. **The process handling behind every implementation is this library's**, both ends of a prompt run on another host included (§ Where a prompt runs).

> **A prompt is one `Agent.Run` call.**

The request goes out, the agent works — reading, editing, calling tools, as many model calls as the task takes — and the call returns once with what it produced. This is the unit every statement about agent work is counted in: what the treasurer approves, what `MaxCostUSD` bounds, what the ledger records, and what a step is said to spend. Nothing smaller has an identity here, because a model call inside the run is the substrate's business and no caller can address one. Nothing larger exists either: a step that prompts twice made two calls, and they are two of everything the first one was one of.

**"While the agent works" is the span inside one prompt**: after the request goes out, before the call returns. It is when the agent reads and edits, when tool calls happen, when a guard refuses an action and the agent adapts to the refusal instead of losing what it has done, and when a gate has nothing to say yet because the result does not exist. Wherever this corpus says a thing happens while an agent works, it means inside that span — one dispatch, one prompt, no journal entry yet, and the step's result not written until the call comes back.

**The word is `prompt`, and it is not `turn`.** A turn in this corpus is a position in a queue — whose move it is, an exclusion arriving inside a running dispatch ([resolution.md](resolution.md) § The treasurer) — and a single word carrying both senses left every reader deciding which was meant. `Prompts` is also what a step declares when it says whether it may do this at all ([flow-registration.md](flow-registration.md) § Step configuration).

## The chokepoint

Within a resolution, `ctx.Agent()` is the **only** route to spend on agent work. Every expense is metered here, and every expense is approved by the treasurer before it is incurred — allowed, blocked, or priced with an allowance ([resolution.md](resolution.md) § The treasurer). There is no second path. A step that needs agent work calls `ctx.Agent().Run(...)` and nothing else.

The chokepoint also **stamps the request's `Worktree`** with the arena's checkout (`Orchestrator.ArenaRoot()`), overwriting whatever the step put there. Where the agent edits is not a step's choice: it is the tree the commit will be taken in and the gates will measure, and a request that names no directory inherits the directory the binary was started in.

It stamps **the session** for the same reason. `ResumeSessionID` and `FreshSession` are set here from the resolution's own session ([resolution.md](resolution.md) § The agent session), overwriting whatever the step put there: which conversation a prompt continues is a property of the resolution, and a step choosing its own would be choosing what the resolution is having. A prompt resumes the handle the resolution is holding; where there is none it spawns a clean slate, which is also why the empty handle is stamped as `FreshSession` rather than left blank — an empty `ResumeSessionID` alone lets a substrate attach to whatever it last cached, and for an entry step that is another item's reasoning. A step that needs a new conversation declares `Session: fresh` ([flow-registration.md](flow-registration.md) § Session continuity), and the machinery does the rest.

It stamps **what the prompt serves** — `Item` and `Step` — because a prompt that cannot name its item and step cannot be observed, refused in favour of, or collected for by anything that did not start it (§ The lifecycle of a prompt). And **every dispatch collects any ending the arena's previous prompt left uncollected, before it dispatches anything**, so the session and cost of a call nobody received reach the resolution first (§ The requester disappearing).

## Nothing mechanical may spend

A prompt is sent **only where somebody asked for work**: a step of resolving an item, against a budget, producing an artifact. Every other path answers by reading, or does not answer.

A mechanical command — `doctor`, `list`, `status` — must never spend. It runs before every item, in CI, and on every machine an operator touches, so a prompt on one of those paths is a standing charge nobody asked for. And the charge is not the worst of it: a preflight that bills the account is one an operator turns off, and a preflight nobody runs prevents nothing. `doctor` carried exactly such a prompt — one tool-free probe, capped at fifty cents, on every run, forever.

This is enforced in three places, because the mistake arrives in three shapes:

| Enforcement | Catches |
|---|---|
| The commit gate's approved list (`tools/build/common/agentturns.go`) | A new call site. The list is exact — file, function, and how many prompts each asks for — and **adding an entry is the maintainer's decision**. Removing one when the call goes away is ordinary upkeep. |
| `App.Agent` refuses `Run` (`cli`) | A prompt requested at runtime from outside a step dispatch, in a binary built from a tree that never passed the gate. The field answers `Name()` and `Efforts()` — declarations, which spend nothing — and nothing else; the real agent is reached only by the dispatch that builds the metered chokepoint. |
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

**It also establishes that the machine can contain a prompt** (§ Containment), by establishing the unit around a process that is not the agent, which spends nothing. A platform on which containment cannot be guaranteed is one on which the agent cannot be invoked, and like every such condition it is found before work is given ([environment.md](environment.md) § The set is closed, and one rule keeps it closed), not by the first prompt's `start-error`.

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
| `Effort` | `EffortLevel` | How hard the agent works on this prompt: one of the levels **this agent** declares for the requested `Model`, or empty for the agent's own default (§ Effort levels). |
| `MaxCostUSD` | `float64` | Ceiling on what this prompt may spend, in USD. Zero means unbounded. |
| `Worktree` | `string` | Working directory for the agent process. **Set by the SDK at the chokepoint, never by the step**: the arena's checkout (`Orchestrator.ArenaRoot()`), the same tree the commit is taken in and the gates measure. In the remote form the prompt host runs the agent in its own arena's checkout, whatever this names (§ Where a prompt runs). |
| `ResumeSessionID` | `string` | Non-empty resumes that exact session. Empty means "don't actively resume a specific session." **Set by the SDK at the chokepoint, never by the step**: the handle the resolution is holding ([resolution.md](resolution.md) § The agent session). Offered and never depended on — a substrate may decline it, and a step whose result depends on its being honoured has made an optimisation load-bearing. |
| `FreshSession` | `bool` | Discard any inherited session state — spawn from a clean slate. **Set by the SDK at the chokepoint, never by the step**: true exactly when the resolution holds no handle, so a prompt with nothing to resume cannot attach to whatever the substrate cached. |
| `Item` | `ItemRef` | The item the prompt serves. **Set by the SDK at the chokepoint, never by the step**, so a live prompt, a refusal naming it, and its ending all say what it served (§ Observing a prompt). |
| `Step` | `StepId` | The step the prompt serves. **Set by the SDK at the chokepoint, never by the step**, for the same reason. |

### MaxCostUSD contract

`MaxCostUSD` is the allowance the treasurer priced for this expense. An implementation that can enforce it passes it to the substrate and reports a stop as `AgentFailure{Kind: FailureCostCap}`. The bound is not exact: the substrate learns what a model call cost only after it returns, so the prompt stops at the **first call that crosses the cap**. The overrun is bounded by one model call, not by a whole prompt — that is the difference this axis provides.

**The bound is the prompt's, across every resumption** (§ Long-running work). Each resumption is given only what the prompt has left, and a prompt with nothing left is not resumed: it ends `cost-cap`.

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

## Effort levels

> **Effort is the agent's own vocabulary, closed per agent and model.** Every `Agent` declares the levels it accepts for each model, and a request names one of them or none.

```go
type EffortLevel string

type EffortDef struct {
    Level       EffortLevel
    Description string
}
```

**There is no scale shared across agents, because substrates do not share one.** Their levels differ in number, in name, and in kind: one substrate's highest is not another's, and a level may combine more reasoning with a way of working — orchestrating further agents, say — that no other substrate offers at all. A shared scale would map each substrate's levels onto names that mean something different on each, drop the levels that map onto nothing, and grow toward the union of every substrate's set, which none of them fully supports.

**The set is closed, declared, and ordered.** `Efforts(model)` returns every level the agent accepts for that model — the empty model standing for the agent's default one — from least effort to most, each with a one-line description for docs, reports and refusals; the description is not load-bearing. The empty level is never among them and always means the agent's own default. The set is the implementation's, and it declares only levels its supported substrate versions accept: its minimum version (§ Checking the agent without spending) covers every level it declares.

**Levels differ by model, so the set is keyed by it.** A substrate's newer models accept levels its older ones do not, and a level valid for the agent is still not valid for a model that does not accept it. An agent whose levels are the same for every model answers the same set whatever it is asked.

**A level outside the set for the requested model is refused at the chokepoint, before anything is sent.** It is carried verbatim — never mapped, and never rounded to the nearest level the agent does have: a step that asked for one level and was silently given another has a result that depends on a substitution nobody chose. The refusal parks `refused` and charges nothing, because the same request is refused identically every time — the treatment a prompt from a step that declared it would not prompt already gets ([flow-registration.md](flow-registration.md) § Step configuration).

**A flow names the levels of the agent it runs with.** Running it on a different agent means naming that agent's levels; nothing translates between two agents' vocabularies on its behalf.

**Every level is a prompt like any other.** Whatever a level makes the agent do — reason longer, delegate, orchestrate further agents — happens inside the prompt: contained, metered into its cost, bounded by `MaxCostUSD` and the deadline, and ended by the ending sequence (§ The lifecycle of a prompt). A level whose work the seam cannot hold the prompt open for is not declared (§ Long-running work).

## AgentResponse

`AgentResponse` is the aggregated result of one `Agent.Run` call — the whole prompt, however many times the seam held it open for its agent's work (§ Long-running work):

| Field | Type | Meaning |
|---|---|---|
| `LastText` | `string` | The **last** assistant text block the agent produced — not every text block joined. An agent that ends on a tool call emits preamble before each one, and concatenating those produces an artifact made entirely of narration. |
| `PlanText` | `string` | What the agent submitted through the plan-submission tool. Empty when the agent did not end that way. |
| `PlanSubmitted` | `bool` | Whether the agent called the plan-submission tool. The pair `(PlanSubmitted=true, PlanText="")` is the case worth failing on: the agent produced a plan and the transport lost it. |
| `ToolsUsed` | `[]string` | Tools the agent invoked. |
| `CostUSD` | `float64` | Cost of this prompt, all of it — on an ending without a result, what the substrate had reported by then. |
| `DurationSeconds` | `float64` | Wall-clock time. |
| `SessionID` | `string` | The conversation this prompt ran in. **Reported whether the prompt finished or died mid-way**: a substrate that named its session opened one, and that conversation holds everything the dead prompt paid for — a prompt killed by a deadline or a broken stream that reported no handle is bought again on the resume ([resolution.md](resolution.md) § Nothing is bought twice). The SDK records it as the resolution's and offers it back as `Request.ResumeSessionID` at the chokepoint; no step chains it itself. Empty where the substrate has no such notion. A call that never returned is no exception: its ending is collected by the next dispatch in the arena, session included (§ The requester disappearing). |
| `Prompt` | `PromptRecord` | The prompt as it ended: where it ran, the process that ran it, what it had started, and which ending it was (§ Observing a prompt). Present on every ending in which a prompt started. |
| `Failure` | `*AgentFailure` | Nil on success; non-nil carries structured failure info. |

## AgentFailure

`AgentFailure` carries structured failure info when `Failure` is non-nil:

| Field | Type | Meaning |
|---|---|---|
| `Kind` | `string` | One of the kinds below. |
| `Transient` | `bool` | The attempt is not charged — see below. |
| `Message` | `string` | Human-readable detail. |
| `Window` | `string` | Which of the account's allowance windows refused, in the substrate's own vocabulary. Set on `account-exhausted` alone; empty when the substrate refused without naming one. |
| `ClearsAt` | `*time.Time` | The instant the refusing window resets, **as the substrate published it** — never re-derived afterwards, which can disagree with it and is unavailable exactly when the account is in no state to be queried. Set on `account-exhausted` alone. A pointer, so absent reads as *no instant* and never as *soon*. |
| `Live` | `*PromptRecord` | The live prompt a `prompt-live` refusal names: its item and step, its host and process, and when it started (§ Observing a prompt). Set on `prompt-live` alone. |

### Failure kinds

The `Kind` field is drawn from a closed set. Each kind is one way a prompt ended without answering cleanly, and **the kind decides what follows**: whether the attempt is charged, and what the step becomes. The treatment is a function of the kind, stated here once; no implementation and no host chooses it.

| Kind | The prompt | Transient | What follows |
|---|---|---|---|
| `start-error` | Could not start: the agent process could not be spawned, or its containment could not be established (§ Containment). Nothing ran. | no | **Not a park, and nothing is charged.** The agent cannot be invoked here — a condition of the machine ([environment.md](environment.md) § The classification is two questions, not one) — reported `blocked`. |
| `unreachable` | Remote form only: the host could not be reached before the prompt started. Nothing ran. | yes | `remote-unreachable` |
| `exit-error` | The agent process exited non-zero without a result, and nothing it was awaiting was still running. | yes | `infra-transient` |
| `no-result` | The agent process exited zero without a result, and nothing it was awaiting was still running. | yes | `infra-transient` |
| `killed` | Ended by a signal the seam did not send: out of memory, a kill from outside the seam, or the death of the process that runs the prompt, which takes the unit with it (§ Containment). | yes | `infra-transient` |
| `connection-lost` | Remote form only: the connection between client and host was lost mid-prompt, and the host ended the prompt (§ The requester disappearing). | yes | `infra-transient` |
| `host-restarted` | The host restarted mid-prompt and the prompt went with it; stated on the prompt record (§ The requester disappearing). | yes | `infra-transient` |
| `error-result` | The substrate finished the prompt and marked its result an error. | no | An ordinary failure of the step: the dispatch is counted. |
| `cost-cap` | The substrate stopped the prompt because it reached `MaxCostUSD`, or the seam did not resume it because nothing of it was left (§ MaxCostUSD contract). | no | `treasurer-refused`, on cost. |
| `timed-out` | The context's deadline passed, and the seam ended the prompt (§ Timeout and cancellation). | no | `treasurer-refused`, on time. |
| `cancelled` | The seam ended the prompt on an instruction: the context was cancelled, or an operator ended it through the prompt host (§ Observing a prompt). | no | On the requester's own cancel the invocation is being interrupted, and nothing parks: the item stays where its journal left it. On an operator's end, `blocked`: a person stopped it, and a person decides what runs next. |
| `left-running` | Finished — its agent answered with nothing outstanding — while processes of the prompt that nothing awaited were still alive. The seam ended them with the prompt, and the failure names each (§ Descendants and sub-agents). | no | An ordinary failure of the step: the dispatch is counted. |
| `unmetered-agent` | An agent process the substrate did not run within this prompt was found among its processes, and the seam ended the prompt (§ Descendants and sub-agents). | no | An ordinary failure of the step: the dispatch is counted. |
| `prompt-live` | Refused: the arena already has a live prompt, which the failure names (§ One prompt per arena). Nothing ran. | no | **Not a park, and nothing is charged.** The step stops naming the live prompt, reported `blocked`. |
| `account-exhausted` | The substrate **refused** the prompt because the agent account's allowance is spent ([environment.md](environment.md) § The agent account). | yes | `account-exhausted` |

### Transient failures

When `Transient` is true, the attempt is not charged: the treasurer does not count the dispatch, and the metered chokepoint bills none of the prompt's reported cost. One flag, because it is one treatment — a flapping runner must not spend the resolution's budget ([environment.md](environment.md)), and neither may an allowance that refused to spend, which has not bought an attempt. **Two kinds charge nothing without being transient**, `start-error` and `prompt-live`: nothing ran, so there is no attempt to charge, and neither is a park, so there is nothing a re-dispatch is waiting to clear.

**`account-exhausted` parks `account-exhausted`, not `infra-transient`** — nothing about the infrastructure failed, and an operator told to re-run *once the infrastructure is back* would be looking at healthy infrastructure for as long as the window lasts. The park carries `ClearsAt` and the account it belongs to.

**`Transient` follows from the kind, never from a host's own reading.** An implementation reports which ending it observed; the flag and the treatment are the table's. A host that decided transience for itself — a refusal it did not recognise, reported as a passing infrastructure failure — would park a condition no re-dispatch clears as one any re-dispatch might, and name nothing that would clear it.

### Reporting an exhausted account

An implementation whose substrate rations its allowance **must establish the condition from the substrate's own statement that it refused, and never from how the prompt died.** A refused prompt also ends badly — without a result, or with an error whose classification may contradict itself — and that ending carries neither which window refused nor when it returns. An implementation that reads only the prompt's outcome will report an ordinary agent failure, bill the refusal, and count the dispatch, which is what every clause above forbids. Error taxonomies must not be the discriminator: they vary between versions, and a classification keyed to them is wrong the first time one changes, silently, and in the direction that bills for it.

The reference implementation reads the substrate's dedicated rate-limit event, which carries all three facts — that the refusal happened, which window, and the reset instant — and sets `Kind`, `Transient`, `Window` and `ClearsAt` from it.

## The lifecycle of a prompt

`Run` is one call, and behind it is a process — usually a tree of them. This section is what happens to that tree: where it runs, what contains it, how it ends, what it may leave behind, and what can be seen of it meanwhile. **It is defined once, here, and implemented once, in this library**, because each rule is a property of every prompt and so can only be held where every prompt passes ([resolution.md](resolution.md) § One seam per outside service).

> **Every prompt, anywhere, is launched by this implementation.** A flow's step reaches it through `ctx.Agent()`; an orchestrator's runner embeds its prompt host; any other project that runs a prompt links it. Nothing spawns the agent substrate by a path of its own.

That is the seam rule carried past this repository ([resolution.md](resolution.md) § One seam per outside service), and the lifecycle is where it earns the most. Containment, the ending sequence and the classification of endings are the parts most easily got almost right. A consumer running its own copy is left holding every defect this section has since corrected, with nothing to tell it so. A consumer that needs a prompt launched differently files that against this library.

### Where a prompt runs

A prompt runs on a **host**: the machine whose process runs the agent, in the arena's checkout. It takes one of two forms.

| Form | The host | What runs the agent process |
|---|---|---|
| **Local** | The machine the requester — the process that called `Run` — runs on | The `Agent` implementation itself |
| **Remote** | A machine the orchestrator dispatches to | The **prompt host** on that machine. The requester holds a **client** `Agent` that forwards `Run` to it |

> **Both ends of the remote form are this library's.** The client `Agent` and the prompt host are implemented here; a runner embeds the prompt host, and an orchestrator supplies the connection between the two. Neither handles an agent process, and neither classifies how a prompt ended.

A host that wrote its own spawning, containment and refusal handling would be a second route to the substrate — one per host implementation — and every rule in this section would hold only where some implementation happened to hold it ([orchestrator.md](orchestrator.md) § What an orchestrator is).

**The protocol between the two ends is the library's too**, versioned with it, so the ends cannot disagree about what a message means. It carries the start, with the request and the deadline; what the prompt reports as it runs — the session once named, the cost as reported, processes as they start; a cancel; the ending; and the two reads below, of a live prompt and of an ending nobody collected.

**The checkout is the host's.** In the remote form the prompt host runs the agent, and every command it runs for the agent, in its own arena's checkout. A `Worktree` naming a path on the requester's machine names nothing there, and the host does not read it.

Everything else in this section holds in both forms. Where the remote form adds something, it says so.

### Containment

> **A prompt's agent process, and every process that process starts, are contained in one kill-scoped unit — on every supported platform, from before the agent process runs anything until the prompt has ended.**

The unit is established **before** the agent process executes, so nothing it starts can predate it. It holds descendants at any depth: shells, builds, test runs, servers, and further agent processes.

> **No process a prompt started survives the prompt. Not on any platform, not by any route out of the unit, and not through any way the process running the prompt can die.**

This is a guarantee the implementation delivers, not a best effort it reports on. **Whatever it takes on a platform is what the implementation does** — the platform's own containment, a privilege boundary, a namespace, an isolation the prompt cannot reach out of — and which mechanism that is, is the implementation's business. That nothing is left behind is not: no lingering process, still working in the background, in the checkout or anywhere else.

**A descendant that tries to leave stays in.** A new session, a detached daemon, a service manager asked to start something, a process re-parented away from the tree: none of these takes a process out of the prompt, because the unit is one a process cannot leave. A mechanism a descendant can step out of — a process group alone, a marker it can strip — is not containment and does not satisfy this.

**The seam still confirms it at every ending**: the unit is observed empty before `Run` returns (§ The ending sequence). A survivor found there is ended at once, and it is a defect in the seam, reported as one — never a tolerated outcome of a platform's limits.

> **The unit dies with the process that runs the prompt.** When that process dies — however it dies, a kill it cannot catch included — the platform ends the whole unit: the agent process and every descendant, at once.

The process that runs the prompt is the requester in the local form and the prompt host in the remote form. The binding is made when the unit is established, and it is the platform's to carry out rather than that process's: a death the process never sees is exactly the one it cannot act on, so nothing about that death may depend on it. Whatever the implementation must do on a platform to make that true, it does; the next run in the arena confirms it (§ The requester disappearing), as the ending confirms the unit empty.

**Containment that cannot be guaranteed is a prompt that does not start.** The ending is `start-error`, naming what could not be established. The binding is part of containment, so a prompt whose unit could outlive the process running it does not start either — and **a platform on which the implementation cannot guarantee this section is not a platform prompts run on**, rather than one where they run with the guarantee relaxed. An uncontained prompt is not a degraded one: the process tree is exactly what a deadline and a cancel have to reach, and a prompt nothing can end is the failure this section exists to prevent.

### How a prompt ends

> **`Run` returns only once the prompt has ended on the host that ran it: the agent process and everything it started are gone. A prompt never outlives the call that asked for it, and the process that made the call does not exit before it returns.**

**The seam decides that a prompt is finished, from what it observes — never from end-of-stream, and never from the substrate having answered.** A prompt is finished when its agent has answered and nothing the agent is awaiting is still running (§ Long-running work). The seam then ends it, and **process exit, not end-of-stream, confirms the ending**. The two come apart in both directions: a descendant holding the agent's output streams keeps them open after the agent exits, so a reader waiting for end-of-stream waits on the descendant; and a stream can close while the process is still working. Once the prompt is finished and its agent process has exited, the ending sequence below ends the rest of the unit, which closes whatever the descendants held.

**Every ending reports what the prompt had established by then**: the session, once the substrate named one; the cost the substrate had reported; and the prompt record, carrying which ending it was, how the agent stopped, the agent process's exit status or signal, and what else was alive at the end (§ Observing a prompt). The exit status and signal are raw diagnostics, as a gate's exit code is ([gates-and-commands.md](gates-and-commands.md) § What the runner reports): the kind is what anything decides on.

#### The ending sequence

Whatever ends a prompt — a deadline, a cancel, a requester gone, an operator, the host shutting down — the seam ends it the same way, in this order:

1. **Stop is requested.** The agent process is sent the platform's request to terminate — a signal it can catch, never one it cannot. It is asked, not killed, because an agent that is asked can do what a killed one cannot: stop its tools, and write its session to the substrate, so the conversation the prompt paid for is there to resume.
2. **A grace passes.** It ends the moment the agent process exits, and at the latest when the seam's grace period runs out. The grace period is the seam's own constant, long enough for the substrate to write its session: never the step's, never a request field, and never extended by anything the prompt does or prints.
3. **What is left is killed.** The whole unit — the agent process if it is still running, and every descendant whether or not the agent exited — is killed, with the signal nothing can catch. Nothing that belongs to the prompt is left to decide for itself whether to stop.
4. **The seam observes the unit empty**, and then removes what a killed process left holding the checkout (§ Descendants and sub-agents).
5. **`Run` returns**, and not before.

**Stopping is not the kill, and neither is the context ending.** The context's end is the seam's cue to begin the sequence; a process killed outright at the deadline loses whatever the substrate had not yet written, and that is exactly the session the next dispatch would otherwise resume ([resolution.md](resolution.md) § Nothing is bought twice). **And returning is not ending.** A `Run` that returned at the deadline while its process was still alive would leave an agent editing the checkout after its step had parked — work nothing will read, in the tree the next dispatch starts from — which is why step 5 comes last.

The ending records **how the agent stopped**: `stopped`, when it exited within the grace, or `killed`, when step 3 had to end it — so a session missing its last exchanges can be told apart from one that was written whole. A finished prompt is ended the same way, except that step 1 may be the substrate's own orderly close of a finished session in place of the signal. Where the prompt is finished and its agent process has already exited, there is nothing to stop: the sequence runs from step 3 against whatever is left alive, which is what `left-running` reports.

### Timeout and cancellation

The step's time allowance reaches the prompt as the context `Run` is given ([step-handler.md](step-handler.md) § Other context).

> **When the deadline passes, the seam runs the ending sequence: stop requested, the grace, what is left killed, the unit observed empty. `Run` returns after that, and reports `timed-out`.**

- **The deadline starts the sequence; it does not include it.** The allowance bounds the agent's work, and the grace is shutdown, not work — so the grace falls after the deadline rather than being carved out of it, and `Run` returns no later than the deadline plus the grace period and the time to observe the unit empty. A grace taken out of the allowance would shorten every step by it, to cover the prompts that run to the end.
- **What is reported** is the session the substrate named — written whole when the agent `stopped` within the grace — the cost it had reported, how the agent stopped, and what the unit held when step 3 killed it (§ Observing a prompt). The park is `treasurer-refused` on time (§ Failure kinds), and the next dispatch resumes the session.
- **A cancel runs the same sequence** and reports `cancelled`.

**In the remote form the deadline crosses with the start, as an absolute instant, and the host enforces it itself** — beginning the ending sequence at that instant whether or not it hears from the requester again, and reporting the ending when it completes. A cancel crosses as a message and starts the same sequence on the host. A host that learned a deadline had passed only by being told would work on whenever the telling failed, and the requester that gave up is exactly the party least able to say so.

**Progress does not extend the deadline, and silence does not shorten it.** Nothing but the deadline ends a prompt for taking long. There is no silence bound: a tool call that runs for forty minutes printing nothing is, from outside, indistinguishable from a wedged one, and elapsed silence is evidence about the clock rather than about the process ([resolution.md](resolution.md) § A claim binds worktrees, not processes). **A hang is ended by the deadline, as `timed-out`, and by nothing else** — which is also why a long, silent tool call is never mistaken for one.

### Descendants and sub-agents

A **descendant** is any process a prompt's agent process starts, directly or through another. A **sub-agent** is a further agent the prompt delegates work to through its own tools — run by the substrate inside the agent process, or as a separate agent process.

- **Containment.** Every descendant is in the prompt's unit, a separate agent process included (§ Containment).
- **Lifetime.** Nothing a prompt started outlives it. Work the agent is awaiting — a command the seam is running for it, or a delegation — holds the prompt open until it finishes (§ Long-running work). Anything of the prompt's still alive once the prompt has finished is a survivor: it is ended with the unit, and **a prompt that left a survivor has not finished cleanly** — it ends `left-running`, naming each, however complete its answer. On a crash, a cancel or a deadline the unit ends them with the agent, and the prompt record names what was alive.
- **Propagation.** Ending the unit is what reaches every descendant; nothing is relied on to pass a cancel along. Once the unit is observed empty, nothing of the prompt holds the checkout, the output streams or a lock — and a lock a killed process left behind in the checkout, the version-control index lock being the ordinary one, is removed at the ending: at most one prompt runs per arena (§ One prompt per arena), so its holder is known to be gone.
- **Cost and budget.** A sub-agent's spend is the prompt's spend: reported in the prompt's cost, bounded by the same `MaxCostUSD`, and approved with the prompt by the treasurer. **The only sub-agents a prompt may have are those the substrate runs within it and meters into its cost.** An agent process started any other way — one a command started, rather than the agent process itself — reaches the substrate outside the metered prompt, so it is refused: by the action guard as it is attempted, and, because a guard fails open ([resolution.md](resolution.md) § Guards), by the seam, which ends a prompt found holding one as `unmetered-agent`.
- **Sessions.** A sub-agent's work enters the prompt's conversation as the result it delegated back. The resolution's session holds the parent conversation with each delegation's result in it, not a sub-agent's own transcript, and a resume continues the parent. A delegation cut off by the ending is not resumed — its result never reached the conversation — and what it changed is in the tree; the resumed parent re-issues what it still needs.
- **Concurrency.** Sub-agents running in parallel inside one prompt are that one prompt for § One prompt per arena. They share its checkout under the same guard, and the step's worktree contract judges what they leave together ([resolution.md](resolution.md) § Steps and the worktree).
- **Failure attribution.** A sub-agent that failed, hung until the deadline, or was ended with the prompt is recorded in the prompt record by the call that delegated to it, with its outcome. **Its output is never taken as the prompt's answer**, and a prompt ended while one was still running reports that ending, never an answer.
- **Observability.** The live tree — every descendant and sub-agent — is part of the prompt record (§ Observing a prompt).

### Long-running work

Work inside a prompt can take longer than a substrate lets one tool call run — a full build and test run takes most of an hour on some projects — and that is ordinary work, not an edge case. It is also work nothing outside the call can arrange around: an agent repairing a broken trunk runs the verify command itself, when it judges it needs to, and no ordering of steps and prompts around `Run` can prevent that. **So long work is handled entirely inside `Run`, and the caller neither arranges it nor can tell how it was done.**

> **A prompt is not finished while work its agent started is still running, and only the deadline bounds that work.**

**The seam runs the agent's commands, not the agent's process.** Every command the agent runs through its tools is executed by the seam's own runner, inside the prompt's unit, and the agent's tool call is only a client of that run — whether the substrate's shell tool is routed to the runner or the seam's own tool is offered in its place. So nothing the substrate does to its tool call reaches the command: a per-call timeout, a call moved to the background, a shell killed when the session ends. The command runs until it finishes or the deadline ends it, and its exit status and output are kept by the seam, not by the substrate.

**Work the agent is awaiting holds the prompt open.** That is every command the seam is running for the agent, and every delegation the substrate is running within the prompt. While any of it runs the prompt is not over — even where the agent has answered, and even where its process has exited. The seam waits for it, and for the agent process to exit, then **resumes the prompt's session** with what it produced — for a command, its exit status, the end of its output, and where the whole output is kept — and the agent continues from there. This repeats until the agent answers with nothing outstanding, and that last answer is the prompt's answer. Resuming the session is the ordinary way a conversation continues (§ The chokepoint), and the only agent work it adds is the agent reading the result it was waiting for.

**A resumption depends on its session, where a dispatch does not.** The handle a dispatch is offered may be declined without making the dispatch wrong (§ AgentRequest). A resumption continues the conversation this prompt is itself having, and the result it delivers means nothing outside it. So a resumption is never sent without its session: one the substrate cannot continue ends the prompt as the resumed process's exit reports it — `no-result` or `exit-error`, both transient — and the step's next dispatch starts from its prompt and its draft, with the command's work already in the tree.

**It is one prompt throughout**: one `Run`, one approval, one `MaxCostUSD` across every resumption, one deadline, and one response whose cost is the whole of it (§ The interface). How many times the seam resumed it is the seam's business, as a model call inside the prompt is the substrate's.

**A delegation runs in the agent process, or in a process the agent process started, so the seam sees it outstanding as that process still running.** No limit of the substrate's own may end a delegation or lose its result before the deadline — a wait ceiling on background work, an idle cap. The seam sets each such limit to the deadline or removes it, and a substrate that cannot be configured so, or that would end its process with a delegation still outstanding, does not offer delegation to a prompt.

**It is all mechanical.** The seam knows what is outstanding because it can see it running in the unit — the commands it started, and the process a delegation runs in (§ Containment) — not because the substrate said so. A substrate's own notices — a post-tool hook, a completion event, a task notification — are evidence an implementation may use, never what the guarantee rests on: a substrate may fire one when a call is moved aside rather than when its command finishes, or skip it when a session ends first. Nothing here depends on how a particular substrate version schedules its tools, and none of it is established by spending: it is exercised against a substituted agent process, never by running the real one (§ Nothing mechanical may spend). For the same reason nothing that must hold about an action waits for such a notice. A check on what the agent does stands before the act, where a guard stands ([resolution.md](resolution.md) § Guards), and a check on what the act produced is made by the seam or the step, from the tree.

**A tool that starts work the seam neither runs nor can wait for is not offered.** A watch that reports later, or a background mechanism of the substrate's own that the seam cannot see through to its end: the seam withholds it, and the action guard refuses it wherever it is reached anyway.

**Work the agent is not awaiting is not held for.** A process a command leaves behind once the command itself has exited — a daemon, a server started with `&` and forgotten — is no command the seam is running for the agent, so it keeps no prompt open. When the prompt finishes it is a survivor, ended with the unit and named, as `left-running` (§ Descendants and sub-agents). Both, not either: ending it keeps the arena clean, and naming it tells the next prompt what its predecessor assumed would keep running. An awaited command that never ends — a server run as a tool call of its own — holds the prompt until the deadline ends it, as `timed-out`, and the record names it.

**Nothing outlives the prompt.** Holding a prompt open is how its work finishes inside it. The seam provides no way for work to continue after `Run` returns and resume the session later — that would be a prompt outliving its call under another name.

**A long, silent command is not a hang** (§ Timeout and cancellation): silence bounds nothing, and no idle limit of a substrate's may stand in for one.

### The requester disappearing

A requester can go without cancelling: its binary crashes or is killed, the machine it runs on restarts, or its connection drops.

> **A prompt whose requester is gone is ended as soon as its host observes the requester gone** — by the ending sequence wherever anything is left to run it, and by the platform, with the unit, where nothing is.

- **Local form.** The requester is the seam, and **a requester that is stopping is not gone.** Interrupted, told to terminate, or failing, it runs the whole ending sequence against its live prompt — the grace waited out, what is left killed, the unit observed empty — and only then exits. There is no shortcut for a hurried exit, and no process is left behind to finish the job: the one running the prompt finishes it. **Only a death the requester cannot intercept** — an uncatchable kill, or the machine lost — leaves the sequence unrun, and **the prompt dies with it anyway**: the unit is bound to the requester's process (§ Containment), so the platform ends the agent and every descendant at the moment the requester dies. That is a kill without the grace, since nobody is left to give one, and the agent's last exchanges may be missing from its session. The next run registering in the arena then, before it starts anything, confirms that nothing of the prompt its predecessor left recorded survives — ending any survivor, and reporting it as the defect in the seam it is (§ Containment) — and states its ending: `killed` for a requester that died, `host-restarted` for a machine that did. A registration whose holder is gone is released ([resolution.md](resolution.md) § A claim binds worktrees, not processes), and what that holder started is already gone with it.
- **Remote form.** The host observes the requester through the connection: its closing, or the transport's own liveness check failing — across a network the only observation there is, made by the transport's protocol and never read from the prompt's silence. **A lost connection is the call ending**; the host does not hold a prompt open for a requester that might come back. The deadline the host enforces bounds the prompt whatever the connection does. **The client does not return on the loss either**, because `Run` returns only once the prompt has ended on the host (§ How a prompt ends): it reads the ending from the prompt record once the host answers again, and where the host stays unreachable it returns `connection-lost` no earlier than the deadline plus the grace period — by which time the host, which ends a prompt on losing its requester and at the deadline regardless, has ended it.

**What survives the requester is the prompt record.** The host writes it durably when the prompt starts and keeps it current — the session the moment the substrate names it, the cost as it is reported — so no ending can lose either. An ending nobody received leaves the record behind with the ending stated: `connection-lost` for a lost connection, and `host-restarted` for a prompt the host finds on restarting with no ending recorded, once it has confirmed nothing of that prompt survives.

> **The next dispatch in the arena collects an uncollected ending before it dispatches anything.**

It records the session as the resolution's, charges the ending as § Failure kinds says for its kind — an answer nobody received included, whose cost is billed like any answer's — and discards the record. It parks nothing on it and completes nothing from it: the dispatch collecting it runs the pending step as it would have, now holding the session that conversation is in. That is how `SessionID`'s rule holds for a call that never returned: the conversation reaches the resolution anyway, and nothing the prompt paid for is bought again ([resolution.md](resolution.md) § Nothing is bought twice). A record naming an item the arena no longer holds is discarded when the arena claims another, its session going the way a released claim's does ([resolution.md](resolution.md) § The agent session).

### One prompt per arena

> **At most one prompt runs per arena.**

An arena has one checkout, and two prompts editing it are two advancing processes in one worktree by another door ([resolution.md](resolution.md) § A claim binds worktrees, not processes).

**A second `Run` while one is live is refused, never joined.** The refusal is `prompt-live`, and it carries the live prompt's record as `AgentFailure.Live`: its item and step, its host and process, and when it started. A join would give one prompt two callers, each believing the answer is its own.

**The refusal is not transient and not `infra-transient`.** Nothing about the infrastructure failed, and a re-dispatch meets the same live prompt. Nothing ran, so nothing is charged and nothing parks: the step stops naming the live prompt, and a person can see what is running, for which item and step, and since when.

**What clears it is the live prompt ending** — at its own deadline, on its own requester's cancel, when its host observes its requester gone, or when an operator ends it through the host. Under the rules above a prompt nobody is waiting on does not stay live, so there is nothing for an override to unlock.

### Observing a prompt

A prompt's **prompt record** can be read without acquiring anything, in both forms — from the arena in the local form, from the prompt host in the remote form — and it is what an ending carries as `AgentResponse.Prompt`.

| Field | Holds |
|---|---|
| Identity | The prompt's own identifier, unique on its host. |
| Arena | The `(HostId, ArenaId)` the prompt runs in. |
| Item, step | The `ItemRef` and `StepId` stamped on the request. |
| Process | The agent process's identifier **and the moment it started** — an identifier alone is reused, and after a restart names nothing. |
| Tree | Every live descendant — its identifier, start time, parent and command line — and every sub-agent, by the call that delegated to it; each marked as what it is — an ordinary process, an agent process, or a delegation — and whether the agent is awaiting it. |
| Session, cost | As last reported. |
| Ending | Absent while the prompt is live. Once it has ended, `answered` or the failure kind; how the agent stopped, `stopped` or `killed`, where the seam had to stop it (§ The ending sequence); and the exit status or signal beside them. |

**Every ending states which it was**: `answered`, or one of the failure kinds — never nothing. A record with no ending is a live prompt, or one whose host has not yet observed it gone.

**An operator can end a live prompt.** In the remote form the prompt host accepts the instruction, runs the ending sequence, and reports `cancelled`, which parks `blocked` (§ Failure kinds). In the local form there is no host apart from the requester, and ending the prompt is interrupting the requester, which is its own cancel.

**A prompt record is never published.** It names paths, command lines and a conversation, and like a draft it is the machinery's scaffolding rather than a result ([resolution.md](resolution.md) § Drafts).

## Cross-references

- [step-handler.md](step-handler.md) — `ctx.Agent()` is how handlers reach the agent.
- [resolution.md](resolution.md) — the treasurer and park semantics.
- [flow-registration.md](flow-registration.md) — step declaration.
- [orchestrator.md](orchestrator.md) — the orchestrator supplies a remote prompt's connection and nothing else.
- [environment.md](environment.md) — what a start that cannot happen says about the machine.
- [gates-and-commands.md](gates-and-commands.md) — the gate runner, whose account of a vanished process this section's endings parallel.
