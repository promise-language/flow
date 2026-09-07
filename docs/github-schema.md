# GitHub storage schema

> **Tag:** `github-schema` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines how state, the journal, and history are stored on a GitHub issue by the GitHub backend. It is written for readers who have no SDK — the wire format is the contract.

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

Item-level: `total_cost_usd`, `total_duration_seconds`, `total_waiting_seconds`.

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
| `id` | string | The backend-assigned question identifier, named by `answer --question`. |
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
| `kind` | string | One of: `blocked`, `question`, `treasurer-refused`, `step-did-not-complete`, `infra-transient`, `remote-unreachable`, `refused`, `write-contract`. |
| `step` | string | The pending step's result id (artifact or signal id). |
| `axis` | string | The treasurer's axis when `kind=treasurer-refused`. |
| `axes` | array | Full spend snapshot at park time, in the treasurer's vocabulary (each with `axis`, `used`, `granted`, `exhausted`). |
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

All labels use a configurable prefix (default `flow:`). The label set is closed:

| Label | Meaning |
|---|---|
| `flow:awaits:<role>` | Whose move it is: the role of the step the journal routes to next — or `flow:awaits:signal:<id>` while the route holds at a signal wait, which is nobody's move. Maintained at every append — added as the journal records what is awaited, moved when it changes, removed at finalization. |
| `flow:requires:<axis>:<value>` | Placement restriction: only arenas whose `<axis>` fact is `<value>` qualify. Axes closed at `os` and `arch` ([environment.md](environment.md) § Placement); several labels on one axis are alternatives. |
| `flow:owner:<login>` | The item is claimed by `<login>`. |
| `flow:arena:<fingerprint>` | The **arena** holding the claim, as an opaque digest of its `(HostId, ArenaId)`. Written and removed with `flow:owner:<login>`, which it is the other half of. |
| `flow:claim:<token>` | Transient claim-race token, self-limiting (see "Claim protocol" below). |
| `flow:blocked` | The item is parked (generic block or deterministic refusal). |
| `flow:needs-answer` | The item is parked waiting for a human answer. |
| `flow:disabled` | The item is excluded from processing. Claim is refused. |
| `flow:manual` | An operator has taken hand control (`ItemEditor.SetManual`). Nothing dispatches the item underneath the person driving it. |
| `flow:infra-transient` | The item is parked due to infrastructure failure. |
| `flow:treasurer-refused:<id>` | The treasurer refused further spend on the step producing `<id>`. |
| `flow:type:<type>` | Item type derivation label. |
| `flow:<binary-name>` | The binary that owns this item. |
| `flow:priority:<critical\|high\|low>` | Where the work sits in the order it is taken in, as whatever manages the backend ranks it. **Absent means `medium`.** |
| `flow:urgency:<next\|deferred>` | What an operator wants done about the item now: start it next, or do not start it unattended. **Absent means `default`.** |

**Both halves of the claim record are needed, and the arena half is a digest.** A lease binds `item ↔ arena` ([orchestrator.md](orchestrator.md) § Required surface → Claiming), so the arena is what the exclusion compares; `flow:owner:<login>` records the account credited, and on the ordinary deployment — one operator, several worktrees — every arena writes the same one, so it cannot separate them. It is a **fingerprint** rather than the pair because [disclosure.md](disclosure.md) closes the categories a flow may not publish and two of the five are exactly what an arena is made of: "Local filesystem paths" (the `ArenaId` is the absolute worktree path) and "Host and account identifiers — machine names, arena names, internal hostnames". Labels are a guarded surface in that document's table. The digest supports equality and nothing else, which is the only operation the exclusion needs, and the same `(HostId, ArenaId)` yields the same digest across restarts, which is the stability [orchestrator.md](orchestrator.md) requires of an `ArenaId`. **Do not read this row as "publish the arena name".**

An item carrying `flow:owner:<login>` and **no** `flow:arena:<fingerprint>` is a record written before this label existed. It means some arena holds the item and the item does not say which, so it is read as held by every arena except the one whose own `.flow/active.json` says otherwise. Recovering from a holder that is gone is `--force`, as it was before.

Park labels are added when a park is recorded and removed when the park is cleared (by a grant, a resume, or a reset). A park label that outlives its condition is worse than no label — it is read as current.

**The neutral values of the two selection axes have no label.** `medium` and `default` are not spellable: a state reachable both by a label and by that label's absence is one state with two spellings, and nothing keeps the two reading alike — an item demoted from `high` to `medium` and an item nobody ever assessed are the same item to selection, and must be the same item to a reader. So `SetPriority(medium)` and `SetUrgency(default)` remove the axis's label rather than writing one, and an item carrying no label of either kind is fully specified.

## Signals from GitHub state

The backend derives four signals by polling the pull request on the claim branch:

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
2. **Re-fetch** the issue's labels.
3. **Collect** every abandoned token: one older than **10 minutes**, or one carrying no readable creation time (a different width, a non-hex character, or the untimestamped format that predates this rule). Its label is removed and it takes no part in the race. A claimer never tests its own token, so a clock adjustment mid-attempt cannot make it collect itself.
4. **Settle** among the tokens that remain: the **lexicographically smallest** wins. Every token is the same fixed width and lowercase, so the comparison is well-defined; because the creation time leads, lexicographic order is chronological order and the earliest attempt still in flight wins.
5. **Losers** remove their own claim label and return `ErrClaimRefused`. A token that wins is a live attempt, so the refusal names the winner and how long ago it started.
6. **Winner**: adds self as assignee, posts `flow:owner:<login>` and `flow:arena:<fingerprint>` **in one request**, removes the transient `flow:claim:<token>` label. Before posting them it removes the previous holder's markers — an owner label naming another account, and an arena label naming another arena. Under one account the owner label is byte-identical between two arenas, so the arena label is the only thing a take-over displaces; the two go in together because a window with one present and not the other is a window in which every reader of the record decides differently.

The token exists only *inside* one claim attempt — every exit from the attempt removes it. A process that dies between step 1 and its removal therefore strands a token that no process holds and nothing expires, and because the smallest token wins, that one token blocks the item for every later claimer, permanently. Collection in step 3 is what makes the token self-limiting, so recovery is an ordinary `claim` rather than an operator deleting the label through the backend.

**The window is sized for the clocks, not for the attempt.** Age is read against the collecting claimer's own clock — there is no shared one — so ten minutes covers not the attempt itself, which is two API calls, but the disagreement between the clocks of two claimers racing from different machines. The rule holds while those agree to within a window: a claimer running more than a window behind the others mints tokens they read as already abandoned, and they settle the race without it.

**Collection is not lease recovery.** It touches the claim-race token and nothing else: `flow:owner:<login>`, `flow:arena:<fingerprint>`, the assignee, and the worktree-local active claim are never removed on a timer. Those record ownership by a person, and recovering a claim held by something no longer running is a separate problem — governed by [resolution-orchestrated.md](resolution-orchestrated.md) under "Interruption", and requiring that the holder be observed to be gone rather than inferred from elapsed time. A settled race token has no holder to observe.

Preflight checks before posting the claim label refuse items that are disabled, owned by another binary, held by **another arena** — whatever account it claims as, so an arena under this very login is refused like any other (unless `OverrideAlreadyHeld` is passed) — awaiting a role the claiming account's detected capabilities cannot assume — or carrying placement restrictions this arena does not meet (unless the `unmet-placement` override is passed).

## Filed items

`FileItem` creates an ordinary issue whose body opens with a provenance marker, machine-managed like every marker here; the draft's own body follows untouched:

```
<!-- flow:filed source=<owner/repo#N> key=<key> by=<login> ts=<RFC3339> -->
```

Idempotence reads the **source** issue, not search: the state comment's `filed` map records each created issue against its key, written immediately after each creation, and `FileItem` returns the recorded ref for a key already present. The marker is the audit stamp on the filed side — what lets a reader of any issue see where it came from — and the backstop for the one gap the two-write sequence leaves: a process dying between creating an issue and recording it leaves a marker without a record, and the next `FileItem` for that key searches for the marker before creating. Search can lag, so the residual race is one item wide, and it fails toward the visible side — a duplicate a person closes, never a silent loss. The recorded map, read exactly, is the authority for everything it holds.

## Drafts

The GitHub backend's draft store is the worktree-local `.flow/draft/` directory. Drafts are keyed by issue number and step result id. Nothing in this directory touches the GitHub API — the structural separation from the outward-facing code **is** the "never published" guarantee. Drafts are cleared when the claim is released (via `clistate.Clear`).

## Cross-references

- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds stored here.
- [orchestrator.md](orchestrator.md) — the SDK ↔ orchestrator boundary this schema implements.
- [resolution.md](resolution.md) — the journal and lifecycle whose state this schema persists.
