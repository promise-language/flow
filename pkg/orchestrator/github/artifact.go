package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
	"github.com/promise-language/flow/pkg/machinecache"
)

// artifactCommentMarker is the leading HTML comment on an artifact comment.
// Encodes the id/type/version + metadata so the backend can locate it later.
const artifactCommentMarkerPrefix = "<!-- flow:artifact "

// errNoStateComment — the issue carries no state-v2 comment, so there is no
// document to mutate. A sentinel rather than a string so a caller that
// distinguishes "nothing recorded yet" from a transport failure matches on
// identity instead of on message text.
var errNoStateComment = errors.New("no state comment")

// AppendEntry appends ONE completed step execution to the item's journal and,
// in the same state-comment round, moves everything derived from it: the
// artifact projection, and — on the first entry — the flow binding and the
// labels that say this binary has begun.
//
// RESULT AND ROUTE LAND TOGETHER OR NOT AT ALL. The captured value is published
// first (an artifact comment, and the orphan branch for the large types), and
// then one document update records the entry and the projection: a reader can
// never see a captured artifact whose route was not recorded.
//
// It PUBLISHES, so the disclosure guard can refuse it — the caller stashes what
// was refused and parks, and nothing is journaled.
//
// The state comment is CREATED when the item has none. Nothing seeds an item any
// more, so the first entry is what brings its record into being.
//
// EVERY REFUSAL COMES BEFORE THE PUBLISH. An append that is going to be refused
// must not first post a comment for a step whose route is never recorded — the
// one thing the single write exists to prevent.
func (b *Orchestrator) AppendEntry(ctx context.Context, ref flow.ItemRef, entry flow.JournalEntry) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	account, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	if entry.Step == "" {
		return errors.New("github: journal entry names no step")
	}
	// Holding the lease is a PRECONDITION THE ORCHESTRATOR CHECKS, not a value
	// the caller supplies (docs/orchestrator.md § What an orchestrator may
	// refuse). It is the whole of what a claim protects on the write path: an
	// arena that lost the item to an `already-held` takeover would otherwise
	// keep journaling under revoked authority, and its entry would route the
	// item out from under the arena that now holds it.
	if err := b.requireOwnClaim(ctx, ref, "AppendEntry"); err != nil {
		return err
	}
	// This orchestrator STORES THE BYTES — a comment, or a file on the orphan
	// branch — so it has no somewhere-else in which to verify that the content
	// an empty body stands for exists. An empty body is therefore refused, named
	// (docs/orchestrator.md § What an orchestrator may refuse). Accepting one
	// would post an empty artifact comment and journal the step as completed,
	// which reads as a result nobody produced.
	//
	// A zero Result.Type is a signal step or a wait: its result IS the
	// observation, and there is no body to be empty.
	if entry.Result.Type != 0 && entry.Result.Empty() {
		return fmt.Errorf(
			"github: step %q completed with an empty %s body and this orchestrator stores the bytes itself — "+
				"there is no out-of-band content for it to stand for",
			entry.Step, entry.Result.Type)
	}
	// The body's shape against the schema this orchestrator declares. Startup
	// validation covers the flow's DECLARATION; this covers the value actually
	// recorded, which is the half a declaration cannot answer for.
	if err := checkDeclaredArtifactType(entry); err != nil {
		return err
	}

	// Publish the payload, if the entry carries one. Version is the entry's own
	// execution number: a step the route reaches again appends again, and the
	// later entry's result stands as the step's current one.
	artifactURL, spillURL, err := b.publishResult(ctx, issueNum, entry, account)
	if err != nil {
		return err
	}
	bodyAt := artifactURL
	if bodyAt == "" {
		bodyAt = spillURL
	}

	var (
		clearedLabel string
		firstEntry   bool
		prevAwaits   string
	)
	want := b.awaitsLabel(entry.Awaits)
	if err := b.mutateOrCreateStateDoc(ctx, ref, "AppendEntry", func(doc *stateDoc) error {
		// The first entry binds the flow. An item with an empty journal is bound
		// to nothing: the binding is a consequence of work having been recorded,
		// not of a binary having looked at the item.
		if len(doc.Journal) == 0 {
			doc.Flow = b.cfg.BinaryName
			firstEntry = true
		}
		// What the item advertises RIGHT NOW, read inside the write that is about
		// to supersede it: the mutation is replayed against whatever document is
		// actually there, so reading it here is what keeps the label being moved
		// off the value that document holds rather than one a lost read saw.
		prevAwaits = b.awaitsLabel(awaitsFromDoc(doc))
		// APPEND. An existing entry is never rewritten, so the pending step
		// derived from the last one cannot change under a reader.
		doc.Journal = append(doc.Journal, journalEntryDocOf(entry, bodyAt))
		// A park recorded against this step is obsolete once the step completes
		// — drop it rather than let Load keep reporting a reason that no longer
		// holds. The questions stay: they are never removed, and one asked and
		// never answered is part of the record whether or not the step went on
		// to complete.
		if doc.Park != nil && doc.Park.Step == string(entry.Step) {
			clearedLabel = parkLabel(b.labels, parkRequestFromDoc(doc.Park))
			doc.Park = nil
		}
		return nil
	}); err != nil {
		return err
	}
	// The step has a result now, so its scaffolding is done: THE DRAFT ENDS
	// WHERE THE RESULT BEGINS, which is why it is cleared inside this write and
	// nowhere else (docs/step-handler.md § Drafts). It matters beyond hygiene
	// under the journal: a route can return to a step it has already completed,
	// and a draft left from the earlier execution would reach that dispatch's
	// agent as its own prior thinking.
	//
	// AFTER the entry has landed, and best-effort. Clearing first would discard
	// what a refused append has to stash; failing here would report a failure
	// for work that is recorded, and send the caller back to append it twice.
	_ = b.ClearWorkInProgress(ctx, ref, entry.Step)
	b.removeParkLabel(ctx, ref, clearedLabel)
	// Every append passes through here — including a finalizing one, whose
	// Awaits is empty and which therefore removes the label by the same path
	// that moves it.
	b.moveAwaitsLabel(ctx, issueNum, prevAwaits, want)
	if firstEntry {
		// The marker that says this binary has begun on the item. It used to go
		// on at seed time; the first entry is where "begun" is now recorded, and
		// the binary label is what separates `auto` from `available`.
		//
		// Best-effort: the entry has already landed, and failing the append over
		// a label would report a failure for work that is recorded.
		_ = b.out.AddLabels(ctx, issueNum, []string{b.labels.Binary(b.cfg.BinaryName)})
	}
	return nil
}

// checkDeclaredArtifactType refuses a recorded body whose type is not the one
// this orchestrator's schema declares for that step's artifact id.
//
// A step id outside the schema is not checked: SupportedArtifacts is the set a
// FLOW's declaration is validated against at startup, and an id nothing
// declared has no declared type to disagree with.
func checkDeclaredArtifactType(entry flow.JournalEntry) error {
	if entry.Result.Type == 0 {
		return nil
	}
	for _, def := range githubSupportedArtifacts {
		if def.Id != flow.ArtifactId(entry.Step) {
			continue
		}
		if def.Type != entry.Result.Type {
			return flow.ErrTypeMismatch{
				Step:     string(entry.Step),
				Expected: def.Type,
				Got:      entry.Result.Type,
			}
		}
		return nil
	}
	return nil
}

// publishResult writes the entry's captured value where it belongs and returns
// the URLs. A signal step's entry carries no body, and nothing is published for
// it; a flag carries no payload, so it gets no comment either.
func (b *Orchestrator) publishResult(
	ctx context.Context,
	issueNum int,
	entry flow.JournalEntry,
	account flow.AccountId,
) (artifactURL, spillURL string, err error) {
	body := entry.Result
	if body.Type == 0 {
		return "", "", nil
	}
	id := flow.ArtifactId(entry.Step)
	version := entry.Execution
	now := entry.At
	if now.IsZero() {
		now = nowUTC()
	}

	// File/Patch always spill to the orphan branch. Markdown spills only when
	// the rendered comment would exceed cfg.MaxCommentBytes.
	switch body.Type {
	case flow.ArtifactFlag:
		// no payload, no spill
	case flow.ArtifactFile:
		filename := sanitizeFilename(body.File.Name)
		if filename == "" {
			filename = string(id)
		}
		path := artifactFilePath(issueNum, string(id), filename)
		url, err := b.putArtifactFile(ctx, path, body.File.Content,
			commitMessageForArtifact(issueNum, string(id), filename, spillFileType))
		if err != nil {
			return "", "", fmt.Errorf("spill file artifact %q: %w", id, err)
		}
		spillURL = url
	case flow.ArtifactPatch:
		path := artifactFilePath(issueNum, string(id), "patch.diff")
		url, err := b.putArtifactFile(ctx, path, body.Patch.Diff,
			commitMessageForArtifact(issueNum, string(id), "patch.diff", spillPatchType))
		if err != nil {
			return "", "", fmt.Errorf("spill patch artifact %q: %w", id, err)
		}
		spillURL = url
	case flow.ArtifactMarkdown:
		commentBody, err := renderArtifactComment(id, body, version, string(account), now, "")
		if err != nil {
			return "", "", err
		}
		if len(commentBody) > b.cfg.MaxCommentBytes {
			path := artifactFilePath(issueNum, string(id), "body.md")
			url, err := b.putArtifactFile(ctx, path, []byte(body.Markdown),
				commitMessageForArtifact(issueNum, string(id), "body.md", spillMarkdownTooLarge))
			if err != nil {
				return "", "", fmt.Errorf("spill markdown artifact %q: %w", id, err)
			}
			spillURL = url
		}
	}

	if body.Type == flow.ArtifactFlag {
		return "", spillURL, nil
	}

	commentBody, err := renderArtifactComment(id, body, version, string(account), now, spillURL)
	if err != nil {
		return "", "", err
	}
	// Truncate the markdown body for the inline preview when spilled.
	if spillURL != "" && body.Type == flow.ArtifactMarkdown {
		preview := body.Markdown
		if len(preview) > 4096 {
			preview = preview[:4096]
		}
		previewBody := flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: preview}
		commentBody, err = renderArtifactComment(id, previewBody, version, string(account), now, spillURL)
		if err != nil {
			return "", "", err
		}
	}
	// The assembled comment, not the artifact: the SDK's marker line and spill
	// notice around prose an agent wrote, which nobody vouches for as a whole.
	c, err := b.out.CreateComment(ctx, flow.ActArtifactComment, issueNum,
		flow.Text{Origin: flow.OriginAgent, Body: commentBody})
	if err != nil {
		return "", "", fmt.Errorf("post artifact comment: %w", err)
	}
	return c.GetHTMLURL(), spillURL, nil
}

// Reset clears the flow's whole record on the item so the next resolution starts
// from an empty journal: journal, ledger, park, the awaited marker, the
// finalization, and this issue's drafts. The artifact projection goes with the
// journal, being derived from it.
//
// Operator-initiated only; the SDK never calls it automatically.
//
// The QUESTIONS THEMSELVES do not go. They are never removed: one leaves the
// pending set by being answered, not by being deleted, and a question dropped
// while still unanswered is one `answer --question <id>` can never reach again.
// What goes is the park that was waiting on it, which is the outstanding marker.
func (b *Orchestrator) Reset(ctx context.Context, ref flow.ItemRef) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	body, stateID, _, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return err
	}
	// Drafts are worktree-local and go whether or not a state comment exists:
	// scratch prose kept past the record it belonged to has nothing to resume.
	//
	// Keyed through workItemKey, the same function the writes use: a second
	// spelling of the key would clear a directory nothing stores records in, and
	// nothing would report that it had.
	workItem, err := b.workItemKey(ref)
	if err != nil {
		return err
	}
	if derr := clistate.ClearItemWork(workItem); derr != nil {
		return fmt.Errorf("github.Reset: clear drafts for #%d: %w", issueNum, derr)
	}
	if body == "" || stateID == 0 {
		// Nothing recorded — the next AppendEntry writes a fresh state comment.
		return nil
	}
	doc, _, found, perr := extractStateDoc(body)
	if perr != nil {
		return fmt.Errorf("github: malformed state comment for issue %d: %w", issueNum, perr)
	}
	if !found || doc == nil {
		// The marker is absent — the comment exists but is not ours to reset.
		return nil
	}
	// The awaited marker is derived from the journal's last entry, and the
	// journal is about to go — so the label goes with it, read off the document
	// before it is cleared.
	clearedAwaits := b.awaitsLabel(awaitsFromDoc(doc))
	doc.Journal = nil
	doc.Ledger = stateLedgerDoc{}
	doc.Finalized = false
	doc.Disposition = ""
	clearedLabel := parkLabel(b.labels, parkRequestFromDoc(doc.Park))
	doc.Park = nil
	if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, owner); err != nil {
		return fmt.Errorf("reset state comment: %w", err)
	}
	b.removeParkLabel(ctx, ref, clearedLabel)
	b.moveAwaitsLabel(ctx, issueNum, clearedAwaits, "")
	return nil
}

// awaitsLabel is the label that advertises what an item awaits — a role, or
// `signal:<id>` for a wait that is nobody's move. Spelled through awaitsString,
// the SAME rendering the wire's `awaits` field is written with, so the cheap
// label index and the journal it indexes cannot disagree about the spelling.
//
// An item awaiting nothing has no label, which is why the empty Awaits maps to
// the empty string rather than to a bare `flow:awaits:`.
func (b *Orchestrator) awaitsLabel(a flow.Awaits) string {
	s := awaitsString(a)
	if s == "" {
		return ""
	}
	return b.labels.Awaits(s)
}

// moveAwaitsLabel moves the awaited marker from what the item advertised to what
// it now awaits: REMOVE THEN ADD, so an item never carries two at once, and
// nothing at all when the new value is empty — which is how a finalizing entry
// clears it.
//
// THE ONE WRITER of the marker, and every caller is an operation that CHANGES
// WHAT awaitsFromDoc ANSWERS: AppendEntry, which appends the entry the answer is
// read off; Reset, which clears the journal; and Finalize, which sets the flag
// that makes the answer nobody regardless of the journal. An operation that
// moved that answer without coming through here would leave the index pointing
// at a wait the record no longer describes.
//
// Best-effort, like the binary label beside it and for the same reason: the
// entry that elected this wait has already landed, and failing the append over a
// label would report a failure for work that is recorded, sending the caller
// back to append it twice. The journal remains the source of truth; this is the
// index over it.
func (b *Orchestrator) moveAwaitsLabel(ctx context.Context, issueNum int, prev, want string) {
	if prev == want {
		return
	}
	if prev != "" {
		_ = b.out.RemoveLabel(ctx, issueNum, prev)
	}
	if want != "" {
		_ = b.out.AddLabels(ctx, issueNum, []string{want})
	}
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------
//
// Every row is keyed by StepId — the result a step produces is that step's
// identity — which is what gives a signal step a row as well as an artifact one.

// RecordDispatch counts one dispatch of the pending step.
func (b *Orchestrator) RecordDispatch(ctx context.Context, ref flow.ItemRef, step flow.StepId) error {
	return b.mutateLedgerRow(ctx, ref, step, "RecordDispatch", func(row *stateLedgerRowDoc, l *stateLedgerDoc) {
		row.Dispatches++
		row.LastRunAt = nowUTC()
	})
}

// RecordResumption counts one resume of a parked step.
func (b *Orchestrator) RecordResumption(ctx context.Context, ref flow.ItemRef, step flow.StepId) error {
	return b.mutateLedgerRow(ctx, ref, step, "RecordResumption", func(row *stateLedgerRowDoc, l *stateLedgerDoc) {
		row.Resumptions++
	})
}

func (b *Orchestrator) AddCost(ctx context.Context, ref flow.ItemRef, step flow.StepId, usd float64) error {
	return b.mutateLedgerRow(ctx, ref, step, "AddCost", func(row *stateLedgerRowDoc, l *stateLedgerDoc) {
		row.CostUSDSpent += usd
		l.TotalCostUSD += usd
	})
}

// AddDuration adds ACTIVE time — time spent doing work. Time blocked on a
// declared exclusion goes to AddWaiting, never here: it is evidence about
// contention rather than about the work.
func (b *Orchestrator) AddDuration(ctx context.Context, ref flow.ItemRef, step flow.StepId, d time.Duration) error {
	return b.mutateLedgerRow(ctx, ref, step, "AddDuration", func(row *stateLedgerRowDoc, l *stateLedgerDoc) {
		row.DurationSeconds += d.Seconds()
		l.TotalDurationSeconds += d.Seconds()
	})
}

// AddWaiting adds time spent blocked on a declared exclusion, reported by the
// party that held the wait.
func (b *Orchestrator) AddWaiting(ctx context.Context, ref flow.ItemRef, step flow.StepId, d time.Duration) error {
	return b.mutateLedgerRow(ctx, ref, step, "AddWaiting", func(row *stateLedgerRowDoc, l *stateLedgerDoc) {
		row.WaitingSeconds += d.Seconds()
		l.TotalWaitingSeconds += d.Seconds()
	})
}

// Grant records an operator's extension against the step's ledger row and
// clears a treasurer-refused park the extension actually satisfies — the
// state-doc field AND the label, so neither outlives the condition (see the
// Orchestrator.Grant contract). The decision is flow.GrantClearsPark's, so both
// orchestrators apply one rule.
func (b *Orchestrator) Grant(ctx context.Context, ref flow.ItemRef, step flow.StepId, g flow.Grant) error {
	var clearedLabel string
	err := b.mutateStateDoc(ctx, ref, "Grant", func(doc *stateDoc) error {
		row := ledgerRowDoc(&doc.Ledger, string(step))
		now := nowUTC()
		for _, gr := range grantRecordDocs(g, now) {
			row.Granted = append(row.Granted, gr)
		}
		doc.Ledger.Steps[string(step)] = *row
		if flow.GrantClearsPark(parkRequestFromDoc(doc.Park), step, rowFromDoc(step, *row), g) {
			clearedLabel = parkLabel(b.labels, parkRequestFromDoc(doc.Park))
			doc.Park = nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	b.removeParkLabel(ctx, ref, clearedLabel)
	return nil
}

// grantRecordDocs splits one Grant into the per-axis records the ledger keeps.
// Zero on an axis means "no change", so nothing is recorded there: a row of
// zero-amount entries would be a history of grants that granted nothing.
func grantRecordDocs(g flow.Grant, at time.Time) []stateLedgerGrantDoc {
	var out []stateLedgerGrantDoc
	if g.Invocations != 0 {
		out = append(out, stateLedgerGrantDoc{Axis: string(flow.AxisInvocations), Amount: float64(g.Invocations), At: at})
	}
	if g.PromptsPerInvocation != 0 {
		out = append(out, stateLedgerGrantDoc{Axis: string(flow.AxisPrompts), Amount: float64(g.PromptsPerInvocation), At: at})
	}
	if g.CostUSD != 0 {
		out = append(out, stateLedgerGrantDoc{Axis: string(flow.AxisCost), Amount: g.CostUSD, At: at})
	}
	if g.TimeoutAdd != 0 {
		out = append(out, stateLedgerGrantDoc{Axis: string(flow.AxisTimeout), Amount: float64(g.TimeoutAdd), At: at})
	}
	return out
}

// rowFromDoc inflates one ledger row, for the rule that has to read it back
// inside the write that produced it.
func rowFromDoc(step flow.StepId, d stateLedgerRowDoc) flow.LedgerRow {
	l := ledgerFromDoc(stateLedgerDoc{Steps: map[string]stateLedgerRowDoc{string(step): d}})
	return l.Steps[step]
}

// mutateLedgerRow applies a mutation to one row and to the item-level totals in
// ONE document update, so a total can never disagree with the rows under it.
func (b *Orchestrator) mutateLedgerRow(
	ctx context.Context,
	ref flow.ItemRef,
	step flow.StepId,
	op string,
	mutate func(*stateLedgerRowDoc, *stateLedgerDoc),
) error {
	if step == "" {
		return fmt.Errorf("github.%s: ledger write names no step", op)
	}
	return b.mutateOrCreateStateDoc(ctx, ref, op, func(doc *stateDoc) error {
		row := ledgerRowDoc(&doc.Ledger, string(step))
		mutate(row, &doc.Ledger)
		doc.Ledger.Steps[string(step)] = *row
		return nil
	})
}

// ledgerRowDoc returns a mutable copy of the step's row, creating one on first
// use. A step that has never been dispatched has spent nothing, which is what
// the zero row says — so a missing row is not an error.
func ledgerRowDoc(l *stateLedgerDoc, step string) *stateLedgerRowDoc {
	if l.Steps == nil {
		l.Steps = map[string]stateLedgerRowDoc{}
	}
	row := l.Steps[step]
	return &row
}

// removeParkLabel drops a park label once the park it advertises is gone.
//
// Best-effort ON PURPOSE: the state doc is the source of truth and has already
// been written by the time this runs. Grant is NOT idempotent — it adds to the
// caps — so failing the call over a leftover label would invite a retry that
// grants the budget a second time. A stale label is cosmetic; a double grant
// is not.
func (b *Orchestrator) removeParkLabel(ctx context.Context, ref flow.ItemRef, label string) {
	if label == "" {
		return
	}
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return
	}
	_ = b.out.RemoveLabel(ctx, issueNum, label)
}

// PostAnswer records a person's answer AGAINST THE QUESTION IT ANSWERS.
//
// It posts the answer as an ordinary issue comment — carrying no flow marker,
// so ReadAnswers picks it up as a human reply — and then writes the text onto
// that Question in the state doc. Without the second half the answer landed
// nowhere: Question.Answer stayed empty, so the question kept coming back as
// pending and the CLI's --question <id> selected a target used for nothing but
// the output line.
//
// THE OUTSTANDING-QUESTION MARKER CLEARS ONLY WHEN NO PENDING QUESTION REMAINS.
// Answering one of three is not answering the item, and clearing on the first
// resumes a flow still waiting on two.
//
// THE PARK IS NOT THE MARKER AND DOES NOT CLEAR HERE. The marker records that a
// human must act; the park records which step stopped and what it resumes from,
// and is dropped by the three triggers docs/github-schema.md:113 names — the
// asking step completing (see AppendEntry), a park of another kind
// superseding it, or a reset. Answering is not one of them, and an answer that
// cleared the park would delete its own delivery: the park carries `asked-at`,
// the only window a resumed step has for finding replies (see
// flow.QuestionAskedAt), so the resume read an empty answers block, re-derived
// the same question, and parked again — once per resume, indefinitely.
//
// An unknown or already-answered id is REFUSED: silently accepting either would
// report an answer that moved nothing.
//
// No claim — the person answering does not hold the item.
func (b *Orchestrator) PostAnswer(ctx context.Context, ref flow.ItemRef, id flow.QuestionId, text string) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}

	// Validate before publishing: an answer comment posted against a question
	// that does not exist is a disclosure that cannot be taken back.
	item, err := b.Load(ctx, ref)
	if err != nil {
		return err
	}
	found := false
	for _, q := range item.Questions {
		if q.ID != id {
			continue
		}
		found = true
		if q.Answer != "" {
			return fmt.Errorf("github: question %q on issue #%d is already answered", id, issueNum)
		}
	}
	if !found {
		return fmt.Errorf("github: question %q not found on issue #%d", id, issueNum)
	}

	if _, err := b.out.CreateComment(ctx, flow.ActAnswer, issueNum,
		flow.Text{Origin: flow.OriginOperator, Body: text}); err != nil {
		return fmt.Errorf("post answer comment: %w", err)
	}

	pendingRemains := true
	if err := b.mutateStateDoc(ctx, ref, "PostAnswer", func(doc *stateDoc) error {
		now := nowUTC()
		matched := false
		for i := range doc.Questions {
			if flow.QuestionId(doc.Questions[i].ID) == id {
				doc.Questions[i].Answer = text
				doc.Questions[i].AnsweredAt = now
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("github: question %q not found in state doc on issue #%d", id, issueNum)
		}
		pendingRemains = anyPendingDoc(doc.Questions)
		return nil
	}); err != nil {
		return err
	}

	if !pendingRemains {
		// Best-effort: a stale label is cosmetic, and the state doc — already
		// written — is the source of truth.
		_ = b.out.RemoveLabel(ctx, issueNum, b.labels.NeedsAnswer())
	}
	return nil
}

// anyPendingDoc reports whether any recorded question is still unanswered.
func anyPendingDoc(qs []stateQuestionDoc) bool {
	for _, q := range qs {
		if q.Answer == "" {
			return true
		}
	}
	return false
}

// isNoStateComment reports whether err is the "this item has no state document"
// sentinel. A sentinel rather than a string so callers that tolerate it match
// on identity instead of on message text.
func isNoStateComment(err error) bool { return errors.Is(err, errNoStateComment) }

// requireOwnClaim checks that this arena holds the item's lease.
//
// The precondition is CHECKED, not supplied: the orchestrator minted the claim
// and the arena is ambient, so it has the lease in hand without a caller having
// to hand back a value that may since have gone stale.
func (b *Orchestrator) requireOwnClaim(ctx context.Context, ref flow.ItemRef, op string) error {
	active, err := b.LookupActiveClaim(ctx)
	if err != nil {
		return fmt.Errorf("github.%s: read active claim: %w", op, err)
	}
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	if active == nil {
		return fmt.Errorf("github.%s: issue #%d is not claimed by this arena: %w", op, issueNum, flow.ErrUnavailable)
	}
	activeNum, err := b.issueNumber(active.ItemRef)
	if err != nil {
		return err
	}
	if activeNum != issueNum {
		return fmt.Errorf("github.%s: this arena holds #%d, not #%d: %w", op, activeNum, issueNum, flow.ErrUnavailable)
	}
	return nil
}

// mutateOrCreateStateDoc is mutateStateDoc for the writes that must succeed on
// an item with no record yet.
//
// Nothing seeds an item any more, so whichever of these lands first is what
// brings its state comment into being: the first journal entry, the first
// ledger row, the first park, the first question, or the first observed signal.
// The last three are here because work can be attempted and record no entry — a
// refusal on the very first step counts no dispatch, and a question and a signal
// are both written mid-handler, before any dispatch is counted.
//
// The writes that can only FOLLOW a record still refuse an absent document
// (mutateStateDoc): an answer needs the question it answers, a park is cleared
// only where one was recorded, and a grant against an item nothing has
// dispatched has no row whose cap it could be raising.
func (b *Orchestrator) mutateOrCreateStateDoc(ctx context.Context, ref flow.ItemRef, op string, mutate func(*stateDoc) error) error {
	return b.withStateDoc(ctx, ref, op, true, mutate)
}

// mutateStateDoc loads the state comment, applies mutate to the whole
// document, and writes it back. Whole-document rather than per-artifact
// because a single grant can touch both an artifact's caps and the park
// field, and those must land in ONE comment update — two writes would leave a
// window where the budget is raised but the item still reads as parked.
func (b *Orchestrator) mutateStateDoc(ctx context.Context, ref flow.ItemRef, op string, mutate func(*stateDoc) error) error {
	return b.withStateDoc(ctx, ref, op, false, mutate)
}

// stateWriteLockTTL is how long the machine-wide per-item write lock is
// believed. Long enough to cover a read-modify-write against a slow API, short
// enough that a process killed mid-write cannot wedge an item's state for good.
const stateWriteLockTTL = 2 * time.Minute

// stateWriteAttempts is how many times a mutation is replayed against a
// document a foreign writer changed underneath it. Three, because a fourth
// collision is not contention any more — it is something writing continuously,
// and answering ErrUnavailable lets the caller park rather than spin.
const stateWriteAttempts = 3

// withStateDoc is the state document's read-modify-write, and the only one.
//
// mutateStateDoc and mutateOrCreateStateDoc were two copies of it differing
// only in whether an absent document is created, which `create` now says.
//
// #219: the document had no compare-and-set, so two concurrent writers silently
// lost each other — "a park recorded by one process and a spend charge recorded
// by another do not merge; the second PATCH wins entirely and the first is gone
// with no error and no trace". The same concurrency is what generated the burst
// of comment edits that trips GitHub's secondary limit, so the two symptoms have
// one mechanism and one fix:
//
//  1. A machine-wide write lock PER ITEM. On one host — the case actually
//     observed, several arenas on one machine — this closes the window outright
//     and stops the burst at its source. It is taken briefly and then proceeded
//     without: a write must not fail because a lock file could not be had.
//
//  2. A revalidation of the comment's own ETAG immediately before the PATCH.
//     GitHub offers no conditional write on a comment; this is the
//     compare-and-set the API admits, not a way around a missing one. Honest
//     about the residual: the window narrows from the whole read-modify-write to
//     the gap between the revalidate and the PATCH, and the lock above closes
//     that on one host.
//
//  3. On a foreign write, the mutation is REPLAYED against the document that is
//     actually there. Every mutator is an in-place delta — AppendEntry, Grant,
//     RecordDispatch, AddCost, AddDuration, PostAnswer, Park — so replay is
//     exactly what they mean, and both changes survive.
//
// The comparison is the comment's ETag, so docs/github-schema.md's wire format
// is untouched: no version field, no new marker, nothing on the surface.
func (b *Orchestrator) withStateDoc(ctx context.Context, ref flow.ItemRef, op string, create bool, mutate func(*stateDoc) error) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	defer b.holdStateWriteLock(issueNum)()

	for range stateWriteAttempts {
		body, stateID, _, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
		if err != nil {
			return err
		}

		var doc *stateDoc
		if body != "" {
			parsed, _, found, perr := extractStateDoc(body)
			if perr != nil {
				if !create {
					return perr
				}
				return fmt.Errorf("github.%s: parse state comment: %w", op, perr)
			}
			if found && parsed != nil {
				doc = parsed
			}
		}
		switch {
		case doc != nil:
		case !create:
			return fmt.Errorf("github: %s: %w", op, errNoStateComment)
		default:
			doc = &stateDoc{
				Flow:   b.cfg.BinaryName,
				Schema: stateSchemaVersion,
			}
		}

		if err := mutate(doc); err != nil {
			return err
		}

		// No comment yet: there is nothing to compare against and nothing a
		// foreign writer could have changed, because the document does not
		// exist. A racing creator loses to GitHub's own ordering, and the loser
		// rescans and finds the winner's comment on its next write.
		if stateID == 0 {
			id, _, err := b.postStateComment(ctx, issueNum, *doc, owner)
			if err != nil {
				return err
			}
			b.rememberStateCommentID(issueNum, id)
			return nil
		}

		// The read that produced this document may have come from a comment
		// SCAN, which carries no per-comment tag. Nothing to compare then —
		// which is the pre-existing behaviour, not a regression — and the next
		// write through this item has an id and therefore a tag.
		// The comparison is against the BYTES this iteration read, re-read
		// now, rather than against an ETag.
		//
		// An ETag is not a fact about the document, it is a fact about one
		// representation of it: GitHub hands a strong tag to one client and a
		// weak one to another for the same comment, and each validates only
		// against itself. A tag captured by one request and replayed on
		// another therefore reports "changed" for a document nobody touched —
		// deterministically, so the retry cannot converge and the caller is
		// told it lost a race that never happened (#366).
		//
		// Two reads of the same comment are directly comparable, whoever asks
		// and however the transport carries them, and the cost is identical:
		// the conditional GET this replaces was a GET too.
		current, _, cerr := b.out.GetComment(ctx, stateID)
		if cerr != nil {
			return fmt.Errorf("github.%s: re-read state comment %d: %w", op, stateID, cerr)
		}
		if current.GetBody() != body {
			// Somebody landed a write between the read and here. The
			// document just computed describes a state that no longer
			// exists, so it is DISCARDED rather than written — writing it
			// is exactly the lost update #219 is about.
			continue
		}

		if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, owner); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("github.%s: the state comment on #%d was rewritten by another writer %d times running: %w",
		op, issueNum, stateWriteAttempts, flow.ErrUnavailable)
}

// holdStateWriteLock takes the machine-wide write slot for one item's state
// document, and returns the release.
//
// It WAITS briefly and then proceeds without the lock rather than failing. The
// lock is what turns concurrent writers on one host into sequential ones — it
// removes the collision instead of detecting it — but the correctness property
// is the compare-and-set below, which holds with or without it. A write that
// failed because a lock file could not be created would be a cache outage
// presenting as a lost park.
func (b *Orchestrator) holdStateWriteLock(issueNum int) func() {
	path, ok := b.out.cache.stateLockPath(issueNum)
	if !ok {
		return func() {}
	}
	for attempt := range stateLockWaits {
		if release, held := machinecache.AcquireLock(path, stateWriteLockTTL, time.Now()); held {
			return release
		}
		if attempt < stateLockWaits-1 {
			time.Sleep(stateLockWait)
		}
	}
	return func() {}
}

// stateLockWaits and stateLockWait bound the wait for the slot: long enough to
// let a sibling's read-modify-write finish, short enough that a step is never
// held up noticeably by one.
const (
	stateLockWaits = 10
	stateLockWait  = 50 * time.Millisecond
)

// parkLabel returns the label that advertises a park of this kind. Adding and
// removing go through the same function so a park can never be labelled by one
// rule and unlabelled by another.
func parkLabel(l labels, req *flow.ParkRequest) string {
	if req == nil {
		return ""
	}
	switch req.Kind {
	case flow.ParkQuestion:
		return l.NeedsAnswer()
	case flow.ParkTreasurerRefused:
		return l.TreasurerRefused(string(req.Step))
	case flow.ParkInfraTransient:
		return l.InfraTransient()
	case flow.ParkRefused:
		// A deterministic refusal is blocked until the environment changes;
		// the generic "blocked" label is correct — no budget grant clears it.
		return l.Blocked()
	case flow.ParkWriteContract:
		// A write-contract violation needs human attention, not a budget
		// grant — the generic "blocked" label is correct.
		return l.Blocked()
	default:
		return l.Blocked()
	}
}

// Park records a park in the state comment's "park" field — the machine-
// readable copy LoadState returns — plus a flow:blocked / flow:needs-answer /
// flow:treasurer-refused:<step-id> label and a timeline comment, so a human
// scanning the issue list sees it too.
func (b *Orchestrator) Park(ctx context.Context, ref flow.ItemRef, req flow.ParkRequest) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	if err := b.out.AddLabels(ctx, issueNum, []string{parkLabel(b.labels, &req)}); err != nil {
		return fmt.Errorf("add park label: %w", err)
	}
	// Post a comment with the park reason so the timeline carries a record.
	parkBody, _ := json.Marshal(req)
	body := "<!-- flow:park -->\n```json\n" + string(parkBody) + "\n```"
	// The JSON frame is the SDK's; the reason and details inside it are a
	// handler's or an agent's, so the assembled record is stated `agent`.
	if _, err := b.out.CreateComment(ctx, flow.ActParkRecord, issueNum,
		flow.Text{Origin: flow.OriginAgent, Body: body}); err != nil {
		var refused flow.ErrDisclosureRefused
		if !errors.As(err, &refused) {
			return err
		}
		// The park is a fact; only its reason is unpublishable.
		// Substitute a disclosure-safe reason and retry.
		req.Reason = fmt.Sprintf("park reason withheld by disclosure guard (%s)", refused.Act)
		req.Details = ""
		parkBody, _ = json.Marshal(req)
		body = "<!-- flow:park -->\n```json\n" + string(parkBody) + "\n```"
		if _, err := b.out.CreateComment(ctx, flow.ActParkRecord, issueNum,
			flow.Text{Origin: flow.OriginFlow, Body: body}); err != nil {
			return err
		}
	}
	// Record it in the state doc, CREATING the document when the item has none.
	//
	// A park is precisely what happens when work was attempted and nothing was
	// recorded: a refusal or an infra failure on the very first step records no
	// dispatch and appends no entry, so an item can reach here with an entirely
	// empty document. This field is the machine-readable copy Load returns —
	// the label and timeline comment are for humans — and dropping it would
	// leave `status` reporting nothing and the next advance re-dispatching a
	// step a person has to unstick.
	if err := b.mutateOrCreateStateDoc(ctx, ref, "Park", func(doc *stateDoc) error {
		doc.Park = parkDocFromRequest(req, nowUTC())
		// The questions are untouched, whatever kind of park this is. They are
		// never removed: a park of another kind supersedes the one the item was
		// waiting under, but it does not answer the question that park was for,
		// and deleting the record is what makes an unanswered question
		// unreachable by the id `answer --question` needs.
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// AskQuestion records ONE agent-asked question on the item: posts it as a
// comment carrying the flow:question marker, appends it to the state doc, and
// adds the outstanding-question marker.
//
// EACH CALL ADDS ONE — THERE IS NO REPLACE. The state doc's question list used
// to be assigned from the recorded batch, which discarded every earlier
// unanswered question: a step that asked twice left one answerable ask and one
// that had silently ceased to exist. A step with several questions calls this
// several times, and each call says exactly which was recorded.
func (b *Orchestrator) AskQuestion(ctx context.Context, ref flow.ItemRef, q flow.AgentQuestion) (flow.Question, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return flow.Question{}, err
	}
	now := nowUTC()

	var sb strings.Builder
	sb.WriteString("<!-- flow:question ts=" + now.UTC().Format(time.RFC3339) + " -->\n")
	fmt.Fprintf(&sb, "### %s\n%s\n", q.Header, q.Text)
	if q.Format == flow.FormatChoice && len(q.Options) > 0 {
		sb.WriteString("\nOptions:\n")
		for _, opt := range q.Options {
			sb.WriteString("- " + opt + "\n")
		}
	}
	sb.WriteString("\n")

	// Keep the created comment: its server-side CreatedAt is the only clock
	// that can be compared against the answers' timestamps. See Question.AskedAt.
	created, err := b.out.CreateComment(ctx, flow.ActQuestion, issueNum,
		flow.Text{Origin: flow.OriginAgent, Body: sb.String()})
	if err != nil {
		return flow.Question{}, fmt.Errorf("post question comment: %w", err)
	}
	askedAt := created.GetCreatedAt().Time

	recorded := flow.Question{
		// Unique within its item, which is the scope every consumer needs:
		// `answer --question <id>` matches among one item's pending questions.
		ID:            flow.QuestionId(fmt.Sprintf("q-%d-%d", askedAt.UnixNano(), created.GetID())),
		AgentQuestion: q,
		AskedAt:       askedAt,
	}

	// Record it in the state doc — the machine-readable half, and the only one
	// Load can return. It must follow the post (whose CreatedAt is the AskedAt
	// this record carries) and precede the label, which advertises "a human must
	// answer this": that is only true once the record that lets `answer` name a
	// question exists.
	//
	// The document is CREATED when the item has none, and a failure here is
	// fatal. Nothing seeds an item any more and a dispatch is counted only
	// after the handler returns, so the first question a first step asks
	// arrives at an item with no record at all. Refusing it — or tolerating the
	// refusal — would produce exactly the state this record exists to prevent:
	// a question park nothing can clear, because `answer --question` has no id
	// to name.
	if err := b.mutateOrCreateStateDoc(ctx, ref, "AskQuestion", func(doc *stateDoc) error {
		doc.Questions = append(doc.Questions, questionDocOf(recorded))
		return nil
	}); err != nil {
		return flow.Question{}, fmt.Errorf("record question in state comment: %w", err)
	}
	if err := b.out.AddLabels(ctx, issueNum, []string{b.labels.NeedsAnswer()}); err != nil {
		return flow.Question{}, fmt.Errorf("add needs-answer label: %w", err)
	}
	return recorded, nil
}

// renderArtifactComment formats the per-artifact GitHub comment body. When
// `spillURL` is non-empty, the body links to the orphan-branch file instead
// of inlining the bytes (file/patch always; markdown when too large).
func renderArtifactComment(id flow.ArtifactId, body flow.ArtifactBody, version int, by string, ts time.Time, spillURL string) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%sid=%s type=%s v=%d by=%s ts=%s -->\n",
		artifactCommentMarkerPrefix, id, artifactTypeString(body.Type), version, by, ts.UTC().Format(time.RFC3339))
	switch body.Type {
	case flow.ArtifactMarkdown:
		sb.WriteString(body.Markdown)
		if spillURL != "" {
			fmt.Fprintf(&sb, "\n\n%s full body at %s]\n", spillNoticePrefix, spillURL)
		}
	case flow.ArtifactCommitHash:
		sb.WriteString("commit: `" + body.CommitHash + "`")
	case flow.ArtifactJSON:
		sb.WriteString("```json\n")
		sb.Write(body.JSON)
		sb.WriteString("\n```\n")
	case flow.ArtifactFile:
		fmt.Fprintf(&sb, "file: [`%s`](%s) (%d bytes)\n", body.File.Name, spillURL, len(body.File.Content))
	case flow.ArtifactPatch:
		fmt.Fprintf(&sb, "patch against `%s` — [download diff](%s) (%d bytes)\n",
			body.Patch.BaseSHA, spillURL, len(body.Patch.Diff))
		if body.Patch.BaseBranch != "" {
			fmt.Fprintf(&sb, "base branch: `%s`\n", body.Patch.BaseBranch)
		}
	case flow.ArtifactFlag:
		sb.WriteString("(flag set)")
	}
	return sb.String(), nil
}

func intToStr(n int) string { return fmt.Sprintf("%d", n) }

// ReadAnswers returns human replies posted on the issue after `since`.
//
// This is the read half of park-for-answer: AskQuestions posts the question as
// a comment and parks, and nothing resumes until somebody replies in the
// thread and an operator re-runs. There is no separate answer store — the issue
// thread IS the store, which is the point of asking where the humans already
// are.
//
// Exclusion is by MARKER, not by author, and `self` is deliberately unused
// here. Every comment this backend writes carries a machine marker (state,
// artifact, question), so markers separate the flow's writing from a human's
// exactly. Author does not: the common case is a human running the flow under
// their own token, so excluding that login would discard the answer they then
// wrote by hand and strand the step forever with nobody able to clear it.
//
// `self` stays in the interface for backends that cannot mark their own
// writes and have no finer instrument.
func (b *Orchestrator) ReadAnswers(ctx context.Context, item flow.Item, since time.Time, _ string) ([]flow.Answer, error) {
	issueNum, err := b.issueNumber(item.Ref)
	if err != nil {
		return nil, err
	}
	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	if !since.IsZero() {
		opts.Since = github.Ptr(since)
	}
	var out []flow.Answer
	for {
		comments, resp, err := b.out.ListCommentsPage(ctx, issueNum, opts)
		if err != nil {
			return nil, fmt.Errorf("list comments on #%d: %w", issueNum, err)
		}
		for _, c := range comments {
			author := c.GetUser().GetLogin()
			body := c.GetBody()
			if isFlowMachineComment(body) {
				continue
			}
			if strings.TrimSpace(body) == "" {
				continue
			}
			// `since` is the moment the question was asked; GitHub's Since
			// filter is inclusive to the second, so drop anything at or before
			// it rather than let the question's own second leak through.
			at := c.GetCreatedAt().Time
			if !since.IsZero() && !at.After(since) {
				continue
			}
			out = append(out, flow.Answer{
				Answer:     body,
				Author:     flow.AccountId(author),
				AnsweredAt: at,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// flowMarkerPrefix opens every HTML comment this backend writes for its own
// bookkeeping — state, artifact, question, park.
const flowMarkerPrefix = "<!-- flow:"

// isFlowMachineComment reports whether a comment is one the SDK wrote for its
// own bookkeeping rather than prose a human meant as an answer.
//
// It matches the shared prefix rather than enumerating known markers. An
// enumeration is a list that silently rots: the park marker was missing from
// one, and because Park posts its comment moments AFTER a question is stamped,
// every question park read as its own answer — the gate cleared itself on the
// next run, the step re-asked, and park-for-answer could never hold. Matching
// the prefix means a marker added later cannot reintroduce that.
func isFlowMachineComment(body string) bool {
	// Line-start, not anywhere in the body. GitHub's "Quote reply" prefixes
	// every quoted line with "> ", so a human answering that way carries a
	// copy of the question's own marker inside their reply — and a substring
	// match would discard exactly the answer the flow is waiting for, blocking
	// the item permanently with nothing to show why.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, flowMarkerPrefix) {
			return true
		}
	}
	return false
}

// artifactCommentRe matches the marker line an artifact comment opens with,
// capturing the id, the type, and the version.
var artifactCommentRe = regexp.MustCompile(
	`^<!-- flow:artifact id=(\S+) type=(\S+) v=(\d+) [^>]*-->\n?`)

// hydrateMarkdownBodies fills in the Markdown of every resolved markdown
// artifact by reading the comments that hold them.
//
// The state comment is an INDEX: it records that an artifact resolved, its
// version and its budget, but not its bytes, which live in a comment of their
// own. Without this pass every markdown artifact loads with an empty body while
// still reporting Resolved — so a step reading an upstream artifact gets
// ("", true) and silently proceeds on nothing. An implement step reads no plan
// and writes code against a blank one, with no error anywhere to say so.
//
// One extra listing per load, and only when something is actually there to
// hydrate. Later versions win: comments arrive in chronological order and a
// re-resolved artifact posts a fresh one.
func (b *Orchestrator) hydrateMarkdownBodies(ctx context.Context, issueNum int, state *flow.Item) error {
	want := map[flow.ArtifactId]bool{}
	for id, rec := range state.Artifacts {
		if rec.Resolved && rec.Type == flow.ArtifactMarkdown && rec.Markdown == "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	seen := map[flow.ArtifactId]int{}
	for {
		comments, resp, err := b.out.ListCommentsPage(ctx, issueNum, opts)
		if err != nil {
			// Fatal, deliberately. Degrading to empty bodies would make an API
			// outage indistinguishable from an artifact that genuinely has no
			// content: the caller reports the wrong cause, and — because the
			// step still dispatched — burns an invocation doing it. Three
			// blips would exhaust the default budget and park the item on a
			// budget message for a network problem.
			return fmt.Errorf("list comments on #%d: %w", issueNum, err)
		}
		for _, c := range comments {
			body := c.GetBody()
			m := artifactCommentRe.FindStringSubmatch(body)
			if m == nil {
				continue
			}
			id := flow.ArtifactId(m[1])
			if !want[id] || m[2] != artifactTypeString(flow.ArtifactMarkdown) {
				continue
			}
			version, _ := strconv.Atoi(m[3])
			if version < seen[id] {
				continue
			}
			seen[id] = version
			text := strings.TrimPrefix(body, m[0])
			// A markdown artifact larger than MaxCommentBytes keeps only a
			// PREVIEW in its comment and spills the rest to the orphan branch.
			// Loading the preview as though it were the body hands a caller a
			// plan cut off mid-sentence that still reads as complete, so the
			// full text is fetched instead.
			if hasSpillNotice(text) {
				full, ferr := b.readArtifactFile(ctx, artifactFilePath(issueNum, string(id), "body.md"))
				if ferr != nil {
					// Fatal, for the same reason the listing failure above is:
					// leaving the body empty makes a transient blip on the
					// artifacts branch indistinguishable from an artifact that
					// genuinely has no content, and the caller then reports the
					// wrong cause after spending an invocation on it.
					return fmt.Errorf("artifact %q spilled to the %s branch and could not be read: %w",
						id, artifactsBranch, ferr)
				}
				text = full
			}
			rec := state.Artifacts[id]
			rec.Markdown = text
			state.Artifacts[id] = rec
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil
}

// spillNoticePrefix opens the notice appended to a markdown artifact comment
// whose body was too large to inline. Readers key off it to know the comment
// holds a preview rather than the artifact.
const spillNoticePrefix = "[truncated preview;"

// hasSpillNotice reports whether a comment body ends in the notice that its
// markdown was too large to inline.
//
// Line-anchored, not a substring scan: an artifact whose own text quotes the
// notice — a review discussing this very code, say — would otherwise be taken
// for a preview, sending the loader after a spill file that does not exist.
func hasSpillNotice(text string) bool {
	// The notice is a TRAILER — appended after the preview — so only the last
	// non-empty line can be one. Scanning every line would take an artifact
	// that merely quotes the notice (a review discussing this code, say) for a
	// preview, sending the loader after a spill file that does not exist and
	// leaving the body empty.
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " \t\r")
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, spillNoticePrefix)
	}
	return false
}

// readArtifactFile reads a file back from the flow-artifacts orphan branch.
func (b *Orchestrator) readArtifactFile(ctx context.Context, path string) (string, error) {
	// DownloadContents, not GetContents: the latter inlines the bytes and caps
	// at 1MB, which a spilled artifact can exceed — spilling is what happens to
	// the large ones. This follows the download URL instead.
	rc, err := b.out.DownloadContents(ctx, path,
		&github.RepositoryContentGetOptions{Ref: artifactsBranch})
	if err != nil {
		return "", fmt.Errorf("download %s@%s: %w", path, artifactsBranch, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read %s@%s: %w", path, artifactsBranch, err)
	}
	return string(body), nil
}
