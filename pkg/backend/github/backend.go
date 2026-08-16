package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// Backend is the GitHub-Issues-backed flow.Backend.
type Backend struct {
	cfg    Config
	gh     *github.Client
	git    *gitOps
	labels labels

	mu sync.Mutex // protects per-claim caches keyed by issue number
	// cache of the most recently observed state-comment id per issue, so
	// LoadState can PATCH in place without re-discovering on every call.
	stateCommentCache map[int]int64
}

// NewBackend constructs a github Backend. If cfg.Owner/Repo are empty, they
// are resolved from `git remote get-url origin` in cfg.WorktreeDir (or the
// current directory if empty). If cfg.Token is empty, `gh auth token` is
// tried, then GITHUB_TOKEN.
func NewBackend(cfg Config) (*Backend, error) {
	cfg = cfg.withDefaults()
	if cfg.BinaryName == "" {
		cfg.BinaryName = deriveBinaryNameFromArgv()
	}

	git := newGitOps(cfg.WorktreeDir)
	if cfg.Owner == "" || cfg.Repo == "" {
		o, r, err := git.OriginOwnerRepo(context.Background())
		if err != nil {
			return nil, fmt.Errorf("github backend: resolve repo: %w", err)
		}
		if cfg.Owner == "" {
			cfg.Owner = o
		}
		if cfg.Repo == "" {
			cfg.Repo = r
		}
	}

	token, err := resolveToken(context.Background(), cfg.Token)
	if err != nil {
		return nil, err
	}
	cfg.Token = token

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	client := github.NewClient(nil).WithAuthToken(token)
	return &Backend{
		cfg:               cfg,
		gh:                client,
		git:               git,
		labels:            newLabels(cfg.LabelPrefix),
		stateCommentCache: map[int]int64{},
	}, nil
}

// WithHTTPClient is a test seam: replaces the underlying http.Client. Used
// by unit tests that drive the github backend via httptest.Server.
func (b *Backend) WithHTTPClient(c *http.Client) *Backend {
	b.gh = github.NewClient(c).WithAuthToken(b.cfg.Token)
	return b
}

// WithBaseURL is a test seam: overrides the API base URL so tests can point
// at an httptest.Server. Sets the URL fields directly (avoids the
// /api/v3/ suffix that WithEnterpriseURLs injects for GitHub Enterprise).
func (b *Backend) WithBaseURL(baseURL, uploadURL string) (*Backend, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	upload, err := url.Parse(uploadURL)
	if err != nil {
		return nil, fmt.Errorf("parse upload URL: %w", err)
	}
	b.gh.BaseURL = base
	b.gh.UploadURL = upload
	return b, nil
}

func (b *Backend) Name() string { return "github" }

func (b *Backend) SupportedSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal("pr-open", "PR with the claim branch as head exists in open state"),
		flow.Signal("pr-approved", "PR review state is approved"),
		flow.Signal("pr-merged", "PR has merged=true"),
		flow.Signal("pr-closed", "PR is closed (merged or not)"),
	}
}

// githubSupportedArtifacts is github's canonical artifact schema. github can
// technically store any id (as a state-doc entry + comment, with
// File/Patch/oversize-Markdown spilled to the orphan branch), but — like the
// tracker — it declares a closed, curated set so flows targeting it coordinate
// on a stable (id, type) vocabulary instead of inventing artifacts ad hoc. It
// covers the standard code-lifecycle artifacts plus github/PR-specific ones.
var githubSupportedArtifacts = []flow.ArtifactDef{
	// Standard code-lifecycle artifacts (shared vocabulary with the tracker).
	flow.Artifact("plan", flow.ArtifactMarkdown).WithDoc("Implementation plan for the issue."),
	flow.Artifact("implementation", flow.ArtifactPatch).WithDoc("The code change for the issue, captured as a diff."),
	flow.Artifact("review", flow.ArtifactMarkdown).WithDoc("Contributor-side code-review findings."),
	flow.Artifact("coverage", flow.ArtifactMarkdown).WithDoc("Test-coverage analysis."),
	flow.Artifact("summary", flow.ArtifactMarkdown).WithDoc("Resolution summary for the issue."),
	// github/PR-specific artifacts — the contributor→maintainer pull-request
	// lifecycle this backend is built around.
	flow.Artifact("verify-impl", flow.ArtifactMarkdown).WithDoc("Contributor-side verification (test/build) output before opening the PR."),
	flow.Artifact("review-maint", flow.ArtifactMarkdown).WithDoc("Maintainer-side review of the PR before merge."),
	flow.Artifact("verify-merge", flow.ArtifactMarkdown).WithDoc("Maintainer-side pre-merge verification output."),
	flow.Artifact("merge-commit", flow.ArtifactCommitHash).WithDoc("Hash of the merge commit on the default branch after the PR merges."),
	flow.Artifact("test-output", flow.ArtifactMarkdown).WithDoc("Captured output of a test run."),
}

// SupportedArtifacts returns github's canonical artifact schema (see
// githubSupportedArtifacts). A flow declaring an artifact outside this set —
// unknown id or mismatched type — is refused at cli.App startup. The
// declared-vs-resolved type consistency for a recorded body stays a
// resolve-time check in ResolveArtifact.
func (b *Backend) SupportedArtifacts() []flow.ArtifactDef { return githubSupportedArtifacts }

// ListEligible returns issues with the binary's flow:<name> label assigned to
// the authenticated user.
func (b *Backend) ListEligible(ctx context.Context) ([]flow.ItemRef, error) {
	// `is:open is:issue label:flow:<binary> assignee:@me` via Search.
	q := fmt.Sprintf("repo:%s/%s is:issue is:open label:%s assignee:@me",
		b.cfg.Owner, b.cfg.Repo, b.labels.Binary(b.cfg.BinaryName))
	result, _, err := b.gh.Search.Issues(ctx, q, &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	refs := make([]flow.ItemRef, 0, len(result.Issues))
	for _, issue := range result.Issues {
		refs = append(refs, b.refFromIssue(issue.GetNumber()))
	}
	return refs, nil
}

func (b *Backend) refFromIssue(number int) flow.ItemRef {
	raw, _ := json.Marshal(map[string]int{"issue": number})
	return flow.ItemRef{
		BackendName: b.Name(),
		Display:     fmt.Sprintf("%s/%s#%d", b.cfg.Owner, b.cfg.Repo, number),
		Ref:         raw,
	}
}

// issueNumber extracts the issue number embedded in an ItemRef.
func (b *Backend) issueNumber(ref flow.ItemRef) (int, error) {
	var doc struct {
		Issue int `json:"issue"`
	}
	if err := json.Unmarshal(ref.Ref, &doc); err != nil {
		return 0, fmt.Errorf("malformed github ItemRef.Ref: %w", err)
	}
	if doc.Issue == 0 {
		return 0, errors.New("github ItemRef.Ref missing issue number")
	}
	return doc.Issue, nil
}

// claimToken extracts the {state_comment_id, claim_id} from Claim.Token.
type claimToken struct {
	StateCommentID int64  `json:"state_comment_id"`
	ClaimID        string `json:"claim_id"`
}

func (b *Backend) loadClaimToken(c flow.Claim) (claimToken, error) {
	var t claimToken
	if len(c.Token) == 0 {
		return t, errors.New("github Claim missing Token")
	}
	if err := json.Unmarshal(c.Token, &t); err != nil {
		return t, fmt.Errorf("malformed github Claim.Token: %w", err)
	}
	return t, nil
}

func (b *Backend) saveClaimToken(t claimToken) (json.RawMessage, error) {
	return json.Marshal(t)
}

// LoadState fetches the issue, locates the state comment, and assembles an
// ItemState from the YAML payload + a PR poll.
func (b *Backend) LoadState(ctx context.Context, claim flow.Claim) (*flow.ItemState, error) {
	issueNum, err := b.issueNumber(claim.ItemRef)
	if err != nil {
		return nil, err
	}
	tok, err := b.loadClaimToken(claim)
	if err != nil {
		return nil, err
	}

	issue, _, err := b.gh.Issues.Get(ctx, b.cfg.Owner, b.cfg.Repo, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}

	item := flow.Item{
		ID:    strconv.Itoa(issueNum),
		Type:  itemTypeFromLabels(b.labels, labelNamesOf(issue.Labels), b.cfg.DefaultType),
		Title: issue.GetTitle(),
		Body:  issue.GetBody(),
		URL:   issue.GetHTMLURL(),
	}

	state := &flow.ItemState{
		Item:      item,
		Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{},
		Signals:   map[flow.SignalId]flow.SignalState{},
	}

	// State comment.
	stateBody, stateID, err := b.fetchStateComment(ctx, issueNum, tok.StateCommentID)
	if err != nil {
		return nil, err
	}
	if stateBody != "" {
		doc, _, found, err := extractStateDoc(stateBody)
		if err != nil {
			return nil, fmt.Errorf("parse state comment: %w", err)
		}
		if found && doc != nil {
			state.Item.Flow = doc.Flow
			for _, ad := range doc.Artifacts {
				rec := recordFromArtifactDoc(ad)
				state.Artifacts[rec.Id] = rec
			}
			for _, sd := range doc.Signals {
				state.Signals[flow.SignalId(sd.Id)] = signalStateFromDoc(sd)
			}
			state.Park = parkRequestFromDoc(doc.Park)
		}
	}
	b.mu.Lock()
	b.stateCommentCache[issueNum] = stateID
	b.mu.Unlock()

	// Poll PR signals (best-effort — never fatal).
	if err := b.refreshPRSignals(ctx, issueNum, state); err != nil {
		// Surface as a non-fatal in the item's Type field — but for now,
		// just continue with whatever state we have.
		_ = err
	}

	return state, nil
}

// fetchStateComment looks up the state comment, either by cached id or by
// scanning newest-first for the begin marker. Returns ("", 0, nil) if
// nothing matched (item not yet seeded).
func (b *Backend) fetchStateComment(ctx context.Context, issueNum int, cachedID int64) (body string, id int64, err error) {
	if cachedID != 0 {
		c, _, err := b.gh.Issues.GetComment(ctx, b.cfg.Owner, b.cfg.Repo, cachedID)
		if err == nil {
			return c.GetBody(), cachedID, nil
		}
		// If the cached comment was deleted (404), fall through to scan.
		if !isNotFound(err) {
			return "", 0, fmt.Errorf("get state comment %d: %w", cachedID, err)
		}
	}
	// Scan all comments newest-first.
	opt := &github.IssueListCommentsOptions{
		Sort:        ptr("created"),
		Direction:   ptr("desc"),
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		comments, resp, err := b.gh.Issues.ListComments(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, opt)
		if err != nil {
			return "", 0, fmt.Errorf("list comments: %w", err)
		}
		for _, c := range comments {
			if stateBeginRe.MatchString(c.GetBody()) {
				return c.GetBody(), c.GetID(), nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return "", 0, nil
}

func isNotFound(err error) bool {
	var er *github.ErrorResponse
	if errors.As(err, &er) {
		return er.Response.StatusCode == http.StatusNotFound
	}
	return false
}

func ptr[T any](v T) *T { return &v }

func labelNamesOf(labels []*github.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.GetName())
	}
	return out
}

func deriveBinaryNameFromArgv() string {
	if len(os.Args) == 0 {
		return "flow"
	}
	base := os.Args[0]
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' || base[i] == '\\' {
			return base[i+1:]
		}
	}
	return base
}

// Doctor probes gh + git + repo permissions. Implements cli.Doctor.
func (b *Backend) Doctor(ctx context.Context) error {
	repo, _, err := b.gh.Repositories.Get(ctx, b.cfg.Owner, b.cfg.Repo)
	if err != nil {
		return fmt.Errorf("repository %s/%s: %w", b.cfg.Owner, b.cfg.Repo, err)
	}
	perms := repo.GetPermissions()
	if !perms["push"] {
		return fmt.Errorf("authenticated user lacks push on %s/%s", b.cfg.Owner, b.cfg.Repo)
	}
	return nil
}

// strclamp truncates s to n bytes for safe log messages.
func strclamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Sprint helpers for diagnostic strings.
func quote(s string) string { return strings.ReplaceAll(s, "\"", "\\\"") }

// (Stub) — the real implementations of the remaining Backend methods land
// in separate files: claim.go, seed.go, artifact.go, signal.go, worktree.go.
// They use shared helpers + state above.

// helpers used by sub-files:

// updateStateComment renders the new state body and PATCHes the cached
// state comment. Returns the new body so callers can keep a local copy.
func (b *Backend) updateStateComment(ctx context.Context, issueNum int, commentID int64, doc stateDoc, owner string) (string, error) {
	body, err := renderStateComment(owner, doc)
	if err != nil {
		return "", err
	}
	_, _, err = b.gh.Issues.EditComment(ctx, b.cfg.Owner, b.cfg.Repo, commentID, &github.IssueComment{Body: &body})
	if err != nil {
		return "", fmt.Errorf("patch state comment %d: %w", commentID, err)
	}
	return body, nil
}

// postStateComment posts a fresh state comment and returns the id + body.
func (b *Backend) postStateComment(ctx context.Context, issueNum int, doc stateDoc, owner string) (int64, string, error) {
	body, err := renderStateComment(owner, doc)
	if err != nil {
		return 0, "", err
	}
	comment, _, err := b.gh.Issues.CreateComment(ctx, b.cfg.Owner, b.cfg.Repo, issueNum, &github.IssueComment{Body: &body})
	if err != nil {
		return 0, "", fmt.Errorf("post state comment: %w", err)
	}
	return comment.GetID(), body, nil
}

// nowUTC is the time source for state comment timestamps. Patched by tests.
var nowUTC = func() time.Time { return time.Now().UTC() }

// suppressWarnings keeps the linter quiet about helpers used only across
// other sub-files.
var _ = strclamp
var _ = quote
