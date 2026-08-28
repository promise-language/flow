# Disclosure

**Normative.** What the flow sends outward, and what stands in its way.

A resolution reads a private machine and writes to a public place. `docs/resolution.md` governs what a step may change; this document governs what a step may **publish**, which is a different question with a different failure: a bad change is caught by review, and a disclosure is permanent the moment it is made.

## The rule

**Every byte the flow sends outward passes a disclosure guard first, and nothing sends without one.**

Not "text an agent wrote" — every byte, including text the SDK composed itself. A template that interpolates a path an agent supplied publishes that path just as surely as prose would, and the SDK's own error strings are the most reliable source of absolute paths in the system.

## The surface

The set is closed. A write not on this list does not exist, and adding one means adding it here.

| What | Carries |
|---|---|
| **Issue comments** | Step artifacts — the plan, the review, the coverage briefing |
| **The parked question** | An agent's own account of what it could not decide, quoting whatever it was looking at |
| **The state comment** | The flow's bookkeeping, rewritten on every step |
| **The pull request** | Title, body, and the artifacts assembled into it |
| **Commit messages** | Text an agent wrote, published by the push |
| **The diff** | The change itself, published by the push |
| **Labels** | Names the flow constructs, including claim identifiers |

The last three are the ones most easily forgotten, because they reach the public surface through git rather than through an API call. They are disclosures all the same: a push is a publication.

## A guard is not a gate

`docs/resolution.md` defines a **gate**: it measures something that exists and reports what it found. A **guard** is a different thing, and the flow has both.

**A guard stands on the execution path of an act that has not happened yet, and prevents it.**

| | Gate | Guard |
|---|---|---|
| **Subject** | **Something that persists independently of the check** | **The act itself, which exists only as a proposal** |
| Position | Beside the work. It observes | **On the path.** The act cannot occur without passing it |
| Answers with | Measurements. Something else judges them | Allow or refuse, and why |
| Its answer | Is stored, ratcheted, re-judged later | Is consumed once, by the caller about to act |

The subject is the distinction, and the rest follows from it — see `docs/resolution.md`. Prevention is not the test: a gate that blocks a push prevents something too, and is still a gate, because its subject persists and its measurement is judged later.

`bin/guard` is the precedent, and the name comes from it: it sits on the critical path of every tool call, receives the action proposed, and answers whether it proceeds. It is not a measuring gate and does not pretend to be one.

**A guard is its own judge, and that is not the contradiction it looks like.** Judging is kept out of a gate because a measurement is stored and re-judged later, so the comparison must be recomputable by someone who was not there, against thresholds held elsewhere. A guard's answer is consumed the moment it is given, is never stored as a fact about anything, and is never re-judged. There is no second judgement to keep honest.

**Being on the path is the whole mechanism.** A guard that is consulted is a convention; a guard that cannot be gone around is a guarantee. If a caller can reach GitHub without passing it, it describes what usually happens rather than what can.

## What it must hold

**It never modifies what it examines.** A guard that redacted and forwarded would be worse than none: the author would never learn what was caught, the same disclosure would be re-proposed every time, and nobody could tell text that was clean from text that was quietly rewritten. **A refusal returns the text to whoever wrote it.** That is the only outcome that improves anything.

**It sees the final bytes.** Not the template, not the artifact before assembly, not the prose before the SDK wrapped it in a heading. Anything examined before its last transformation can have content introduced after the examination, and a guard that ran a step too early is worse than none, because it reports a safety it did not establish.

## It fails closed

This inverts the rule for gates that measure a tree.

A gate over a worktree that could not run has measured nothing, and the correct response is to retry: refusing the change would blame it for a dead machine. **A disclosure guard that cannot answer refuses to send.** The asymmetry is in what an error costs. Not publishing something publishable wastes a step; publishing something unpublishable cannot be undone — deletion removes it from the page and not from anyone's index, mail, or memory.

## It is wired in, not launched

The guard is **part of the SDK, on the path from the backend to GitHub.** It is not a program the flow execs, and it needs no protocol for receiving its subject: the act reaches it as a value, because it is a step in the code that performs the act.

That is not merely simpler than an external program. It is what satisfies the rule every guard is under: **a guard must not be authored by the party it constrains**, because a guard leaves no review window — one weakened at a step authorises the next step immediately, before any review exists, and the run in which it was bypassed looks exactly like the run in which it had nothing to refuse.

This guard has a second reason on top of that one, and it is why it is worth the strongest available form. A gate over a tree may be a project artifact because a wrong answer is correctable: the measurement persists, the judgement is recomputable, and a change wrongly passed can be reverted. **A disclosure wrongly allowed cannot be.** For other guards, editing the guard costs a review that would have caught it. Here there is nothing left to catch.

Being wired into the SDK satisfies both by construction. A flow is delivered from outside the tree it resolves, so a guard inside the flow is, with no further arrangement, something the subject cannot author.

**A project may tighten what counts as private for it. It may not supply the thing that decides.**

**There is no bypass to configure.** A guard that can be switched off for a run is a guard whose refusal is optional, and the party who most wants it off is the party it refuses.

## Where it sits

**At the boundary between the backend and GitHub**, and nowhere else.

Every outward write goes through the github backend: the API calls that create and edit comments and labels, the `gh` invocations that open a pull request, and the git operations that push a branch. That seam is the one place where a byte is both **final** and **not yet sent** — the two properties the guard needs.

Anywhere earlier is too early: the text is still being assembled, and a template is not what gets published. Anywhere later does not exist.

**One seam, not a check per call site.** A guard installed at six call sites is a guard absent from the seventh, and the seventh is the one someone adds later without knowing this document exists.

This also puts it where provenance is still legible. The backend knows which repository it is writing to; a step composing prose does not necessarily know where that prose will end up.

## What a refusal carries

A refusal names **what** it found and **where**, and quotes enough to act on. "This text may contain private information" is indistinguishable from a guard that gave up, and an author who cannot see what was caught will re-propose it.

A refusal is not a failure of the step. The text is revised and re-offered — the same shape as a failing check, and for the same reason: the work is sound and its expression is not.

## Overriding

**A person may override a refusal. Nothing else may.**

Not the agent that wrote the text, not the step, not a retry, not a configuration. The party proposing a disclosure is exactly the party that must not be able to authorise it — the artifact rule, applied to publication rather than to thresholds.

An override is **recorded with the text it permitted**, so a later reader can see that a person decided, and what they decided about.

## What counts as a disclosure

The categories are closed. Each is a thing that is invisible to whoever wrote it, because it was ordinary in the place it came from.

| Category | Why it escapes notice |
|---|---|
| **Local filesystem paths** | `/home/<user>/prog/...` is what every error string and stack trace contains. It names a person and the shape of their machine. |
| **Host and account identifiers** | Machine names, arena names, internal hostnames, addresses. Ordinary in a log, identifying in an issue. |
| **Credentials and tokens** | Rare and catastrophic. Anything that looks like one is refused without judging whether it is live. |
| **Content from outside the repository being written to** | The one that is invisible from inside a single resolution. An agent with several checkouts, or one relaying another project's design, publishes work belonging to a repository with different visibility — and every sentence of it looks like ordinary technical prose. |
| **Text from another session or agent** | A message from a peer is written for that context. Quoting it into a public issue republishes someone else's words somewhere they did not choose. |

The last two share a failure, and it is the reason this is a guard rather than a search for secrets: **nothing about the text itself indicates it should not be published.** A token can be recognised; a paragraph about another project's architecture cannot be told from a paragraph about this one. Only provenance distinguishes them, and provenance is exactly what is lost when text is copied.

## What this is not

**Not a substitute for the repository's own rules about what belongs in it.** A disclosure guard answers "may this text be published at all", not "is this the right issue for it".

**Not a review of correctness.** Text that is wrong but harmless passes. Text that is right and private does not.
