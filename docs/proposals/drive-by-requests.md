# Drive-by requests

**Proposal. Not normative.** A pull request that arrives with no item, and why the answer to one is an item rather than a merge.

> **A change lands by resolving an item. There is no second path.**

[proposals/outside-contribution.md](outside-contribution.md) specifies that path for a contributor the project does not trust: the item is the project's, the journal is shared, the contributor produces and proposes, the maintainer measures and lands. This document is about what arrives outside it — a request with no item behind it — and its answer is short, because the arithmetic is.

## What a drive-by request is missing

Not politeness, and not quality. **Every mechanism this project uses to make a change cheap to accept is item-scoped**, and a request with no item has none of them:

- **No plan.** The intent exists only in the code, and the plan step exists precisely because intent written after the code is a description of work done ([issue-flow.md](../issue-flow.md) § Plan).
- **No producing phases.** Implement, review and coverage are three passes and three commits, so what was built, what was corrected, and what was tested are separable. One commit is one undifferentiated act.
- **No gate result.** Nothing was measured by this project's gate before the request was opened, so the request is a guess about whether it can land.
- **No journal.** There is no item to attach one to, so nothing the review measures has anywhere to be recorded, and a second reviewer starts from zero.
- **Nothing carrying reconciliation debt.** A change that no item asked for leaves no trace of why the code exists, and nothing carries the gaps it opens ([org/normative.md](../org/normative.md) § 7).

## Retrofitting costs more than resolving

> **The effort to make a drive-by request landable is bounded below by the effort of resolving the item, and normally exceeds it.**

It is bounded below because every step of the resolution has to happen anyway: the change still needs a plan it answers to, a review pass, coverage to this project's standard, and a measurement under this project's gate. None of that is skipped by the code already existing.

It exceeds because **the retrofit runs backwards.** A resolution starts from intent and produces an artifact that fits by construction. A retrofit starts from an artifact and has to recover the intent — and a diff cannot say what its author decided *not* to do, which is most of what a plan is. What comes back from that reconstruction is a guess, and every later step measures against the guess.

**The shortcut is the expensive option, not the cheap one.** Landing a request without the passes that produce quality does not save the work; it defers it, into a tree where the change is now load-bearing, its intent is unrecorded, and the next person to touch it pays for all of it. That is the quality regression, and it is paid with interest.

The observed case is [promise-language/promise#25](https://github.com/promise-language/promise/pull/25), reviewed at length in [proposals/outside-contribution.md](outside-contribution.md) § The problem, observed. That review was thorough, it was correct, and what it established was that landing the request would cost more than doing the work from an item — which is the conclusion this document generalises rather than an unlucky instance of it.

## The value is the observation, and the observation is cheap to keep

**What is valuable in a drive-by request is almost never the code.** It is that somebody noticed something: a gap, a defect, a need the project had not written down. That observation costs a minute to capture and it is not attached to the diff.

So the answer is to **file the item** — the observation, the evidence, and the request as a reference — and let the ordinary path resolve it. Credit for the finding stays with the person who found it, in the item, where the next plan step will read it.

**Then close the request.** Not because it is poor work: because it arrives outside the only path that exists, and because the item now carries everything about it that was worth carrying. The response says exactly that, names the item, and does not disparage the change.

## Even the exception goes through the item

A change can be small enough that its intent is fully legible from the change itself — a typo in a document, a one-line fix whose oracle is obvious — and it would be absurd to redo it by hand for the ceremony.

> **Where such a change is landed, the item is filed first and the change answers to it.**

That keeps one path rather than two. The item is what a change answers to, what carries its reconciliation debt, and what a later reader consults to find out why the code exists; a change that answers to nothing has none of that, however small it was on the day. **The exception is about who writes the code, never about whether the item exists.**

## What this is not

- **Not a judgement of the contributor.** Someone who opens a request has spent real effort on a project that is not theirs, and the response says so.
- **Not a claim that outside people cannot contribute.** They can, through [proposals/outside-contribution.md](outside-contribution.md), which is the path this project should light.
- **Not a rule that the code is thrown away.** Whoever resolves the item may read the request, and a resolution that arrives at the same code having started from a plan is a better version of it.

## What this needs that does not exist

**There is no item, so there is nothing to journal against.** Whatever mechanism answers a drive-by request — a step, or a documented maintainer practice — its first act is to create the item, and until it exists the flow has nothing to attach a claim, a route, or a result to ([artifacts-and-signals.md](../artifacts-and-signals.md) § The record).

## Open questions

- **Whether this is a flow at all.** Triage could be a step whose product is the filed items, which makes the decision recorded and reviewable like every other; or it could be a contribution rule and a maintainer's judgement, needing no graph. The argument for a step is that "was anything valuable here" is a decision, and decisions are what flows record; the argument against is that it runs once, on an object the tracker does not model.
- **Where the rule is stated to contributors.** It is a contribution rule before it is a flow, and a project that closes requests without having said this in writing is answering people with a policy they had no way to read.
- **Whether an item filed from a request is answerable automatically.** The request's text is written by an untrusted party and would seed an item's text ([proposals/untrusted-sources.md](untrusted-sources.md)), so the item it produces is subject to the same acceptance question as any other.

## Relationship to other documents

- [proposals/outside-contribution.md](outside-contribution.md) — the path a change actually takes, and the measurements a maintainer makes on it.
- [issue-flow.md](../issue-flow.md) — the producing phases and the filing shape an item's resolution uses.
- [proposals/untrusted-sources.md](untrusted-sources.md) — the acceptance question an item filed from a request inherits.
- [org/normative.md](../org/normative.md) — the reconciliation invariant a change with no item cannot satisfy.
