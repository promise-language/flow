package common

import (
	"strings"
	"testing"
)

// The list is a claim about THIS repository, so the first test is that the
// claim is true right now. A drifted list is not a ratchet: it either refuses
// every commit or approves a site nobody looked at.
func TestApprovedOutsideRoutesMatchesTheRepository(t *testing.T) {
	if err := checkOutsideServiceRoutes(repoRoot(t)); err != nil {
		t.Fatalf("the approved outside-route list does not match the tree:\n%v", err)
	}
}

// What the check is worth depends entirely on it still detecting, and against a
// clean tree it reports nothing whether it works or not. These are files that DO
// reach GitHub outside the seam, so a check that stopped seeing one fails here
// rather than passing quietly forever.
//
// The clean cases are the other half. The seam package really does import the
// client library for option structs and github.Ptr, and the repository really
// does keep an artifact called "push", a capability constant spelled "push", and
// a permission map keyed by it. A check that flagged those would be turned off
// within a week, which is the same outcome as not having one.
func TestOutsideRoutes_FindsTheRoutesItNames(t *testing.T) {
	const lib = "github.com/google/go-github/v68/github"
	for _, tc := range []struct {
		name string
		path string
		src  string
		want string // substring the report must contain; "" means no report
	}{{
		name: "a client under the library's own name, inside the package",
		path: "pkg/orchestrator/github/discover.go",
		src: `package github
import "` + lib + `"
func f(token string) { _ = github.NewClient(nil).WithAuthToken(token) }`,
		want: "second GitHub client",
	}, {
		name: "a client under an alias",
		path: "pkg/orchestrator/github/discover.go",
		src: `package github
import gh "` + lib + `"
func f(token string) { _ = gh.NewClient(nil).WithAuthToken(token) }`,
		want: "second GitHub client",
	}, {
		name: "the client type held in a field",
		path: "pkg/orchestrator/github/signal.go",
		src: `package github
import "` + lib + `"
type backend struct{ client *github.Client }`,
		want: "second GitHub client",
	}, {
		name: "the library dot-imported",
		path: "pkg/orchestrator/github/signal.go",
		src: `package github
import . "` + lib + `"
func f() { _ = NewClient(nil) }`,
		want: "dot-imports",
	}, {
		name: "the library imported outside the seam package",
		path: "cli/cmd_list.go",
		src: `package cli
import "` + lib + `"
func f(c *github.IssueComment) string { return c.GetBody() }`,
		want: "imports the GitHub client library",
	}, {
		name: "gh spawned as a call argument",
		path: "cli/app.go",
		src: `package cli
import "context"
func f(ctx context.Context, run func(context.Context, string, string, ...string) error) {
	_ = run(ctx, "", "gh", "pr", "create")
}`,
		want: "names `gh`",
	}, {
		name: "gh spawned from an argument slice",
		path: "issue/steps.go",
		src: `package issue
func f(base string) []string { return []string{"gh", "pr", "create", "--base", base} }`,
		want: "names `gh`",
	}, {
		name: "gh held in a variable and spawned later",
		path: "issue/steps.go",
		src: `package issue
const tool = "gh"`,
		want: "names `gh`",
	}, {
		name: "gh inside the seam package but outside the credential path",
		path: "pkg/orchestrator/github/outward.go",
		src: `package github
import "os/exec"
func openPR() { _ = exec.Command("gh", "pr", "create") }`,
		want: "names `gh`",
	}, {
		name: "a push run outside the seam",
		path: "pkg/orchestrator/github/worktree.go",
		src: `package github
import "context"
func f(ctx context.Context, run func(context.Context, ...string) error, branch string) {
	_ = run(ctx, "push", "-u", "origin", branch)
}`,
		want: "outside the seam",
	}, {
		name: "a push assembled into an argument vector outside the seam",
		path: "issue/steps.go",
		src: `package issue
func f(branch string) []string { return []string{"push", "-u", "origin", branch} }`,
		want: "outside the seam",
	}, {
		name: "the host named in a literal",
		path: "cli/output.go",
		src: `package cli
const base = "https://api.github.com/"`,
		want: "api.github.com",
	}, {
		name: "the push the seam itself performs",
		path: "pkg/orchestrator/github/outward.go",
		src: `package github
import "context"
func f(ctx context.Context, run func(context.Context, ...string) error, branch string) {
	_ = run(ctx, "push", "-u", "origin", branch)
}`,
		want: "",
	}, {
		name: "the library used for what the seam package may legitimately use it for",
		path: "pkg/orchestrator/github/artifact.go",
		src: `package github
import "` + lib + `"
func f(perms map[string]bool) *github.IssueComment {
	_ = map[string]bool{"push": true, "pull": true}
	_ = perms["push"]
	return &github.IssueComment{Body: github.Ptr("hi")}
}`,
		want: "",
	}, {
		name: "push as a name rather than a command",
		path: "role.go",
		src: `package flow
type Capability string
const CapPush Capability = "push"
func artifacts() any { return Artifact("push", ArtifactCommitHash) }`,
		want: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			root := treeWith(t, tc.path, tc.src)
			// An empty approved list, so a report is a report: the real list's
			// one entry is exercised by the whole-tree test above.
			err := checkServiceRoutes(root, map[string]string{})
			if tc.want == "" {
				if err != nil {
					t.Errorf("clean file reported:\n%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("no report for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("reported:\n%v\nwant one mentioning %q", err, tc.want)
			}
		})
	}
}

// The refusal has to say who approves an exception and where the list lives, or
// the committer's next move is to delete the check.
func TestOutsideRoutes_RefusalNamesTheMaintainerAndTheList(t *testing.T) {
	root := treeWith(t, "cli/app.go", `package cli
import "os/exec"
func login() { _ = exec.Command("gh", "api", "user") }`)
	err := checkServiceRoutes(root, map[string]string{})
	if err == nil {
		t.Fatal("a gh invocation outside the seam was allowed")
	}
	for _, want := range []string{"cli/app.go", "MAINTAINER", "approvedOutsideRoutes", "one seam"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// An approved SITE covers every route inside it — `gh auth token` is a LookPath
// and a CommandContext, which is two reports about one exception.
func TestOutsideRoutes_AnApprovedSiteCoversItsWholeBody(t *testing.T) {
	root := treeWith(t, "pkg/orchestrator/github/outward.go", `package github
import "os/exec"
func ghAuthToken() {
	_, _ = exec.LookPath("gh")
	_, _ = exec.Command("gh", "auth", "token").Output()
}`)
	approved := map[string]string{
		"pkg/orchestrator/github/outward.go ghAuthToken": "the credential",
	}
	if err := checkServiceRoutes(root, approved); err != nil {
		t.Fatalf("the approved credential path was refused:\n%v", err)
	}
}

// A list that outlives the call it approved stops being exact, and an exact list
// is the whole mechanism. Removing an entry is self-service, and the message
// says so.
func TestOutsideRoutes_RefusesAStaleEntry(t *testing.T) {
	root := treeWith(t, "pkg/orchestrator/github/outward.go", "package github\n")
	err := checkServiceRoutes(root, map[string]string{
		"pkg/orchestrator/github/outward.go ghAuthToken": "the credential",
	})
	if err == nil {
		t.Fatal("an entry approving a call that no longer exists was accepted")
	}
	if !strings.Contains(err.Error(), "ordinary upkeep") {
		t.Errorf("a stale entry should read as upkeep, not as a violation:\n%v", err)
	}
}

// Tests are not scanned. The seam's own tests build clients on purpose — that is
// what pointing go-github at an httptest server means — and a rule forbidding it
// would forbid testing the seam.
func TestOutsideRoutes_IgnoresTests(t *testing.T) {
	root := treeWith(t, "cli/thing_test.go", `package cli
import "`+"github.com/google/go-github/v68/github"+`"
func TestX() { _ = github.NewClient(nil) }`)
	if err := checkServiceRoutes(root, map[string]string{}); err != nil {
		t.Fatalf("a test file was scanned: %v", err)
	}
}
