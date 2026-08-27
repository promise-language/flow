# Orchestrated resolution

**Normative.** How an item is resolved when a **central server** schedules the work and the flow binary executes one step on command.

Everything in `resolution.md` applies. This document states only what is specific to the orchestrated model; a statement true of both drive models belongs there, not here.

This document specifies **what the flow binary must do** under an orchestrator. The orchestrator's own internals — how it ranks work, how it places items on machines, how it stores its ledger — are outside this SDK and outside this document.

## What "orchestrated" means

A server owns scheduling. It decides which item runs, where it runs, and when. The binary is invoked to advance **one step**, and returns.

The binary is therefore **not** the system, and does not behave as though it were:

- It does not select its own work. An item is given to it.
- It does not decide when to run again. The server decides.
- It does not loop. One invocation, one step, one report.

The loop that drives an item to completion belongs to the server. A binary that ran the loop itself would be making scheduling decisions the server has already made, on staler information.

## The lease

Exclusivity is held by the server, in a ledger it owns. The binary does not arbitrate access — it presents itself and is either granted the item or refused.

**Every refusal is typed.** The binary distinguishes the reasons without reading prose, because it acts on them differently: a refusal another item might survive means trying elsewhere, and a refusal no item would survive means stopping. A refusal that cannot be told apart from another is one the binary must treat as fatal.

A refusal carries what a person needs to clear it: a one-line reason, the failing check's own output reproduced verbatim, and the flag that would override it where an override exists.

The lease outlives a single invocation. Steps run under a lease the server granted earlier, and re-proving it on every step would put a recurring check on the path of every step and let a transient failure kill work in flight.

## Placement

An item runs where the server says. The binary does not choose its machine, and does not move an item between machines.

A machine's fitness to receive work is established **before** work is given to it, not discovered part-way through an item. The binary reports what it finds about its own environment; the server decides what to do with it.

## Reporting back

The binary's report is the server's only account of what happened. Every invocation reports one status, and the report is the authority — the server does not infer outcomes from side effects.

**Health classification travels with the report.** Whether a failure was the work's fault or the environment's determines whether it counts against a retry ceiling and how long the server waits, and the binary is the only party that saw it happen. A failure reported without that distinction is one the server has to guess about.

Infrastructure failures consume no attempt. The binary that observed a transient environment failure says so, and the server does not spend the item's budget on it.

## No branch, no pull request

Where the orchestrated backend commits directly to the mainline, there is no per-item branch and no pull request, and the SDK's branch and request surfaces do not apply.

A step that assumes a branch, or that terminates in a request, is a step that does not run in this model. Flows written for both models do not assume either mechanism.

Nothing here weakens the recording requirement in `resolution.md`: every modification a step makes is still either recorded or refused. Without a branch, the mainline is the record, which makes it *more* important that nothing unrecorded is left behind, not less.

## Interruption

A binary invoked per step is interrupted routinely — the server restarts, a machine is reclaimed, a run is cancelled. Interruption is normal operation, not an error path.

The item's durable state is what survives, and it is sufficient on its own to derive the next step. Nothing in the binary's memory is required to resume, and nothing about resumption depends on the same machine being used again.

An interrupted step's claim is not silently abandoned. Recovering a lease held by something no longer running is the server's responsibility, and it requires observing that the holder is gone rather than assuming it from elapsed time.
