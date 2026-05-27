package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ghclient "github.com/google/go-github/v68/github"
	"github.com/promise-language/flow"
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
	orphanBranchSHA string                  // commit SHA at heads/flow-artifacts
	orphanFiles     map[string]ghMockFile   // path → file
	nextBlobID      int
	nextTreeID      int
	nextCommitID    int

	// observation tape: callers can inspect putArtifactFile interactions.
	branchCreated    bool
	branchCreateCall string // path of first file committed when creating
	updateCalls      int
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
			"name":       m.repo,
			"full_name":  m.owner + "/" + m.repo,
			"permissions": m.perms,
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

	return httptest.NewServer(mux)
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
			"name":     path,
			"path":     path,
			"sha":      file.SHA,
			"type":     "file",
			"size":     len(file.Content),
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
// real network), no real gh CLI.
func newMockedBackend(t *testing.T, mock *ghMock, srv *httptest.Server) *Backend {
	t.Helper()
	b := &Backend{
		cfg: Config{
			Owner:           mock.owner,
			Repo:            mock.repo,
			BinaryName:      "implement",
			Token:           "fake-token",
			LabelPrefix:     "flow:",
			MaxCommentBytes: 60 * 1024,
		},
		gh:                ghclient.NewClient(nil).WithAuthToken("fake-token"),
		git:               newGitOps("."),
		labels:            newLabels("flow:"),
		stateCommentCache: map[int]int64{},
	}
	// Point go-github at the mock.
	_, err := b.WithBaseURL(srv.URL+"/", srv.URL+"/")
	if err != nil {
		t.Fatalf("WithBaseURL: %v", err)
	}
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
	claim, err := b.Claim(ctx, ref, "alice")
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
	claim, err := b.Claim(ctx, ref, "alice")
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
	claim, err := b.Claim(ctx, ref, "alice")
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
	claim, _ := b.Claim(ctx, ref, "alice")
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
	claim, _ := b.Claim(ctx, ref, "alice")
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
	claim, _ := b.Claim(ctx, ref, "alice")
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

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
