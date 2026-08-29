# The issue flow

**Normative.** The step set this repository ships for resolving issues, and what each step must do.

`docs/resolution.md` defines the process, boundaries and responsibilities common to any resolution. `docs/resolution-standalone.md` defines the model this flow runs under. This document defines **these steps** — a different flow may hold every property in those documents with a different step set.

## Contributor steps

In order. Each produces exactly one result, and the result is the step's identity.

| # | Step | Concern | What the agent decides | Writes | Deliverable |
|---|---|---|---|---|---|
| 1 | plan | What will be done, before anything changes | What to do | the `plan` — nothing in the worktree | A decision about the work to do |
| 2 | **open branch** | Put the worktree on a branch for this item | **nothing — mechanical** | the branch, and a record of what it was cut from | A worktree ready to be changed |
| 3 | implement | Make it work | How to change it, and how to answer a failing check | the solution, a commit, and the `implementation` record naming it | A branch carrying the change |
| 4 | review | Make it right | What is wrong, and how to fix it | the solution, a commit, the `review` briefing | Corrections applied, not reported |
| 5 | coverage | Make it tested | What needs testing, and what must change to allow it | tests **and** the solution, a commit, the `coverage` briefing | Tests meeting the project's standard |
| 6 | **open request** | Propose a change that can land | **nothing — mechanical** | the gate's result, the pushed branch, the request, the `pr-open` signal | A request that will pass the maintainer's gate |
| 7 | **close branch** | Return the worktree to the base | **nothing — mechanical** | nothing — it restores | An arena ready for the next item |

Steps 3 to 5 are the producing phase. Each carries the change further under a different concern, and each may modify the worktree — including the solution itself. The properties governing them are in `resolution-standalone.md` and are not repeated here.

### Verify is a tool, not a step

There is no verification step, and that is deliberate.

The verify command is **something a producing step uses while working** — it formats, it checks, it tells the agent whether what it has written holds together. A step runs it as often as it finds useful, which is why implement's loop is built around it. It is an instrument, and instruments do not get their own place in a sequence.

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

The plan step answers two questions, and only the first has a plan as its answer: *should this be done*, and *how*.

Some items should not be done, and discovering that is the plan step working rather than failing. It is also the cheapest possible moment to discover it — before a branch exists, before an agent has written anything, and before anyone has reviewed a change that should not have been made.

So the step's deliverable is a plan **or** a refusal, and a refusal names which of these it is:

| Refusal | Means | Evidence it carries | What a person does |
|---|---|---|---|
| **already done** | The change exists, or the desired state already holds | Where it is already true | Close the item |
| **duplicate** | The work is pending under another item | The item that covers it | Redirect to that item |
| **conflicts** | The item asks for something the normative documents forbid | The document and what it says | Change the item, or the document |
| **not viable** | The work cannot be done as asked | The specific reason | Rethink or close |

**Already done and duplicate are not the same finding.** A duplicate means the work is pending somewhere else and will happen there. Already done means there is no pending work anywhere — the item asks for something that is already true, whether because somebody did it or because it never needed doing. They differ in what a person does next, which is why they are separate.

**Every refusal carries evidence a person can check.** That is what makes it a finding rather than a shrug: a refusal naming the duplicate item, or the line where the behaviour already exists, or the document it contradicts, can be confirmed or overturned in a minute. One that says only "this cannot be done" is indistinguishable from an agent that gave up, and a reader has no way to tell which it was.

A refusal **blocks the resolution on a named reason.** It is not a failure and not a dead end: the flow stops, the reason says what would unblock it, and when a person acts on that reason the resolution continues from where it stopped. An item found to duplicate another is usually closed instead — but if the finding was wrong, clearing the block resumes the work rather than starting it again.

The set is closed. A refusal fitting none of these means the vocabulary is wrong, not that a fifth may be invented in prose.

**Not knowing enough to plan is different and is not a refusal.** An item that could be planned given an answer asks the question and parks, which is a resumable state — the plan step will run again with the answer in hand. Refusing means no answer would help.

A step that stops this way **keeps as much of its work as it can**, and continues from it when it resumes. How much survives depends on what the work was: implement's changes are in the worktree, so a run cut off mid-step — by a park, a crash, or a power failure — leaves them there to resume from. The plan step has no such durable half-product, so it needs somewhere to put one. The question exists because of that work: the step read enough to find the ambiguity, and discarding it means re-deriving the same reasoning to arrive at the same question, now answered. The plan step is where this matters most, because it changes no files — a park with nothing kept erases the step entirely.

What it keeps is scaffolding, not a result: it does not resolve the step, only the step that wrote it reads it, and it is discarded once the step completes. `resolution.md` § "Work in progress" states the general rule.

### A step's write contract is checked, not merely stated

The **Writes** column is a contract, and every step is held to it after it runs.

Prevention comes first where it exists — the plan step runs in a mode that forbids editing, and an agent may be given a gate over the actions it proposes, which refuses a forbidden command before it runs and lets the agent adapt mid-turn rather than losing the whole turn.

But prevention of either kind is enforced **by the agent**, not by this flow, which passes a flag or a configuration and trusts the outcome. A shell, a tool that shells out, or a mode that does not apply cleanly goes straight through it. So prevention is worth having, is the cheapest place to catch a violation, and is not the guarantee.

**The two layers are a guard and a gate**, in the senses `resolution.md` defines. Neither substitutes for the other, and the reason is what each can and cannot see: a guard refuses an action before it happens and is bypassed by any route that does not pass through it, while a gate measures the result however it came about and cannot speak until the turn is over.

**The guarantee is the check afterwards.** Each step records the branch, the commit, and the tree state before its agent turn, and verifies against them after:

| Violation | Means |
|---|---|
| The branch moved | The agent switched or created a branch |
| The commit moved | The agent committed |
| The tree is dirty where the step writes nothing | The agent edited what it was not there to edit |

A violation **blocks the resolution and names what happened.** It is not a failure to retry: the same prompt against the same state will very likely do the same thing, so retrying spends a turn to arrive back here.

**The changes are not discarded.** Reverting would restore the invariant by destroying work nobody has seen — the same silent loss this flow spends three steps preventing elsewhere. What an agent did outside its contract may be worthless or may be the most valuable thing in the run, and the flow is not in a position to tell. It stops, leaves the evidence in place, and says precisely what was violated so a person can look at it and decide.

One check covers all three because they are one question: **did this step do only what it said it would.**

### Branches are moved only by mechanical steps

**Steps 2, 7 and 8 are the only ones that create, switch or publish a branch, and none of them runs an agent.** Every other step finds the worktree already on the right branch and leaves it there.

This is a restriction on the agent-driven steps, and it is deliberate. An agent given a shell and a goal will reach for git when it seems expedient — cutting a branch of its own, committing directly to the base, resetting to escape a state it does not understand. Each of those is locally reasonable and globally wrong: a ghost branch strands the work where nothing will find it, and a commit on the base defeats the entire proposal model, which exists so that nothing reaches the mainline unreviewed.

Telling the agent not to is necessary and not sufficient. The prompts say so, and an agent that decides otherwise mid-turn leaves no trace that anything unusual happened.

So the producing steps **check**. Each records the branch and the commit it is on before its agent turn, and refuses to continue if either moved: a changed branch means the agent switched away, and a moved commit means it committed. Both are failures of the step, reported as what they are rather than discovered later as a branch nobody expected or a mainline nobody meant to touch.

The check costs two reads and turns an unenforceable instruction into an invariant.

### Closing the branch is part of finishing, not of stopping

Step 8 runs when the resolution **completed**. A run that parked, was blocked, or failed leaves the worktree exactly where it stopped, because that state is what someone will resume from or diagnose.

Returning the worktree is not the same act as releasing a claim. An operator who releases mid-work is stepping away and keeps their branch; a resolution that finished is done with it and owes the arena a clean starting point for the next item.

### The implementation lives in the branch, and the record names it

The deliverable of a producing step is the **commit it left on the branch**, and what it records is that commit.

Recording a diff instead would be recording a copy. The copy can legitimately be empty — a resumed branch whose work an earlier run already committed has a clean tree, so there is nothing left to capture — which means an empty record cannot be read as "the step did nothing" without deadlocking a resumption over work sitting right there in the branch. A copy that may be empty, that nothing reads back, and that can disagree with the thing it copies is not a record worth keeping.

A commit identifier has none of those problems. It is never ambiguous, it names exactly one state, and anyone can resolve it to the change.

So the question of whether the work exists is answered by the branch: **does this branch carry anything its base does not?** That is what establishes the step succeeded, and the record says which commit it produced.

### Every step writes something

No step is purely an inspection. Step 1 touches no file and still produces the plan, which is its whole output: a plan existing only inside an agent's turn would be a decision nobody could review, revisit, or hold the change against.

Where a result is stored is the backend's business, not this flow's. What this flow requires is that it **is** stored, and that it reaches a reader.

### Where judgement lives, and why it is worth knowing

Steps 1 through 4 spend an agent turn on a decision. Step 5 spends one on prose about a decision something else made. Step 6 spends none.

That distinction is not bookkeeping. A step whose outcome an agent decides is **neither cheap nor reproducible**: it costs a turn, and running it twice on the same input can produce different work. A mechanical step is both — it costs nothing beyond the operations it performs, and it does the same thing every time.

So the flow's cost and its variability both sit almost entirely in steps 1 to 4, and every one of them is there because a decision has to be made. Step 5's turn is the one that buys prose rather than a decision, which is why the fallback exists: when it produces nothing, nothing of consequence is lost.

**Deliverable and record are not the same thing**, and the difference matters most where they diverge. Implement's deliverable is a working change; what is recorded is the patch. Review's deliverable is the corrections themselves — already in the tree — and what is recorded is prose *about* them. A step is not finished when its artifact resolves; it is finished when its deliverable exists.

Steps 2 through 4 are the producing phase. Each carries the change further under a different concern, and each may modify the worktree — including the solution itself. The properties that govern them are in `resolution-standalone.md` and are not repeated here.

### 1. Plan

States what will be done and why, before the worktree is touched.

**This step does not modify anything.** It is the one step in the flow that cannot, and the restriction is the point: a plan written by a step that has already started changing things is a description of work done, not a decision about work to do. The distinction is what makes the plan reviewable.

The plan is required. A resolution that reaches the proposal without one refuses to propose — a request carrying no plan presents a silent read failure as a finished change.

### 2. Open branch

Puts the worktree on a branch for this item, cut from the base, and records what it was cut from.

Mechanical, and it exists as its own step for three reasons. A branch that fails to open — a dirty tree, a name already taken, a base that cannot be resolved — fails here, naming its own cause, instead of surfacing as an implement failure that sends a reader looking at the agent. Implement's concern stays "make it work" rather than "prepare a workspace and then make it work". And every step after this one commits, so the branch must exist before any of them runs.

Recording the base is what makes the change answerable later: *what is this relative to* has one answer, fixed at the moment the branch was cut, rather than being re-derived against a base that has since moved.

### 3. Implement

Makes the change work.

It drives the change against the gate in a bounded loop: each failing run re-prompts with the gate's output, until the gate passes or the step's prompt budget is exhausted. Exhaustion parks; it does not fail.

**This step commits**, and it is the only producing step that does. It stages, captures the patch, then commits — in that order, because a patch captured before staging cannot see added files and one captured after committing sees a clean tree.

The result resolves only on a passing gate. A change attached after a failing gate would record work that does not build.

### 4. Review

**A second implementation pass, accountable to the change rather than to the plan.**

Implement follows the plan: its job is to produce what was decided. Review's job is to take what implement produced and make it better — correctness, surprising behaviour, missed edge cases, unnecessary complexity. It is not restricted to what the plan anticipated, and it is not obliged to defend the plan's choices.

That is the whole boundary, and it is worth stating because the two steps otherwise look like the same activity run twice. They differ in what they answer to: implement answers to the plan, review answers to the code.

**The boundary is physical: two commits.** Implement's work and review's work are separate commits on the branch, so what each step did is visible rather than inferred. A reader asking "what did the review change" reads a commit, not a diff between two states nobody recorded.

**It fixes what it finds.** It has the change and the context loaded; leaving a fault for someone else costs another turn to rediscover what this step already knows.

Its result is a briefing for the person who will review the proposal — what was looked for, what was changed, what still needs a human decision.

### 5. Coverage

Makes the change tested, to this project's standard.

**This is a requirement to meet, not a state to assess.** Nothing runs after the producing phase and the item is resolved when the flow finishes, so a gap described rather than closed is a gap that ships, handed to nobody.

Where a change cannot be tested as written, this step **restructures it so that it can be**. Code that cannot be tested is not finished.

Leaving something untested is an exception, justified specifically — what resisted, why, and what would have to change — never a list handed onward.

### 6. Open request

Proposes a change that can land.

**It runs the gate on the branch first, and does not propose if the gate fails.** The maintainer will run the same gate; a request that cannot pass it wastes their attention on a change that was never going to land, and returns the item for a round trip the contributor could have prevented. Measuring first is what makes the proposal a proposal rather than a guess.

The gate's result is recorded and travels with the request, so a reader knows what was established rather than taking it on trust.

Before anything leaves the machine it **records what the producing steps after implement left behind**, as its own commit — implement is the only other step that commits, so without this the request would describe a branch missing that work.

**It refuses to propose over a worktree still carrying changes.** A request describing a branch that does not contain the work is the failure this exists to prevent, and it is silent from every angle but `git status`.

It then pushes the branch and opens the request, in that order: opening first would describe a branch that does not yet exist.

The request body carries the plan, each producing step's briefing, and the gate's result. The plan is required; the others appear when they have content.

### 7. Close branch

Returns the worktree to the base branch.

Runs only when the resolution **completed**. A run that stopped — parked, blocked, or failed — leaves the worktree exactly where it stopped, because that state is what someone will resume from or diagnose.

It does not delete the branch. The branch is the product: it carries the request, and the request outlives the resolution that opened it.

## Integration

The phase that lands the change. It belongs to the maintainer capability, and a principal holding both may reach it in the same resolution — see `resolution-standalone.md`.

### Integrate

Runs the **gate** on the merge result, and lands the change if it passes.

The gate answers one question: *will the mainline still be green*. It measures and modifies nothing, which is what makes its answer reproducible by anyone who was not there — a reviewer, a later bisect, a rebuild elsewhere.

**It measures the merge result, not the branch.** The branch was already measured before the request was opened; what has changed since is the mainline. Re-measuring the branch would re-establish something already known and miss the only thing that moved.

Landing is a push to the mainline, or a merge of the request — the same act by two routes, and which one depends on how the change was proposed.

A failing gate does not land, and does not return the change to the producing phase. The producing steps have had their turns; a gate failing after all of them is a fact for a person.

> Not built. The step identities exist and the flow that would use them refuses on dispatch — [#29](https://github.com/promise-language/flow/issues/29).
