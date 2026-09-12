# The environment

> **Tag:** `environment` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** What makes a machine fit to be given an item, and what follows from a condition that makes it unfit. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it. Nothing here records progress, status, or history.

`resolution.md` defines what a gate is, and requires that infrastructure failures consume no budget. `gates-and-commands.md` defines the gate contract and names what a project supplies. This document defines the one gate whose subject is not the change, and the rules that follow from a subject that is not the change.

## What an environment condition is

**A property of the machine, of the delivered tree, or of a remote — never of the work.** Two questions decide it, and it is one only when both answer the same way:

- Does the same item succeed on another machine?
- Does a different item here fail identically?

Everything else a resolution can stop on is a property of the item: a budget cap, an unanswered question, a refused write, a failing suite. An environment condition is the one class where the item is incidental. It was in the way, and any item would have been.

**Both questions name a machine, and one condition is not keyed to one.** The allowance an agent account spends against is exhausted for every host spending it and for none of the accounts beside it, so "another machine" separates nothing: the same item fails on all of them under that account and succeeds on any of them under a different one. The questions still decide that the item is incidental — which is what puts it here — and the axis they vary is the payer rather than the host (§ The agent account).

**The test is deliberately not "is retrying free".** A machine with no disk left is not a transient failure — retrying costs exactly what the first attempt cost and produces exactly the same answer. Nor is it a deterministic refusal, because the same item on another machine succeeds immediately. A vocabulary that offers only those two has no place to put it, and whichever is chosen is wrong: *transient* sends the item back to the machine that cannot run it, and *refused* blames a change that is fine.

## The classification is two questions, not one

**What would clear it, and what it is a property of.** The second is what the first is derived from, and a classification that answers only the first cannot be acted on.

| Property of | Is | Whose problem |
|---|---|---|
| **The machine** | No space left where the worktree or the build writes; the agent cannot be invoked; anything the project knows its work requires and does not have | A person acting on the machine — or the machine recovering, which is not knowable in advance |
| **The delivered tree** | The gate entry point or the verify command absent or not executable; normative documentation absent | Whoever delivered the tree |
| **A remote** | The backend or a git remote unreachable | The remote returning |
| **The agent account** | The allowance the agent substrate spends against is exhausted | The window resetting, at an instant the substrate publishes |

Two of these owners are already the runner's — the host, and whoever declared the gate or delivered the tree — and they are kept apart here for the reason `gates-and-commands.md` gives there: collapsing them costs attribution even where it never costs a wrong retry.

**Retryability is derived, never declared alongside the condition.** A remote clears on its own; a machine does not, until someone acts on it or something on it finishes; a delivered tree never does. Reporting "transient" states the conclusion and discards what it was drawn from, which leaves the next reader unable to check it or to act.

## Nothing is concluded from a failure observed on an unfit machine

**A failure observed on an unfit machine is not a measurement of anything.** It does not resolve a step and it does not fail one. It does not reach an agent. It does not move a baseline, and it does not become a park on the item.

This is the whole of what the classification buys, and each clause forbids something that otherwise happens by default:

- **It does not reach an agent.** No prompt is rendered from it and no prompt is spent on it. This does not follow from *infrastructure failures consume no budget* (`resolution.md`): a prompt that is not charged against a cap is still a prompt that was paid for, and a loop that re-prompts an agent with a full disk's error text spends real money to be told the same thing again. Where a failure is handed back to an agent to work from, the environment is measured **before** the hand-back, not after it.
- **It does not fail a step.** A step that stopped because the machine did has not attempted anything, and reporting a failure against it makes the item look defective on every machine.
- **It does not move a baseline.** `gates-and-commands.md` already requires that a run which measured less than usual must not move one. A run on an unfit machine measured less than usual by definition.

## `fit` measures the machine

`resolution.md` names this gate already: *"Whether a machine is fit to be given work — before work is given."* It is the third row of the gate table there, and it is the only gate whose subject is not the code.

**It is `fit`, and the project provides it.** One of the required set in `gates-and-commands.md`, reached the same way every gate is: `bin/gate fit --envelope`, spawned by the runner, reported as one of the five outcomes, and judged by the project's judge against the project's thresholds.

**The project provides it because only the project knows what its work requires.** How much disk a build needs, which services a suite expects, what toolchain must be present — the SDK cannot know any of it, and a floor compiled into the SDK is a threshold held by the wrong party. This is the same boundary every other gate sits on: the gate measures, and something holding the thresholds decides.

**It divides into instances like any other concept.** `fit:disk`, `fit:toolchain`, `fit:services` — separately runnable, so a wait on one condition re-measures that condition rather than the whole set. The concept is closed and the instances are the project's, exactly as for `tested`.

**It does not appear among the concerns a change is measured against.** `formatted`, `builds`, `checked`, `tested` and `covered` are properties of the code, and a project reads that table to decide what its `integration` is made of. `fit` belongs in neither place: a machine that cannot build is not a change that may not land.

### A `fit` gate may come from the tree, because the tree is not what it judges

`gates-and-commands.md` permits a gate to be a tree artifact because what it reported is checkable against its subject afterwards, by anyone. A machine's free disk at an instant is not checkable afterwards, so that reason does not carry, and `fit` needs its own.

**It has one, and it is stronger: a `fit` gate does not judge the change.** The rule that keeps a judgement out of the reach of what it judges exists because a party under judgement can make itself pass. A `fit` gate reporting falsely buys the tree nothing — claiming *fit* when unfit starts work that then fails visibly, and claiming *unfit* stops work without landing anything. There is no version of lying that gets a change past a gate, which is the risk the rule was written against.

**What its subject not persisting does cost is the ability to settle a disagreement afterwards**, and the answer to that is the section below: the machine, unlike a process that has exited, is still there to be asked again.

## Unfit is a wait, not a verdict

**An environment condition is re-measured, never assumed to persist.** A machine is not a vanished process: it can be asked again, and the answer changes without anyone announcing it — a build elsewhere finishes, a log rotates, a person frees space. Work resumes when `fit` reports clear, and nothing about resuming requires a person to have said so.

This is what `resolution.md` § *Parking* requires of any advertised condition — that it stops being advertised the moment it ends. Re-measuring is how that is achieved for a condition nobody reports the end of.

**The wait is bounded, and exhausting the bound is not a verdict.** `gates-and-commands.md` states this for a queued gate and it holds identically here: exhausting a wait is *still unfit*, which is a condition to report, never a refusal to act on. What the bound protects is the claim — an item held indefinitely on a machine nobody is fixing is an item no other machine can take.

**Who waits is the drive model's, and neither model's answer belongs here.** Standalone drives its own lifecycle and holds; under an orchestrator the binary reports and the server decides, which `resolution-orchestrated.md` already assigns to it. What is required of both is that the condition is re-measured rather than concluded from once.

**Waiting holds the claim and touches nothing else.** `resolution.md` requires that a claim survives a step failing; an environment condition is not even that, so the item is left exactly as it was found — unparked, unmodified, and workable by another machine the moment this one lets go of it.

## The condition is reported, not recorded on the item

**It is reported as `blocked`**, which already means the run stopped and a person must act, and which already exits non-zero. What it carries is the measurement: the gate's own envelope, the verdict the judge returned, and the run the verdict was reached from.

**Nothing about the condition is written to the item.** `resolution.md` requires that every park names the step it belongs to, and that a park stops advertising its condition the moment the condition ends. A full disk satisfies neither — the step did not stop for a reason belonging to it, and the marker would travel with the item to a machine that has disk, where it is read as current and is false.

**The report names the condition, the measurement, and what would clear it.** `12 MB free on /srv/work/promise, floor 2 GB` is actionable; *verify failed* is not, and *infrastructure failure* is not either. A condition reported without the measurement that established it is one an operator has to reproduce before believing, on the machine that is already the problem.

## The agent account is a scope, and exhausting it parks with the instant it clears

**An account is not a machine.** One account spends across many hosts at once, and one host may drive several — `cli.md` § the agent account keeps it a separate axis for exactly that reason, because "paying for a run and being permitted to perform it are different questions". So an exhausted allowance stops every host spending that account, and stops nothing else on any of them.

**It is not measured by `fit`, and the scope is why.** `fit` measures the machine. A gate answering "is this machine fit" would have every host driving one account discover the same exhaustion separately and report a healthy machine as unfit, and it could not say *fit for that credential, not for this one* — which is the answer a host driving two accounts needs, because it must keep working the one that still has allowance.

> **The scopes are closed at four: the machine, the delivered tree, a remote, and the agent account. A condition scoped to the account holds on every host spending that account, and on nothing else running on any of them.**

**It parks, where the conditions above wait.** The machine, the tree and the remote are measured **before** a dispatch, so nothing has started and § *Unfit is a wait* leaves the item untouched. An allowance is different: it is discovered while the agent is working, the turn ends without a result, and the step must be dispatched again. That is a park in the sense `resolution.md` § *Parking* already defines — a step stopped without completing, and why — and the step resumes with its draft and the conversation it was having (`resolution.md` § The agent session).

**The kind is `account-exhausted`** ([orchestrator.md](orchestrator.md) § Vocabularies), and it is its own member of that closed set rather than a reading of an existing one. It is not `infra-transient`: nothing about the infrastructure failed, and an operator told to re-run "once the infrastructure is back" would be looking at healthy infrastructure for as long as the window lasts. Re-dispatch **does** clear it, so it classifies with the kinds that can — and it is the only one that also knows *when*, which is why the park carries the instant and the account it belongs to.

**The park records the instant it clears.** This is the one condition whose end is published rather than observed: a machine recovering "is not knowable in advance", a remote returns when it returns, but a window states when it resets. § *Unfit is a wait* requires re-measurement because nobody announces the end; here somebody does.

> **A condition that publishes its own end is parked with that end recorded, and waited out to it. Re-measuring toward a published instant, or backing off incrementally against it, spends invocations to rediscover an answer the system was already given.**

This is also how the park satisfies `resolution.md` § *Parking* — that a park stops advertising its condition the moment the condition ends. A recorded instant expires on its own, where a marker waiting to be cleared by an observer does not.

**The condition is established from the substrate's own statement that it refused, never inferred from how the turn died.** A substrate that rations its allowance says so in a dedicated signal, distinct from any result it reports afterwards, and that signal carries three facts: that the refusal happened, which window it belongs to, and the instant that window resets.

**The signal is read where it arrives, not reconstructed from the wreckage.** A turn refused for an exhausted allowance also ends badly — no result, or an error whose classification may contradict itself — and that ending is not the evidence. **An implementation that reads only a turn's outcome cannot classify this condition**, because the outcome no longer carries which window or when it resets; it will report an ordinary agent failure, bill the refusal, and count the dispatch, which is what every clause above forbids.

**Error taxonomies must not be the discriminator.** They vary between versions and are not required to distinguish an exhausted allowance from any other refusal. A classification keyed to them is wrong the first time one changes, silently, and in the direction that bills for it.

**Two moments read two sources, and neither is a fallback for the other.**

- **Before a dispatch there is no refusal to read**, so the account's published usage is the only source and is what the pre-check consults: a window already exhausted withholds the dispatch rather than spending a turn to be refused.
- **On a refusal the substrate's own statement is authoritative**, because it is what the substrate actually said about the request it actually refused. That statement is recorded as given — not replaced by a figure re-derived afterwards, which can disagree with it and is unavailable exactly when the account is in no state to be queried.

A published usage reading is the fallback only for the second moment, and only for a substrate that refuses without naming a reset.

**Nothing is concluded from the turn it interrupted.** § *Nothing is concluded from a failure observed on an unfit machine* applies clause for clause: it does not reach an agent, it does not fail a step, and it does not move a baseline. `resolution.md` requires that infrastructure failures consume no budget, and this is one — **the interrupted turn is not billed and its dispatch is not counted**, because an allowance that refused to spend has not bought an attempt.

**It is checked before work is given, as every member must be.** § *The set is closed* admits a condition only when it is both a check made before work is given and a classification made during it. It is both: the account's usage is read before a dispatch, and an exhausted window withholds the dispatch rather than spending a turn to be refused; the same reading classifies a turn the allowance interrupted. A host that has not yet dispatched must not learn this by spending.

> **The arena stays leased until the resolution resumes.** A wait on a published instant releases nothing: not the claim, not the worktree, not the draft. The lease ends when the work does, or when an operator ends it deliberately — never because a window is far away.

**The claim is held across the wait; the process is not.** `resolution.md` requires that a claim survive a step parking and a process exiting, and that holds here without exception — the item stays bound to the arena that parked it. What does not persist is the run: a window may be hours or days from resetting, and nothing is served by a process sitting in front of it. **Length of wait is not a reason to let go.** A lease dropped because the wait looked long is the one case where the system knows exactly how long it will be and gives up anyway.

**The arena holds the valuable state, which is why it keeps the lease.** A parked step resumes with its draft, and a draft lives where its own next dispatch finds it (`resolution.md` § Drafts) — on the arena that wrote it, together with whatever continuation the substrate offered and the worktree the work is already in. That arena is not idle: it is holding the most expensive thing the resolution owns. An item resumed elsewhere starts from nothing and pays again for reasoning this one still has, which `resolution.md` § *Nothing is bought twice* forbids: nothing already paid for is discarded while it is still needed. **An arena released while it still holds a resolution's state has not been freed; the work it was holding has been destroyed, and the next dispatch buys it again.**

So the run exits, the claim and its arena remain, and whatever returns at the recorded instant resumes there. An operator who would rather free the arena than keep the draft releases it explicitly, which is a decision, and `resolution.md` already requires that it be one.

**Reporting the condition is this SDK's whole half of it.** § *Unfit is a wait* already assigns the rest: who waits is the drive model's, and under an orchestrator the binary reports and the server decides. That division holds here unchanged, and it is what keeps the account scope out of this SDK's mechanics — **an account spans hosts, and nothing here coordinates hosts.** A run reports what it observed on the invocation it was asked to perform; whether every other host spending that account stops too, and how they come to know, is the deciding layer's, which is the only layer that can see them.

> **The condition and the instant it clears are reported outward on the invocation's result, in the vocabulary a caller that never links this SDK can read. A fact a driver must act on is not left to be inferred from a message written for a person.**

A caller that cannot read *this account is refused until that instant* from the result has to parse prose or re-derive the answer, and the drive model that was handed responsibility for the wait is exactly the one given nothing to act on.

**The account is identified by the stable identifier its substrate issues, and by nothing else.** A scope is only as good as the identity it is scoped to, and this one is compared across hosts by parties that never met — so it must be an identifier the substrate assigns to the account and changes for no other reason.

- **A display name is not an identity.** An e-mail address, an operator's alias, a rendered account name: each of them changes while the account does not, and each of them can be missing on a host where the account is perfectly usable. They may accompany the identifier so a person can read the report; they are never what a comparison is made on.
- **It is read from a stated field in a stated place, never discovered.** A reader that scans configuration for a key that looks like it might name an account, and takes the first field that looks like it might be one, returns a different answer on two hosts holding the same account — and the failure is silent, because each answer is individually plausible. The location and the field are named, and a reader that does not find them has not found the account.
- **It is never synthesized.** A reader that cannot establish the identity says so; it does not fall back to a host name, a configuration path, a token, or any other value that happens to be at hand. A synthesized identity is worse than none, because it compares equal to itself and will eventually compare equal to something else.

> **An unidentified account is not an identity and must never be used as one.** Two runs that cannot name the account they spend as are not thereby spending as the same account. A condition scoped to an account nobody could name is reported, and scoped to nothing.

**The report names the account, the window, and the instant.** § *The condition is reported* requires the condition, the measurement, and what would clear it. `agent account exhausted — 5h window, resets 18:42Z` is actionable; *agent failure* is the *infrastructure failure* that section already rejects as unactionable, and it is worse here, because the one fact a driver needs is the one such a report omits. The identifier is what the report is keyed by; the readable name is what it is read by, and a report carrying only the second cannot be acted on by anything but a person.

## Placement

Some items run only on some machines: work on a platform-specific fault needs the platform. That is not unfitness — a Linux arena refused a Windows item is a perfectly fit machine looking at somebody else's work — so nothing above applies to it: there is no wait, nothing to re-measure, and no condition to clear. It is a stable fact about the **pair**, and it is acted on where pairs are made: selection and placement.

> **The placement axes are closed: `os` and `arch`. An item carries restrictions on them; a machine reports facts about them; a pair qualifies when every restriction admits the machine's fact.**

- **The machine's side is measured, never declared** — the same rule as capabilities ([resolution.md](resolution.md) § Accounts, capabilities and roles): the facts are ambient constants of the arena, read where it runs.
- **The item's side is carried on the item**, recorded like any other item-level fact ([orchestrator.md](orchestrator.md)) — by whoever files it, by triage, or by a resolution that discovers it mid-run. No restriction on an axis means any value qualifies; several restrictions on one axis are alternatives.
- **A mismatched pair is never worked.** Selection does not offer the item to the arena, placement does not send it there, and a claim named explicitly is refused — typed, with an override for an operator who knows better. A resolution that discovers the restriction mid-run records it and stops as `blocked`, naming it; the journal is what lets a qualifying arena resume exactly where this one stopped, and nothing about the work is lost to the move.

The axes are closed the way every vocabulary here is: extending them is an SDK change, not a project's. What a **project's** machines must have — disk, toolchain, services — already has its seam in the `fit` gate and its thresholds; placement is for what varies **per item**, and it is closed at the platform.

## The set is closed, and one rule keeps it closed

**Every member is both a check made before work is given and a classification made during it.** A condition worth recognising after effort has been spent is worth refusing before it is spent; a condition not worth checking up front is not a member. Adding one means adding it in both places, and that requirement is what stops the set growing by accident into "things that went wrong".

The two points are not alternatives. The first avoids starting work that cannot finish. The second is the load-bearing one: a machine fit when the item started fills up during a step, and no up-front check can see that coming.

| Measured by | Members |
|---|---|
| The SDK | The backend is reachable; the agent can be invoked; the verify command and the gate entry point exist and are executable; normative documentation is present |
| The project's `fit` gate | Whatever the project knows its work requires — disk, toolchain, services |
| The agent substrate's published usage | Whether the account's allowance is exhausted, and when each window resets |

The SDK's members are `doctor`'s check set, which `cli.md` holds and which is closed there. **`fit` is the seam**: a project extends what fitness means through its own gate and its own thresholds, and never by adding a member to a set everyone else must understand.

**The third row is not a `doctor` check and must not become one.** `doctor` is a diagnosis of the worktree it is run from: the same worktree answers the same way on any machine, which is what makes its report something an operator can compare, quote and act on. An allowance is the opposite in both directions — it is **not in the worktree**, and it **changes while the worktree does not**. Putting it there would make `doctor` answer differently at two times from one tree, for a reason no reader of that tree could see, and every other check would inherit the doubt.

It is also not `fit`'s. `fit` measures the machine (§ `fit` measures the machine), and an account is not a property of a machine either — it is shared by every host spending it, so a host answering for it would be reporting a condition that is not its own.

`cli.md` bars it from a third direction: `doctor` spends nothing, and it names quota among the things "answered only by spending", so a report there must not imply it has established what a prompt would do. Reading published usage spends nothing and is therefore permitted — but it belongs where it can be acted on rather than where it would be quoted: before a dispatch, where an exhausted window withholds it, and on the turn an exhausted allowance interrupts.

**Memory is not a member**, and the reason is worth stating rather than discovering. A gate killed for memory is already `died` under the runner's account, owned by the host and named as such. There is no second mechanism to build, and adding one would give the same condition two names.
