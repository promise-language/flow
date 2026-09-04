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
)

// artifactCommentMarker is the leading HTML comment on an artifact comment.
// Encodes the id/type/version + metadata so the backend can locate it later.
const artifactCommentMarkerPrefix = "<!-- flow:artifact "

// errNoStateComment — the issue carries no state-v1 comment, so there is no
// document to mutate. A sentinel rather than a string so callers that tolerate
// it (Park, which can fire before an item is seeded) match on identity instead
// of on message text.
var errNoStateComment = errors.New("no state comment")

// SeedState writes the initial state comment with the artifact checklist +
// budget caps. Refuses if the issue already has a state comment.
func (b *Orchestrator) SeedState(ctx context.Context, ref flow.ItemRef, specs []flow.ArtifactSpec) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	// Seeding requires the lease. The item is the subject, so the item is the
	// address; holding the lease is a precondition checked here, not a value
	// the caller supplies. The orchestrator minted the claim and the arena is
	// ambient, so it has the lease in hand without re-proving anything.
	if err := b.requireOwnClaim(ctx, ref, "SeedState"); err != nil {
		return err
	}

	// If we already have a state-comment id and the document has artifacts,
	// refuse a re-seed.
	body, stateID, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return err
	}
	if body != "" {
		doc, _, _, perr := extractStateDoc(body)
		if perr == nil && doc != nil && len(doc.Artifacts) > 0 {
			return errors.New("github: item already seeded; SeedState refused")
		}
	}

	doc := stateDoc{
		Flow:     b.cfg.BinaryName,
		Schema:   stateSchemaVersion,
		SeededAt: nowUTC(),
	}
	for _, sp := range specs {
		doc.Artifacts = append(doc.Artifacts, stateArtifactDoc{
			Id:                          string(sp.Id),
			Type:                        artifactTypeString(sp.Type),
			Required:                    sp.Required,
			GrantedInvocations:          sp.Budget.MaxInvocations,
			GrantedPromptsPerInvocation: sp.Budget.MaxPromptsPerInvocation,
			GrantedCostUSD:              sp.Budget.MaxCostUSD,
			GrantedTimeout:              sp.Budget.Timeout,
		})
	}

	if stateID != 0 {
		if _, err := b.updateStateComment(ctx, issueNum, stateID, doc, owner); err != nil {
			return err
		}
	} else {
		id, _, err := b.postStateComment(ctx, issueNum, doc, owner)
		if err != nil {
			return err
		}
		stateID = id
	}

	// Idempotent label adds.
	if err := b.out.AddLabels(ctx, issueNum, []string{
		b.labels.Seeded(),
		b.labels.Binary(b.cfg.BinaryName),
	}); err != nil {
		return fmt.Errorf("add seeded labels: %w", err)
	}

	// Update the claim token cache (caller persists Claim across calls but
	// the Token JSON is what's serialized — we can't mutate it here).
	b.mu.Lock()
	b.stateCommentCache[issueNum] = stateID
	b.mu.Unlock()
	return nil
}

// ResetSeed clears the state comment's artifact set so the next SeedState
// pass writes a fresh artifact checklist. Operator-initiated (e.g. a UI
// "re-seed with current flow" action); the SDK never calls this
// automatically. The state comment's metadata is preserved; only the
// artifact list is cleared, since artifact comments live in their own
// per-version comments and re-deriving from those would be lossy on
// schema change.
func (b *Orchestrator) ResetSeed(ctx context.Context, ref flow.ItemRef) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	body, stateID, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return err
	}
	if body == "" || stateID == 0 {
		// Nothing to reset — never seeded. The next SeedState will write
		// a fresh state comment.
		return nil
	}
	doc, _, found, perr := extractStateDoc(body)
	if perr != nil {
		return fmt.Errorf("github: malformed state comment for issue %d: %w", issueNum, perr)
	}
	if !found || doc == nil {
		// State-v1 marker absent — comment exists but isn't ours to reset.
		// Same outcome as 'never seeded': next SeedState writes fresh.
		return nil
	}
	doc.Artifacts = nil
	doc.SeededAt = nowUTC()
	// The park belongs to the cleared checklist — a park naming a step that no
	// longer has a budget record would survive the reset and misreport the item
	// as blocked forever. (fake.ResetSeed clears its park for the same reason.)
	//
	// The questions do NOT go with it. They are never removed: one leaves the
	// pending set by being answered, not by being deleted, and a question
	// dropped while still unanswered is one `answer --question <id>` can never
	// reach again.
	clearedLabel := parkLabel(b.labels, parkRequestFromDoc(doc.Park))
	doc.Park = nil
	if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, owner); err != nil {
		return fmt.Errorf("reset state comment: %w", err)
	}
	b.removeParkLabel(ctx, ref, clearedLabel)
	return nil
}

// ResolveArtifact persists a handler-produced value to the issue. The
// canonical storage is a new "artifact comment" (per-version, append-only);
// the state comment's resolved_by / produced_at / version pointers move to
// the new comment.
func (b *Orchestrator) ResolveArtifact(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, body flow.ArtifactBody) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	account, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}

	// Pull the current state doc so we know the next version.
	stateBody, stateID, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return err
	}
	if stateBody == "" {
		return errors.New("github: ResolveArtifact called before SeedState")
	}
	doc, owner, _, err := extractStateDoc(stateBody)
	if err != nil {
		return err
	}

	// Find the artifact's spec entry; record the type for validation.
	idx := -1
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Id == string(id) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("github: artifact %q not seeded on issue #%d", id, issueNum)
	}
	declaredType := artifactTypeFromString(doc.Artifacts[idx].Type)
	if body.Type != declaredType {
		return flow.ErrTypeMismatch{Step: string(id), Expected: declaredType, Got: body.Type}
	}

	version := doc.Artifacts[idx].Version + 1
	now := nowUTC()

	// Post the artifact comment (or skip for Flag — no payload).
	// File/Patch always spill to the orphan branch. Markdown spills only
	// when the rendered comment would exceed cfg.MaxCommentBytes.
	var (
		artifactURL string
		spillURL    string
	)
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
			return fmt.Errorf("spill file artifact %q: %w", id, err)
		}
		spillURL = url
	case flow.ArtifactPatch:
		path := artifactFilePath(issueNum, string(id), "patch.diff")
		url, err := b.putArtifactFile(ctx, path, body.Patch.Diff,
			commitMessageForArtifact(issueNum, string(id), "patch.diff", spillPatchType))
		if err != nil {
			return fmt.Errorf("spill patch artifact %q: %w", id, err)
		}
		spillURL = url
	case flow.ArtifactMarkdown:
		commentBody, err := renderArtifactComment(id, body, version, string(account), now, "")
		if err != nil {
			return err
		}
		if len(commentBody) > b.cfg.MaxCommentBytes {
			path := artifactFilePath(issueNum, string(id), "body.md")
			url, err := b.putArtifactFile(ctx, path, []byte(body.Markdown),
				commitMessageForArtifact(issueNum, string(id), "body.md", spillMarkdownTooLarge))
			if err != nil {
				return fmt.Errorf("spill markdown artifact %q: %w", id, err)
			}
			spillURL = url
		}
	}

	if body.Type != flow.ArtifactFlag {
		commentBody, err := renderArtifactComment(id, body, version, string(account), now, spillURL)
		if err != nil {
			return err
		}
		// Truncate markdown body for the inline preview when spilled.
		if spillURL != "" && body.Type == flow.ArtifactMarkdown {
			preview := body.Markdown
			if len(preview) > 4096 {
				preview = preview[:4096]
			}
			previewBody := flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: preview}
			commentBody, err = renderArtifactComment(id, previewBody, version, string(account), now, spillURL)
			if err != nil {
				return err
			}
		}
		// The assembled comment, not the artifact: the SDK's marker line and
		// spill notice around prose an agent wrote, which nobody vouches for
		// as a whole.
		c, err := b.out.CreateComment(ctx, flow.ActArtifactComment, issueNum,
			flow.Text{Origin: flow.OriginAgent, Body: commentBody})
		if err != nil {
			return fmt.Errorf("post artifact comment: %w", err)
		}
		artifactURL = c.GetHTMLURL()
	}

	// Update state doc in place.
	a := &doc.Artifacts[idx]
	a.Resolved = true
	a.Stale = false
	a.Version = version
	a.ProducedAt = now
	a.ResolvedBy = artifactURL
	a.ResolvedByPrincipal = string(account)
	if body.Type == flow.ArtifactCommitHash {
		a.CommitHash = body.CommitHash
	}
	if body.Type == flow.ArtifactJSON {
		a.JSONInline = string(body.JSON)
	}

	// A park recorded against this step is obsolete once the step resolves —
	// drop it (and its label) rather than let Load keep reporting a reason that
	// no longer holds. The questions stay: they are never removed, and one that
	// was asked and never answered is part of the record of what happened here
	// whether or not the step it belonged to went on to resolve.
	clearedLabel := ""
	if doc.Park != nil && doc.Park.Step == string(id) {
		clearedLabel = parkLabel(b.labels, parkRequestFromDoc(doc.Park))
		doc.Park = nil
	}

	_ = owner // we patch as the current user (state comment author); owner stays as-is

	if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, account); err != nil {
		return err
	}
	b.removeParkLabel(ctx, ref, clearedLabel)
	return nil
}

// MarkStale flips the stale bit on the artifact's state entry.
func (b *Orchestrator) MarkStale(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId) error {
	return b.mutateArtifact(ctx, ref, id, func(a *stateArtifactDoc) {
		a.Stale = true
	})
}

// BumpInvocations increments Invocations + resets PromptsThisInvocation.
func (b *Orchestrator) BumpInvocations(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId) error {
	return b.mutateArtifact(ctx, ref, id, func(a *stateArtifactDoc) {
		a.Invocations++
		a.PromptsThisInvocation = 0
		a.LastRunAt = nowUTC()
	})
}

func (b *Orchestrator) BumpPrompts(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId) error {
	return b.mutateArtifact(ctx, ref, id, func(a *stateArtifactDoc) {
		a.PromptsThisInvocation++
	})
}

func (b *Orchestrator) AddCost(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, usd float64) error {
	return b.mutateArtifact(ctx, ref, id, func(a *stateArtifactDoc) {
		a.CostUSDSpent += usd
	})
}

func (b *Orchestrator) AddDuration(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, d time.Duration) error {
	return b.mutateArtifact(ctx, ref, id, func(a *stateArtifactDoc) {
		a.DurationWorked += d
	})
}

// Grant adds budget to the artifact's caps and clears a ParkBudgetExhausted
// park the grant actually satisfies — the state-doc field AND the
// flow:budget-exhausted:<step-id> label, so neither outlives the condition
// (see the Orchestrator.Grant contract).
func (b *Orchestrator) Grant(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, g flow.Grant) error {
	var clearedLabel string
	err := b.mutateStateDoc(ctx, ref, "Grant", func(doc *stateDoc) error {
		a := findArtifactDoc(doc, string(id))
		if a == nil {
			return fmt.Errorf("github: artifact %q not found in state doc", id)
		}
		a.GrantedInvocations += g.Invocations
		a.GrantedPromptsPerInvocation += g.PromptsPerInvocation
		a.GrantedCostUSD += g.CostUSD
		a.GrantedTimeout += time.Duration(g.TimeoutAdd) * time.Second
		if flow.GrantClearsPark(parkRequestFromDoc(doc.Park), id, recordFromArtifactDoc(*a), g) {
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
// resumes a flow still waiting on two. The park clears with it, and only then.
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
		// The park is what the questions were waiting on, so it clears with the
		// last of them and not before.
		if !pendingRemains && doc.Park != nil && flow.ParkKind(doc.Park.Kind) == flow.ParkQuestion {
			doc.Park = nil
		}
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

// mutateArtifact applies a mutation to one artifact entry of the state doc.
// Centralizes the load-modify-store cycle used by the per-axis bumpers.
func (b *Orchestrator) mutateArtifact(ctx context.Context, ref flow.ItemRef, id flow.ArtifactId, mutate func(*stateArtifactDoc)) error {
	return b.mutateStateDoc(ctx, ref, "mutateArtifact", func(doc *stateDoc) error {
		a := findArtifactDoc(doc, string(id))
		if a == nil {
			return fmt.Errorf("github: artifact %q not found in state doc", id)
		}
		mutate(a)
		return nil
	})
}

// mutateStateDoc loads the state comment, applies mutate to the whole
// document, and writes it back. Whole-document rather than per-artifact
// because a single grant can touch both an artifact's caps and the park
// field, and those must land in ONE comment update — two writes would leave a
// window where the budget is raised but the item still reads as parked.
func (b *Orchestrator) mutateStateDoc(ctx context.Context, ref flow.ItemRef, op string, mutate func(*stateDoc) error) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	owner, err := b.resolveAccount(ctx)
	if err != nil {
		return err
	}
	body, stateID, err := b.fetchStateComment(ctx, issueNum, b.cachedStateCommentID(issueNum))
	if err != nil {
		return err
	}
	if body == "" {
		return fmt.Errorf("github: %s: %w", op, errNoStateComment)
	}
	doc, _, _, err := extractStateDoc(body)
	if err != nil {
		return err
	}
	if err := mutate(doc); err != nil {
		return err
	}
	_, err = b.updateStateComment(ctx, issueNum, stateID, *doc, owner)
	return err
}

// findArtifactDoc returns a pointer to the doc's entry for key, or nil.
func findArtifactDoc(doc *stateDoc, key string) *stateArtifactDoc {
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Id == key {
			return &doc.Artifacts[i]
		}
	}
	return nil
}

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
	case flow.ParkBudgetExhausted:
		return l.BudgetExhausted(string(req.Step))
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
// flow:budget-exhausted:<step-id> label and a timeline comment, so a human
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
	// Record it in the state doc. An item can be parked before it is seeded
	// (a preflight refusal, say), and there is no state comment to write to
	// then — the label and timeline comment above still carry the record, so
	// that is not an error.
	if err := b.mutateStateDoc(ctx, ref, "Park", func(doc *stateDoc) error {
		doc.Park = parkDocFromRequest(req, nowUTC())
		// The questions are untouched, whatever kind of park this is. They are
		// never removed: a park of another kind supersedes the one the item was
		// waiting under, but it does not answer the question that park was for,
		// and deleting the record is what makes an unanswered question
		// unreachable by the id `answer --question` needs.
		return nil
	}); err != nil && !errors.Is(err, errNoStateComment) {
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
	// A failure here is fatal, deliberately unlike Park's errNoStateComment
	// tolerance. An item can be parked before it is seeded; a question cannot be
	// asked before it is, because the handler that asks runs after the
	// mandatory-seed gate. Tolerating it would recreate exactly the state this
	// record exists to prevent — a question park nothing can clear.
	if err := b.mutateStateDoc(ctx, ref, "AskQuestion", func(doc *stateDoc) error {
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
