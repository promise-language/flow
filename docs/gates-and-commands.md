# Gates and commands a project provides

**Normative.** What a project must supply for a flow to run against it, and the names for the things it supplies.

`docs/resolution.md` defines what gates and commands *are*. This document says which ones a flow expects to find.

## Required

Two, and a flow cannot run without them.

| | Kind | Does |
|---|---|---|
| `verify` | command | Repairs what is mechanically repairable, then reports whether what remains is sound |
| `integration` | gate | Measures whether the mainline would still be green with this in it |

**`verify` is what a producing step works with.** It is run by steps, by agents mid-turn, and by people at a terminal, and it does the same thing for all three. A step should not fail over something `verify` would have fixed.

**`integration` is what a decision rests on.** It runs before a change is proposed and again before it lands, and nothing reaches the mainline without it. It modifies nothing, so its measurement is reproducible by whoever asks — which is the entire reason a decision may rest on it and not on `verify`. The decision itself is still made by the layer holding the thresholds; the gate supplies the numbers it is made from.

**The gate is about a tree, not about the mainline.** It takes a state of the code and reports whether it is sound. It does not know or care where the mainline is — which is what lets the same gate answer two different questions, depending on what it is pointed at:

| Pointed at | Measures |
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

## Running a gate, and reading what it reported

Three parties, three jobs, and the separation is the whole design:

| Party | Does | Comes from |
|---|---|---|
| **The gate** | Measures | The tree |
| **The runner** | Spawns the gate and observes what happened to it | Outside the tree |
| **`verify`** | Judges the measurement against the thresholds | The project's manifest |

**A gate is never invoked directly.** A caller asks the runner to run one and reads what the runner reports. Spawning the process is the runner's job.

### The gate has exactly one output

The envelope, one JSON object on stdout: what was measured, and the reason it measured less than usual if it did. Never a verdict.

**Stdout carries the envelope and nothing else.** Progress, logs and a failing suite's own output go to stderr. A gate that narrates on stdout produces something that is not an envelope, which is `broke the contract`.

**The gate's exit code is not consulted.** Not as a verdict — it does not have one — and not as an account of the run either, because the states that matter most are the ones a gate is not alive to report. A process killed at the declared timeout, killed for memory, or truncated mid-write by a full disk says nothing at all. And the safe-looking direction fails too: **a gate that exits 0 having printed nothing has stated something false**, and a caller reading its code believes it.

Only the party that spawned the process can tell those apart, and it can say so in terms no gate could have reached.

This is also what keeps a second channel from existing. The gate says one thing; everything else a caller learns is the runner's account of what it watched. Two channels that could disagree never arise, so there is no rule here for when they do.

### The exec line is exec'd, never interpreted

The exec line is `bin/gate <name>`, and the question is asked one way only — two callers asking the same thing must not be able to get different answers and both be right.

**The runner appends `--envelope`.** The flag is protocol rather than project configuration, so it has one spelling everywhere and a runner adds it without being told to. It is appended last, after whatever the project declared, because that is the only rule that works without parsing the line.

**A gate prints an envelope only when it was given the flag.** Any other invocation is a person or an agent at a terminal, and must print nothing on stdout and exit non-zero. Silence and failure are what stop the human path from becoming a second channel: a bare invocation that printed measurements and exited `0` would be read as a pass by the first script that wrapped it, which is the ambiguity the three parties exist to remove.

`bin/gate tested --envelope` is the program `bin/gate` with the argument `tested`. No shell, so no quoting rules, no word splitting, no `.rc` file, no dialect. This is portability, not preference: a line that is interpreted gives different answers on different hosts for reasons that have nothing to do with the subject, which is the measurement half of reproducibility failing at the point of invocation.

### What the runner reports

An outcome the runner determines from what it observed, not a number the gate chose:

| Outcome | Means | Whose problem |
|---|---|---|
| **measured** | The process completed and printed a valid envelope | Nobody's yet — judging the numbers is `verify`'s question |
| **timed out** | Killed at the declared timeout | The wait, or the host. Not the change |
| **could not start** | The program the exec line names is absent or not executable, so nothing ran | Whoever declared the gate, or whoever delivered the tree |
| **died** | Killed by a signal, or exited without printing a readable envelope | The host |
| **broke the contract** | Printed something that is not an envelope, disagreed with what the manifest declared, or modified the subject it measured | The gate's own code |

**Only `timed out` is worth retrying unchanged.** The other three failures all recur, which is why a retry policy keys on that split — but they are owned by three different people, so collapsing them costs attribution even where it never costs a wrong retry.

`could not start` is the one it is most tempting to fold into `died`, and the most expensive to. `died` carries *retry is correct*; a retry loop pointed at a missing binary never terminates and never learns, and reads as a flaky host for as long as anyone lets it run. It is not `broke the contract` either — that is a defect in what the gate *printed*, fixed in the gate's code, whereas an absent program is a defect in what was declared or in what was delivered. Different repository, different person.

The gate's own exit code is kept beside the outcome as a raw diagnostic — it is the only place the kernel's number survives, and a person debugging a gate wants it — but nothing decides on it.

**This vocabulary is not flow's.** It is a wire contract shared with base, defined once in [base's gate contract](https://github.com/promise-language/base/blob/main/docs/gate-contract.md) — the piece the SDK reads in every project, so it has to mean the same thing everywhere. The five names above are reproduced for readability; where this document and that one differ about them, that one is right. What is flow's, and stated here, is what flow does with each outcome.

### Progress reaches the reader as it is written

Stdout is captured. **Stderr is passed straight through, unbuffered.** Gates run long, and a gate that is working and a gate that is wedged produce the same thing — nothing — for as long as the output is held. Someone watching a ten-minute suite needs to see it working, and an operator deciding whether to kill a run is deciding on that silence.

**Passed through means the reader's own stream, not a pipe the runner copies.** The distinction is not pedantry: a runner that pipes stderr and forwards it faithfully still defeats the rule, because most runtimes switch from line to block buffering the moment their output is not a terminal — so the gate's progress arrives in one block at exit, which is the silence this exists to prevent. The gate must be genuinely attached to whatever the reader is attached to.

Two consequences worth stating before anyone hits them:

- **Progress does not extend a deadline.** Otherwise a wedged-but-chatty gate runs until something else kills it, which is the failure the deadline exists to bound.
- **Concurrent gates interleave**, and the obvious remedy — prefixing each line with the gate's name — is forbidden by passthrough: a runner that rewrites the stream is no longer passing it through, and a gate's own output is not the runner's to edit. This needs settling before anything runs gates in parallel.

### The runner bounds what it reads

**The bound is on stdout**, which is the stream the runner holds in order to parse it. Stderr is passed through and never accumulated, so it needs no bound — it is already going somewhere with a person on the end of it.

A gate that gets stdout wrong is the ordinary case, not an exotic one — a test runner left to print its log there emits as much as the suite feels like. So the runner reads **up to a bound**, and what it does at the bound is part of the contract rather than an implementation detail:

- **It stops at the bound.** A runner that drains an unbounded stream has handed a gate the ability to exhaust it. That failure is categorically worse than the one it is supervising: a gate exhausting its own memory is one red result, while the runner going down takes everything it was watching with it — and unattended, that is the difference between a failure someone sees and a silence nobody does.
- **It keeps a prefix, and stores only that.** Enough to identify what the gate was actually emitting, which is what a person diagnosing `broke the contract` needs. The full stream never reaches a store.
- **It does not leave the child wedged.** A runner that stops reading a pipe the gate is still writing to blocks that gate on a full pipe buffer, turning an over-talkative gate into a hung one — which then reports as `timed out`, naming the wrong problem. Closing the read side or killing the child is part of stopping, not a separate concern.

Bounded output is what makes `broke the contract` safe to detect at all. Without it, the runner has to hold everything a defective gate produced in order to conclude that it was defective.

### How the runner tells them apart

`could not start` is decided at the spawn — the exec fails and no process exists. The rest are decided by what the process printed, because the envelope is written whole:

- **Parses** → measured. The envelope is the gate's own account of what it measured.
- **Absent or truncated** → died. Silence is absence, not a malformed envelope.
- **Present and not an envelope** → broke the contract.
- **An envelope that contradicts what the manifest declared** → broke the contract. A float where the manifest says `int` is a mismatch to name, not a widening to absorb: a metric whose type changed mid-history measured something else, and absorbing it silently moves a ratchet that by construction never moves back.

### The non-modification rule is checked, not assumed

`resolution.md` requires a gate to leave its subject exactly as it found it. That is checkable only if *subject* names something specific, so it does: **the subject is the tracked tree.** The runner records tracked state before spawning and compares after; paths the project ignores are outside the subject, which is where a build cache and a report legitimately go.

A difference means **broke the contract**, never `measured`.

Three consequences, and the middle one is why the check earns its cost:

- **A violation is not a failing measurement.** A gate that broke the contract has not reported that its subject is bad — it has failed to report anything. That is expensive read either way: as a failure it blames a change for a defect in the gate, as a pass it admits a transition on a measurement nobody made. So no measurement is stored, no baseline moves, and whatever the gate was blocking is refused **for the gate**, not for the tree.
- **A modified worktree is spent.** The remaining gates for that transition must not run in it. There is no partial recovery: nothing downstream can distinguish which gates ran before the modification and which ran after, so their honest numbers describe a tree nobody proposed.
- **A repairing gate reads as an improvement.** It is the ordinary way this happens — a format gate that fixes rather than reports — and its numbers get better, feed a ratchet, and raise a floor no honest run can meet.

Two limits, stated rather than discovered. **A gate that modifies and restores passes the check** — before-and-after cannot see it, and a gate built to evade this is a gate written to pass itself, which is what the thresholds boundary handles instead. And **detection is after the fact**: the modification has happened, and what the check buys is that it is reported rather than absorbed.

This is what makes deriving completeness from the *absence* of a stated reason safe. A gate that deliberately measured less says so and gives a reason; a truncated envelope is not an envelope at all. Without parse-or-nothing, a truncation would read as a complete run — exactly backwards.

**A run that measured less than usual must never move a baseline.** Honest numbers that understate what was checked are indistinguishable from a regression unless the run says it measured less.

### The runner comes from outside the tree, and that is the whole requirement

A gate an agent can edit must not be what decides whether that agent's change passed. A runner taken from the worktree could be edited to skip. **The property is whose artifact it is** — the same boundary the write-contract sits behind.

**Being remote is not what makes a runner trusted.** A purely local runner that never speaks to a server and only keeps tabs on what it started satisfies this exactly as well.

That matters here, because the standalone model has no server and this is the one requirement it might have been thought to fail. It does not — and flow is already positioned to satisfy it. **A flow is delivered from outside the tree it resolves**; that is stated below for a different reason, and it is precisely the property a runner needs.

### Running one gate by hand

A person or an agent wanting one gate's result runs **`bin/run <gate>`**, which takes the same path the runner takes rather than a parallel one. Running a single gate is not a lesser case: it is faster than everything that blocks a transition, and it is what someone iterating on one failure actually wants.

**It reaches a verdict and prints it for a human**, because it is the judging layer and the judging layer is the only thing that can — it holds the thresholds, so it can put a number beside the terms it was judged on. A gate could only ever print the left-hand column.

That is also why a gate keeps exactly one output mode. A gate that pretty-printed when it thought a human was watching would have two, and one of them would not parse.

### Where the verdict is made

A gate cannot be asked for a verdict, and neither can a runner: `measured` says a measurement exists, not that it is acceptable.

The verdict is computed by whoever is deciding, from two inputs:

| Input | From | Says |
|---|---|---|
| The measurement | The gate, this run | What is true of this subject now |
| The thresholds | The project, out of the subject's reach | What would be acceptable |

**The thresholds are a distinct artefact from the gates.** Which gates must be green before a change may proceed, what floor coverage must clear, which baselines ratchet — that is a manifest the project provides alongside its gates, and no gate reads it.

**A gate holding its thresholds can be passed by editing the gate.** When the subject is a change written by an agent, the agent can edit it, and a gate must not be able to acquit the thing it is measuring. That is the artifact rule, and it is why the thresholds are a separate artefact.

**But separate does not mean elsewhere.** A threshold must be a function of the subject — `resolution.md` — which a manifest versioned with the tree satisfies and a per-host cache or independently-moving server state does not. A commit carries the terms it was judged on, so any machine reaches the same verdict for that commit, offline, whenever it is asked.

**The rule is about the author, not the location.** What is forbidden is the party under judgement moving what judges it: a resolution's diff may not contain the manifest, refused rather than merged and flagged, because once a floor has moved no later run can tell it was wrong. A person lowering a threshold deliberately in a reviewed change is doing something legitimate — a metric that got worse for a reason the project accepts — and a rule that forbade it by location would forbid that too.

### What is universal, and what is not

**The exec line and the outcome vocabulary are universal.** Every project's gates are exec'd the same way and every run reports one of the same five outcomes, because the SDK reads them in every project and cannot hold one dialect per repository.

**The envelope's shape is between a project and whoever judges it.** Two projects may carry different detail without either being wrong, so long as each is consistent with the thing reading it. The SDK does not read a project's measurements; it runs the gate and reports what became of the run.

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

**Waiting is not failing, and losing that distinction is expensive.** A gate that queued and then ran is exactly as authoritative as one that ran immediately. A gate that gave up waiting has measured nothing at all — and reporting that as a measurement marks a sound change unsound, and sends someone to look for a defect that does not exist.

The failure is not hypothetical: a wait capped below the time a real run takes turns every busy period into a stream of false failures, each indistinguishable from a genuine one.

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
