# Trunk health

> **Tag:** `trunk-health` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** What it means for a trunk commit to be healthy, how that fact is established, recorded and read, and what a step does when the gate it runs comes back red. Every statement is a requirement. Where the code does not satisfy one, an issue is open against it. Nothing here records progress, status, or history.

[gates-and-commands.md](gates-and-commands.md) § A green `verify` blesses the tree it verified defines the evidence this document distributes: a green run completed over exactly this content. That fact is tree-scoped and local — it lives in one checkout and says nothing about any other. This document is the same evidence at a different scope: one commit, recorded where arenas that did not produce it can read it.

## Trunk health is a property of a commit

> **Trunk health is a property of a commit, never of a branch.** A commit that measured green is green for as long as it exists, and the trunk's tip having moved since says nothing about it.

The blessing is about content and never about time, and health inherits that unchanged. What it adds is an identity a second arena can name: a tree id is meaningful only to whoever holds that tree, where a commit is what every arena was dispatched onto and what every fix lands as.

> **The trunk is the branch changes are integrated into. The base is the trunk commit an arena was dispatched onto.**

They are different questions and only one of them is answerable. The trunk's tip is wherever the last push left it — possibly a commit nothing has measured, possibly a commit that appeared after this arena started — and it is not the commit this arena's work sits on. So a step never asks whether the trunk is healthy. It asks about its base, and what it reads is that one commit's health.

Keeping them apart is what makes the record stable under a moving branch: an answer about a commit stays true while the branch runs on ahead of it, where an answer about "the trunk" is stale the moment it is recorded.

> **An arena whose base is not a trunk commit has no trunk health to read, and reads `unknown`.**

An item's base branch need not be the trunk ([orchestrator.md](orchestrator.md) § Worktree, `CutPoint`), and what is recorded here is recorded for trunk commits. `unknown` is the honest answer rather than a hole in the record: nothing has measured that base, which is precisely what the state says, and § The three states already fixes what a step does with it.

### A verdict names the platform it was reached on

> **A verdict is about a commit on a platform, and names both.** The platform is spelled in the placement axes [environment.md](environment.md) § Placement closes — `os` and `arch` — and never in a vocabulary of this document's own.

A commit measured green on one platform and never measured at all on another is the ordinary case, not an exotic one. A record keyed to the commit alone reports the first answer to an arena asking the second, and that arena concludes its base is green and then cannot explain the gate it just watched go red. That is case (3) misread as case (2), which is the skew this document exists to remove — so the platform is part of the key rather than a note beside it.

It follows that `unknown` is per platform too: a commit can be green on one and unknown on another at the same instant, and both answers are complete.

A project whose work runs on one platform carries one value in the key and pays nothing for it. A project whose work runs on several cannot omit it without being wrong.

## A red gate is three things, and only one of them is the step's

A step runs the project's gate over its own work and it comes back red. Exactly three things can be true:

| | What is true | Whose it is |
|---|---|---|
| 1 | The change under construction broke it | **The step's.** Ordinary work — fix it and run again. |
| 2 | The base is red, and every arena dispatched onto it meets this | Whoever fixes the base. Nothing this step does to its own change will clear it. |
| 3 | The base is green and this arena cannot progress | [environment.md](environment.md)'s — an environment condition, a property of the machine or of the delivered tree and never of the work. |

**Telling them apart is not a matter of reading the failure.** The three produce identical evidence inside one arena: a gate, red, over a tree that contains both the base and the change. What separates them is what the *same base* does elsewhere, which no single arena can see.

**Guessing skews, and it skews the expensive way.** A failure that looks unrelated to the change gets called pre-existing, the step proceeds as though the base were at fault, and the work is proposed on top of a defect it introduced. The opposite error is cheaper and rarer: a step that blames itself for a red base spends its budget failing to fix something it did not break, and says so.

## The classification is mechanical

> **Nothing an agent concludes is evidence of trunk health.** The classification is a measurement, reproducible by anyone who runs it, and no agent participates in reaching it in any form.

**Where it runs is deliberately not fixed here.** An arena that happens to be idle, the check that admitted the commit, a job the backend schedules — each is a legitimate place to run it, and this document requires none of them. What it requires is that the answer is a measurement and not a judgement, because the record is read by parties that never met and a verdict they cannot reproduce is one they cannot compare.

That is also why the rule is stated about the answer rather than about the party. *The orchestrator classifies, the agent does not* would be an arrangement, and arrangements admit exceptions. *The answer is mechanical* admits none: an agent's conclusion fails the requirement wherever it is reached and whoever asked for it, including when it is correct.

**An agent that diagnoses one of these correctly has still not produced evidence.** It has produced reasoning, which is worth having — it belongs in what the step reports (§ What a step reports). It is not what the record is written from.

### The techniques an agent would reach for are already forbidden

The prohibition is not merely about reproducibility. Both ways an agent could gather the evidence locally are refused by rules that exist for their own reasons, so a step attempting either is breaking something else on the way:

- **Re-running the gate on the base commit** means moving the work out of the worktree. The worktree is where the arena's un-committed state lives and the only record a resumed step reads ([orchestrator.md](orchestrator.md) § The binding is durable, because the state lives in the arena), so a stop while the work sits aside loses it, or buys it a second time on resume — which [resolution.md](resolution.md) § Nothing is bought twice forbids.
- **Cloning the base elsewhere and measuring there** means writing outside the checkout, which [gates-and-commands.md](gates-and-commands.md) § The worktree is the default boundary refuses outright: an agent's edits are confined to the worktree it is working in.

A step with no admissible technique and no input has only a guess, and § A red gate is three things says which way the guess goes. The input is what this document supplies.

## The three states

> **`green`, `red` and `unknown`, and each names the commit and the platform it is about.**

| State | Means |
|---|---|
| `green` | A run over exactly this commit's content, on this platform, whose outcome was a measurement and whose verdict was acceptable. |
| `red` | The same run, its verdict not acceptable. |
| `unknown` | No such verdict is on record — including where a run was attempted and produced no measurement. |

> **Only a judged measurement records a state. A run that measured nothing records nothing.**

A gate does not pass or fail. It measures, a runner observes what became of it, and the judge decides against thresholds the gate does not hold ([resolution.md](resolution.md) § A gate does not decide) — so a verdict here is a `GateVerdict` over a `GateRun`, never a reading of how a process exited. Four of the five outcomes are not measurements at all: timed out, could not start, died, broke its contract.

**The dangerous direction is the second one.** [orchestrator.md](orchestrator.md) § `GateRun` already keeps *no answer* from being read as a passing one; here the symmetric error costs more. An evaluation that timed out, recorded as `red`, stops every arena dispatched onto that base until a person notices — the fleet-wide form of exactly what `unknown` exists to prevent.

> **Absent evidence is `unknown`, and `unknown` is never read as `green`.**

A fleet that reports fine while every arena is stuck is what reading absence as health produces, and it is the failure this state exists to prevent. The requirement is symmetric and worth stating in both directions: `unknown` is not a weak `green` and it is not a weak `red`. It is the honest answer that no measurement exists, and a step handed it knows that its base has been established as neither.

**The states correspond to the blessing's, and there is no fourth.** [gates-and-commands.md](gates-and-commands.md) fixes the record's states as *blesses tree X* against *anything else*, deliberately collapsing absent, blank and unreadable because the only safe response to all three is the same. `unknown` is that collapsed answer carried to commit scope, and it collapses for the same reason: a base nobody measured and a base whose measurement is in flight are different histories, and a step meeting a red gate can do exactly one thing about either.

## Where a green verdict comes from

The measurement that establishes a commit green can be bought twice or once, and the difference is a real trade rather than an implementation detail. Both are admissible.

> **A green verdict is either a run over the commit alone in a clean arena, or the landing measurement that admitted the commit. A project declares which it records, and does not decide it per commit.**

| Source | What it costs | What it trades |
|---|---|---|
| **A run over the commit alone**, in an arena holding nothing else | A full `integration` run per commit measured, on each platform | Nothing. The tree holds the commit and no other item's state. |
| **The landing measurement, reused** | Nothing — it has already run | The arena it ran in was not clean. |

**The reuse is not a shortcut around the gate; it is the same gate, already run.** [orchestrator.md](orchestrator.md) § `verify` and `integration` requires `integration` to pass over the merge result before anything is integrated into trunk — the tree that is about to become the commit. A verdict over that tree is a verdict over the commit's content, and asking for it again on the same platform buys a second answer to a question already answered.

**The saving is one full run per push, and on a single-platform project that is half of them.** Measured separately, every commit is gated twice: once by the arena landing it, and once by whatever establishes the trunk. Where the platforms coincide the second run measures the identical content on identical hardware and can only agree.

**What reuse trades is the cleanliness of the arena, and the risk is small rather than absent.** A gate modifies nothing it measures ([resolution.md](resolution.md) § Gates), so in principle the landing arena's leftover state — build outputs, caches, anything a previous command left behind — is outside what the measurement is about. In practice a gate carrying a defect can let exactly that state mask a failure that a clean tree would have surfaced, and a reused verdict inherits the defect along with the answer. A project accepting the cheaper source is accepting that, knowingly; the safer source exists because for some projects it is not worth accepting.

**It is declared, not chosen per commit.** A record whose provenance varies from one commit to the next cannot be compared across commits, and comparing is what a record read by parties that never met is for. A project that changes which source it records changes it for the record, not for a commit it has doubts about.

### Reuse is a source of `green` only, and that is not the same as never reaching `red`

A landing measurement that came back red admitted nothing — the commit does not exist to have health. So reuse produces one state, and a project recording only reused verdicts holds `green` for what landed through the flow and `unknown` for everything else: a commit a person pushed, a merge made through the backend's own interface, a revert applied directly.

**`red` enters the record from a measurement over a commit already on the trunk**, and that does not require a job built for it. The arena that meets an `unknown` base and measures it before starting work (§ When the question is asked) is running exactly that measurement, in exactly the clean tree the safer source calls for — so a project recording reused verdicts and asking early reaches `red` routinely, paid for by whichever arena happened to arrive first.

**The combination that cannot reach `red` is reuse together with asking late.** There the record only ever grows from landings, no arena measures a base on its own, and an operator learns the trunk is broken by reading block reports from several arenas rather than from the record. That is a coherent and cheap choice, and it is the weakest one this document admits; a project making it should know it is making it.

### The verdict is keyed to what landed, not to what was measured

> **A verdict names the tree it was reached over, and is recorded against a commit only when that commit's tree is that tree.**

The reused measurement is taken over a *simulated* merge, **before** the merge. Between the measurement and the landing, two things can move what lands: a squash rewrites the commit, and a trunk that advances in the meantime changes the merge result itself. A verdict recorded against whatever commit appeared afterwards is a verdict about content nobody measured.

Expressed as a tree id the question is decidable rather than a matter of trusting that nothing moved: **a squash changes the commit and not the tree, and a trunk that moved changes both.** [gates-and-commands.md](gates-and-commands.md) already has the primitive — a bless that applies only when the tree equals a given id, so a party holding a green result over tree X records it without ever blessing different content — and recording someone else's verdict is exactly what it is for. The guard against recording a verdict for content that has since changed is that guard, not a second one written here.

Where the ids differ, nothing is recorded and the commit is `unknown`. That is the honest answer: a measurement exists, and it is not about this commit.
## When the question is asked

Establishing a commit's health and consulting it are separate decisions, and the second has two placements. Both are admissible.

> **A resolution may ask about its base before it begins, or on the first red gate. The arena can answer for itself only in the first.**

**Asked before it begins** — as a precondition of starting, or as the resolution's first step — the base's health is an input to the work rather than a rescue from it. An `unknown` base is measured then and there, and the arena is the right place to measure it: at that moment it holds nothing but the base, clean and on the base branch, because [orchestrator.md](orchestrator.md) § Claiming refuses a claim over a stale base and refuses a release that leaves a worktree dirty or HEAD off the base. So the clean-arena source of § Where a green verdict comes from is available without a second arena, and the answer it produces is recorded for every other arena on that commit.

A base that measures red stops the resolution before a prompt is bought, which is the cheapest moment to stop.

**Asked on the first red gate**, the ordinary path costs nothing: where the base is fine the question never arises, and most bases are fine. What it gives up is the arena. By the time the gate has gone red the worktree holds the change, and § The techniques an agent would reach for forbids both ways of putting the base back into it — so the answer must come from the record or from somewhere that is not this arena, and an `unknown` on the record is an `unknown` this arena cannot improve. The step reports what it observed and blocks (§ A block is reported, not asked).

**The trade is one gate run against one lost opportunity.** Asking early pays an `integration` run at the start of every resolution whose base is `unknown`, and a multi-platform project meets that often — a commit green on the platform it landed from is `unknown` on every other, so in practice it is a run before most resolutions on every platform but one. Asking late pays nothing until it pays the whole thing: arriving at the question at the one moment the arena can no longer answer it.

How often a project pays the early cost is set by what it records, not by which placement it picks. Reuse (§ Where a green verdict comes from) is what makes a base usually `green` on the platform it landed from; a project recording reused verdicts asks early and rarely measures, and a project recording none measures nearly every time.
## An evaluation in progress is a lock, not a state

> **A measurement running against a commit is a lock, never a record. A commit with an evaluation in flight is `unknown`, exactly as one with nothing running against it.**

**An evaluation is an action, and actions die.** A process killed, a machine lost, an arena that went away — each leaves the work undone and nothing to notice it. A stored *in progress* survives all of them, as a state whose subject has vanished and which nothing will ever clear, and the next reader meets a commit that is permanently neither measured nor measurable. Absence has none of that failure mode: it is already the right answer, and it is self-correcting.

So there is no fourth state. *A measurement is running* is a true fact, worth reporting to a person watching a run — it is not a fact about the commit, and § The three states stays closed at three.

> **The lock is keyed as the record is keyed: the commit and the platform, within the project whose record it is.**

They are one question in two tenses — who is answering it, and what the answer was — so they key alike, and one evaluation runs per key. A project's record is its own, so the project is the store rather than a field in it; a lock held somewhere serving several projects names the project too, for the ordinary reason that it has to say which store it is standing in front of. A second arena meeting the same unmeasured base waits for that answer or proceeds without one; what it does not do is buy the identical measurement a second time. [resolution.md](resolution.md) § Nothing is bought twice is the principle, and this is that principle reaching past a single resolution — the section already names the fleet's wall-clock as one of the places the cost lands.

**The lock is an optimisation; the record is the correctness.** A lock that dies costs one duplicated run. A pending state stored where the record lives would cost an answer, and cost it permanently. Every ambiguity about a lock resolves toward releasing it.

**Serializing for hardware is a different concern with a different key.** A project whose evaluations saturate a machine serializes them for that reason too, and that is host scope ([gates-and-commands.md](gates-and-commands.md) § Two scopes, for two different reasons). The two do not substitute: evaluations of *different* commits contend for hardware without duplicating anything, and evaluations of the *same* commit duplicate without necessarily contending. A backend may need both locks, and it needs them for unrelated reasons.

## How a base is established red

> **A base is `red` when a mechanical run over exactly that commit's content, on that platform, was measured and judged not acceptable. Nothing else records `red`.**

§ The classification is mechanical forbids the conclusion; this is the measurement that replaces it. There is one, and § Where a green verdict comes from names where it comes from: only the clean-arena source produces it, because a landing measurement that came back red admitted no commit to be red about.

> **A run on an unfit machine records nothing, in either direction.**

[environment.md](environment.md) § Nothing is concluded from a failure observed on an unfit machine closes with *it does not move a baseline*, and a trunk verdict is a baseline every arena on that commit reads. Fitness is established before the evaluation rather than reasoned about after it — which is the ordinary order, since `fit` runs before work is given.

### A rebase comparison is evidence for a report, never a verdict

An arena that blessed its own tree, rebased onto a new base, and watched the gate go red holds a controlled comparison — same arena, same machine, same change, and the base as the only thing that moved. It is mechanical, no agent reasons about it, and both measurements are already paid for.

**What it isolates is the base together with its interaction with the change, which is not the base.** A change sound on the old base can be unsound on the new one without the new one being unsound: the two touch the same thing and the combination fails where neither does alone — [resolution.md](resolution.md) § Duplicate-fix conflicts during rebase is that case, named already.

So the comparison earns its place in what a step reports (§ What a step reports): it establishes that the change in isolation was sound, and names the rebase as where the failure entered. What it may not do is record `red` against the commit, because a base published as broken on the strength of an interaction sends every other arena to look for a defect that is not there — the cost § A red gate is three things attaches to guessing, paid out to a fleet instead of to one arena.

## Health is delivered, never derived

> **A step receives the base it was dispatched onto and that base's health as inputs. It derives neither.**

Both are already held by the party that dispatched the step: the base is the commit the arena was cut from, and the health is a record to be read rather than a measurement to be taken. A step computing either would be re-deriving what it was given, and § The techniques an agent would reach for is what it would have to break to do it.

> **The input is present whatever the answer.** `unknown` is delivered as `unknown`, never as an absent field.

An absent field is read as nothing by a careful reader and as `green` by every other, which is § The three states' requirement lost at the last hop. A step that is told nothing about its base and a step told its base is unmeasured behave differently, and only the second is being told the truth.

## What a step reports

> **A step reports what it observed. It does not report whose fault it is.**

What it observed is the gate it ran, over what content, on what platform, against what base, and what that base's health was when it was handed over. Where the arena holds the comparison of § A rebase comparison, that too — as an observation, with what it isolates stated as narrowly as it actually isolates it.

**An agent's diagnosis accompanies the observation and is never the observation.** A step that works out exactly what is wrong has produced something worth reading and worth keeping, and § The classification is mechanical is not a rule against thinking — it is a rule about what the record is written from. The diagnosis goes in the report, where a person reads it. It does not become a verdict, and nothing downstream branches on it.

### An arena-local verdict is about the arena, and generalizes to nothing

> **An arena reporting that it cannot progress states that about itself, and about no commit.**

This is case (3), and the name is the whole of it: the verdict is *local*. It is not recorded against the base, it is not recorded on the item, it does not travel to another arena, and it is not evidence that anything is wrong with the content — [environment.md](environment.md) § What an environment condition is decides that with two questions, and both of them need a machine this arena is not.

> **It is reported and parked on nothing.** An environment condition is reported as the invocation status `blocked` ([environment.md](environment.md) § The condition is reported, not recorded on the item), which is not the same act as parking and must not become one.

That document gives the reason and it is decisive: a marker for a condition scoped to *this arena* travels with the item to an arena where it is false, and is read there as current. The one environment condition that does park — an exhausted agent allowance — earns it by being scoped to an account rather than to a machine, so its marker cannot lie anywhere; a condition local to one arena has no such licence, and being discovered mid-work does not supply one.

Nothing is lost by parking nothing. The claim stays with the arena, the worktree keeps the work, and the draft keeps the reasoning — all of which § Unfit is a wait already holds — and the arena is where a resumption happens in any case.

What the report is, is the honest end of the narrowing. A `green` base and a red gate establish that the base is not at fault, which leaves the change or the arena, and no admissible technique inside one arena separates those two. So the step says so — the base was green, the gate was red, and here is what it ran — and stops. Reporting a narrowing as a narrowing is what lets a party that *can* see two arenas finish it; reporting it as a conclusion is the skew § A red gate is three things describes.

## A block is reported, not asked

> **A block is not a question. No answer unblocks a red trunk.**

A step whose base is `red`, or whose base is `unknown` where it is in no position to measure it, has nothing left to do and nothing for anyone to decide. It parks, naming what would clear it.

> **The kind is `condition-unmet`: a park that no re-dispatch cures and that no person has to lift.**

Neither condition fits the kinds that existed before it. Re-dispatching against an unchanged record and an unchanged trunk reproduces the stop exactly, so this is not one of the kinds a retry cures. The clearing condition is a predicate over the record and the trunk rather than an instant, so it is not the kind that publishes when it returns. And a kind meaning *a person must act* gives an operator the one instruction that is wrong here — there is nothing for them to do, and the item resumes when a commit lands that is not theirs to write. [orchestrator.md](orchestrator.md) § Vocabularies defines the kind; that it maps to a `BlockKind` of `waits-on-condition` is what it was missing.

Routing it through the question channel instead is wrong three times over, and the worked cases are all three at once:

- **It asks for a decision where there is none to make.** A question with one possible answer is not a question; it is a report wearing a channel that demands a person before it will move.
- **It holds up everything behind it.** An unanswered question is checked before dispatch ([resolution.md](resolution.md) § Questions), so an item parked on a question nobody can meaningfully answer blocks its own re-dispatch and anything sequenced after it.
- **The answer clears the question and not the condition.** The flow resumes into the identical stop, which is the inert outcome [resolution.md](resolution.md) § Every outcome leads somewhere forbids: a state naming nothing that would clear it, re-entered.

This is the park half only. Case (3) is reported and not parked (§ An arena-local verdict), and the difference is scope: these two conditions are true of a commit and read the same from every arena, where that one is true of one arena and false everywhere else.

**What the block must name is the condition, not the symptom.** [environment.md](environment.md) § The condition is reported already sets the standard — the condition, the measurement that established it, and what would clear it — and it holds here unchanged. *Verify failed* is not a block anyone can act on; *the base is red on this platform, measured at commit `abc1234`, and it clears when a commit green on this platform lands ahead of it* is.

## Two conditions, two clocks

A step parks on more than one thing here, and the two do not end alike. A mechanism built for one releases neither of them.

| Blocked on | Clears when | Read against |
|---|---|---|
| The base's health is `unknown` and this arena cannot measure it | A verdict for that commit on that platform is recorded | The record |
| The base is `red` | A commit `green` on this platform lands ahead of the base, to rebase onto | The trunk |

Case (3) is absent from this table deliberately: it parks on nothing, so there is no clock to release (§ An arena-local verdict).

> **Both are carried on the park as structured values, never as prose for something to parse back out.**

A park naming its clearing condition in a sentence is a park whose release depends on reading that sentence correctly, forever, in every reader. The precedent is set: [environment.md](environment.md) § The agent account requires an exhausted allowance to carry the instant it returns, for this reason and with this result — a condition that states its own end is waited out rather than rediscovered.

**A missing verdict clears by being supplied, and nothing about that is a person's act.** The block names the commit and the platform whose verdict is absent; it ends when one is recorded, whoever recorded it — a job the backend scheduled, or simply the next arena dispatched onto the same base with the question asked early (§ When the question is asked). That is the loop back to the placement choice: **asking late can arrive at a block whose clearing condition the project produces nothing to satisfy.** A project whose arrangement measures a base only when an arena asks for it up front should ask up front, where the arena that needs the answer is the one positioned to produce it.

**A red base waits on the trunk, not on its own history.** The arena has not rebased and cannot — that is what it is blocked on — so its ancestry is exactly the thing the fix is missing from, and a test against it never passes. What it waits for is a commit ahead of its base that is `green` on its platform, which is a fact about the trunk and the record, readable by anyone at any moment. A fix that lands and is never measured on this platform does not clear it, and that is correct rather than pedantic: the arena would rebase onto a base it knows nothing about and be back here.

What the flow does once the condition ends is the flow's — the park clears, the step runs again, and the rebase is ordinary work.

**Neither clock is re-dispatch.** `condition-unmet` reports that re-dispatch does not clear it ([cli.md](cli.md) § A park is re-dispatched when the kind says a re-dispatch clears it), and that is the honest answer for both: running the step again against an unchanged record and an unchanged trunk reproduces the stop exactly. What clears them is the condition, evaluated before the step is dispatched at all — which is what makes the park worth carrying rather than a stop an operator has to notice.

## What trunk health is not

**It is not `fit`.** `fit` measures the machine, before work is given ([environment.md](environment.md) § `fit` measures the machine). Trunk health is about one commit's content, and a machine's fitness and a commit's health are independent in both directions.

**It is not a schedule.** This document fixes what a verdict is, what it is keyed to, and where it may come from. How often a project measures, and whether anything measures a commit nothing was dispatched onto, is the project's — a norm that mandated a periodic job would be legislating cost for projects whose bases are already green from landing.

**It is not recorded on the item.** The record is keyed by commit and platform, and an item is neither. An item that met a red base carries what every stopped item carries — a park, naming its condition and what clears it — and the item moving to another arena does not take a claim about a commit with it.
