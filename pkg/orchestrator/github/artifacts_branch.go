package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
)

// artifactsBranch is the orphan branch where large-artifact blobs live.
const artifactsBranch = "flow-artifacts"

// artifactFilePath returns the canonical path inside the orphan branch where
// an artifact's bytes are stored.
func artifactFilePath(issueNum int, artifactID, filename string) string {
	return fmt.Sprintf("flow/artifacts/issue-%d/%s/%s", issueNum, artifactID, filename)
}

// rawArtifactURL returns the raw.githubusercontent.com URL for an artifact
// file. Stable across re-publishes because the path includes only id +
// filename (no version segment).
func (b *Orchestrator) rawArtifactURL(path string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
		b.cfg.Owner, b.cfg.Repo, artifactsBranch, path)
}

// putArtifactFile commits `content` to the orphan branch at `path`. Creates
// the branch on first use. Idempotent: re-publishing the same path with new
// content just bumps the commit.
func (b *Orchestrator) putArtifactFile(ctx context.Context, path string, content []byte, commitMessage string) (rawURL string, err error) {
	exists, err := b.artifactsBranchExists(ctx)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := b.createArtifactsBranch(ctx, path, content, commitMessage); err != nil {
			return "", err
		}
		return b.rawArtifactURL(path), nil
	}
	// Branch exists — use the high-level Contents API. We need the
	// previous file's blob SHA when updating; on 404 we use CreateFile.
	prevSHA, err := b.getArtifactFileSHA(ctx, path)
	if err != nil {
		return "", err
	}
	opts := &github.RepositoryContentFileOptions{
		Message: github.Ptr(commitMessage),
		Content: content,
		Branch:  github.Ptr(artifactsBranch),
	}
	if prevSHA != "" {
		opts.SHA = github.Ptr(prevSHA)
	}
	if err := b.out.PutFile(ctx, path, opts); err != nil {
		return "", fmt.Errorf("put artifact file %s: %w", path, err)
	}
	return b.rawArtifactURL(path), nil
}

// artifactsBranchExists returns true if refs/heads/flow-artifacts is
// resolvable in the repo.
func (b *Orchestrator) artifactsBranchExists(ctx context.Context) (bool, error) {
	_, resp, err := b.out.GetRef(ctx, "heads/"+artifactsBranch)
	if err == nil {
		return true, nil
	}
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("get ref heads/%s: %w", artifactsBranch, err)
}

// createArtifactsBranch creates the orphan branch with one initial commit
// containing the given file. Uses the low-level Git Data API because the
// Contents API has no way to create a parentless commit.
func (b *Orchestrator) createArtifactsBranch(ctx context.Context, path string, content []byte, commitMessage string) error {
	// 1. Create the blob.
	blob, err := b.out.CreateBlob(ctx, &github.Blob{
		Content:  github.Ptr(string(content)),
		Encoding: github.Ptr("utf-8"),
	})
	if err != nil {
		return fmt.Errorf("create blob: %w", err)
	}

	// 2. Create the tree.
	tree, err := b.out.CreateTree(ctx, "", []*github.TreeEntry{{
		Path: github.Ptr(path),
		Mode: github.Ptr("100644"),
		Type: github.Ptr("blob"),
		SHA:  blob.SHA,
	}})
	if err != nil {
		return fmt.Errorf("create tree: %w", err)
	}

	// 3. Create the orphan commit (parents: []).
	commit, err := b.out.CreateCommit(ctx, &github.Commit{
		Message: github.Ptr(commitMessage),
		Tree:    tree,
		Parents: []*github.Commit{},
	})
	if err != nil {
		return fmt.Errorf("create commit: %w", err)
	}

	// 4. Create the ref.
	if err := b.out.CreateRef(ctx, &github.Reference{
		Ref:    github.Ptr("refs/heads/" + artifactsBranch),
		Object: &github.GitObject{SHA: commit.SHA},
	}); err != nil {
		return fmt.Errorf("create ref heads/%s: %w", artifactsBranch, err)
	}
	return nil
}

// getArtifactFileSHA returns the current blob SHA at `path` on the orphan
// branch, or empty string if the file doesn't exist there yet.
func (b *Orchestrator) getArtifactFileSHA(ctx context.Context, path string) (string, error) {
	file, resp, err := b.out.GetContents(ctx, path, &github.RepositoryContentGetOptions{Ref: artifactsBranch})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", nil
		}
		return "", fmt.Errorf("get contents %s: %w", path, err)
	}
	if file == nil {
		return "", nil
	}
	return file.GetSHA(), nil
}

// spillReason classifies why an artifact was written to the orphan branch
// (used in commit messages + marker-comment summaries).
type spillReason string

const (
	spillFileType         spillReason = "file"
	spillPatchType        spillReason = "patch"
	spillMarkdownTooLarge spillReason = "markdown-spillover"
)

// commitMessageForArtifact composes a one-line commit message describing
// what was published.
func commitMessageForArtifact(issueNum int, artifactID, filename string, reason spillReason) string {
	return fmt.Sprintf("flow: issue-%d %s/%s (%s, %s)",
		issueNum, artifactID, filename, reason, time.Now().UTC().Format(time.RFC3339))
}

// sanitizeFilename ensures a filename is path-safe (no slashes, no leading
// dots). Falls back to "blob" if input is empty after cleaning.
func sanitizeFilename(name string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, name)
	clean = strings.TrimLeft(clean, ".")
	if clean == "" {
		return "blob"
	}
	return clean
}

// errNotImplemented is a sentinel for paths the v1 backend can't service
// yet. Kept here so artifact.go can return it without importing extras.
var errNotImplemented = errors.New("not implemented")

var _ = errNotImplemented // not used outside this file; reserved for future
