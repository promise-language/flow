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

// docs/disclosure.md: "It never modifies what it examines." The label and
// assignee writes are the ones that hand the guard a slice rather than a
// string. If it were the SAME slice the API call carries, a guard could rewrite
// the bytes after inspecting them — and then what was examined and what was
// published are two different texts, with nothing to say so.
func TestTheGuardCannotAlterWhatIsSent(t *testing.T) {
	b, mock, _ := newSeamBackend(t)
	ctx := t.Context()
	b.out.guard = func(_ context.Context, d disclosure) error {
		for i := range d.text {
			d.text[i] = "rewritten-after-inspection"
		}
		return nil
	}

	if err := b.out.AddLabels(ctx, 42, []string{"flow:owner:alice"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if err := b.out.AddAssignees(ctx, 42, []string{"alice"}); err != nil {
		t.Fatalf("AddAssignees: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
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
	b.out.guard = guard.fn

	if _, err := b.out.CreateComment(t.Context(), actArtifactComment, 42, "a body"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	seen := guard.of(actArtifactComment)
	if len(seen) != 1 {
		t.Fatalf("guard saw %d disclosures, want 1", len(seen))
	}
	if seen[0].owner != "o" || seen[0].repo != "r" {
		t.Errorf("disclosure names %q/%q, want the repository the backend writes to",
			seen[0].owner, seen[0].repo)
	}
}

// The act vocabulary is closed, and a name in it is worth nothing unless some
// call site produces it carrying that write's final bytes. The kinds asserted
// above — the artifact comment, the spilled bytes, the constructed label, the
// park record — are not repeated here; this drives the rest of a resolution and
// fails when a declared act has no call site behind it, which is what a write
// naming the wrong act looks like from outside.
func TestEveryActReachesTheGuardWithItsFinalBytes(t *testing.T) {
	b, _, _ := newSeamBackend(t)
	ctx := t.Context()
	guard := &recordingGuard{}
	b.out.guard = guard.fn

	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", false)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactFile, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	// A file artifact always spills, so this reaches the artifacts branch too.
	if err := b.ResolveArtifact(ctx, claim, "plan", flow.ArtifactBody{
		Type: flow.ArtifactFile,
		File: flow.FileBody{Name: "notes.txt", Content: []byte("the spilled bytes")},
	}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	if _, err := b.AskQuestions(ctx, claim, []flow.AgentQuestion{
		{Header: "Which base branch?", Text: "main, or the release branch?"},
	}); err != nil {
		t.Fatalf("AskQuestions: %v", err)
	}
	if err := b.Park(ctx, claim, flow.ParkRequest{
		Kind: flow.ParkQuestion, Step: "plan", Reason: "waiting on the base branch",
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	w := &worktree{b: b, claim: claim, issueNum: 42}
	prURL, err := w.Open(ctx, "main", "a pull request title", "a pull request body")
	if err != nil {
		t.Fatalf("worktree.Open: %v", err)
	}
	if err := w.Merge(ctx, prURL); err != nil {
		t.Fatalf("worktree.Merge: %v", err)
	}

	for _, a := range declaredActs(t) {
		if len(guard.of(a)) == 0 {
			t.Errorf("nothing produced the act %q: either no call site names it, "+
				"or the one that should names another", a)
		}
	}

	for _, want := range []struct {
		act       act
		fragments []string
	}{
		{actAssignee, []string{"alice"}},
		{actStateComment, []string{"flow:state-v1 begin"}},
		{actQuestion, []string{"<!-- flow:question", "Which base branch?", "main, or the release branch?"}},
		{actPullRequest, []string{"a pull request title", "a pull request body", "main", "flow/issue-42"}},
		{actMerge, []string{prURL}},
	} {
		seen := guard.of(want.act)
		for _, fragment := range want.fragments {
			if !slices.ContainsFunc(seen, func(d disclosure) bool {
				return slices.ContainsFunc(d.text, func(s string) bool {
					return strings.Contains(s, fragment)
				})
			}) {
				t.Errorf("no %q disclosure carries %q; saw %v", want.act, fragment, seen)
			}
		}
	}

	// The pull request publishes a branch, and the disclosure has to say which.
	if pr := guard.of(actPullRequest); len(pr) != 1 || pr[0].ref != "flow/issue-42" {
		t.Errorf("pull-request disclosure ref = %v, want the head branch it opens from", pr)
	}
}

// declaredActs reads the act vocabulary out of outward.go. Hand-listing it in
// the test would let an act be added that nothing ever produces; reading the
// source keeps the set closed in the direction that matters — every name
// declared has a call site behind it.
func declaredActs(t *testing.T) []act {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "outward.go", nil, 0)
	if err != nil {
		t.Fatalf("parse outward.go: %v", err)
	}
	var out []act
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) != 1 {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "act" {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(lit.Pos()), err)
			}
			out = append(out, act(v))
		}
	}
	if len(out) == 0 {
		t.Fatal("found no act constants — the check is not running")
	}
	// Two names for one string are one act: the guard cannot tell the writes
	// apart, and a refusal cannot say which of them it refused.
	seen := map[act]bool{}
	for _, a := range out {
		if seen[a] {
			t.Errorf("the act %q is declared twice", a)
		}
		seen[a] = true
	}
	return out
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
	o.guard = func(ctx context.Context, d disclosure) error {
		pushesWhenAsked = pushes
		return guard.fn(ctx, d)
	}
	if err := o.Push(t.Context()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	seen := guard.of(actPush)
	if len(seen) != 1 {
		t.Fatalf("guard saw %d push disclosures, want 1", len(seen))
	}
	d := seen[0]
	if d.ref != "flow/issue-42" {
		t.Errorf("push disclosure ref = %q, want the branch being published", d.ref)
	}
	carries := func(fragment string) bool {
		return slices.ContainsFunc(d.text, func(s string) bool { return strings.Contains(s, fragment) })
	}
	if !carries("flow/issue-42") {
		t.Errorf("the push disclosure does not name the branch it would create: %q", d.text)
	}
	if !carries("new work here") {
		t.Errorf("the push disclosure does not carry the commit message it would publish: %q", d.text)
	}
	if !carries("a line only this branch has") {
		t.Errorf("the push disclosure does not carry the diff it would publish: %q", d.text)
	}
	if carries("on origin already") {
		t.Errorf("the push disclosure carries a commit origin already has: %q", d.text)
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
// _test.go is the one exemption: newMockedBackend builds a client on purpose,
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
