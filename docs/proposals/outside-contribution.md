# Outside contribution

**Proposal. Not normative.** How an item is resolved when the contributor is a principal the project does not trust, and what the maintainer measures before the change lands.

[issue-flow.md](../issue-flow.md) declares two roles and the boundary between them: the contributor's part ends at the proposal, the item awaits the maintainer, and one principal covering both roles crosses without a handoff. What it does not say is what changes when the contributor principal is **outside** — no write access to the repository, working in a fork, appending journal entries with their own account. That is the ordinary case for a public project, and it is the path a change should take to reach the mainline.

> **The item is the project's, the journal is shared, and the trust is not.**

Everything here follows from those three facts holding at once. A request that arrives with no item at all is a different situation, and a lesser one: [drive-by-requests.md](drive-by-requests.md).

## The main path

**A change reaches the mainline by resolving an item.** An item exists in this project's tracker; a contributor resolves it through the flow, producing a plan, three producing commits, briefings, and a gate result, all recorded against that item; the maintainer reviews what the journal holds, measures what it cannot vouch for, and lands it.

That path is worth being the well-lit one because of what it makes available at review. The maintainer is not reading a request body: the intent was written down before the code, by a step whose whole contract is that it modifies nothing ([issue-flow.md](../issue-flow.md) § Plan); the producing phase is three commits, so what implement did and what review changed are separable; and the gate ran on the branch before the request was opened, so what is proposed has been measured by the same gate the maintainer will run.

**None of that is available to a review of a naked request, and none of it can be reconstructed.** A plan derived after the fact is not a plan, and a diff cannot say what its author decided not to do.

## One item, one journal, two principals

The contributor and the maintainer work **the same item**. The journal is one sequence of entries on it, each carrying a captured result and the route the step elected ([artifacts-and-signals.md](../artifacts-and-signals.md) § The record), and both roles read all of it.

Three properties come from that and from nothing else:

- **Rework is a route, not a new request.** The maintainer's review elects `implement`, the transfer message carries what must change, and the work continues as further commits on the existing branch. There is no second request, no re-derivation of intent, and the rounds are visible in the history.
- **Resumption is shared.** An item picked up again reads the same journal and derives the same pending step, whichever principal picks it up.
- **Supersession is visible.** Entries are never rewritten, so a superseded plan stays in the journal as the record of what every step that ran in between actually saw.

**This is why the flow is worth requiring rather than merely offering.** The artifacts are not paperwork; they are what makes the maintainer's side cheap and the contributor's side resumable.

## The trust asymmetry

The journal is shared. Its **entries are not equally strong**, and which is which is checkable.

> **A journal entry is a record when a trusted account appended it, and a claim when the party under review did.**

[proposals/untrusted-sources.md](untrusted-sources.md) defines the test and it applies unchanged here: a source is trusted when it holds write, maintain or admin permission on the repository, and the probe is capability, not association. An outside contributor holds none, so every entry their runs append — the plan, each briefing, the gate result — is a claim.

**A claim is worth much more than nothing.** It is durable, ordered, attributed, on the project's own item, and it cannot be quietly rewritten. It states what the author decided before they wrote code, which is the single most useful thing a reviewer can have. What it is not is a measurement: nothing in the entry establishes that the plan was followed, that the gate ran, or that it passed. The claim says what happened; only this project's own run says what is.

So the maintainer measures — not because the author is suspected, but because a claim by the party under review is the one kind of evidence a review cannot use to shorten itself. **The measurement is the same for every outside contributor**, which is what keeps it from being a judgement about any of them.

## The problem, observed

The measuring sequence below is derived from one manual review — [promise-language/promise#25](https://github.com/promise-language/promise/pull/25), branch `feat/wasm-web-reactor` — by recording each measurement performed, in the order performed, and what it produced. That request carried no item at all, which is [drive-by-requests.md](drive-by-requests.md)'s subject; the measurements it took are the ones any untrusted change needs.

No flow ran, so no plan step preceded the work, no pass reviewed it against the change, and no pass covered it. Those are facts about what stood between the work and the request, not choices the author made. They followed a phased plan the subject document set out at the time, which was the right thing to do; the document has since had its phasing removed, because a specification may not carry status ([org/normative.md](../org/normative.md) § 3), and the rule that removed it postdates the work.

**Nothing below is a criticism of the author.** They did what they could with what was established then, against documents as they then read. Three of the review's findings are the argument for three of the steps:

- **A citation whose section says something else.** A module header justified itself as `hand-written (no bindgen — §14.2)`. §14.2 is titled *It must be usable without bindgen* — which, read as a title, is close to the permission the header claimed; read as a section, its subject is the consumer's program rather than the module. It is an easy reading to arrive at and a load-bearing one to be wrong about, since the whole artifact's shape rested on it. Reading the diff does not find this; only opening the cited section does.
- **An export bug no test in the change could see.** The new module exported none of its five entry points. Its tests passed, because they lived inside the module and called everything unqualified; the acceptance program the normative document names failed to compile against it. In-module tests cannot observe an export defect by construction.
- **A finding that shrank when it was dated.** Sixteen dead documentation links read, at first, as sixteen the request introduced. Author-versus-committer dates and per-commit diffs said otherwise: one was rebase residue from before a rename, fifteen were written after it, and the commit message had already said so. Same facts, a materially smaller finding, and a different response.

The first two are what any change looks like from outside when no planning step and no consumer-position check stood between it and the request — a statement about the process that was available, not about the person using it. The third is why measurement is separated from judgement below.

## Reading what the maintainer does not trust

[proposals/untrusted-sources.md](untrusted-sources.md) states the inbound rule one boundary earlier: item text is executable input, and acceptance is a flow whose product is a decision about text. The measuring sequence is the same premise at the next boundary, where the product is a decision about a change. Its step rule governs here directly:

> **A step declares whether it may read untrusted text. The declaration is explicit, and the default is that it may not.**

**Every measuring step declares that it may.** The diff, the commit messages, the journal entries the contributor's runs appended, and the comments in the code are all text the party under review wrote, and there is no reviewing them without reading them. So these steps are one of the places that exposure is intended, and — since they cannot be bounded by what they read — they are bounded by what they may **do**: they write nothing to the tree, file nothing, and end nothing.

**Text under review is read as input, never as instruction.** A briefing asserting that the acceptance program may be skipped, or a commit message describing what the reviewer should conclude, is a finding — not a permission.

## The measuring sequence

The sequence runs on the maintainer's side, after the handoff and before the proposal is judged. It is elected when the contributor principal is untrusted; where both roles are the same principal, or the contributor holds write, `review the proposal` reads a journal of records and the sequence is not elected.

Steps in **bold** are mechanical — no agent prompt.

| Step | Role | Concern | Writes | Routes to |
|---|---|---|---|---|
| **situate the branch** | maintainer | Where the change sits relative to the tree that exists | the `base` record | establish provenance |
| **establish provenance** | maintainer | What each commit introduced, and when it was authored against when it was replayed | the `provenance` record | verify doc references |
| **verify doc references** | maintainer | Every document path and section the change cites resolves in this project's copy | the `reference-report` | verify doc claims |
| verify doc claims | maintainer | Where the change cites a document as justification, the document says that | the `claim-report` | check conformance |
| check conformance | maintainer | The change contradicts no document binding here, at the version this project holds | the `conformance-report` | run the acceptance program |
| run the acceptance program | maintainer | The change works from a consumer's position — the one its own tests cannot occupy | the `acceptance` record | check the plan of record |
| check the plan of record | maintainer | The change claims no place and no name something else already spoke for | the `plan-conflict` record | state coverage |
| state coverage | maintainer | What was measured, and what was not | the `coverage-statement` | review the proposal |

It elects into `review the proposal`, which is unchanged: it judges the proposal as what will land, and its three declared routes — verify merge result, implement, finalize: rejected — are [issue-flow.md](../issue-flow.md)'s. The sequence supplies what that step measures with; it does not decide.

**There is no separate tag-reconciliation step.** What the change closes is the item's own subject, already in the journal, and whether a document's tag query is complete is the project's reconciliation pass rather than this review's ([org/normative.md](../org/normative.md) § 7).

### Measurement is separated from judgement, and the separation is the sequence

> **A measuring step records what it finds and elects its declared successor. Only `review the proposal` decides.**

The measuring steps change nothing, file nothing, and end nothing. This is the gate rule applied to review: verify repairs and the gate decides ([resolution.md](../resolution.md)); here the measuring steps measure and the judging step decides.

The alternative — a step that finds a blocking condition and ends the review on it — is what a person does naturally and what costs the most. It spends a contributor round per finding, and it spends them in the wrong order: the cheapest measurements are first, so a review that stops early is always the one that reports the least valuable thing it knows. It also reports findings a later step would have corrected, which is exactly what the dated-links finding was.

**A measurement that cannot be taken is a finding, not a failure.** A branch that will not merge does not stop the sequence; every subsequent record names the tree it measured — the request's head, or the trial merge result — so a reader knows what each measurement is a measurement of.

### A finding is dated before it is attributed

> **The change is measured against the documents as they stand. The author is judged against the documents as they stood.**

Two questions, both real, and a review that answers only the first is factually correct and wrong about who has work to do. A citation that no longer resolves is a finding either way — the code says something the tree does not support, and somebody has to fix it. Whether the author introduced it, or an amendment landed under it after they wrote it, decides what the response asks for and what it costs them to hear.

The observed review had one of each. Fifteen dead links were written after a rename and one before it, and a single measurement separated them. The change's whole shape followed a phasing section that a later rule required to be deleted — the code is out of step with its document, and nothing the author did caused that.

**So the measurement is dated at both ends.** `establish provenance` dates the commits; `verify doc references` and `check conformance` date the documents — when a cited section stopped resolving, when a binding rule was amended. Neither date is in the diff, and neither is guessable from it.

> **A finding the project owes itself is not a change request.**

Where a document moves, [org/normative.md](../org/normative.md) § 7 puts the remaining work on the project: every gap is covered by an open item carrying that document's tag, filed by the reconciliation pass that follows the amendment. Work that pass has not done is the project's debt, and a review that hands it to whoever's request happened to touch the same files is charging an author for it. That is not hypothetical: the observed change was written when no such pass had run and no items existed to point at, so nobody had asked the author — or anyone — to bring the tree back into step.

So the review **files** it and says so, rather than requesting it: the finding is real, the item carries it, and the response names the item. Where the change genuinely depends on that reconciliation, the item is declared a blocker and the resolution stops as blocked on it ([resolution.md](../resolution.md) § Blocked on items) instead of absorbing it.

### The findings vocabulary is closed

A measuring step's record carries findings, each of one kind. The set is closed; a finding fitting none of these means the vocabulary is wrong, not that a seventh may be invented in prose:

| Finding | Means | Evidence it carries | What it does to the verdict |
|---|---|---|---|
| **stale-base** | The branch cannot merge, or the mainline has superseded the work | The merge-base, the intervening commits, the conflict | Blocks: nothing downstream is measured against the tree that would exist |
| **misread-normative** | The change cites a document as justification for something the document does not say | The citation, and the quoted passage | Blocks |
| **contradicts-normative** | The change contradicts a document binding here, whether or not it cites it | The document, the rule, and the contradicting code | Blocks |
| **fails-acceptance** | The acceptance program the document names does not pass against the change | The command and its output | Blocks |
| **defect** | Anything else the review would have the author change | Path and line, or command and output | Blocks unless the judging step says otherwise |
| **question** | Something whose answer could reasonably be "yes, we need that" | What was observed, and what the answer changes | Does not block |

**`misread-normative` and `contradicts-normative` are separate findings because the author's position differs.** One believed they were complying and was reading the wrong sentence; the other worked from a document that does not bind here, or consulted none. The change requested is different, and so is the response.

### Where judgement lives

Situate the branch, establish provenance and verify doc references spend no agent prompt: they are git plumbing, path and section existence, and counts. They run first because they are cheap and because their output is the context every later step reads.

The remaining steps spend a prompt on a decision, and the mechanical checks they rely on are **instruments, not steps**, for the reason [issue-flow.md](../issue-flow.md) gives for the verify command: an instrument used inside a step does not earn a place in the graph. Check conformance runs the formatter and counts annotations before it judges; run the acceptance program builds and executes before it decides whose defect a failure is. Making either half its own step would put a checkpoint where there is no decision.

The split is visible in the result types. The mechanical steps produce `ArtifactJSON`, whose shape is declared with the artifact ([artifacts-and-signals.md](../artifacts-and-signals.md) § A JSON artifact's id names one shape) and read by the steps after them. The judging steps produce `ArtifactMarkdown`, because what they produce is prose for a reader.

### The measuring steps leave the tree as they found it

Every step declares the worktree state it needs and the state it leaves ([resolution.md](../resolution.md) § Steps and the worktree). Every step in the sequence needs the **merge result** and leaves the tree `as-found`. No step branches, commits, or edits, and the write contract's check after each step ([issue-flow.md](../issue-flow.md) § A step's write contract is checked) is what holds it.

**The acceptance program is built outside the worktree.** Its whole point is the consumer's position — a separate project that depends on the change, not a test living inside it — so it is compiled and run in a scratch location, and no build output lands in the tree under review. That is not a convenience: a program built inside the module can resolve names the module never exported, which is the exact defect this step exists to find.

**The acceptance program is not the gate.** The gate measures whether the mainline stays green ([resolution.md](../resolution.md)); the acceptance program measures whether a consumer can use what the change added. A change can pass the gate and fail acceptance — the observed one did, and every test in it passed.

## The steps

### Situate the branch

Establishes, before any diff is read: the merge-base of the request and the mainline; the commits landed on the mainline since that base, with subjects; the intersection between the files those commits touched and the files the request touches; the result of a trial merge (`git merge-tree --write-tree`); and the diffstat, split into new files and modified files.

The base the branch was cut from is in the journal — `open branch` records it — and it is a claim like every other entry the contributor's runs appended. This step derives it, and where the two disagree the tree is right.

*Is this already implemented* and *does this conflict* are cheap to answer and change how everything after them reads. A clean trial merge is a statement about text only: where the intervening commits touch the same subsystem, the record says so, and the suite still has to run against the merge result.

### Establish provenance

Measures when the work was authored against when it was replayed, and what each commit actually introduced.

Author date against committer date, per commit: a gap means the work was authored earlier and replayed, which decides whether a stale reference was correct when written or introduced after the fact. Then what each commit introduced, against what the journal says the producing steps did — the producing phase is three commits by declaration, and a branch carrying one commit under a journal describing three is a discrepancy this step exists to surface. The claims the commit messages make about the tree are carried forward unjudged: reading them is the next steps' work, measuring them is this one's.

### Verify doc references

For every document path and section the changed code cites: whether the file exists at that path in the merge result, whether the cited section still exists in it, and whether the citation was introduced by this request or inherited — the count at the merge-base, on the mainline, and at the request's head. Where a path or section no longer resolves, **when it stopped**: the commit that renamed or deleted it, dated against the commit that wrote the citation.

Renames and deleted sections are invisible to compilers and to a reader of the diff. A section deleted deliberately — a phasing section removed because a specification may not carry status ([org/normative.md](../org/normative.md) § 3) — leaves comments speaking a vocabulary the document no longer has, and nothing in the change looks wrong.

### Verify doc claims

Opens every section the change cites as justification for a decision, and reads it.

This is the step that catches a citation pointing at a real section that says something else, or something narrower, or the opposite. Its finding is `misread-normative`, and the evidence is the quoted passage: a claim that a document does not say what the code says it says is checkable in a minute by anyone, which is what makes it a finding rather than an opinion.

### Check conformance

Routes by what the change touches: every Go file against [org/engineering-guide-go.md](../org/engineering-guide-go.md); every command-line surface — a flag, a subcommand, an exit code — against [org/cli-guide.md](../org/cli-guide.md); and every binding document in the documentation root whose subject the change falls under.

**Conformance is measured against this project's copy, at the version this project holds** — the org corpus as vendored here, at the release [org/stamp.json](../org/stamp.json) names. An author may have been conformant to the copy they had, in a fork or at the time: a rule amended since binds here now and did not bind them then. That is a `contradicts-normative` finding whose response is a sync, not a rebuke, and the two are told apart by dating the amendment against the work — the stamp for the corpus, the document's own history for a rule that changed kind. **A document that stopped permitting what it used to require is the hardest case and the one most likely to be misread as carelessness**: the work followed the document, the document moved, and both facts are true at once.

Mechanically checkable rules first — formatter clean, annotation coverage, forbidden constructs — then the rules that need judgement. **Convention is measured against the tree, not asserted.** "This module has six public declarations and no documentation annotations; the comparable modules are nine of nine and twenty-eight of thirty-two" is evidence a reader can check; "this project documents its public surface" is an assertion the author has no way to test.

Where two binding documents genuinely conflict, the record says so and the review does not resolve it. A conflict between documents is a defect in the documents, filed where the documents live ([org/normative.md](../org/normative.md) § 7) — never settled inside a review of somebody's change.

### Run the acceptance program

Builds and runs the program the normative document names as its acceptance test, from a consumer's position. Its record carries the command, the exit status, and the output.

This is the highest-yield step and the one a diff-reading review never performs. A green suite is a statement about the tests that exist, and the tests that exist were written by the same party that wrote the change — honest, complete, and still unable to see what a consumer sees. The observed export defect is exactly that: every test passed, and nothing outside the module could call it.

**A failure is attributed before it is reported.** If the acceptance program fails because the change is incomplete, that is `fails-acceptance` against the change. If it fails because the program in the document does not compile on its own terms, that is a defect in the document, recorded as such and filed against the document's home — and the change is not blamed for it.

### Check the plan of record

Asks whether the change places code somewhere, or claims a name, that an existing repository, item, or manifest already spoke for.

This is the question an outside contributor cannot answer from where they stand: what else the project has spoken for is not visible from a fork, and the plan step they ran could not have consulted it. Manifest form is evidence — an entry carrying no source location declares an embedded copy, and embedding is a hosting decision, made silently. **Being legal under the specification is not the test.** A specification may permit both forms and the project may still have decided which one it uses; the finding is a `question`, not an assertion that the change is wrong.

### State coverage

Names what was measured, and what was not.

Everything the sequence did not measure the review is taking on the journal's word — which is the one thing about this review nobody can reconstruct afterwards, and the reason the statement is a step rather than a closing sentence. It is the sequence's own account of where it stopped measuring, and it is what `review the proposal` weighs its verdict against.

## Rework or reject

[issue-flow.md](../issue-flow.md) names the distinction between its handback and its rejection as "the one worth deciding deliberately" and leaves the test open. The measurements supply it:

> **A proposal is rejected rather than reworked when bringing it to the bar would cost more than resolving the item from the start.**

That is a measurement, and the findings are what it is measured from. A change whose citations are inverted, whose structure answers a document that has since moved, and whose tests cannot see its own surface does not have a blocking set — it has a rewrite. Conducting a rewrite through rework rounds is the most expensive way to perform one: every round costs another walk of the sequence, the contributor re-derives the intent from review comments, and the rounds accumulate on a branch whose plan no longer describes it.

**The measurement is of the project's cost, not the contributor's effort** — and the two are unrelated. The cost is often highest exactly where the contributor did everything they knew to do, because a change built carefully on a premise that has moved is one whose careful parts all rest on the premise.

**Rejection ends the proposal, not the item.** The item stays, its plan is rewritten, and the next resolution starts from a plan that knows what the last one found. Where the contributor wants to carry it, they carry it; the findings are on the item where the next plan step will read them.

## What the contributor's side needs that it does not have

The path this document specifies is not runnable today, and the obstacles are on the contributor's side rather than the maintainer's:

- **An outside contributor cannot hold the contributor role.** The role requires `CapPush` and roles are derived from capabilities detected on the repository ([flow-registration.md](../flow-registration.md) § Roles). An outside contributor has push on their fork and nothing on the project, so the capability probe denies them the only role that can produce a change — the role exists for exactly the party that cannot assume it.
- **An outside contributor cannot claim an item.** Claiming writes to the item, and writing to an item in this project's tracker requires the access the contributor does not have.
- **Push and request are cross-repository.** `open branch` and `open request` assume one repository: the branch is pushed where the item lives, and the request's head is `flow/issue-<N>` on it. A fork's branch is neither, and the `pr-open`, `pr-merged` and `pr-approved` signals are derived by finding the request on that branch name ([github-schema.md](../github-schema.md)).
- **The worktree state the measuring sequence needs has no name.** `Needs` is closed at `any`, `base` and `item-branch` ([flow-registration.md](../flow-registration.md) § Step configuration); every measuring step needs the **merge result**, which is none of the three and which changes underneath the sequence whenever the mainline moves.

## One flow, one binary

[flow-registration.md](../flow-registration.md) requires a binary to register exactly one flow and forbids selecting among graphs, so the measuring sequence is part of the same registered graph as the steps it sits between — elected by a route, with its reasons in the journal, never by a second flow chosen before the journal begins.

## Open questions

- **Which step elects the sequence.** `close branch` is the handoff and the natural place, but it is mechanical and the trust probe is a live query against the backend. The alternative is `review the proposal` electing into the sequence and being re-entered afterwards, which makes the step its own predecessor.
- **Whether a trusted contributor skips the whole sequence.** The argument for is that a journal of records is what `review the proposal` was designed to read. The argument against is `run the acceptance program`, whose finding no journal can carry however trusted its author — a green suite is a statement about the tests that exist, and that is true of a maintainer's suite too.
- **Whether `stale-base` should end the sequence.** It is the one finding that makes later measurements measurements of a tree that will not exist. Continuing means the contributor acts on everything at once; stopping means half the findings may evaporate on rebase.
- **What a re-review reads after a rework round.** The journal holds measurements of a tree that no longer exists. Re-running the sequence is correct and expensive; reusing records keyed by the commits they measured is cheap and easy to get subtly wrong.
- **Whether the response is one comment or many.** One ordered response makes the priority legible; inline comments make a mechanical finding actionable — and sixteen inline comments for one rule is the enumeration the response is supposed to replace.

## Relationship to other documents

- [issue-flow.md](../issue-flow.md) — the graph this sequence sits inside, the roles, the handoff, and the routes `review the proposal` elects.
- [proposals/drive-by-requests.md](drive-by-requests.md) — a request that arrives with no item, which is not this path.
- [proposals/untrusted-sources.md](untrusted-sources.md) — the same premise one boundary earlier, the trust test, and the step declaration every measuring step makes.
- [flow-registration.md](../flow-registration.md) — where the roles, the graph, and the worktree states are declared.
- [artifacts-and-signals.md](../artifacts-and-signals.md) — the journal the two principals share, and the result kinds the steps produce.
- [org/normative.md](../org/normative.md) — what binds here, which is what `check conformance` measures against, and the reconciliation invariant a review may not charge to an author.
