# The issue flow

> **Tag:** `issue-flow` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** The step graph this repository ships for resolving issues, and what each step must do.

`docs/resolution.md` defines the process, boundaries and responsibilities common to any resolution. `docs/resolution-standalone.md` defines the model this flow runs under. This document defines **these steps and these routes** — a different flow may hold every property in those documents with a different graph.

## Roles

The graph declares two roles ([resolution.md](resolution.md) § Accounts, capabilities and roles):

| Role | Requires | Performs |
|---|---|---|
| **contributor** | push | Producing and proposing a change, or filing the items that resolve the issue |
| **maintainer** | push, merge | Judging a proposal: integrating it, returning it for rework, or rejecting it |

One principal covering both roles, on an account that backs both, crosses the boundary without a handoff — that is carry-through ([resolution.md](resolution.md) § One principal, several roles). Two principals are the split: the contributor's part ends at the proposal, and the item awaits the maintainer.

## The graph

Each step produces exactly one result, and the result is the step's identity. **Routes to** is the declared successor set — the whole route space; which route an execution elects is decided at runtime, with the reasons in its message.

| Step | Role | Concern | Writes | Routes to |
|---|---|---|---|---|
| plan | contributor | What will be done, before anything changes — and **which shape the work is** | the `plan` — nothing in the worktree | open branch · review the filing |
| **open branch** | contributor | Put the worktree on a branch for this item | the branch, and a record of what it was cut from | implement |
| implement | contributor | Make it work | the solution, a commit, the `implementation` record naming it | review |
| review | contributor | Make it right | the solution, a commit, the `review` briefing | coverage |
| coverage | contributor | Make it tested | tests **and** the solution, a commit, the `coverage` briefing | open request |
| **open request** | contributor | Propose a change that can land | the gate's result, the pushed branch, the request, the `pr-open` signal | close branch · repair disclosure *(refused push)* |
| repair disclosure | contributor | Make what the branch carries publishable | the rewritten history, the `disclosure-repair` record | open request |
| **close branch** | contributor | Return the worktree to the base | nothing — it restores | review the proposal |
| review the proposal | maintainer | Judge the proposal as what will land | the `proposal-review` briefing — nothing in the worktree | verify merge result · implement *(rework)* · **finalize: rejected** |
| **verify merge result** | maintainer | Measure the merge result | the gate's result | merge |
| **merge** | maintainer | Land the change | the merge, the `pr-merged` signal | record merge commit |
| **record merge commit** | maintainer | Name what landed | the `merge-commit` record | **finalize: resolved** |
| review the filing | contributor | Do the intended items close the gap | the `filing-review` briefing — nothing in the worktree | file the items · plan |
| **file the items** | contributor | File what the plan decided | the filed items on the backend, the `filed-items` list naming them — nothing in the worktree | **finalize: resolved** · plan *(refused text)* |

Steps in **bold** are mechanical — no agent prompt. Implement, review and coverage are the producing phase; the properties governing producing steps are in `resolution-standalone.md` and are not repeated here.

### The work has a shape, and plan elects it

The plan step's deliverable is a decision about the work — and part of that decision is **which shape the work is**, because the shape is the route:

- **A change.** The issue is resolved by changing the tree: branch, produce, propose. The route through open branch.
- **A filing.** The issue is resolved by filing items — a reconciliation pass after a document amendment, a set of follow-ups to record — and the deliverable is the filed items themselves. The route through review the filing.

The shapes differ in what the artifact is and in what the worktree contract means. On the filing route no step touches the tree, and that is by declaration, not by exception: a filing forced through the change route arrives at steps whose contract expects commits, where the actual deliverable — items filed on the backend — reads as a violation and the resolution stops on work done correctly. The route is how the same flow holds both kinds of work to the right contract.

Filing writes are **outward writes**: every item filed passes the disclosure guard ([disclosure.md](disclosure.md)) like anything else that leaves the machine.

### Verify is a tool, not a step

There is no verification step, and that is deliberate.

The verify command is **something a producing step uses while working** — it formats, it checks, it tells the agent whether what it has written holds together. A step runs it as often as it finds useful, which is why implement's loop is built around it. It is an instrument, and instruments do not get their own place in a graph.

Making it a step would put a checkpoint where there is no decision: by the time the producing phase ends, every step has been using the verify command throughout, and a further run establishes nothing that the last producing step did not already know.

**The gate is a different thing, and it runs twice.** It measures whether the mainline stays green, and it modifies nothing. `resolution.md` has the full distinction; the short form is that verify repairs and the gate decides.

It runs **before the request is opened** and again **at integration**, and the two measure different things:

| When | Question | If it fails |
|---|---|---|
| Before proposing | Does this branch, exactly as it stands, pass? | Do not propose |
| At integration | Does the **merge result** pass — the mainline having moved since? | Do not land |

Neither replaces the other. Skipping the first proposes a change the maintainer's own gate will reject, which spends a reviewer's attention on something that was never going to land and returns the item for a round trip that the contributor could have avoided. Skipping the second lands a change that was green against a mainline that no longer exists.

The first is what makes the request honest: **what is proposed has been measured, by the same gate the maintainer will run.**

### Planning can conclude that there is no plan

The plan step answers three questions, and only the first has a plan as its answer: *should this be done*, *what shape is it*, and *how*.

Some items should not be done, and discovering that is the plan step working rather than failing. It is also the cheapest possible moment to discover it — before a branch exists, before an agent has written anything, and before anyone has reviewed a change that should not have been made.

So the step's deliverable is a plan **or** a refusal, and a refusal names which of these it is:

| Refusal | Means | Evidence it carries | What a person does |
|---|---|---|---|
| **already done** | The change exists, or the desired state already holds | Where it is already true | Close the item |
| **duplicate** | The work is pending under another item that replaces this one | The item that covers it | Redirect to that item |
| **conflicts** | The item asks for something the normative documents forbid | The document and what it says | Change the item, or the document |
| **not viable** | The work cannot be done as asked | The specific reason | Rethink or close |

**Already done and duplicate are not the same finding.** A duplicate means the work is pending somewhere else and will happen there. Already done means there is no pending work anywhere — the item asks for something that is already true, whether because somebody did it or because it never needed doing. They differ in what a person does next, which is why they are separate.

**Every refusal carries evidence a person can check.** That is what makes it a finding rather than a shrug: a refusal naming the duplicate item, or the line where the behaviour already exists, or the document it contradicts, can be confirmed or overturned in a minute. One that says only "this cannot be done" is indistinguishable from an agent that gave up, and a reader has no way to tell which it was.

A refusal **blocks the resolution on a named reason.** It is not a route and not a dead end: the flow stops, the reason says what would unblock it, and when a person acts on that reason the resolution continues from where it stopped. An item found to duplicate another is usually closed instead — but if the finding was wrong, clearing the block resumes the work rather than starting it again.

The set is closed. A refusal fitting none of these means the vocabulary is wrong, not that a fifth may be invented in prose.

**Waiting on items is not a refusal.** A plan that finds the work pending under other items — behaviour this item needs that is being built elsewhere, or a change that cannot be made until something else has landed — declares those items as blockers and stops as blocked on them ([resolution.md](resolution.md) § Blocked on items). It is not `duplicate`: a duplicate says this item is redundant and a person redirects, where a blocked item is still this item's work, waiting. And it is not a fifth refusal, because nobody decides anything: the blockers land, and the plan runs again from where it stood with nobody having acted. The line is who acts — a refusal means no change will help and a person decides; blocked on items means the work exists elsewhere and will arrive.

**Not knowing enough to plan is different and is not a refusal.** An item that could be planned given an answer asks the question and parks, which is a resumable state — the plan step will run again with the answer in hand. Refusing means no answer would help.

A step that stops this way **keeps as much of its work as it can**, and continues from it when it resumes. How much survives depends on what the work was: implement's changes are in the worktree, so a run cut off mid-step — by a park, a crash, or a power failure — leaves them there to resume from. The plan step has no such durable half-product, so its draft is where one goes. The question exists because of that work: the step read enough to find the ambiguity, and discarding it means re-deriving the same reasoning to arrive at the same question, now answered. The plan step is where this matters most, because it changes no files — a park with nothing kept erases the step entirely.

What it keeps is a draft, not a result: it completes nothing, only the step that wrote it reads it, and it is discarded once the step completes. `resolution.md` § "Drafts" states the general rule.

### A step's write contract is checked, not merely stated

The **Writes** column is a contract, and every step is held to it after it runs.

Prevention comes first — each step's declaration is a layer of the action guard, so a plan dispatch has a file write refused as it is attempted: the union of the general rules, the contributor role's restrictions, and plan's own layer ([resolution.md](resolution.md) § Guards), refusing a forbidden action before it runs and letting the agent adapt while it works rather than losing the whole prompt.

But prevention is enforced **by the agent**, not by this flow, which passes a configuration and trusts the outcome. A shell, a tool that shells out, or a mode that does not apply cleanly goes straight through it. So prevention is worth having, is the cheapest place to catch a violation, and is not the guarantee.

**The two layers are a guard and a gate**, in the senses `resolution.md` defines. Neither substitutes for the other, and the reason is what each can and cannot see: a guard refuses an action before it happens and is bypassed by any route that does not pass through it, while a gate measures the result however it came about and cannot speak until the agent has finished.

**The guarantee is the check afterwards.** Each step records the branch, the commit, and the tree state before its agent prompt, and verifies against them after:

| Violation | Means |
|---|---|
| The branch moved | The agent switched or created a branch |
| The commit moved | The agent committed |
| The tree is dirty where the step writes nothing | The agent edited what it was not there to edit |

A violation **blocks the resolution and names what happened.** It is not a failure to retry: the same prompt against the same state will very likely do the same thing, so retrying spends a prompt to arrive back here.

**The changes are not discarded.** Reverting would restore the invariant by destroying work nobody has seen — the same silent loss this flow spends three steps preventing elsewhere. What an agent did outside its contract may be worthless or may be the most valuable thing in the run, and the flow is not in a position to tell. It stops, leaves the evidence in place, and says precisely what was violated so a person can look at it and decide.

One check covers all three because they are one question: **did this step do only what it said it would.**

### The branching sequence is declared

Every step declares the worktree state it needs and the state it leaves ([resolution.md](resolution.md) § Steps and the worktree), so the resolution's whole branching story is written down here, not improvised route by route — committed and clean at every boundary, which is the commit contract's guarantee:

| Step | Needs | Leaves |
|---|---|---|
| plan · review the proposal · review the filing · file the items · merge · record merge commit | `any` | `as-found` |
| open branch | `any` | `item-branch` |
| implement · review · coverage · open request · repair disclosure | `item-branch` | `item-branch` |
| verify merge result | `item-branch` | `as-found` |
| close branch | `item-branch` | `base` |

Implement, review and coverage refuse to run anywhere but the item's branch, and complete only with their changes committed there — work cannot end on the base, half-committed, or split across a stray branch, because no step's declared boundary accepts that state.

### Branches are moved only by mechanical steps

**Open branch, open request and close branch are the only steps that create, switch or publish a branch, and none of them runs an agent.** Every other step finds the worktree already established on its declared branch and leaves it there.

Repair disclosure is the case that looks like an exception and is not: it is agent-driven and it **rewrites history**, which moves the commit and is what it is for. Moving the commit is not moving the branch — nothing is created, nothing is switched to, and nothing is published — and the step declares exactly that, so the rule is enforced against it like any other.

This is a restriction on the agent-driven steps, and it is deliberate. An agent given a shell and a goal will reach for git when it seems expedient — cutting a branch of its own, committing directly to the base, resetting to escape a state it does not understand. Each of those is locally reasonable and globally wrong: a ghost branch strands the work where nothing will find it, and a commit on the base defeats the entire proposal model, which exists so that nothing reaches the mainline unreviewed.

Telling the agent not to is necessary and not sufficient. The prompts say so, and an agent that decides otherwise while it works leaves no trace that anything unusual happened.

So the producing steps **check**. Each records the branch and the commit it is on before its agent prompt, and refuses to continue if either moved: a changed branch means the agent switched away, and a moved commit means it committed. Both are failures of the step, reported as what they are rather than discovered later as a branch nobody expected or a mainline nobody meant to touch.

The check costs two reads and turns an unenforceable instruction into an invariant.

### Closing the branch is part of finishing, not of stopping

Close branch runs when the contributor's part **completed**. A run that parked, was blocked, or failed leaves the worktree exactly where it stopped, because that state is what someone will resume from or diagnose.

Returning the worktree is not the same act as releasing a claim. An operator who releases mid-work is stepping away and keeps their branch; a contributor role that finished is done with it and owes the arena a clean starting point for the next item.

### The implementation lives in the branch, and the record names it

The deliverable of a producing step is the **commit it left on the branch**, and what it records is that commit — a result captured from the tree, in [artifacts-and-signals.md](artifacts-and-signals.md)'s sense.

Recording a diff instead would be recording a copy. The copy can legitimately be empty — a resumed branch whose work an earlier run already committed has a clean tree, so there is nothing left to capture — which means an empty record cannot be read as "the step did nothing" without deadlocking a resumption over work sitting right there in the branch. A copy that may be empty, that nothing reads back, and that can disagree with the thing it copies is not a record worth keeping.

A commit identifier has none of those problems. It is never ambiguous, it names exactly one state, and anyone can resolve it to the change.

So the question of whether the work exists is answered by the branch: **does this branch carry anything its base does not?** That is what establishes the step succeeded, and the record says which commit it produced.

### Every step writes something

No step is purely an inspection. Plan touches no file and still produces the plan, which is its whole output: a plan existing only inside a prompt would be a decision nobody could review, revisit, or hold the change against.

Where a result is stored is the backend's business, not this flow's. What this flow requires is that it **is** stored, and that it reaches a reader.

### Where judgement lives, and why it is worth knowing

Plan, implement, review, coverage, review the proposal and review the filing spend an agent prompt on a decision, and so does repair disclosure — the one whose prompt answers a refusal rather than producing a deliverable. Open branch, open request, close branch, verify merge result, merge, record merge commit and file the items spend none.

That distinction is not bookkeeping. A step whose outcome an agent decides is **neither cheap nor reproducible**: it costs a prompt, and running it twice on the same input can produce different work. A mechanical step is both — it costs nothing beyond the operations it performs, and it does the same thing every time. File the items is mechanical for exactly that reason: *what* to file was the plan's decision and *whether it closes the gap* was the filing review's; executing the filing decides nothing.

**Deliverable and record are not the same thing**, and the difference matters most where they diverge. Implement's deliverable is a working change; what is recorded is the commit carrying it. Review's deliverable is the corrections themselves — already in the tree — and what is recorded is prose *about* them. A step is not finished when its entry is journaled; it is finished when its deliverable exists.

## The contributor steps

### Plan

States what will be done and why, before the worktree is touched — and elects the route matching the work's shape.

On the filing shape, its deliverable is the intended items themselves — each fully drafted and carrying a key stable within this resolution — because two steps consume them verbatim: the filing review judges exactly what would be filed, and the filing files exactly what was judged, idempotently by that key.

**This step does not modify anything.** It is the one step in the flow that cannot, and the restriction is the point: a plan written by a step that has already started changing things is a description of work done, not a decision about work to do. The distinction is what makes the plan reviewable.

The plan is required. A resolution that reaches the proposal without one refuses to propose — a request carrying no plan presents a silent read failure as a finished change.

### Open branch

Puts the worktree on a branch for this item, cut from the base, and records what it was cut from.

Mechanical, and it exists as its own step for three reasons. A branch that fails to open — a dirty tree, a name already taken, a base that cannot be resolved — fails here, naming its own cause, instead of surfacing as an implement failure that sends a reader looking at the agent. Implement's concern stays "make it work" rather than "prepare a workspace and then make it work". And every step after this one commits, so the branch must exist before any of them runs.

Recording the base is what makes the change answerable later: *what is this relative to* has one answer, fixed at the moment the branch was cut, rather than being re-derived against a base that has since moved.

### Implement

Makes the change work.

It drives the change against the gate in a bounded loop: each failing run re-prompts with the gate's output, until the gate passes or the treasurer stops allowing rounds. Exhaustion parks; it does not fail.

**This step commits.** The prompts tell the agent not to — committing mid-loop would bury a half-finished round in history — so the step stages and commits once the gate is green, and its result names that commit.

The result is captured only on a passing gate. A commit recorded after a failing gate would name work that does not build.

**When the route returns here** — a rework handback from the maintainer's review — the transfer message carries what must change, and the work happens on the item's existing branch, as further commits: the proposal's history is the record of the rounds, not a rewrite of them.

### Review

**A second implementation pass, accountable to the change rather than to the plan.**

Implement follows the plan: its job is to produce what was decided. Review's job is to take what implement produced and make it better — correctness, surprising behaviour, missed edge cases, unnecessary complexity. It is not restricted to what the plan anticipated, and it is not obliged to defend the plan's choices.

That is the whole boundary, and it is worth stating because the two steps otherwise look like the same activity run twice. They differ in what they answer to: implement answers to the plan, review answers to the code.

**The boundary is physical: two commits.** Implement's work and review's work are separate commits on the branch, so what each step did is visible rather than inferred. A reader asking "what did the review change" reads a commit, not a diff between two states nobody recorded.

**It fixes what it finds.** It has the change and the context loaded; leaving a fault for someone else costs another prompt to rediscover what this step already knows.

Its result is a briefing for the person who will review the proposal — what was looked for, what was changed, what still needs a human decision.

### Coverage

Makes the change tested, to this project's standard.

**This is a requirement to meet, not a state to assess.** No producing step runs after this one, so a gap described rather than closed is a gap that ships, handed to nobody.

Where a change cannot be tested as written, this step **restructures it so that it can be**. Code that cannot be tested is not finished.

Leaving something untested is an exception, justified specifically — what resisted, why, and what would have to change — never a list handed onward.

### Open request

Proposes a change that can land.

**It runs the gate on the branch first, and does not propose if the gate fails.** The maintainer will run the same gate; a request that cannot pass it wastes their attention on a change that was never going to land, and returns the item for a round trip the contributor could have prevented. Measuring first is what makes the proposal a proposal rather than a guess.

The gate's result is recorded and travels with the request, so a reader knows what was established rather than taking it on trust.

Before anything leaves the machine it **records what the producing steps after implement left behind**, as its own commit — implement is the only other step that commits, so without this the request would describe a branch missing that work.

**It refuses to propose over a worktree still carrying changes.** A request describing a branch that does not contain the work is the failure this exists to prevent, and it is silent from every angle but `git status`.

It then pushes the branch and opens the request, in that order: opening first would describe a branch that does not yet exist.

The request body carries the plan, each producing step's briefing, and the gate's result. The plan is required; the others appear when they have content.

**On a rework round the request already exists.** The push updates it; a second request is never opened for the same branch, and the step completes on the request being current rather than on it being new.

**It is mechanical on every path, and that is a declaration it is held to.** What the branch carries can be refused twice on the way out — by the pre-commit hook at the commit, and by the disclosure guard at the push — and **neither refusal is answered here**. Repairing one in place would make the longest cheap step in the flow one that can spend, which costs it all three guarantees a mechanical step earns ([flow-registration.md](flow-registration.md) § Step configuration): free, deterministic, and cheap to retry.

The two refusals stop differently, and what separates them is the state of the tree:

| Refused | What it does | Why |
|---|---|---|
| **the push**, by the disclosure guard | elects **repair disclosure** and completes | the tree is committed and clean, so the step completes into its declared state and the repair takes the branch from there |
| **the commit**, by the pre-commit hook | **blocks**, keeping the hook's words with the step | the refused work is still in the tree, and a step that ends over a dirty tree has not completed ([resolution.md](resolution.md) § Steps and the worktree) — a flow does not leave one step's changes uncommitted for a later step to sweep up |

**A commit refused here is an anomaly, not a routine repair.** Every producing step commits its own work through the same repair, in its own dispatch where prompting is what it is for, so uncommittable content reaching this step means a step before it left work behind. The block says so, and the remedy is the one the commit contract names — deletion, not an ignore rule ([resolution.md](resolution.md) § The commit contract).

**A push refusal that survives one repair round blocks.** The repair answers the refusal in one dispatch, so a branch arriving back from it still refused has had its round, and electing the same round again is how a loop is built. What was refused is kept with the step, unpublished, for whoever clears the block. A later rework round arrives from coverage rather than from the repair, so a fresh refusal there earns a fresh repair.

### Repair disclosure

Rewrites the history that names what may not leave the machine, so the branch can be pushed, and elects the request back.

It exists because the mechanical part is the deliverable: the request's job is to gate, commit, push and propose, and the repair is a rare answer to a refusal on that job's failure path — the one shape `flow-registration.md` recommends splitting. The record it leaves is the more valuable half: **a resolution that rewrote its own history to get a push out used to leave no trace of why**, and the journal entry names it.

**It is not handed the refusal.** Every hand-off is published — an election message, a park record — through the same guard that refused it, and the unpublished draft belongs to the step that wrote it ([resolution.md](resolution.md) § Drafts). So it receives the **act**, and asks the guard itself what a push of this branch would carry, without pushing ([disclosure.md](disclosure.md) § A refusal does not travel). The answer never leaves the dispatch, and it is the current one where a copy could be stale.

**It commits before it asks**, through the same repair every producing step commits through. The tree it is handed is clean, so that is normally nothing at all — it is there because what the guard is asked about must be the branch **as the commit left it**, and a round arriving over a tree that still carries work would otherwise ask about a state nobody proposes.

**It moves no branch.** Rewriting history moves the commit, which it declares; it neither creates, switches nor publishes a branch, which is what keeps § Branches are moved only by mechanical steps true with an agent-driven step in the graph.

Where an arena's worktree cannot answer what a push would disclose, the step **blocks naming that** rather than prompting: an agent asked to rewrite history without being told what was refused has nothing to work from.

**It answers the push refusal only.** A commit refused at the request blocks instead of routing here, because the refused work is still in the tree and a step cannot complete over one (§ Open request). Both refusals name the same problem, so the intended end state is that both route.

> The commit refusal reaches this step once the write-contract dirty check is differential rather than absolute — [#334](https://github.com/promise-language/flow/issues/334). Until then a commit refused at the request stops for a person where it used to be repaired in place; the push path, which is the one that fires in practice, keeps its repair.

### Close branch

Returns the worktree to the base branch, and routes the item to the maintainer's review — the contributor's part is complete, and where the maintainer is another principal, this election is the handoff.

It does not delete the branch. The branch is the product: it carries the request, and the request outlives the role that opened it.

## The maintainer steps

### Review the proposal

Judges the proposal **as what will land**: the diff, the plan it claims to implement, the briefings, the gate result it arrived with — and the item it all answers to.

Its election is the decision, and the three routes are the three honest outcomes:

- **verify merge result** — the proposal should land. The route continues to integration.
- **implement** — the proposal needs work the contributor must do. The message carries what must change and why, specifically enough to act on: a handback with a vague message spends a full contributor round to rediscover what this step already knew. The route crosses back to the contributor's account of record.
- **finalize: rejected** — the item should not be resolved by this proposal or any successor to it, with the reasons in the finalizing message.

The distinction between the last two is the one worth deciding deliberately: rework expects the resolution to continue; rejection ends it ([resolution.md](resolution.md) § Finalizing).

A merged request found already in place — a human integrated by hand — is not an anomaly: the step observes `pr-merged` set and elects the route onward, so the record still completes.

### Verify merge result

Measures the **merge result**, not the branch. A branch that was green when proposed can be red after merging, because the mainline moved underneath it. Verifying the branch again would re-establish something already known and miss the thing that changed.

Mechanical: the gate answers, and this step does not argue with it. A failing gate does not land, and does not route the change back to the producing phase on its own authority — the producing steps have had their turns; a gate failing after all of them is a fact for the maintainer's review, where the handback carries the gate's own output.

### Merge

Lands the change: merges the request. The same act by two routes — a push to the mainline, or a merge of the request — and which one depends on how the change was proposed.

### Record merge commit

Records the merge commit the landing produced, and finalizes the item as resolved. What landed has exactly one name, and this is where it is written down.

## The filing steps

### Review the filing

Checks the plan's intended items against the gap they exist to close: together, do they cover it; singly, is each one actionable, correctly scoped, and filed where it belongs. It answers to the gap, not to the plan — items the plan missed are its findings as much as items the plan got wrong.

It elects **file the items** when the set is right, or routes back to **plan** with what is missing or wrong — the same accountability structure as the change route's review, applied to a different deliverable.

### File the items

Files what was decided, and records the filed references as its result — the `filed-items` list is the deliverable, and each reference in it must resolve to an item that exists on the backend.

Mechanical: what to file was decided at plan, and that the set closes the gap was established at review the filing. Every filed item is an outward write, guarded as every outward write is.

**Filing is resumable, not atomic — completion is the atomic act.** The backend files one item at a time and offers no transaction, so the step never pretends otherwise: each intended item is filed through the orchestrator's idempotent surface ([orchestrator.md](orchestrator.md) § Filing), keyed by this item and the intended item's key, so a dispatch interrupted after filing three of five files the two missing on resume and duplicates nothing. The journal entry appends only when every intended item exists — the step completes whole or not at all, and the `filed-items` artifact lists exactly what exists.

**A guard refusal routes back to plan.** The refused text is the plan's — the intended items are its artifact — and this step is mechanical: retrying it cannot change a word, so parking here would buy the loop the treasurer exists to stop. The election back to plan names **the act and the origin** and quotes nothing the guard refused — a refusal does not travel ([disclosure.md](disclosure.md) § A refusal does not travel), so the plan step asks the guard what it would publish rather than reading a copy. Items already filed stay filed and stay recorded, and the corrected set returns through the filing review before anything more goes out.

It finalizes the item as resolved: the filing **is** the resolution.
