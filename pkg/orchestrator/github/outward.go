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
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// outward is the boundary between this backend and GitHub.
//
// docs/disclosure.md requires every byte the flow sends outward to pass a
// guard at ONE seam, and says why: "A guard installed at six call sites is a
// guard absent from the seventh, and the seventh is the one someone adds later
// without knowing this document exists."
//
// This type is that seam. It holds the package's only *github.Client and is the
// only route to `git push` — so the property is not that publish is convenient
// to call, it is that there is nothing else to call. Reaching GitHub from
// elsewhere would mean building a second client, naming api.github.com, or
// spawning `gh`, and the `service seams` step of bin/verify refuses a commit
// that does any of the three (tools/build/common/outsideroutes.go).
//
// ONE TRANSPORT. `gh` used to open and merge pull requests and to read the
// authenticated login; all three went through the client here. Both routes
// authenticate with the same token against the same limits, so the split was
// never a capability one — and a seam that meters, caches and names a rate
// limit (transport.go) can only do it for traffic it carries. `gh auth token`
// remains, and is the single named exception the gate holds: it OBTAINS a
// credential from the store the operator already logged into, once at
// construction, and never talks to GitHub.
//
// Reads live here too. Not because they publish anything, but so that no other
// file in the package needs the client at all — a file holding a client for its
// reads is a file that can write with it.
type outward struct {
	client *github.Client
	git    *gitOps
	owner  string
	repo   string

	// meter counts what this seam spent, and cache is the machine-wide record
	// every flow process on the host shares for this repository and account.
	// Both are installed in the client's transport (transport.go) rather than
	// consulted per method, so nothing here has to remember they exist. cache
	// is nil on a machine with nowhere to cache; every use tolerates it.
	meter *meter
	cache *seamCache

	// guard is consulted before every outward write and may refuse it.
	//
	// Nil means nothing is published. docs/disclosure.md fails closed, and
	// inverts the rule for gates over a tree while doing so: "not publishing
	// something publishable wastes a step; publishing something unpublishable
	// cannot be undone". An unfilled seam is the case that rule is for, so a
	// nil guard is not a bypass to configure — it is the state in which this
	// backend writes nothing at all.
	guard flow.DisclosureGuard
}

// newOutward builds the seam and the client it owns. Construction lives here
// too: a file that can build a client is a file that can write with one, so
// NewBackend hands over a token rather than a client.
//
// guard may be nil, and construction is where a missing one is tolerated: the
// reads below still work, so `list`, `status` and `doctor` do. The first write
// refuses.
func newOutward(token string, git *gitOps, owner, repo string, guard flow.DisclosureGuard) *outward {
	m := newMeter()
	c := newSeamCache(owner, repo, token)
	return &outward{
		client: newGitHubClient(token, nil, m, c),
		git:    git,
		owner:  owner,
		repo:   repo,
		meter:  m,
		cache:  c,
		guard:  guard,
	}
}

// itemOf is the disclosure's item id for a write scoped to an issue. Empty
// when the write is not issue-scoped, which is what flow.Disclosure.Item means.
func itemOf(issue int) string {
	if issue == 0 {
		return ""
	}
	return strconv.Itoa(issue)
}

// errUndeclaredAct and errUnstatableOrigin are the two refusals the seam issues
// on its own, before any guard is consulted. Both are defects in a call site
// rather than judgements about text, and both are refusals rather than errors
// because docs/disclosure.md says so: "an origin that cannot be stated is a
// refusal. Not an error and not a default-allow".
var (
	errUndeclaredAct    = errors.New("act is not in the declared vocabulary")
	errUnstatableOrigin = errors.New("a published string states no valid origin")
)

// publish is the funnel. Every method on outward that writes wraps its call in
// it; nothing else in this package reaches GitHub.
//
// It refuses before `do` runs, never after — the whole value of a guard is that
// the act has not happened yet.
//
// A refusal fails the backend call, and what becomes of it is the caller's.
// docs/disclosure.md wants the text returned to the agent that wrote it for
// revision, which is a step-handler decision rather than a seam one: the issue
// package's prose steps re-prompt on a flow.ErrDisclosureRefused and re-offer.
// The patch and pull-request write paths do not yet — a refused diff is not
// fixed by revising a sentence — and #52 is open for them.
func (o *outward) publish(ctx context.Context, d flow.Disclosure, do func(context.Context) error) error {
	d.Owner, d.Repo = o.owner, o.repo
	if !d.Act.Valid() {
		return flow.ErrDisclosureRefused{Act: d.Act, Reason: errUndeclaredAct}
	}
	for _, t := range d.Text {
		if !t.Origin.Valid() {
			return flow.ErrDisclosureRefused{
				Act:    d.Act,
				Reason: fmt.Errorf("%w: %q", errUnstatableOrigin, t.Origin),
			}
		}
	}
	if o.guard == nil {
		return flow.ErrDisclosureRefused{Act: d.Act, Reason: flow.ErrNoDisclosureGuard}
	}
	if err := o.guard.Examine(ctx, d); err != nil {
		// The guard's answer is the reason, carried unchanged: a refusal that
		// lost what it found is one the author cannot act on.
		return flow.ErrDisclosureRefused{Act: d.Act, Reason: err}
	}
	return do(ctx)
}

// ---------------------------------------------------------------------------
// Writes. Each one goes through publish.
// ---------------------------------------------------------------------------

// stated turns a slice of strings this package built into disclosure parts,
// all standing behind the same party. A fresh slice, always: what the guard is
// shown must not be the slice the API call carries, or a guard could rewrite
// the bytes after inspecting them.
func stated(origin flow.Origin, bodies ...string) []flow.Text {
	out := make([]flow.Text, 0, len(bodies))
	for _, b := range bodies {
		out = append(out, flow.Text{Origin: origin, Body: b})
	}
	return out
}

// CreateComment posts a comment on an issue. The act says which of the
// backend's comment kinds this is; the body is the comment as it will appear,
// carrying the origin its composer stated.
func (o *outward) CreateComment(ctx context.Context, a flow.DisclosureAct, issue int, body flow.Text) (*github.IssueComment, error) {
	var created *github.IssueComment
	d := flow.Disclosure{Act: a, Item: itemOf(issue), Text: []flow.Text{body}}
	err := o.publish(ctx, d, func(ctx context.Context) error {
		c, _, err := o.client.Issues.CreateComment(ctx, o.owner, o.repo, issue, &github.IssueComment{Body: &body.Body})
		created = c
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// EditComment rewrites an existing comment in place.
func (o *outward) EditComment(ctx context.Context, a flow.DisclosureAct, issue int, commentID int64, body flow.Text) error {
	d := flow.Disclosure{Act: a, Item: itemOf(issue), Text: []flow.Text{body}}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.EditComment(ctx, o.owner, o.repo, commentID, &github.IssueComment{Body: &body.Body})
		return err
	})
}

// AddLabels adds labels to an issue. Label names are the case that looks
// exempt and is not: the flow CONSTRUCTS them — flow:owner:<login>,
// flow:treasurer-refused:<step-id> — so a name is text the flow chose to
// publish.
//
// The origin is fixed here rather than taken from the caller because the whole
// input class is the flow's: a name is a closed suffix vocabulary joined to
// identifiers the SDK was configured with, and no agent prose reaches one.
//
// flow:arena: is the one suffix whose value is NOT configuration — an arena is
// a machine name and an absolute worktree path, both of them things
// docs/disclosure.md closes the door on. It keeps the justification above true
// by carrying a digest of the pair instead of the pair (see fingerprintArena),
// so what reaches this function is still a value the flow computed and not a
// fact about the operator's machine.
func (o *outward) AddLabels(ctx context.Context, issue int, names []string) error {
	d := flow.Disclosure{Act: flow.ActLabel, Item: itemOf(issue), Text: stated(flow.OriginFlow, names...)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.AddLabelsToIssue(ctx, o.owner, o.repo, issue, names)
		return err
	})
}

// RemoveLabel drops one label from an issue.
func (o *outward) RemoveLabel(ctx context.Context, issue int, name string) error {
	d := flow.Disclosure{Act: flow.ActLabel, Item: itemOf(issue), Text: stated(flow.OriginFlow, name)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, err := o.client.Issues.RemoveLabelForIssue(ctx, o.owner, o.repo, issue, name)
		return err
	})
}

// AddAssignees assigns logins to an issue. A login is a person's, typed by the
// person configuring the flow or read back from their own `gh` session — so
// the origin is fixed to operator for the same reason AddLabels fixes flow.
func (o *outward) AddAssignees(ctx context.Context, issue int, logins []string) error {
	d := flow.Disclosure{Act: flow.ActAssignee, Item: itemOf(issue), Text: stated(flow.OriginOperator, logins...)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.AddAssignees(ctx, o.owner, o.repo, issue, logins)
		return err
	})
}

func (o *outward) RemoveAssignees(ctx context.Context, issue int, logins []string) error {
	d := flow.Disclosure{Act: flow.ActAssignee, Item: itemOf(issue), Text: stated(flow.OriginOperator, logins...)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Issues.RemoveAssignees(ctx, o.owner, o.repo, issue, logins)
		return err
	})
}

// PutFile commits content to path on the artifacts branch. A non-empty
// opts.SHA updates the file already there; an empty one creates it.
//
// The path is stated `agent`, which is the case docs/disclosure.md uses to
// define the rule: artifactFilePath templates a filename the agent chose in
// flow.FileBody.Name, so the SDK stands behind its frame and nobody stands
// behind the whole string. The commit message interpolates the same filename.
func (o *outward) PutFile(ctx context.Context, path string, opts *github.RepositoryContentFileOptions) error {
	d := flow.Disclosure{
		Act:  flow.ActArtifactFile,
		Ref:  opts.GetBranch(),
		Text: stated(flow.OriginAgent, path, opts.GetMessage(), string(opts.Content)),
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
	d := flow.Disclosure{Act: flow.ActArtifactFile, Text: stated(flow.OriginAgent, blob.GetContent())}
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
	d := flow.Disclosure{Act: flow.ActArtifactFile, Text: stated(flow.OriginAgent, paths...)}
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
	d := flow.Disclosure{Act: flow.ActArtifactFile, Text: stated(flow.OriginAgent, commit.GetMessage())}
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

// CreateRef creates the artifacts branch. Its name is a constant this package
// holds — refs/heads/flow-artifacts — so the flow stands behind the whole
// string, which is the narrow case where an origin other than agent is honest.
func (o *outward) CreateRef(ctx context.Context, ref *github.Reference) error {
	d := flow.Disclosure{Act: flow.ActArtifactFile, Ref: ref.GetRef(), Text: stated(flow.OriginFlow, ref.GetRef())}
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, _, err := o.client.Git.CreateRef(ctx, o.owner, o.repo, ref)
		return err
	})
}

// OpenPullRequest opens a PR for head against base and returns its URL.
//
// Through the client, not `gh pr create`. Both authenticate as the same account
// against the same limits, so the split was never a capability one — and only
// one of the two transports can be metered, cached or told apart from a rate
// limit (see transport.go). The head needs no owner qualification because owner
// and repo come from this checkout's origin, which is the repository the branch
// was pushed to.
//
// What went with the shell-out is the `--repo` versus `-C` argument-validation
// note that used to stand here: it was a workaround for a transport, and it
// disappears with the transport.
func (o *outward) OpenPullRequest(ctx context.Context, base, head, title, body string) (string, error) {
	var prURL string
	d := flow.Disclosure{Act: flow.ActPullRequest, Ref: head}
	// Title and body are the agent's; base and head are branch names the flow
	// chose, so they are the two parts of this write that anyone vouches for.
	d.Text = append(stated(flow.OriginAgent, title, body), stated(flow.OriginFlow, base, head)...)
	err := o.publish(ctx, d, func(ctx context.Context) error {
		pr, _, err := o.client.PullRequests.Create(ctx, o.owner, o.repo, &github.NewPullRequest{
			Base:  github.Ptr(base),
			Head:  github.Ptr(head),
			Title: github.Ptr(title),
			Body:  github.Ptr(body),
		})
		if err != nil {
			return fmt.Errorf("create pull request: %w", err)
		}
		prURL = pr.GetHTMLURL()
		return nil
	})
	if err != nil {
		return "", err
	}
	return prURL, nil
}

// MergePullRequest squash-merges the PR at prURL, synchronously.
//
// The merge happens now or fails now; it is not queued. The caller's next step
// reads the merge commit this produced, and the step before it already ran the
// integration gate on the merge result — so there is nothing left to wait for,
// and deferring to GitHub's own checks (`--auto`) would only separate what the
// flow measured from what eventually lands. A failure here because the base
// moved is the ordinary outcome, and the answer to it is to bring the branch up
// and measure again — the landing loop of docs/gates-and-commands.md, whose
// arbiter is the landing step (there, the push) because it is the only one
// atomic with respect to other people landing. Merging the request is that same
// act by the other route.
//
// The URL is stated `item`: GitHub issued it, and it is already published
// there.
//
// The number is taken from the URL BEFORE the guard is consulted, because an
// unparseable one is a defect in the caller rather than a judgement about text:
// there is no write for a guard to have an opinion about, and publish's contract
// is that it refuses before the act, never after.
func (o *outward) MergePullRequest(ctx context.Context, prURL string) error {
	num, err := prNumberFromURL(prURL)
	if err != nil {
		return err
	}
	d := flow.Disclosure{Act: flow.ActMerge, Text: stated(flow.OriginItem, prURL)}
	return o.publish(ctx, d, func(ctx context.Context) error {
		// The strategy is --squash's successor and nothing else: #292 is open
		// for whether this repository should name one at all.
		_, _, err := o.client.PullRequests.Merge(ctx, o.owner, o.repo, num, "",
			&github.PullRequestOptions{MergeMethod: "squash"})
		if err != nil {
			return fmt.Errorf("merge pull request %d: %w", num, err)
		}
		return nil
	})
}

// prNumberFromURL reads the pull request number out of the URL GitHub issued
// for it — the trailing element of …/pull/<n>, which is what every caller here
// carries because that is what Open returned and what the state document holds.
//
// `gh pr merge` took the URL itself; the API takes a number, so this is where
// the two meet. A URL that names no number is a caller defect and says so.
func prNumberFromURL(prURL string) (int, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(prURL), "/")
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return 0, fmt.Errorf("pull request URL names no number: %q", prURL)
	}
	num, err := strconv.Atoi(trimmed[i+1:])
	if err != nil || num <= 0 {
		return 0, fmt.Errorf("pull request URL names no number: %q", prURL)
	}
	return num, nil
}

// Push pushes the current branch to origin with -u (set upstream).
//
// It lives here, and gitOps has no Push, because a push IS a publication:
// docs/disclosure.md lists commit messages and the diff on the surface, and
// notes they are the ones most easily forgotten because they reach the public
// through git rather than through an API call. The guard therefore has to see
// what the push would carry, which is what PushMaterial computes.
func (o *outward) Push(ctx context.Context) error {
	d, err := o.pushDisclosure(ctx)
	if err != nil {
		return err
	}
	branch := d.Ref
	return o.publish(ctx, d, func(ctx context.Context) error {
		_, stderr, err := o.git.run(ctx, "push", "-u", "origin", branch)
		if err != nil {
			return fmt.Errorf("git push -u origin %s: %w (%s)", branch, err, string(stderr))
		}
		return nil
	})
}

// ExaminePush asks the guard what a push would carry, and pushes nothing —
// flow.PushExaminer, for a step that must act on a refusal it was not handed
// (docs/disclosure.md § A refusal does not travel).
//
// It goes through the same funnel with a no-op act, rather than calling the
// guard directly: act validation, origin validation, the missing-guard refusal
// and the ErrDisclosureRefused wrapping all live in publish, and an examine
// that reimplemented them would be a second policy answering about the first
// one's write.
func (o *outward) ExaminePush(ctx context.Context) error {
	d, err := o.pushDisclosure(ctx)
	if err != nil {
		return err
	}
	return o.publish(ctx, d, func(context.Context) error { return nil })
}

// pushDisclosure assembles what pushing the current branch would publish.
//
// ONE ASSEMBLY, TWO CALLERS — Push publishes it, ExaminePush only asks about
// it. An examine that built its own copy could disagree with the push it
// predicts, which is the one way asking is worse than not asking.
func (o *outward) pushDisclosure(ctx context.Context) (flow.Disclosure, error) {
	branch, err := o.git.CurrentBranch(ctx)
	if err != nil {
		return flow.Disclosure{}, err
	}
	messages, patch, err := o.git.PushMaterial(ctx, branch)
	if err != nil {
		return flow.Disclosure{}, err
	}
	// Three origins in one write, which is why a disclosure states them per
	// string: the branch name the flow constructed, commit messages an agent
	// wrote, and the diff as it stands in the tree under resolution. Any single
	// origin for the whole push would be false about two thirds of it.
	text := stated(flow.OriginFlow, branch)
	text = append(text, stated(flow.OriginAgent, messages...)...)
	text = append(text, stated(flow.OriginWorktree, patch)...)
	return flow.Disclosure{Act: flow.ActPush, Ref: branch, Text: text}, nil
}

// EditIssue applies a title / body / labels change in one PATCH.
//
// One request because the three land ATOMICALLY: the endpoint takes them
// together, so an editor staging all three never leaves a caller asking which
// half succeeded. `labels`, when non-nil, REPLACES the set — which is why the
// editor re-reads and applies its deltas to what is actually there rather than
// assigning what it read at Edit time.
//
// The disclosure carries the prose with the origin its composer stated and the
// label names separately: a title and a body can carry anything a caller put in
// them, while a label is a name the flow constructed, and a guard decides
// differently about each.
func (o *outward) EditIssue(ctx context.Context, issue int, title, body *flow.Text, labelNames []string) error {
	var texts []flow.Text
	if title != nil {
		texts = append(texts, *title)
	}
	if body != nil {
		texts = append(texts, *body)
	}
	texts = append(texts, stated(flow.OriginFlow, labelNames...)...)
	return o.publish(ctx, flow.Disclosure{Act: flow.ActItemEdit, Item: itemOf(issue), Text: texts}, func(ctx context.Context) error {
		req := &github.IssueRequest{}
		if title != nil {
			req.Title = github.Ptr(title.Body)
		}
		if body != nil {
			req.Body = github.Ptr(body.Body)
		}
		if labelNames != nil {
			names := append([]string{}, labelNames...)
			req.Labels = &names
		}
		_, _, err := o.client.Issues.Edit(ctx, o.owner, o.repo, issue, req)
		return err
	})
}

// AddBlockedBy records that `issue` waits on the issue with the given GLOBAL
// id, via POST /repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by.
//
// The global id, not the number: that is what the endpoint takes, and it is why
// the editor resolves every blocker before committing — which is also the
// refusal the contract asks for, since an identifier naming nothing cannot be
// resolved.
//
// Recording one already recorded is not an error, so the 422 that says so is
// absorbed: the pair is in the store, which is what the caller asked for. The
// message, not the status, is the discriminator, because the status is shared
// with refusals that must stay errors — a pull request or cross-repository
// target, the feature's own limits — and only the message tells them apart.
// "Already been taken" is the uniqueness-validation wording; if it is ever
// reworded, the failure mode is today's visible error, never a swallowed
// refusal.
func (o *outward) AddBlockedBy(ctx context.Context, issue int, blockerID int64) error {
	return o.publish(ctx, flow.Disclosure{
		Act:  flow.ActBlocker,
		Item: itemOf(issue),
		Text: stated(flow.OriginFlow, strconv.FormatInt(blockerID, 10)),
	}, func(ctx context.Context) error {
		u := fmt.Sprintf("repos/%v/%v/issues/%d/dependencies/blocked_by", o.owner, o.repo, issue)
		req, err := o.client.NewRequest("POST", u, map[string]int64{"issue_id": blockerID})
		if err != nil {
			return err
		}
		resp, err := o.client.Do(ctx, req, nil)
		if err != nil && resp != nil && resp.StatusCode == http.StatusUnprocessableEntity && isAlreadyRecorded(err) {
			return nil
		}
		return err
	})
}

// isAlreadyRecorded reports whether err is GitHub's refusal to record a
// dependency pair that is already recorded, by the one thing that separates it
// from the other refusals sharing its status: the validation message.
func isAlreadyRecorded(err error) bool {
	var er *github.ErrorResponse
	return errors.As(err, &er) && strings.Contains(er.Message, "already been taken")
}

// RemoveBlockedBy retracts one dependency. Removing one that is not recorded is
// not an error, so a 404 is absorbed.
func (o *outward) RemoveBlockedBy(ctx context.Context, issue int, blockerID int64) error {
	return o.publish(ctx, flow.Disclosure{
		Act:  flow.ActBlocker,
		Item: itemOf(issue),
		Text: stated(flow.OriginFlow, strconv.FormatInt(blockerID, 10)),
	}, func(ctx context.Context) error {
		u := fmt.Sprintf("repos/%v/%v/issues/%d/dependencies/blocked_by/%d", o.owner, o.repo, issue, blockerID)
		req, err := o.client.NewRequest("DELETE", u, nil)
		if err != nil {
			return err
		}
		resp, err := o.client.Do(ctx, req, nil)
		if err != nil && resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil
		}
		return err
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

// GetComment reads one comment and returns the ETag GitHub issued for it.
//
// The tag is the state document's compare-and-set. GitHub offers no conditional
// WRITE on a comment, so the closest thing the API admits is to revalidate the
// tag immediately before the PATCH — see CommentUnchanged and withStateDoc.
// Returning it here rather than re-reading it there is what keeps the read that
// produced the document and the tag that guards it the same read.
func (o *outward) GetComment(ctx context.Context, commentID int64) (*github.IssueComment, string, error) {
	c, resp, err := o.client.Issues.GetComment(ctx, o.owner, o.repo, commentID)
	if err != nil {
		return nil, "", err
	}
	return c, etagOf(resp), nil
}

// CommentUnchanged asks whether a comment is still exactly what etag named,
// with a conditional GET.
//
// True means nobody has written since; false means a foreign writer landed and
// whatever the caller computed from the earlier read is about a document that
// no longer exists. An empty etag is no comparison at all and answers false —
// the safe side, because it makes the caller re-read rather than overwrite.
//
// Through NewRequest/Do because go-github's typed method sends no conditional
// header, and a hand-rolled http.Client here would be a second route around the
// seam. A 304 is not a 2xx, so it arrives as an error carrying the response —
// which is the answer, not a failure.
func (o *outward) CommentUnchanged(ctx context.Context, commentID int64, etag string) (bool, error) {
	if etag == "" {
		return false, nil
	}
	u := fmt.Sprintf("repos/%v/%v/issues/comments/%d", o.owner, o.repo, commentID)
	req, err := o.client.NewRequest("GET", u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("If-None-Match", etag)
	resp, err := o.client.Do(ctx, req, nil)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// etagOf reads the tag off a response, tolerating a nil one — a cached answer
// served by the transport carries the tag it was stored with, and a response
// that carries none simply cannot be compared against.
func etagOf(resp *github.Response) string {
	if resp == nil || resp.Response == nil {
		return ""
	}
	return resp.Header.Get("ETag")
}

// ListCommentsPage returns ONE page of an issue's comments plus the response
// its callers page with. Deliberately not a collect-all-pages helper: the
// state-comment scan stops at the first match, and collapsing that into a full
// walk would change how many requests this backend makes.
func (o *outward) ListCommentsPage(ctx context.Context, issue int, opt *github.IssueListCommentsOptions) ([]*github.IssueComment, *github.Response, error) {
	return o.client.Issues.ListComments(ctx, o.owner, o.repo, issue, opt)
}

// GetAuthenticatedUser returns the login the client's credentials act as, via
// `GET /user`.
//
// Through the client rather than `gh api user`, because the shell-out is a
// route around this seam: it ignores o.client, so it ignores the token the
// backend was configured with AND the base URL that points the client at a test
// server. A caller that mocked /user got a handler nothing reached, and a unit
// test silently depended on whoever last ran `gh auth login` — passing on a
// developer's machine and failing anywhere without ambient credentials.
func (o *outward) GetAuthenticatedUser(ctx context.Context) (string, error) {
	u, _, err := o.client.Users.Get(ctx, "")
	if err != nil {
		return "", err
	}
	return u.GetLogin(), nil
}

// ListBlockedBy returns the issues this one is declared to wait on, via
// GET /repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by.
//
// go-github v68 carries no binding for the endpoint, so this builds the request
// through the client's own NewRequest/Do pair. That keeps `outward` the only
// thing in this package that talks to GitHub — a hand-rolled http.Client here
// would be a second route around the seam, ignoring both the configured token
// and the base URL a test points at its own server.
//
// A repository where the feature is unavailable answers 404 or 410. That is
// reported as NO BLOCKERS rather than as an error: an orchestrator with no
// dependency notion reports no blockers and is fully conformant, and failing
// every listing over it would take `list` and `status` down on such a repo.
func (o *outward) ListBlockedBy(ctx context.Context, issue int) ([]*github.Issue, error) {
	u := fmt.Sprintf("repos/%v/%v/issues/%d/dependencies/blocked_by", o.owner, o.repo, issue)
	req, err := o.client.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	var out []*github.Issue
	resp, err := o.client.Do(ctx, req, &out)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// CollaboratorPermission returns one account's standing on the repository via
// GET /repos/{owner}/{repo}/collaborators/{login}/permission.
//
// BOTH fields the endpoint answers with are returned, because they say
// different things and only one of them is precise. `role_name` is the account's
// actual role — "admin", "maintain", "write", "triage", "read", or an
// organisation's custom role — while `permission` reports only the closest
// LEGACY base role, which is one of "admin", "write", "read" and "none": a
// maintainer comes back as "write" there and a triager as "read". A caller
// reading `permission` alone would deny the merge capability to exactly the
// accounts a repository with maintainers has, and one reading `role_name` alone
// would read a custom role as no permission at all.
//
// A refusal is a refusal. A 403 (the token may not read collaborators) and a
// 404 (no such account, or none this token can see) are returned as errors
// rather than as an empty permission, because the two readings are not the
// same: "detected nothing" is a fact about the account that role derivation
// acts on, and "could not ask" is a fact about the caller that it must not.
func (o *outward) CollaboratorPermission(ctx context.Context, login string) (roleName, permission string, err error) {
	lvl, _, err := o.client.Repositories.GetPermissionLevel(ctx, o.owner, o.repo, login)
	if err != nil {
		return "", "", err
	}
	return lvl.GetRoleName(), lvl.GetPermission(), nil
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

// ListIssues pages through the authoritative Issues API (not Search), which
// returns immediately-consistent results. Used by Discover.
func (o *outward) ListIssues(ctx context.Context, opt *github.IssueListByRepoOptions) ([]*github.Issue, *github.Response, error) {
	return o.client.Issues.ListByRepo(ctx, o.owner, o.repo, opt)
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
//
// Through newGitHubClient, like construction, so the swapped client still
// carries the metered transport and the same meter. A seam whose test path
// bypassed its own metering would be a seam nothing about it was ever tested.
func (b *Orchestrator) WithHTTPClient(c *http.Client) *Orchestrator {
	b.out.client = newGitHubClient(b.cfg.Token, c, b.out.meter, b.out.cache)
	return b
}

// ServiceRequests reports what this orchestrator has spent at the seam: how
// many requests it made, split by method, and what the cache and the rate
// limiter saved or cost.
//
// A seam that meters can say what a resolution cost, which is the only way any
// of this stays fixed.
func (b *Orchestrator) ServiceRequests() ServiceUsage { return b.out.meter.Snapshot() }

// ServiceSpend renders that snapshot as the line cli prints beside the agent's
// quota (cli.ServiceMeter).
//
// Two methods for one fact, and each has a job the other cannot do: the
// snapshot is the datum a test and a caller in this package act on, and the
// line is what a CLI that knows nothing about GitHub can print without learning
// this package's units.
func (b *Orchestrator) ServiceSpend() string {
	u := b.ServiceRequests()
	if u.Requests == 0 && u.Served == 0 {
		return ""
	}
	line := fmt.Sprintf("github: %d request(s)", u.Requests)
	if u.Served > 0 || u.Revalidated > 0 {
		line += fmt.Sprintf(", %d served from cache, %d revalidated", u.Served, u.Revalidated)
	}
	if u.Waits > 0 {
		line += fmt.Sprintf(", waited %s on %d rate limit(s)", u.Waited.Round(time.Second), u.Waits)
	}
	return line
}

// WithBaseURL is a test seam: overrides the API base URL so tests can point
// at an httptest.Server. Sets the URL fields directly (avoids the
// /api/v3/ suffix that WithEnterpriseURLs injects for GitHub Enterprise).
func (b *Orchestrator) WithBaseURL(baseURL, uploadURL string) (*Orchestrator, error) {
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

// newGitHubClient is the ONE construction path for the package's client — and
// it is in THIS file because this is the only file the `service seams` gate
// lets hold one.
//
// Construction and the metered transport are inseparable: a test that swapped
// the HTTP client and got a bare client would be exercising a seam with none of
// the properties the seam exists for. base may be nil.
//
// The cache arrives already keyed rather than being built here: it is keyed by
// repository AND account together — a token that cannot see a private
// repository must never read an answer another token cached for it — and the
// second caller, WithHTTPClient, must reuse the one already resolved rather
// than open a second record.
func newGitHubClient(token string, base *http.Client, m *meter, c *seamCache) *github.Client {
	return github.NewClient(meteredHTTPClient(base, m, c)).WithAuthToken(token)
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
