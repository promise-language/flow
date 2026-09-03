package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	cfg Config
	// out is the only route from this backend to GitHub — API, `gh`, and the
	// push alike. See outward.
	out    *outward
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

	return &Backend{
		cfg: cfg,
		// A missing cfg.Guard is not refused here: construction is not a
		// publication, and refusing it would take `list`, `status` and
		// `doctor` — all reads — down with it. The first write refuses.
		out:               newOutward(token, git, cfg.Owner, cfg.Repo, cfg.Guard),
		git:               git,
		labels:            newLabels(cfg.LabelPrefix),
		stateCommentCache: map[int]int64{},
	}, nil
}

func (b *Backend) Name() string { return "github" }

func (b *Backend) SupportedSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal("pr-open", "a PR for the claim branch has been opened (latched — not unset by merge or close)"),
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
	flow.Artifact("branch", flow.ArtifactCommitHash).WithDoc("Hash of the commit the issue's working branch was cut from."),
	flow.Artifact("implementation", flow.ArtifactCommitHash).WithDoc("Hash of the commit carrying the code change for the issue."),
	flow.Artifact("review", flow.ArtifactMarkdown).WithDoc("Contributor-side code-review findings."),
	flow.Artifact("coverage", flow.ArtifactMarkdown).WithDoc("Test-coverage analysis."),
	flow.Artifact("summary", flow.ArtifactMarkdown).WithDoc("Resolution summary for the issue."),
	flow.Artifact("branch-closed", flow.ArtifactFlag).WithDoc("The worktree was returned to the base branch after the resolution completed."),
	// github/PR-specific artifacts — the contributor→maintainer pull-request
	// lifecycle this backend is built around.
	flow.Artifact("review-maint", flow.ArtifactMarkdown).WithDoc("Maintainer-side review of the PR before merge."),
	flow.Artifact("verify-merge", flow.ArtifactMarkdown).WithDoc("Maintainer-side pre-merge verification output."),
	flow.Artifact("merge-commit", flow.ArtifactCommitHash).WithDoc("Hash of the merge commit on the default branch after the PR merges."),
	flow.Artifact("test-output", flow.ArtifactMarkdown).WithDoc("Captured output of a test run."),
	// Maintainer-side structured verdict. JSON rather than markdown-with-a-
	// fenced-block: the body is machine-read (verdict / quality / completeness
	// / suggestions), and this backend already stores JSON inline on the state
	// doc, so a consumer gets json.RawMessage instead of parsing prose.
	flow.Artifact("inspection", flow.ArtifactJSON).WithDoc("Maintainer-side structured inspection verdict."),
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
	result, err := b.out.SearchIssues(ctx, q, &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	refs := make([]flow.ItemRef, 0, len(result.Issues))
	for _, issue := range result.Issues {
		refs = append(refs, b.refFromIssue(issue.GetNumber()))
	}
	return refs, nil
}

// ResolveRef implements flow.RefResolver: turn a user-supplied issue number
// (e.g. "42") directly into an ItemRef, without enumerating eligible items.
// Resolution is direct — the ref is constructed from the number alone, no API
// call — so `claim 42` and `status 42` reach any issue regardless of labels,
// assignee, or search-index lag.
func (b *Backend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return flow.ItemRef{}, fmt.Errorf("github: %q is not a valid issue number", id)
	}
	return b.refFromIssue(n), nil
}

// ListEligibleWithTags implements flow.TagFilterer: the same search as
// ListEligible with additional `label:<tag>` terms for each tag. The filter
// is conjunctive — an item must carry all tags.
func (b *Backend) ListEligibleWithTags(ctx context.Context, tags []string) ([]flow.ItemRef, error) {
	q := fmt.Sprintf("repo:%s/%s is:issue is:open label:%s assignee:@me",
		b.cfg.Owner, b.cfg.Repo, b.labels.Binary(b.cfg.BinaryName))
	for _, tag := range tags {
		q += " label:" + tag
	}
	result, err := b.out.SearchIssues(ctx, q, &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}})
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
	return b.loadState(ctx, issueNum, tok.StateCommentID)
}

// LoadStateByRef implements flow.StateInspector: the state document is
// addressed by issue number, and fetchStateComment already finds it with no
// cached id.
func (b *Backend) LoadStateByRef(ctx context.Context, ref flow.ItemRef) (*flow.ItemState, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	cachedID := b.stateCommentCache[issueNum]
	b.mu.Unlock()
	return b.loadState(ctx, issueNum, cachedID)
}

// loadState is the shared core of LoadState and LoadStateByRef: everything
// after the issue number is known.
func (b *Backend) loadState(ctx context.Context, issueNum int, cachedCommentID int64) (*flow.ItemState, error) {
	issue, err := b.out.GetIssue(ctx, issueNum)
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
	stateBody, stateID, err := b.fetchStateComment(ctx, issueNum, cachedCommentID)
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
			// Questions are returned exactly while the item is parked on one.
			// The record is dropped with the park that was waiting on it, so
			// this gate is a backstop rather than the rule — it covers the one
			// window the writes cannot: an ask whose park never landed,
			// because the run died between AskQuestions and Park. Reporting
			// those questions would offer `answer` a question no park is
			// waiting on, and answering it would move nothing.
			if state.Park != nil && state.Park.Kind == flow.ParkQuestion {
				state.Questions = questionsFromDocs(doc.Questions)
			}
			state.Item.Finalized = doc.Finalized
		}
	}
	b.mu.Lock()
	b.stateCommentCache[issueNum] = stateID
	b.mu.Unlock()

	// The state comment is an index, not a store: markdown bodies live in
	// their own comments. Without this a resolved artifact loads with an empty
	// body, and a step reading it proceeds on nothing with no error to show.
	//
	// NOT best-effort. Silently loading empty bodies would let an API outage
	// present as "the plan is empty", which the caller then reports as the
	// cause — after dispatching a step and spending an invocation on it. A
	// failure here says what actually went wrong, and costs nothing.
	if err := b.hydrateMarkdownBodies(ctx, issueNum, state); err != nil {
		return nil, err
	}

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
		c, err := b.out.GetComment(ctx, cachedID)
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
		comments, resp, err := b.out.ListCommentsPage(ctx, issueNum, opt)
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
	perms, err := b.RepoPermissions(ctx)
	if err != nil {
		return err
	}
	if !perms.Push {
		return fmt.Errorf("authenticated user lacks push on %s/%s", b.cfg.Owner, b.cfg.Repo)
	}
	return nil
}

// RepoPermissions reports what the authenticated user may do on the repo.
//
// GitHub returns these as a bag of booleans that are cumulative in practice —
// an admin also carries maintain/push/triage/pull — so callers deciding a role
// should test from the most privileged flag down rather than expecting exactly
// one to be set. Doctor uses it as a health check; the issue package uses it to
// pick a step set (see issue.RoleProber).
func (b *Backend) RepoPermissions(ctx context.Context) (flow.RepoPermissions, error) {
	repo, err := b.out.GetRepo(ctx)
	if err != nil {
		return flow.RepoPermissions{}, fmt.Errorf("repository %s/%s: %w", b.cfg.Owner, b.cfg.Repo, err)
	}
	p := repo.GetPermissions()
	return flow.RepoPermissions{
		Admin:    p["admin"],
		Maintain: p["maintain"],
		Push:     p["push"],
		Triage:   p["triage"],
		Pull:     p["pull"],
	}, nil
}

// DefaultBranch reports the repository's default branch ("main", "master",
// "trunk", ...). Callers that cut a working branch need it: there is no safe
// literal, and a branch cut from the wrong base is not discovered until the PR
// is opened against it.
func (b *Backend) DefaultBranch(ctx context.Context) (string, error) {
	repo, err := b.out.GetRepo(ctx)
	if err != nil {
		return "", fmt.Errorf("repository %s/%s: %w", b.cfg.Owner, b.cfg.Repo, err)
	}
	if br := repo.GetDefaultBranch(); br != "" {
		return br, nil
	}
	return "", fmt.Errorf("repository %s/%s reports no default branch", b.cfg.Owner, b.cfg.Repo)
}

// Login returns the authenticated principal this backend acts as. The issue
// package needs it to tell the flow's own comments apart from a human's when
// scanning an issue thread for answers.
func (b *Backend) Login(ctx context.Context) (string, error) {
	return resolveLogin(ctx)
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
//
// The rendered body is stated `agent`, not `flow`. The frame is the SDK's
// YAML, but it interpolates a park reason and resolved-by values that came
// from a handler or an agent turn, and nobody vouches for the assembled
// string. Splitting it is not available: what is published is the whole
// comment.
func (b *Backend) updateStateComment(ctx context.Context, issueNum int, commentID int64, doc stateDoc, owner string) (string, error) {
	body, err := renderStateComment(owner, doc)
	if err != nil {
		return "", err
	}
	if err := b.out.EditComment(ctx, flow.ActStateComment, issueNum, commentID,
		flow.Text{Origin: flow.OriginAgent, Body: body}); err != nil {
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
	comment, err := b.out.CreateComment(ctx, flow.ActStateComment, issueNum,
		flow.Text{Origin: flow.OriginAgent, Body: body})
	if err != nil {
		return 0, "", fmt.Errorf("post state comment: %w", err)
	}
	return comment.GetID(), body, nil
}

// nowUTC is the time source for state comment timestamps. Patched by tests.
var nowUTC = func() time.Time { return time.Now().UTC() }

// Compile-time conformance checks for optional capabilities the CLI reaches
// via type assertion. A missing method degrades silently to "not supported" at
// runtime, so a dropped signature is only caught here.
var _ flow.RefResolver = (*Backend)(nil)
var _ flow.StateInspector = (*Backend)(nil)
var _ flow.Discoverer = (*Backend)(nil)
var _ flow.TagFilterer = (*Backend)(nil)
var _ flow.Finalizer = (*Backend)(nil)
var _ flow.QuestionAnswerer = (*Backend)(nil)
var _ flow.FitnessChecker = (*Backend)(nil)

// CheckFit runs the fit gate in the repo root (before any worktree exists)
// and returns the verdict. Reuses the same runGate / askJudge functions the
// per-claim worktree delegates to.
func (b *Backend) CheckFit(ctx context.Context) (flow.GateVerdict, error) {
	argv := append(append([]string{}, gateEntryPoint...), string(flow.GateFit), envelopeFlag)
	run, err := runGate(ctx, b.cfg.WorktreeDir, flow.GateFit, argv, b.cfg.GateTimeout)
	if err != nil {
		return flow.GateVerdict{}, err
	}
	if run.Outcome != flow.OutcomeMeasured {
		return flow.GateVerdict{}, fmt.Errorf("fit gate outcome %s: %s", run.Outcome, run.Detail)
	}
	judgeArgv := append(append([]string{}, judgeEntryPoint...), string(flow.GateFit), verdictFlag)
	return askJudge(ctx, b.cfg.WorktreeDir, run, judgeArgv, b.cfg.GateTimeout)
}

// suppressWarnings keeps the linter quiet about helpers used only across
// other sub-files.
var _ = strclamp
var _ = quote
