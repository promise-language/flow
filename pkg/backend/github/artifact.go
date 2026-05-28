package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// artifactCommentMarker is the leading HTML comment on an artifact comment.
// Encodes the id/type/version + metadata so the backend can locate it later.
const artifactCommentMarkerPrefix = "<!-- flow:artifact "

// SeedState writes the initial state comment with the artifact checklist +
// budget caps. Refuses if the issue already has a state comment.
func (b *Backend) SeedState(ctx context.Context, claim flow.Claim, specs []flow.ArtifactSpec) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	tok, err := b.loadClaimToken(claim)
	if err != nil {
		return err
	}

	// If we already have a state-comment id and the document has artifacts,
	// refuse a re-seed.
	body, stateID, err := b.fetchStateComment(ctx, issueNum, tok.StateCommentID)
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
		if _, err := b.updateStateComment(ctx, issueNum, stateID, doc, claim.Owner); err != nil {
			return err
		}
	} else {
		id, _, err := b.postStateComment(ctx, issueNum, doc, claim.Owner)
		if err != nil {
			return err
		}
		stateID = id
	}

	// Idempotent label adds.
	if _, _, err := b.gh.Issues.AddLabelsToIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{
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
func (b *Backend) ResetSeed(ctx context.Context, claim flow.Claim) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	tok, err := b.loadClaimToken(claim)
	if err != nil {
		return err
	}
	body, stateID, err := b.fetchStateComment(ctx, issueNum, tok.StateCommentID)
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
	if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, claim.Owner); err != nil {
		return fmt.Errorf("reset state comment: %w", err)
	}
	return nil
}

// ResolveArtifact persists a handler-produced value to the issue. The
// canonical storage is a new "artifact comment" (per-version, append-only);
// the state comment's resolved_by / produced_at / version pointers move to
// the new comment.
func (b *Backend) ResolveArtifact(ctx context.Context, claim flow.Claim, id flow.ArtifactId, body flow.ArtifactBody) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	tok, err := b.loadClaimToken(claim)
	if err != nil {
		return err
	}

	// Pull the current state doc so we know the next version.
	stateBody, stateID, err := b.fetchStateComment(ctx, issueNum, tok.StateCommentID)
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
		commentBody, err := renderArtifactComment(id, body, version, claim.Owner, now, "")
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
		commentBody, err := renderArtifactComment(id, body, version, claim.Owner, now, spillURL)
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
			commentBody, err = renderArtifactComment(id, previewBody, version, claim.Owner, now, spillURL)
			if err != nil {
				return err
			}
		}
		c, _, err := b.gh.Issues.CreateComment(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, &github.IssueComment{Body: &commentBody})
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
	a.ResolvedByPrincipal = claim.Owner
	if body.Type == flow.ArtifactCommitHash {
		a.CommitHash = body.CommitHash
	}
	if body.Type == flow.ArtifactJSON {
		a.JSONInline = string(body.JSON)
	}

	_ = owner // we patch as the current user (state comment author); owner stays as-is

	if _, err := b.updateStateComment(ctx, issueNum, stateID, *doc, claim.Owner); err != nil {
		return err
	}
	return nil
}

// MarkStale flips the stale bit on the artifact's state entry.
func (b *Backend) MarkStale(ctx context.Context, claim flow.Claim, id flow.ArtifactId) error {
	return b.mutateArtifact(ctx, claim, string(id), func(a *stateArtifactDoc) {
		a.Stale = true
	})
}

// BumpInvocations increments Invocations + resets PromptsThisInvocation.
func (b *Backend) BumpInvocations(ctx context.Context, claim flow.Claim, key string) error {
	return b.mutateArtifact(ctx, claim, key, func(a *stateArtifactDoc) {
		a.Invocations++
		a.PromptsThisInvocation = 0
		a.LastRunAt = nowUTC()
	})
}

func (b *Backend) BumpPrompts(ctx context.Context, claim flow.Claim, key string) error {
	return b.mutateArtifact(ctx, claim, key, func(a *stateArtifactDoc) {
		a.PromptsThisInvocation++
	})
}

func (b *Backend) AddCost(ctx context.Context, claim flow.Claim, key string, usd float64) error {
	return b.mutateArtifact(ctx, claim, key, func(a *stateArtifactDoc) {
		a.CostUSDSpent += usd
	})
}

func (b *Backend) Grant(ctx context.Context, claim flow.Claim, key string, g flow.Grant) error {
	return b.mutateArtifact(ctx, claim, key, func(a *stateArtifactDoc) {
		a.GrantedInvocations += g.Invocations
		a.GrantedPromptsPerInvocation += g.PromptsPerInvocation
		a.GrantedCostUSD += g.CostUSD
		a.GrantedTimeout += time.Duration(g.TimeoutAdd) * time.Second
	})
}

// mutateArtifact loads the state comment, applies the mutation, and writes
// the comment back. Centralizes the load-modify-store cycle used by the
// per-axis bumpers and Grant.
func (b *Backend) mutateArtifact(ctx context.Context, claim flow.Claim, key string, mutate func(*stateArtifactDoc)) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	tok, err := b.loadClaimToken(claim)
	if err != nil {
		return err
	}
	body, stateID, err := b.fetchStateComment(ctx, issueNum, tok.StateCommentID)
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("github: mutateArtifact: no state comment")
	}
	doc, _, _, err := extractStateDoc(body)
	if err != nil {
		return err
	}
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Id == key {
			mutate(&doc.Artifacts[i])
			_, err := b.updateStateComment(ctx, issueNum, stateID, *doc, claim.Owner)
			return err
		}
	}
	return fmt.Errorf("github: artifact %q not found in state doc", key)
}

// Park records a park via the state comment's "park" field (not yet in the
// schema) plus a flow:blocked / flow:needs-answer / flow:budget-exhausted:<id>
// label so consumers can spot parked issues at a glance.
func (b *Backend) Park(ctx context.Context, claim flow.Claim, req flow.ParkRequest) error {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return err
	}
	var labelToAdd string
	switch req.Kind {
	case flow.ParkBlocked:
		labelToAdd = b.labels.Blocked()
	case flow.ParkQuestion:
		labelToAdd = b.labels.NeedsAnswer()
	case flow.ParkBudgetExhausted:
		labelToAdd = b.labels.BudgetExhausted(req.Step)
	case flow.ParkInfraTransient:
		labelToAdd = b.labels.InfraTransient()
	default:
		labelToAdd = b.labels.Blocked()
	}
	if _, _, err := b.gh.Issues.AddLabelsToIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{labelToAdd}); err != nil {
		return fmt.Errorf("add park label: %w", err)
	}
	// Post a comment with the park reason so the timeline carries a record.
	parkBody, _ := json.Marshal(req)
	body := "<!-- flow:park -->\n```json\n" + string(parkBody) + "\n```"
	_, _, err = b.gh.Issues.CreateComment(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, &github.IssueComment{Body: &body})
	return err
}

// AskQuestions persists the agent's questions as a single comment with the
// flow:question marker, returns the questions populated with ids.
func (b *Backend) AskQuestions(ctx context.Context, claim flow.Claim, qs []flow.AgentQuestion) ([]flow.Question, error) {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	out := make([]flow.Question, 0, len(qs))
	var sb strings.Builder
	sb.WriteString("<!-- flow:question ts=" + now.UTC().Format(time.RFC3339) + " -->\n")
	for i, q := range qs {
		id := fmt.Sprintf("q-%d-%d", now.UnixNano(), i)
		sb.WriteString(fmt.Sprintf("### %s\n%s\n", q.Header, q.Text))
		if q.Format == flow.FormatChoice && len(q.Options) > 0 {
			sb.WriteString("\nOptions:\n")
			for _, opt := range q.Options {
				sb.WriteString("- " + opt + "\n")
			}
		}
		sb.WriteString("\n")
		out = append(out, flow.Question{ID: id, AgentQuestion: q})
	}
	body := sb.String()
	if _, _, err := b.gh.Issues.CreateComment(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, &github.IssueComment{Body: &body}); err != nil {
		return nil, fmt.Errorf("post questions comment: %w", err)
	}
	if _, _, err := b.gh.Issues.AddLabelsToIssue(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, []string{b.labels.NeedsAnswer()}); err != nil {
		return nil, fmt.Errorf("add needs-answer label: %w", err)
	}
	return out, nil
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
			fmt.Fprintf(&sb, "\n\n[truncated preview; full body at %s]\n", spillURL)
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
