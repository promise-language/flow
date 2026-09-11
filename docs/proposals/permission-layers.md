# Permission layers

**Proposal. Not normative.** The floors an action guard enforces in each context, how the dispatcher states which context applies, and what a step's declaration is held against.

[resolution.md](../resolution.md) § Guards defines the action guard, the rule that it may not be authored by the party it constrains, and the layering: what it refuses during a dispatch is the union of "the general rules binding any resolution here, the acting role's restrictions, and the running step's own", and no layer widens another.

Every word of that is scoped to a **dispatch**.

> **The same guard runs where no step dispatched anything, and nothing specifies what it refuses there.**

The binary installed as a tool-use hook fires for every tool call an agent makes on the machine — whether a step launched that agent or a person is driving it at a terminal. Its input carries the hook event, the tool name, the working directory and the tool's own input, and nothing about a step, a role, a flow or a person. So the layered model is specified for one of the guard's two callers, and applied to neither differently.

## Two floors, not an ordering

> **An interactive agent and a dispatched agent each have their own floor, and neither is a narrowing of the other.**

The tempting model is a single floor that the dispatch narrows — one ladder, interactive at the top. It fails on the first capability that inverts, and capabilities do invert in both directions:

- **Answering a parked question, and launching a resolution.** An operator's agent holds both. A dispatched agent must hold neither — see below. Interactive is wider.
- **Rewriting published history.** A force-push is refused outright today, at the only floor that exists, which is why a published branch is reconciled by merging rather than by rebasing. Yet the party with a legitimate claim to that capability is a landing step operating under a declared contract, not a person improvising at a terminal. Dispatch is the side that could be wider.

So which floor is tighter is **a policy decision per capability**, not a property of the contexts. The two floors are independently adjustable, and a mechanism whose soundness depends on one of them being uniformly stricter is a mechanism that breaks the first time the project changes its mind about one capability.

§ Guards' three layers are unaffected: they are the narrowing that happens below the **dispatch** floor, and nothing in this document lets a step, a role or a general rule grant what that floor withholds.

## A role is a third axis, not a further narrowing

> **Context, role and step are three independent axes. None is derivable from another, and none orders the others.**

A role is not a rung below a floor. The same role narrows differently in each context, and knowing the context tells you nothing about which role is in force — an account's coverage and the step's tag decide that ([resolution-standalone.md](../resolution-standalone.md) § Declaring what a binary may do). What composes them is unchanged: the guard refuses the **union of the restrictions** every applicable axis contributes, which is the intersection of what they each permit. Independence is about derivation, not about composition.

Stating it matters because a stack invites a shortcut — read the context, infer the rest — and there is no such inference to make.

## Nothing is read off the machine

> **The floors are the project's. Nothing is taken from the machine beyond what is given: the operating system and the architecture.**

A project is worked interactively and automatically, on any machine, and a permission that varied by host would make the same action allowed in one arena and refused in another with nothing in the tree accounting for the difference. The acts worth refusing are properties of the project and the context, not of whose laptop is in front of them. Operating system and architecture are the exception because they are not choices — a command that does not exist on a platform cannot be permitted there.

## The direct context is the model for both

A person cannot push a change that did not pass the integration gate, and what refuses them is the repository's own permissions rather than any guard. An administrator can defeat that, and the shape of the defeat is the point: it takes standing not everyone has, and a deliberate act, so it cannot happen as a side effect of ordinary work.

> **A bound is worth having when defeating it requires both standing and intent.**

That is the test to hold every mechanism below against. A bound one can cross by accident is not a bound; one anybody can cross is a suggestion.

## What the dispatch floor withholds

Two capabilities an operator's agent holds and a dispatched agent must not.

> **A dispatched agent does not answer its own questions.**

A step that parks on a question is asking a party outside the resolution to decide something the resolution cannot. If the agent inside it could answer, the park would be a formality: the flow would ask itself, proceed, and leave a question record showing a decision nobody made. An operator's agent answering a parked question is the intended case — a person's decision, typed through an agent. The same command issued from inside the step that parked is the same bytes with none of the meaning.

> **A dispatched agent does not run a nested resolution.**

Three reasons, each sufficient alone. The treasurer's ledger is per-resolution, so a nested one spends against a budget nobody granted it. The arena holds one claim, so an inner resolution either fails to claim or takes what the outer holds. And the recursion has no bound, because nothing in the graph counts depth.

The deeper reason is what a resolution *is*: an item somebody decided should be worked, with a plan recording what would be done before anything changed. A resolution launched by an agent mid-prompt has neither — nothing outside the recursion decided the work should happen, which is the one thing the item and the plan exist to record.

## The dispatcher tells the guard everything it needs

> **The context is not inferred, and it is not looked up. The party that launches the agent states it, in full, before the agent exists.**

A step dispatch launches its agent with the **effective policy** in the invocation: which context this is, which step it is, and the composed restrictions that apply — the union of every axis, already resolved. An interactive session launches with the interactive context and nothing else. The agent never chooses its own context, because the choice is made before it runs.

**The guard is told, never asked to read, and never able to ask back.** That distinction is the whole of its integrity:

- A guard that **reads** a configuration has a pointer, and a pointer is a thing to attack. Prescription removes it — the rules are fixed in the guard's own source, which is built rather than committed.
- A guard that **asks** its launcher has a dependency, and a dependency is a thing to be unavailable. It also creates a loop — orchestrator to flow to agent to guard and back to flow — where every tool call waits on a process that is busy running the turn that made the call.
- A guard that is **told** has neither. The parameters arrive from a party the guarded agent cannot be, at a moment it does not exist yet.

**Composition happens once, in the flow, rather than per tool call in the guard.** The flow holds the declarations — the step's `Writes`, the role's restrictions, the context's floor — so it is the only party that can compose them, and it composes them at dispatch. The guard receives a result and applies it.

This is not a second home for the declaration, which is the objection it has to answer. A stored copy would drift from the registration on the first change; a value **recomputed at every dispatch from the single source** cannot. The declaration stays where it is declared, and what travels is derived, fresh, and thrown away when the turn ends.

**A back-channel was considered and rejected**, and the loop is why. It would let the guard ask what a step declared rather than be told, which sounds more honest and is worse in every operational respect: it makes each refusal depend on the flow being reachable, it puts an IPC protocol and its versioning between a tool call and its verdict, and it makes the guard's correctness a function of the flow's liveness at an arbitrary moment mid-turn. Parameters have none of those properties.

**Process ancestry was also considered and rejected.** Walking the ancestor chain for the flow binary is sound in one direction only — an agent cannot make itself a descendant of a binary that did not launch it, but it can leave the chain with a detached spawn and present with no flow ancestor at all. That is only safe while the context it escapes to is uniformly stricter, which two independent floors do not guarantee: for any capability where the dispatch floor is wider, escaping becomes profitable. It also reads process topology, which is manipulated for lifecycle reasons and has nothing to do with permission.

### The parameter path is the capability bound

Removing the loop moves the whole bound into one channel, so **a defect in the parameter path is not a malfunction, it is a hole**. A guard that mishandles what it was told does not fail loudly and stop work; it permits something, once, invisibly, and the run looks exactly like one that had nothing to refuse.

> **Every failure in the parameter path resolves to a refusal.**

The asymmetry is what forces it: a wrong refusal is visible, costs a prompt, and is reported by the party it stopped. A wrong allowance is invisible, costs the guarantee, and is discovered by its consequences. So each way the channel can go wrong has exactly one answer:

| Failure | Answer | Why not the alternative |
|---|---|---|
| No policy passed at all | refuse everything | Treating absence as the interactive floor would make a dropped parameter silently grant the widest set — the one mistake that must not be cheap |
| A policy that does not parse | refuse everything | A partially-read policy is a policy nobody wrote |
| A restriction the guard does not recognise | refuse the act | An older guard meeting a newer flow must not drop rules it cannot enforce; not understanding a bound is not permission to ignore it |
| A context the guard does not recognise | refuse the act | The same reasoning, one level up |
| Policy present, act not covered by any rule | refuse the act | A floor is what is permitted, not what is forbidden: an act no rule admits is outside it |

**The vocabulary is closed, and it is checked the way the others are.** The contexts, the restriction kinds and the policy's shape are closed sets, so a guard can be proved to handle each — the pattern this repository already uses for its wire vocabularies, where an exhaustiveness test walks the declarations and fails when a member is added without a handler. A permission vocabulary is the one that most needs it: an unhandled member is a hole by construction, and the test is what makes adding one loud.

**The dispatch says what it is speaking.** Because the guard must refuse what it cannot understand, the policy carries the contract version it was composed against, and a guard that does not understand that version refuses rather than guesses. Version skew then presents as a run that cannot proceed and names why, which is recoverable, instead of as a run that proceeds with bounds nobody checked.

### What it costs



**The parameter set must be complete at dispatch.** A guard that cannot ask cannot learn a fact nobody passed it, so a rule needing something new needs the dispatcher to pass it — a coupling between what the guard enforces and what the flow sends, which shows up as a version skew rather than as a silent hole. That is the honest trade for removing the loop, and it fails in the direction of a refusal rather than an allowance.

**There is no mid-turn revocation.** A policy composed at dispatch is the policy for the whole turn: a claim lost or an item parked by another party while the turn runs does not reach the guard. That belongs to the gate, which measures the result however it came about, rather than to a guard whose answer is consumed once and never stored ([resolution.md](../resolution.md) § Guards).

## The flow outlives every turn it launched

> **A flow does not exit while a turn it dispatched is still running, and a guard that cannot reach the flow refuses everything.**

The handle is only as good as the party at the other end, and the failure is not hypothetical: a step whose deadline parks it can leave its turn running, and the turn then holds a handle to a process that has gone. That state is **forbidden rather than handled**. A ghost turn outliving its resolution is not a condition to degrade gracefully into — it spends budget nobody is watching, edits a tree nobody is holding, and answers to no declaration.

The rule stands on its own terms: the flow does not terminate while a turn it launched is live, and a turn that outlives its flow is a defect to report rather than a state to support.

**Telling the guard everything up front is what keeps that rule out of the guard's correctness.** A guard that had to ask the flow would be the first thing to break when the flow went, and it would break by being unable to answer — the worst possible failure for the party on the execution path. Told in full at dispatch, it keeps refusing correctly whether or not anything is listening.

**A liveness check is still worth having, as a detector rather than a dependency.** The distinction is which way the need runs. Asking the flow *what is permitted* is a dependency, and it is what this design removes. Observing that the flow *is gone* is corroboration of an invariant the flow itself states: a dispatch policy in force with no flow behind it is the forbidden state, proven rather than suspected. Refusing there costs nothing the guard needed and catches exactly the thing the rule exists to prevent — work lurking in the background after the resolution that authorised it has ended.

It stays optional for the same reason it is safe: a guard that cannot determine liveness has already been told everything it needs, so the check can fail to answer without the guard failing to decide.

**The identity is the PID and its start time, and nothing else.** A PID identifies a *running* process uniquely; paired with the start time the kernel assigned it, it is unique over the whole process space, because a recycled PID necessarily carries a different one. So the dispatch states both, and the guard's check is that a process with that PID exists and started then.

Nothing further belongs in the pair. The executable path is not part of a process's identity: it proves only that some process is running that program, which is the fact least in doubt and the one a second arena's copy of the same binary satisfies. It is also the expensive half — resolving it costs a subprocess on a platform without `/proc` — and the wrong half, since a relocated or rebuilt binary changes the path without changing the process.

**This is an observation, not a conversation.** The guard reads the operating system's process table; it sends the flow nothing and waits on nothing. The loop this design removes is the guard *asking for policy*, and a passive liveness read is not that — which is why it can be added without reintroducing what was rejected. The identity it checks against arrives in the parameters like everything else: told, not discovered.

## Where the floors live

**Both floors are prescribed in the guard's own source.** Neither is stored, so neither is addressable by the thing it constrains: the guard reads no configuration, and the only parsing it does is of the tool payload it is judging. What a dispatch supplies is which floor applies and the restrictions composed for this step — arguments, not a configuration, arriving from a party the agent cannot be.

Because the rules are prescribed, the requirement that the dispatch floor live outside the worktree is already satisfied and needs no mechanism: the guard is a built binary the tree does not contain. Nothing has to be placed anywhere, and there is no file for an `implement` step to find.

> **A guard that is told has no pointer to attack. A guard that reads has one, wherever the reading points.**

That is what terminates the self-reference problem earlier than [proposals/anchoring.md](anchoring.md) does. Anchoring terminates the recursion at a person approving each change; prescription terminates it at the build, with a person having written the policy once. The pattern is already the house one: the precommit guard is a fixed set of checks with no configuration anywhere, the baseline protection among them.

**One pointer does remain, and it is the divergence below**: the hook wiring that invokes the guard is a tracked file, so a step cannot change what the guard refuses but can stop it being invoked.

**Installation is not this document's subject.** How a conformant tree acquires its guards — which tool set, which paths, the hook wiring — is specified by `promise-language/workspace`'s generic-projects proposal, which declares installation and explicitly declares nothing about what a guard permits. A project needing a guard *configured* differently is a request against this document, not a field in that file.

## What this needs that does not exist

- **`AgentRequest` carries no channel for the effective policy.** It holds `Prompt`, `PermissionMode`, `Model`, `Effort`, `MaxCostUSD` and `Worktree` ([agent.md](../agent.md) § AgentRequest). It needs to carry the context, the step's identity, and the composed restrictions — everything the guard will be asked to enforce, because it will not be able to ask for more. The step's name alone is not enough: a name is a thing to look up, and looking up is what this design removes.
- **The action guard has no step layer today, so half of a specified enforcement does not exist.** `flow-registration.md` says `Writes` is "enforced twice: as a layer of the action guard while the step runs, refusing the rest as it is attempted, and checked after the step runs against what actually happened". Only the second exists: the guard's input carries nothing about a step and its source knows nothing of `Writes`, so a forbidden write is caught after the prompt rather than refused during it. That is the layer the handle exists to make possible, and it is a divergence in its own right.
- **The guard's wiring is editable from inside the tree it constrains.** The rules are prescribed and safe; the *installation* is a tracked file, so a step can stop the guard being invoked at all rather than change what it refuses. That is a live divergence from § Guards rather than a design question, and it is filed separately.

## Open questions

None outstanding. Where the composed policy travels is `AgentRequest`'s shape, which is a contract question for [agent.md](../agent.md) rather than an undecided one here.

## Relationship to other documents

- [resolution.md](../resolution.md) § Guards — the guard, the authorship rule, and the three layers below the dispatch floor.
- [agent.md](../agent.md) — the chokepoint this mechanism mirrors, and the request that carries no channel for it.
- [proposals/anchoring.md](anchoring.md) — the self-reference problem, and the recursion this document terminates earlier.
- [proposals/untrusted-sources.md](untrusted-sources.md) — the step declaration that bounds what an accepting flow may read.
- [flow-registration.md](../flow-registration.md) § Roles — where a role's restrictions are declared, and what the handle lets a guard read.
