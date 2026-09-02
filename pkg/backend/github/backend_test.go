package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// ghMock is a tiny httptest.Server that emulates the GitHub REST API
// surface the backend uses. State is kept in memory; concurrent-safe.
type ghMock struct {
	t  *testing.T
	mu sync.Mutex

	owner string
	repo  string

	// issue state
	issueNum    int
	issueTitle  string
	issueBody   string
	issueLabels []string
	assignees   []string

	// comments
	nextCommentID int64
	comments      []ghMockComment

	// permissions (for Doctor)
	perms map[string]bool

	// orphan branch state for the artifacts spillover
	orphanBranchSHA string                // commit SHA at heads/flow-artifacts
	orphanFiles     map[string]ghMockFile // path → file
	nextBlobID      int
	nextTreeID      int
	nextCommitID    int

	// observation tape: callers can inspect putArtifactFile interactions.
	branchCreated    bool
	branchCreateCall string // path of first file committed when creating
	updateCalls      int

	// mutations records every non-GET request the server received, whether or
	// not a route existed for it. Recorded before routing on purpose: a write
	// that reached the network is a disclosure even when GitHub rejects it.
	mutations []string
}

// recordMutations counts the requests that change something, so a test can
// assert that a refused disclosure sent nothing at all.
func (m *ghMock) recordMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			m.mu.Lock()
			m.mutations = append(m.mutations, r.Method+" "+r.URL.Path)
			m.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

type ghMockFile struct {
	Content []byte
	SHA     string
}

type ghMockComment struct {
	ID   int64
	Body string
	User string
}

func newGHMock(t *testing.T) *ghMock {
	return &ghMock{
		t:             t,
		owner:         "o",
		repo:          "r",
		issueNum:      42,
		issueTitle:    "Test issue",
		issueBody:     "Add hello()",
		nextCommentID: 1000,
		perms:         map[string]bool{"push": true, "pull": true, "admin": false},
		orphanFiles:   map[string]ghMockFile{},
	}
}

func (m *ghMock) server() *httptest.Server {
	mux := http.NewServeMux()

	prefix := fmt.Sprintf("/repos/%s/%s", m.owner, m.repo)

	// GET /repos/{o}/{r}
	mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"name":           m.repo,
			"full_name":      m.owner + "/" + m.repo,
			"permissions":    m.perms,
			"default_branch": "main",
		})
	})

	// GET/PATCH /repos/{o}/{r}/issues/{n}
	mux.HandleFunc(prefix+"/issues/", func(w http.ResponseWriter, r *http.Request) {
		// Sub-paths: /issues/{n}/labels, /issues/{n}/assignees, /issues/{n}/comments, /issues/comments/{id}
		path := strings.TrimPrefix(r.URL.Path, prefix+"/issues/")

		if strings.HasPrefix(path, "comments/") { // single-comment endpoint
			m.handleSingleComment(w, r, strings.TrimPrefix(path, "comments/"))
			return
		}
		parts := strings.SplitN(path, "/", 2)
		issueNum := parts[0]
		if issueNum != fmt.Sprintf("%d", m.issueNum) {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 1 {
			m.handleIssue(w, r)
			return
		}
		switch parts[1] {
		case "comments":
			m.handleIssueComments(w, r)
		case "labels":
			m.handleIssueLabels(w, r)
		case "assignees":
			m.handleIssueAssignees(w, r)
		default:
			// labels/<name> for DELETE
			if strings.HasPrefix(parts[1], "labels/") {
				m.handleRemoveLabel(w, r, strings.TrimPrefix(parts[1], "labels/"))
				return
			}
			http.NotFound(w, r)
		}
	})

	// GET /repos/{o}/{r}/pulls
	mux.HandleFunc(prefix+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		// No PRs in default mock state.
		writeJSON(w, []any{})
	})

	// git data API — used for orphan-branch creation.
	// GET uses /git/ref/heads/<branch> (singular); CreateRef POSTs to
	// /git/refs (plural).
	mux.HandleFunc(prefix+"/git/ref/heads/"+artifactsBranch, m.handleGitRefRead)
	mux.HandleFunc(prefix+"/git/refs", m.handleGitRefCreate)
	mux.HandleFunc(prefix+"/git/blobs", m.handleGitBlobCreate)
	mux.HandleFunc(prefix+"/git/trees", m.handleGitTreeCreate)
	mux.HandleFunc(prefix+"/git/commits", m.handleGitCommitCreate)

	// contents API — used for orphan-branch updates after creation. Catches
	// paths under /contents/... by registering a prefix handler.
	mux.HandleFunc(prefix+"/contents/", m.handleContents)

	// GET /search/issues
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"total_count": 1,
			"items": []map[string]any{
				{"number": m.issueNum, "title": m.issueTitle, "user": map[string]string{"login": "alice"}},
			},
		})
	})

	// GET /user
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"login": "alice"})
	})

	return httptest.NewServer(m.recordMutations(mux))
}

func (m *ghMock) handleIssue(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writeJSON(w, map[string]any{
		"number":     m.issueNum,
		"title":      m.issueTitle,
		"body":       m.issueBody,
		"html_url":   fmt.Sprintf("https://github.com/%s/%s/issues/%d", m.owner, m.repo, m.issueNum),
		"labels":     toLabelObjs(m.issueLabels),
		"assignees":  toLoginObjs(m.assignees),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func (m *ghMock) handleIssueComments(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method == http.MethodPost {
		var doc struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		m.nextCommentID++
		c := ghMockComment{ID: m.nextCommentID, Body: doc.Body, User: "alice"}
		m.comments = append(m.comments, c)
		writeJSON(w, ghCommentJSON(c))
		return
	}
	// GET — list, newest-first by default
	out := make([]map[string]any, 0, len(m.comments))
	for i := len(m.comments) - 1; i >= 0; i-- {
		out = append(out, ghCommentJSON(m.comments[i]))
	}
	writeJSON(w, out)
}

func (m *ghMock) handleSingleComment(w http.ResponseWriter, r *http.Request, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.comments {
		if fmt.Sprintf("%d", m.comments[i].ID) == id {
			if r.Method == http.MethodPatch {
				var doc struct {
					Body string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&doc); err == nil {
					m.comments[i].Body = doc.Body
				}
			}
			writeJSON(w, ghCommentJSON(m.comments[i]))
			return
		}
	}
	http.NotFound(w, r)
}

func (m *ghMock) handleIssueLabels(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		// go-github sends a raw []string body for AddLabelsToIssue; the
		// "labels" wrapping form is also accepted by GitHub. Try both.
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err != nil {
			var wrapper struct {
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(raw, &wrapper); err == nil {
				arr = wrapper.Labels
			}
		}
		m.issueLabels = append(m.issueLabels, arr...)
		writeJSON(w, toLabelObjs(m.issueLabels))
	case http.MethodGet:
		writeJSON(w, toLabelObjs(m.issueLabels))
	default:
		http.Error(w, "bad method", 405)
	}
}

func (m *ghMock) handleRemoveLabel(w http.ResponseWriter, r *http.Request, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.issueLabels[:0]
	for _, lbl := range m.issueLabels {
		if lbl != name {
			out = append(out, lbl)
		}
	}
	m.issueLabels = out
	writeJSON(w, toLabelObjs(m.issueLabels))
}

func (m *ghMock) handleIssueAssignees(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method == http.MethodPost {
		var add struct {
			Assignees []string `json:"assignees"`
		}
		if err := json.NewDecoder(r.Body).Decode(&add); err == nil {
			m.assignees = append(m.assignees, add.Assignees...)
		}
	}
	if r.Method == http.MethodDelete {
		var del struct {
			Assignees []string `json:"assignees"`
		}
		if err := json.NewDecoder(r.Body).Decode(&del); err == nil {
			out := m.assignees[:0]
			for _, a := range m.assignees {
				keep := true
				for _, d := range del.Assignees {
					if a == d {
						keep = false
						break
					}
				}
				if keep {
					out = append(out, a)
				}
			}
			m.assignees = out
		}
	}
	writeJSON(w, map[string]any{
		"number":    m.issueNum,
		"assignees": toLoginObjs(m.assignees),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func toLabelObjs(names []string) []map[string]string {
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	return out
}

func toLoginObjs(logins []string) []map[string]string {
	out := make([]map[string]string, 0, len(logins))
	for _, l := range logins {
		out = append(out, map[string]string{"login": l})
	}
	return out
}

// --- orphan-branch / contents API handlers ---

func (m *ghMock) handleGitRefRead(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.orphanBranchSHA == "" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{
		"ref": "refs/heads/" + artifactsBranch,
		"object": map[string]string{
			"type": "commit",
			"sha":  m.orphanBranchSHA,
		},
	})
}

func (m *ghMock) handleGitRefCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "bad method", 405)
		return
	}
	var req struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.Ref == "refs/heads/"+artifactsBranch {
		m.orphanBranchSHA = req.SHA
	}
	writeJSON(w, map[string]any{
		"ref":    req.Ref,
		"object": map[string]string{"type": "commit", "sha": req.SHA},
	})
}

func (m *ghMock) handleGitBlobCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "bad method", 405)
		return
	}
	var req struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextBlobID++
	sha := fmt.Sprintf("blob%06d", m.nextBlobID)
	writeJSON(w, map[string]string{"sha": sha})
}

func (m *ghMock) handleGitTreeCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "bad method", 405)
		return
	}
	var req struct {
		Tree []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextTreeID++
	sha := fmt.Sprintf("tree%06d", m.nextTreeID)
	// Record the file in our virtual filesystem under each tree-entry path.
	for _, e := range req.Tree {
		m.orphanFiles[e.Path] = ghMockFile{SHA: e.SHA}
	}
	writeJSON(w, map[string]string{"sha": sha})
}

func (m *ghMock) handleGitCommitCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "bad method", 405)
		return
	}
	var req struct {
		Tree string `json:"tree"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextCommitID++
	sha := fmt.Sprintf("commit%06d", m.nextCommitID)
	if m.orphanBranchSHA == "" {
		m.branchCreated = true
	}
	writeJSON(w, map[string]string{"sha": sha})
}

// handleContents serves GET (get-contents) and PUT (create/update file)
// against /repos/o/r/contents/<path>.
func (m *ghMock) handleContents(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/contents/", m.owner, m.repo))
	switch r.Method {
	case http.MethodGet:
		m.mu.Lock()
		file, ok := m.orphanFiles[path]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"name":         path,
			"path":         path,
			"sha":          file.SHA,
			"type":         "file",
			"size":         len(file.Content),
			"download_url": "https://raw.githubusercontent.com/" + m.owner + "/" + m.repo + "/" + artifactsBranch + "/" + path,
		})
	case http.MethodPut:
		var req struct {
			Message string `json:"message"`
			Content string `json:"content"` // base64
			SHA     string `json:"sha,omitempty"`
			Branch  string `json:"branch,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		m.mu.Lock()
		m.nextBlobID++
		newSHA := fmt.Sprintf("blob%06d", m.nextBlobID)
		m.orphanFiles[path] = ghMockFile{Content: []byte(req.Content), SHA: newSHA}
		m.updateCalls++
		m.mu.Unlock()
		writeJSON(w, map[string]any{
			"content": map[string]any{"path": path, "sha": newSHA},
			"commit":  map[string]string{"sha": "commit-update"},
		})
	default:
		http.Error(w, "bad method", 405)
	}
}

func ghCommentJSON(c ghMockComment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       map[string]string{"login": c.User},
		"html_url":   fmt.Sprintf("https://github.com/o/r/issues/42#issuecomment-%d", c.ID),
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// newMockedBackend wires a Backend at the mock server. Uses Test mode (no
// real network), no real gh CLI. Sets FLOW_DIR to a tempdir so Backend.Claim
// (which now writes .flow/active.json via pkg/clistate) doesn't pollute
// the package directory.
func newMockedBackend(t *testing.T, mock *ghMock, srv *httptest.Server) *Backend {
	t.Helper()
	t.Setenv("FLOW_DIR", t.TempDir())
	// One gitOps, shared with the seam: a test that substitutes b.git.runner
	// must also be substituting what `gh` and the push go through.
	git := newGitOps(".")
	b := &Backend{
		cfg: Config{
			Owner:           mock.owner,
			Repo:            mock.repo,
			BinaryName:      "implement",
			Token:           "fake-token",
			LabelPrefix:     "flow:",
			MaxCommentBytes: 60 * 1024,
		},
		// An allowing guard, because with none installed this backend
		// publishes nothing and every test below would fail for that reason
		// rather than its own. TestNoGuardPublishesNothing is where the
		// absent-guard case is asserted.
		out:               newOutward("fake-token", git, mock.owner, mock.repo, allowing()),
		git:               git,
		labels:            newLabels("flow:"),
		stateCommentCache: map[int]int64{},
	}
	// Point go-github at the mock.
	_, err := b.WithBaseURL(srv.URL+"/", srv.URL+"/")
	if err != nil {
		t.Fatalf("WithBaseURL: %v", err)
	}

	// Install a default git recorder so the pre-claim worktree precondition
	// checks pass without real git. Tests that need specific git behaviour
	// (Finalize, precondition tests) replace the recorder after this call.
	rec := newGitRecorder()
	scriptCleanWorktree(rec)
	b.git.runner = rec.run
	return b
}

func TestBackend_LookupClaim_OnOwnerLabel(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	// No owner label yet → nil.
	info, err := b.LookupClaim(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info != nil {
		t.Errorf("LookupClaim = %+v, want nil", info)
	}

	mock.mu.Lock()
	mock.issueLabels = append(mock.issueLabels, "flow:owner:bob")
	mock.mu.Unlock()

	info, err = b.LookupClaim(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("LookupClaim: %v", err)
	}
	if info == nil || info.Owner != "bob" {
		t.Errorf("LookupClaim = %+v, want owner=bob", info)
	}
}

func TestBackend_ClaimSeedResolveRoundTrip(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)

	// Claim. The race-check sees only one flow:claim:* label (ours), so we win.
	claim, err := b.Claim(ctx, ref, "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Owner != "alice" {
		t.Errorf("claim.Owner = %q, want alice", claim.Owner)
	}
	// Owner + binary labels should be present, claim:* label cleaned.
	mock.mu.Lock()
	labels := append([]string(nil), mock.issueLabels...)
	mock.mu.Unlock()
	if !contains(labels, "flow:owner:alice") {
		t.Errorf("missing owner label; have %v", labels)
	}
	if !contains(labels, "flow:implement") {
		t.Errorf("missing binary label; have %v", labels)
	}
	for _, l := range labels {
		if strings.HasPrefix(l, "flow:claim:") {
			t.Errorf("claim label should be cleaned up; have %v", labels)
		}
	}

	// SeedState — should post the state comment with the artifact set.
	specs := []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}
	if err := b.SeedState(ctx, claim, specs); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	// LoadState should return the seeded artifact.
	state, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	rec, ok := state.Artifacts["plan"]
	if !ok {
		t.Fatalf("LoadState missing plan artifact; got %+v", state.Artifacts)
	}
	if rec.GrantedInvocations != flow.DefaultStepBudget().MaxInvocations {
		t.Errorf("GrantedInvocations = %d, want default %d",
			rec.GrantedInvocations, flow.DefaultStepBudget().MaxInvocations)
	}
	if state.Item.Type != "" {
		t.Errorf("Item.Type = %q, want empty (no type:* labels in mock)", state.Item.Type)
	}

	// ResolveArtifact (markdown) — posts a new comment + updates state.
	body := flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan content"}
	if err := b.ResolveArtifact(ctx, claim, "plan", body); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}
	state, err = b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState after resolve: %v", err)
	}
	rec = state.Artifacts["plan"]
	if !rec.Resolved || rec.Version != 1 {
		t.Errorf("after resolve: %+v, want Resolved version=1", rec)
	}

	// Second seed must refuse.
	if err := b.SeedState(ctx, claim, specs); err == nil {
		t.Errorf("expected SeedState to refuse re-seed")
	}
}

func TestBackend_BumpInvocations_PersistsViaStateComment(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)
	claim, err := b.Claim(ctx, ref, "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	if err := b.BumpInvocations(ctx, claim, "plan"); err != nil {
		t.Fatalf("BumpInvocations: %v", err)
	}
	if err := b.BumpInvocations(ctx, claim, "plan"); err != nil {
		t.Fatalf("BumpInvocations 2: %v", err)
	}
	if err := b.AddCost(ctx, claim, "plan", 1.5); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	state, _ := b.LoadState(ctx, claim)
	rec := state.Artifacts["plan"]
	if rec.Invocations != 2 {
		t.Errorf("Invocations = %d, want 2", rec.Invocations)
	}
	if rec.CostUSDSpent != 1.5 {
		t.Errorf("CostUSDSpent = %v, want 1.5", rec.CostUSDSpent)
	}
}

func TestBackend_ResolveFileArtifactSpills(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)
	claim, err := b.Claim(ctx, ref, "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "screenshot", Type: flow.ArtifactFile, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	body := flow.ArtifactBody{
		Type: flow.ArtifactFile,
		File: flow.FileBody{Name: "result.png", Content: []byte("PNG\x89big content")},
	}
	if err := b.ResolveArtifact(ctx, claim, "screenshot", body); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	// Branch was created on first spill.
	mock.mu.Lock()
	created := mock.branchCreated
	wantPath := artifactFilePath(42, "screenshot", "result.png")
	_, fileRecorded := mock.orphanFiles[wantPath]
	mock.mu.Unlock()
	if !created {
		t.Errorf("orphan branch was not created on first spill")
	}
	if !fileRecorded {
		t.Errorf("orphan-branch file %q not recorded", wantPath)
	}

	// The marker comment should link to the raw URL.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	wantRaw := b.rawArtifactURL(wantPath)
	hit := false
	for _, c := range mock.comments {
		if strings.Contains(c.Body, wantRaw) {
			hit = true
		}
	}
	if !hit {
		t.Errorf("no comment links to raw URL %s; comments: %d", wantRaw, len(mock.comments))
	}
}

func TestBackend_ResolvePatchArtifactSpills(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)
	claim, _ := b.Claim(ctx, ref, "alice", nil)
	_ = b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "implementation", Type: flow.ArtifactPatch, Required: true, Budget: flow.DefaultStepBudget()},
	})

	patch := []byte("--- a/foo\n+++ b/foo\n@@ -1 +1 @@\n-old\n+new\n")
	body := flow.ArtifactBody{
		Type:  flow.ArtifactPatch,
		Patch: flow.PatchBody{Diff: patch, BaseSHA: "abc1234", BaseBranch: "main"},
	}
	if err := b.ResolveArtifact(ctx, claim, "implementation", body); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	mock.mu.Lock()
	_, ok := mock.orphanFiles[artifactFilePath(42, "implementation", "patch.diff")]
	mock.mu.Unlock()
	if !ok {
		t.Errorf("patch.diff was not committed to orphan branch")
	}
}

func TestBackend_LargeMarkdownAutoSpills(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)
	// Force a small comment ceiling so any non-trivial body spills.
	b.cfg.MaxCommentBytes = 256

	ctx := t.Context()
	ref := b.refFromIssue(42)
	claim, _ := b.Claim(ctx, ref, "alice", nil)
	_ = b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "log", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	})

	// 2 KiB markdown — well above 256 byte cap.
	bigBody := strings.Repeat("verbose output line\n", 200)
	if err := b.ResolveArtifact(ctx, claim, "log", flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: bigBody}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	mock.mu.Lock()
	_, spilled := mock.orphanFiles[artifactFilePath(42, "log", "body.md")]
	commentCount := len(mock.comments)
	mock.mu.Unlock()
	if !spilled {
		t.Errorf("large markdown was not spilled to orphan branch")
	}
	if commentCount < 2 {
		t.Errorf("expected at least state + artifact comments; got %d", commentCount)
	}
}

func TestBackend_SecondSpillUpdatesViaContentsAPI(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)
	claim, _ := b.Claim(ctx, ref, "alice", nil)
	_ = b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "blob", Type: flow.ArtifactFile, Required: true, Budget: flow.DefaultStepBudget()},
	})

	// First resolve creates the branch.
	if err := b.ResolveArtifact(ctx, claim, "blob", flow.ArtifactBody{Type: flow.ArtifactFile, File: flow.FileBody{Name: "x.bin", Content: []byte("v1")}}); err != nil {
		t.Fatalf("first ResolveArtifact: %v", err)
	}
	// Mark the artifact stale so a second resolve is accepted.
	if err := b.MarkStale(ctx, claim, "blob"); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	// Second resolve must use the Contents PUT path (branch exists).
	if err := b.ResolveArtifact(ctx, claim, "blob", flow.ArtifactBody{Type: flow.ArtifactFile, File: flow.FileBody{Name: "x.bin", Content: []byte("v2")}}); err != nil {
		t.Fatalf("second ResolveArtifact: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.updateCalls < 1 {
		t.Errorf("expected at least one contents PUT call; got %d", mock.updateCalls)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"normal.txt", "normal.txt"},
		{"weird name with spaces.png", "weird_name_with_spaces.png"},
		{"../../escape", ".._.._escape"}, // leading dots trimmed → ".._.._escape" without leading dot
		{"", "blob"},
		{"...", "blob"},
	}
	for _, c := range cases {
		got := sanitizeFilename(c.in)
		// Trim-left of dots can produce different result depending on input;
		// the contract is just "no path separators, no leading dots, non-empty".
		if strings.ContainsAny(got, "/\\") {
			t.Errorf("sanitizeFilename(%q) = %q, contains path separator", c.in, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeFilename(%q) = %q, starts with dot", c.in, got)
		}
		if got == "" {
			t.Errorf("sanitizeFilename(%q) = empty", c.in)
		}
	}
}

// ---------------------------------------------------------------------------
// Claim: refuse when another person holds the issue
// ---------------------------------------------------------------------------

// An issue assigned to someone else must be refused without force.
func TestBackend_Claim_RefusesWhenAssignedToAnother(t *testing.T) {
	mock := newGHMock(t)
	mock.assignees = []string{"bob"}
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse when the issue is assigned to another person")
	}
	if !strings.Contains(err.Error(), "assigned to bob") {
		t.Errorf("error = %v, want mention of bob", err)
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T", err)
	}
	if refused.Code != "already-held" {
		t.Errorf("Code = %q, want already-held", refused.Code)
	}
	if !refused.ItemScoped {
		t.Error("ItemScoped = false, want true (another item could succeed)")
	}

	// No claim label should have been posted — the refusal is in preflight.
	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	for _, m := range mutations {
		if strings.Contains(m, "labels") {
			t.Errorf("a label mutation reached GitHub despite preflight refusal: %s", m)
		}
	}
}

// An issue carrying a flow:owner:<other> label must be refused without force.
func TestBackend_Claim_RefusesWhenOwnerLabelForAnother(t *testing.T) {
	mock := newGHMock(t)
	mock.issueLabels = []string{"flow:owner:carol"}
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse when owner label names another login")
	}
	if !strings.Contains(err.Error(), "owner label for carol") {
		t.Errorf("error = %v, want mention of carol", err)
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T", err)
	}
	if refused.Code != "already-held" {
		t.Errorf("Code = %q, want already-held", refused.Code)
	}
}

// Both assignee and owner-label checks are bypassed when force=true.
func TestBackend_Claim_ForceOverridesHeldIssue(t *testing.T) {
	mock := newGHMock(t)
	mock.assignees = []string{"bob"}
	mock.issueLabels = []string{"flow:owner:bob"}
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	claim, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", []flow.ClaimOverride{flow.OverrideAlreadyHeld})
	if err != nil {
		t.Fatalf("Claim with force=true should succeed: %v", err)
	}
	if claim.Owner != "alice" {
		t.Errorf("claim.Owner = %q, want alice", claim.Owner)
	}

	// bob's stale owner label should have been removed.
	mock.mu.Lock()
	labels := append([]string(nil), mock.issueLabels...)
	mock.mu.Unlock()
	if contains(labels, "flow:owner:bob") {
		t.Errorf("bob's owner label was not removed; labels = %v", labels)
	}
	if !contains(labels, "flow:owner:alice") {
		t.Errorf("alice's owner label missing; labels = %v", labels)
	}
}

// Self-assignment (already assigned to owner) is not a refusal.
func TestBackend_Claim_AllowsSelfAssigned(t *testing.T) {
	mock := newGHMock(t)
	mock.assignees = []string{"alice"}
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim should allow self-assigned issues: %v", err)
	}
}

// Self owner-label is not a refusal.
func TestBackend_Claim_AllowsSelfOwnerLabel(t *testing.T) {
	mock := newGHMock(t)
	mock.issueLabels = []string{"flow:owner:alice"}
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim should allow own owner label: %v", err)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// End-to-end park lifecycle against the mocked API: Park writes the state-doc
// field AND the label, LoadState surfaces it, and Grant clears both — but only
// when the grant actually gives the parked axis headroom.
func TestBackend_ParkSurvivesLoadAndClearsOnGrant(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 2, MaxCostUSD: 10}},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	for range 2 {
		if err := b.BumpInvocations(ctx, claim, "plan"); err != nil {
			t.Fatalf("BumpInvocations: %v", err)
		}
	}
	if err := b.Park(ctx, claim, flow.ParkRequest{
		Kind: flow.ParkBudgetExhausted, Step: "plan", Axis: flow.AxisInvocations,
		Reason: `ran 2 times without resolving "plan"`,
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}

	state, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !state.Parked() {
		t.Fatal("LoadState did not surface the park")
	}
	if state.Park.Step != "plan" || state.Park.Axis != flow.AxisInvocations {
		t.Errorf("park = %+v, want plan/invocations", state.Park)
	}
	parkLabelName := b.labels.BudgetExhausted("plan")
	if !hasLabel(mock.labelNames(), parkLabelName) {
		t.Errorf("labels = %v, want %q", mock.labelNames(), parkLabelName)
	}

	// A cost grant does not clear an invocations park.
	if err := b.Grant(ctx, claim, "plan", flow.Grant{CostUSD: 5}); err != nil {
		t.Fatalf("Grant (cost): %v", err)
	}
	state, _ = b.LoadState(ctx, claim)
	if !state.Parked() {
		t.Error("park cleared by a grant on an unrelated axis")
	}
	if !hasLabel(mock.labelNames(), parkLabelName) {
		t.Error("park label removed by a grant on an unrelated axis")
	}

	// One more invocation puts the parked axis back in the black.
	if err := b.Grant(ctx, claim, "plan", flow.Grant{Invocations: 1}); err != nil {
		t.Fatalf("Grant (invocations): %v", err)
	}
	state, _ = b.LoadState(ctx, claim)
	if state.Parked() {
		t.Errorf("park = %+v, want cleared", state.Park)
	}
	if hasLabel(mock.labelNames(), parkLabelName) {
		t.Errorf("labels = %v, want %q removed", mock.labelNames(), parkLabelName)
	}
	if rec := state.Artifacts["plan"]; rec.GrantedInvocations != 3 || rec.GrantedCostUSD != 15 {
		t.Errorf("record = inv %d / cost %v, want 3 / 15", rec.GrantedInvocations, rec.GrantedCostUSD)
	}
}

// A side-effect signal write edits the document in place, so it must not take
// the park down with it (the bug a rebuild-from-ItemState would have).
func TestBackend_SignalWritePreservesPark(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	if err := b.Park(ctx, claim, flow.ParkRequest{
		Kind: flow.ParkBudgetExhausted, Step: "plan", Axis: flow.AxisCost,
	}); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if err := b.markSignalSetOnState(ctx, claim, "pr-open"); err != nil {
		t.Fatalf("markSignalSetOnState: %v", err)
	}

	state, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !state.Parked() {
		t.Fatal("the signal write erased the park")
	}
	if state.Park.Axis != flow.AxisCost {
		t.Errorf("park axis = %q, want cost", state.Park.Axis)
	}
	if rec := state.Artifacts["plan"]; rec.GrantedInvocations != flow.DefaultStepBudget().MaxInvocations {
		t.Errorf("the signal write disturbed the budget: %+v", rec)
	}
}

// labelNames returns the issue's current labels.
func (m *ghMock) labelNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.issueLabels...)
}

// ---------------------------------------------------------------------------
// LoadStateByRef (flow.StateInspector)
// ---------------------------------------------------------------------------

// The compile-time assertion lives in backend.go. This test exercises the
// method end-to-end: claim, seed, resolve an artifact, then load via ref
// alone — no claim token — and verify the result matches LoadState.
func TestBackend_LoadStateByRef_MatchesLoadState(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)

	claim, err := b.Claim(ctx, ref, "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	specs := []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}
	if err := b.SeedState(ctx, claim, specs); err != nil {
		t.Fatalf("SeedState: %v", err)
	}
	if err := b.ResolveArtifact(ctx, claim, "plan", flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"}); err != nil {
		t.Fatalf("ResolveArtifact: %v", err)
	}

	// LoadState (via claim) is the reference.
	want, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	// LoadStateByRef must return the same state without a claim.
	got, err := b.LoadStateByRef(ctx, ref)
	if err != nil {
		t.Fatalf("LoadStateByRef: %v", err)
	}

	if got.Item.Title != want.Item.Title {
		t.Errorf("Title = %q, want %q", got.Item.Title, want.Item.Title)
	}
	if len(got.Artifacts) != len(want.Artifacts) {
		t.Fatalf("Artifacts count = %d, want %d", len(got.Artifacts), len(want.Artifacts))
	}
	rec := got.Artifacts["plan"]
	if !rec.Resolved {
		t.Errorf("plan artifact not resolved via LoadStateByRef")
	}
	if rec.Markdown != "the plan" {
		t.Errorf("plan Markdown = %q, want %q", rec.Markdown, "the plan")
	}
}

// LoadStateByRef with a cold cache (no prior LoadState call) must scan the
// issue's comments to find the state comment — the same path fetchStateComment
// takes when cachedID is 0.
func TestBackend_LoadStateByRef_ColdCache(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)

	// Claim and seed via a SEPARATE backend instance (simulating a different
	// process) so b's stateCommentCache is cold.
	b2 := newMockedBackend(t, mock, srv)
	claim, err := b2.Claim(ctx, ref, "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b2.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown, Required: true, Budget: flow.DefaultStepBudget()},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	// b has never seen this issue — cache is empty.
	got, err := b.LoadStateByRef(ctx, ref)
	if err != nil {
		t.Fatalf("LoadStateByRef (cold): %v", err)
	}
	if _, ok := got.Artifacts["plan"]; !ok {
		t.Errorf("plan artifact missing from cold-cache LoadStateByRef; got %+v", got.Artifacts)
	}
}

// LoadStateByRef on an issue with NO state comment returns an empty (unseeded)
// state — not an error. This is the expected shape for an issue that has never
// been claimed.
func TestBackend_LoadStateByRef_NoStateComment(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	ref := b.refFromIssue(42)

	got, err := b.LoadStateByRef(ctx, ref)
	if err != nil {
		t.Fatalf("LoadStateByRef: %v", err)
	}
	if len(got.Artifacts) != 0 {
		t.Errorf("expected no artifacts on unseeded issue; got %+v", got.Artifacts)
	}
	if got.Item.Title != "Test issue" {
		t.Errorf("Title = %q, want %q", got.Item.Title, "Test issue")
	}
}

// ---------------------------------------------------------------------------
// Finalize (flow.Finalizer)
// ---------------------------------------------------------------------------

// gitRecorder is a runner that records git commands and replays scripted
// responses. Only commands matching a registered pattern fire; the rest
// fall through to an optional fallback.
type gitRecorder struct {
	mu       sync.Mutex
	calls    [][]string                                     // every invocation's args
	handlers map[string]func(args []string) ([]byte, error) // key = first distinguishing arg
}

func newGitRecorder() *gitRecorder {
	return &gitRecorder{handlers: map[string]func([]string) ([]byte, error){}}
}

func (r *gitRecorder) run(_ context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()

	// Match on the git sub-command (args after -C <dir>): args[0]="-C",
	// args[1]=dir, args[2]=sub-command.
	if len(args) >= 3 {
		// Build a lookup key from all args after -C <dir>.
		key := strings.Join(args[2:], " ")
		r.mu.Lock()
		h, ok := r.handlers[key]
		r.mu.Unlock()
		if ok {
			out, err := h(args)
			var stderr []byte
			if err != nil {
				stderr = []byte(err.Error())
			}
			return out, stderr, err
		}
	}
	return nil, []byte("unhandled git call"), fmt.Errorf("unhandled git call: %v", args)
}

func (r *gitRecorder) called(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if len(c) >= 3 && strings.HasPrefix(strings.Join(c[2:], " "), sub) {
			return true
		}
	}
	return false
}

// stateCommentID is the comment ID pre-seeded into the mock for Finalize
// tests. Matches what claimForFinalize puts in the claim token.
const finalizeStateCommentID = 999

// newFinalizeBackend sets up a mocked backend with a git recorder for
// Finalize tests. Returns the backend, the mock, and the recorder.
func newFinalizeBackend(t *testing.T) (*Backend, *ghMock, *gitRecorder) {
	t.Helper()
	mock := newGHMock(t)
	// Pre-set owner label and assignee so Release has something to clean up.
	mock.issueLabels = []string{"flow:owner:alice", "flow:implement"}
	mock.assignees = []string{"alice"}
	// Pre-seed a state comment so Finalize can read and update it.
	stateBody, err := renderStateComment("alice", stateDoc{
		Flow:   "issue",
		Schema: stateSchemaVersion,
	})
	if err != nil {
		t.Fatalf("render seed state: %v", err)
	}
	mock.comments = []ghMockComment{
		{ID: finalizeStateCommentID, Body: stateBody, User: "alice"},
	}
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedBackend(t, mock, srv)

	rec := newGitRecorder()
	b.git.runner = rec.run
	return b, mock, rec
}

// claimForFinalize builds a claim suitable for Finalize tests without
// hitting the GitHub API (the git recorder can't serve the Claim flow).
func claimForFinalize(b *Backend) flow.Claim {
	ref := b.refFromIssue(42)
	tok, _ := b.saveClaimToken(claimToken{StateCommentID: finalizeStateCommentID, ClaimID: "test"})
	return flow.Claim{
		BackendName: b.Name(),
		ItemRef:     ref,
		Owner:       "alice",
		Token:       tok,
	}
}

func TestBackend_Finalize_ReturnsWorktreeToBaseAndReleases(t *testing.T) {
	b, mock, rec := newFinalizeBackend(t)

	// Script: on a feature branch, clean, base exists.
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("flow/issue-42\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=no"] = func([]string) ([]byte, error) {
		return []byte(""), nil // clean
	}
	rec.handlers["rev-parse --verify refs/heads/main"] = func([]string) ([]byte, error) {
		return []byte("abc123\n"), nil // exists
	}
	rec.handlers["checkout main"] = func([]string) ([]byte, error) {
		return []byte(""), nil
	}

	claim := claimForFinalize(b)
	// Save claim file so Release can clear it.
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}

	if err := b.Finalize(t.Context(), claim); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Checkout to base was issued.
	if !rec.called("checkout") {
		t.Error("expected checkout to main")
	}

	// Release ran: owner label removed, assignee removed, claim file cleared.
	if contains(mock.labelNames(), "flow:owner:alice") {
		t.Error("owner label not removed")
	}
	if c, _ := clistate.Load(); c != nil {
		t.Errorf("claim file not cleared: %+v", c)
	}

	// State comment now carries finalized: true.
	mock.mu.Lock()
	var finalizedInDoc bool
	for _, c := range mock.comments {
		if doc, _, found, _ := extractStateDoc(c.Body); found && doc != nil {
			finalizedInDoc = doc.Finalized
		}
	}
	mock.mu.Unlock()
	if !finalizedInDoc {
		t.Error("state comment should carry finalized: true after Finalize")
	}
}

func TestBackend_Finalize_AlreadyOnBase(t *testing.T) {
	b, mock, rec := newFinalizeBackend(t)

	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("main\n"), nil
	}

	claim := claimForFinalize(b)
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}

	if err := b.Finalize(t.Context(), claim); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// No checkout issued.
	if rec.called("checkout") {
		t.Error("checkout should not be called when already on base")
	}

	// Release still ran.
	if contains(mock.labelNames(), "flow:owner:alice") {
		t.Error("owner label not removed")
	}
}

func TestBackend_Finalize_RefusesDirtyWorktree(t *testing.T) {
	b, _, rec := newFinalizeBackend(t)

	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("flow/issue-42\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=no"] = func([]string) ([]byte, error) {
		return []byte(" M dirty-file.go\n"), nil // dirty
	}

	claim := claimForFinalize(b)
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}

	err := b.Finalize(t.Context(), claim)
	if err == nil {
		t.Fatal("Finalize should refuse a dirty worktree")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Errorf("error = %v, want mention of dirty", err)
	}

	// No checkout, claim NOT released.
	if rec.called("checkout") {
		t.Error("checkout should not be called on dirty worktree")
	}
	c, _ := clistate.Load()
	if c == nil {
		t.Error("claim file should not be cleared on dirty worktree")
	}
}

func TestBackend_Finalize_RefusesMissingBaseBranch(t *testing.T) {
	b, _, rec := newFinalizeBackend(t)

	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("flow/issue-42\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=no"] = func([]string) ([]byte, error) {
		return []byte(""), nil // clean
	}
	rec.handlers["rev-parse --verify refs/heads/main"] = func([]string) ([]byte, error) {
		return nil, fmt.Errorf("base branch main not found")
	}

	claim := claimForFinalize(b)
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}

	err := b.Finalize(t.Context(), claim)
	if err == nil {
		t.Fatal("Finalize should refuse when base branch is missing")
	}
	if !strings.Contains(err.Error(), "base branch") {
		t.Errorf("error = %v, want mention of base branch", err)
	}

	// No checkout, claim NOT released.
	if rec.called("checkout") {
		t.Error("checkout should not be called when base branch is missing")
	}
	c, _ := clistate.Load()
	if c == nil {
		t.Error("claim file should not be cleared when base branch is missing")
	}
}

func TestBackend_Finalize_CheckoutFailureKeepsClaimIntact(t *testing.T) {
	b, mock, rec := newFinalizeBackend(t)

	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("flow/issue-42\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=no"] = func([]string) ([]byte, error) {
		return []byte(""), nil // clean
	}
	rec.handlers["rev-parse --verify refs/heads/main"] = func([]string) ([]byte, error) {
		return []byte("abc123\n"), nil
	}
	rec.handlers["checkout main"] = func([]string) ([]byte, error) {
		return nil, fmt.Errorf("checkout failed: locked index")
	}

	claim := claimForFinalize(b)
	if err := clistate.Save(claim); err != nil {
		t.Fatalf("clistate.Save: %v", err)
	}

	err := b.Finalize(t.Context(), claim)
	if err == nil {
		t.Fatal("Finalize should fail when checkout fails")
	}
	if !strings.Contains(err.Error(), "checkout") {
		t.Errorf("error = %v, want mention of checkout", err)
	}

	// Claim must still be held: owner label present, claim file intact.
	if !contains(mock.labelNames(), "flow:owner:alice") {
		t.Error("owner label should still be present after checkout failure")
	}
	c, _ := clistate.Load()
	if c == nil {
		t.Error("claim file should not be cleared after checkout failure")
	}
}

func TestBackend_LoadState_RoundTripsFinalized(t *testing.T) {
	mock := newGHMock(t)
	// Pre-seed a state comment with finalized: true.
	stateBody, err := renderStateComment("alice", stateDoc{
		Flow:      "issue",
		Schema:    stateSchemaVersion,
		Finalized: true,
	})
	if err != nil {
		t.Fatalf("render state: %v", err)
	}
	mock.comments = []ghMockComment{
		{ID: 800, Body: stateBody, User: "alice"},
	}
	mock.issueLabels = []string{"flow:owner:alice"}
	mock.assignees = []string{"alice"}
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ref := b.refFromIssue(42)
	tok, _ := b.saveClaimToken(claimToken{StateCommentID: 800, ClaimID: "test"})
	claim := flow.Claim{
		BackendName: b.Name(),
		ItemRef:     ref,
		Owner:       "alice",
		Token:       tok,
	}

	state, err := b.LoadState(t.Context(), claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !state.Item.Finalized {
		t.Error("LoadState should return Item.Finalized=true when state comment carries finalized: true")
	}
}

// ---------------------------------------------------------------------------
// Pre-claim worktree precondition tests (#105)
// ---------------------------------------------------------------------------

// newClaimPrecondBackend sets up a mocked backend with a git recorder for
// pre-claim worktree precondition tests.
func newClaimPrecondBackend(t *testing.T) (*Backend, *ghMock, *gitRecorder) {
	t.Helper()
	mock := newGHMock(t)
	srv := mock.server()
	t.Cleanup(srv.Close)
	b := newMockedBackend(t, mock, srv)

	rec := newGitRecorder()
	b.git.runner = rec.run
	return b, mock, rec
}

// scriptCleanWorktree registers git handlers that make all four precondition
// checks pass: fetch succeeds, HEAD is on main, local and remote SHAs match,
// and the tree is clean.
func scriptCleanWorktree(rec *gitRecorder) {
	rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		return nil, nil
	}
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("main\n"), nil
	}
	rec.handlers["rev-parse main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["rev-parse origin/main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte(""), nil
	}
}

func TestBackend_Claim_RefusesOnFetchFailure(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		return nil, fmt.Errorf("git fetch origin: fatal: could not read from remote")
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse when fetch fails")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "fetch-failed" {
		t.Errorf("Code = %q, want fetch-failed", refused.Code)
	}
	if refused.ItemScoped {
		t.Error("ItemScoped = true, want false (arena property)")
	}
	if refused.Check != "fetch" {
		t.Errorf("Check = %q, want fetch", refused.Check)
	}
	if refused.Override != "force" {
		t.Errorf("Override = %q, want force", refused.Override)
	}
}

func TestBackend_Claim_RefusesWhenNotOnBase(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		return nil, nil
	}
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("feature\n"), nil
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse when HEAD is not on the base branch")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "not-on-base" {
		t.Errorf("Code = %q, want not-on-base", refused.Code)
	}
	if refused.ItemScoped {
		t.Error("ItemScoped = true, want false")
	}
	if refused.Check != "base-branch" {
		t.Errorf("Check = %q, want base-branch", refused.Check)
	}
	if refused.Override != "force" {
		t.Errorf("Override = %q, want force", refused.Override)
	}
}

func TestBackend_Claim_RefusesWhenBaseStale(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		return nil, nil
	}
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("main\n"), nil
	}
	rec.handlers["rev-parse main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["rev-parse origin/main"] = func([]string) ([]byte, error) {
		return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse when base is stale")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "base-stale" {
		t.Errorf("Code = %q, want base-stale", refused.Code)
	}
	if refused.ItemScoped {
		t.Error("ItemScoped = true, want false")
	}
	if refused.Check != "base-branch" {
		t.Errorf("Check = %q, want base-branch", refused.Check)
	}
	if refused.Override != "force" {
		t.Errorf("Override = %q, want force", refused.Override)
	}
}

func TestBackend_Claim_RefusesOnDirtyTree(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		return nil, nil
	}
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("main\n"), nil
	}
	rec.handlers["rev-parse main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["rev-parse origin/main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte("?? leftover.txt\n"), nil
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse when tree is dirty")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "dirty-tree" {
		t.Errorf("Code = %q, want dirty-tree", refused.Code)
	}
	if refused.ItemScoped {
		t.Error("ItemScoped = true, want false")
	}
	if refused.Check != "clean-tree" {
		t.Errorf("Check = %q, want clean-tree", refused.Check)
	}
	if refused.Override != "force" {
		t.Errorf("Override = %q, want force", refused.Override)
	}
	if !strings.Contains(refused.Detail, "leftover.txt") {
		t.Errorf("Detail = %q, want it to contain leftover.txt", refused.Detail)
	}
}

func TestBackend_Claim_ForceBypassesWorktreeChecks(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	// Script a dirty tree — but pass all three overrides.
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte("?? leftover.txt\n"), nil
	}

	overrides := []flow.ClaimOverride{
		flow.OverrideStaleBase,
		flow.OverrideDirtyTree,
		flow.OverrideAlreadyHeld,
	}
	claim, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", overrides)
	if err != nil {
		t.Fatalf("Claim with all overrides should succeed: %v", err)
	}
	if claim.Owner != "alice" {
		t.Errorf("claim.Owner = %q, want alice", claim.Owner)
	}
}

func TestBackend_Claim_NoMutationsOnWorktreeRefusal(t *testing.T) {
	b, mock, rec := newClaimPrecondBackend(t)
	// Dirty tree, no overrides.
	rec.handlers["fetch origin"] = func([]string) ([]byte, error) {
		return nil, nil
	}
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("main\n"), nil
	}
	rec.handlers["rev-parse main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["rev-parse origin/main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte("M dirty.go\n"), nil
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err == nil {
		t.Fatal("Claim should refuse dirty tree")
	}

	mock.mu.Lock()
	mutations := append([]string(nil), mock.mutations...)
	mock.mu.Unlock()
	for _, m := range mutations {
		if strings.Contains(m, "labels") || strings.Contains(m, "assignees") {
			t.Errorf("a mutation reached GitHub despite worktree refusal: %s", m)
		}
	}
}

// OverrideStaleBase skips fetch+branch+stale checks but the dirty-tree
// check must still fire. If someone collapses both guards into one
// override check, this fails.
func TestBackend_Claim_OverrideStaleBaseStillChecksDirtyTree(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	// Skip the stale-base block entirely via override, but script a dirty tree.
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte("M uncommitted.go\n"), nil
	}

	overrides := []flow.ClaimOverride{flow.OverrideStaleBase}
	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", overrides)
	if err == nil {
		t.Fatal("OverrideStaleBase should not bypass the dirty-tree check")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "dirty-tree" {
		t.Errorf("Code = %q, want dirty-tree", refused.Code)
	}
}

// OverrideDirtyTree skips the clean-tree check but must not skip the
// stale-base checks.  Complement of the test above.
func TestBackend_Claim_OverrideDirtyTreeStillChecksStaleBase(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	// Make the base stale.
	rec.handlers["rev-parse origin/main"] = func([]string) ([]byte, error) {
		return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
	}
	// The tree is dirty, but overridden.
	rec.handlers["status --porcelain --untracked-files=normal"] = func([]string) ([]byte, error) {
		return []byte("M uncommitted.go\n"), nil
	}

	overrides := []flow.ClaimOverride{flow.OverrideDirtyTree}
	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", overrides)
	if err == nil {
		t.Fatal("OverrideDirtyTree should not bypass the stale-base check")
	}
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if refused.Code != "base-stale" {
		t.Errorf("Code = %q, want base-stale", refused.Code)
	}
}

// The not-on-base refusal must name both branches so the operator knows
// which branch to switch to.
func TestBackend_Claim_NotOnBaseReasonNamesBothBranches(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	rec.handlers["rev-parse --abbrev-ref HEAD"] = func([]string) ([]byte, error) {
		return []byte("feature-x\n"), nil
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if !strings.Contains(refused.Reason, "feature-x") {
		t.Errorf("Reason should mention current branch: %q", refused.Reason)
	}
	if !strings.Contains(refused.Reason, "main") {
		t.Errorf("Reason should mention base branch: %q", refused.Reason)
	}
}

// The base-stale refusal must include both SHAs and the recovery command.
func TestBackend_Claim_BaseStaleReasonContainsSHAsAndRecovery(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)
	rec.handlers["rev-parse main"] = func([]string) ([]byte, error) {
		return []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), nil
	}
	rec.handlers["rev-parse origin/main"] = func([]string) ([]byte, error) {
		return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"), nil
	}

	_, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	var refused flow.ErrClaimRefused
	if !errors.As(err, &refused) {
		t.Fatalf("error is not ErrClaimRefused: %T: %v", err, err)
	}
	if !strings.Contains(refused.Reason, "aaaaaaaa") {
		t.Errorf("Reason should contain local SHA prefix: %q", refused.Reason)
	}
	if !strings.Contains(refused.Reason, "bbbbbbbb") {
		t.Errorf("Reason should contain remote SHA prefix: %q", refused.Reason)
	}
	if !strings.Contains(refused.Reason, "git pull --ff-only") {
		t.Errorf("Reason should contain recovery command: %q", refused.Reason)
	}
}

func TestBackend_Claim_CleanTreeSucceeds(t *testing.T) {
	b, _, rec := newClaimPrecondBackend(t)
	scriptCleanWorktree(rec)

	claim, err := b.Claim(t.Context(), b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim should succeed with clean worktree: %v", err)
	}
	if claim.Owner != "alice" {
		t.Errorf("claim.Owner = %q, want alice", claim.Owner)
	}
}

// A disclosure refusal on the park reason must not abort the park: the park is
// recorded with a substitute reason naming the refusal, not the matched text.
func TestBackend_ParkRecordsOnDisclosureRefusal(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	// Guard that refuses the first ActParkRecord attempt (agent prose) but
	// allows the retry (the substituted, disclosure-safe body).
	attempts := 0
	b.out.guard = guardFunc(func(_ context.Context, d flow.Disclosure) error {
		if d.Act == flow.ActParkRecord {
			attempts++
			if attempts == 1 {
				return errors.New("an absolute home path names the machine's user — found \"/home/someone\"")
			}
		}
		return nil
	})

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "review", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	parkReq := flow.ParkRequest{
		Kind:   flow.ParkBlocked,
		Step:   "review",
		Reason: "blocked on /home/someone/prog/project — needs manual input",
	}
	if err := b.Park(ctx, claim, parkReq); err != nil {
		t.Fatalf("Park returned error: %v", err)
	}

	state, err := b.LoadState(ctx, claim)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !state.Parked() {
		t.Fatal("item is not parked after Park")
	}
	if state.Park.Kind != flow.ParkBlocked {
		t.Errorf("park.Kind = %v, want %v", state.Park.Kind, flow.ParkBlocked)
	}
	if state.Park.Step != "review" {
		t.Errorf("park.Step = %q, want %q", state.Park.Step, "review")
	}
	wantReason := "park reason withheld by disclosure guard (park-record)"
	if state.Park.Reason != wantReason {
		t.Errorf("park.Reason = %q, want %q", state.Park.Reason, wantReason)
	}

	// The park label must be present.
	parkLabelName := b.labels.Blocked()
	if !hasLabel(mock.labelNames(), parkLabelName) {
		t.Errorf("labels = %v, want %q", mock.labelNames(), parkLabelName)
	}

	// The mock must have received a comment (the retried one).
	mock.mu.Lock()
	nComments := len(mock.comments)
	mock.mu.Unlock()
	if nComments == 0 {
		t.Error("no comment was created; the retried park comment should have been posted")
	}
}

// A non-disclosure error from CreateComment must still propagate — the new
// fallback only catches ErrDisclosureRefused.
func TestBackend_ParkNonDisclosureErrorStillFails(t *testing.T) {
	mock := newGHMock(t)
	srv := mock.server()
	defer srv.Close()
	b := newMockedBackend(t, mock, srv)

	ctx := t.Context()
	claim, err := b.Claim(ctx, b.refFromIssue(42), "alice", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := b.SeedState(ctx, claim, []flow.ArtifactSpec{
		{Id: "review", Type: flow.ArtifactMarkdown, Required: true,
			Budget: flow.StepBudget{MaxInvocations: 3, MaxCostUSD: 10}},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	// Shut down the server so the HTTP call fails with a network error,
	// not a disclosure refusal.
	srv.Close()

	parkReq := flow.ParkRequest{
		Kind:   flow.ParkBlocked,
		Step:   "review",
		Reason: "some reason",
	}
	err = b.Park(ctx, claim, parkReq)
	if err == nil {
		t.Fatal("Park should have failed with a network error")
	}
	var refused flow.ErrDisclosureRefused
	if errors.As(err, &refused) {
		t.Errorf("error is ErrDisclosureRefused, want a non-disclosure error: %v", err)
	}
}
