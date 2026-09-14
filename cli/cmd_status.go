package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

func (app *App) cmdStatus(ctx context.Context, args []string) int {
	fs := app.newFlagSet("status")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 1 {
		return app.usageError("status: unexpected argument %q (status takes an optional item id)", fs.Arg(1))
	}
	mode, ok := of.mode(app, "status")
	if !ok {
		return 2
	}

	var (
		state     *flow.Item
		display   string
		owner     string
		overrides []string
		// elsewhere is whether the item is held by an arena that is not this
		// one. It is read from the CLAIM — who holds it, and where — because a
		// process identity on another machine is not knowable from here in
		// principle, so the only honest evidence is the lease.
		elsewhere bool
	)
	self := arenaKey(flow.ArenaAt(app.Orchestrator.ArenaRoot()))
	if fs.NArg() == 1 {
		// Inspect an arbitrary item READ-ONLY, without claiming it. Reading an
		// item is not a privileged act, so Load is addressed by ref and needs
		// no lease.
		ref, err := app.resolveClaimRef(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		st, err := app.Orchestrator.Load(ctx, ref)
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		state = st
		display = ref.Display
		// Show the lease holder (if any) so the read-only view still tells the
		// operator who, if anyone, currently owns the item.
		if info, _ := app.Orchestrator.LookupClaim(ctx, ref); info != nil {
			owner = string(info.Account)
			// `status <item-id>` inspects without claiming, so the item may be
			// leased on another host. The arena the claim names is what says
			// which, and it is the half that used to be dropped here.
			elsewhere = arenaKey(info.Arena) != self
		}
	} else {
		claim, err := app.Orchestrator.LookupActiveClaim(ctx)
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		if claim == nil {
			fmt.Fprintln(app.Err, "status: no active claim (run `claim <id>` first, or `status <id>` to inspect any item)")
			return 1
		}
		st, err := app.Orchestrator.Load(ctx, claim.ItemRef)
		if err != nil {
			fmt.Fprintln(app.Err, "status:", err)
			return 1
		}
		state = st
		display = claim.ItemRef.Display
		owner = string(claim.Account)
		overrides = claim.Overrides
	}

	// The remit, read exactly as the advance reads it (inRemit) rather than
	// assumed: an item outside it is not this binary's work, and RunOne blocks
	// it without dispatching anything. Reporting the flow here regardless would
	// have `status` call the item eligible while `resolve` stops before
	// dispatch — the one fact answered two ways that statusFlowState exists to
	// prevent.
	var f, typeFlow *flow.Flow
	if inRemit(app, state) {
		// The binary's flow, independent of step eligibility. A
		// finalized/complete item has no eligible step (SelectFlow → nil), but
		// it still belongs to the flow — so its checklist is what gets rendered
		// either way.
		typeFlow = app.Flow
		// A Position refusal leaves f nil, which reads as "no eligible step" —
		// the same thing `status` says about any item this binary will not
		// advance. It is not swallowed anywhere it decides something: RunOne
		// returns it, and this command only reports.
		f, _, _ = SelectFlow(app, state)
	}
	if owner == "" {
		owner = "(unclaimed)"
	}

	payload := statusPayload{
		Item:      display,
		Title:     state.Title,
		Owner:     owner,
		Priority:  string(state.Priority),
		Urgency:   string(state.Urgency),
		Overrides: overrides,
		Flow:      flowName(f, typeFlow),
		FlowState: statusFlowState(state, f, typeFlow),
		// Every kind, not only the one that stops an advance: the block is the
		// item's, and reporting the subset this command acts on would answer a
		// different question from the one `list` answers about the same item.
		blockPayload: blockPayloadOf(state.Blocked, state.BlockKind, state.BlockReason, state.BlockedBy),
		Finalized:    state.Finalized,
		Park:         parkPayloadOf(state.Park),
		// The route, not a checklist (docs/cli.md § Status). Load already
		// returned the journal, the ledger and the awaited marker, so none of
		// the three costs a read the command was not already making.
		Journal:   journalPayloads(state.Journal),
		Awaits:    awaitsPayloadOf(state.Awaits),
		Spend:     spendPayloadOf(state.Ledger),
		Steps:     app.stepPayloads(typeFlow, state, elsewhere),
		Questions: questionPayloads(state),
		Waiting:   waitingPayloadOf(display),
	}

	return app.emit(mode, payload, func() {
		fmt.Fprintf(app.Out, "item:  %s\n", payload.Item)
		// Right under the id, where "which task IS this?" is actually asked.
		// Dropped entirely when the backend has no title, so the header never
		// carries an empty field.
		if line := titleLine(payload.Title); line != "" {
			fmt.Fprintf(app.Out, "title: %s\n", line)
		}
		fmt.Fprintf(app.Out, "owner: %s\n", payload.Owner)
		// The two selection axes, reported as `list` reports them — one fact
		// through one pair of fields, whichever command asks.
		fmt.Fprintf(app.Out, "priority: %s\n", payload.Priority)
		fmt.Fprintf(app.Out, "urgency: %s\n", payload.Urgency)
		if len(payload.Overrides) > 0 {
			fmt.Fprintf(app.Out, "overrides: %s\n", strings.Join(payload.Overrides, ", "))
		}
		fmt.Fprintf(app.Out, "flow:  %s\n", statusFlowLine(state, f, typeFlow))
		fmt.Fprintln(app.Out)

		// The route so far, above the steps still to come, so the page reads in
		// the order the work happened. Dropped entirely for an item with no
		// entries — an unstarted item has an empty route, and a header over
		// nothing is a line an operator learns to skip.
		printRoute(app, payload.Journal)

		// Only the type-matching flow's checklist. The "flow:" line names it, so
		// no redundant header. If no flow handles this item's type, there's
		// nothing.
		printChecklist(app, payload.Steps)

		// Whose move it is, and what the run has cost — the two facts the
		// checklist cannot carry because neither belongs to a step.
		if a := payload.Awaits; a != nil {
			fmt.Fprintf(app.Out, "\nwhose move: %s\n", awaitsLine(a))
		}
		if s := payload.Spend; s != nil {
			fmt.Fprintf(app.Out, "spend: %s\n", spendLine(s))
		}
		// A run that is alive and deliberately idle. Reported because the
		// alternative reading of "claim held, nothing running" is a stalled
		// run, and an operator who reads it that way goes looking for a process
		// to kill.
		if w := payload.Waiting; w != nil {
			fmt.Fprintf(app.Out, "waiting: %s\n", waitingLine(w))
		}

		// Above the park, because a park is one of the things a block is
		// derived FROM: reading "blocked: waits-on-person — budget exhausted"
		// and then the park that says which step and which axis is the order
		// the two facts explain each other in.
		if payload.Blocked {
			fmt.Fprintf(app.Out, "\nblocked: %s\n", blockLine(payload.blockPayload))
		}
		if payload.Park != nil {
			fmt.Fprintf(app.Out, "\nparked: %s\n", parkLine(payload.Park))
			// What would clear it, from the one table `resolve` narrates a park
			// with. docs/cli.md § Status asks for "the park, with what would
			// clear it", and the kind is what knows.
			if act := resumingAct(flow.ParkKind(payload.Park.Kind), selfPath(app.Name)); act != "" {
				fmt.Fprintf(app.Out, "  to resume: %s\n", act)
			}
		}
		if len(payload.Questions) > 0 {
			fmt.Fprintln(app.Out, "\nquestions:")
			for _, q := range payload.Questions {
				// AN UNANSWERED QUESTION PRINTS IN FULL — header AND text,
				// unclipped. The one-line form is right for a listing of many
				// and wrong for the one the item is parked on, which is what
				// the operator opened `status` to read: clipped to a header,
				// the options, the evidence and the recommendation reached
				// --json only, and reading them meant `gh api` against the raw
				// comment.
				//
				// An ANSWERED one stays on its line. It is history, and the
				// whole history unrolled would bury the question that is still
				// waiting.
				if q.Answered {
					fmt.Fprintf(app.Out, "  [x] %s — %s\n", q.ID, questionLine(q))
					continue
				}
				fmt.Fprintf(app.Out, "  [ ] %s\n", q.ID)
				for _, line := range questionBlock(q) {
					fmt.Fprintf(app.Out, "      %s\n", line)
				}
			}
		}
	})
}

// registeredTypes lists the flow's remit, sorted and deduplicated, for a
// message that has to tell an operator what the item's type should have been.
// "none" when the flow declares no type — which, because an empty Types() set
// is universal, is also the only case where the caller's no-match verdict could
// not have come from a type list at all.
func registeredTypes(app *App) string {
	seen := map[flow.ItemType]bool{}
	var types []string
	for _, t := range app.Flow.Types() {
		if !seen[t] {
			seen[t] = true
			types = append(types, string(t))
		}
	}
	if len(types) == 0 {
		return "none"
	}
	slices.Sort(types)
	return strings.Join(types, ", ")
}

// flowName is the bare flow name for the payload: the eligible flow, else the
// type-matching one, else empty.
func flowName(eligible, typeFlow *flow.Flow) string {
	if eligible != nil {
		return eligible.Name()
	}
	if typeFlow != nil {
		return typeFlow.Name()
	}
	return ""
}

// statusFlowState is the machine-readable counterpart of statusFlowLine: the
// same decision, as a closed enum instead of a rendered string.
//
// The "will the next advance run a step?" half of it is RunOne's own predicate,
// blockedFromAdvancing, rather than a restatement of it: an item that reads
// `eligible` here and stops before dispatch there is the one fact answered two
// ways this reports to prevent. It displaces `eligible` and nothing else — a
// finalized item is finalized whatever it waits on, and an item with no
// eligible step has nothing to be blocked from, which is exactly where RunOne
// puts the check too.
func statusFlowState(state *flow.Item, eligible, typeFlow *flow.Flow) string {
	if state.Finalized {
		return flowStateFinalized
	}
	if eligible != nil {
		if blockedFromAdvancing(state) {
			return flowStateBlocked
		}
		return flowStateEligible
	}
	if typeFlow != nil {
		return flowStateNoEligibleStep
	}
	return flowStateNoMatchingFlow
}

// statusFlowLine renders the "flow:" value. A finalized item is reported as
// finalized (its run is complete — NOT "no flow eligible", which misleadingly
// implies unstarted/blocked). "(finalized)" is shown ONLY when the persistent
// Item.Finalized flag is set, which only Finalize writes. Otherwise: the
// currently-eligible flow's name; or, when no step is eligible, the
// type-matching flow tagged "(no eligible step)"; or a no-match note.
//
// There is no "(not seeded)" rung any more. Nothing seeds an item, so an item
// with nothing recorded is one whose entry step is pending — which reads as the
// eligible flow, not as a state of its own.
func statusFlowLine(state *flow.Item, eligible, typeFlow *flow.Flow) string {
	if state.Finalized {
		if typeFlow != nil {
			return typeFlow.Name() + " (finalized)"
		}
		return "finalized"
	}
	if eligible != nil {
		if blockedFromAdvancing(state) {
			return eligible.Name() + " (blocked)"
		}
		return eligible.Name()
	}
	if typeFlow != nil {
		return typeFlow.Name() + " (no eligible step)"
	}
	return "(no matching flow)"
}

// blockLine renders a block for humans the way parkLine renders a park: the
// kind, then the reason. A block that names items carries a second, indented
// line listing the ones still open — the references are what the operator goes
// and works instead, and the reason never names them.
func blockLine(b blockPayload) string {
	var sb strings.Builder
	sb.WriteString(b.BlockKind)
	if b.BlockReason != "" {
		fmt.Fprintf(&sb, " — %s", b.BlockReason)
	}
	if line := blockedByLine(b.BlockedBy); line != "" {
		fmt.Fprintf(&sb, "\n  %s", line)
	}
	return sb.String()
}

// stepPayloads projects a flow's lifecycle items onto the state. Returns an
// empty (non-nil) slice when no flow handles the item's type, so the JSON
// carries [] rather than null.
func (app *App) stepPayloads(f *flow.Flow, state *flow.Item, elsewhere bool) []stepPayload {
	if f == nil {
		return []stepPayload{}
	}

	// Load the running-step record and verify liveness before entering the
	// per-step loop. A stale record (dead PID or wrong exe) is silently
	// ignored — the step will report as pending.
	var runningStep string
	var runningPID int
	var runningExe string
	if rec, err := clistate.LoadRunning(); err == nil && rec != nil {
		if clistate.ProcessAlive(rec.PID, rec.Exe) {
			runningStep = rec.Step
			runningPID = rec.PID
			runningExe = rec.Exe
		}
	}

	// The pending step is the first one still outstanding, and only it can be
	// executing elsewhere.
	markedElsewhere := false

	items := f.Items()
	out := make([]stepPayload, 0, len(items))
	for _, li := range items {
		sp := stepPayload{
			ID:       string(li.Result()),
			Label:    li.Description,
			Required: li.Required,
		}
		switch li.Kind {
		case flow.LifecycleArtifact:
			// Consumption from the step's ledger row, caps from
			// flow.EffectiveBudget — the same arithmetic the pre-dispatch gate
			// and `grant` use, so the three cannot report different caps for one
			// step. Prompts show no consumption: the counter is per-invocation
			// and lives only inside a running dispatch.
			row := state.Ledger.Row(li.Result())
			eff := app.effectiveBudget(state, li.Result())
			sp.Kind = kindArtifact
			sp.State = artifactState(state, li.ArtifactId)
			sp.Budget = &budgetPayload{
				Invocations:          intAxis{Used: row.Dispatches, Granted: eff.MaxInvocations},
				CostUSD:              costAxis{Used: row.CostUSD, Granted: eff.MaxCostUSD},
				PromptsPerInvocation: intAxis{Granted: eff.MaxPromptsPerInvocation},
				TimeoutSeconds:       grantedOnly{Granted: int(eff.Timeout.Seconds())},
			}
		case flow.LifecycleSignal, flow.LifecycleAwait:
			if li.Kind == flow.LifecycleSignal {
				sp.Kind = kindSignal
			} else {
				sp.Kind = kindAwait
			}
			sp.State = statePending
			if state.SignalSet(li.SignalId) {
				sp.State = stateResolved
			}
			// Budget stays nil: signal steps own no budget record, which is
			// exactly what makes them invalid grant targets.
		}
		// A pending step that matches the verified running record is
		// promoted to running. Running only overrides pending — a resolved
		// step keeps its state even if a stale record names it.
		if sp.State == statePending && sp.ID == runningStep {
			sp.State = stateRunning
			sp.RunningPID = runningPID
			sp.RunningExe = runningExe
		} else if sp.State == statePending && elsewhere && !markedElsewhere {
			// Executing under another party's claim. It displaces PENDING on
			// the PENDING STEP ONLY — the first one still outstanding, which is
			// the one a holder would be working — so the steps behind it stay
			// pending rather than all claiming to be running somewhere.
			//
			// It carries no process: what is known here is the lease, and a
			// process identity on another machine is not knowable from this
			// one. The correct response is to leave the item alone, not to go
			// looking for a stalled process — which is what `pending` on an
			// item leased elsewhere invites.
			sp.State = stateElsewhere
			markedElsewhere = true
		}
		out = append(out, sp)
	}
	return out
}

// artifactState mirrors Flow.stepPending's view of one artifact record.
// Resolved or pending, and nothing else: the operator opt-out and the stale bit
// both went with seeding and MarkStale, and nothing writes either any more.
func artifactState(state *flow.Item, id flow.ArtifactId) string {
	if state.Artifact(id).Resolved {
		return stateResolved
	}
	return statePending
}

// journalPayloads projects the journal onto the payload. The journal is the
// route so far, and Load already returned it whole — nothing here reads
// anything.
//
// Returns an empty (non-nil) slice for an item with no entries, so the JSON
// carries [] rather than null: an unstarted item has an empty route, not an
// unknown one.
func journalPayloads(entries []flow.JournalEntry) []journalEntryPayload {
	out := make([]journalEntryPayload, 0, len(entries))
	for _, e := range entries {
		out = append(out, journalEntryPayload{
			Step:            string(e.Step),
			Execution:       e.Execution,
			By:              string(e.By),
			Role:            string(e.Role),
			Elected:         electionOf(e.Route),
			Reason:          e.Message,
			Note:            e.Note,
			At:              e.At,
			CostUSD:         e.Spend.CostUSD,
			DurationSeconds: e.Spend.Duration.Seconds(),
		})
	}
	return out
}

// electionOf renders what an execution elected: its successor, or the
// finalization with the disposition it ended on. One renderer, so the human
// route line and the payload cannot describe one election two ways.
func electionOf(r flow.Route) string {
	if r.Finalizes() {
		return "finalize:" + string(r.Finalize)
	}
	return string(r.Next)
}

// awaitsPayloadOf reports whose move it is. Nil when the item awaits nothing —
// unstarted, or finalized — because "nobody's move" and "a move nobody
// recorded" are the same fact here and neither is a role.
func awaitsPayloadOf(a flow.Awaits) *awaitsPayload {
	if a.Empty() {
		return nil
	}
	return &awaitsPayload{
		Role:    string(a.Role),
		Account: string(a.Account),
		Signal:  string(a.Signal),
	}
}

// spendPayloadOf reports the treasurer's item-level record, with WAITING APART
// from active time. Nil when nothing has been spent and nothing has run: a
// zeroed spend block on an unstarted item reads as a measurement.
func spendPayloadOf(l flow.Ledger) *spendPayload {
	if l.TotalCostUSD == 0 && l.TotalActive == 0 && l.TotalWaiting == 0 {
		return nil
	}
	return &spendPayload{
		CostUSD:        l.TotalCostUSD,
		ActiveSeconds:  l.TotalActive.Seconds(),
		WaitingSeconds: l.TotalWaiting.Seconds(),
	}
}

// waitingPayloadOf reports a run that is alive, holds this arena's claim, and
// has no step executing — pacing, or between steps.
//
// OBSERVED, NEVER ASSUMED, exactly as a running step is: the registration must
// name this item and its holder must be alive right now. A record left behind
// by a process that died would otherwise report a run that is waiting for
// something when in fact it stopped hours ago, which is the failure the
// liveness rule exists to prevent — and here it would be worse than for a
// running step, because "waiting" invites the operator to keep waiting too.
func waitingPayloadOf(display string) *waitingPayload {
	rec, err := clistate.LoadRunning()
	if err != nil || rec == nil {
		return nil
	}
	// A record naming a step is a DISPATCH, reported as the running step. Only
	// a record with no step is a wait, so the two can never both be claimed.
	if rec.Step != "" || rec.Waiting == "" || rec.Item != display {
		return nil
	}
	if !clistate.ProcessAlive(rec.PID, rec.Exe) {
		return nil
	}
	return &waitingPayload{Reason: rec.Waiting, Until: rec.WaitUntil, PID: rec.PID}
}

func questionPayloads(state *flow.Item) []questionPayload {
	out := make([]questionPayload, 0, len(state.Questions))
	for _, q := range state.Questions {
		out = append(out, questionPayload{
			ID: string(q.ID), Header: q.Header, Text: q.Text, Answered: q.Answer != "",
		})
	}
	return out
}

// printRoute renders the journal: one line per completed execution, in order,
// with who ran it, in which role, what it elected and why.
//
// Nothing at all for an empty journal. An unstarted item has an empty route,
// and a header over no entries is a line that says something happened.
func printRoute(app *App, entries []journalEntryPayload) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintln(app.Out, "route:")
	for _, e := range entries {
		var b strings.Builder
		fmt.Fprintf(&b, "  %s", e.Step)
		// Only past the first: a step the route reached once needs no counter,
		// and printing "#1" on every line buries the one that says "#3".
		if e.Execution > 1 {
			fmt.Fprintf(&b, " #%d", e.Execution)
		}
		if e.By != "" {
			fmt.Fprintf(&b, " — %s", e.By)
			if e.Role != "" {
				fmt.Fprintf(&b, " as %s", e.Role)
			}
		} else if e.Role != "" {
			fmt.Fprintf(&b, " — as %s", e.Role)
		}
		fmt.Fprintf(&b, " → %s", e.Elected)
		if suffix := entrySpend(e); suffix != "" {
			fmt.Fprintf(&b, " %s", suffix)
		}
		fmt.Fprintln(app.Out, b.String())
		// The reason and the note go under the line rather than on it: both are
		// free prose from a step, and spliced inline they push the election —
		// the fact the line is scanned for — off the screen. Bounded through
		// titleLine, the one renderer free backend text goes through.
		if line := titleLine(e.Reason); line != "" {
			fmt.Fprintf(app.Out, "      %s\n", line)
		}
		if line := titleLine(e.Note); line != "" {
			fmt.Fprintf(app.Out, "      note: %s\n", line)
		}
	}
	fmt.Fprintln(app.Out)
}

// entrySpend renders one execution's cost, in the shape a step outcome already
// uses. Empty when the entry records neither, which is what an entry written
// before the ledger tracked them looks like.
func entrySpend(e journalEntryPayload) string {
	if e.DurationSeconds == 0 && e.CostUSD == 0 {
		return ""
	}
	return fmt.Sprintf("(%s, $%.2f)",
		formatDurationCompact(time.Duration(e.DurationSeconds*float64(time.Second))), e.CostUSD)
}

// awaitsLine renders whose move it is: the awaited role with its account of
// record, or the signal a pending wait is held on.
func awaitsLine(a *awaitsPayload) string {
	if a.Signal != "" {
		// Nobody's move. An awaited signal waits on an observation, and there
		// is no party to name — saying so is the point, because "awaiting
		// maintainer" and "awaiting an observation" call for different acts.
		return "nobody — awaiting signal " + a.Signal
	}
	if a.Account != "" {
		return fmt.Sprintf("%s (%s)", a.Role, a.Account)
	}
	return string(a.Role)
}

// spendLine renders the treasurer's item-level record, with WAITING APART from
// active time: a total that folded the two together would read as a step that
// took hours to do a minute's work.
func spendLine(s *spendPayload) string {
	line := fmt.Sprintf("$%.2f, %s active",
		s.CostUSD, formatDurationCompact(time.Duration(s.ActiveSeconds*float64(time.Second))))
	if s.WaitingSeconds > 0 {
		line += fmt.Sprintf(" (%s waiting)",
			formatDurationCompact(time.Duration(s.WaitingSeconds*float64(time.Second))))
	}
	return line
}

// waitingLine renders a deliberately idle run: what it waits on, and until when
// where the run knows. A wait that ends on a re-measurement rather than a clock
// reports no instant, because inventing one would be a prediction.
func waitingLine(w *waitingPayload) string {
	line := w.Reason
	if !w.Until.IsZero() {
		line += " until " + w.Until.Local().Format("15:04")
	}
	if w.PID != 0 {
		line += fmt.Sprintf(" (pid %d)", w.PID)
	}
	return line
}

// questionBlock is an unanswered question in FULL: its header, then its text,
// each unclipped and each kept on the lines the asker wrote them on.
//
// No titleLine here, and that is the whole point of the function. Free backend
// prose is bounded everywhere else because it sits in a header or a cell that a
// paragraph would swamp; this is the paragraph the operator opened `status` to
// read, and the ask convention puts the options, the evidence and the
// recommendation in it.
func questionBlock(q questionPayload) []string {
	var out []string
	if h := strings.TrimSpace(q.Header); h != "" {
		out = append(out, strings.Split(h, "\n")...)
	}
	if t := strings.TrimSpace(q.Text); t != "" {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, strings.Split(t, "\n")...)
	}
	return out
}

// questionLine renders a question as ONE bounded line, the way titleLine
// bounds the title.
//
// Header first, and Text only when there is no header: the header is the short
// scannable form — under the ask convention it is the question itself — while
// Text is where a whole fenced evidence block belongs. A checklist entry built
// from Text splices that block across the listing and buries the question that
// the entry exists to show. JSON carries both unclipped.
func questionLine(q questionPayload) string {
	if s := titleLine(q.Header); s != "" {
		return s
	}
	return titleLine(q.Text)
}

func parkPayloadOf(p *flow.ParkRequest) *parkPayload {
	if p == nil {
		return nil
	}
	pp := &parkPayload{
		Kind:   string(p.Kind),
		Step:   string(p.Step),
		Axis:   string(p.Axis),
		Reason: p.Reason,
	}
	for _, a := range p.Axes {
		pp.Axes = append(pp.Axes, axisReportPayload{
			Axis:      string(a.Axis),
			Used:      a.Used,
			Granted:   a.Granted,
			Exhausted: a.Exhausted,
		})
	}
	return pp
}

// parkLine renders a park for humans: "budget-exhausted on "implementation"
// (invocations) — ran 3 times without resolving "implementation"".
//
// A budget park carries a second, indented line listing every axis, because
// the headline names only the axis that tripped first. Granting that one and
// nothing else is what turned a single blocked step into one operator
// round-trip per axis; the axes tagged "(flat)" are the ones a grant has to
// cover to actually get the step moving.
func parkLine(p *parkPayload) string {
	var b strings.Builder
	b.WriteString(p.Kind)
	if p.Step != "" {
		fmt.Fprintf(&b, " on %q", p.Step)
	}
	if p.Axis != "" {
		fmt.Fprintf(&b, " (%s)", p.Axis)
	}
	if p.Reason != "" {
		fmt.Fprintf(&b, " — %s", p.Reason)
	}
	if line := axesLine(p.Axes); line != "" {
		fmt.Fprintf(&b, "\n  axes: %s", line)
	}
	return b.String()
}

// axesLine joins the per-axis meters into one " · "-separated run.
func axesLine(axes []axisReportPayload) string {
	reports := make([]flow.AxisReport, len(axes))
	for i, a := range axes {
		reports[i] = flow.AxisReport{
			Axis:      flow.BudgetAxis(a.Axis),
			Used:      a.Used,
			Granted:   a.Granted,
			Exhausted: a.Exhausted,
		}
	}
	return flow.FormatAxes(reports)
}

// statusTitleMax bounds the human "title:" line, in runes — and the title cell
// of the human listing row, which renders through the same titleLine.
// Item.Title is free prose the backend supplies — a pasted paragraph or a
// 300-char sentence would swamp the three-line header the operator actually
// reads and wrap it across the terminal. JSON carries the title unclipped, so
// nothing that needs the whole string loses it.
const statusTitleMax = 72

// oneLine collapses free backend text onto ONE line: every whitespace run
// (newlines and tabs included) becomes a single space and the ends are
// trimmed. Returns "" for text that is empty or all whitespace. It is the
// collapse behind titleLine, and the human listing row applies it to every
// cell it fills with backend text — there tab is the column separator, so an
// uncollapsed tab would shift every cell after it.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// titleLine renders a title as ONE bounded line: oneLine's collapse, so a
// multi-line title cannot break the header block, then clipped to
// statusTitleMax RUNES — not bytes, which would split a multi-byte character
// mid-sequence and print a replacement glyph. Returns "" for a title that is
// empty or all whitespace; the caller drops the line entirely. The human
// listing row's title cell uses it too.
func titleLine(title string) string {
	s := oneLine(title)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= statusTitleMax {
		return s
	}
	return string(r[:statusTitleMax]) + "…"
}

// printChecklist renders the lifecycle checklist ID-FIRST: the id is the
// step's identity (it keys the budget record and it is the only name `grant`
// accepts), so it leads the line and the human label trails it. Steps that own
// no budget say so, which is what makes the listing sufficient on its own to
// know what can be granted.
func printChecklist(app *App, steps []stepPayload) {
	width := 0
	for _, s := range steps {
		if len(s.ID) > width {
			width = len(s.ID)
		}
	}
	for _, s := range steps {
		fmt.Fprintf(app.Out, "  %s %-*s  %s", stepMarker(s.State), width, s.ID, s.Label)
		if note := budgetNote(s); note != "" {
			fmt.Fprintf(app.Out, "  %s", note)
		}
		if s.State == stateRunning && s.RunningPID != 0 {
			fmt.Fprintf(app.Out, "  (pid %d)", s.RunningPID)
		}
		// The state is NAMED for a step executing elsewhere. A marker alone
		// would leave an operator to guess, and the guess that matters here is
		// the wrong one — a step that looks stalled invites them to go take the
		// item over, when the correct response is to leave it alone.
		if s.State == stateElsewhere {
			fmt.Fprintf(app.Out, "  (%s)", stateElsewhere)
		}
		fmt.Fprintln(app.Out)
	}
}

func stepMarker(state string) string {
	switch state {
	case stateResolved:
		return "[x]"
	case stateRunning:
		return "[>]"
	case stateElsewhere:
		// Its own glyph, because it is its own state: not "[>]", which this
		// arena says only of a process it has OBSERVED, and not "[ ]", which
		// says nobody is working it.
		return "[~]"
	}
	return "[ ]"
}

// budgetNote is the trailing column: consumption for artifact steps that have
// any, and the "not a grant target" tag for signal steps.
func budgetNote(s stepPayload) string {
	if s.Budget == nil {
		return "(signal — no budget)"
	}
	b := s.Budget
	if b.Invocations.Used == 0 && b.CostUSD.Used == 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if b.Invocations.Granted > 0 || b.Invocations.Used > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d inv", b.Invocations.Used, b.Invocations.Granted))
	}
	if b.CostUSD.Granted > 0 || b.CostUSD.Used > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f/$%.2f", b.CostUSD.Used, b.CostUSD.Granted))
	}
	return strings.Join(parts, " · ")
}
