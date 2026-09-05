# Orchestrator contract

> **Tag:** `orchestrator` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** This document defines the boundary between the SDK and an orchestrator: what an orchestrator must implement, and what it may refuse.

**must** and **must not** state conformance: an orchestrator that does otherwise is not one. **may** marks a decision this contract deliberately leaves open — both choices conform, and a caller has to tolerate either, which is why so little here is a `may`.

**There are no optional capabilities.** Every method below is required, and an orchestrator that cannot do one refuses it — see "What an orchestrator may refuse". An interface an orchestrator may omit leaves a caller with nothing to call and no way to ask why; a method that refuses gives an answer.

## What an orchestrator is

An orchestrator is what the SDK talks to. It leases items to arenas, holds their state, runs gates and commands in a worktree, and lands what those produce. **Where it stores any of that is its own business** — the GitHub orchestrator (`pkg/orchestrator/github`) keeps state on issues, the tracker orchestrator (closed-source) in its own service — and a store is something an orchestrator uses, not something it is.

It may be **local or remote**. A remote one is a service that dispatches to many arenas; a local one is the binary orchestrating itself against a store it reaches directly, which is what the GitHub orchestrator is. Both satisfy the same `Orchestrator` interface, and nothing above this boundary knows which it is talking to.

## Identities

Every name that crosses this boundary is one of the following. The set is closed: an orchestrator is passed no other kind of identifier, and a method parameter not listed here is a value, not a name.

| Identity | What it names | Definition |
|---|---|---|
| `ItemRef` | An item | **The item identity, and the only one.** The SDK carries it whole and interprets none of it. Every `ref` parameter is one. Its parts are not identities: a orchestrator-internal address, a human-readable display such as `owner/repo#123`, and the orchestrator's own store key are all **projections of the ref**, derived from it, and could as well be functions of it as fields on it. Nothing may be reconstructed into a ref from a projection — that is what `ResolveRef` exists to do. |
| `Claim` | A held lease **on one item** | The credentialed handle returned by `Orchestrator.Claim`. **It is never a parameter.** It carries the `ItemRef`, the `AccountId` credited, and the orchestrator-internal `Token`, and it is returned — by `Claim` and by `LookupActiveClaim` — never passed back in. Every method addresses the item by `ItemRef`; holding the lease is a **precondition the orchestrator checks against the claim it currently has**, not a value a caller supplies. A supplied claim is a value, and values go stale: The `already-held` override lets another arena take an item over, leaving the first holding a struct that still looks valid. A write that carried its own proof would be writing under revoked authority. Serialized to `.flow/active.json` and recoverable via `LookupActiveClaim`. It leases an item and nothing else: the other exclusions in this system — a lock per `GateName`, and the project-scope lock that serializes landing — are not `Claim`s, are not named in this contract, and belong to whatever runs the gates. It carries the `(HostId, ArenaId)` of the arena it binds — a handle to a lease that could not name what the lease binds would leave every holder unidentifiable wherever one account runs more than one arena. |
| `AccountId` | Who to credit a claim to | A person's account **in the orchestrator's own namespace** — for GitHub, the authenticated login. **Never passed in; always derived** from the credentials the arena acts as. An arena that can commit, push and merge has exactly one account by construction, so the orchestrator reads it rather than being told, and there is no way for a caller's idea of the account to differ from the one the work is actually done as. Non-empty. It becomes a label (`flow:owner:<account>`) and an assignee, so it carries the same floor as `TagId`. **Attribution only** — it is not what the lease binds to: a claim is `item ↔ arena`, never `item ↔ user`. Neither `Claim` nor `LookupActiveClaim` takes one. |
| `HostId` | The machine an arena lives on | The host's **short name — its first dotted segment**, normalized. **It must be unique across every host the orchestrator can see.** A duplicate is not a cosmetic collision: arenas are addressed by `(HostId, ArenaId)`, so two machines sharing an id merge into one identity, and the one-to-one claim invariant stops holding — the orchestrator dispatches two arenas' worth of work as though to one. Uniqueness cannot be inherited from the FQDN, because the short form drops the domain: `build01.us-east` and `build01.eu-west` both normalize to `build01`. A fleet spanning domains **must assign unique short names** rather than relying on the domain to separate them. The normalization is a requirement for the same reason — these are compared by string equality across systems, so two spellings of one host are two hosts. Derived by default and explicitly settable; see "Setting a `HostId`" below. |
| `(HostId, ArenaId)` | An arena | The pair is the arena identity, and the only form unique beyond a single machine. **`ArenaId` alone is not an identity** — it is a component, unique only within its host, so it names an arena no more than a house number names a house. It is a worktree plus a stable identity, where work physically happens, and it must be **stable across restarts**: it outlives at least one item's full lifecycle, because the claim it holds does. An orchestrator enforcing one-to-one claims compares the pair. For the GitHub orchestrator both halves are implicit — the arena is the local checkout, and `.flow/active.json` is the whole lease store, so one file per checkout does the scoping a fleet-serving orchestrator must do explicitly. |
| `ArtifactId` | A handler-produced artifact — and therefore the step that produces it, its budget record, and its grant name | Opaque string, unique within `App.Artifacts`. **One identity per step:** the id is the only name a step answers to, and the human label a flow gives a step when declaring it is not an identity at all. |
| `SignalId` | An orchestrator-observed durable boolean | **An independent namespace from `ArtifactId`.** Signal steps are never handler-writable and own no budget record, so they are never counted and never grantable. |
| `StepId` | One step within a flow | The step's result id: its `ArtifactId` when it produces an artifact, its `SignalId` when it completes on a signal. **Inside a flow the two are a single namespace** — registering a second step under an id already taken is refused whichever kind it is — so a `StepId` is unambiguous without carrying a discriminator. The two vocabularies stay independent everywhere else; it is the flow that merges them. A `StepId` that is an `ArtifactId` also keys a budget record; one that is a `SignalId` keys none. |
| `GateName` | A gate, as a declared concept and an optional instance | `tested`, or `tested:wasm`. The concept half is a closed vocabulary; the instance half is the project's. `integration` and `fit` are required and sit outside that vocabulary — one is the composition the others compose into, the other asks whether a machine may be given work at all. **`fit` is a gate like any other**, run through `RunGate` and judged through `Judge`: it is not a separate capability, and giving it its own call is what previously let it return a verdict with no run, so that *could not measure* and *measured, unacceptable* became one answer. See [gates-and-commands.md](gates-and-commands.md). |
| `CommandName` | One command the arena can run | Names something that **may modify the worktree or the arena environment** — which is precisely what separates a command from a gate, and why no decision may rest on what a command reports. The set is **closed at three**: `setup` prepares the arena before a claim is taken, `verify` repairs what is mechanically repairable and reports on the rest, `cleanup` tidies after a resolution. A project does not invent a fourth, because the flow decides when each runs and would have no place to run one it did not know about — unlike gates, which a project composes into `integration` freely. |
| `BinaryName` | Which binary is asking | The **bare executable name** — `issue`, never `bin/issue`. Set explicitly in configuration, or derived from `argv[0]` with the directory stripped. The bareness is a requirement, not a convention: the value becomes a label (`flow:issue`) and is interpolated into the query that finds it, so a path separator would name a label the orchestrator cannot hold and silently corrupt the search. It decides what counts as `processable` and `auto` — the same item is `unhandled` to one binary and workable to another — so it is an input to availability, not a property of the item. |
| `TagId` | An area of work, in the orchestrator's own vocabulary | A label carried by an item; each orchestrator maps its own vocabulary onto it — issue labels, tracker tags. Free-form, but **not every string is one**: a `TagId` is non-empty, single-line, and carries no leading or trailing whitespace. Below that floor a value is not a tag and is refused rather than stored. Above it, validity is the orchestrator's own, and an orchestrator refuses a tag it cannot store rather than storing a mangled one. The floor is load-bearing, not decorative: a `TagId` is interpolated into the orchestrator's own query, where a value containing a space does not fail — it silently becomes a different query. **Not a closed set**, and reported in full rather than filtered to what a flow recognises. **Compared by exact equality:** an orchestrator filtering server-side must match the way the SDK does, or one `--tag` value means two different things across `list` and `resolve`, which are meant to read as symmetrical. |
| `QuestionId` | One asked question | Assigned by the orchestrator in `AskQuestion` and returned on the persisted `Question`. **Opaque to the SDK** — its form is the orchestrator's business (the GitHub orchestrator spells it `q-<nanos>-<index>`). **Unique within its item**, which is the scope every consumer needs: `answer --question <id>` matches among one item's pending questions, and an operator needs the id only to disambiguate when more than one is pending. An orchestrator may make it unique more widely; nothing may rely on that. It must be **stable for the life of the question** — the id is read from one command and passed to another. |
| `OrchestratorName` | Which orchestrator owns an item | Returned by `Name()` and carried on every `ItemRef`. A ref whose `OrchestratorName` is not this orchestrator's is not this orchestrator's to interpret — the check that stops one orchestrator reading another's addressing. |
| `BranchName` | A line of work in the worktree | A git branch. `base` is the branch a new one is created off, and the one revision besides `HEAD` that `RevParse` must resolve. |
| `Revision` | Something resolvable to a commit | Whatever `RevParse` is asked for. Only `"HEAD"` and the item's base branch must resolve; beyond that is best-effort, and an orchestrator that cannot resolve one says so rather than falling back to `HEAD`. |
| `CommitSha` | One commit | What `RevParse` returns. |
| `RequestUrl` | An opened pull request | Produced only by `Open`; consumed by `Merge` and `FindPR`. Orchestrator-specific in form and opaque to the SDK, but never a bare string at the boundary. |

Every one of these answers the same test: **given the value, exactly one thing can be found.** A value that fails it is not an identity, whatever it is called.

**Nothing below this table names a thing with anything else, and no identity is ever a bare `string`.** Every parameter, field and return that identifies something is one of the typed values above — never a `string` standing in for one, and never a projection of one. This binds returns as strictly as inputs: a method handing back a `string` that names something has produced an identity and dropped its type on the way out. A `string` where an identity belongs is a defect in this contract, not a shorthand.

The one exception is `ResolveRef`, whose `input` is deliberately a bare string — it is a human-typed value *because it is not yet an identity*. Turning it into an `ItemRef` is the whole of what the method does, and it is the only place a value crosses into this document without already being one.

**Availability is computed in the orchestrator, from knowledge that is partly the SDK's.** That is why `List` takes a predicate at all: the ladder's third level — whether any flow here handles this item's type — is the SDK's business, and the orchestrator cannot answer it alone. The predicate is how that knowledge crosses without the orchestrator importing flow definitions.

**Signatures show types, not parameter names.** A parameter's name is not its definition — it could be `p0` — so the type is what appears. A name is added only where two parameters share a type and the order matters, as in `Branch(ctx, branch, base BranchName)`. Where a bare `string` or `bool` appears, it is genuinely a value and not a name for anything.

A projection of an identity is not an identity. An item's display string, its orchestrator-internal address and its store key all name the same item, but none of them **is** the name — each is a rendering of `ItemRef` for one audience, and a value in one of those forms has to be resolved back to a ref before anything may address with it.

Two things that look like identities and are not: a step's **human label** (display only, and never accepted where an id is expected), and a **block reason**, which is prose for a person and carries no reference — the references are `BlockedBy`. See "Dependencies".

### Setting a `HostId`

A `HostId` is **derived from the machine by default and explicitly settable**. The derived value is the machine's own name, normalized. An explicit setting replaces it **without changing the FQDN**. Both halves are needed: derivation means the ordinary case takes no configuration, and the override is the only way to resolve a collision, because the colliding machines are usually named by a DNS the operator does not control.

**The orchestrator must refuse to register an arena under a `HostId` already registered to a different machine.** Uniqueness that is only requested is not uniqueness. What a duplicate causes is silent — two machines' arenas merging into one identity, with work dispatched to one as though it were both — so the check belongs at registration, which is the last moment there is still something to refuse.

**A machine re-registering its own `HostId` must succeed.** Arenas survive restarts by contract, and a restart is a re-registration; refusing one would cost the arena the claim it is required to keep. So the registry retains what the id was derived from alongside the id itself — which is precisely what an override that leaves the FQDN alone preserves, and what lets *the same machine again* be told apart from *a second machine with the same short name*.

**Adoption verifies; it never assumes.** Whatever admits an arena into a registry checks the `HostId` against what is already there, and where the match cannot be resolved it treats the two as **two hosts, not one**. The unsafe reading is the convenient one: coalescing them requires no action and yields a registry that looks correct, while two machines' arenas answer to a single identity. So the ambiguous case refuses — the same direction every unrecognised condition in this contract resolves, for the same reason that stopping is recoverable and proceeding is not.

Which layer performs the check is the registry's business, not this contract's. An orchestrator with no registry has nothing to adopt: the GitHub orchestrator's single arena is the local checkout, never registered, and none of this reaches it.

### Vocabularies

These cross the boundary too and need defining, but they classify rather than name — given one, you find a category, not a thing. They are not identities and no method addresses anything with them.

| Vocabulary | What it classifies | Definition |
|---|---|---|
| `ItemStatus` | Where an item stands in the **orchestrator's own** lifecycle | Closed at two: **`open`** — work may still happen — and **`terminal`** — it will not. The orchestrator's own disposition names (`done`, `won't fix`, `duplicate`, `closed as not planned`) are its vocabulary, carried alongside for display and never interpreted here: this contract needs only to know whether more work is possible, and every orchestrator can answer that about its own items. `Finalize` refuses anything not `terminal`. Distinct from `Availability`, which says whether *this binary* could work the item now, and from `Item.Finalized`, which says whether the **flow** is done with it — an item can be `terminal` and not finalized (closed by hand mid-run) or finalized only because it reached `terminal` first. |
| `ItemType` | What kind of item it is | Routes flow selection; a flow declares which types it handles. **Must be non-empty.** An item whose type no flow accepts is `unhandled`. |
| `ArtifactType` | An artifact's shape | The type half of the `(ArtifactId, ArtifactType)` pair `SupportedArtifacts()` closes over. Validated at startup by id **and** type. |
| `ItemScope` | How far up the listing ladder to report | Closed set of six, named for the levels themselves: `all`, `open`, `processable`, `workable`, `free`, `auto`. |
| `Availability` | Where one item sits on that ladder | Closed set of six: `auto`, `available`, `held`, `blocked`, `unhandled`, `closed`. Each is exactly the boundary between two adjacent scopes. |
| `ParkKind` | Why a step stopped without completing | Closed set of eight: `blocked`, `question`, `budget-exhausted`, `step-did-not-resolve`, `refused`, `infra-transient`, `remote-unreachable`, `write-contract`. The kind decides what clears the park: a grant clears **only** a budget-exhausted one, and only when it raises the offending axis above consumption. Every other kind is cleared by whatever caused it ending, or by an operator. |
| `BudgetAxis` | Which budget ran out | Names the axis a `budget-exhausted` park stopped on, and the axis a `Grant` must raise above consumption to clear it. |
| `ClaimOverride` | A safety check the operator chose to bypass | Closed set of four: `dirty-tree`, `already-held`, `unadmitted`, `stale-base`. **flow's own vocabulary** — these are CLI flag names, not the orchestrator's, and an orchestrator receives them rather than defining them. |
| `Outcome` | How a gate or command run ended | Closed set of five: **`measured`** — it ran and produced a result — and `timed_out`, `could_not_start`, `died`, `broke_contract`, none of which are results. **Only `measured` may be judged**: the other four report that no measurement exists, and a run that measured nothing has not reported that the tree is bad. `broke_contract` is where a gate broken in its own code lands — one that printed something that is not an envelope, or that modified what it measured. |
| `BlockKind` | **Who must act, and on what**, for the item to become workable | Closed set of three. Each names an actor and a subject, which together are the only thing a caller does differently.<br>**`waits-on-items`** — actor: whoever can work the blockers. Subject: those items, named in `BlockedBy`. It clears when they finish and nobody touches this item at all.<br>**`waits-on-condition`** — actor: nobody. Subject: nothing addressable — a network away, a service down, a rate limit. It clears on its own and there is nothing to go work; the only response is to come back.<br>**`waits-on-person`** — actor: a person. Subject: this item — answer its question, grant its budget, re-enable it. It will not clear on its own.<br>The causes within each are open-ended; who acts and on what is not, which is why this is a field and the reason is prose. |
| `ClaimRefusalCode` | Why an orchestrator refused a claim | **The orchestrator's own vocabulary**, carried through verbatim. flow deliberately defines no constants for it, because mirroring the orchestrator's protocol here would be a second enum nothing keeps in sync. |

## Structures

The payloads the surface returns. They are not identities — none of them names anything — but a caller cannot read a signature without knowing what comes back.

### `ItemInfo` — the orchestrator's standing on an item

Returned by `List` and `Get`.

| Field | Holds |
|---|---|
| `Ref` | The item's `ItemRef`. |
| `Title` | For display in a listing. |
| `Status` | `ItemStatus` — `open` or `terminal`, with the orchestrator's own disposition alongside. |
| `Availability` | Where the item sits on the listing ladder, **for the asking `BinaryName`**. |
| `Holder` | The `(HostId, ArenaId)` of the arena holding it and the `AccountId` credited, or empty when unclaimed. |
| `Tags` | Every `TagId` the item carries — the operator's and the orchestrator's own markers alike, never filtered to what a flow recognises. |
| `BlockedBy` | Every blocker declared on the item: each one's `ItemRef` **and its `ItemStatus`**, so a caller can tell which are still blocking from which have finished. See "Dependencies". |
| `Blocked` | Whether the item is blocked. Item-level and the same whoever asks — unlike `Availability`, which can report `closed` or `unhandled` instead. |
| `BlockKind` | Who must act and on what — `waits-on-items`, `waits-on-condition` or `waits-on-person`. |
| `BlockReason` | One line **for a person**, and nothing else. Nothing parses it, branches on it, or infers a state from it — not from its wording and not from whether it is empty. Every machine-readable fact about a block is a field beside it. |

### `Item` — the item, and what the flow recorded on it

Returned by `Load`. Nothing in it is a state value: an item's states are `ItemStatus`, `Availability` and the two flags below.

| Field | Holds |
|---|---|
| `Ref` | The item's `ItemRef`. |
| `Type` | Its `ItemType`. **Must be non-empty** — it is what routes flow selection. |
| `Title`, `Body`, `URL` | The request as a person wrote it, and where to read it. |
| `Flow` | The flow last selected for it, when known. |
| `Status` | Its `ItemStatus`. |
| `Holder` | The arena holding it and the `AccountId` credited, or empty when unclaimed. |
| `Tags` | Every `TagId` it carries. |
| `BlockedBy`, `BlockReason` | Its blockers, each with its own `ItemStatus`, and why it is blocked. Same meaning as on `ItemInfo`. |
| `Finalized` | Whether the **flow** is done with it. `Load` **must** report this truthfully; `Finalize` is the only thing that sets it. |
| `Manual` | Whether an operator has taken hand control. `Load` **must** report this truthfully. |
| `Artifacts` | Each declared `ArtifactId`'s record: its value if resolved, its stale bit, and its budget counters. |
| `Signals` | Each `SignalId`'s observed state. |
| `Questions` | Every question asked, answered or not, each with its `QuestionId`. Never removed. |
| `Park` | Why the item stopped, or empty when it has not. |

There is no separate metadata struct nested inside this one. An item's own fields and the flow's record of working on it are read together, by one call, and nothing needs half of it.

**It carries everything `ItemInfo` does except `Availability`**, and the exception is the whole reason there are two types. Every other field is true of the item whoever asks; availability is computed *for a binary*, and `Load` is not told which one. A caller holding an `Item` can therefore answer *is this blocked, who holds it, what is it tagged* without a second call — it cannot answer *would this binary pick it up*.

**Every field `ItemEditor` can change, `Item` reports.** Editing is a read-then-write: an operator adds a tag to the tags an item already has, retracts one blocker of several, or corrects a title they first had to see. A load that showed less than the editor can change would mean editing blind, and the two lists are checkable against each other — title, body, tags, blockers and the manual flag, all present on both.

**What separates them is cost, not meaning.** `ItemInfo` is the listing projection: cheap enough to return hundreds of, so it omits artifacts, signals, questions and park. Where the two overlap they mean the same thing and **must agree** — a field that read one way through `List` and another through `Load` would make the two calls into two answers about one item.

**It carries no store id.** The orchestrator's own key for the item is a projection of `ItemRef`, and a caller that has the item has the ref it was loaded by.

### Declaration payloads

What `Supported*` returns. Each is an entry the SDK validates flow references against at startup.

| Type | Holds |
|---|---|
| `SignalDef` | A `SignalId`, and a one-line description for docs, UI and startup errors. The description is not load-bearing. |
| `GateDef` | A `GateName`, and whether it is required. `integration` and `fit` must appear. |
| `CommandDef` | A `CommandName`. Only `verify`, `setup` and `cleanup` exist. |
| `ArtifactDef` | An `ArtifactId` **and** its `ArtifactType` — the pair is the schema, and both halves are validated. Plus a one-line description of what the artifact holds and when it is produced. |

### Seeding and writing payloads

| Type | Holds |
|---|---|
| `ArtifactSpec` | What `SeedState` pre-loads for one artifact: its `ArtifactId`, its `ArtifactType`, whether it is required for the flow to complete, and its budget caps. |
| `ArtifactBody` | The value `ResolveArtifact` writes. A union: it carries an `ArtifactType` and exactly one populated payload, and which one is legal is determined by that type. An empty body is a legal call — see the method. |
| `Grant` | What `Grant` adds, per axis: invocations, prompts per invocation, cost, and additional time. Every axis is optional; a grant may raise one and leave the rest. |
| `ParkRequest` | Why a step stopped: its `ParkKind`, the `StepId` that parked, and a one-line reason with optional detail. When the kind is budget exhaustion it also carries the offending `BudgetAxis` **and the state of every axis** — the axes go flat together, so reporting only the tripping one costs an operator a grant per axis. |

### Question payloads

| Type | Holds |
|---|---|
| `AgentQuestion` | What a step asks: the question text, a short header, and — when it offers choices — the option list and whether more than one may be picked. |
| `Question` | An `AgentQuestion` once recorded: the same content, plus its `QuestionId`, when the orchestrator recorded it, and the answer when one has been given. A question with no answer is pending. |

### `GateVerdict` — the project's answer about a measurement

Returned by `Judge`. Carries the `GateRun` it is about — so a verdict can never be separated from the measurement it judges — and whether that measurement is acceptable. Only a run whose `Outcome` is a measurement may be judged; the other outcomes are not verdicts and must never be passed off as one, because **a run that measured nothing has not reported that the tree is bad**.

### `PRInfo` — a pull request

Returned by `FindPR`. Carries the `RequestUrl` and the merge commit, the latter meaningful only once the request is merged.

### `ClaimInfo` — who holds an item

Returned by `LookupClaim`. Carries the `(HostId, ArenaId)` of the holding arena, the `AccountId` credited, and when the lease was taken. It is not a `Claim`: it carries no token and authorises nothing.

### `GateRun` and `CommandRun` — what a run observed

Returned by `RunGate` and `Run`. Each carries what was asked for, an **`Outcome`** — whether the thing ran and produced a result, or timed out, could not start, died, or broke its contract — the exit status as a raw diagnostic, and what was printed. **Only an outcome that is a measurement may be judged**, which is what keeps *no answer* from being read as a passing one.

## Required surface

Every orchestrator implements the `Orchestrator` interface. The methods group by concern:

### Declaration

| Method | Contract |
|---|---|
| `Name()` → `OrchestratorName` | Returns the orchestrator's name. |
| `SupportedSignals()` → `[]SignalDef` | The set of `SignalDef` values this orchestrator knows how to observe. The SDK validates every signal reference against this list at startup. |
| `SupportedGates()` → `[]GateDef` | Every gate that can be run here. **`integration` and `fit` are both required and must appear.** Nothing lands without `integration` passing, and `fit` **must run before a claim is taken** — a fitness answer that arrives after the work started is the failure it exists to prevent, so an orchestrator that could only answer it later has not implemented it. The rest are the project's own concerns — `formatted`, `builds`, `tested`, `covered`, `checked` — and which exist is the project's decision. The SDK validates every gate a flow names against this list at startup, as it does signals and artifacts, so a flow naming a gate nothing can run fails before an item is claimed rather than part-way through one. Listing them is also what makes `integration`'s parts addressable: it is assembled from smaller gates, and each is separately runnable only if a caller can discover what they are. |
| `SupportedCommands()` → `[]CommandDef` | Which of the three `CommandName`s this orchestrator can run. **`verify` is required** — a step should not fail over something `verify` would have fixed. **`setup` is optional and runs before a claim is taken**, preparing the arena for work; like `fit`, an answer that arrives after the work started is worthless. **`cleanup` is optional and runs after a resolution**, and is the one thing here that is **not guaranteed to run at all** — see below. |
| `SupportedArtifacts()` → `[]ArtifactDef` | The orchestrator's canonical artifact schema: a closed, curated set of `ArtifactDef` values it knows how to record. The SDK validates every declared artifact against this set at startup — by id **and** type. See "Artifact schema ownership" below. |

### Discovery

| Method | Contract |
|---|---|
| `ResolveRef(ctx, string)` → `(ItemRef, error)` | Turn a user-supplied string into an `ItemRef`. **The one place a value enters this contract before it is an identity**, and the only supported way to reach an item by a name a person typed. Required because the alternative is matching on `Display`, which is a projection: it resolves by substring and first-match, so it answers with *an* item rather than *the* item. |
| `List(ctx, ItemScope, BinaryName, func(ItemType) bool)` → `([]ItemInfo, error)` | Items at the given `ItemScope`, with per-item availability, tags, holder and blockers. The `BinaryName` names the label that marks an item opted in, which is what separates `auto` from `available`. The predicate is **not a value at all**: it is the SDK lending the orchestrator its own knowledge of which flows are registered, without which the orchestrator could not tell `unhandled` from `processable`. Feeds `list`. The auto-select path **must never** call it. |
| `Get(ctx, ItemRef, BinaryName, func(ItemType) bool)` → `(*ItemInfo, error)` | One of exactly what `List` returns, addressed by ref instead of enumerated. **It must answer identically to `List` for the same item at the same moment** — one derivation serving both, never two. An item that reads `blocked` in `list` and `available` in `status` is a contradiction an operator cannot resolve, and nothing in the item caused it. Feeds `status`'s availability line, and is what makes an `ItemRef` sufficient — without it, listing metadata is reachable only by enumerating everything and searching. |
| `ListAutoSelectable(ctx, []TagId)` → `([]ItemRef, error)` | The items an unattended `resolve` may start on, carrying every `TagId` given. An empty list means no filter. Feeds the auto-select path. **Must not** return a `blocked` item. Filtering belongs here because tags live in the orchestrator and `ItemRef` does not carry them — a caller has nothing to filter on. An orchestrator with no tag vocabulary returns nothing when tags are given, which is an honest answer. See "Dependencies" below. |

### Claiming

| Method | Contract |
|---|---|
| `Claim(ctx, ItemRef, []ClaimOverride)` → `(Claim, error)` | Acquires an exclusive lease binding `item ↔ arena`, one-to-one in both directions (at most one item per arena, at most one arena per item). **Neither the arena nor the account is a parameter.** Both are ambient, fixed by where the call runs: the arena is the worktree the process sits in, and the account is whoever that arena's credentials act as. An arena must commit, push and merge, so it always has exactly one account, and the orchestrator can read it — a caller-supplied one could only agree or be wrong. `[]ClaimOverride` names safety checks the operator chose to bypass. |
| `Release(ctx, ItemRef)` → `error` | Relinquishes the lease. |
| `LookupClaim(ctx, ItemRef)` → `(*ClaimInfo, error)` | Who holds this item, or nil if unclaimed. `ClaimInfo` names the **arena** holding it — `(HostId, ArenaId)` — alongside the `AccountId` credited. The account alone does not answer it: one person may hold this item in one arena and twenty others elsewhere, and "held by that person" tells an operator nothing about where the work is. |
| `LookupActiveClaim(ctx)` → `(*Claim, error)` | The claim this arena holds right now, or nil. Single source of truth for "what am I currently working on?" **It takes no key**: the arena is ambient, the same way it is for `Claim`, and one arena holds at most one claim — so the question has exactly one answer. Keying it by `AccountId` would not: a person running many arenas holds many claims at once, and no single return value is the right one. |

### State

| Method | Contract |
|---|---|
| `Load(ctx, ItemRef)` → `(*Item, error)` | The item and everything the flow has recorded on it — artifacts, signals, questions and park — in one round. Signals are refreshed by orchestrator-internal polling. **Addressed by ref, not by claim**: reading an item is not a privileged act, and the item is what is being loaded. An orchestrator wanting the shortcut a held claim affords looks up its own — `LookupActiveClaim` takes no argument, so it can always find it. Carries no availability and no blockers; those come from `Get`. |
| `SeedState(ctx, ItemRef, []ArtifactSpec)` → `error` | Pre-loads the artifact set and budget caps. **Must refuse a second seed** for the same item — mid-flight items are frozen against later flow-source changes. **Must refuse an unclaimed item**, and one claimed by another arena. The item is the subject, so the item is the address; holding the lease is a precondition the orchestrator checks, not a value the caller supplies. The orchestrator minted the claim and the arena is ambient, so it has the lease in hand without re-proving anything. |
| `ResetSeed(ctx, ItemRef)` → `error` | Clears the existing seed so the next `SeedState` succeeds. Operator-initiated only; the SDK never calls it automatically. An orchestrator with no separable seed concept refuses; the refusal is typed, so a caller can tell *not supported* from a transient failure. |

**`Load` and `Get` are not two readings of one thing.** `Item` carries the item and what the *flow* wrote onto it: artifacts it produced, signals observed for it, questions it asked, why it parked. `ItemInfo` is the item's standing in the *orchestrator's* own world: open or closed, held, tagged, blocked. One is the flow's record, the other the tracker's, and an item has both independently — a finished flow on a reopened issue, or an untouched item that is already blocked. `status` asks both because an operator's question spans both. Only `Get` needs a `BinaryName` and the predicate, because availability depends on who is asking and lifecycle state does not.

### Editing

| Method | Contract |
|---|---|
| `Edit(ctx, ItemRef)` → `(ItemEditor, error)` | Opens an edit on the item. **Nothing changes until `Commit`.** Opening one is not a lock: another writer may land first, and `Commit` is where that is discovered. |

#### ItemEditor

| Method | Contract |
|---|---|
| `SetTitle(string)` | Replaces the title. |
| `SetBody(string)` | Replaces the body. |
| `AddTag(TagId)` | Adds one tag. Adding one already present changes nothing. |
| `RemoveTag(TagId)` | Removes one tag. Removing one absent changes nothing. |
| `AddBlocker(ItemRef)` | Records that this item waits on that one. Adding one already recorded changes nothing. See "Dependencies". |
| `RemoveBlocker(ItemRef)` | Retracts one dependency. Removing one not recorded changes nothing. |
| `SetManual(bool)` | Sets or clears manual control of the item. |
| `Commit(ctx)` → `error` | Applies every change made on this editor, **or none of them**. |

**The editor is the transaction.** Fields are staged on it and land together, so a caller never has to ask which half of an edit succeeded — before `Commit` nothing has happened, and after it either everything has or nothing has. An orchestrator that cannot write some combination of these together refuses at `Commit` rather than applying the part it can: refusing is an answer, a partial success is not.

Each field keeps its own rule:

- **Tags** are added and removed rather than assigned as a set, because an item's tags have more than one author — the operator's classification, and the markers an orchestrator maintains as consequences of contract operations. Assigning the set entire would silently delete whichever half the caller did not know about. **An orchestrator must refuse to remove a marker it maintains itself**: the owner, binary and blocked markers follow from `Claim`, seeding and a blocker declaration, and a caller able to delete one directly could make an item report a state no operation put it in.
- **Blockers** are added and removed one at a time, for the same reason tags are: they have more than one author. An orchestrator may carry native dependencies a person set through its own interface, so a call that assigned the whole set would erase what the caller never knew was there.
- **Manual** stops anything dispatching the item underneath the person now driving it, and setting it resolves any unresolved park — the operator's `run-step` **is** the resume. Clearing it returns the item to automatic dispatch; an item that could be taken over and never handed back would be stranded by the act of helping it. `Load` **must** report the current value.

**Committing publishes.** The result is visible to everyone who can see the item and is not undone by forgetting it happened, so it is an outward write and subject to whatever guards those — see [disclosure.md](disclosure.md).

### Artifacts

| Method | Contract |
|---|---|
| `ResolveArtifact(ctx, ItemRef, ArtifactId, ArtifactBody)` → `error` | Writes a handler-produced artifact value. There is no orchestrator method for writing signals — signals are written by orchestrator-internal side effects or the `Load` poll path. An empty body is a legal call (side-effect pattern). |
| `MarkStale(ctx, ItemRef, ArtifactId)` → `error` | Flips the stale bit on an artifact, causing its step to re-run. |

### Budget

Every counter is keyed by the `ArtifactId` of the step's artifact: the artifact a step produces is that step's identity, and the budget record hangs off it. It is the only name `grant` accepts. Signal steps produce no artifact, so they own no budget record — they are never counted and never grantable. The counters are transactional with the artifact record.

| Method | Contract |
|---|---|
| `BumpInvocations(ctx, ItemRef, ArtifactId)` → `error` | Increments the invocation counter and resets prompts-this-invocation. |
| `BumpPrompts(ctx, ItemRef, ArtifactId)` → `error` | Increments the prompt counter. |
| `AddCost(ctx, ItemRef, ArtifactId, float64)` → `error` | Adds cost to the running total. |
| `AddDuration(ctx, ItemRef, ArtifactId, time.Duration)` → `error` | Adds elapsed time to the running total. Reported alongside cost when present. |
| `Grant(ctx, ItemRef, ArtifactId, Grant)` → `error` | Adds budget to the artifact record. **Must clear** a `budget-exhausted` park when the grant raises the parked step's offending axis above its consumption. Parks of any other kind, and grants too small to clear the cap, **must** be left in place. The rule is the same for every orchestrator and the SDK provides it — an orchestrator must not re-derive when a grant is sufficient. |

### Parking and questions

| Method | Contract |
|---|---|
| `Park(ctx, ItemRef, ParkRequest)` → `error` | Records that a step stopped without completing, and why. |
| `SaveWorkInProgress(ctx, ItemRef, StepId, string)` → `error` | Stores what a step worked out when it stopped without completing, so the next attempt does not start from nothing. **Never published** — the record is the step's own scratch state, and for a refused write the text to store *is* the text a guard refused. See [resolution.md](resolution.md). |
| `LoadWorkInProgress(ctx, ItemRef, StepId)` → `(string, error)` | Returns what was stored, or empty. |
| `ClearWorkInProgress(ctx, ItemRef, StepId)` → `error` | Discards it. Clearing what is not there is not an error. |
| `PostAnswer(ctx, ItemRef, QuestionId, string)` → `error` | Records a person's answer **against the question it answers**; the `string` is the answer text. The orchestrator stores it on that `Question`, so the question stops being pending — an answer that lands nowhere leaves the question still listed as unanswered. **The outstanding-question marker clears only when no pending question remains:** answering one of three is not answering the item, and clearing on the first resumes a flow still waiting on two. No claim — the person answering does not hold the item. Feeds `answer`. |
| `AskQuestion(ctx, ItemRef, AgentQuestion)` → `(Question, error)` | Records one agent-asked question on the item. The orchestrator assigns it a `QuestionId` and persists the payload. **The return is where a `QuestionId` comes from** — the same question, carrying its id; it is reachable afterwards from `Load` as one of `Item.Questions`. A step with several questions calls this several times, and **each call adds one — there is no replace.** Nothing needs them recorded together; one call per question tells the caller exactly which were recorded, where a partly-failed batch would not; and taking one question rather than a list leaves no way to ask for none, which is a state a parked item cannot be answered out of. **Questions are never removed** — one leaves the pending set by being answered, not by being deleted, so the record of what was asked survives the answering. |

### Completion

| Method | Contract |
|---|---|
| `Finalize(ctx, ItemRef)` → `error` | Marks the item's flow run complete and releases its claim. **Must refuse an item whose `ItemStatus` is not `terminal`.** Finalizing does not *make* an item terminal; there is no method here that closes one, and an item reaches terminal by the orchestrator's own means. So `Finalize` records that the flow is finished with an item already finished, and refusing an open one is what keeps the two facts from drifting: a finalized item still open claims the work is over while the orchestrator says it is not. **This is the only way completion is ever recorded** — nothing infers it. A finished item and an item with no currently eligible step are otherwise identical, so an orchestrator that could not finalize could end nothing: selection would keep offering the item and `status` could never say it was done. Its read is required with it — `Load` **must** report `Item.Finalized` truthfully, because a write nothing can observe is not a record. |

### Worktree

| Method | Contract |
|---|---|
| `Worktree(ctx, ItemRef)` → `(Worktree, error)` | Returns the local-git surface for the item's arena. See "Worktree surface" below. |

## Worktree surface

The `Worktree` interface is the local-git boundary handlers use via `ctx.Worktree()`:

| Method | Contract |
|---|---|
| `Branch(ctx, branch, base BranchName)` → `(created bool, error)` | Ensures the named branch is checked out. Creates it off `base` (or HEAD). Both are branch names. **The bool reports whether it created the branch or found one already there**, which is how a caller tells a fresh start from a resumption. Idempotent. Errors on dirty tree. |
| `CurrentBranch(ctx)` → `(BranchName, error)` | Returns the current branch name. |
| `IsDirty(ctx)` → `(bool, error)` | Reports whether the worktree carries uncommitted changes. |
| `Stage(ctx)` → `error` | Makes every change (including untracked files) visible to the next `CapturePatch`. The guarantee is the outcome, not a particular mechanism — an orchestrator whose `CapturePatch` already accounts for untracked content legitimately implements this as a no-op. |
| `Commit(ctx, string)` → `error` | Commits the tree whole. Every file not ignored is staged. |
| `Push(ctx)` → `error` | Publishes the branch. **May wait** — the orchestrator serializes landing across everything sharing the mainline. Waiting is not failing. |
| `RevParse(ctx, Revision)` → `(CommitSha, error)` | Resolves a revision to a commit SHA. Must answer `"HEAD"` and the item's base branch. Anything beyond is best-effort; an orchestrator that cannot resolve arbitrary revisions returns an error rather than falling back to HEAD. |
| `Run(ctx, CommandName)` → `(CommandRun, error)` | Runs the named command — one of the three `SupportedCommands()` declares. **May modify the worktree or the arena environment**, which is what makes it a command, and why **no landing decision rests on what it reports**. Like `RunGate`, it returns a run rather than a bare error: the `Outcome` separates *ran and reported* from *could not start*, *timed out* or *died*, so a caller can tell a failing check from a command that never executed. A non-nil error means no command was run and no outcome exists. |
| `RunGate(ctx, GateName)` → `(GateRun, error)` | Runs the named `GateName` and returns a `GateRun`, whose `Outcome` distinguishes a real measurement from a gate that timed out, could not start, died, or broke its contract and reports what the runner observed. The SDK is the runner. A non-nil error means no gate was run and no outcome exists. A gate **modifies nothing it measures** — tree or environment. `RunGate(ctx, "integration")` is the landing gate — see below. See [gates-and-commands.md](gates-and-commands.md). |
| `Judge(ctx, GateRun)` → `(GateVerdict, error)` | Asks the project whether a measurement is acceptable. The argument is the `GateRun` that `RunGate` produced — this is the second half of a pair, never called on anything else. The SDK never computes the verdict itself. Only a run whose `Outcome` is `measured` may be judged: an outcome that is not a measurement has nothing to judge. A non-nil error means no verdict exists, and is never a refusal. |
| `CapturePatch(ctx)` → `([]byte, error)` | Produces a unified diff. Returning no bytes is legal — the content may live server-side. |
| `Request()` → `RequestManager` | Returns the `RequestManager` for pull-request operations, or nil when unsupported. |

### `cleanup` may not run

`cleanup` is best-effort, and the reason is structural rather than a tolerance for sloppiness: the arena it would clean can be gone by the time it would run — the machine lost, the worktree destroyed, the process killed between the last step and the tidying. There is no layer that can promise otherwise, because the thing that would keep the promise is the thing that vanished.

**So nothing may depend on `cleanup` having run.** Anything required for correctness belongs where it is guaranteed: a claim is released by `Release` or recovered by the orchestrator observing the holder is gone, never by a cleanup that tidies up after itself. A secret that must not persist must not be written where only `cleanup` would remove it. State a later run needs must be durable in the orchestrator, not left for cleanup to reconcile.

Read the other way, that is what `cleanup` is *for*: the work that is worth doing and survivable to skip. Reclaiming disk, dropping caches, tearing down a scratch database. A next run finds the mess and proceeds.

### `verify` and `integration`

**A gate measures; what it measures is its own business.** A tree, the arena's environment, or both — the subject is the gate's, and in every case it is the arena's and ambient here as everywhere else. The rule that binds all of them is symmetric with what defines a command: a command **may modify the worktree or the arena environment**, and a gate **modifies neither**. That is the whole distinction, and it is why a decision may rest on one and not the other.

**A gate that modifies is a protocol violation, not a gate with a side effect.** The result is not a poor measurement, it is *no measurement*: the state it reported on no longer exists, so nothing can reproduce the answer and no decision may rest on it — the same reason nothing rests on `verify`. The outcome is `broke_contract`, which is where a gate broken in its own code belongs, distinct from a change that failed and from a program that would not start. It is **never** judged.

**A dirty tree is not the violation.** A gate may be pointed at one and should be: `verify` repairs before a gate measures, and uncommitted work is exactly what an operator often wants measured. The requirement is that the tree is **the same afterwards as before**, not that it was clean to begin with. So the check is a comparison, not a flag — `IsDirty` alone only catches a gate that dirtied a clean tree, and says nothing about one that changed a tree already dirty. `CapturePatch` before and after answers it properly.

**No gate requires a claim.** Running one is a measurement, and measuring is not a privileged act. `fit` is not special in that respect either — it is distinguished only by **when** it runs, before an item is taken, which is the whole point of asking it. Everything else about it is ordinary: a `GateName`, a `GateRun` carrying an `Outcome`, a `Judge` verdict, and the rule that **no outcome is not a passing outcome**.

`Run` and `RunGate` are the same shape and not the same kind of thing. `verify` is a **command**: it repairs what is mechanically repairable, then reports whether what remains is sound. It modifies the worktree, and a step runs it as part of producing work. `integration` is a **gate**: it measures whether the mainline would still be green with this change in it, and it modifies nothing it measures.

**No decision rests on `verify`.** A measurement taken by something that also repairs is not reproducible by whoever asks next — the tree it measured no longer exists. `integration` is reproducible by anyone, offline, from the commit alone, and that is the entire reason a decision may rest on it and not on `verify`.

**`integration` must pass before anything is integrated into trunk.** It runs before a change is proposed and again before it lands, and nothing reaches the mainline without it. It measures the **merge result** rather than the branch in isolation — what will actually land, not what was written — which is what `PrepareMergeResult` exists to set up.

An orchestrator supplies both: `Run(ctx, "verify")` runs the command, `RunGate(ctx, "integration")` runs the gate. A project assembles `integration` from smaller gates it names itself, and **each part is separately runnable** — so a failed landing costs one part's re-run rather than the whole suite. What a project puts inside it is the project's decision; that it must pass is not. See [gates-and-commands.md](gates-and-commands.md).

### RequestManager

The optional pull-request surface exposed via `Worktree.Request()`. It is one capability, not six: proposing a change, finding it, measuring the merge result, and landing it are all the same statement — **this orchestrator lands changes through pull requests**. An orchestrator that does not returns nil from `Request()` and implements none of it.

| Method | Contract |
|---|---|
| `Open(ctx, BranchName, title, body string)` → `(RequestUrl, error)` | Opens a pull request and returns its URL. **This is the only place a pull-request URL originates** — `Merge` and `FindPR` consume what this produced. May trigger orchestrator signals (e.g. `pr-open`). |
| `Merge(ctx, RequestUrl)` → `error` | Merges the pull request named by that URL. |
| `FindPR(ctx)` → `(PRInfo, error)` | The pull request for the current claim branch. |
| `PrepareMergeResult(ctx, BranchName)` → `error` | Set the tree to reflect the merge result, so a gate measures what will actually land rather than the branch in isolation. |
| `RevertMergePrep(ctx)` → `error` | Undo that preparation. |
| `RebuildTools(ctx)` → `error` | Rebuild project tools so they match the current tree. Needed after `PrepareMergeResult` changes it — compiled tools go stale when a merge brings newer tool source from the base branch. |

An orchestrator that does not land changes through pull requests refuses these the way it refuses anything else it cannot do — typed, so *never supported here* is distinguishable from *not right now*.

## What an orchestrator may refuse

**Required does not mean always possible.** A required method must exist and must answer; it need not succeed. An orchestrator that cannot do a thing **at all** says so with a typed refusal, and one that cannot do it **right now** says that instead — they are different answers and a caller acts differently on each: the first is permanent and the second is worth retrying. What being required forbids is silence. An absent method leaves a caller nothing to call and no way to ask why; a method that quietly succeeds while doing nothing gives a false answer, which is worse than either.

That is why so little here is optional. A capability an orchestrator lacks is still a question the contract can ask and the orchestrator can decline — and a declined answer is information, where an unimplemented interface is only a gap the caller has to guess around.

- **A second seed.** `SeedState` must refuse when the item is already seeded. Mid-flight items are frozen against later flow-source changes.
- **An empty artifact body** (when the orchestrator stores the bytes). An orchestrator that carries content elsewhere verifies the side effect happened and fails naming what is missing.
- **A disabled item claim.** An item carrying the disabled label is refused.
- **A claim held by another.** Unless the operator passes the `already-held` override.
- **A blocker it cannot resolve**, and **an item declared as its own blocker.** See "Dependencies".
- **Finalizing an item whose `ItemStatus` is `open`.** Only an item the orchestrator already considers finished may have its flow run recorded as complete.
- **Seeding an unclaimed item**, or one claimed by another arena.

## Dependencies

An item waiting on another item that has not finished is `blocked`, its reason is reported alongside, and the blocking items are reported **by reference — their identifiers, as data** — not named inside the reason text. That is [cli.md](cli.md)'s requirement. This section is what an orchestrator owes it.

Dependencies are the orchestrator's knowledge. The SDK carries the references and interprets none of them: it does not resolve a blocker, walk a graph, or decide when one clears. An orchestrator with no dependency notion reports no blockers and refuses every blocker it is given, and is fully conformant — it answers the question rather than lacking the method.

### What an orchestrator reports

`ItemInfo` carries `BlockReason` and `BlockedBy`. Both reads return them — `List` for a listing, `Get` for a single item — so `list` and `status` report the same fact through the same field. There is no third read: an item's blockers are obtained the same way its availability and tags are.

**`BlockedBy` is every blocker declared on the item, each carrying its own `ItemStatus`.** It is the recorded dependency set — what `AddBlocker` put there and `RemoveBlocker` takes away — so a blocker that has since finished stays listed until someone retracts it. A set that quietly dropped satisfied entries could not be edited, because nothing could see what was there to remove.

**Each blocker's status comes with it**, because the list alone answers the wrong question. *These items were declared as blockers* is not *this is what you are waiting on*: an operator wants the one still open, and without its status would have to look up every entry to find it. The orchestrator is not doing extra work to say so — it **cannot** derive whether the item is blocked without already knowing which blockers are unfinished, so the answer exists before anyone asks. What it must not do is resolve anything **beyond** that: a blocker's title, its holder, its own blockers are all a lookup the caller can make itself.

**`Blocked` is the answer to "is this blocked right now?", not `Availability`.** It is item-level and says the same thing whoever is asking.

`Availability` cannot answer it. The ladder is ordered and each item gets one state at the lowest boundary it fails, so `closed` and `unhandled` both sit below `blocked`: an item this binary does not handle reports `unhandled`, and a closed one reports `closed`, each of them blocked or not. Reading `Availability == blocked` therefore answers a narrower question — *is this blocked and otherwise workable by me* — which is the right question for selection and the wrong one for an operator asking why an item is stuck.

Nor is the length of `BlockedBy` the answer. It holds every blocker ever declared, so a non-empty list on an unblocked item simply means they have all finished.

**And it is never `BlockReason`.** That is prose for a person: it explains, it does not instruct. Nothing reads a state out of its wording, and nothing reads one out of its being empty — a block whose explanation nobody wrote is still a block. Everything a caller acts on is a field: `Blocked` says whether, `BlockKind` says what to do about it, `BlockedBy` says what it waits on. The reason exists so a person understands what those three already told the machine.

The `BinaryName` and the predicate that `Get` and `List` take are for availability alone. Blockers are a property of the item and the same whoever asks; nothing about them varies with which binary is looking.

- When an item is blocked, `BlockReason` is set. It is one line, for a person, and it says **whether someone must act or whether the block will clear on its own**. Those are the two cases an operator does something different about, and the reason is the only thing that distinguishes them — the state does not.
- **Who must act and on what is a field, not something to read out of the sentence.** `BlockKind` carries it, and it is `waits-on-items` whenever any blocker is still open. The causes of a block are open-ended — disabled, unmet dependency, rate limit, whatever an orchestrator adds next — but the responses are not: go work the blocker, wait for a condition, or fetch a person. An open-ended cause set is no reason to leave a three-valued fact in prose, where nothing can check it and every orchestrator words it differently.
- **A `waits-on-condition` block is the item's, not the machine's.** A condition that makes *this machine* unable to work anything is unfitness, reported by the `fit` gate. The item is not blocked by it, and **it does not move.** An unfit arena keeps the lease it holds: waiting holds the claim and touches nothing else, so the item is left unparked and unmodified, exactly as it was found. Whether to release it — and let some other arena take it — is a decision, not something that happens because a machine went quiet. An item is `waits-on-condition` only when the condition belongs to the item and would stop any arena alike.
- **The reason names the kind of block, never which items.** *"waiting on unfinished dependencies"* clears on its own; *"disabled by an operator"*, *"waiting for an answer"* and *"budget exhausted on `implementation`"* need a person. Each says what an operator should do about it, and none of them contains an item's identity. `BlockedBy` says which — a reason reading *"blocked by owner/repo#73, owner/repo#74"* is the reference copied into prose, where nothing can act on it and nothing updates it when a blocker lands.
- When the reason is an unfinished dependency, `BlockedBy` carries the blocking items **as `ItemRef`s**, and `BlockReason` **must not** name them. The refs are the reference; prose repeating them is a second copy that nothing can act on and nothing keeps in sync.
- `BlockedBy` carries identities, not display strings. A display is a projection — the GitHub orchestrator's `ResolveRef` rejects one, and the fallback matcher resolves it only by substring, first-match — so a blocker reported as a display can be read by a person and reached by nothing. A blocker nothing can address is not a reference, and the whole point of reporting blockers as data rather than prose is that something can act on them.
- The identifiers are reported alone. An orchestrator does **not** resolve the blockers' own state on their behalf — that is a lookup per blocker for something the caller can look up itself, or spot elsewhere in the listing.

**Blocked-for-dependency is derived, never stored.** An orchestrator answers "is this blocked?" by looking at whether the blockers have finished. It does not keep a blocked bit that some later write must remember to clear. This is the whole of "it will clear on its own": the item whose last blocker finishes is workable at the next read, with nobody having acted.

A stored bit beside a stored edge is the failure this rule exists to prevent. It reads as well-formed, selection honours it, and nothing ever lifts it.

It follows that an orchestrator **must never** report a dependency reason with an empty `BlockedBy`. The two are one fact, and splitting them yields an item that every consumer reads as correct, that names nothing to go work instead, and that no layer can verify — there is nothing to check it against.

A `blocked` with some other cause — disabled, an unanswered question, an exhausted budget — sets `BlockReason` and leaves `BlockedBy` empty. That is not a dependency, and this rule does not reach it.

### What selection does

`ListAutoSelectable` **must not** return a `blocked` item. Auto-selection never picks one, and the orchestrator that knows about the dependency is the one that keeps it out of the selectable set. The SDK does not filter afterwards: it cannot, without knowing what the orchestrator knows, and a rule enforced in two places is a rule with two owners and one of them wrong.

`List` does report blocked items. That difference is the reason the auto-select path must never call it.

### What an orchestrator accepts

`ItemEditor.AddBlocker` and `RemoveBlocker` are the only supported way to record that one item waits on another. Without it the only route is writing to the orchestrator's store by hand — unportable, unvalidated, and invisible to the layer that would have checked it. It is on the required surface for the same reason `Park` is: recording *this step stopped, and why* and recording *this item waits on those* are the same kind of durable statement about an item, and an orchestrator that can write the first can write the second. An orchestrator whose store has no dependency notion still implements it and refuses — which is an answer, where silence is not.

Blockers are added and removed one at a time, never assigned as a set. An item's dependencies can come from a person using the orchestrator's own interface as well as from a flow that discovered one mid-run, so assigning the set entire would delete whichever the caller had not read first — and needing to read first is the tell that the operation was the wrong shape. Each call is idempotent: adding a blocker already recorded, or removing one that is not, changes nothing and is not an error.

It is a field on the item, which is why it is set through `Edit` and not through `Park`. That fold is tempting — a `blocked` park kind already exists — and wrong: **a blocker belongs to the item and a park belongs to a step.** `ParkRequest.Step` names a `StepId`, while blockers are true of the whole item whether or not any step ever ran, so an item-level fact recorded on a step-level record would die when that record cleared and take the dependency with it.

Editing takes no claim. An operator recording a dependency does not hold the item — usually that nobody holds it is the point — so requiring a lease would put it out of reach in its commonest case.

An orchestrator **must** refuse a blocker it cannot resolve. An identifier naming nothing is a typo, and accepting it blocks the item forever on something that does not exist.

**An orchestrator must refuse a cycle**, including an item named as its own blocker. Self-reference is a cycle of length one, and refusing that while accepting length two would be an arbitrary line. What a cycle produces is the failure this whole section exists to prevent, in its worst form: every item in the ring is blocked, each one correctly, and no blocker ever finishes — so nothing clears on its own and the listing sends an operator following *go work the blocker instead* around the ring. Nothing reports it, because every individual item is in a defensible state. Detection costs a traversal, and it is a traversal the orchestrator is already partway through: it reads each blocker's status to derive blockedness at all.

An orchestrator **may** refuse a blocker outside its own scope. What counts as its own scope is the orchestrator's to define.

Those two rules compose, and that is what makes refusing every cycle implementable. An orchestrator that cannot traverse into a blocker cannot see a ring passing through it — so it refuses the blocker. Every blocker it accepts is therefore one it can follow, and every cycle among them is one it can find. An orchestrator never has to detect what it cannot reach; it only has to stop accepting references it cannot reason about.

An already-finished blocker is **accepted**, not refused. Naming a blocker that has landed is not an error — it simply does not block, which the derivation above already reports correctly.

## What this surface does not create

**No method here brings an item into being.** Items originate outside this contract — filed by a person, or by an agent at a terminal — through the orchestrator's own interface. This surface reads them, edits them, leases them and records work against them, and every one of those acts presupposes an item that already exists.

## Artifact schema ownership

`SupportedArtifacts()` returns a closed, curated set. The `(ArtifactId, ArtifactType)` pair is a stable schema that multiple flows — even across projects — must agree on. Owning that schema in the orchestrator keeps it coordinated; letting each flow invent artifacts ad hoc would push schema coordination onto the flows. An orchestrator that can technically store any id (e.g. GitHub) still declares a curated set.

## Cross-references

- [cli.md](cli.md) — the operator-facing behaviour this contract supplies.
- [disclosure.md](disclosure.md) — the guard every outward write passes.
- [artifacts-and-signals.md](artifacts-and-signals.md) — the result kinds the orchestrator stores.
- [step-handler.md](step-handler.md) — what a handler may do with the worktree.
- [github-schema.md](github-schema.md) — how the GitHub orchestrator stores state on an issue.
- [resolution.md](resolution.md) — the lifecycle that calls orchestrator methods.
