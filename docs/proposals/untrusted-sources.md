# Untrusted sources

**Proposal. Not normative.** A gate the project does not have, on an input it already executes.

Most repositories this flow drives are public, so anyone with an account can file an item on them. Nothing today asks who wrote one.

## The rule

> **Under automated resolution, item text is executable input, and accepting an item is granting execution.**

Everything below follows from that sentence, and it is worth stating in the plainest available words because it is not visible from the code. An issue body reads like a description of work. It is not: the plan step reads it, the implement step builds what it says, and the agent that does both runs with the operator's credentials.

**This is the inbound counterpart to [disclosure.md](../disclosure.md), and that document already contains the assumption this one attacks.** Its closed set of origins vouches for item text like this:

| Origin | Text that came from | Vouched for by |
|---|---|---|
| `item` | The item being resolved: title, body, comments | The destination itself — it is already published there |

That is exactly right for the question disclosure asks. Re-publishing text that is already public on the destination leaks nothing, so the item is the one origin that needs no scrutiny **on the way out**. It says nothing whatever about the same bytes on the way in. One set of bytes, two questions, and only one of them has ever been asked.

## The merge is not the control

The tempting reasoning is that an untrusted item is safe because a contributor flow stops at a proposed pull request, and a person reviews before anything lands. That protects **the default branch**, and the default branch is not the asset most at risk.

By the time a pull request exists, the agent has already run: in a worktree, authenticated as the operator, on a machine that holds other checkouts of other projects. Before any human sees anything, text from the item can direct it to

- push branches, open or comment on issues and pull requests, and edit item bodies **in any repository the token reaches**, not only this one;
- read the filesystem — sibling arenas, configuration directories, tokens, keys — and write what it finds into a public comment, a branch, or a commit message;
- create or delete releases and tags, on a repository whose releases may be how a fleet updates its own tooling;
- spend money: a resolution costs real budget and real time, so filing items is an unauthenticated way to spend someone else's.

A review at the end of that sequence is not a control over any of it. **The amplifier is that dispatch is automatic**: runners poll and take work, and the fleet exists precisely so that items are picked up without a person deciding to pick each one up. *Someone would notice* is the assumption the design removes.

## Trust is a property of the source

An item's **source** is the account that wrote a given piece of its text. A source is **trusted** when it holds write, maintain or admin permission on the repository, and **untrusted** otherwise.

**Capability is the question, so capability is what is checked.** The probe is the one already used for role detection, and it is not GitHub's `author_association`: association describes a relationship — `MEMBER`, `CONTRIBUTOR`, `NONE` — and a relationship is not a permission. A former maintainer, a fork's owner, and an outside collaborator with write access are sorted correctly by one of these and wrongly by the other.

**Trust is not a judgement about a person.** It is the answer to *could this account already do this by hand?* An account with write access can push a branch and edit a workflow without going through an item at all, so admitting its text to an agent grants nothing it did not have. That equivalence is the whole justification, and it is why the test is capability on **this repository** rather than reputation, history, or membership anywhere else.

## Every piece of text has its own source

This is the part most easily implemented incompletely, because *an item* reads like one thing and is not. The flow reads the body, the title, and **the comments** — comments are context an agent is given, and steering a run by leaving one is ordinary practice, not an edge case.

> **Every piece of item text an agent is given comes from a trusted source, or the item is not accepted.**

A trusted author's item carrying one untrusted comment delivers untrusted instructions through exactly the same path as an untrusted body. Any rule that checks the body alone is a rule that can be walked around by replying to somebody else's issue.

The alternative — accept the item, and filter untrusted comments out of the context instead — is possible and is not proposed here. A filter that misses one field fails **silently** and looks identical to one that works, and the fields it must cover grow every time the flow reads something new. A rule about the whole item fails loudly and in one place.

## The trust block

An item whose text has an untrusted source is **blocked**, and it is blocked for a reason distinct from the three that exist.

`BlockKind` names *who must act, and on what.* The existing members are `waits-on-items`, `waits-on-condition`, and `waits-on-person`. A trust block is none of them:

- It is not `waits-on-items` — nothing else has to finish.
- It is not `waits-on-condition` — it will never clear on its own, and there is nothing to come back to.
- It is not `waits-on-person`, and this is the one worth arguing. Today a person is the only party who can accept. That is a fact about what exists, not about what the state means: **acceptance is a decision about text, and a decision about text is the kind of thing that can be automated.** A reviewing flow that reaches a verdict without a human is a coherent future, and folding this into `waits-on-person` would make that future a contradiction in the vocabulary rather than a feature.

So the set gains a member. The name proposed is **`waits-on-acceptance`**, matching the `waits-on-` form the others take and naming what must happen rather than who must do it. Its actor is whoever may accept, and its subject is the item's own text.

Reporting it as `blocked` is not a placement chosen for convenience. It is exactly what `blocked` already means — *in this binary's remit, and not workable by anyone right now* — and it buys the rest of the behaviour from rules that already exist. `ListAutoSelectable` must not return a blocked item, so an unaccepted item is out of the selectable set without a second rule saying so. `list` reports it with its kind and reason, so a maintainer asking why their bug is not being worked gets an answer rather than an absence.

## Acceptance is a flow

Accepting an item is work: reading text, judging it, and recording a verdict. Work is what flows are for, so **acceptance is a flow whose product is a decision about text** rather than a side channel bolted onto `claim`.

That has a consequence worth making explicit, because it is the trap this whole document is about: **the accepting flow reads untrusted text.** It cannot do otherwise — judging text means looking at it. So the flow that grants execution is itself exposed to what it is judging, and it is the one place in the system where that exposure is intended.

Which means the exposure has to be bounded by what the flow may *do*, since it cannot be bounded by what the flow may *read*.

> **A step declares whether it may read untrusted text. The declaration is explicit, and the default is that it may not.**

This mirrors the existing rule that a step declares whether it may modify the worktree, and for the same reason: a capability acquired by omission is a capability nobody decided to grant. Under that rule an ordinary implement step cannot run against unaccepted text at all — not because a gate refuses it, but because it never declared that it could.

## The permit

Running the accepting flow requires an explicit flag, proposed as **`--untrusted-source`**.

**It permits; it does not force.** That distinction is why it does not join the `--force-*` family and is not a `ClaimOverride`. Every existing override bypasses a check that has already run and answered — a dirty tree, a held claim, a stale base — and each says *I have seen this and I am proceeding anyway*. This flag answers a different question. Nothing has refused anything yet; the flag states what the invocation **is**, and in doing so enables steps that are otherwise unavailable. An override subtracts a safeguard. A permit selects a mode.

Stating it in the invocation rather than inferring it is the same requirement that applies to every other drive-model decision in this system: a capability that arrives through an environment variable is invisible to the command, to its help text, and to review.

## Where acceptance is recorded

> **Acceptance is not recorded anywhere the untrusted party can write it.**

If the record lives on the item, and the item's author can alter the item, then the gate authorises itself. That is not a hypothetical about labels or fields; it is the general property, and it is why the mechanism cannot be chosen by this document.

**The orchestrator owns the mechanism**, and what it owes is a guarantee rather than an implementation:

> **An acceptance record establishes both that a trusted source produced it and that the text it covers has not changed since.**

How is the orchestrator's own business, and the two obvious shapes are genuinely different:

- An orchestrator with **protected storage** — a store the item's author cannot reach — records acceptance there and the guarantee is the storage's.
- An orchestrator whose store **is the item**, as GitHub's is, cannot rely on location and must rely on **verifiable content**: an acceptance signed by the accepting party, over the exact text accepted, so that a reader establishes origin and integrity from the record itself rather than from where it was found.

An orchestrator that can offer neither refuses to accept anything, and reports that it cannot. That is an answer; silently treating everything as accepted is not.

## Acceptance names the text it accepted

Acceptance is a decision about specific bytes, so it must say which bytes.

The reason is not bookkeeping. **If acceptance is recorded over the text, then "the text changed" and "the item is no longer accepted" are the same fact**, established by the same check, at the moment the check runs. Nothing has to notice an edit and revoke anything. A new comment from an untrusted account, or an edit to a body after acceptance, leaves the record covering text that is no longer what is there — and it stops verifying on its own.

That self-invalidating property is the argument for a record over the text and against a plain marker. A marker has to be revoked by someone who noticed; a record over the text is falsified by the change itself. The failure modes differ in the way that matters: a marker fails silently open, a record fails closed.

**Verification happens before each dispatch, and nothing in flight is interrupted.** One invocation advances at most one step, so the boundary between steps is where the question is asked. An item whose text changed under a running resolution finishes the step it is in and takes no further one.

**The cost of this is real and is accepted deliberately.** Anyone who can comment can return an item to blocked, including one that is mid-resolution — a denial of service available to any account. It is chosen over the alternative because it is loud, visible in the listing with its reason, and cleared by the same act that accepted the item in the first place. The alternative is executing text that nobody approved.

## What the surface does

- **`list`** reports an unaccepted item as `blocked`, with the trust block's kind and a reason. It is not hidden: an item nobody can see is an item nobody can accept.
- **Auto-selection** never picks it, by the existing rule that a blocked item is not in the selectable set.
- **`claim` and `resolve` refuse it**, naming the block and how it is cleared. A refusal, not a silent skip — a maintainer whose item is not being worked deserves the reason.
- **The refusal is item-scoped.** Nothing is wrong with the arena, and a driver working a queue tries the next item rather than stopping.
- A person claiming the item by name is refused for the same reason a runner is. Driving an unaccepted item by hand still runs an agent against the same text; the hand on the keyboard is not the control, the acceptance is.

## Open questions

- **The block kind's name.** `waits-on-acceptance` is proposed; the members it joins are named for what is waited on, and a better name may exist.
- **Whether acceptance can be standing rather than per-item.** Accepting an account's items in advance is the obvious ergonomic request and a materially larger grant: it accepts text that does not exist yet, which is the one thing a record over the text cannot cover.
- **What the accepting flow produces**, and whether its verdict may be reached without a person. The vocabulary above deliberately leaves room for both; nothing here specifies the flow's steps.
- **Whether an untrusted comment blocks the whole item or only itself.** This document takes the whole item, on the grounds that a filter fails silently. The narrower rule is defensible if the filtering is provably total.
- **What happens to an item accepted, worked, and then edited after its pull request exists.** The proposal stops at dispatch; the request is already published by then.

## Relationship to other items

- **#181** selects *which* flow runs on an item already being worked. This is prior to it: whether the item may be worked automatically at all. #181's rule needs this one, because "unresolved item" is satisfied by an issue filed sixty seconds ago by a stranger.
- **#182** — environment variables must not select behaviour. `--untrusted-source` is stated in the invocation for that reason.
- **#174** — multi-role resolution. Acceptance is a role's decision and wants a home in that document.

## Relationship to other documents

- [disclosure.md](../disclosure.md) — the outbound guard, and the origin table this document qualifies.
- [resolution.md](../resolution.md) — the lifecycle acceptance sits in front of, and the step-declaration rule this one mirrors.
- [orchestrator.md](../orchestrator.md) — where the `BlockKind` member and the acceptance-record contract would land.
- [cli.md](../cli.md) — where the permit, the refusals, and the listing behaviour would land.
- [github-schema.md](../github-schema.md) — where the GitHub orchestrator's own acceptance record would be described.
