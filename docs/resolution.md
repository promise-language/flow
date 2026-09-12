# Resolution

> **Tag:** `resolution` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how an item is resolved, independent of what drives the resolution. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it. Nothing here records progress, status, or history.

Two drive models exist, and they are the only two:

| Model | Who decides what runs next |
|---|---|
| **Standalone** | The flow binary. No server exists. See `resolution-standalone.md`. |
| **Orchestrated** | A server schedules, leases and dispatches; the binary executes one step on command. See `resolution-orchestrated.md`. |

Everything in *this* document is true of both. A statement that is true of only one belongs in that model's document, and a statement true of both belongs **here and nowhere else** — restating it in the model documents is how two copies drift into disagreement.

## Nothing is bought twice

> **A resolution pays for each thing once.** What a dispatch established — reasoning, a conversation, a tree, an answer, a measurement — is carried to whoever needs it next. **Nothing already paid for is discarded while it is still needed, and nothing still held is bought again.**

This is a requirement on the system, not advice to a step author, and it is not a performance concern to be weighed against convenience. **Discarding work the resolution is still holding, and then buying it back, is a defect.** It is the same defect whether the cost lands on the agent's budget, on the operator's time, or on the wall-clock of a fleet — and in every case it is paid by someone other than whoever decided not to keep it.

Every mechanism in this document that carries something across a stop exists to serve this, and none of them is optional in the sense that matters:

- **The journal** carries the route and the reasons, so nothing re-derives where the resolution is.
- **What a step committed** carries the work itself, so nothing re-does it.
- **The draft** carries the reasoning behind an unfinished result, so a resumed step does not think it through again (§ Drafts).
- **The session** carries the conversation itself across every step of the route, so the agent is not reintroduced to work it has already done (§ The agent session).
- **The claim** keeps the item with the arena holding all of the above, so a resume finds it rather than starting over (§ Claiming).

A step cannot be asked to notice that its own work was thrown away between dispatches: by the time it runs it has no way to tell a first attempt from a resume that lost everything. So the obligation is on the mechanisms — they preserve what they can, hand it over without being asked, and never drop something cheap to keep because keeping it was not wired up.

**The test is not whether a re-derivation is affordable.** It is whether the resolution already had the answer. A dispatch that spends to reproduce something the system was holding has not been inefficient; it has been wrong, and the budget it consumed reports a cost that bought nothing.

## The item and its lifecycle

An **item** is a unit of work owned by a backend. A **flow** is a set of steps that resolves items of a given type — a graph, not a sequence. Each step declares what may run after it, and the route an item actually takes is elected step by step as it is worked.

A **step** is an independent function. It receives the item, the journal of everything that has happened so far, and the context to work in; it does its work; and it ends in one act that records its result and elects the route onward — the next step, or finalization. Producing and routing are inseparable: a result cannot be recorded without saying where the resolution goes from here.

A step produces exactly one result: an artifact, or a signal. The distinction between the two — what each is, who writes it, and what it carries — is defined in [artifacts-and-signals.md](artifacts-and-signals.md). A step that produces nothing has not run. The result is the step's identity — it is what the treasurer's ledger meters, what a park names, and what `status` reports.

The lifecycle is: **claim → advance one step at a time → finalize**, the claim released and retaken at each handoff between roles (§ Accounts, capabilities and roles). An item enters at claim and leaves at finalize; everything between is a sequence of single-step advances, each recorded in the journal.

## Accounts, capabilities and roles

A resolution involves people, and the flow knows who they are. Three layers, each defined by how it is established:

- An **account** is an identity in the backend's own namespace. Two accounts matter to every item — the **creator**, who filed it, and the **runner**, whose credentials the current resolution acts as — and one more per role, below.
- A **capability** is a verifiable fact about an account on the repository: it can push, it can merge, it can approve. Capabilities are **detected from the backend, never declared** — an account's word for what it may do is worth exactly what the backend will actually permit, so the backend is asked rather than told.
- A **role** is a part the flow defines — contributor, maintainer, reviewer — and the vocabulary it routes and authorizes by. Every step is tagged with the role that performs it. A role names the capabilities it requires. The roles a runner *could* assume are **derived from its detected capabilities**, never assigned by hand and never assumed by assertion; the roles it *may* assume are those narrowed by the **coverage** it declares — the roles it is meant to play. Capability is the ceiling and coverage is the choice within it: a runner may decline a role its account could back, and nothing it declares can add a role its account cannot.

> **Every step belongs to exactly one role, and only a runner that may assume that role executes it.**

> **A role is assumable only where the runner both declares it and its account backs it.**

The role check is what keeps a resolution from starting work it cannot finish; the backend's own permissions are the enforcement of last resort — an account without the merge capability cannot merge, whatever it believes its role to be. The two layers agree by construction, because the first is derived from the second. Coverage only ever narrows, so it cannot disagree with either: a declared role the account cannot back is simply not assumable — a handoff at that boundary, not a misconfiguration.

The creator's account matters beyond attribution: the creator's detected standing is an input a step may route on, which is how a flow gives an untrusted source's item a stricter route than a maintainer's own.

### Whose move it is

At any moment an unfinalized item **awaits exactly one thing**: the role of the step its journal routes to next — or, while that step is a signal wait, the signal. Awaiting a role is what a runner's work-selection answers to: the items offered to a runner are the items awaiting a role it may assume, on an arena their placement restrictions admit ([environment.md](environment.md) § Placement).

Reaching a step whose role the current runner cannot assume — or may not continue into — is a **handoff**: the runner's part of the resolution is complete, the claim is released, and the item records the role it now awaits. A handoff is not a park and not a failure; it is a role finishing its part.

The first entry a runner appends in a role binds its account as that role's **account of record** for the item. A route that returns to a role returns to its account of record — a rework handback goes to *the* contributor who proposed, not to whichever account could have. The binding is read from the journal, which already carries who ran every step.

**A role is matched only against the flow's declared set.** The vocabulary is open — a flow names whatever roles its work needs — and that is exactly why every reference is checked: a step's tag, a lookup by role name, the awaited-role marker an item carries. A reference that names no declaration is refused loudly, never left to match nothing, because the silent reading is a trap: an awaited role that does not exist looks identical to a role whose runner has not arrived, and the item sits unofferable forever with nothing naming why. A recorded awaited role outside the declaration therefore reports the item blocked, naming the unknown role — a person fixes the flow or the record; nothing ever matches against what is not defined.

### One principal, several roles

A runner whose capabilities cover the roles on both sides of a boundary crosses it without a handoff: the same resolution continues, the claim is kept, and the phases remain distinct in the journal — the change is still proposed, the record still shows which role performed each step. Carrying an item through from proposal to integration is this, and nothing more: a principal covering both roles, on an account that backs both, crossing the boundary. Nothing declares carrying through as such, and nothing selects a flow that does it: the runner covers both roles or it does not, and the boundary is a step in the one graph like any other. Where a standalone binary declares its coverage, and what it must say about it at runtime, is [resolution-standalone.md](resolution-standalone.md) § Declaring what a binary may do.

## Claiming

A claim is **exclusive**: at most one worktree works an item at a time, and at most one item is worked per worktree. The binding is one-to-one in both directions.

A claim is **idempotent for its holder**: re-claiming an item this worktree already holds succeeds and changes nothing.

A claim carries the credential for every subsequent write. Operations that modify an item require it; read-only inspection does not.

A claim is taken **for the role the item awaits, on an arena its placement restrictions admit** ([environment.md](environment.md) § Placement): a runner that cannot assume the role, or an arena the item excludes, is refused — claiming an item whose pending work is not yours to do, or not doable here, wastes an exclusive claim on work that cannot proceed.

Claims are released explicitly, by handing off, or by finalizing. A claim is never released as a side effect of a step failing, a step parking, or a process exiting — work in progress keeps its claim so it can be resumed.

### A claim binds worktrees, not processes

A claim says which item a worktree holds. It does not say whether anything is running there, and it cannot: a worktree sitting idle under a claim and one mid-resolution carry the same claim. So two runs started in one worktree — one by an unattended driver, one typed by a person who walked into the same checkout — satisfy the binding above exactly, one worktree and one item, while doing what that binding exists to prevent. They advance the same lifecycle twice, interleave writes to the same durable records, spend against one ledger twice, and drive git against one tree from two processes. None of it is detected as it happens; it surfaces later as a corrupted record or a wedged branch, at a moment that has nothing to do with the collision.

> **At most one process advances an item in a worktree at a time.**

**Advancing is the subject, not the command.** Whatever drives a step — a whole resolution or a single step — is one advancing process. Reading is not one, and neither is an operator writing to the item from somewhere else: what is being separated is processes changing one tree, not everything that touches the item.

**It is a second exclusion, not a stronger claim.** A claim is asserted where every worktree can see it, because what it separates is worktrees. This one is asserted in the worktree, because what it separates is processes inside it — and no assertion made elsewhere can see them.

An advancing process therefore **registers its run in the worktree** before its first step, and gives the registration up when the run ends. Four properties are what make the registration worth having:

- **Acquisition is atomic.** It either succeeds or reports the run already holding the worktree. Reading first and writing after is not an implementation of this: two attempts arriving together both read an unheld worktree, and both proceed.
- **It names its holder** — the machine, the process, and the moment that process started. The start time is not decoration. A process identifier is reused, so an identity without it says only that *something* holds this, which after a reboot is indistinguishable from saying nothing at all.
- **A holder that is gone is observed to be gone.** A crash must not wedge the worktree, and a long step must not be mistaken for a crash — so a registration is released when its holder is found not to be running, and never when a timer expires. That is what this system does with everything it holds, for the same reason each time: elapsed time is evidence about the clock, not about the holder.
- **It is readable without acquiring it.** Asking what is running in a worktree is not an attempt to run something there. A caller that had to contend for the registration in order to read it could not ask the question at all, and would be left inferring the answer from the state of the tree — which is the guess the registration exists to replace.

A process finding the worktree held **does not wait and does not take it over.** It stops, naming the holder. There is nothing an override could unlock: a holder no longer running is already released by the rule above without anyone asking, and a holder still running is part-way through changing the very state a second run would start from.

## Advancing

One invocation advances **at most one step**. This holds in both drive models and is what makes a run inspectable: after any invocation, the item is in a state some human can read.

### The journal

> **The journal is the durable record of the route: an append-only sequence of completed step executions, and the only thing position is ever derived from.**

Every completed step execution appends exactly one entry, written in the same act that records the step's result — result and route land together or not at all. An **execution** is the unit the journal records, and it spans from the first **dispatch** of the pending step, across any parks and resumes, to the completion that appends it: several dispatches may serve one execution, and only the completion writes. An entry carries:

- the **step**, and which execution of it this was — a step the route reaches again appends again, and the later entry's result stands as the step's current one;
- the **result** — the artifact value, or the signal observation;
- the **route elected** — the next step, or finalization with its disposition;
- the **message to the successor** — why it is being run, from the step that sent it;
- optionally, a **standing note** — addressed to every subsequent step, not only the next one;
- **who** ran it — the account, and the role it acted in — when it ran, and what it spent.

Only completion appends. A park, a skip, a failure, an interruption — none of them is a journal entry; each is recorded beside the journal, and the journal reads the same before and after it.

A step receives the journal whole: the route traveled, every prior result, the messages and notes along the way, and how many times each step on the route has run and resumed. That is the context its work arrives in — a step never reconstructs what happened from side effects, and never needs a channel outside the journal to learn it.

**A standing note informs; it never binds.** It is the one channel addressed past the immediate successor — for what later steps should know that no artifact's shape can carry. A later step reads it and decides for itself; a note that constrained the route would be control exercised at a distance, by a step no longer present to answer for it.

### Deriving the next step

The pending step is read from the journal — the route its last entry elected — and from nothing else. An empty journal pends the flow's declared entry step. Two consequences follow, and both are requirements:

- Resolution is **resumable**. Any runner, on any machine, after any interruption, derives the same pending step by reading the same journal. Nothing about position lives in process memory.
- **The journal decides; telemetry observes.** What ran and what it chose is durable state, recorded ahead of the act it authorizes. Execution telemetry — narration, notifications, timing detail — remains observational and never decides what happens next.

A step runs when the route names it, and for no other reason. There is no eligibility beside the route: no step is latched done, none is skipped by a marker, and reaching a step a second time is not an anomaly but a route — rework arrives as an ordinary election, carrying the reasons in its message.

### Blocked on items

An item may wait on other items: blockers declared on the item itself, each with its own state ([orchestrator.md](orchestrator.md) § Dependencies). Whether it waits *right now* is **derived before every dispatch, at every step, under both drive models**. Once the pending step is known and before anything is dispatched to it, the item's blockedness is read from its declared blockers and their current states, and nothing stores the answer. Selection, claiming and advancing read the same derived fact — so an item cannot be selectable and blocked at once, and a blocker landing is visible to the next read without anyone touching the item.

Blocking is not settled when the item is picked. Between any two steps, from outside the resolution, a dependency can be declared on the item after it was claimed, a blocker that was closed can be reopened, a blocker's own resolution can revert. And from within it, by a step's own work: a plan finds the work is pending elsewhere, a step files items the resolution then waits on, a later step finds that what it must build on has not landed. Every one of these is the same state, met at whatever step the route stands on.

> **An item waiting on an unfinished item is `blocked`, kind `waits-on-items`, and the advance stops clean: nothing is dispatched, nothing is spent, nothing is recorded, the claim is kept, and the pending step stays pending.**

Clean means exactly that. No agent prompt runs. Nothing is written beside the journal or in it — a park, a skip and a failure append nothing (§ The journal), and neither does this. The claim is not released: it is an arena reservation, not work, and a blocked item still belongs to the arena that holds it. The pending step is still the pending step. When the last blocker lands, the next advance runs it from where the route stood; a blocker reopened blocks the item again at the next read. Both happen without anyone touching the item, which is the whole meaning of a derived fact.

**A step may declare the blockers it finds, and stop on them.** A step whose work turns out to wait on other items — ones that already exist, or ones it filed — records them on the item as blockers and completes by stopping as blocked on them. That stop is the same state the check before dispatch produces: not a park, not a failure, not a refusal. It appends nothing, charges nothing, resolves nothing, and keeps the step's draft for the resume (§ Drafts).

The line between stopping on items and refusing is **who acts**. A refusal says no answer and no change will help, and a person decides. Blocked on items says the work exists elsewhere and will land, and nobody touches this item until it does. A step that used a refusal to say *waits on those items* would turn a self-clearing condition into one a person must clear, and spend the prompt it took to do it.

What a fleet does with an arena whose claim is held by a blocked item — release it to free the machine, or keep it so the same worktree resumes — is policy above the flow. The flow's rule is only that the stop does not break the claim.

### Routing

A step's possible routes are **declared at registration**, and the route it elects at runtime must be one of them: its declared successors, and whether it may finalize, with which dispositions. The declaration is what makes the graph a reviewable object — every route an item can take, including every handback and every role boundary, is visible before anything runs — and it is what bounds election at runtime: a handler talked into an arbitrary jump by whatever influenced it has no such route to elect.

Startup validates the graph whole: every declared successor exists, every step is reachable from the entry, and finalization is reachable from every step — a step from which no election could ever end the item is refused before any item is claimed.

This is [org/engineering-guide.md](org/engineering-guide.md) § One obvious way, projected onto resolution. Every restriction here — successors declared, roles tagged, worktree states needed and left — exists to shrink the combinations a step's author must consider: a step reached only by declared edges, in a declared state, carrying a message that says why, is written against a small closed set of situations, while a step reachable from anywhere, in any state, for any reason, must be correct in all of them and will not be. Fewer reachable situations is fewer flow bugs, and the route that exists is the one obvious one.

Every election carries its message. The successor is told why it is being run — what was found, what is expected of it, what changed since it last ran — by the step that decided, at the moment it decided. An election with an empty message hands the successor a task with no brief, and the journal a decision with no reason.

## The treasurer

Steps try to resolve the item. The **treasurer** keeps what that costs at bay. The two concerns are held by two parties deliberately, and the separation is load-bearing:

> **The treasurer is independent of what it constrains. Its policy is out of reach of the steps, the prompts, and the agent whose spending it bounds.**

The reasoning is the guard rule (§ Guards): a limit held by the party it limits is editable by that party, and an agent that can move its own bounds has none.

The treasurer keeps a **durable ledger**: time and cost, per step and for the resolution whole, the count of dispatches and resumptions of each step on the route, and **the count of agent sessions the resolution has opened**. The ledger survives interruption with the same durability as the journal, and `status` reports from it.

**The session count is checked against the journal, not against a policy.** A resolution opens one session at its entry, and one more for each **execution** of a step declaring a new one ([flow-registration.md](flow-registration.md) § Session continuity). The journal records exactly one entry per execution, so the number a resolution should have opened is read off the route it actually travelled — **not off the graph**, which may route through the same step many times and offers no static answer.

> **A resolution that has opened more sessions than its journal accounts for has paid for conversations its route never asked for. The count must be reported and an excess flagged.**

The excess is the signal, and it is very nearly the only one: nothing else about such a resolution looks wrong afterwards. What it does not say by itself is *why* — whether the machinery decided a moment was special and started over, which is a defect in the flow, or the handle was simply gone, which is a limit of the substrate or the backend (§ The agent session). Both cost the same and have different fixes, so the flag names the count and the record names the cause. The comparison is within the vantage the treasurer already has, which reads the journal and its own ledger and nothing else.

**A count that matches the journal and is still high is a routing problem, not a session one.** A route crossing a declaring step repeatedly opens a session each time, correctly; what that reports is a resolution going around, which is the runaway the treasurer detects by its own means. The two are told apart by which record disagrees — the ledger against the journal, or the journal against itself.

**The time it keeps is active time — time spent doing work — and waiting is recorded apart from it.** Cost measures what the agent spent; active time measures everything else the resolution consumes — the machine hosting it while it runs gates, commands, and git. A dispatch blocked on a declared exclusion — the serialized landing, a gate's lock, any resource the system queues work for — spends wall-clock and consumes nothing, and the two must not be confused in either direction: waiting charged as work has the treasurer refusing a resolution for being queued behind another, naming the wrong problem, while waiting dropped entirely leaves `status` unable to say where an afternoon went. So the ledger carries both, separately — active time, which the treasurer prices and bounds, and waiting time, which it reads as evidence about contention rather than about the work.

**The time allowance bounds active time.** A dispatch is never ended for being queued: waiting is not failing, and elapsed time in a queue is evidence about the queue, not about the work.

**Waiting on an exclusion is not waiting on a signal, and the ledger's waiting covers only the first.** Serialization is temporal: the dispatch is running, the claim is held, and the wait ends when the queue reaches it — nothing about the world needs to change, only its turn to arrive. A signal wait is the opposite in every dimension: a declared position in the graph, the item sitting between dispatches with nobody's move pending, ended by the world changing — a fact observed, not a turn arriving ([artifacts-and-signals.md](artifacts-and-signals.md)). One is a queue inside a dispatch; the other is a state of the item. The ledger accrues waiting only while a dispatch holds it, so a signal wait accrues nothing.

It is consulted at exactly three chokepoints, each **before** the act it governs — two before a spend, and one before a discard:

- **Every dispatch of a step.** The treasurer approves or refuses it; an approval sets the **time allowance** the dispatch runs within.
- **Every agent expense.** The treasurer allows or blocks it before it is incurred, and prices the allowance it grants — see [agent.md](agent.md) for how the allowance reaches the agent substrate.
- **Every new agent session.** Opening one discards context the resolution has already paid for (§ Nothing is bought twice), so it is asked for and recorded rather than taken. The treasurer approves it against the route: a session the journal accounts for is approved and counted, and **one that no declaration accounts for is refused before the context is discarded.**

**The third chokepoint is what makes § Nothing is bought twice enforceable rather than merely detectable.** A count reconciled afterwards says a session was opened that should not have been, once the conversation it replaced is already gone. A chokepoint answers at the moment: the machinery that decided this dispatch was special enough to start over has to say so to a party that knows what the route declared, and be told no. It is the same shape as refusing a prompt from a step that declared it would not prompt — the refusal lands before the thing it is refusing has cost anything, which is the only point at which refusing it is worth anything.

**What it refuses is a decision, not a circumstance.** A session the machinery chose to start, where the route declared none, is refused. A session that had to be opened because the handle was gone — the substrate declined it, or the backend keeps none — was nobody's decision and is approved and counted (§ The agent session).

**A refusal does not stop the resolution.** The safe direction is the cheap one: the session that was about to be discarded is kept, the dispatch proceeds on it, and the attempt is recorded. Parking would stop work over something no operator can clear, since the fix is in the graph or in the machinery rather than on the item; continuing silently would leave it invisible, which is what the record is for.

**So the count answers "how many", and the record answers "why".** An excess over what the journal accounts for says the resolution paid for conversations the graph did not ask for — which is worth knowing whether the cause was machinery starting over or a backend that keeps no handle. Which of those it was is in what each request recorded, and the two have different fixes: one is a defect in the flow, the other a limit of the backend it runs on.

**A declared new session is not the treasurer's to second-guess.** Where a step declared one, the reason is independence — a judgement that must not be coloured by the reasoning that produced what it judges ([flow-registration.md](flow-registration.md) § Session continuity) — and that is a property of the graph, not a spending decision. The treasurer records it, counts it, and approves it. What it refuses is a request the route does not account for, and what it stops is a route opening them without going anywhere, which is the runaway it detects by its own means.

What the treasurer decides from is everything the resolution has durably produced: the journal — the route traveled — and its own ledger, the per-step dispatch and resumption counts included, and the pending step. That is what makes it the party that **detects runaway**: a route cycling without its messages changing, a step resumed past reason, a resolution whose spend grows while its journal does not. Steps cannot be asked to notice this about themselves; the treasurer has exactly the vantage they lack, and stopping it is its purpose, not a side effect.

**How the treasurer decides is its own design, and deliberately not this document's.** What is required of any policy:

- **Ordinary progress is never stalled.** Admission is the default and refusal is the exception; a resolution advancing normally passes both chokepoints without waiting on anything.
- **Every refusal parks the item** (§ Parking), in the treasurer's own words: what was exhausted or detected, and what an operator can do about it.
- **An operator can extend.** A grant is an operator's instruction to the treasurer; it clears the park it satisfies, and only that park.
- **Infrastructure failures consume nothing.** A step that could not run because the environment was unavailable has not spent an attempt, and the treasurer does not count it as one. What counts as an infrastructure failure is [environment.md](environment.md).

**Spending resources must produce progress.** Every dispatch either advances the item or leaves behind something the next dispatch starts from — the work done, the answer needed, or the reason the attempt could not be recorded. A dispatch that consumes budget and leaves the item exactly as it found it has not failed once; it has established that every remaining dispatch will fail the same way, because nothing about the next attempt differs from the last.

That is the shape to check any retry against: **a retry that cannot differ from the attempt before it is not a retry, it is a loop with a budget.** It exhausts whatever the treasurer allows, reports the limit as the reason, and names the wrong problem — an operator extending the allowance would buy an identical failure.

So an attempt stopped by something correctable hands the correction back. A refused write returns to the step that produced the text, carrying what was refused and why, so the next attempt is answering something the last one did not know.

**And spending must not re-buy what the resolution already has** (§ Nothing is bought twice). The rule above read backwards: a dispatch must leave progress behind, and must start from the progress left for it. The treasurer is where the failure becomes visible — a resolution whose spend grows while its journal does not is the shape it detects — but the obligation is on the mechanisms that carry work across a stop, not on the step that finds them empty.

**A correction round is priced as a round, not as a dispatch.** A dispatch is an attempt at the step; a refused *expression* of finished work is not a failed attempt, and a treasurer that charged it as one would report exhaustion after three refused sentences — naming the wrong problem, which is exactly what this section is about. It must cost something: a correction that were free is a loop against whatever refused it, with nothing bounding it at all.

## Parking

A park records that a step stopped without completing, and why. Every park names the step it belongs to — always the pending step, since no other is running. A park is not a journal entry: the route is untouched, and when the park clears, the same step runs again, its resumption counted and the park's reason in hand.

Parks divide into two kinds, and the division is what an operator acts on:

- **Self-clearing** — the condition resolves without human action, and the item resumes when it does.
- **Human-clearing** — nothing changes until a person acts. A treasurer refusal, an unanswered question, or a condition only a human can lift.

A park that advertises a condition **stops advertising it the moment the condition ends**. A marker outliving its condition is worse than no marker, because it is read as current.

## Drafts

A step does not write its artifact while working — the result is captured once, when the step completes ([artifacts-and-signals.md](artifacts-and-signals.md) defines the capture). What a step may keep mid-flight is its **draft**: the artifact taking shape, and the working state behind it, stored where its own next dispatch finds it. That is what makes a dispatch that could not finish still produce progress: the work that led to the question, or to the refused sentence, is the expensive part, and it is exactly the part a stop would otherwise discard. A parked step resumes with its draft in hand.

The draft is **scaffolding, not a result**:

- **It does not complete the step.** An unfinished plan captured as the plan artifact would append a journal entry and route the resolution onward against a plan that was never finished — a worse outcome than losing it.
- **It decides nothing.** The route is read from the journal; a draft is read by the step that wrote it and by nothing else. It is not part of what a reviewer reads, and it does not appear in what is proposed.
- **It is keyed by item and step, and read only when both match.** Keying is the correctness property; clearing is hygiene. Every path that skips a cleanup — a crash, a kill, a lost machine, a working directory left by an abandoned run — would otherwise feed one item's reasoning to another item's agent, where it arrives indistinguishable from that agent's own thinking. A missing draft costs a re-derivation, which is the cost of not having the mechanism at all; a wrong draft costs a plan built on another item's reasoning, published under this item's number. Every ambiguity resolves toward discarding.
- **It is never published.** For a refused write the text to keep *is* the text a guard refused, so a store that could go outward is a store that cannot hold it.
- **It is cleared when the step completes**, and when the claim is released or the item finalized. Scaffolding that outlives its work becomes stale prose a later reader mistakes for a record; reasoning left behind after the work is over is a disclosure sitting around for no benefit.
- **It is optional, and the progress rule is why a parking step rarely wants to skip it.** A step that does not use it behaves exactly as one would without the mechanism — but every dispatch must leave behind something the next one starts from (§ The treasurer), and a step whose work lives nowhere durable — no commit, no tree — has only the draft to leave. Parking without one re-derives the same reasoning at full price on resume, and the treasurer counts both times.
Where the draft physically lives is the backend's: beside its claim state on a machine that holds one, or with the claim on a server, so that an arena can lose its disk without losing the record.

**A draft is not the session, and the two must not be folded together.** A draft belongs to one step, is read only when item and step both match, and is cleared when that step completes. The agent session belongs to the resolution and outlives every step on the route (§ The agent session). Keying the session like a draft would end it at the first step boundary; clearing a draft like a session would feed one step's unfinished reasoning to the next. They are stored beside each other and neither is the other's key.

## The agent session

The **session** is the conversation a resolution has with its agent. It is **the resolution's**, not a step's and not a process's: the same session carries across every step on the route, across a park and its resume, across a process exiting and another picking the item up, and across the boundary between a binary driving the whole route and one advancing a single step at a time.

> **A new session is opened only where the graph declared one, and the entry of a resolution is where its first one begins.** Which steps declare one, and why, is [flow-registration.md](flow-registration.md) § Session continuity.

It is state of the same kind as the draft and lives with it — beside the claim on a machine that holds one, or with the claim on a server — and it is subject to the same rules: it is never published, it is not part of what a reviewer reads, and it is cleared when the claim is released or the item finalized. What differs is scope and lifetime, and those differ deliberately.

**A backend that cannot store it is not incorrect, only expensive.** Where there is nowhere to keep the handle, every dispatch opens a session and every step starts from the prompt it was given — which is exactly what the best-effort rule below already requires a step to tolerate. The mechanism is absent rather than broken.

**The handle is offered, never depended on.** A substrate may decline to resume, expire the conversation, or have no such notion at all. So a resolution hands the handle over and proceeds correctly without it: the prompt and the draft are what make a dispatch right, and the handle decides only what it costs. **A step whose result differs depending on whether the substrate honoured the handle has made an optimisation load-bearing, and is wrong for that reason rather than for an expensive one.**

**A handle that is unavailable is not a new session being declared.** A substrate may decline one; a backend may have nowhere to keep one. In both the conversation ended somewhere the system did not choose, and what follows is the best-effort clause above doing its job — not a decision to start over, and so not what the treasurer's third chokepoint refuses (§ The treasurer).

> **The distinction is who decided.** A resolution that chose to discard its context must have declared it. A resolution whose context was taken from it carries on, and says so.

## Questions

A step that needs a human decision asks a question and parks. It asks **in the open**, where the humans already are — an answer from anyone counts, not only from whoever filed the item.

Answering does not resume the item. Resumption is a separate deliberate act, because somebody has to judge the answer complete.

Re-running an item with an unanswered question consumes no budget and runs no agent prompt. The check happens before dispatch, not inside it.

## Gates

A **gate** measures something. Every gate holds four properties, and they are what make one worth trusting:

- **It measures; it never modifies what it measures — including afterwards.** A gate may write elsewhere, a build cache or a report, but the subject it reports on is exactly as it found it and exactly as it leaves it. Measuring faithfully and then tidying up is not a gate: a producing step asks one mid-work, and cleaning behind the answer discards the work the step is in the middle of.
- **It reports what it measured, and does not judge it.** Coverage, size, duration, failure counts — numbers, not a verdict. Whether those numbers are acceptable is decided elsewhere, against thresholds the gate does not hold.
- **Its measurement is complete, or says why it is not.** A run that skipped a suite reports honest numbers that understate what was checked, which is indistinguishable from a regression unless the run says so.
- **It is reproducible.** The same subject gives the same measurement to anyone who runs it, anywhere.

### A gate does not decide

This is the property most easily lost, and losing it is expensive.

The thresholds a measurement is judged against — a coverage floor, a size limit, a baseline that ratchets — are not the gate's. They belong to whoever decides, and they are deliberately **out of reach of whatever the gate is measuring**. A gate that carries its own thresholds can be made to pass by editing the gate, and when the thing being measured is a change written by an agent, the agent can edit it.

So `test_failures: 3` is not a verdict. It is a pass or a failure depending on state the gate does not have and must not be given.

**A gate also does not report whether it finished.** A process killed for memory, or truncated mid-write, is not alive to say so — and one that exits cleanly having measured nothing can say something false. What became of a run is the account of whatever spawned it, which is a third party: the gate measures, a runner observes, and the layer holding the thresholds judges. [gates-and-commands.md](gates-and-commands.md) states the contract.

### Reproducibility has two halves, and one rule covers both

**The measurement half** is why a gate is a program rather than a script. A script inherits whatever the environment hands it — user configuration, path differences, shell dialects — so two hosts can disagree about a textually identical gate for reasons that have nothing to do with the subject.

**The judgement half** is the same requirement applied to what the measurement is compared against: **what a verdict depends on must be a function of the subject.** A threshold that moves on its own schedule fails this — the same tree is judged one way today and another way next month, and neither answer is about the tree. Checking out an old commit and judging it against a threshold that has moved since answers a question about neither.

Note what this does *not* say. A threshold versioned with the tree is the one place it cannot vary: it moves when the subject moves, so a commit carries the terms it was judged on and any machine reaches the same verdict offline. Being *near* the gate is not the problem — being independent of the subject is.

That is why "a gate does not judge" is a separate rule with a separate reason, and not a corollary of this one. It is the artifact rule: the party under judgement must not hold what judges it.

The non-modification rule is what makes any of it repeatable. A measurement that changes its subject cannot be repeated: run it twice and the second answer is about a different thing than the first.

**Gates differ only in what they measure and when.** They are one mechanism, not a family of similar ones:

| Measures | Runs |
|---|---|
| A tree, or a merge result | At the decision to integrate |
| What a step actually did, against what it said it would | After the step |
| Whether a machine is fit to be given work | Before work is given |

Every one of them measures something that already exists, and reports on it.

The third row is the only one whose subject is not the work. What makes a machine fit, and what follows from an answer of unfit, is [environment.md](environment.md).

## Guards

A **guard** is not a gate. It stands on the execution path of an act that has not happened yet, and **prevents it**.

| | Gate | Guard |
|---|---|---|
| **Subject** | **Something that persists independently of the check** | **The act itself, which exists only as a proposal** |
| Position | Beside the work. It observes | On the path. The act cannot occur without passing it |
| Answers | A measurement, judged elsewhere | Allow or refuse, decided there and then |
| Its answer | Is stored, compared across runs, re-judged later | Is consumed once, by the caller about to act |

**The subject is the distinction; everything else follows from it.** A gate's answer is storable and judged elsewhere *because there is something for it to be about afterwards*. A guard's answer is consumed immediately and never stored *because after the decision there is nothing left for it to be about* — the act either happened or it did not.

Prevention is **not** the test, and using it as one misclassifies the ordinary case. A gate that blocks a push prevents something: remove it and the push happens. It is still a gate, because its subject is a tree that exists, its answer is a measurement, and that measurement is judged against a baseline later. Whether a verdict blocks something is a fact about what consumes it, not about what it is.

Two guards exist in a resolution: one over the actions an agent proposes to take, and one over what the flow publishes ([disclosure.md](disclosure.md)).

**A guard decides for itself, where a gate must not.** Judging is kept out of a gate because its measurement is re-judged later, so the comparison has to be recomputable by someone who was not there — which is why thresholds are a separate artifact. A guard has no persistent subject to re-judge, so there is no second judgement for separation to keep honest.

**A guard and a gate over the same concern are complementary, and neither substitutes for the other.** A guard on a proposed command is the cheapest place to stop a violation — the agent learns the constraint while it works and adapts, rather than losing the whole prompt. A gate on the result catches it however it happened, including by routes nobody anticipated. The guard fails open when it is absent or bypassed; the gate costs a whole prompt before it speaks.

**A guard must not be authored by the party it constrains**, and this holds for every guard rather than only the ones whose refusals are irreversible.

The reason is that a guard leaves no review window. A weakened gate is caught by review before its answer authorises anything — the measurement persists, and a wrong one can be recomputed and contradicted. **A guard weakened at one step authorises the next step immediately**, before any review exists, and leaves no trace: a run in which the guard was bypassed looks exactly like a run in which it had nothing to refuse.

So a guard's rules come from outside the tree it constrains. Where a resolution runs under an orchestrator, that is the arena applying rules from a companion repository; where it runs standalone, the guard is part of the flow, which is delivered from outside the tree it resolves. A guard configured from inside the worktree is one an `implement` step can edit, and an agent that can edit its own bounds has none.

**The action guard's refusals are layered, and layers only narrow.** What it refuses during a dispatch is the union of every layer that applies: the general rules binding any resolution here, the acting role's restrictions, and the running step's own — a step declared to write no files has a file write refused as it is attempted, in that step's name, not merely caught after the prompt. No layer widens another: a step's declaration cannot grant what the general rules forbid, and a role's cannot lift a step's. The step and role layers are derived from the flow's declarations, which are legitimate guard sources under the authorship rule above — the flow arrives from outside the tree it resolves, and no step can edit its own registration mid-run.

**This is a property, not a machinery.** What defines the gates, schedules them, records their measurements, holds the thresholds those measurements are judged against and decides what a failure means for the work queue belongs to whatever schedules work — not to this SDK. What is stated here is what any of them must be.

## Commands

A **command** does work. It may modify anything it is pointed at, and it **may run gates as part of doing its job**.

**Anyone runs them.** A step invokes a command mechanically; an agent runs one while it works to see whether what it has written holds together; a person runs one at a terminal. The same command does the same thing for all three, which is what lets an agent check its own work exactly the way the developer reviewing it will — and what makes a project's own tooling the flow's tooling, rather than the flow needing a parallel set of its own.

The same is true of gates, and there it is the point rather than a convenience: a gate that gave a different answer to the person who ran it than to the step that ran it would not be reproducible, and reproducibility is most of what a gate is for.

Commands and gates are the two kinds of thing that get run, and the difference is not how they are implemented but what may be concluded from them:

| | Gate | Command |
|---|---|---|
| Modifies | never | freely |
| Produces | a measurement | whatever work it did |
| A decision may rest on it | yes | no |

The last row is the whole point. A command's result cannot support a decision about the thing it ran against, because it changed that thing on the way. Asking "did this pass" of a command that repaired what was failing gets an answer about a state that did not exist when the question was asked.

A gate's measurement *may* support a decision — but it is not itself the decision. Something else holds the thresholds and reaches the verdict; see [gates-and-commands.md](gates-and-commands.md).

**The rule constrains the caller, not the tool.** A repairing command that repairs is behaving correctly; the defect is in what was asked. That is why this cannot be caught by checking tools for misbehaviour — there is nothing misbehaving to catch, and the wrong answer arrives as a green result rather than as an error.

**Formatting is the clearest pair.** `format` is a command: it rewrites the source. *"Is this correctly formatted"* is a gate: it reports and changes nothing. Same subject, same underlying rules, and the two are not interchangeable — one repairs, and only the other can be cited.

Most concerns worth checking have both, and building a new one is usually a question of which you need rather than what to write.

**The verify command composes them.** It formats, applies the other fixes that have one correct answer, and then runs the gate — so it modifies, and anything running it re-reads worktree state afterwards rather than assuming the tree is unchanged.

That composition is exactly what a developer wants: repair what is mechanical, then measure what is left. It is also exactly what a decision cannot rest on, which is why integration runs the gate directly rather than the command that wraps it.

A step should not fail over a misplaced brace it can fix. That is what commands are for. Whether something may be integrated is a decision. That is what gates are for.

### Which is required where

**Integration requires the gate.** That is the point where the measurement must be reproducible by someone who was not there: a reviewer, a later bisect, a rebuild on a different machine. A verify run that repaired something on its way to a pass says the tree passes *after being changed*, which is a different claim.

**Producing steps may run either**, and generally want verify — they are working, not deciding, and the repairs are the point.

### One value, every consumer

Whichever a project configures, it is configured **once** and reaches everything that needs it: the gate that must pass, and the prompt that tells an agent what to satisfy. Two settings that both mean "the check" will disagree, and the failure is silent — the agent runs one and it passes, the gate runs another and it cannot.

## Steps and the worktree

A step declares its worktree contract, and the declaration is explicit. A step whose product is a report does not acquire the ability to edit by default or by omission.

The contract has three parts, each enforced at its own moment: the state the step **needs**, established mechanically before dispatch — the worktree is put there, checked out from durable state rather than trusted to be current, and a state that cannot be established blocks the item naming what is missing; what the step **may do** while it runs — a layer of the action guard, refusing the rest as it is attempted (§ Guards) — and checked afterwards against what actually happened; and the state it must **leave**, verified before its result is captured — a step that ends off its declared branch, or over a dirty tree, has not completed: nothing journals, and the changes stay where they are for a person to judge.

> **A step runs only in its declared worktree state, and completes only into its declared worktree state.**

What this buys is a single expressible branching story. The route's worktree sequence is declared step by step at registration, so there are not several ways for work to reach finalization: a producing step cannot run on the base branch, work cannot end half-committed, and nothing arrives at the proposal scattered between a mainline commit here and an abandoned branch there. An agent's locally reasonable improvisation — cutting its own branch, committing to the base, leaving the tree dirty for later — is refused at the first declared boundary it violates, named as that step's violation rather than discovered downstream as a branch nobody expected.

**Every modification a step makes is either recorded or refused.** A flow does not leave a step's changes uncommitted for a later step to sweep up, and does not complete an item while carrying changes no step recorded. Silent loss and silent inclusion are the same defect seen from two sides.

### The commit contract

**A step commits the tree whole.** Every file the tree carries — everything not already ignored — is staged, and the commit captures all of them or it is rejected. A half-staged tree, some files in and the rest left behind, is not a commit this contract permits: a commit of part of the tree is not the tree the gate measured, so a passing gate would certify a state that never lands.

**So nothing uncommittable may be in the tree.** Not as a third tolerated state beside committed and ignored — this is what makes committing the tree whole safe rather than merely conventional. While such a file is present — a build artifact a repository guard refuses, anything a commit hook rejects — no step can satisfy the requirement above.

**The remedy is deletion.** Not leaving the file uncommitted, and not marking it ignored. An ignore rule that predates the failure is a different thing — build output a project has always excluded is in a settled state, and the contract is already satisfied. What is forbidden is reaching for the ignore list *because* something was refused. The three moves are not stylistic variants:

| Move | Effect |
|---|---|
| **Delete it** | The tree becomes committable. The failure ends. |
| Leave it uncommitted | The file survives; the next step commits the tree whole and is refused identically |
| Mark it ignored | The file survives and stops being reported |

Ignoring is the worst of the three and reads as the most helpful. The gate measures the **working tree**, so it measures a tree containing the file and passes; what lands never contains it, and breaks. A loud, local, immediate failure becomes a silent one that surfaces later, on somebody else's change. Whatever repair a flow offers must name deletion and must not name the other two: an error that volunteers "or ignore it" is teaching the failing move.

**A refusal returns to the step that caused it, in the refusing tool's own words.** The refusal names the offending file and the remedy precisely; a step told only that committing failed has to rediscover both. This is the path a failing gate already takes.

**A refusal that survives repair parks, and costs nothing.** Once retrying is known to be pointless — the same tree, committed again, refused identically — the step parks carrying the refusal's own message, rather than spending dispatches on a repetition it cannot change.

### Duplicate-fix conflicts during rebase

When a rebase conflict exists because the mainline already landed the same fix our branch carries — two independent spellings of one repair — integrating both sides produces a file that differs from the mainline's version permanently. The same conflict returns on the very next rebase, and the one after that. No retry clears it.

The correct resolution is to accept the mainline's version and drop the branch's redundant change. The rebase commit message must record what was dropped and the upstream commit that supersedes it, so the decision is auditable.

**Evidence test (mid-rebase):** run `git log --oneline -5 HEAD -- <file>`. During a rebase `HEAD` is the mainline side. If an upstream commit in that list applies what our change applies, the conflict is a duplicate fix. If a `DUPLICATE-WORK CANDIDATES` block is present in the rebase output, it lists exactly these files and commits and serves as a shortcut; its absence does not change the rule.

**Default:** absent clear evidence that the conflict is a duplicate fix, integrate both sides — the standard conflict-resolution rule applies. The carve-out is not licence to discard work; it gates on objective evidence that the work already landed.

## Finalizing

Finalizing marks an item's resolution complete and releases the claim. It is terminal: a finalized item is not reprocessed.

Only a step finalizes, by electing it as its route, and the election carries a **disposition** — the set is closed at two:

| Disposition | Means |
|---|---|
| **resolved** | The work was done. |
| **rejected** | The item was reviewed and declined, with the reasons in the finalizing entry's message. |

Rejection is terminal in a way a handback is not. A route that returns work for rework expects the resolution to continue; a rejection ends it, and the difference is a decision the finalizing step makes in the open, with its reasons recorded where every decision is.

An item that no flow will act on — because no flow accepts its type — is not finalized on that basis. Reporting success for work that was never attempted hides a misconfiguration, and doing it terminally makes the misconfiguration irreversible.

Such an item is reported `blocked`, naming the type it carries and the types that are registered: nothing failed and no later cycle will pass, so what clears it is a person — a flow registered for that type, or a corrected type on the item.

## Every outcome leads somewhere

**No step ends in a dead end.** Whatever a step concludes, there is a path from it to a terminal state — the item resolves, or a person acts and it continues.

A step that cannot complete ends in exactly one of:

| Outcome | Means | Cleared by |
|---|---|---|
| **Transient failure** | Something outside the work went wrong | Retrying |
| **Invalidates earlier work** | An earlier step's result is wrong; the route returns to that step, carrying why | The flow itself, as an ordinary election |
| **Waits on a person** | A question, a decision, a permission | An answer |
| **Waits on something else** | Another item, an external condition | That condition clearing |

"Error" is not among them. A step that simply stops, with no route onward and nothing named that would unstick it, leaves an item nobody can finish and nobody can close — and it will sit that way indefinitely, because nothing is watching for it.

That is why every stopping outcome names what would clear it. The name is not documentation; it is the difference between a state someone can act on and one that is merely inert.

## Reporting

Every invocation reports exactly one status: `done`, `skipped`, `parked`, `blocked`, or `failed`. These five are the vocabulary, and anything mirroring them mirrors all five.

`skipped` is the status of an invocation that stopped **before any dispatch**: a pre-dispatch check found nothing runnable — an unanswered question, a manual hold — so nothing ran, nothing was spent, and the item reads exactly as it did. No handler can produce it: a handler that has run has been dispatched, and a dispatched step's stop is one of the named outcomes (§ Every outcome leads somewhere), never a bare "no progress".

A `blocked` report that stopped on the item's own blockedness names the block kind, and for `waits-on-items` it names the blockers **as references** — the declared items, each with its own state — and the pending step it will resume at, never only prose about them (§ Blocked on items).

The report says what happened to **one step**, not to the item. An item's overall state is derived from its journal, never from the last report.
