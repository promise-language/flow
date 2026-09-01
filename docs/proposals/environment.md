# The environment

**Proposal.** A resolution against `promise` fails with `no space left on device`, and the flow spends agent turns asking the agent to fix it. This proposes the normative home for environment conditions, the mechanism that recognises one, and what to file.

## What happens today

`bin/verify` fails on a full disk. `stepImplement` reads that as a failing gate and re-prompts:

`issue/steps.go:224-241`

```go
verr := wt.Verify(ctx.Context())
if verr == nil { break }
if attempt >= rounds {
    return fmt.Errorf("verify still failing after %d fix attempts: %s", attempt, verifyTail(verr))
}
pc.VerifyOutput = verifyTail(verr)
prompt, err = renderPrompt(b.cfg, PromptImplementFix, pc)
```

The prompt it renders (`issue/prompt.go:263`) is:

> The verify command (`bin/verify`) is still failing. Its output ends: … Make `bin/verify` pass.

The tail ends in `No space left on device`. `DefaultMaxFixRounds` is 5, so one invocation is six agent turns against a condition no edit can change; `MaxInvocations` is 3, so the step spends eighteen before it stops. It then stops with `verify still failing after 5 fix attempts` — an error naming the change as the problem, charged as a full invocation, with the disk mentioned nowhere an operator reads.

`commitWithRepair` does the same thing one step later. `issue/steps.go:339-360`: any `wt.Stage` failure is read as a repository-guard refusal, so ENOSPC buys an agent turn rendered from `PromptStageRepair` — *delete the offending files* — and then parks `ParkRefused` with `staging refused twice`, naming a guard that never spoke.

## The cause is one thing

**Nothing in the SDK ever classifies an environment condition as one.** The vocabulary exists in full and is produced almost nowhere:

| Declared | Producers outside tests |
|---|---|
| `flow.ErrTransient` (`errs.go:40`) | **none** |
| `flow.ParkRemoteUnreachable` (`wire.go:30`) | **none** |
| `AgentFailure.Transient` (`agent.go:68`) | **none** — `claude/claude.go` leaves it false on all four failure paths |
| `flow.ErrRefused` (`errs.go:59`) | one: `commitWithRepair`'s second refusal |

So every environment condition reaches `cli/orchestrator.go` as a bare error, lands in `translateHandlerError`, and is charged. `issue/steps.go:353` even carries the comment `// infrastructure failure, NOT ErrRefused` — the distinction was seen, and the error is still returned bare.

## Against the normative documents

Three of these are already requirements. The code does not meet them.

| Document | What it requires | Status |
|---|---|---|
| `resolution.md:57` | "Infrastructure failures consume no budget. A step that could not run because the environment was unavailable has not spent an attempt." | violated by every path above |
| `resolution.md:61` | "a retry that cannot differ from the attempt before it is not a retry, it is a loop with a budget. It exhausts the grant, reports the cap as the reason, and names the wrong problem" | the fix loop is the exact instance |
| `resolution.md` § *Every outcome leads somewhere* | "every stopping outcome names what would clear it" | `verify still failing after 5 fix attempts` names nothing |
| `resolution-orchestrated.md:41` | "Health classification travels with the report… A failure reported without that distinction is one the server has to guess about." | no classification is ever produced |
| `cli.md:272` | "`doctor` … runs before work is trusted to a machine" | `cmdResolve` never calls it; nothing enforces the ordering |

One document anticipated this precise condition. `gates-and-commands.md:81`:

> A process killed at the declared timeout, killed for memory, or **truncated mid-write by a full disk** says nothing at all… Only the party that spawned the process can tell those apart.

That is right, and it does not reach here: it is about a **gate**, and `Verify` is a **command**. A gate is spawned by a runner that reports one of five outcomes, `died` among them. `Verify` has no runner, no outcome vocabulary, and no account of what became of the process — `pkg/backend/github/worktree.go:229-235` returns `fmt.Errorf("%s failed: %w\noutput:\n%s%s")` and the caller has the tool's prose and nothing else. **The party that spends the money is the one with no way to ask what became of the process.**

## What no document says: scope

The existing split is one-dimensional — *is retrying free* (`ErrTransient`) or *is retrying pointless* (`ErrRefused`). A full disk is neither. Retrying is not free, and it is not pointless either: the same item on another machine succeeds immediately.

Every `ParkKind` names a condition scoped to the **item**. A full disk is scoped to the **host**: every item on that machine fails identically, and this item elsewhere does not. The vocabulary cannot express that, and where the SDK needed it, it was smuggled into prose — `wire.go:26` explains that `ParkRemoteUnreachable` exists because the tracker "pauses dispatch GLOBALLY and runs a probe ticker", a scope no field carries.

That is the claim a new document makes and no existing one does:

> **A failure is classified by two questions, not one: what would clear it, and what it is a property of.**

It also decides where the condition may be recorded. `resolution.md` § *Parking*: "A park records that a step stopped without completing, and why. Every park names the step it belongs to," and "A park that advertises a condition stops advertising it the moment the condition ends." A full-disk park fails both — the step did not stop for a reason belonging to it, and the marker outlives the condition by travelling with the item to a machine that has disk. **An environment condition must not be recorded on the item at all.**

## Proposed: `docs/environment.md`

Named for the question `doctor` already asks — *is this environment fit to be given an item?* — and deliberately **not** "transient": `transient` is a taken term in this tree meaning *retry is free*, and the whole point of this condition is that it is not transient. A document named for it would enshrine the confusion it exists to remove.

It states only what the existing documents do not. `resolution.md` keeps line 57 and the outcome table; this document does not restate them.

1. **The test.** An environment condition is a property of the host, the worktree, or the remote — never of the work. The same item elsewhere succeeds; a different item here fails identically. That test is the definition, and it is what separates the condition from everything the existing vocabulary covers.

2. **Scope decides the remedy, and the two are the classification.** Three scopes, three people:

   | Scope | Example | Cleared by |
   |---|---|---|
   | **host** | no space on the worktree's filesystem; the agent cannot be invoked | a person acting on the machine |
   | **worktree** | `bin/gate` absent or not executable; `docs/` missing | whoever delivered the tree — `./make` |
   | **remote** | the backend or a git remote unreachable | the remote returning |

   Retryability is derived, not declared: a remote-scoped condition clears on its own, a host-scoped one does not, and reporting only "transient" loses which.

3. **An environment condition never reaches an agent.** No prompt is rendered from it and no turn is spent on it. This does not follow from *consumes no budget* — a turn that is not charged is still a turn that is paid for, and it is the whole of the observed failure. It has two enforcement points, and they are the only two places in the tree that re-prompt an agent from a failure: the verify-fix loop and `commitWithRepair`.

4. **It is not recorded on the item.** The run stops, the claim is kept, the item is left exactly as it was and is workable elsewhere unchanged. What is reported names the host and the remedy, not the step.

5. **One list, two consumption points.** Every member of the closed set is both a `doctor` check *and* a mid-item classification. A condition worth recognising after effort has been spent is one worth refusing before it is spent, and a condition not worth checking up front is not a member. This is what keeps the set from growing ad hoc, and it is why the set is closed.

6. **The set.** Backend unreachable · git remote unreachable · agent not invocable · no space on the worktree's filesystem · verify command or gate entry point absent or not executable · normative documentation absent.

   Out, with reason: **memory** — an OOM kill of a gate is already `died` under the runner's account, and there is no second mechanism to build. Adding a member requires the argument in point 5.

7. **The report is actionable.** The condition, its scope, the measurement that established it (`12 MB free on /home/djabi/prog/promise`), and what would clear it. A condition reported without its measurement is one an operator has to reproduce before believing.

## Proposed: the mechanism

### Probe the host; do not read the tool's prose

When an operation fails, ask the host whether it is still fit. If it is not, the failure is the host's, whatever the message said.

The alternative — matching `no space left on device` — needs the dialect of git, go, `gh`, `claude`, and every project's verify command in every language it is written in, and #4 is already open against exactly that shape of dependency: *"Lease refusals are untyped prose, so flow string-matches them — … one reworded message from breaking."* A `statfs` is a measurement; a string match is a guess about somebody else's wording.

One function serving both consumption points from one list:

```go
// flow.CheckEnvironment reports the conditions that make this environment
// unfit to be given an item. Empty means fit. Cheap enough to call on any
// failure path: a statfs and a stat, no subprocess.
func CheckEnvironment(ctx context.Context, cfg EnvironmentConfig) []Condition
```

Two honest limits, stated rather than discovered:

- **Detection is after the fact.** Space freed between the failure and the probe is a condition missed — and a missed reclassification is exactly today's behaviour, so it fails in the safe direction. `gates-and-commands.md:161` already states the same limit for the non-modification check.
- **A floor is a threshold, and thresholds are the project's.** "Zero bytes free" is not the condition — a verify needing 2 GB with 100 MB free fails identically. The probe reports free bytes; what counts as unfit is configuration, not a constant in the SDK.

### Three hook points

| Where | Why there |
|---|---|
| `cli/orchestrator.go` `runOne`, a fourth branch beside the `ErrTransient` / `ErrRefused` ones (`:254`, `:271`) — before `bumpInvocations` at `:277` | every handler in every flow, without touching a step |
| `issue/steps.go:224`, after `wt.Verify` fails and **before** the re-prompt | the loop never returns to the orchestrator between rounds; this is point 3 made concrete |
| `issue/steps.go:341`, before the stage-repair turn | the second of the two re-prompt sites |

### `doctor` runs before a resolution

`cmdResolve` runs the checks before claiming, rather than an operator remembering to. The free checks are a statfs and a few stats. The agent probe is the expensive one — `cli.md:283` requires it to be a real invocation — which is the one open question below.

## What to file

Following *fewer, larger issues*:

- **Extend #15** (`doctor` only probes the backend) with the filesystem-space check and the requirement that the checks run before a resolution rather than only on demand. #15 already owns the check set; this is two more entries and a wiring requirement.
- **One new issue**: *No environment condition is ever classified as one — three vocabularies are declared and nothing produces them.* Covers the ENOSPC re-prompt loop, the stage-repair turn, `claude` never setting `Transient`, and the code changes above. Filed against `docs/environment.md` once that lands.
- **#74** (`stepCloseBranch` charges an invocation for git-level failures) is the same cause seen at one step. Its remedy is a special case of the orchestrator hook point, and leaving both open invites two competing mechanisms. Recommend absorbing it into the new issue.

## Open for review

1. **The document's name.** `environment.md` recommended, for the reason above. Alternatives: `host.md` (precise about the commonest scope, wrong for the other two), `fitness.md` (echoes `doctor`'s question without naming its subject).
2. **How the condition is reported.** Recommended: no new `ParkKind`; the run reports the existing `blocked` status — which already means *stopped, a person must act, exit non-zero* — carrying a typed condition. `resolution-orchestrated.md:41` asks for the classification to travel with the *report*, which argues for a typed field on `InvocationResult` rather than a park. The alternative is `ParkEnvironmentUnfit`, which the tracker's ledger may prefer; it costs a new member of a closed set and records the condition on the item, which point 4 forbids.
3. **What `resolve` checks up front.** Free checks always; the agent probe is a real `claude` invocation. Either `resolve` pays it once before the first step of an item, or it stays `doctor`'s alone and `resolve` runs only the free checks.
4. **#74** — absorb, or keep open and cross-reference.
