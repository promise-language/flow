# Gates and commands a project provides

**Normative.** What a project must supply for a flow to run against it, and the names for the things it supplies.

`docs/resolution.md` defines what gates and commands *are*. This document says which ones a flow expects to find.

## Required

Two, and a flow cannot run without them.

| | Kind | Answers |
|---|---|---|
| `verify` | command | Repair what is mechanically repairable, then report whether what remains is sound |
| `integration` | gate | Will the mainline still be green if this lands |

**`verify` is what a producing step works with.** It is run by steps, by agents mid-turn, and by people at a terminal, and it does the same thing for all three. A step should not fail over something `verify` would have fixed.

**`integration` is what a decision rests on.** It runs before a change is proposed and again before it lands, and nothing reaches the mainline without it. It modifies nothing, so its answer is reproducible by whoever asks — which is the entire reason a decision may rest on it and not on `verify`.

**The gate is about a tree, not about the mainline.** It takes a state of the code and reports whether it is sound. It does not know or care where the mainline is — which is what lets the same gate answer two different questions, depending on what it is pointed at:

| Pointed at | Answers |
|---|---|
| The working tree, or the branch | Is this change sound? |
| The merge result | Would the mainline still be green with this in it? |

A producing step wants the first, and wants it without any reference to the mainline's position. Being behind is not a reason for a step's work to be judged unsound.

**Being behind the mainline is not a gate failure.** It is a stale question. If the mainline has moved, the merge result the gate measured is not the merge result that would be created now, so the answer is about a tree nobody will land. The fix is to rebase and ask again — not to report the change as failing.

So landing is a loop, not a single verdict:

1. Bring the branch up to the mainline, if it is behind. **This is a command; it modifies.**
2. Measure the merge result. **This is the gate.**
3. Push.

If the push is rejected the mainline moved during steps 1–2, and the loop repeats. **The push is the arbiter**, not the gate: only the push can establish that nothing changed underneath, because it is the only step that is atomic with respect to other people landing. A gate that passed and a push that succeeded together prove the landed state was measured; a gate alone proves only that some state was.

The loop is bounded. A branch that cannot land after a few rounds is losing a race it will keep losing, which is a fact for a person rather than something to retry indefinitely.

### `integration` is a composition, and its parts are addressable

A project assembles `integration` from smaller gates — formatting, build, one test suite per target, whatever it values. **Each part is runnable on its own**, and that is a requirement rather than an implementation detail.

The reason is cost. A failing gate usually fails in one area, and fixing it means iterating: change something, check, change again. Re-running everything each round means paying for the whole suite to learn about the one part being worked on, and on a project of any size that is the dominant cost of the entire resolution. A step working on a failing test suite should re-run **that suite**, not the build, the formatter, and every other target.

So the narrow parts are what iteration uses, and the whole is what a decision rests on:

| | Runs | Establishes |
|---|---|---|
| A part | While fixing that part | Whether this attempt helped |
| The whole | Once the part passes, and before landing | Whether the change is sound |

**Passing the parts is not the same as passing the whole, and only the whole may be cited.** A fix for one area can break another — that is not an edge case, it is the ordinary way a change goes wrong — and a sequence of narrow passes at different moments describes no single state. The full run is the only thing that says *this tree, all of it, at once*.

The narrow parts inform the work. The whole supports the decision. It is the same division as commands and gates, one level down.

A project may still implement `integration` as a single indivisible command. It will work, and every fix round will cost the full suite.

## Where these live

**Gates and commands belong to the project. The flow does not.**

Ownership follows the subject, and that settles every case:

| A gate measuring | Belongs to | Because |
|---|---|---|
| The code | the project | The rules it measures against are part of the code |
| What a step did | **the flow** | Only the flow knows what the step promised |
| What an agent does | the environment | It applies whether or not a flow is running |

The write contract is the one gate a flow supplies, and it is the exception that the rule predicts rather than one it has to accommodate: a project cannot own it, because a project has no idea what a step undertook to write.

### Gates live in the tree they measure

A gate measures a state of the code, and the rules it measures against are part of that state. A change that adds a test changes what the test gate reports; a change that tightens a check changes what the check gate rejects. That is correct and is the point: **the gate for a tree is the gate in that tree.**

It also means a change can move its own goalposts, deliberately. Raising a standard and meeting it in the same change is one act, measured by the standard as it now is — not by whatever the standard was when the branch was cut.

### The flow is delivered from outside

**A flow must not live in the tree it resolves.**

The reason is that flows have bugs, and a bug found mid-resolution has to be fixable without disturbing the resolution. If the flow lived in the tree it was working on, fixing it would mean editing that tree — so the fix would land in the item's branch and become part of a change it has nothing to do with, and the resolution would be altering its own behaviour while running.

Delivered from outside, none of that arises. The flow can be replaced between steps, or mid-step, and the item's branch carries only the item's work.

This is not theoretical. **This flow resolves issues against its own source**, and does so safely only because of this separation: an agent edits the flow's code on a branch while the flow driving that very resolution runs from a binary provisioned elsewhere. The edits are the change under review; the binary is unaffected by them.

The consequence for a project is that it pins a flow version, and updating the flow is not a change to the project. The two version independently, which is what lets a broken flow be fixed for everyone without anyone's in-flight work being touched.

## The names

A gate is named as a **concept** and an optional **instance**: `tested`, or `tested:wasm`.

**The concept is closed.** These names are the whole vocabulary. A project must not call one of them something else, and must not use one for something it does not name — that is what makes a name mean the same thing in a project a reader has never seen.

**The instance is the project's, and nothing validates it.** One concept commonly covers several separately runnable gates: a host suite, a wasm suite and a stress suite are all obviously tests and all worth asking for on their own. Collapsing them into one name would destroy the property naming exists to provide — that a step fixing one failing suite asks for *that suite* rather than paying for the whole set — and it would do so hardest on the projects whose suites cost the most.

So the vocabulary is closed where everyone must understand it, and open where only the project knows how its work divides.

### Gates may have to wait, and the backend is what makes them

Some gates cannot run beside another. A full suite that saturates a machine measures its own contention as much as it measures the code, and two of them at once give both a worse answer than either alone would.

**Serializing them belongs to whatever runs them.** Only that layer knows what else is on the hardware, what a gate costs, and which gates can safely overlap. Nothing about a resolution knows any of it, and a flow that tried to schedule around machine load would be guessing from the wrong side.

So a gate may **queue before it runs**, and the caller waits.

**Waiting is not failing, and losing that distinction is expensive.** A gate that queued and then ran is exactly as authoritative as one that ran immediately. A gate that gave up waiting has measured nothing at all — and reporting that as a refusal marks a sound change unsound, and sends someone to look for a defect that does not exist.

The failure is not hypothetical: a wait capped below the time a real run takes turns every busy period into a stream of false refusals, each indistinguishable from a genuine one.

Two things follow:

- **A wait bound is not a verdict.** Exhausting it is "no answer yet", which is a transient condition to retry — never a refusal to act on.
- **Naming gates makes this tractable.** A formatting check and a full test suite have nothing in common in what they cost, so serializing per name lets the cheap ones run freely while only the expensive ones queue. A single undifferentiated gate would have to take the heaviest lock every time.

### Two scopes, for two different reasons

Serialization is not one thing. What is being protected differs, and so does how far the protection has to reach:

| Scope | Protects | Example |
|---|---|---|
| **Host** | One machine's resources | A suite that saturates the hardware; two at once measure their own contention |
| **Project** | Shared state, across every machine | Landing a change on the mainline |

**Host scope is about the quality of an answer.** A gate competing for the machine reports something about the load as much as about the code, so running it alone is what makes the measurement mean anything.

**Project scope is about a loop terminating**, which is a stronger requirement than it sounds. Landing is rebase → measure the merge result → push, and a push that lands first invalidates every merge result measured against the old mainline. With two arenas landing at once and no serialization, each one's push sends the other back to rebase and re-measure, and both can do this indefinitely — not a conflict to resolve but a livelock, where the work is sound, the gate passes every time, and nothing ever lands.

Serializing the landing turns that into a queue. The loop's bound then catches genuine starvation rather than ordinary contention, which is the difference between a bound that fires when something is wrong and one that fires on a busy afternoon.

**Both scopes are the backend's**, because only it knows what shares a machine and what shares a mainline. And waiting is not failing at either scope.

### A project has gates the flow knows nothing about

**The set above is what the flow asks for, not what a project may have.** A project's gates are its own, and most projects have more than these.

Some serve purposes a resolution has no part in: a size measurement watched for a trend, a release invariant checked on a schedule, an installation exercised nightly. They answer questions about the project over time rather than about a change about to land, and nothing in a resolution needs them.

**A gate the flow does not recognise is not a gap in this vocabulary.** It is a project doing something the flow is not involved in, and requiring it to be renamed into one of these concepts would be worse than leaving it alone — it would either distort what it measures or make `integration` mean "everything left over".

Two consequences worth stating:

- The gate entry point carries whatever the project gates. Asking it for a name it does not have is an error; **having names the flow never asks for is not.**
- Nothing enumerates a project's gates on the flow's behalf. The flow asks for what it needs by name, and learns nothing about the rest.

**A project decides what `integration` is made of, and that is where the two sets meet.** A check that should stop a change from landing belongs inside `integration`; one that watches the project over time does not. A size measurement is the clear case: as a trend it is periodic and none of the flow's business, and as a limit a change must not cross it is part of what the project means by integrable — the same underlying check, placed by what the project wants it to decide.

The flow does not need to know which of them is which. It asks for `integration` and gets the project's answer to *may this land*.

Each concern appears as a command, a gate, or both. Where both exist they are the same rules asked two ways: one repairs, and only the other can be cited.

| Concern | Command | Gate |
|---|---|---|
| It is written the agreed way | `format` | `formatted` |
| It compiles | `build` | `builds` |
| It compiles but is probably wrong | `fix` — the repairs with one right answer | `checked` |
| It behaves correctly when run | — | `tested` |
| Enough of it is exercised | — | `covered` — a measurement against a floor |

A project need not have all of them. It must not call any of them something else, and must not use one of these names for something that is not what it says.

**Names describe the concern, not the tool that historically served it.** `lint` is a C utility from 1978, `vet` is one language's spelling of the same idea, and a reader who knows neither learns nothing from either. What the third row *does* is find code that compiles and is still a mistake — an unused result, a shadowed name, a conversion that cannot be meant — and repair the subset with one obvious fix.

`checked` is the weakest name here and worth knowing why it is still the choice: every gate is a check, so on its own the word says little. It earns its place by sitting beside the others — `formatted`, `builds`, `tested`, `covered` each claim something specific, and `checked` is what remains. The alternatives are worse: historical ones teach nothing, and `static` or `analysis` name the mechanism rather than what is being protected.

**The last two rows have no command form**, and that is not an oversight. "Make the tests pass" and "raise the coverage" are not mechanical repairs with one right answer — they are the work itself, and a tool that did them would be writing the change. Where a row has both, the command handles what has one right answer and the gate reports what is left; where a row has only a gate, nothing about it has one right answer.

## Gates on what an agent does

Two gates measure an agent's proposed actions rather than the code:

| Gate | Subject | When |
|---|---|---|
| **run tool** | A command an agent is about to run | Before it runs |
| **edit file** | A file an agent is about to change | Before the edit |

They are separate because their answers are. An agent may be free to run anything and forbidden to touch a particular file, or the reverse; one combined "may the agent do this" gate would force a single answer to two questions.

### They are not the flow's, and they do not switch off

**These apply to every agent action, whoever asked for it.** They are properties of the environment an agent runs in, not of a resolution — so they hold when a flow is driving, and equally when a person is sitting at a terminal asking for something ill-advised. A constraint that lapses the moment a human is in the loop protects nothing: the mistakes are the same mistakes, and a person asking for them directly is if anything the likelier case.

That also means the flow does not supply them. It benefits from them, and it must not assume them.

### The worktree is the default boundary

**An agent's edits are confined to the worktree it is working in.**

Reaching outside is the failure this prevents, and it is not hypothetical: where several checkouts of one repository sit side by side, an agent that resolves a path a little wrong edits a tree belonging to work it knows nothing about. The change lands somewhere nobody is looking, in a tree someone else is mid-way through, and the damage surfaces later as a mystery — the file was not edited by the person whose tree it is, and nothing in their history explains it.

The default is worth more than its exceptions. Widening it is a decision to make deliberately, per case, with a reason.

## The gate the flow provides

| Gate | Subject | When |
|---|---|---|
| **write contract** | What a step actually did, against what it said it would | After the step |

This one *is* the flow's — the only gate a flow supplies. A project cannot own it: the contract being measured is the step's, and a project knows nothing about steps.

It exists because the two gates above **fail open**: they are enforced by the agent, and an agent that reaches a shell, or runs somewhere they are not configured, goes straight past them. They are worth having because they are cheap and immediate — the agent learns the constraint mid-turn and adapts, rather than losing a whole turn to a rejection after the fact. They are not the guarantee. This is.

## Rules

**One name, one thing.** A project that has both `format` and `fmt`, or a `test` that sometimes writes fixtures, has two things wearing one name and no reader can tell which they are getting.

**A gate named as a gate must behave as one.** Something called `formatted` that quietly reformats is worse than having no gate at all: a decision will be made on its answer, and the answer will be about a state that no longer exists.

**Configured once.** Whatever a project supplies is configured in one place and reaches every consumer — the step that runs it, and the prompt that tells an agent what to satisfy. Two settings that both mean "the check" will disagree, and the failure is silent: the agent runs one and it passes, the flow runs another and it cannot.
