# Labels

> **Tag:** `tags` — remaining work to complete this document: the query named in
> [`docs/index.md`](index.md).

**Normative.** The label vocabulary for this repository's issues, and the rules for applying it. Every statement is a requirement. Consult this document before labelling an issue.

**Out of scope:** issue titles, bodies and state. This governs the labels field only.

Labels are how an issue is found again. A vocabulary that grows by improvisation stops being a way to find anything: two people file one class of defect under two names, both searches look complete, and neither is. So the vocabulary is small, written down, and extended deliberately.

## Three kinds, and only three

| Kind | Spelling | Applied by |
|---|---|---|
| **Document tags** | the file's basename minus `.md` — `resolution`, `environment`, `cli` | a person, when the issue is a gap against that specification |
| **Flow machinery** | `flow:*` | the flow, never by hand ([github-schema.md](github-schema.md) § Labels) |
| **Cross-cutting axes** | named below | a person, when the axis applies |

**Document tags are the primary vocabulary**, and most issues need nothing else. Each root document declares its own in its header, and the remaining work for a specification is the open issues carrying it ([index.md](index.md)). An issue against two specifications carries both; an issue against none is either not a gap or is missing its document.

**`flow:*` is never applied or removed by hand.** A claim, an owner, a park, a priority: these are machine state, and editing one lies to the flow about work in progress. The priority and urgency axes live there — `flow:priority:<critical|high|low>` and `flow:urgency:<next|deferred>` — and their neutral values have no label ([github-schema.md](github-schema.md) § Labels).

## Cross-cutting axes

An axis is not a topic. It says what **kind of harm** an issue represents, which the document tags cannot: those say which specification is unmet, and two gaps against one document can be nothing alike. An axis earns a place here only when the harm it names is one that no other axis would surface.

### `wasteful`

> **The system spends something it already had.**

Applied when the defect is that work already paid for is discarded and then bought again — a conversation thrown away and restarted, reasoning re-derived because nowhere kept it, a measurement taken twice, an arena released while it still held the state the next dispatch needs. It is the label for a violation of [resolution.md](resolution.md) § Nothing is bought twice.

**The discriminator is whether the system already had the answer.** Work that is expensive because it is expensive is not wasteful: a gate suite takes what it takes, and an agent reading a repository it has not read is paying a fair price. Work is wasteful when the resolution was holding the answer and spent to produce it a second time.

**It exists as its own axis because waste is invisible to every other one.** A wasteful defect passes its tests, produces correct output, routes correctly, and reports success — nothing about the run looks wrong, and no document tag distinguishes it from a gap that merely costs nothing. What it spends is the agent's budget, the operator's time, and the throughput of every other item sharing the allowance, and none of those three is spent by the party that decided not to keep the thing.

**It is applied alongside a document tag, never instead of one.** The axis says what kind of harm; the document tag says which requirement is unmet. An issue carrying only `wasteful` names a cost without naming what is wrong.
