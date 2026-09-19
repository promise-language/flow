# GitHub storage schema

> **Tag:** `github-schema` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how state, the journal, and history are stored on a GitHub issue by the GitHub orchestrator. It is written for readers who have no SDK — the wire format is the contract.

## Items are issues

An item is an issue. GitHub's Issues API returns **pull requests** as well, and the two are numbered out of one space, so a number a person types can name either.

The two paths that meet one answer differently, and the difference is which end the number came from. **`List` skips a pull request** — a listing of what can be worked has nothing to say about one, and an entry nothing will ever take is not an answer. **A by-ref path that takes an item or describes one refuses it** — `Claim`, `Get` and `Load` alike — because a number typed by a person is a number they meant, and by-ref is the only place a pull request number can arrive by accident. The refusal is **item-scoped**: this ref is the problem and another might succeed. Nothing overrides it; no flag can make a pull request into an item.

**A by-ref path that gives a lease back refuses nothing on the kind of thing it names**, and `Release` is the one that matters. A claim record already standing on a pull request is given up exactly as any other is, and a refusal there would make that record permanently unclearable — the check would then preserve the state it exists to prevent. The refusal belongs where the lease is taken, not where it is surrendered.

Labels are one namespace too, so a pull request **can** carry `flow:*` labels. A claim record on one is a defect, not a state this schema defines: `flow:owner:<login>` and `flow:arena:<fingerprint>` mean an arena holds an item, and a pull request is not one.

## State comment

Each issue carries at most one **state comment** — the machine-readable record `Load` returns. It is identified by HTML-comment markers and wrapped in a `<details>` element:

```
<!-- flow:state-v2 begin owner=<login> -->
<details><summary>📋 Flow state — <binary> (machine-managed, do not edit)</summary>

```yaml
<YAML body>
```

</details>
<!-- flow:state-v2 end -->
```

The `owner=<login>` attribute on the `begin` marker records who authored the state comment. When a different user claims the item, a fresh state comment is posted under their identity.

### Schema version

The YAML body carries a `schema` field. The current version is **2**. The version is bumped only on incompatible schema changes.

### YAML fields

| Field | Type | Meaning |
|---|---|---|
| `flow` | string | The binary name that opened this record. |
| `schema` | int | Schema version (currently 2). |
| `journal` | array | The journal: one entry per completed step execution, in order. See below. |
| `ledger` | object | The treasurer's durable record. See below. |
| `signals` | array | Per-signal state entries. See below. |
| `park` | object or null | Current park record, or absent when not parked. |
| `questions` | array | Questions the item is parked on. Written by an ask and dropped with the park that was waiting on it; read back only while `park.kind` is `question`. See below. |
| `filed` | map | Items filed from this item: intended-item key → issue reference, written as each is created. See "Filed items". |
| `finalized` | bool | Whether the item's flow run is complete. |
| `disposition` | string | `resolved` or `rejected`; present only when `finalized` is true. |

### Journal entries

Each entry in the `journal` array is one completed step execution ([resolution.md](resolution.md) § The journal). The array is **append-only**: entries are never rewritten, reordered, or removed, and the last entry's `next` (or `finalize`) is what the pending step is derived from.

| Field | Type | Meaning |
|---|---|---|
| `step` | string | The step's result id (artifact or signal id). |
| `execution` | int | Which completed execution of this step this is: 1 on first completion, counting up as the route returns. |
| `type` | string | Result kind: one of `flag`, `commit_hash`, `markdown`, `json`, `file`, `patch` for an artifact; `signal` for a signal step or wait. |
| `commit_hash` | string | Inline value for `commit_hash` type. |
| `json` | string | Inline value for `json` type. |
| `body_at` | string | URL of the artifact comment carrying the payload, for types stored as comments. |
| `next` | string | The elected successor's step id. Exactly one of `next` and `finalize` is present. |
| `awaits` | string | What the item now awaits: the successor's declared role, or `signal:<id>` when the successor is a signal wait — what the `flow:awaits:<…>` label is maintained from. Present with `next`. |
| `finalize` | string | The elected disposition: `resolved` or `rejected`. |
| `message` | string | The message to the successor — why it is being run. On a finalizing entry, the closing reasons. |
| `note` | string | Standing note addressed to every subsequent step, when one was set. |
| `by` | string | Login of the account that ran the step. |
| `role` | string | The declared role it acted in. |
| `at` | timestamp | When the execution completed. |
| `cost_usd` | float | What the execution spent. |
| `duration_seconds` | float | The execution's active time — waits on declared exclusions excluded, as in the ledger. |

### Ledger

The `ledger` object carries the treasurer's record: a `steps` map keyed by step result id, and item-level totals.

Each entry in `steps`:

| Field | Type | Meaning |
|---|---|---|
| `dispatches` | int | Dispatches of this step, counting attempts that did not complete. |
| `resumptions` | int | Times a park on this step was resumed. |
| `cost_usd_spent` | float | Cost consumed across all dispatches. |
| `duration_seconds` | float | Active time across all dispatches — time doing work; waits are not in it. |
| `waiting_seconds` | float | Time blocked on declared exclusions across all dispatches. |
| `granted` | array | Operator extensions recorded against this step: each with `axis`, `amount`, `at`. |
| `last_run_at` | timestamp | When the last dispatch started. |

Item-level: `total_cost_usd`, `total_duration_seconds`, `total_waiting_seconds`, and `sessions`.

The `sessions` object is the treasurer's count of the agent sessions this resolution opened, kept by **why** each was opened. Item-level and not a row: the session belongs to the resolution, not to whichever step was running when it was opened.

| Field | Type | Meaning |
|---|---|---|
| `declared` | int | Sessions the route asked for: the resolution's first, and one per execution of a step declaring `fresh`. |
| `handle_gone` | int | Sessions opened because the handle was gone — the substrate declined it, or the orchestrator keeps none. Nobody's decision. |
| `refused` | int | Requests to discard a live conversation the route does not account for. **Nothing was opened**; the attempt is recorded so it is not invisible. |

A state comment written before the treasurer counted sessions carries no `sessions` key, and reads back as zeroes.

### Signal entries

Each entry in the `signals` array:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The signal identifier. |
| `set` | bool | Whether the signal is currently set. |
| `observed_at` | timestamp | When the signal was last observed. |
| `observed_via` | string | `side-effect` or `poll`. |

### Question entries

Each entry in the `questions` array:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The orchestrator-assigned question identifier, named by `answer --question`. |
| `header` | string | Short scannable label. |
| `text` | string | The full prompt. |
| `format` | string | One of: `text`, `yes_no`, `choice`. |
| `options` | array | Presentation hints for `choice`; never constrain the answer. |
| `multi_select` | bool | Whether a `choice` question accepts several options. |
| `asked_at` | timestamp | When the question comment was created, on **GitHub's** clock — the clock the replies it is compared against are stamped by. |

A new ask replaces the array rather than appending to it: the field carries the questions currently outstanding, and the question comments carry the history. Answers are not recorded here — the issue thread is the answer store.

The array belongs to the park that is waiting on it, and goes wherever that park does: dropped when the asking step completes, when a park of another kind supersedes it, and on a reset. A record left behind would be inherited by the next question park and presented as its outstanding ask, which `answer` would then accept — an answer to a question nothing is waiting on.

### Park record

When present, the `park` object:

| Field | Type | Meaning |
|---|---|---|
| `kind` | string | One of: `blocked`, `question`, `treasurer-refused`, `step-did-not-complete`, `infra-transient`, `remote-unreachable`, `refused`, `write-contract`, `account-exhausted`. |
| `step` | string | The pending step's result id (artifact or signal id). |
| `axis` | string | The treasurer's axis when `kind=treasurer-refused`. |
| `axes` | array | Full spend snapshot at park time, in the treasurer's vocabulary (each with `axis`, `used`, `granted`, `exhausted`). |
| `clears_at` | timestamp | When `kind=account-exhausted`: the instant the agent account's allowance returns, as the substrate published it ([environment.md](environment.md) § The agent account). **Persisted rather than recomputed** — the run exits, and the arena resumes at this instant; one that did not survive the exit is one nobody can wait out. Absent on every other kind, and **absent never means *soon***. |
| `account` | string | When `kind=account-exhausted`: the agent account the spent allowance belongs to, as the **substrate's opaque identifier and never a readable name** — [disclosure.md](disclosure.md) closes "Host and account identifiers", and this record is published as an issue comment. Absent when nothing on the host could name the account; the park is still written, scoped to nothing rather than to a synthesized key. |
| `reason` | string | Human-readable reason. |
| `details` | string | Additional detail (e.g. question timestamp marker). |
| `parked_at` | timestamp | When the park was recorded. |

## Artifact comments

Each captured artifact (except `flag`, which has no payload) gets its own issue comment, identified by an HTML-comment marker:

```
<!-- flow:artifact id=<id> type=<type> v=<execution> by=<login> ts=<RFC3339> -->
<body>
```

The body format depends on the type:

| Type | Body format |
|---|---|
| `markdown` | Raw markdown text. When spilled, a truncated preview followed by a spill notice. |
| `commit_hash` | `` commit: `<sha>` `` |
| `json` | Fenced `json` code block. |
| `file` | Link to the orphan-branch file with byte count. |
| `patch` | Link to the orphan-branch diff with byte count and base SHA. |

A step the route reaches again produces a further comment (append-only); `v=` carries the execution count, matching the journal entry's `execution`, and the journal entry's `body_at` names the comment that carries its payload.

## Remarks

A remark ([orchestrator.md](orchestrator.md) § Remarks) is an ordinary issue comment opened by the `flow:remark` marker, with the operator's text below it and nothing else:

```html
<!-- flow:remark -->
released: the arena was needed elsewhere
```

**The marker is what keeps a remark from being read as an answer.** Answers are not stored anywhere on this schema — *the issue thread is the answer store* (§ Questions), and the read half takes every comment posted after a question's `asked-at` that carries **no** flow marker. That is the one reader here selecting on a marker's *absence*, so an unmarked remark recorded while a step waits for a human would clear that wait and resume the step on prose nobody offered as a reply.

It costs the remark nothing a reader would notice. An HTML comment does not render, so it still appears in the thread exactly where a person writing the same sentence by hand would have put it, and reads as what it is.

**Nothing reads it back.** The marker separates a remark from a reply; it does not make one addressable. There is no remark store, no index and no id: the issue thread is where it lives, and the way to read it is the way a person reads any comment.

## Large artifact storage

Artifacts too large for an issue comment (file and patch types always; markdown when exceeding the configured `MaxCommentBytes`) are stored on the **orphan branch** `flow-artifacts`:

```
flow/artifacts/issue-<N>/<id>/<filename>
```

Where:
- `<N>` is the issue number.
- `<id>` is the artifact identifier.
- `<filename>` is the sanitized original filename (file type), `patch.diff` (patch type), or `body.md` (spilled markdown).

The orphan branch is created on first use with a parentless commit. Subsequent writes use the Contents API. The artifact comment on the issue links to the `raw.githubusercontent.com` URL for the file.

### Spill notice

A markdown artifact whose comment body was truncated carries a trailing notice:

```
[truncated preview; full body at <URL>]
```

Readers detecting this trailer must fetch the full body from the orphan branch rather than using the inline preview.

## Labels

All labels use a configurable prefix (default `flow:`). The label set is closed. Every label below goes **on an item**, except `flow:landing`, which is the one label that is a repository-scoped object in its own right:

| Label | Meaning |
|---|---|
| `flow:awaits:<role>` | Whose move it is: the role of the step the journal routes to next — or `flow:awaits:signal:<id>` while the route holds at a signal wait, which is nobody's move. Maintained at every append — added as the journal records what is awaited, moved when it changes, removed at finalization. |
| `flow:requires:<axis>:<value>` | Placement restriction: only arenas whose `<axis>` fact is `<value>` qualify. Axes closed at `os` and `arch` ([environment.md](environment.md) § Placement); several labels on one axis are alternatives. |
| `flow:owner:<login>` | The item is claimed by `<login>`. |
| `flow:arena:<fingerprint>` | The **arena** holding the claim, as an opaque digest of its `(HostId, ArenaId)`. Written and removed with `flow:owner:<login>`, which it is the other half of. |
| `flow:claim:<token>` | Transient claim-race token, self-limiting (see "Claim protocol" below). |
| `flow:landing` | The **project-scope exclusion**: the one landing round this repository's mainline admits at a time ([gates-and-commands.md](gates-and-commands.md) § Two scopes). **Never attached to an item**, and the only label that is not. Its **description** carries the holder — `<arena-fingerprint> <acquired-unix-hex>` — written in the same request as the name. See "Landing exclusion" below. |
| `flow:blocked` | The item is parked (generic block or deterministic refusal). |
| `flow:needs-answer` | The item is parked waiting for a human answer. |
| `flow:disabled` | The item is excluded from processing. Claim is refused. |
| `flow:manual` | An operator has taken hand control (`ItemEditor.SetManual`). Nothing dispatches the item underneath the person driving it. |
| `flow:infra-transient` | The item is parked due to infrastructure failure. |
| `flow:account-exhausted` | The item is parked because the **agent account's** allowance is spent ([environment.md](environment.md) § The agent account). Its own label rather than `flow:infra-transient`: the two parks share their treatment — nothing billed, no dispatch counted — and differ in what a human scanning the list should do, which is go look at the infrastructure for one and nothing at all for the other. The label says only that the condition holds; **when it clears is in the park record**, because a label cannot carry an instant without becoming a value nobody can query and everybody must parse. |
| `flow:treasurer-refused:<id>` | The treasurer refused further spend on the step producing `<id>`. |
| `flow:type:<type>` | Item type derivation label. |
| `flow:<binary-name>` | The binary that owns this item. |
| `flow:priority:<critical\|high\|low>` | Where the work sits in the order it is taken in, as whatever manages the work ranks it. **Absent means `medium`.** |
| `flow:urgency:<next\|deferred>` | What an operator wants done about the item now: start it next, or do not start it unattended. **Absent means `default`.** |

**Both halves of the claim record are needed, and the arena half is a digest.** A lease binds `item ↔ arena` ([orchestrator.md](orchestrator.md) § Required surface → Claiming), so the arena is what the exclusion compares; `flow:owner:<login>` records the account credited, and on the ordinary deployment — one operator, several worktrees — every arena writes the same one, so it cannot separate them. It is a **fingerprint** rather than the pair because [disclosure.md](disclosure.md) closes the categories a flow may not publish and two of the five are exactly what an arena is made of: "Local filesystem paths" (the `ArenaId` is the absolute worktree path) and "Host and account identifiers — machine names, arena names, internal hostnames". Labels are a guarded surface in that document's table. The digest supports equality and nothing else, which is the only operation the exclusion needs, and the same `(HostId, ArenaId)` yields the same digest across restarts, which is the stability [orchestrator.md](orchestrator.md) requires of an `ArenaId`. **Do not read this row as "publish the arena name".**

An item carrying `flow:owner:<login>` and **no** `flow:arena:<fingerprint>` is a record written before this label existed. It means some arena holds the item and the item does not say which, so it is read as held by every arena except the one whose own `.flow/active.json` says otherwise. Recovering from a holder that is gone is `--force`, as it was before.

The other half-record reads the other way: `flow:arena:<fingerprint>` with **no** `flow:owner:<login>` is **not a claim** — the account half is what says a lease was taken, and a fingerprint alone names an arena without saying it holds anything. The two labels go on in one request, so only their removal can be observed halfway, and which half goes first is chosen per path so that the halfway state is the true one. **Releasing** removes the arena half first: the lease is being given up but is not given up yet — the worktree-local record outlives the failure — so a release that stops halfway must leave the item reading as held. **Rolling back a claim that failed after posting the record** removes the owner half first, for the same reason read from the other end: no lease was taken, so a rollback that stops halfway must leave the item reading as free.

Park labels are added when a park is recorded and removed when the park is cleared (by a grant, a resume, or a reset). A park label that outlives its condition is worse than no label — it is read as current.

**The neutral values of the two selection axes have no label.** `medium` and `default` are not spellable: a state reachable both by a label and by that label's absence is one state with two spellings, and nothing keeps the two reading alike — an item demoted from `high` to `medium` and an item nobody ever assessed are the same item to selection, and must be the same item to a reader. So `SetPriority(medium)` and `SetUrgency(default)` remove the axis's label rather than writing one, and an item carrying no label of either kind is fully specified.

### Landing exclusion

> **`flow:landing` is a label object and never an item's label, and the refusal to create a name already taken is the exclusion itself.**

A label name is unique within a repository and its creation is atomic, so creating one is an **atomic create-if-absent every machine working this mainline can see, needing no server** — which is the whole of what [gates-and-commands.md](gates-and-commands.md) § Two scopes asks of project scope. Deleting the label releases it. It is not attached to an item because the resource it protects is the mainline, which is not any one item's.

**The holder is written in the same request as the name.** § Two scopes requires that naming the holder be part of taking the exclusion: a holder its own tools cannot recognise is a deadlock rather than a missing diagnostic. The description is `<arena-fingerprint> <acquired-unix-hex>` — a **fingerprint** for the reason `flow:arena:<fingerprint>` carries one, and an instant because the round's own declared bound is what says a holder is gone.

**A record older than the round's bound is collected.** There is no kernel in common between two machines, so the release authority host scope has — the process dying — does not exist here. What replaces it is the round's own bound: one `integration` timeout plus what the rest of the round costs. A record past that belongs to an arena that parked, crashed or went quiet, and it is removed by whoever meets it, exactly as an abandoned `flow:claim:<token>` is. A record nothing can parse is collected too, for the same reason an untimestamped claim token is: it has no way to expire on its own.

**The instant is the round's, so an arena meeting its own abandoned record rewrites it rather than inheriting it.** A record an arena finds under its own fingerprint is one no round is inside — an arena runs one round at a time ([resolution.md](resolution.md) § A claim binds worktrees, not processes) — so it is that arena's to take back. What it must not take back is the instant: that is the abandoned round's, and a round measuring its bound from a clock that started before it begins with part of the bound spent, or with none of it left, and is collected from under a measurement still running. The description is rewritten **in place**, because a delete followed by a create leaves the mainline momentarily unheld and whoever polls in that gap takes an exclusion the arena is about to believe it holds.

**A collection can never cost a wrong landing.** The bound is a judgement about what a round costs, and one set too short would otherwise put two arenas inside one round. So the holder **re-reads the record before it merges** and refuses when it is no longer its own — a transient refusal, not a verdict about the change: the work comes back behind whatever landed meanwhile and re-enters through the drift election ([orchestrator.md](orchestrator.md) § Drift is evidence for judgment).

## Signals from GitHub state

The GitHub orchestrator derives four signals by polling the pull request on the claim branch:

| Signal | Set when |
|---|---|
| `pr-open` | A PR for the claim branch has been opened. Latched: once set, not unset by merge or close. |
| `pr-merged` | The PR is merged. |
| `pr-closed` | The PR state is `closed`. |
| `pr-approved` | At least one reviewer's latest review state is `APPROVED`. |

These are refreshed on every `Load` call. The PR must be on the branch `flow/issue-<N>` to be found.

## Branch naming

| Branch | Purpose |
|---|---|
| `flow/issue-<N>` | Work branch for issue `<N>`. One per item. |
| `flow-artifacts` | Orphan branch for large artifact storage. |

## Claim protocol

Claiming uses a label-based race to achieve exclusivity without server-side locking:

1. **Post** a claim label `flow:claim:<token>`. The token is exactly 24 lowercase hex digits: 8 of creation time (unix seconds) followed by 16 of randomness.
2. **Re-fetch** the issue's labels, and make the already-held comparison again on what comes back — refusing if another arena now holds the item. The preflight's reading is stale by this point: the worktree preconditions run between the two reads and the first of them is a `git fetch`, so the gap is seconds wide rather than the two API calls the token window is sized for. An arena that completed its own claim inside that gap is invisible to the settle below, which compares `flow:claim:*` tokens and nothing else — and the holder removed its token as the last act of step 6.
3. **Collect** every abandoned token: one older than **10 minutes**, or one carrying no readable creation time (a different width, a non-hex character, or the untimestamped format that predates this rule). Its label is removed and it takes no part in the race. A claimer never tests its own token, so a clock adjustment mid-attempt cannot make it collect itself.
4. **Settle** among the tokens that remain: the **lexicographically smallest** wins. Every token is the same fixed width and lowercase, so the comparison is well-defined; because the creation time leads, lexicographic order is chronological order and the earliest attempt still in flight wins.
5. **Losers** remove their own claim label and return `ErrClaimRefused`. A token that wins is a live attempt, so the refusal names the winner and how long ago it started.
6. **Winner**: adds self as assignee, posts `flow:owner:<login>` and `flow:arena:<fingerprint>` **in one request**, removes the transient `flow:claim:<token>` label. Before posting them it removes the previous holder's markers — an owner label naming another account, and an arena label naming another arena. Under one account the owner label is byte-identical between two arenas, so the arena label is the only thing a take-over displaces; the two go in together because a window with one present and not the other is a window in which every reader of the record decides differently.

The token exists only *inside* one claim attempt — every exit from the attempt removes it. A process that dies between step 1 and its removal therefore strands a token that no process holds and nothing expires, and because the smallest token wins, that one token blocks the item for every later claimer, permanently. Collection in step 3 is what makes the token self-limiting, so recovery is an ordinary `claim` rather than an operator deleting the label through GitHub's own interface.

**The window is sized for the clocks, not for the attempt.** Age is read against the collecting claimer's own clock — there is no shared one — so ten minutes covers not the attempt itself, which is two API calls, but the disagreement between the clocks of two claimers racing from different machines. The rule holds while those agree to within a window: a claimer running more than a window behind the others mints tokens they read as already abandoned, and they settle the race without it.

**Collection is not lease recovery.** It touches the claim-race token and nothing else: `flow:owner:<login>`, `flow:arena:<fingerprint>`, the assignee, and the worktree-local active claim are never removed on a timer. Those record ownership by a person, and recovering a claim held by something no longer running is a separate problem — governed by [resolution-orchestrated.md](resolution-orchestrated.md) under "Interruption", and requiring that the holder be observed to be gone rather than inferred from elapsed time. A settled race token has no holder to observe.

Preflight checks before posting the claim label refuse refs that are not issues at all (§ Items are issues), and items that are disabled, owned by another binary, held by **another arena** — whatever account it claims as, so an arena under this very login is refused like any other (unless `OverrideAlreadyHeld` is passed) — awaiting a role the claiming account's detected capabilities cannot assume — or carrying placement restrictions this arena does not meet (unless the `unmet-placement` override is passed). The already-held one of those is made **twice**, because it is the comparison the lease turns on and its subject — the claim record on the item — is what another party writes while the preconditions run: step 2 above repeats it on the re-read, and that reading is the one the lease is taken on.

## Filed items

`FileItem` creates an ordinary issue whose body opens with a provenance marker, machine-managed like every marker here; the draft's own body follows untouched:

```
<!-- flow:filed source=<owner/repo#N> key=<key> by=<login> ts=<RFC3339> -->
```

Idempotence reads the **source** issue, not search: the state comment's `filed` map records each created issue against its key, written immediately after each creation, and `FileItem` returns the recorded ref for a key already present. The marker is the audit stamp on the filed side — what lets a reader of any issue see where it came from — and the backstop for the one gap the two-write sequence leaves: a process dying between creating an issue and recording it leaves a marker without a record, and the next `FileItem` for that key searches for the marker before creating. Search can lag, so the residual race is one item wide, and it fails toward the visible side — a duplicate a person closes, never a silent loss. The recorded map, read exactly, is the authority for everything it holds.

## Drafts

The GitHub orchestrator's draft store is the worktree-local `.flow/draft/` directory. Drafts are keyed by issue number and step result id. Nothing in this directory touches the GitHub API — the structural separation from the outward-facing code **is** the "never published" guarantee. Drafts are cleared when the claim is released (via `clistate.Clear`).

## The agent session

The orchestrator's session store is the worktree-local `.flow/session/` directory, beside the draft tree and never published for the same structural reason: nothing in it touches the GitHub API. One file per issue — the session belongs to the resolution ([resolution.md](resolution.md) § The agent session), so it is **keyed by the issue number alone**, and keying it like a draft would end the conversation at the first step boundary. The record holds the substrate's handle and the step whose `fresh` declaration it already honoured; the issue number is stored in the file as well as in its path, so two ids that sanitise onto one name lose a record rather than hand one resolution another's conversation. It is cleared when the claim is released (via `clistate.Clear`) and when the item's record is reset.

**Nothing of the session record reaches the issue.** The handle and the boundary it was stored with are not in the state comment, not in the journal, and not in any published body: the handle names a conversation holding the resolution's whole reasoning, and the boundary says which step's reasoning it is.

**The treasurer's session count is not the session record**, and it does travel in the state comment, with the rest of the ledger (§ Ledger). A count of how many conversations a resolution bought, and why, names none of them and carries none of their reasoning — it is a spending figure of the same kind as cost and active time, and it is published for the same reason those are: an operator cannot see waste that is recorded nowhere they can read ([resolution.md](resolution.md) § The treasurer).

## Cross-references

- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds stored here.
- [orchestrator.md](orchestrator.md) — the SDK ↔ orchestrator boundary this schema implements.
- [resolution.md](resolution.md) — the journal and lifecycle whose state this schema persists.
