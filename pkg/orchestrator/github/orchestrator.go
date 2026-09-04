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

// Orchestrator is the GitHub-Issues-backed flow.Orchestrator. It is a LOCAL,
// standalone orchestrator: the binary orchestrating itself against a store it
// reaches directly, with one arena — the local checkout — and one account, the
// one its credentials act as.
type Orchestrator struct {
	cfg Config
	// out is the only route from this orchestrator to GitHub — API, `gh`, and
	// the push alike. See outward.
	out    *outward
	git    *gitOps
	labels labels

	mu sync.Mutex // protects the caches below
	// cache of the most recently observed state-comment id per issue, so
	// Load can PATCH in place without re-discovering on every call.
	stateCommentCache map[int]int64
	// account memoises the login the credentials act as. It is AMBIENT — no
	// caller passes one — and it is derived exactly once, through one helper,
	// so the identity `claim` writes and the one `list` compares against are
	// the same value by construction rather than by agreement.
	account flow.AccountId
}

// New constructs a github Orchestrator. If cfg.Owner/Repo are empty, they
// are resolved from `git remote get-url origin` in cfg.WorktreeDir (or the
// current directory if empty). If cfg.Token is empty, `gh auth token` is
// tried, then GITHUB_TOKEN.
func New(cfg Config) (*Orchestrator, error) {
	cfg = cfg.withDefaults()
	if cfg.BinaryName == "" {
		cfg.BinaryName = deriveBinaryNameFromArgv()
	}

	git := newGitOps(cfg.WorktreeDir)
	if cfg.Owner == "" || cfg.Repo == "" {
		o, r, err := git.OriginOwnerRepo(context.Background())
		if err != nil {
			return nil, fmt.Errorf("github orchestrator: resolve repo: %w", err)
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

	return &Orchestrator{
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

func (b *Orchestrator) Name() flow.OrchestratorName { return "github" }

func (b *Orchestrator) SupportedSignals() []flow.SignalDef {
	return []flow.SignalDef{
		flow.Signal("pr-open", "a PR for the claim branch has been opened (latched — not unset by merge or close)"),
		flow.Signal("pr-approved", "PR review state is approved"),
		flow.Signal("pr-merged", "PR has merged=true"),
		flow.Signal("pr-closed", "PR is closed (merged or not)"),
	}
}

// githubSupportedGates is what this orchestrator declares it can run.
//
// `integration` and `fit` are required of every orchestrator and appear first.
// The rest are the project's own concerns, reached through the same `bin/gate`
// entry point — listing them is what makes integration's parts addressable, so
// a step fixing one failing suite runs that suite instead of the whole set.
var githubSupportedGates = []flow.GateDef{
	flow.Gate(flow.GateIntegration, true),
	flow.Gate(flow.GateFit, true),
	flow.Gate(flow.GateFormatted, false),
	flow.Gate(flow.GateBuilds, false),
	flow.Gate(flow.GateChecked, false),
	flow.Gate(flow.GateTested, false),
	flow.Gate(flow.GateCovered, false),
}

// SupportedGates returns the gate set. cli.App validates every gate a flow
// names against it at startup, so a flow naming a gate nothing can run fails
// before an item is claimed rather than part-way through one.
func (b *Orchestrator) SupportedGates() []flow.GateDef { return githubSupportedGates }

// githubSupportedCommands is what this orchestrator declares it can run.
// `verify` is required and configured through Config.VerifyCmd; setup and
// cleanup have no configured command here, so they are not declared — an
// orchestrator declares what it can actually run.
var githubSupportedCommands = []flow.CommandDef{
	flow.Command(flow.CommandVerify),
}

// SupportedCommands returns the command set.
func (b *Orchestrator) SupportedCommands() []flow.CommandDef { return githubSupportedCommands }

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
func (b *Orchestrator) SupportedArtifacts() []flow.ArtifactDef { return githubSupportedArtifacts }

// ResolveRef turns a user-supplied issue number (e.g. "42") into an ItemRef.
// Resolution is direct — the ref is constructed from the number alone, no API
// call — so `claim 42` and `status 42` reach any issue regardless of labels,
// assignee, or search-index lag.
//
// It is the ONE place a value enters this contract before it is an identity.
// A display string (owner/repo#42) is deliberately NOT accepted: it is a
// projection, and resolving one by substring answers with AN item rather than
// THE item.
func (b *Orchestrator) ResolveRef(ctx context.Context, input string) (flow.ItemRef, error) {
	n, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || n <= 0 {
		return flow.ItemRef{}, fmt.Errorf("github: %q is not a valid issue number", input)
	}
	return b.refFromIssue(n), nil
}

// account resolves — once — the login this arena's credentials act as.
//
// The memo is not an optimisation. It is what makes the account ONE derivation:
// the claim path and the discovery path both come through here, so the label
// `claim` writes (flow:owner:<login>) and the holder `list` compares against
// cannot be two different values. They were, until this existed — `claim` used
// $USER and `list` used GET /user, and where the two differed `claim` wrote a
// label for a user GitHub may not recognise.
func (b *Orchestrator) resolveAccount(ctx context.Context) (flow.AccountId, error) {
	b.mu.Lock()
	if b.account != "" {
		defer b.mu.Unlock()
		return b.account, nil
	}
	b.mu.Unlock()

	login, err := b.out.GetAuthenticatedUser(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve authenticated account: %w", err)
	}
	acct := flow.AccountId(login)
	if !acct.Valid() {
		return "", fmt.Errorf("github: authenticated account %q is not a storable account id", login)
	}
	b.mu.Lock()
	b.account = acct
	b.mu.Unlock()
	return acct, nil
}

func (b *Orchestrator) refFromIssue(number int) flow.ItemRef {
	raw, _ := json.Marshal(map[string]int{"issue": number})
	return flow.ItemRef{
		OrchestratorName: b.Name(),
		Display:          fmt.Sprintf("%s/%s#%d", b.cfg.Owner, b.cfg.Repo, number),
		Ref:              raw,
	}
}

// issueNumber extracts the issue number embedded in an ItemRef.
func (b *Orchestrator) issueNumber(ref flow.ItemRef) (int, error) {
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

// claimToken is what Claim carries in its Token: the orchestrator-internal
// credential. It is returned and never passed back in — every method addresses
// the item by ItemRef and finds the state comment through the cache below.
type claimToken struct {
	StateCommentID int64  `json:"state_comment_id"`
	ClaimID        string `json:"claim_id"`
}

func (b *Orchestrator) saveClaimToken(t claimToken) (json.RawMessage, error) {
	return json.Marshal(t)
}

// cachedStateCommentID returns the last state-comment id observed for an issue,
// or 0. It is a cache and not an authority: fetchStateComment falls back to a
// newest-first scan whenever it is empty or stale, which is what lets every
// method address by ref alone.
func (b *Orchestrator) cachedStateCommentID(issueNum int) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateCommentCache[issueNum]
}

// Load returns the item and everything the flow has recorded on it, addressed
// by ref. Reading an item is not a privileged act, so no claim is involved.
func (b *Orchestrator) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	return b.loadItem(ctx, issueNum, b.cachedStateCommentID(issueNum))
}

// loadItem is everything after the issue number is known.
func (b *Orchestrator) loadItem(ctx context.Context, issueNum int, cachedCommentID int64) (*flow.Item, error) {
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}

	lbls := labelNamesOf(issue.Labels)
	state := &flow.Item{
		Ref:         b.refFromIssue(issueNum),
		Type:        itemTypeFromLabels(b.labels, lbls, b.cfg.DefaultType),
		Title:       issue.GetTitle(),
		Body:        issue.GetBody(),
		URL:         issue.GetHTMLURL(),
		Status:      itemStatusFromIssue(issue),
		Disposition: dispositionFromIssue(issue),
		Holder:      b.holderFromLabels(lbls),
		Tags:        tagsOf(lbls),
		// Manual is read from the label the editor maintains, so Load reports
		// it truthfully — a write nothing can observe is not a record.
		Manual:    hasLabel(lbls, b.labels.Manual()),
		Artifacts: map[flow.ArtifactId]flow.ArtifactRecord{},
		Signals:   map[flow.SignalId]flow.SignalState{},
	}

	// Blockers travel with their own statuses, and blockedness is derived from
	// them on every read. A failure here is not fatal to the load: the item's
	// own fields are what Load is for, and `Get` is the read that answers "is
	// this blocked".
	if blockers, berr := b.blockersOf(ctx, issueNum); berr == nil {
		state.BlockedBy = blockers
		_, _, state.BlockReason = b.blockedness(blockers, lbls)
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
			state.Flow = doc.Flow
			for _, ad := range doc.Artifacts {
				rec := recordFromArtifactDoc(ad)
				state.Artifacts[rec.Id] = rec
			}
			for _, sd := range doc.Signals {
				state.Signals[flow.SignalId(sd.Id)] = signalStateFromDoc(sd)
			}
			state.Park = parkRequestFromDoc(doc.Park)
			// Questions are never removed — one leaves the pending set by
			// being answered, not by being deleted — so every recorded
			// question is reported, answered or not. That is what lets
			// PostAnswer clear the marker only when no pending question
			// remains, and what makes a second ask add rather than replace.
			state.Questions = questionsFromDocs(doc.Questions)
			state.Finalized = doc.Finalized
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
func (b *Orchestrator) fetchStateComment(ctx context.Context, issueNum int, cachedID int64) (body string, id int64, err error) {
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
func (b *Orchestrator) Doctor(ctx context.Context) error {
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
func (b *Orchestrator) RepoPermissions(ctx context.Context) (flow.RepoPermissions, error) {
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
func (b *Orchestrator) DefaultBranch(ctx context.Context) (flow.BranchName, error) {
	repo, err := b.out.GetRepo(ctx)
	if err != nil {
		return "", fmt.Errorf("repository %s/%s: %w", b.cfg.Owner, b.cfg.Repo, err)
	}
	if br := repo.GetDefaultBranch(); br != "" {
		return flow.BranchName(br), nil
	}
	return "", fmt.Errorf("repository %s/%s reports no default branch", b.cfg.Owner, b.cfg.Repo)
}

// Login returns the authenticated principal this orchestrator acts as. The
// issue package needs it to tell the flow's own comments apart from a human's
// when scanning an issue thread for answers.
//
// It goes through the same one derivation the claim path and the discovery path
// use, so nothing in this package can disagree about who it is acting as.
func (b *Orchestrator) Login(ctx context.Context) (string, error) {
	acct, err := b.resolveAccount(ctx)
	return string(acct), err
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

// The remaining Orchestrator methods land in separate files: claim.go,
// discover.go, edit.go, artifact.go, signal.go, work.go, worktree.go. They use
// the shared helpers and state above.

// helpers used by sub-files:

// updateStateComment renders the new state body and PATCHes the cached
// state comment. Returns the new body so callers can keep a local copy.
//
// The rendered body is stated `agent`, not `flow`. The frame is the SDK's
// YAML, but it interpolates a park reason and resolved-by values that came
// from a handler or an agent turn, and nobody vouches for the assembled
// string. Splitting it is not available: what is published is the whole
// comment.
func (b *Orchestrator) updateStateComment(ctx context.Context, issueNum int, commentID int64, doc stateDoc, owner flow.AccountId) (string, error) {
	body, err := renderStateComment(string(owner), doc)
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
func (b *Orchestrator) postStateComment(ctx context.Context, issueNum int, doc stateDoc, owner flow.AccountId) (int64, string, error) {
	body, err := renderStateComment(string(owner), doc)
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

// One assertion covers the whole surface. There are no optional capabilities
// to check separately: every method is required, and an orchestrator that
// cannot do one refuses it rather than omitting it.
var _ flow.Orchestrator = (*Orchestrator)(nil)

// suppressWarnings keeps the linter quiet about helpers used only across
// other sub-files.
var _ = strclamp
var _ = quote
