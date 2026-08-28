package github

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// spawnTape records every process the backend would have started, and answers
// plausibly enough for the callers that read the output.
type spawnTape struct {
	mu    sync.Mutex
	calls [][]string // name followed by its arguments
}

func (s *spawnTape) run(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string{name}, args...))
	s.mu.Unlock()
	switch {
	case name == "gh":
		return []byte("https://github.com/o/r/pull/1\n"), nil, nil
	case slices.Contains(args, "rev-parse"):
		return []byte("flow/issue-42\n"), nil, nil
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

func (s *spawnTape) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

// newSeamBackend wires a Backend at the mock server with every process spawn
// intercepted, so a test can watch both halves of the boundary at once.
func newSeamBackend(t *testing.T) (*Backend, *ghMock, *spawnTape) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedBackend(t, mock, srv)
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
			_, err := o.CreateComment(ctx, actArtifactComment, 42, "a body")
			return err
		},
		"EditComment": func(ctx context.Context, o *outward) error {
			return o.EditComment(ctx, actStateComment, 42, seedCommentID, "a body")
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
	}
}

// outwardReads names the methods on *outward that only read. Every exported
// method must be in exactly one of these two sets.
var outwardReads = map[string]bool{
	"SearchIssues":     true,
	"GetIssue":         true,
	"GetComment":       true,
	"ListCommentsPage": true,
	"GetRepo":          true,
	"DownloadContents": true,
	"GetContents":      true,
	"GetRef":           true,
	"ListPullRequests": true,
	"ListReviews":      true,
}

// The load-bearing test. docs/disclosure.md requires the guard to be
// unavoidable — "a guard that is consulted is a convention; a guard that
// cannot be gone around is a guarantee" — so this drives EVERY writing method
// with a refusing guard and asserts the mock server saw no mutating request
// and no publishing process was spawned.
//
// Each method is driven twice. Once with no guard, to prove the drive actually
// reaches GitHub — without that, a method that errored on its arguments would
// satisfy the refusal assertion vacuously. Then once refusing, where nothing
// may leave.
func TestEveryOutwardWriteIsRefusable(t *testing.T) {
	writes := outwardWrites()
	for name, drive := range writes {
		t.Run(name, func(t *testing.T) {
			b, mock, tape := newSeamBackend(t)

			// Allowed: something must actually go out.
			if err := drive(t.Context(), b.out); err != nil {
				t.Fatalf("%s with no guard: %v", name, err)
			}
			mock.mu.Lock()
			sent := len(mock.mutations)
			mock.mu.Unlock()
			if sent == 0 && len(tape.publishing()) == 0 {
				t.Fatalf("%s sent nothing even unguarded — the drive does not reach GitHub, "+
					"so the refusal below would prove nothing", name)
			}

			// Refused: nothing may.
			mock.mu.Lock()
			mock.mutations = nil
			mock.mu.Unlock()
			tape.reset()
			b.out.guard = func(context.Context, disclosure) error { return errGuardRefused }

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

// The seam is only one seam if every write is on it. Reflection closes the set:
// a method added to *outward that is in neither table fails here until it is
// classified, and classifying it as a write puts it under the refusal test
// above.
func TestOutwardMethodsAreAllClassified(t *testing.T) {
	writes := outwardWrites()
	typ := reflect.TypeOf(&outward{})
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		_, isWrite := writes[name]
		switch {
		case isWrite && outwardReads[name]:
			t.Errorf("outward.%s is listed as both a read and a write", name)
		case !isWrite && !outwardReads[name]:
			t.Errorf("outward.%s is neither driven by outwardWrites nor listed in outwardReads; "+
				"if it writes, add it to outwardWrites so TestEveryOutwardWriteIsRefusable covers it", name)
		}
	}
}

// recordingGuard captures what the seam was shown, and allows.
type recordingGuard struct {
	mu   sync.Mutex
	seen []disclosure
}

func (g *recordingGuard) fn(_ context.Context, d disclosure) error {
	g.mu.Lock()
	g.seen = append(g.seen, d)
	g.mu.Unlock()
	return nil
}

func (g *recordingGuard) of(a act) []disclosure {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []disclosure
	for _, d := range g.seen {
		if d.act == a {
			out = append(out, d)
		}
	}
	return out
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

	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", false)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{{Id: "plan", Type: flow.ArtifactMarkdown}}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	guard := &recordingGuard{}
	b.out.guard = guard.fn

	prose := strings.Repeat("plan prose. ", 900) // comfortably over 4KiB
	if err := b.ResolveArtifact(ctx, claim, "plan", flow.ArtifactBody{
		Type:     flow.ArtifactMarkdown,
		Markdown: prose,
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	comments := guard.of(actArtifactComment)
	if len(comments) != 1 {
		t.Fatalf("guard saw %d artifact comments, want 1", len(comments))
	}
	body := comments[0].text[0]
	if !strings.HasPrefix(body, artifactCommentMarkerPrefix) {
		t.Errorf("guard was shown the prose, not the assembled comment: %.60q", body)
	}
	if !strings.Contains(body, spillNoticePrefix) {
		t.Error("guard was not shown the spill notice, which is text the SDK appended after assembly")
	}
	if strings.Contains(body, prose) {
		t.Error("guard was shown the whole artifact; the published comment carries a truncated preview")
	}
	if comments[0].issue != 42 {
		t.Errorf("disclosure names issue %d, want 42", comments[0].issue)
	}

	// The spilled bytes are their own disclosure — the comment holds only a
	// preview, so a guard that saw the comment alone would never see the rest.
	files := guard.of(actArtifactFile)
	if len(files) == 0 {
		t.Fatal("the spill to the artifacts branch never reached the guard")
	}
	if !slices.ContainsFunc(files, func(d disclosure) bool {
		return slices.Contains(d.text, prose)
	}) {
		t.Error("the spill disclosure does not carry the artifact's bytes")
	}
}

// "The labels are the case that looks exempt and is not: the flow constructs
// label names, and a constructed name is text the flow chose to publish."
func TestGuardSeesConstructedLabelNames(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	ctx := t.Context()

	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", false)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{{Id: "implement", Type: flow.ArtifactMarkdown}}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	guard := &recordingGuard{}
	b.out.guard = guard.fn

	if err := b.Park(ctx, claim, flow.ParkRequest{
		Kind:   flow.ParkBudgetExhausted,
		Step:   "implement",
		Reason: "spent the budget",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	want := b.labels.BudgetExhausted("implement")
	if !slices.ContainsFunc(guard.of(actLabel), func(d disclosure) bool {
		return slices.Contains(d.text, want)
	}) {
		t.Errorf("guard never saw the constructed label %q; saw %v", want, guard.of(actLabel))
	}
	if len(guard.of(actParkRecord)) != 1 {
		t.Errorf("guard saw %d park records, want 1", len(guard.of(actParkRecord)))
	}
}

// A refusal must reach the caller rather than being absorbed on the way out.
func TestRefusedWriteFailsTheBackendCall(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()

	b.out.guard = func(context.Context, disclosure) error { return errGuardRefused }
	_, err := b.Claim(ctx, b.refFromIssue(42), "alice", false)
	if !errors.Is(err, errGuardRefused) {
		t.Errorf("Claim under a refusing guard: err = %v, want the refusal", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.mutations) > 0 {
		t.Errorf("Claim wrote to GitHub despite a refusing guard: %v", mock.mutations)
	}
}

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
	b.out.guard = func(context.Context, disclosure) error { shown++; return nil }

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

// A refused push must not push. Pairs with the refusal table above by driving
// it through the worktree, which is how the SDK reaches it.
func TestRefusedPushDoesNotPush(t *testing.T) {
	b, _, tape := newSeamBackend(t)
	b.out.guard = func(context.Context, disclosure) error { return errGuardRefused }

	w := &worktree{b: b, issueNum: 42}
	if err := w.Push(t.Context()); !errors.Is(err, errGuardRefused) {
		t.Errorf("worktree.Push under a refusing guard: err = %v, want the refusal", err)
	}
	if spawned := tape.publishing(); len(spawned) > 0 {
		t.Errorf("a refused push still ran %v", spawned)
	}
}

// ---------------------------------------------------------------------------
// No second route.
// ---------------------------------------------------------------------------

// The seam is a guarantee only while it is the ONLY way out. This fails any
// file in the package that builds a GitHub client of its own, or that names
// `gh` or `push` in a string literal.
//
// The client check follows the IMPORT rather than the identifier `github`:
// aliasing the library is a one-word edit, and a check that only knows the
// default name is one a future author defeats without meaning to. The command
// check looks at composite literals as well as call arguments, because
// `args := []string{"gh", ...}` spawns gh exactly as well as
// `runner(ctx, "", "gh", args...)` does. Both are positions a string can reach
// a process from; `p["push"]`, a permission map key, is not one.
//
// _test.go is the one exemption: newMockedBackend builds a client on purpose,
// and the tables above name the very commands this forbids elsewhere.
func TestNoSecondRouteToGitHub(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "outward.go" {
			continue
		}
		checked++
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// Whatever local name this file gave the client library, if any. The
		// files that legitimately import it do so for option structs and
		// github.Ptr, which is why the import itself is not the violation.
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
				t.Errorf("%s: %s dot-imports the client library, which puts NewClient in scope "+
					"unqualified and out of this check's reach", fset.Position(imp.Pos()), name)
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
				t.Errorf("%s: %s reaches GitHub with %q outside the seam; "+
					"add a method to outward.go instead", fset.Position(lit.Pos()), name, v)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				pkg, ok := node.X.(*ast.Ident)
				if !ok || !clientPkg[pkg.Name] {
					return true
				}
				// The type itself, and every constructor of one: go-github
				// names them all New* (NewClient, NewTokenClient,
				// NewEnterpriseClient, NewClientWithEnvProxy).
				if node.Sel.Name == "Client" || strings.HasPrefix(node.Sel.Name, "New") {
					t.Errorf("%s: %s holds a second GitHub client (%s.%s); the only one lives in outward.go",
						fset.Position(node.Pos()), name, pkg.Name, node.Sel.Name)
				}
			case *ast.CallExpr:
				commands(node.Args)
			case *ast.CompositeLit:
				commands(node.Elts)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("scanned no files — the check is not running")
	}
}
