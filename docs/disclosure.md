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

## What it is asked, and what it answers

The guard is asked one question, by whatever is about to publish:

> **(the text, where the text came from) → allow or refuse**

**The flow declares that seam and cannot fill it.** It is a shape — an interface — and the thing that actually enforces anything is supplied from outside and injected, the same way a backend and an agent are.

That is not an implementation preference; it is what the authorship rule requires. A concrete implementation living in the flow would be code inside a tree that agents edit, rebuildable by the party it refuses. A shape the flow declares and something else fills means the flow can state *that* every write is checked without owning *what* the check permits. **The guarded project has no control over it**, which is the whole requirement, and it is satisfied by the dependency direction rather than by anyone's discipline.

Three things about the signature carry the rest of the design.

**The caller supplies the origin. The guard never infers it.** This is what makes a definitive answer possible without a model and without guesswork. Provenance is not a property of text — a paragraph about another project's architecture reads exactly like a paragraph about this one — but it *is* known to whoever is about to publish. A backend writing an artifact knows it came from an agent turn in this worktree. A pre-tool hook knows which repository the command is running in. The party that has the fact states it, and the guard decides from it.

**The set of origins is closed.**

| Origin | Text that came from |
|---|---|
| `worktree` | The tree under resolution — an agent's turn within it, or a file read from it |
| `item` | The item being resolved: its title, body, its comments. Already published where this is going |
| `flow` | The SDK itself — templates, headings, its own error strings |
| `operator` | A person, typed |
| `elsewhere` | Anywhere else: another repository, another session, another machine |

Closed, because an open set is one a caller extends by inventing a name that means "fine, probably". Text that fits none of these is not a new origin — it is `elsewhere`, which is the member that exists to have somewhere honest to put it.

**There is no `unknown`.** A caller that cannot state an origin cannot make the call, and a guard that is not called does not permit anything — the write does not happen. Providing an `unknown` member would turn the one case this guard exists for into a value that can be passed, and a value that can be passed is a value someone defaults to.

**An origin that cannot be stated is a refusal.** Not an error and not a default-allow: unattributable text is exactly the case this exists for, and a guard that waved it through would be safe only for text that was never at risk.

**It answers fast enough to sit on every path it is on.** One of its callers runs on every commit, over every staged file; another runs on every tool call an agent makes. A guard that adds a visible pause to those is a guard someone turns off, and a guard that is off is indistinguishable from one that was never written. That rules out a model call and a network round trip in any implementation — not as a preference, but because the alternative is the guard's own removal.

**The answer is definitive.** Allow or refuse — no confidence, no maybe, nothing for a caller to weigh, and no allowed-plus-a-warning. A guard that returns a degree of concern has moved the decision to the caller, and the caller is the party trying to publish. A refusal carries its reason; permission carries nothing, because there is nothing a caller should do differently when the answer is yes.

## It is wired in, not launched

It is a call, not a program the flow execs, and it needs no protocol for receiving its subject: the text and its origin reach it as values, because it is a step in the code that performs the act.

**A flow with nothing injected publishes nothing that needs guarding — or does not publish.** Whether an unfilled seam is an error at construction or a refusal at the first write is a question for the implementation, but it is not silently permissive: an interface that defaults to allow is an interface whose whole purpose is optional.

That is not merely simpler than an external program. It is what satisfies the rule every guard is under: **a guard must not be authored by the party it constrains**, because a guard leaves no review window — one weakened at a step authorises the next step immediately, before any review exists, and the run in which it was bypassed looks exactly like the run in which it had nothing to refuse.

This guard has a second reason on top of that one, and it is why it is worth the strongest available form. A gate over a tree may be a project artifact because a wrong answer is correctable: the measurement persists, the judgement is recomputable, and a change wrongly passed can be reverted. **A disclosure wrongly allowed cannot be.** For other guards, editing the guard costs a review that would have caught it. Here there is nothing left to catch.

Being wired into the SDK satisfies both by construction. A flow is delivered from outside the tree it resolves, so a guard inside the flow is, with no further arrangement, something the subject cannot author.

**A project may tighten what counts as private for it. It may not supply the thing that decides.**

**There is no bypass to configure.** A guard that can be switched off for a run is a guard whose refusal is optional, and the party who most wants it off is the party it refuses.

## Where it sits

Text reaches GitHub by two paths, and the guard stands on both. It is **one guard in two positions**, not two policies — a rule that applied to one path and not the other would be a rule about who is writing rather than about what is published.

| Path | Position | Who is writing |
|---|---|---|
| The SDK's own writes | The seam between the github backend and GitHub | A resolution step |
| **A tool call** | The pre-tool hook | **Any agent with a terminal — including one working with a person** |

**The first: one seam, not a check per call site.** Every outward write goes through the github backend — the API calls that create and edit comments and labels, the `gh` invocations that open a pull request, the git operations that push a branch. That seam is the one place a byte is both **final** and **not yet sent**, which are the two properties the guard needs. Anywhere earlier is too early: the text is still being assembled, and a template is not what gets published. A guard installed at six call sites is a guard absent from the seventh.

**The second is the path that is easy to forget, because it is not the flow.** An agent at a terminal running `gh issue create`, `gh pr comment` or `git push` publishes exactly as permanently as a resolution does, and reaches none of the SDK's code to do it. It does pass the pre-tool hook — so the same guard applies there, and an agent working alongside a person is bound by it too.

**The hook resolves what the command refers to; it does not stop at the arguments.** Text may be inline (`--body "…"`) or named (`--body-file …`), and a guard that examined only the literal arguments would be one `--body-file /tmp/x` away from irrelevant — every refusal avoidable by writing the text out first. So the hook reads what a command would publish, wherever the command points, and that includes what a push would carry: its commit messages and its diff.

This is the same requirement as *final bytes*, arriving at a different position. A file reference is a template's cousin: something that is not yet the text, standing where the text will be.

Covering only the first would protect the automated path and leave the interactive one open, which is backwards: the interactive path is where an agent has several repositories in context at once.

**The hook's guard must also come from outside the tree.** The rule above is not weaker for being enforced by a hook — a guard an `implement` step can edit is one an agent grants itself. A pre-tool guard built from the project's own tooling satisfies the letter of "there is a guard" and none of its purpose.

**Neither the binary nor its invocation may live in the tree it constrains.** Both halves, and the second is the one that is easy to satisfy on paper and not in fact.

A guard whose binary is unreachable but whose *invocation* is editable is not enforced — it is requested. If the hook that names the binary lives inside the guarded tree, an agent removes the line and the guard is gone, with no diff anywhere to show that it ever ran. A gitignored configuration file is the worst version of this: the removal is not merely unreviewed, it is unrecorded.

So the binary is referenced at a path the guarded tree does not write, and the wiring that names it lives somewhere that tree does not contain.

**One supplier, and a project does not build its own.** Which projects receive it is decided where it is supplied, not where it lands — a project neither opts itself in nor opts itself out.

Note what this dissolves rather than resolves. If two parties wrote a guard to the same path, something would have to adjudicate: precedence, build ordering, or a refusal to overwrite. All three are decisions made *by a build*, and the build of the guarded tree is edited by the party being guarded. Separate paths mean there is nothing to adjudicate, and a rule that is unnecessary is stronger than one enforced correctly.

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
