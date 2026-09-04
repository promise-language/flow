package github

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	ghclient "github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
)

// errGuardRefused is what the refusing guard answers with. A sentinel rather
// than a message so the assertions match on identity.
var errGuardRefused = errors.New("guard says no")

// guardFunc adapts a function to flow.DisclosureGuard, so a test can state its
// answer inline. The SDK ships no such adapter on purpose: a guard is supplied
// from outside, and a one-line way to write a permissive one is not something
// the SDK should make convenient.
type guardFunc func(context.Context, flow.Disclosure) error

func (f guardFunc) Examine(ctx context.Context, d flow.Disclosure) error { return f(ctx, d) }

// allowing permits everything. Tests about something OTHER than the guard
// install it, because with none installed nothing is published at all — which
// is the point of TestNoGuardPublishesNothing and would otherwise be the
// uninformative reason every other test failed.
func allowing() flow.DisclosureGuard {
	return guardFunc(func(context.Context, flow.Disclosure) error { return nil })
}

// refusing refuses everything, with the sentinel above.
func refusing() flow.DisclosureGuard {
	return guardFunc(func(context.Context, flow.Disclosure) error { return errGuardRefused })
}

// bodies drops the origins, for assertions about WHAT a disclosure carries.
// The assertions about WHO stands behind each string read d.Text directly.
func bodies(d flow.Disclosure) []string {
	out := make([]string, 0, len(d.Text))
	for _, t := range d.Text {
		out = append(out, t.Body)
	}
	return out
}

// carries reports whether any of the disclosure's strings contains fragment.
func carries(d flow.Disclosure, fragment string) bool {
	return slices.ContainsFunc(bodies(d), func(s string) bool { return strings.Contains(s, fragment) })
}

// spawnTape records every process the backend would have started, and answers
// plausibly enough for the callers that read the output.
type spawnTape struct {
	mu     sync.Mutex
	calls  [][]string // name followed by its arguments
	branch string     // current branch; "" means "main" for the first query
}

func (s *spawnTape) run(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string{name}, args...))

	// Track branch switches: `git -C <dir> checkout [-b] <name> [<base>]`.
	if name == "git" && slices.Contains(args, "checkout") {
		for i, a := range args {
			if a == "-b" && i+1 < len(args) {
				s.branch = args[i+1]
				break
			}
			if a == "checkout" && i+1 < len(args) && args[i+1] != "-b" {
				s.branch = args[i+1]
				break
			}
		}
	}
	cur := s.branch
	s.mu.Unlock()

	switch {
	case name == "gh":
		return []byte("https://github.com/o/r/pull/1\n"), nil, nil
	case slices.Contains(args, "--abbrev-ref") && slices.Contains(args, "HEAD"):
		if cur != "" {
			return []byte(cur + "\n"), nil, nil
		}
		// Before any checkout: report the base branch so pre-claim
		// preconditions pass.
		return []byte("main\n"), nil, nil
	case slices.Contains(args, "rev-parse"):
		// All other rev-parse calls (local SHA, origin/main, branch name).
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil, nil
	case slices.Contains(args, "log"):
		return []byte("a commit message\x00"), nil, nil
	}
	return nil, nil, nil
}

// outward returns the spawns that publish something: any `gh`, and any git
// push. Reads (rev-parse, log) are how the seam learns what a push would
// carry, so they are expected even when the guard refuses.
func (s *spawnTape) publishing() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]string
	for _, c := range s.calls {
		if c[0] == "gh" || slices.Contains(c, "push") {
			out = append(out, c)
		}
	}
	return out
}

func (s *spawnTape) setBranch(name string) {
	s.mu.Lock()
	s.branch = name
	s.mu.Unlock()
}

func (s *spawnTape) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

// newSeamBackend wires a Orchestrator at the mock server with every process spawn
// intercepted, so a test can watch both halves of the boundary at once.
func newSeamBackend(t *testing.T) (*Orchestrator, *ghMock, *spawnTape) {
	t.Helper()
	mock := newGHMock(t)
	// A second issue, so the drive can name one item as another's blocker.
	mock.otherIssues = []int{43}
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedOrchestrator(t, mock, srv)
	tape := &spawnTape{}
	b.git.runner = tape.run
	// One pre-existing comment, so the drive for EditComment has something to
	// edit. Carries no flow marker, so the state-comment scan ignores it.
	mock.mu.Lock()
	mock.nextCommentID++
	if mock.nextCommentID != seedCommentID {
		t.Fatalf("ghMock numbered the seed comment %d, not %d — fix seedCommentID",
			mock.nextCommentID, seedCommentID)
	}
	mock.comments = append(mock.comments, ghMockComment{
		ID: seedCommentID, Body: "a human said something", User: "carol",
	})
	mock.mu.Unlock()
	return b, mock, tape
}

// seedCommentID is the id ghMock gives that pre-existing comment. A constant
// because the drive table below takes no fixture argument; newSeamBackend
// checks the mock still agrees.
const seedCommentID = 1001

// outwardWrites drives every writing method on *outward. The map is the point
// of the test below: a write added later that does not appear here fails the
// closure check, and one that does appear but skips publish fails the refusal
// check.
func outwardWrites() map[string]func(context.Context, *outward) error {
	return map[string]func(context.Context, *outward) error{
		"CreateComment": func(ctx context.Context, o *outward) error {
			_, err := o.CreateComment(ctx, flow.ActArtifactComment, 42,
				flow.Text{Origin: flow.OriginAgent, Body: "a body"})
			return err
		},
		"EditComment": func(ctx context.Context, o *outward) error {
			return o.EditComment(ctx, flow.ActStateComment, 42, seedCommentID,
				flow.Text{Origin: flow.OriginAgent, Body: "a body"})
		},
		"AddLabels": func(ctx context.Context, o *outward) error {
			return o.AddLabels(ctx, 42, []string{"flow:seeded"})
		},
		"RemoveLabel": func(ctx context.Context, o *outward) error {
			return o.RemoveLabel(ctx, 42, "flow:seeded")
		},
		"AddAssignees": func(ctx context.Context, o *outward) error {
			return o.AddAssignees(ctx, 42, []string{"alice"})
		},
		"RemoveAssignees": func(ctx context.Context, o *outward) error {
			return o.RemoveAssignees(ctx, 42, []string{"alice"})
		},
		"PutFile": func(ctx context.Context, o *outward) error {
			return o.PutFile(ctx, artifactFilePath(42, "plan", "body.md"), &ghclient.RepositoryContentFileOptions{
				Message: ghclient.Ptr("flow: issue-42 plan/body.md"),
				Content: []byte("the plan"),
				Branch:  ghclient.Ptr(artifactsBranch),
			})
		},
		"CreateBlob": func(ctx context.Context, o *outward) error {
			_, err := o.CreateBlob(ctx, &ghclient.Blob{
				Content:  ghclient.Ptr("the plan"),
				Encoding: ghclient.Ptr("utf-8"),
			})
			return err
		},
		"CreateTree": func(ctx context.Context, o *outward) error {
			_, err := o.CreateTree(ctx, "", []*ghclient.TreeEntry{{
				Path: ghclient.Ptr(artifactFilePath(42, "plan", "body.md")),
				Mode: ghclient.Ptr("100644"),
				Type: ghclient.Ptr("blob"),
				SHA:  ghclient.Ptr("blob000001"),
			}})
			return err
		},
		"CreateCommit": func(ctx context.Context, o *outward) error {
			_, err := o.CreateCommit(ctx, &ghclient.Commit{
				Message: ghclient.Ptr("flow: issue-42 plan/body.md"),
				Parents: []*ghclient.Commit{},
			})
			return err
		},
		"CreateRef": func(ctx context.Context, o *outward) error {
			return o.CreateRef(ctx, &ghclient.Reference{
				Ref:    ghclient.Ptr("refs/heads/" + artifactsBranch),
				Object: &ghclient.GitObject{SHA: ghclient.Ptr("commit000001")},
			})
		},
		"OpenPullRequest": func(ctx context.Context, o *outward) error {
			_, err := o.OpenPullRequest(ctx, "main", "flow/issue-42", "a title", "a body")
			return err
		},
		"MergePullRequest": func(ctx context.Context, o *outward) error {
			return o.MergePullRequest(ctx, "https://github.com/o/r/pull/1")
		},
		"Push": func(ctx context.Context, o *outward) error {
			return o.Push(ctx)
		},
		"EditIssue": func(ctx context.Context, o *outward) error {
			return o.EditIssue(ctx, 42,
				&flow.Text{Origin: flow.OriginAgent, Body: "a new title"},
				&flow.Text{Origin: flow.OriginAgent, Body: "a new body"},
				[]string{"flow:seeded"})
		},
		"AddBlockedBy": func(ctx context.Context, o *outward) error {
			return o.AddBlockedBy(ctx, 42, 900001)
		},
		"RemoveBlockedBy": func(ctx context.Context, o *outward) error {
			return o.RemoveBlockedBy(ctx, 42, 900001)
		},
	}
}

// outwardReads names the methods on *outward that only read. Every exported
// method must be in exactly one of these two sets.
var outwardReads = map[string]bool{
	"GetAuthenticatedUser": true,
	"SearchIssues":         true,
	"GetIssue":             true,
	"GetComment":           true,
	"ListCommentsPage":     true,
	"ListIssues":           true,
	"GetRepo":              true,
	"DownloadContents":     true,
	"GetContents":          true,
	"GetRef":               true,
	"ListPullRequests":     true,
	"ListReviews":          true,
	"ListBlockedBy":        true,
}

// The load-bearing test. docs/disclosure.md requires the guard to be
// unavoidable — "a guard that is consulted is a convention; a guard that
// cannot be gone around is a guarantee" — so this drives EVERY writing method
// with a refusing guard and asserts the mock server saw no mutating request
// and no publishing process was spawned.
//
// Each method is driven twice. Once under an ALLOWING guard, to prove the drive
// actually reaches GitHub — without that, a method that errored on its
// arguments would satisfy the refusal assertion vacuously. Then once refusing,
// where nothing may leave.
func TestEveryOutwardWriteIsRefusable(t *testing.T) {
	writes := outwardWrites()
	for name, drive := range writes {
		t.Run(name, func(t *testing.T) {
			b, mock, tape := newSeamBackend(t)
			b.out.guard = allowing()

			// Allowed: something must actually go out.
			if err := drive(t.Context(), b.out); err != nil {
				t.Fatalf("%s under an allowing guard: %v", name, err)
			}
			mock.mu.Lock()
			sent := len(mock.mutations)
			mock.mu.Unlock()
			if sent == 0 && len(tape.publishing()) == 0 {
				t.Fatalf("%s sent nothing even when allowed — the drive does not reach GitHub, "+
					"so the refusal below would prove nothing", name)
			}

			// Refused: nothing may.
			mock.mu.Lock()
			mock.mutations = nil
			mock.mu.Unlock()
			tape.reset()
			b.out.guard = refusing()

			err := drive(t.Context(), b.out)
			if !errors.Is(err, errGuardRefused) {
				t.Errorf("%s refused: err = %v, want the guard's refusal", name, err)
			}
			mock.mu.Lock()
			leaked := append([]string(nil), mock.mutations...)
			mock.mu.Unlock()
			if len(leaked) > 0 {
				t.Errorf("%s reached GitHub despite a refusing guard: %v", name, leaked)
			}
			if spawned := tape.publishing(); len(spawned) > 0 {
				t.Errorf("%s spawned a publishing process despite a refusing guard: %v", name, spawned)
			}
		})
	}
}

// The headline behaviour, and the one regression that matters: with NO guard
// installed, nothing is published. This is the mirror of the test above —
// same drive table, same assertions about the mock and the spawn tape — and
// the difference is that there is no guard to refuse, which is exactly the
// case docs/disclosure.md fails closed for: "not publishing something
// publishable wastes a step; publishing something unpublishable cannot be
// undone."
//
// Before #49 a nil guard allowed, so every one of these wrote to GitHub.
func TestNoGuardPublishesNothing(t *testing.T) {
	for name, drive := range outwardWrites() {
		t.Run(name, func(t *testing.T) {
			b, mock, tape := newSeamBackend(t)
			b.out.guard = nil

			err := drive(t.Context(), b.out)
			if !errors.Is(err, flow.ErrNoDisclosureGuard) {
				t.Errorf("%s with no guard: err = %v, want ErrNoDisclosureGuard", name, err)
			}
			var refused flow.ErrDisclosureRefused
			if !errors.As(err, &refused) {
				t.Errorf("%s with no guard: err = %v, want an ErrDisclosureRefused naming the act", name, err)
			} else if !refused.Act.Valid() {
				t.Errorf("%s refused naming the act %q, which is not a declared one", name, refused.Act)
			}
			// A refusal is not a dead host. The orchestrator retries
			// ErrTransient, and retrying a refusal re-proposes the very text
			// that was refused.
			if errors.Is(err, flow.ErrTransient) {
				t.Errorf("%s: a refusal is indistinguishable from a transient failure, so it will be retried", name)
			}

			mock.mu.Lock()
			leaked := append([]string(nil), mock.mutations...)
			mock.mu.Unlock()
			if len(leaked) > 0 {
				t.Errorf("%s reached GitHub with no guard installed: %v", name, leaked)
			}
			if spawned := tape.publishing(); len(spawned) > 0 {
				t.Errorf("%s spawned a publishing process with no guard installed: %v", name, spawned)
			}
		})
	}
}

// A whole resolution against a backend with no guard publishes nothing, which
// is the property a per-method table cannot show: the SDK reaches GitHub
// through the backend, and a step that swallowed a refusal would still leave
// the mock untouched method by method while the run as a whole carried on.
func TestNoGuardStopsTheResolutionAtItsFirstWrite(t *testing.T) {
	b, mock, tape := newSeamBackend(t)
	b.out.guard = nil

	_, err := b.Claim(t.Context(), b.refFromIssue(42), nil)
	if !errors.Is(err, flow.ErrNoDisclosureGuard) {
		t.Errorf("Claim with no guard: err = %v, want ErrNoDisclosureGuard", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.mutations) > 0 {
		t.Errorf("Claim wrote to GitHub with no guard installed: %v", mock.mutations)
	}
	if spawned := tape.publishing(); len(spawned) > 0 {
		t.Errorf("Claim spawned a publishing process with no guard installed: %v", spawned)
	}
}

// Failing closed is about writes, and only writes. A backend built with no
// guard still READS — which is why NewBackend does not refuse construction, and
// why `list`, `status` and `doctor` keep working on a binary that has not been
// handed one yet. Refusing at construction would have taken them down too.
func TestNoGuardStillReads(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	b.out.guard = nil
	ctx := t.Context()

	if _, err := b.ListAutoSelectable(ctx, nil); err != nil {
		t.Errorf("ListAutoSelectable with no guard: %v", err)
	}
	if err := b.Doctor(ctx); err != nil {
		t.Errorf("Doctor with no guard: %v", err)
	}
	if _, err := b.out.GetIssue(ctx, 42); err != nil {
		t.Errorf("GetIssue with no guard: %v", err)
	}
	if _, err := b.LookupClaim(ctx, b.refFromIssue(42)); err != nil {
		t.Errorf("LookupClaim with no guard: %v", err)
	}
}

// Every guard above is installed by assigning b.out.guard, a field no binary
// can reach. Config.Guard is the only route one has and NewBackend the only
// constructor, so the line that carries cfg.Guard into newOutward is what makes
// all of it real — and it is the one part nothing above touches. Drop it and
// every test here still passes while no injected guard is ever consulted again;
// put an allowing one there instead and the backend publishes everything, with
// nothing able to say so.
func TestConfigGuardIsTheOnlyWayToInstallOne(t *testing.T) {
	// Built the way a binary builds it: through NewBackend, from a Config.
	configured := func(t *testing.T, guard flow.DisclosureGuard) (*Orchestrator, *ghMock) {
		t.Helper()
		mock := newGHMock(t)
		srv := mock.server()
		t.Cleanup(srv.Close)
		b, err := New(Config{
			Owner:      mock.owner,
			Repo:       mock.repo,
			BinaryName: "implement",
			Token:      "fake-token",
			Guard:      guard,
		})
		if err != nil {
			t.Fatalf("NewBackend: %v", err)
		}
		if _, err := b.WithBaseURL(srv.URL+"/", srv.URL+"/"); err != nil {
			t.Fatalf("WithBaseURL: %v", err)
		}
		return b, mock
	}
	write := func(t *testing.T, b *Orchestrator) error {
		t.Helper()
		_, err := b.out.CreateComment(t.Context(), flow.ActArtifactComment, 42,
			flow.Text{Origin: flow.OriginAgent, Body: "a body"})
		return err
	}
	sent := func(t *testing.T, mock *ghMock) []string {
		t.Helper()
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return append([]string(nil), mock.mutations...)
	}

	t.Run("the guard in the config is the one consulted", func(t *testing.T) {
		guard := &recordingGuard{}
		b, mock := configured(t, guard)
		if err := write(t, b); err != nil {
			t.Fatalf("CreateComment: %v", err)
		}
		if n := len(guard.of(flow.ActArtifactComment)); n != 1 {
			t.Errorf("the guard handed to NewBackend saw %d disclosures, want 1: Config.Guard never reached the seam", n)
		}
		// Without this, the two refusals below would be satisfied just as well
		// by a backend that cannot write at all.
		if len(sent(t, mock)) == 0 {
			t.Error("an allowed write reached nothing, so the refusals below would prove nothing")
		}
	})

	t.Run("its refusal stops the write", func(t *testing.T) {
		b, mock := configured(t, refusing())
		if err := write(t, b); !errors.Is(err, errGuardRefused) {
			t.Errorf("err = %v, want the configured guard's refusal", err)
		}
		if leaked := sent(t, mock); len(leaked) > 0 {
			t.Errorf("a write reached GitHub although the configured guard refused it: %v", leaked)
		}
	})

	t.Run("a config with no guard constructs and publishes nothing", func(t *testing.T) {
		// Construction must succeed — refusing it would take `list`, `status`
		// and `doctor` down with it — and then the first write refuses.
		b, mock := configured(t, nil)
		if err := write(t, b); !errors.Is(err, flow.ErrNoDisclosureGuard) {
			t.Errorf("err = %v, want ErrNoDisclosureGuard", err)
		}
		if leaked := sent(t, mock); len(leaked) > 0 {
			t.Errorf("a write reached GitHub from a backend configured with no guard: %v", leaked)
		}
	})
}

// "An origin that cannot be stated is a refusal. Not an error and not a
// default-allow: unattributable text is exactly the case this exists for."
// The guard is not even consulted — there is nothing for it to decide about a
// string whose provenance the caller could not name.
func TestUnstatableOriginIsRefused(t *testing.T) {
	for _, origin := range []flow.Origin{"", "unknown", "probably-fine"} {
		t.Run(string(origin), func(t *testing.T) {
			b, mock, _ := newSeamBackend(t)
			shown := 0
			b.out.guard = guardFunc(func(context.Context, flow.Disclosure) error { shown++; return nil })

			_, err := b.out.CreateComment(t.Context(), flow.ActArtifactComment, 42,
				flow.Text{Origin: origin, Body: "a body"})
			if !errors.Is(err, errUnstatableOrigin) {
				t.Errorf("origin %q: err = %v, want a refusal naming the unstatable origin", origin, err)
			}
			if !strings.Contains(err.Error(), string(origin)) {
				t.Errorf("origin %q: the refusal %q does not quote what it found", origin, err)
			}
			if shown != 0 {
				t.Errorf("origin %q: the guard was consulted %d times about text with no stated origin", origin, shown)
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if len(mock.mutations) > 0 {
				t.Errorf("origin %q reached GitHub: %v", origin, mock.mutations)
			}
		})
	}
}

// The act vocabulary is closed on both sides: a guard cannot answer about a
// write it has no name for, and a refusal that named an undeclared act would
// tell nobody anything. Undeclared is refused at the seam, before the guard.
func TestUndeclaredActIsRefused(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	shown := 0
	b.out.guard = guardFunc(func(context.Context, flow.Disclosure) error { shown++; return nil })

	_, err := b.out.CreateComment(t.Context(), flow.DisclosureAct("comment"), 42,
		flow.Text{Origin: flow.OriginAgent, Body: "a body"})
	if !errors.Is(err, errUndeclaredAct) {
		t.Errorf("err = %v, want a refusal naming the undeclared act", err)
	}
	if shown != 0 {
		t.Errorf("the guard was consulted %d times about an act it has no name for", shown)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.mutations) > 0 {
		t.Errorf("a write with an undeclared act reached GitHub: %v", mock.mutations)
	}
}

// "A refusal names what it found and where, and quotes enough to act on. 'This
// text may contain private information' is indistinguishable from a guard that
// gave up." The seam adds the act and keeps the guard's own answer intact —
// wrapping that replaced the reason would lose the only part an author can act
// on.
func TestRefusalNamesTheActAndKeepsTheReason(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	found := errors.New("line 3 names /home/someone/prog/other-project")
	b.out.guard = guardFunc(func(context.Context, flow.Disclosure) error { return found })

	_, err := b.out.CreateComment(t.Context(), flow.ActArtifactComment, 42,
		flow.Text{Origin: flow.OriginAgent, Body: "a body"})
	if !errors.Is(err, found) {
		t.Errorf("err = %v, want the guard's own reason reachable through it", err)
	}
	var refused flow.ErrDisclosureRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v (%T), want an ErrDisclosureRefused", err, err)
	}
	if refused.Act != flow.ActArtifactComment {
		t.Errorf("refusal names the act %q, want %q", refused.Act, flow.ActArtifactComment)
	}
	if !strings.Contains(err.Error(), found.Error()) {
		t.Errorf("the refusal %q does not carry what the guard found", err)
	}
	if !strings.Contains(err.Error(), string(flow.ActArtifactComment)) {
		t.Errorf("the refusal %q does not name what was refused", err)
	}
}

// The seam is only one seam if every method on it is accounted for. A method
// added to *outward that is in neither table fails here until it is classified,
// and classifying it as a write puts it under the refusal test above.
//
// The declarations are read out of the package source rather than reflected
// over the type. Reflection sees exported methods only, and an unexported one
// that reached the client directly would be exactly the seventh call site
// docs/disclosure.md is about — invisible to this check and to
// TestNoSecondRouteToGitHub, which exempts outward.go.
func TestOutwardMethodsAreAllClassified(t *testing.T) {
	// Neither a read nor a write of its own: publish IS the funnel the writes
	// go through, and repoFullName only formats "owner/repo". Adding a name
	// here is a claim that the method reaches GitHub by neither route.
	notARoute := map[string]bool{"publish": true, "repoFullName": true}

	writes := outwardWrites()
	found := 0
	for _, path := range packageSourceFiles(t) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 ||
				receiverTypeName(fn.Recv.List[0].Type) != "outward" {
				continue
			}
			found++
			name := fn.Name.Name
			_, isWrite := writes[name]
			switch {
			case isWrite && outwardReads[name]:
				t.Errorf("outward.%s is listed as both a read and a write", name)
			case isWrite || outwardReads[name] || notARoute[name]:
				// classified
			default:
				t.Errorf("%s: outward.%s is neither driven by outwardWrites nor listed in "+
					"outwardReads; if it writes, add it to outwardWrites so "+
					"TestEveryOutwardWriteIsRefusable covers it", fset.Position(fn.Pos()), name)
			}
		}
	}
	if found == 0 {
		t.Fatal("found no methods on *outward — the check is not running")
	}
}

// receiverTypeName is the bare type name of a method receiver, pointer or not.
func receiverTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// packageSourceFiles lists the package's non-test .go files. The checks that
// read them are the ones that cannot be expressed as an assertion about a
// value, so they have to agree on what "the package" is.
func packageSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("scanned no files — the check is not running")
	}
	return out
}

// recordingGuard captures what the seam was shown, and allows.
type recordingGuard struct {
	mu   sync.Mutex
	seen []flow.Disclosure
}

func (g *recordingGuard) Examine(_ context.Context, d flow.Disclosure) error {
	g.mu.Lock()
	g.seen = append(g.seen, d)
	g.mu.Unlock()
	return nil
}

func (g *recordingGuard) of(a flow.DisclosureAct) []flow.Disclosure {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []flow.Disclosure
	for _, d := range g.seen {
		if d.Act == a {
			out = append(out, d)
		}
	}
	return out
}

// all returns every disclosure the guard was shown, in order.
func (g *recordingGuard) all() []flow.Disclosure {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]flow.Disclosure(nil), g.seen...)
}

// docs/disclosure.md: "It sees the final bytes. Not the template, not the
// artifact before assembly, not the prose before the SDK wrapped it in a
// heading."
//
// A markdown artifact over MaxCommentBytes is the case where that bites: what
// is published is neither the artifact nor a template but a THIRD string —
// the marker line, a 4KiB truncation of the prose, and a spill notice pointing
// at the orphan branch. A guard shown the artifact would be examining text
// that is never published, and missing the URL that is.
func TestGuardSeesTheAssembledCommentAndNotTheArtifact(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	b.cfg.MaxCommentBytes = 512
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{{Id: "plan", Type: flow.ArtifactMarkdown}}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	guard := &recordingGuard{}
	b.out.guard = guard

	prose := strings.Repeat("plan prose. ", 900) // comfortably over 4KiB
	if err := b.ResolveArtifact(ctx, claim.ItemRef, "plan", flow.ArtifactBody{
		Type:     flow.ArtifactMarkdown,
		Markdown: prose,
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	comments := guard.of(flow.ActArtifactComment)
	if len(comments) != 1 {
		t.Fatalf("guard saw %d artifact comments, want 1", len(comments))
	}
	body := comments[0].Text[0].Body
	if !strings.HasPrefix(body, artifactCommentMarkerPrefix) {
		t.Errorf("guard was shown the prose, not the assembled comment: %.60q", body)
	}
	if !strings.Contains(body, spillNoticePrefix) {
		t.Error("guard was not shown the spill notice, which is text the SDK appended after assembly")
	}
	if strings.Contains(body, prose) {
		t.Error("guard was shown the whole artifact; the published comment carries a truncated preview")
	}
	if comments[0].Item != "42" {
		t.Errorf("disclosure names item %q, want %q", comments[0].Item, "42")
	}

	// The spilled bytes are their own disclosure — the comment holds only a
	// preview, so a guard that saw the comment alone would never see the rest.
	files := guard.of(flow.ActArtifactFile)
	if len(files) == 0 {
		t.Fatal("the spill to the artifacts branch never reached the guard")
	}
	if !slices.ContainsFunc(files, func(d flow.Disclosure) bool {
		return slices.Contains(bodies(d), prose)
	}) {
		t.Error("the spill disclosure does not carry the artifact's bytes")
	}
}

// "The labels are the case that looks exempt and is not: the flow constructs
// label names, and a constructed name is text the flow chose to publish."
func TestGuardSeesConstructedLabelNames(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{{Id: "implement", Type: flow.ArtifactMarkdown}}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	guard := &recordingGuard{}
	b.out.guard = guard

	if err := b.Park(ctx, claim.ItemRef, flow.ParkRequest{
		Kind:   flow.ParkBudgetExhausted,
		Step:   "implement",
		Reason: "spent the budget",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	want := b.labels.BudgetExhausted("implement")
	if !slices.ContainsFunc(guard.of(flow.ActLabel), func(d flow.Disclosure) bool {
		return slices.Contains(bodies(d), want)
	}) {
		t.Errorf("guard never saw the constructed label %q; saw %v", want, guard.of(flow.ActLabel))
	}
	if len(guard.of(flow.ActParkRecord)) != 1 {
		t.Errorf("guard saw %d park records, want 1", len(guard.of(flow.ActParkRecord)))
	}
}

// A refusal must reach the caller rather than being absorbed on the way out.
func TestRefusedWriteFailsTheBackendCall(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()

	b.out.guard = refusing()
	_, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if !errors.Is(err, errGuardRefused) {
		t.Errorf("Claim under a refusing guard: err = %v, want the refusal", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.mutations) > 0 {
		t.Errorf("Claim wrote to GitHub despite a refusing guard: %v", mock.mutations)
	}
}

// docs/disclosure.md: "It never modifies what it examines." Every disclosure
// hands the guard a slice, and if it were the SAME slice the API call carries,
// a guard could rewrite the bytes after inspecting them — and then what was
// examined and what was published are two different texts, with nothing to say
// so. A comment carries the most, and the labels and assignees are the writes
// whose input the caller already holds as a slice.
func TestTheGuardCannotAlterWhatIsSent(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	b.out.guard = guardFunc(func(_ context.Context, d flow.Disclosure) error {
		for i := range d.Text {
			d.Text[i].Body = "rewritten-after-inspection"
			d.Text[i].Origin = flow.OriginFlow
		}
		return nil
	})

	if err := b.out.AddLabels(ctx, 42, []string{"flow:owner:alice"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if err := b.out.AddAssignees(ctx, 42, []string{"alice"}); err != nil {
		t.Fatalf("AddAssignees: %v", err)
	}
	const body = "the body the guard was shown"
	if _, err := b.out.CreateComment(ctx, flow.ActArtifactComment, 42,
		flow.Text{Origin: flow.OriginAgent, Body: body}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !slices.ContainsFunc(mock.comments, func(c ghMockComment) bool { return c.Body == body }) {
		t.Error("the comment body the guard was shown is not the one that was posted")
	}
	for _, c := range mock.comments {
		if c.Body == "rewritten-after-inspection" {
			t.Error("the guard rewrote a comment body by mutating what it was shown")
		}
	}
	if !slices.Contains(mock.issueLabels, "flow:owner:alice") {
		t.Errorf("the label the guard was shown is not the one that was sent: %v", mock.issueLabels)
	}
	if !slices.Contains(mock.assignees, "alice") {
		t.Errorf("the assignee the guard was shown is not the one that was sent: %v", mock.assignees)
	}
	for _, sent := range append(append([]string(nil), mock.issueLabels...), mock.assignees...) {
		if sent == "rewritten-after-inspection" {
			t.Error("the guard rewrote what was published by mutating what it was shown")
		}
	}
}

// A disclosure carries where it was going, not just what it said. The seam is
// where docs/disclosure.md puts the guard partly because "this also puts it
// where provenance is still legible" — every caller below names an issue at
// most, so the repository has to be filled in here or it is nowhere.
func TestDisclosureNamesTheRepositoryItWouldReach(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	guard := &recordingGuard{}
	b.out.guard = guard

	if _, err := b.out.CreateComment(t.Context(), flow.ActArtifactComment, 42,
		flow.Text{Origin: flow.OriginAgent, Body: "a body"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	seen := guard.of(flow.ActArtifactComment)
	if len(seen) != 1 {
		t.Fatalf("guard saw %d disclosures, want 1", len(seen))
	}
	if seen[0].Owner != "o" || seen[0].Repo != "r" {
		t.Errorf("disclosure names %q/%q, want the repository the backend writes to",
			seen[0].Owner, seen[0].Repo)
	}
}

// driveAResolution runs a whole resolution against the mock — claim, seed,
// resolve a file artifact (which always spills, so the artifacts branch is
// reached too), ask, park, open and merge — and returns the PR URL. Every
// declared act comes out of it, which is what makes it worth sharing between
// the tests that assert about the set rather than about one write.
func driveAResolution(t *testing.T, b *Orchestrator, tape *spawnTape) (prURL flow.RequestUrl) {
	t.Helper()
	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Simulate the branch switch that ensureBranch would do in a real
	// resolution — the test constructs a worktree directly, and Open checks
	// that CurrentBranch returns the claim branch.
	tape.setBranch(b.claimBranch(42))
	if err := b.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactFile, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	if err := b.ResolveArtifact(ctx, claim.ItemRef, "plan", flow.ArtifactBody{
		Type: flow.ArtifactFile,
		File: flow.FileBody{Name: "notes.txt", Content: []byte("the spilled bytes")},
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	q, err := b.AskQuestion(ctx, claim.ItemRef, flow.AgentQuestion{
		Header: "Which base branch?", Text: "main, or the release branch?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}
	if err := b.Park(ctx, claim.ItemRef, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "waiting on the base branch",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := b.PostAnswer(ctx, claim.ItemRef, q.ID, "use main"); err != nil {
		t.Fatalf("PostAnswer: %v", err)
	}
	// The editor. Title, body and tags are one PATCH; a blocker goes through
	// its own endpoint, and a stage mixing the two is refused — so the drive is
	// two edits, which is what the contract asks a caller to do.
	ed, err := b.Edit(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.SetTitle("a rewritten title")
	ed.SetBody("a rewritten body")
	ed.AddTag("priority:high")
	if err := ed.Commit(ctx); err != nil {
		t.Fatalf("Commit fields: %v", err)
	}
	blockers, err := b.Edit(ctx, claim.ItemRef)
	if err != nil {
		t.Fatalf("Edit (blockers): %v", err)
	}
	blockers.AddBlocker(b.refFromIssue(43))
	if err := blockers.Commit(ctx); err != nil {
		t.Fatalf("Commit blockers: %v", err)
	}

	w := &worktree{b: b, issueNum: 42}
	prURL, err = w.Open(ctx, "main", "a pull request title", "a pull request body")
	if err != nil {
		t.Fatalf("worktree.Open: %v", err)
	}
	if err := w.Merge(ctx, prURL); err != nil {
		t.Fatalf("worktree.Merge: %v", err)
	}
	if err := w.Push(ctx); err != nil {
		t.Fatalf("worktree.Push: %v", err)
	}
	return prURL
}

// The act vocabulary is closed, and a name in it is worth nothing unless some
// call site produces it carrying that write's final bytes. The kinds asserted
// above — the artifact comment, the spilled bytes, the constructed label, the
// park record — are not repeated here; this drives the rest of a resolution and
// fails when a declared act has no call site behind it, which is what a write
// naming the wrong act looks like from outside.
func TestEveryActReachesTheGuardWithItsFinalBytes(t *testing.T) {
	b, _, tape := newSeamBackend(t)
	guard := &recordingGuard{}
	b.out.guard = guard
	prURL := driveAResolution(t, b, tape)

	for _, a := range flow.AllDisclosureActs() {
		if len(guard.of(a)) == 0 {
			t.Errorf("nothing produced the act %q: either no call site names it, "+
				"or the one that should names another", a)
		}
	}

	for _, want := range []struct {
		act       flow.DisclosureAct
		fragments []string
	}{
		{flow.ActAssignee, []string{"alice"}},
		{flow.ActStateComment, []string{"flow:state-v1 begin"}},
		{flow.ActQuestion, []string{"<!-- flow:question", "Which base branch?", "main, or the release branch?"}},
		{flow.ActPullRequest, []string{"a pull request title", "a pull request body", "main", "flow/issue-42"}},
		{flow.ActMerge, []string{string(prURL)}},
	} {
		seen := guard.of(want.act)
		for _, fragment := range want.fragments {
			if !slices.ContainsFunc(seen, func(d flow.Disclosure) bool { return carries(d, fragment) }) {
				t.Errorf("no %q disclosure carries %q; saw %v", want.act, fragment, seen)
			}
		}
	}

	// The pull request publishes a branch, and the disclosure has to say which.
	if pr := guard.of(flow.ActPullRequest); len(pr) != 1 || pr[0].Ref != "flow/issue-42" {
		t.Errorf("pull-request disclosure ref = %v, want the head branch it opens from", pr)
	}
}

// The whole point of stating an origin is that provenance cannot be recovered
// from text, so a call site that forgets to state one has published something
// nothing could have judged. This drives the same full resolution as the test
// above and checks every part of every disclosure names a declared origin —
// which is what catches a write added later that leaves the field zero.
//
// The seam refuses an unstatable origin (TestUnstatableOriginIsRefused), so a
// forgotten one would fail this as a refusal rather than as a bad value. Both
// are the same defect; this names it.
func TestEveryPublishedStringStatesAnOrigin(t *testing.T) {
	b, _, tape := newSeamBackend(t)
	guard := &recordingGuard{}
	b.out.guard = guard
	driveAResolution(t, b, tape)

	seen := guard.all()
	if len(seen) == 0 {
		t.Fatal("the guard was shown nothing — the drive is not publishing")
	}
	parts := 0
	for _, d := range seen {
		if len(d.Text) == 0 {
			t.Errorf("the %q disclosure carries no text at all", d.Act)
		}
		for _, part := range d.Text {
			parts++
			if !part.Origin.Valid() {
				t.Errorf("a %q disclosure states the origin %q for %.60q, which is not one of %v",
					d.Act, part.Origin, part.Body, flow.AllOrigins())
			}
		}
	}
	if parts == 0 {
		t.Fatal("no disclosure carried any text — the check is not running")
	}
}

// WHICH party is stated is the entire content of a disclosure: text carries no
// provenance, so an origin the guard is told is one it cannot check. Every call
// site's choice is deliberate and argued in a comment beside it, and each
// argument runs against the obvious reading — the state comment and the park
// record are the SDK's own YAML and JSON and are stated `agent` because of what
// they interpolate; a label name looks like text someone composed and is stated
// `flow` because the SDK constructs the whole of it.
//
// TestEveryPublishedStringStatesAnOrigin asserts only that SOME declared origin
// is stated. Outside the push and the artifact path, nothing asserts which — so
// "correcting" the state comment to `flow`, which is what its frame looks like,
// would pass every test in this file.
func TestEachWriteStatesWhoStandsBehindIt(t *testing.T) {
	b, _, tape := newSeamBackend(t)
	guard := &recordingGuard{}
	b.out.guard = guard
	prURL := driveAResolution(t, b, tape)

	for _, want := range []struct {
		act      flow.DisclosureAct
		fragment string
		origin   flow.Origin
		why      string
	}{
		{flow.ActArtifactComment, artifactCommentMarkerPrefix, flow.OriginAgent,
			"the SDK's marker line wraps an artifact an agent produced, so nobody vouches for the assembled comment"},
		{flow.ActStateComment, "flow:state-v1 begin", flow.OriginAgent,
			"the YAML frame is the SDK's, but it interpolates values a handler or an agent turn supplied"},
		{flow.ActParkRecord, "waiting on the base branch", flow.OriginAgent,
			"the JSON frame is the SDK's; the park reason inside it is not"},
		{flow.ActQuestion, "Which base branch?", flow.OriginAgent,
			"an agent composed the question the SDK renders"},
		{flow.ActLabel, "flow:", flow.OriginFlow,
			"a label name is a closed suffix vocabulary joined to identifiers the SDK was configured with"},
		{flow.ActAssignee, "alice", flow.OriginOperator,
			"a login is a person's, typed by whoever configured the flow or read back from their own gh session"},
		{flow.ActMerge, string(prURL), flow.OriginItem,
			"GitHub issued the URL, and the destination has already published it"},
		{flow.ActPullRequest, "a pull request title", flow.OriginAgent, "an agent wrote the title"},
		{flow.ActPullRequest, "a pull request body", flow.OriginAgent, "an agent wrote the body"},
		{flow.ActPullRequest, "flow/issue-42", flow.OriginFlow, "the SDK constructed the claim branch name"},
	} {
		// EVERY part carrying the fragment, for the reason
		// TestPushSeparatesBranchMessagesAndDiff checks every one: a fragment
		// turning up in a second part under another origin means one of the two
		// vouches for text it did not compose.
		found := 0
		for _, d := range guard.of(want.act) {
			for _, p := range d.Text {
				if !strings.Contains(p.Body, want.fragment) {
					continue
				}
				found++
				if p.Origin != want.origin {
					t.Errorf("%q states %q for %.60q, want %q — %s",
						want.act, p.Origin, p.Body, want.origin, want.why)
				}
			}
		}
		if found == 0 {
			t.Errorf("no %q disclosure carries %q at all", want.act, want.fragment)
		}
	}
}

// A push is the one write whose parts genuinely have three different origins,
// and the case a single origin per write could not express: collapsing it would
// have to discard either the worktree origin the document defines for exactly
// this, or the agent origin of the commit messages.
//
// Against a real repository, for the reason TestPushMaterial is.
func TestPushSeparatesBranchMessagesAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, origin := t.TempDir(), t.TempDir()
	gitInTest(t, origin, "init", "--bare", "-b", "main", ".")
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "old content\n", "on origin already")
	gitInTest(t, dir, "remote", "add", "origin", origin)
	gitInTest(t, dir, "push", "origin", "main")
	gitInTest(t, dir, "checkout", "-b", "flow/issue-42")
	commitFile(t, dir, "b.txt", "a line only this branch has\n", "new work here")

	g := newGitOps(dir)
	spawn := g.runner
	g.runner = func(ctx context.Context, wd, name string, args ...string) ([]byte, []byte, error) {
		if slices.Contains(args, "push") {
			return nil, nil, nil
		}
		return spawn(ctx, wd, name, args...)
	}

	guard := &recordingGuard{}
	o := &outward{git: g, owner: "o", repo: "r", guard: guard}
	if err := o.Push(t.Context()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	seen := guard.of(flow.ActPush)
	if len(seen) != 1 {
		t.Fatalf("guard saw %d push disclosures, want 1", len(seen))
	}
	for _, want := range []struct {
		fragment string
		origin   flow.Origin
		why      string
	}{
		{"flow/issue-42", flow.OriginFlow, "the SDK constructed the claim branch name"},
		{"new work here", flow.OriginAgent, "an agent wrote the commit message"},
		{"a line only this branch has", flow.OriginWorktree, "the diff is the tree under resolution"},
	} {
		// EVERY part that carries the fragment, not the first one that does.
		// Each of the three is composed end to end by one party, and that is
		// what makes its origin true; a fragment turning up in a second part
		// under another origin means one of them vouches for text it did not
		// compose. A first-match check would pass through exactly that — the
		// patch query carrying the commit messages alongside the diff.
		found := 0
		for _, p := range seen[0].Text {
			if !strings.Contains(p.Body, want.fragment) {
				continue
			}
			found++
			if p.Origin != want.origin {
				t.Errorf("%q is stated %q, want %q — %s", want.fragment, p.Origin, want.origin, want.why)
			}
		}
		if found == 0 {
			t.Errorf("the push disclosure does not carry %q at all", want.fragment)
		}
	}
}

// docs/disclosure.md's own example of an assembled string: artifactFilePath
// templates a filename the agent chose in flow.FileBody.Name, and the commit
// message interpolates the same one. The SDK stands behind its frame, but not
// behind what it was handed, so nobody stands behind the whole string — and
// stating either as `flow` would be the SDK vouching for an agent's text.
func TestArtifactPathCarriesTheAgentFilename(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	ctx := t.Context()
	guard := &recordingGuard{}

	claim, err := b.Claim(ctx, b.refFromIssue(42), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim.ItemRef, []flow.ArtifactSpec{{Id: "plan", Type: flow.ArtifactFile}}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	b.out.guard = guard

	const filename = "an-agent-chose-this.txt"
	resolve := func(content string) {
		t.Helper()
		if err := b.ResolveArtifact(ctx, claim.ItemRef, "plan", flow.ArtifactBody{
			Type: flow.ArtifactFile,
			File: flow.FileBody{Name: filename, Content: []byte(content)},
		}); err != nil {
			t.Fatalf("ResolveArtifact: %v", err)
		}
	}
	// Two routes reach the artifacts branch and both template the filename: the
	// first spill creates the branch through the Git Data API, and every later
	// one goes through the Contents API. A test that drove only the first would
	// leave PutFile's origin unasserted.
	resolve("the bytes")
	afterBranchCreation := len(guard.all())
	resolve("the bytes, again")

	stating := func(t *testing.T, seen []flow.Disclosure, route string) {
		t.Helper()
		found := 0
		for _, d := range seen {
			for _, part := range d.Text {
				if !strings.Contains(part.Body, filename) {
					continue
				}
				found++
				if part.Origin != flow.OriginAgent {
					t.Errorf("%s: %.80q is stated %q, want %q: it interpolates a filename the agent chose",
						route, part.Body, part.Origin, flow.OriginAgent)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s: nothing carried the agent's filename %q at all", route, filename)
		}
	}
	all := guard.all()
	stating(t, all[:afterBranchCreation], "creating the artifacts branch")
	stating(t, all[afterBranchCreation:], "updating a file already on the branch")
}

// The set now lives in the SDK (flow.AllDisclosureActs), so the check above
// enumerates it directly. The property it keeps is the same one the AST parse
// used to keep — every declared name has a call site behind it — and the
// mirror property, that every constant is listed, is asserted in the root
// package's disclosure_test.go, which is where the constants are.

// ---------------------------------------------------------------------------
// What a push would publish.
// ---------------------------------------------------------------------------

func gitInTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func commitFile(t *testing.T, dir, name, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, dir, "add", name)
	gitInTest(t, dir, "commit", "-m", message)
}

// PushMaterial is what the guard is shown before a push, so it has to be right
// against a real repository: a mocked answer would agree with whatever the
// implementation happened to ask for.
func TestPushMaterial(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	origin := t.TempDir()
	gitInTest(t, origin, "init", "--bare", "-b", "main", ".")
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "first\n", "already published")

	g := newGitOps(dir)
	ctx := t.Context()

	// No origin refs at all — the first push publishes the whole branch, and
	// `--not --remotes=origin` excludes nothing, so it is all reported.
	msgs, patch, err := g.PushMaterial(ctx, "main")
	if err != nil {
		t.Fatalf("PushMaterial (no origin): %v", err)
	}
	if !slices.Equal(msgs, []string{"already published"}) {
		t.Errorf("no origin: messages = %v, want the branch's only commit", msgs)
	}
	if !strings.Contains(patch, "first") {
		t.Errorf("no origin: patch does not carry the commit's content:\n%s", patch)
	}

	// Everything on origin — a push publishes nothing new.
	gitInTest(t, dir, "remote", "add", "origin", origin)
	gitInTest(t, dir, "push", "origin", "main")
	msgs, patch, err = g.PushMaterial(ctx, "main")
	if err != nil {
		t.Fatalf("PushMaterial (up to date): %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("up to date: messages = %v, want none", msgs)
	}
	if strings.TrimSpace(patch) != "" {
		t.Errorf("up to date: patch = %q, want empty", patch)
	}

	// Two new commits — exactly those, newest first, and not the one already
	// on origin.
	commitFile(t, dir, "b.txt", "second\n", "unpublished one")
	commitFile(t, dir, "c.txt", "third\n", "unpublished two")
	msgs, patch, err = g.PushMaterial(ctx, "main")
	if err != nil {
		t.Fatalf("PushMaterial (two ahead): %v", err)
	}
	if !slices.Equal(msgs, []string{"unpublished two", "unpublished one"}) {
		t.Errorf("two ahead: messages = %v", msgs)
	}
	for _, want := range []string{"second", "third"} {
		if !strings.Contains(patch, want) {
			t.Errorf("two ahead: patch is missing %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "already published") {
		t.Error("two ahead: patch carries a commit origin already has")
	}
}

// A branch git cannot resolve is an error, not an empty answer. Reporting
// "nothing to disclose" for a repository it could not read is exactly the
// failure docs/disclosure.md calls worse than no guard at all.
func TestPushMaterialFailsRatherThanReportNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "first\n", "only commit")

	msgs, patch, err := newGitOps(dir).PushMaterial(t.Context(), "no-such-branch")
	if err == nil {
		t.Fatalf("PushMaterial on an unknown branch returned (%v, %q, nil)", msgs, patch)
	}
	if msgs != nil || patch != "" {
		t.Errorf("PushMaterial errored but still returned material: %v / %q", msgs, patch)
	}
}

// The claim branch is flow/issue-<n>, and nothing stops a repository from also
// having a tracked path with that name. git then calls the argument ambiguous
// and refuses, so PushMaterial would fail a push that has nothing wrong with
// it — a failure `git push` itself never had.
func TestPushMaterialOnABranchThatIsAlsoAPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	branch := "flow/issue-42"
	gitInTest(t, dir, "init", "-b", "main", ".")
	if err := os.MkdirAll(filepath.Join(dir, "flow"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, dir, filepath.Join("flow", "issue-42"), "a file named like the branch\n", "collide")
	gitInTest(t, dir, "checkout", "-b", branch)
	commitFile(t, dir, "a.txt", "first\n", "on the claim branch")

	msgs, patch, err := newGitOps(dir).PushMaterial(t.Context(), branch)
	if err != nil {
		t.Fatalf("PushMaterial on a branch that is also a path: %v", err)
	}
	if !slices.Contains(msgs, "on the claim branch") {
		t.Errorf("messages = %v, want the branch's commit", msgs)
	}
	if !strings.Contains(patch, "first") {
		t.Errorf("patch does not carry the commit's content:\n%s", patch)
	}
}

// A merge commit's conflict resolution is content git log --patch silently
// omits. Without --diff-merges the guard never sees it, and the push publishes
// bytes nothing examined — the failure docs/disclosure.md calls worse than no
// guard at all.
//
// Against a real repository, for the reason TestPushMaterial is.
func TestPushMaterialIncludesMergeCommitDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	origin := t.TempDir()
	gitInTest(t, origin, "init", "--bare", "-b", "main", ".")
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "original\n", "base")
	gitInTest(t, dir, "remote", "add", "origin", origin)
	gitInTest(t, dir, "push", "origin", "main")

	// A branch with its own change to a.txt.
	gitInTest(t, dir, "checkout", "-b", "topic")
	commitFile(t, dir, "a.txt", "topic-side\n", "topic change")

	// A conflicting change on main.
	gitInTest(t, dir, "checkout", "main")
	commitFile(t, dir, "a.txt", "main-side\n", "main change")
	gitInTest(t, dir, "push", "origin", "main")

	// Merge main into topic, resolve the conflict by hand.
	gitInTest(t, dir, "checkout", "topic")
	// git merge will fail due to conflict — that is expected.
	g := newGitOps(dir)
	g.run(t.Context(), "merge", "main")
	// Resolve with content that appears only in the resolution.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("RESOLVED-SECRET-LINE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, dir, "add", "a.txt")
	gitInTest(t, dir, "commit", "-m", "merge main into topic")

	msgs, patch, err := g.PushMaterial(t.Context(), "topic")
	if err != nil {
		t.Fatalf("PushMaterial after merge: %v", err)
	}
	if !slices.Contains(msgs, "merge main into topic") {
		t.Errorf("messages = %v, want the merge commit's message", msgs)
	}
	if !strings.Contains(patch, "RESOLVED-SECRET-LINE") {
		t.Errorf("patch does not carry the merge's conflict resolution:\n%s", patch)
	}
}

// A merge commit that does not modify anything (a clean merge with no conflict
// resolution) should not cause PushMaterial to fail. The diff for that merge is
// empty, which is fine — there is nothing to disclose.
//
// Against a real repository, for the reason TestPushMaterial is.
func TestPushMaterialCleanMergeDoesNotFail(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	origin := t.TempDir()
	gitInTest(t, origin, "init", "--bare", "-b", "main", ".")
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "original\n", "base")
	gitInTest(t, dir, "remote", "add", "origin", origin)
	gitInTest(t, dir, "push", "origin", "main")

	// A branch that touches a different file.
	gitInTest(t, dir, "checkout", "-b", "topic")
	commitFile(t, dir, "b.txt", "topic-only\n", "topic adds b")

	// main adds a non-conflicting file.
	gitInTest(t, dir, "checkout", "main")
	commitFile(t, dir, "c.txt", "main-only\n", "main adds c")
	gitInTest(t, dir, "push", "origin", "main")

	// Clean merge — no conflict.
	gitInTest(t, dir, "checkout", "topic")
	gitInTest(t, dir, "merge", "main")

	g := newGitOps(dir)
	msgs, patch, err := g.PushMaterial(t.Context(), "topic")
	if err != nil {
		t.Fatalf("PushMaterial after clean merge: %v", err)
	}
	if !slices.Contains(msgs, "topic adds b") {
		t.Errorf("messages = %v, want the topic's own commit", msgs)
	}
	// The topic's own commit should show up in the patch.
	if !strings.Contains(patch, "topic-only") {
		t.Errorf("patch missing topic's own content:\n%s", patch)
	}
}

// The sharpest form of the bug: when a merge commit is the ONLY unpushed
// commit, git log --patch (without --diff-merges) returns an empty diff. The
// guard sees nothing, yet the push publishes the conflict resolution — exactly
// the disclosure gap that must not happen. This test isolates the merge as the
// sole unpushed commit so that any regression empties the patch entirely rather
// than merely omitting one commit among several.
//
// Against a real repository, for the reason TestPushMaterial is.
func TestPushMaterialMergeOnlyCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	origin := t.TempDir()
	gitInTest(t, origin, "init", "--bare", "-b", "main", ".")
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "original\n", "base")
	gitInTest(t, dir, "remote", "add", "origin", origin)
	gitInTest(t, dir, "push", "origin", "main")

	// A branch whose only content is also on main — nothing unpushed yet.
	gitInTest(t, dir, "checkout", "-b", "topic")
	commitFile(t, dir, "a.txt", "topic-side\n", "topic change")
	gitInTest(t, dir, "push", "origin", "topic")

	// A conflicting change on main.
	gitInTest(t, dir, "checkout", "main")
	commitFile(t, dir, "a.txt", "main-side\n", "main change")
	gitInTest(t, dir, "push", "origin", "main")

	// Merge main into topic, resolve by hand. The merge commit is now the
	// ONLY unpushed commit on topic.
	gitInTest(t, dir, "checkout", "topic")
	g := newGitOps(dir)
	g.run(t.Context(), "merge", "main") // conflict expected
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("MERGE-ONLY-SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInTest(t, dir, "add", "a.txt")
	gitInTest(t, dir, "commit", "-m", "merge main into topic")

	msgs, patch, err := g.PushMaterial(t.Context(), "topic")
	if err != nil {
		t.Fatalf("PushMaterial (merge-only): %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("messages is empty — the merge commit's message was lost")
	}
	if !slices.Contains(msgs, "merge main into topic") {
		t.Errorf("messages = %v, want the merge commit", msgs)
	}
	if !strings.Contains(patch, "MERGE-ONLY-SECRET") {
		t.Errorf("patch is missing the conflict resolution (the only unpushed content):\n%s", patch)
	}
}

// A push whose material cannot be computed must not happen. docs/disclosure.md
// fails closed — "a disclosure guard that cannot answer refuses to send" — and
// the material query is what gives this guard something to answer about, so a
// failure there is a guard that cannot answer, not a push with nothing to
// examine.
func TestPushRefusesWhenItCannotSeeWhatWouldBePublished(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	tape := &spawnTape{}
	b.git.runner = func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if slices.Contains(args, "log") {
			return nil, []byte("fatal: bad revision"), errors.New("exit status 128")
		}
		return tape.run(ctx, dir, name, args...)
	}
	// A guard that allows everything it is shown, so a refusal here can only
	// be the seam declining to show it anything.
	shown := 0
	b.out.guard = guardFunc(func(context.Context, flow.Disclosure) error { shown++; return nil })

	if err := b.out.Push(t.Context()); err == nil {
		t.Fatal("Push succeeded although it could not compute what the push would publish")
	}
	if shown != 0 {
		t.Errorf("the guard was consulted %d times about material git refused to produce", shown)
	}
	if spawned := tape.publishing(); len(spawned) > 0 {
		t.Errorf("a push ran although its material could not be computed: %v", spawned)
	}
}

// The branch name is the other thing a push needs before it can say what it
// would publish, and git can decline to produce that too — a detached HEAD, a
// repository the runner is not in. It is a separate early return from the one
// above and the same rule: a seam that cannot describe the write does not make
// it, and does not ask a guard to bless a description it does not have.
func TestPushRefusesWhenItCannotNameTheBranch(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	tape := &spawnTape{}
	b.git.runner = func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if slices.Contains(args, "rev-parse") {
			return nil, []byte("fatal: ambiguous argument 'HEAD'"), errors.New("exit status 128")
		}
		return tape.run(ctx, dir, name, args...)
	}
	shown := 0
	b.out.guard = guardFunc(func(context.Context, flow.Disclosure) error { shown++; return nil })

	if err := b.out.Push(t.Context()); err == nil {
		t.Fatal("Push succeeded although git would not name the branch it would publish")
	}
	if shown != 0 {
		t.Errorf("the guard was consulted %d times about a push whose branch git would not name", shown)
	}
	if spawned := tape.publishing(); len(spawned) > 0 {
		t.Errorf("a push ran although its branch could not be named: %v", spawned)
	}
}

// A refused push must not push. Pairs with the refusal table above by driving
// it through the worktree, which is how the SDK reaches it.
func TestRefusedPushDoesNotPush(t *testing.T) {
	b, _, tape := newSeamBackend(t)
	b.out.guard = refusing()

	w := &worktree{b: b, issueNum: 42}
	if err := w.Push(t.Context()); !errors.Is(err, errGuardRefused) {
		t.Errorf("worktree.Push under a refusing guard: err = %v, want the refusal", err)
	}
	if spawned := tape.publishing(); len(spawned) > 0 {
		t.Errorf("a refused push still ran %v", spawned)
	}
}

// PushMaterial's answer is only worth computing if it reaches the guard. A Push
// that showed the branch name alone would satisfy every refusal test above
// while publishing commit messages and a diff nothing examined — and
// docs/disclosure.md puts both on the surface, calling them the ones most
// easily forgotten because they reach the public through git.
//
// Against a real repository, for the reason TestPushMaterial is: a mocked
// answer would agree with whatever the implementation happened to ask for.
func TestGuardSeesTheCommitsAndDiffAPushWouldPublish(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, origin := t.TempDir(), t.TempDir()
	gitInTest(t, origin, "init", "--bare", "-b", "main", ".")
	gitInTest(t, dir, "init", "-b", "main", ".")
	commitFile(t, dir, "a.txt", "old content\n", "on origin already")
	gitInTest(t, dir, "remote", "add", "origin", origin)
	gitInTest(t, dir, "push", "origin", "main")
	gitInTest(t, dir, "checkout", "-b", "flow/issue-42")
	commitFile(t, dir, "b.txt", "a line only this branch has\n", "new work here")

	// Everything but the push itself runs for real; the push is the one command
	// that would leave the machine.
	g := newGitOps(dir)
	spawn := g.runner
	pushes := 0
	g.runner = func(ctx context.Context, wd, name string, args ...string) ([]byte, []byte, error) {
		if slices.Contains(args, "push") {
			pushes++
			return nil, nil, nil
		}
		return spawn(ctx, wd, name, args...)
	}

	guard := &recordingGuard{}
	pushesWhenAsked := -1
	o := &outward{git: g, owner: "o", repo: "r"}
	o.guard = guardFunc(func(ctx context.Context, d flow.Disclosure) error {
		pushesWhenAsked = pushes
		return guard.Examine(ctx, d)
	})
	if err := o.Push(t.Context()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	seen := guard.of(flow.ActPush)
	if len(seen) != 1 {
		t.Fatalf("guard saw %d push disclosures, want 1", len(seen))
	}
	d := seen[0]
	if d.Ref != "flow/issue-42" {
		t.Errorf("push disclosure ref = %q, want the branch being published", d.Ref)
	}
	if !carries(d, "flow/issue-42") {
		t.Errorf("the push disclosure does not name the branch it would create: %q", bodies(d))
	}
	if !carries(d, "new work here") {
		t.Errorf("the push disclosure does not carry the commit message it would publish: %q", bodies(d))
	}
	if !carries(d, "a line only this branch has") {
		t.Errorf("the push disclosure does not carry the diff it would publish: %q", bodies(d))
	}
	if carries(d, "on origin already") {
		t.Errorf("the push disclosure carries a commit origin already has: %q", bodies(d))
	}
	if pushesWhenAsked != 0 {
		t.Errorf("the guard was asked after %d pushes had already run", pushesWhenAsked)
	}
	if pushes != 1 {
		t.Errorf("an allowed push ran %d times, want 1", pushes)
	}
}

// ---------------------------------------------------------------------------
// No second route.
// ---------------------------------------------------------------------------
// secondRouteViolations parses one file and reports every place it could reach
// GitHub without passing the seam, as "<position>: <what>".
//
// The client check follows the IMPORT rather than the identifier `github`:
// aliasing the library is a one-word edit, and a check that only knows the
// default name is one a future author defeats without meaning to. The command
// check looks at composite literals as well as call arguments, because
// `args := []string{"gh", ...}` spawns gh exactly as well as
// `runner(ctx, "", "gh", args...)` does. Both are positions a string can reach
// a process from; `p["push"]`, a permission map key, is not one.
//
// Separate from the test that walks the package because the package is clean:
// against real files every report below is dead code, and a check whose
// reporting path never runs is one that can stop working in silence.
// TestSecondRouteCheckFindsTheRoutesItNames is what runs it.
func secondRouteViolations(t *testing.T, name string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var found []string
	report := func(pos token.Pos, format string, args ...any) {
		found = append(found, fset.Position(pos).String()+": "+fmt.Sprintf(format, args...))
	}

	// Whatever local name this file gave the client library, if any. The files
	// that legitimately import it do so for option structs and github.Ptr,
	// which is why the import itself is not the violation.
	clientPkg := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.HasPrefix(path, "github.com/google/go-github/") {
			continue
		}
		local := "github" // the library's own package name
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." {
			report(imp.Pos(), "%s dot-imports the client library, which puts NewClient in scope "+
				"unqualified and out of this check's reach", name)
			continue
		}
		clientPkg[local] = true
	}

	// commands flags any of exprs that is the string "gh" or "push".
	commands := func(exprs []ast.Expr) {
		for _, e := range exprs {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				e = kv.Value // a key is never an argument
			}
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || (v != "gh" && v != "push") {
				continue
			}
			report(lit.Pos(), "%s reaches GitHub with %q outside the seam; "+
				"add a method to outward.go instead", name, v)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			pkg, ok := node.X.(*ast.Ident)
			if !ok || !clientPkg[pkg.Name] {
				return true
			}
			// The type itself, and every constructor of one: go-github names
			// them all New* (NewClient, NewTokenClient, NewEnterpriseClient,
			// NewClientWithEnvProxy).
			if node.Sel.Name == "Client" || strings.HasPrefix(node.Sel.Name, "New") {
				report(node.Pos(), "%s holds a second GitHub client (%s.%s); the only one lives in outward.go",
					name, pkg.Name, node.Sel.Name)
			}
		case *ast.CallExpr:
			commands(node.Args)
		case *ast.CompositeLit:
			commands(node.Elts)
		}
		return true
	})
	return found
}

// The seam is a guarantee only while it is the ONLY way out. This fails any
// file in the package that builds a GitHub client of its own, or that names
// `gh` or `push` in a string literal.
//
// _test.go is the one exemption: newMockedOrchestrator builds a client on purpose,
// and the tables above name the very commands this forbids elsewhere.
func TestNoSecondRouteToGitHub(t *testing.T) {
	for _, name := range packageSourceFiles(t) {
		if name == "outward.go" {
			continue
		}
		for _, v := range secondRouteViolations(t, name, nil) {
			t.Error(v)
		}
	}
}

// What the check above is worth depends entirely on it still detecting, and
// against a clean package it reports nothing whether it works or not. These are
// files that DO reach GitHub, so a check that stopped seeing one fails here
// rather than passing quietly forever.
//
// The last case is the other half: the package really does import the client
// library for option structs and github.Ptr, and really does keep a permission
// map keyed "push". A check that flagged those would be turned off within a
// week, which is the same outcome as not having one.
func TestSecondRouteCheckFindsTheRoutesItNames(t *testing.T) {
	const lib = "github.com/google/go-github/v68/github"
	for _, tc := range []struct {
		name string
		src  string
		want string // substring the report must contain; "" means no report
	}{{
		name: "a client under the library's own name",
		src: `package github
import "` + lib + `"
func f(token string) { _ = github.NewClient(nil).WithAuthToken(token) }`,
		want: "second GitHub client",
	}, {
		name: "a client under an alias",
		src: `package github
import gh "` + lib + `"
func f(token string) { _ = gh.NewClient(nil).WithAuthToken(token) }`,
		want: "second GitHub client",
	}, {
		name: "the client type held in a field",
		src: `package github
import "` + lib + `"
type backend struct{ client *github.Client }`,
		want: "second GitHub client",
	}, {
		name: "the library dot-imported",
		src: `package github
import . "` + lib + `"
func f() { _ = NewClient(nil) }`,
		want: "dot-imports",
	}, {
		name: "gh spawned as a call argument",
		src: `package github
import "context"
func f(ctx context.Context, run func(context.Context, string, string, ...string) error) {
	_ = run(ctx, "", "gh", "pr", "create")
}`,
		want: `with "gh"`,
	}, {
		name: "gh spawned from an argument slice",
		src: `package github
func f(base string) []string { return []string{"gh", "pr", "create", "--base", base} }`,
		want: `with "gh"`,
	}, {
		name: "a push run outside the seam",
		src: `package github
import "context"
func f(ctx context.Context, run func(context.Context, ...string) error, branch string) {
	_ = run(ctx, "push", "-u", "origin", branch)
}`,
		want: `with "push"`,
	}, {
		name: "the library used for what files may legitimately use it for",
		src: `package github
import "` + lib + `"
func f(perms map[string]bool) *github.IssueComment {
	_ = map[string]bool{"push": true, "pull": true}
	_ = perms["push"]
	return &github.IssueComment{Body: github.Ptr("hi")}
}`,
		want: "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := secondRouteViolations(t, "subject.go", tc.src)
			if tc.want == "" {
				if len(got) > 0 {
					t.Errorf("clean file reported %v", got)
				}
				return
			}
			if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, tc.want) }) {
				t.Errorf("reported %v, want one mentioning %q", got, tc.want)
			}
		})
	}
}
