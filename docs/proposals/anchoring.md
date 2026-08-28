# Anchoring

**Proposal. Not normative.** A sketch of a mechanism the project does not have.

Some parts of a project are not ordinary code. Changing them changes what "correct" means for every change that follows — and an agent resolving an item can change them as easily as it changes a comment.

An **anchored** aspect is one whose modification requires a person to approve it, specifically and at the time.

## The problem, observed

Two consecutive resolutions amended a normative document. Both amendments were improvements, and both were made by an agent whose work was being judged against the document it edited:

- `#49` tightened the origin rule while implementing the origin rule.
- `#46` rewrote the required-set table and the three-party table while implementing the third party.

Nothing stopped either, nothing flagged either, and neither was mentioned in a step's report. They surfaced because a human happened to read the diffstat and ask what a `docs/` change was doing in a code resolution.

**The failure is not that an agent edited a document.** It is that the edit is invisible at the moment it matters. A forty-line spec change inside a fifteen-hundred-line diff is not what a reviewer's attention is on, and the change most worth reading is the one that makes the rest of the diff acceptable.

Note what it is not. Neither amendment was self-serving, and a rule built on the assumption of bad faith would be the wrong rule. The problem is that *nothing distinguishes* the two cases: a resolution that improves a specification and a resolution that relaxes one to fit what it built produce the same shape of diff, and only reading the reasoning tells them apart. Anchoring is a mechanism for making sure someone reads it.

## What may be anchored

Aspects, not files. The unit is whatever a person would want to be told about:

| Aspect | Example |
|---|---|
| A whole document | `docs/resolution.md` |
| A section within one | `## Gates` and its body |
| A declared interface | `flow.Backend`, `flow.Worktree` |
| An exported surface | Everything a consumer can call |
| A specific value | A cap, a baseline, a timeout the project chose deliberately |

The last row already exists as a special case. `docs/gates-and-commands.md` says a resolution's diff may not contain the baseline — that is an anchor whose approval is never granted to an agent, written as a one-off rule because there was no general mechanism to express it. Anchoring generalises it, which is a point in the design's favour: it is not a new idea, it is the idea already present in one place made available everywhere.

## Approval

**A person approves. Nothing else does.** The party proposing a change to an anchor is exactly the party that must not be able to authorise it — the same rule that governs thresholds, disclosure overrides, and guard authorship.

A request to change an anchor carries:

- **what would change** — the diff against the anchored aspect, alone, not embedded in the rest of the work;
- **why** — the reasoning, which is the thing actually being reviewed;
- **what it unblocks** — the change that cannot land without it.

Presenting the anchor diff *separately* is most of the value. The reviewer's question is "should this document say something different", which is a different question from "is this implementation right", and answering it inside a large diff means answering it badly.

**Scope of a grant.** An approval covers the aspect and the change described. Whether it then extends to further changes in the same session is a real choice:

- **Per-change** — every subsequent edit re-asks. Safest, and it makes an agent that is iterating on a document ask repeatedly for what a person has already agreed to.
- **Per-session, per-aspect** — approving a change to `docs/resolution.md` lets that session keep editing that document. Practical, and it means the third edit lands on the strength of a decision made about the first.

A middle form is probably right: the grant is per-aspect and lasts the session, and the *final* state of the anchored aspect is presented again before the change lands — so incremental editing is cheap, and what a person actually approved is what actually ships.

**An approval is recorded with what it permitted**, so a later reader sees that a person decided and what they decided about. An anchor change with no recorded approval is indistinguishable from one that was never asked.

## Enforcement, at more than one level

The two positions `docs/resolution.md` already defines, doing what they are respectively good at.

**A guard, at the edit.** The agent proposes to modify an anchored aspect and is refused before it happens, with the reason and the way to ask. Cheap, immediate, and the agent adapts mid-turn rather than losing the work. It **fails open**: an agent that reaches a shell, or runs somewhere the guard is not configured, goes straight past it.

**A gate, at integration.** Compares the anchored aspects as they stand against **the state the resolution started from**, and refuses the change if any differ without a recorded approval. Catches the modification however it happened, including by routes nobody anticipated.

Neither substitutes for the other, which is the standing argument for having both: the guard is bypassable and the gate costs a whole resolution before it speaks.

**Against the originating state, not against the mainline.** This is the detail most likely to be got wrong. The mainline may have moved during the resolution — someone else's approved change to the same document is legitimate and must not be reported as this resolution's violation. What the gate measures is *what this change modified*, which means the resolution has to record the anchored aspects' state when it began.

## The self-reference problem

**The anchor declaration must itself be anchored**, or the first move available to an agent is to unanchor what it wants to change.

That cannot be solved by putting the declaration outside the tree: an anchor names things in the tree and moves with them, so a declaration held elsewhere would drift from what it protects — the same reason a threshold versioned with its subject is right and a per-host copy is wrong.

So the declaration lives in the tree and is anchored to itself: changing it requires approval, recursively, and the recursion terminates because the approver is a person. That is sound but it is exactly the kind of construction that is easy to implement subtly wrong, and it should be built with that in mind.

## What this is not

**Not immutability.** An anchored aspect is one that changes deliberately, not one that never changes. A mechanism that made normative documents unchangeable would make them wrong instead.

**Not a substitute for review.** It decides what must be looked at, not whether it is good.

**Not the write contract.** [#33](https://github.com/promise-language/flow/issues/33) is about whether a step wrote only what it said it would — a claim about a step's declaration. Anchoring is about whether an aspect may be changed at all, by anyone, regardless of what was declared. A step could honestly declare that it will edit `docs/resolution.md` and still need approval to do it.

**Not a file permission system.** The unit is an aspect a person cares about. Anchoring every file would produce a project where nothing can be changed without an interruption, which is a mechanism people turn off.

## Open questions

- **How an anchor is declared**, and in what syntax. A section anchor has to survive the section being renamed or moved, which a line-range cannot and a heading name barely can.
- **Whether approval is per-aspect or per-hunk.** Per-hunk is precise and noisy; per-aspect is coarse and might approve more than was read.
- **What a person sees.** The value is concentrated here: an anchor request that is hard to read gets approved reflexively, which is worse than no anchor because it manufactures a record of a decision nobody made.
- **Whether an agent may propose the anchor set.** Adding an anchor is safe in a way removing one is not, so the two directions may not need the same approval.
