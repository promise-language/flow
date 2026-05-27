// Package github implements flow.Backend against GitHub Issues + the
// project's own git repo, with no server to host. State lives in a single
// "state comment" per issue (machine-managed YAML), per-artifact comments,
// and an optional flow-artifacts orphan branch for large blobs. Auth piggy-
// backs on the `gh` CLI's stored token.
package github

import (
	"context"
	"fmt"
	"strings"
)

// Config is the per-binary configuration the flow binary passes to
// NewBackend. Most fields are optional with sensible defaults.
type Config struct {
	// BinaryName is the name used in flow:<binary-name> labels — typically
	// the basename of the flow binary (e.g., "implement"). When empty,
	// NewBackend derives it from os.Args[0].
	BinaryName string

	// Owner / Repo are the GitHub repository coordinates. When either is
	// empty, NewBackend resolves them from `git remote get-url origin` in
	// the current working directory.
	Owner string
	Repo  string

	// Token overrides token lookup. When empty, NewBackend tries
	// `gh auth token` then GITHUB_TOKEN.
	Token string

	// VerifyCmd is the project's verify command, run by Worktree.Validate.
	// Default: {"bash", "bin/verify.sh"}.
	VerifyCmd []string

	// DefaultType maps issues with no `type:*` label to this Item.Type
	// value. When empty, those issues are excluded from ListEligible.
	DefaultType string

	// LabelPrefix is the prefix for SDK-managed labels. Default "flow:".
	LabelPrefix string

	// MaxCommentBytes — markdown artifacts longer than this auto-spill to
	// the flow-artifacts orphan branch. Default 60 KiB.
	MaxCommentBytes int

	// WorktreeDir is the local git worktree path. Default ".".
	WorktreeDir string
}

// withDefaults returns a copy of c with empty fields filled in.
func (c Config) withDefaults() Config {
	if c.LabelPrefix == "" {
		c.LabelPrefix = "flow:"
	}
	if c.MaxCommentBytes == 0 {
		c.MaxCommentBytes = 60 * 1024
	}
	if c.WorktreeDir == "" {
		c.WorktreeDir = "."
	}
	if len(c.VerifyCmd) == 0 {
		c.VerifyCmd = []string{"bash", "bin/verify.sh"}
	}
	return c
}

// repoFullName returns "owner/repo" for log + Display strings.
func (c Config) repoFullName() string {
	return c.Owner + "/" + c.Repo
}

// validate returns an error if Config is missing fields NewBackend couldn't
// fill in from the environment.
func (c Config) validate() error {
	missing := []string{}
	if c.Owner == "" {
		missing = append(missing, "Owner")
	}
	if c.Repo == "" {
		missing = append(missing, "Repo")
	}
	if c.BinaryName == "" {
		missing = append(missing, "BinaryName")
	}
	if c.Token == "" {
		missing = append(missing, "Token")
	}
	if len(missing) > 0 {
		return fmt.Errorf("github backend: missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// authedContext returns ctx unchanged; reserved for future per-call auth
// injection if the go-github client API ever changes shape.
func (c Config) authedContext(ctx context.Context) context.Context { return ctx }
