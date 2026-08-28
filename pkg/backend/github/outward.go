package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/v68/github"
)

// outward is the boundary between this backend and GitHub.
//
// docs/disclosure.md requires every byte the flow sends outward to pass a
// guard at ONE seam, and says why: "A guard installed at six call sites is a
// guard absent from the seventh, and the seventh is the one someone adds later
// without knowing this document exists."
//
// This type is that seam. It holds the package's only *github.Client, and it
// is the only route to `gh` and to `git push` — so the property is not that
// publish is convenient to call, it is that there is nothing else to call.
// Reaching GitHub from elsewhere in the package would mean building a second
// client or spawning gh directly, which TestNoSecondRouteToGitHub fails.
//
// Reads live here too. Not because they publish anything, but so that no other
// file in the package needs the client at all — a file holding a client for its
// reads is a file that can write with it.
type outward struct {
	client *github.Client
	git    *gitOps
	owner  string
	repo   string

	// guard is consulted before every outward write and may refuse it.
	//
	// Nil until #49 installs one. A nil guard allows, which is the behaviour
	// this package had before the seam existed and not a bypass: nothing
	// reads configuration to decide, and an installed guard cannot be
	// switched off — see docs/disclosure.md § "There is no bypass to
	// configure".
	guard func(context.Context, disclosure) error
}

// newOutward builds the seam and the client it owns. Construction lives here
// too: a file that can build a client is a file that can write with one, so
// NewBackend hands over a token rather than a client.
func newOutward(token string, git *gitOps, owner, repo string) *outward {
	return &outward{
		client: github.NewClient(nil).WithAuthToken(token),
		git:    git,
		owner:  owner,
		repo:   repo,
	}
}

func (o *outward) repoFullName() string { return o.owner + "/" + o.repo }

// act names what is being published. The set is closed: a write whose act is
// not named here does not exist, and adding one means adding it here.
type act string

const (
	actArtifactComment act = "artifact-comment"
	actStateComment    act = "state-comment"
	actParkRecord      act = "park-record"
	actQuestion        act = "question"
	actLabel           act = "label"
	actPullRequest     act = "pull-request"
	actMerge           act = "pull-request-merge"
	actPush            act = "push"

	// actAssignee and actArtifactFile are writes this backend performs that
	// docs/disclosure.md's surface table — which declares itself closed —
	// does not list. The gap predates the seam; making the writes explicit
	// only made it visible. These names are the interim vocabulary, and they
	// should be renamed after the document's rows once #50 settles what those
	// rows are.
	actAssignee     act = "assignee"
	actArtifactFile act = "artifact-file"
)

// disclosure is one proposed outward write: the final bytes, plus enough
// provenance to say where they were going.
//
// Final is the load-bearing word. A guard examining a template, or an artifact
// before assembly, reports a safety it did not establish — so text holds the
// strings as they will be sent, after every truncation, notice and marker this
// package appends.
type disclosure struct {
	act   act
	owner string
	repo  string
	issue int      // the issue being written to, or 0 when not issue-scoped
	ref   string   // the git ref or branch being written to, or ""
	text  []string // every string this write publishes, as it will be sent
}

// publish is the funnel. Every method on outward that writes wraps its call in
// it; nothing else in this package reaches GitHub.
func (o *outward) publish(ctx context.Context, d disclosure, do func(context.Context) error) error {
	d.owner, d.repo = o.owner, o.repo
	if o.guard != nil {
		if err := o.guard(ctx, d); err != nil {
			return fmt.Errorf("disclosure refused (%s): %w", d.act, err)
		}
	}
	return do(ctx)
}

// ---------------------------------------------------------------------------
// Writes. Each one goes through publish.
// ---------------------------------------------------------------------------

// CreateComment posts a comment on an issue. The act says which of the
// backend's comment kinds this is; the body is the comment as it will appear.
func (o *outward) CreateComment(ctx context.Context, a act, issue int, body string) (*github.IssueComment, error) {
	var created *github.IssueComment
	err := o.publish(ctx, disclosure{act: a, issue: issue, text: []string{body}}, func(ctx context.Context) error {
		c, _, err := o.client.Issues.CreateComment(ctx, o.owner, o.repo, issue, &github.IssueComment{Body: &body})
		created = c
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// EditComment rewrites an existing comment in place.
func (o *outward) EditComment(ctx context.Context, a act, issue int, commentID int64, body string) error {
	return o.publish(ctx, disclosure{act: a, issue: issue, text: []string{body}}, func(ctx context.Context) error {
		_, _, err := o.client.Issues.EditComment(ctx, o.owner, o.repo, commentID, &github.IssueComment{Body: &body})
		return err
	})
}

// AddLabels adds labels to an issue. Label names are the case that looks
// exempt and is not: the flow CONSTRUCTS them — flow:owner:<login>,
// flow:budget-exhausted:<step-id> — so a name is text the flow chose to
// publish.
func (o *outward) AddLabels(ctx context.Context, issue int, names []string) error {
	d := disclosure{act: actLabel, issue: issue, text: append([]string(nil), names...)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.AddLabelsToIssue(ctx, o.owner, o.repo, issue, names)
		return err
	})
}

// RemoveLabel drops one label from an issue.
func (o *outward) RemoveLabel(ctx context.Context, issue int, name string) error {
	d := disclosure{act: actLabel, issue: issue, text: []string{name}}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, err := o.client.Issues.RemoveLabelForIssue(ctx, o.owner, o.repo, issue, name)
		return err
	})
}

func (o *outward) AddAssignees(ctx context.Context, issue int, logins []string) error {
	d := disclosure{act: actAssignee, issue: issue, text: append([]string(nil), logins...)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.AddAssignees(ctx, o.owner, o.repo, issue, logins)
		return err
	})
}

func (o *outward) RemoveAssignees(ctx context.Context, issue int, logins []string) error {
	d := disclosure{act: actAssignee, issue: issue, text: append([]string(nil), logins...)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.RemoveAssignees(ctx, o.owner, o.repo, issue, logins)
		return err
	})
}

// PutFile commits content to path on the artifacts branch. A non-empty
// opts.SHA updates the file already there; an empty one creates it.
func (o *outward) PutFile(ctx context.Context, path string, opts *github.RepositoryContentFileOptions) error {
	d := disclosure{
		act:  actArtifactFile,
		ref:  opts.GetBranch(),
		text: []string{path, opts.GetMessage(), string(opts.Content)},
	}
	return o.publish(ctx, d, func(ctx context.Context) error {
		var err error
		if opts.GetSHA() != "" {
			_, _, err = o.client.Repositories.UpdateFile(ctx, o.owner, o.repo, path, opts)
		} else {
			_, _, err = o.client.Repositories.CreateFile(ctx, o.owner, o.repo, path, opts)
		}
		return err
	})
}

func (o *outward) CreateBlob(ctx context.Context, blob *github.Blob) (*github.Blob, error) {
	var created *github.Blob
	d := disclosure{act: actArtifactFile, text: []string{blob.GetContent()}}
	err := o.publish(ctx, d, func(ctx context.Context) error {
		b, _, err := o.client.Git.CreateBlob(ctx, o.owner, o.repo, blob)
		created = b
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (o *outward) CreateTree(ctx context.Context, baseTree string, entries []*github.TreeEntry) (*github.Tree, error) {
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.GetPath())
	}
	var created *github.Tree
	d := disclosure{act: actArtifactFile, text: paths}
	err := o.publish(ctx, d, func(ctx context.Context) error {
		tr, _, err := o.client.Git.CreateTree(ctx, o.owner, o.repo, baseTree, entries)
		created = tr
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (o *outward) CreateCommit(ctx context.Context, commit *github.Commit) (*github.Commit, error) {
	var created *github.Commit
	d := disclosure{act: actArtifactFile, text: []string{commit.GetMessage()}}
	err := o.publish(ctx, d, func(ctx context.Context) error {
		c, _, err := o.client.Git.CreateCommit(ctx, o.owner, o.repo, commit, nil)
		created = c
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (o *outward) CreateRef(ctx context.Context, ref *github.Reference) error {
	d := disclosure{act: actArtifactFile, ref: ref.GetRef(), text: []string{ref.GetRef()}}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Git.CreateRef(ctx, o.owner, o.repo, ref)
		return err
	})
}

// OpenPullRequest opens a PR for head against base and returns its URL.
//
// `gh pr create` rather than the Go client: the client requires an
// owner-qualified head, and gh handles the cross-repo (fork) case natively.
// --repo, not -C: `-C` is git's flag for selecting a working directory and gh
// has no such flag — passing it fails argument validation before gh does
// anything ("unknown shorthand flag: 'C'"). --repo also removes the dependency
// on the process working directory entirely, which the runner does not set.
func (o *outward) OpenPullRequest(ctx context.Context, base, head, title, body string) (string, error) {
	var prURL string
	d := disclosure{act: actPullRequest, ref: head, text: []string{title, body, base, head}}
	err := o.publish(ctx, d, func(ctx context.Context) error {
		args := []string{"--repo", o.repoFullName(), "pr", "create",
			"--base", base, "--title", title, "--body", body, "--head", head}
		stdout, stderr, err := o.git.runner(ctx, "", "gh", args...)
		if err != nil {
			return fmt.Errorf("gh pr create: %w (stderr=%s)", err, strings.TrimSpace(string(stderr)))
		}
		// gh pr create prints the PR URL on stdout.
		prURL = strings.TrimSpace(string(stdout))
		return nil
	})
	if err != nil {
		return "", err
	}
	return prURL, nil
}

// MergePullRequest queues an auto-squash-merge of the PR at prURL.
func (o *outward) MergePullRequest(ctx context.Context, prURL string) error {
	d := disclosure{act: actMerge, text: []string{prURL}}
	return o.publish(ctx, d, func(ctx context.Context) error {
		// --repo, not -C: see OpenPullRequest. gh has no -C flag.
		args := []string{"--repo", o.repoFullName(), "pr", "merge", prURL, "--squash", "--auto"}
		_, stderr, err := o.git.runner(ctx, "", "gh", args...)
		if err != nil {
			return fmt.Errorf("gh pr merge: %w (stderr=%s)", err, strings.TrimSpace(string(stderr)))
		}
		return nil
	})
}

// Push pushes the current branch to origin with -u (set upstream).
//
// It lives here, and gitOps has no Push, because a push IS a publication:
// docs/disclosure.md lists commit messages and the diff on the surface, and
// notes they are the ones most easily forgotten because they reach the public
// through git rather than through an API call. The guard therefore has to see
// what the push would carry, which is what PushMaterial computes.
func (o *outward) Push(ctx context.Context) error {
	branch, err := o.git.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	messages, patch, err := o.git.PushMaterial(ctx, branch)
	if err != nil {
		return err
	}
	text := make([]string, 0, len(messages)+2)
	text = append(text, branch)
	text = append(text, messages...)
	text = append(text, patch)
	return o.publish(ctx, disclosure{act: actPush, ref: branch, text: text}, func(ctx context.Context) error {
		_, stderr, err := o.git.run(ctx, "push", "-u", "origin", branch)
		if err != nil {
			return fmt.Errorf("git push -u origin %s: %w (%s)", branch, err, string(stderr))
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Reads. These publish nothing and do not go through publish; they are here so
// that no other file in the package holds the client.
// ---------------------------------------------------------------------------

func (o *outward) SearchIssues(ctx context.Context, query string, opt *github.SearchOptions) (*github.IssuesSearchResult, error) {
	result, _, err := o.client.Search.Issues(ctx, query, opt)
	return result, err
}

func (o *outward) GetIssue(ctx context.Context, issue int) (*github.Issue, error) {
	iss, _, err := o.client.Issues.Get(ctx, o.owner, o.repo, issue)
	return iss, err
}

func (o *outward) GetComment(ctx context.Context, commentID int64) (*github.IssueComment, error) {
	c, _, err := o.client.Issues.GetComment(ctx, o.owner, o.repo, commentID)
	return c, err
}

// ListCommentsPage returns ONE page of an issue's comments plus the response
// its callers page with. Deliberately not a collect-all-pages helper: the
// state-comment scan stops at the first match, and collapsing that into a full
// walk would change how many requests this backend makes.
func (o *outward) ListCommentsPage(ctx context.Context, issue int, opt *github.IssueListCommentsOptions) ([]*github.IssueComment, *github.Response, error) {
	return o.client.Issues.ListComments(ctx, o.owner, o.repo, issue, opt)
}

func (o *outward) GetRepo(ctx context.Context) (*github.Repository, error) {
	repo, _, err := o.client.Repositories.Get(ctx, o.owner, o.repo)
	return repo, err
}

func (o *outward) DownloadContents(ctx context.Context, path string, opt *github.RepositoryContentGetOptions) (io.ReadCloser, error) {
	rc, _, err := o.client.Repositories.DownloadContents(ctx, o.owner, o.repo, path, opt)
	return rc, err
}

func (o *outward) GetContents(ctx context.Context, path string, opt *github.RepositoryContentGetOptions) (*github.RepositoryContent, *github.Response, error) {
	file, _, resp, err := o.client.Repositories.GetContents(ctx, o.owner, o.repo, path, opt)
	return file, resp, err
}

func (o *outward) GetRef(ctx context.Context, ref string) (*github.Reference, *github.Response, error) {
	return o.client.Git.GetRef(ctx, o.owner, o.repo, ref)
}

func (o *outward) ListPullRequests(ctx context.Context, opt *github.PullRequestListOptions) ([]*github.PullRequest, error) {
	prs, _, err := o.client.PullRequests.List(ctx, o.owner, o.repo, opt)
	return prs, err
}

func (o *outward) ListReviews(ctx context.Context, prNum int, opt *github.ListOptions) ([]*github.PullRequestReview, error) {
	reviews, _, err := o.client.PullRequests.ListReviews(ctx, o.owner, o.repo, prNum, opt)
	return reviews, err
}

// ---------------------------------------------------------------------------
// Client construction and the `gh` invocations that precede a backend.
// ---------------------------------------------------------------------------

// WithHTTPClient is a test seam: replaces the underlying http.Client. Used
// by unit tests that drive the github backend via httptest.Server.
func (b *Backend) WithHTTPClient(c *http.Client) *Backend {
	b.out.client = github.NewClient(c).WithAuthToken(b.cfg.Token)
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
	b.out.client.BaseURL = base
	b.out.client.UploadURL = upload
	return b, nil
}

// resolveToken returns a GitHub token, preferring the explicit override,
// then `gh auth token`, then GITHUB_TOKEN.
//
// Here rather than in a file of its own so that `gh` is spawned in exactly one
// file, with no exemption list to remember.
func resolveToken(ctx context.Context, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if tok, err := ghAuthToken(ctx); err == nil && tok != "" {
		return tok, nil
	}
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		return tok, nil
	}
	return "", errors.New("no GitHub token found; run `gh auth login` or set GITHUB_TOKEN")
}

// ghAuthToken runs `gh auth token` and returns the trimmed output. Returns a
// useful error message when gh is not installed.
func ghAuthToken(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("gh CLI not installed (see https://cli.github.com/)")
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveLogin returns the authenticated gh login (used as the claim
// owner when the caller didn't override). Falls back to "" when gh is
// absent — the cli/app.go layer derives a fallback from $USER.
func resolveLogin(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", nil
	}
	// `gh api user --jq .login` is the simplest path.
	out, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
