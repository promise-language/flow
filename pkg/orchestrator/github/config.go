// Package github implements flow.Orchestrator against GitHub Issues + the
// project's own git repo, with no server to host. State lives in a single
// "state comment" per issue (machine-managed YAML), per-artifact comments,
// and an optional flow-artifacts orphan branch for large blobs. Auth piggy-
// backs on the `gh` CLI's stored token.
package github

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/promise-language/flow"
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
	// WorktreeDir.
	Owner string
	Repo  string

	// Token overrides token lookup. When empty, NewBackend tries
	// `gh auth token` then GITHUB_TOKEN.
	Token string

	// Guard examines every byte this backend would send to GitHub and may
	// refuse it. It is REQUIRED to publish anything: with none installed, the
	// reads still work — so `list`, `status` and `doctor` do — and the first
	// write refuses with flow.ErrNoDisclosureGuard.
	//
	// There is no implementation in this repository, deliberately. A guard
	// living in the tree it constrains would be rebuildable by the party it
	// refuses, so it is supplied from outside and injected here, the same way
	// the backend and the agent are. See docs/disclosure.md.
	Guard flow.DisclosureGuard

	// VerifyCmd is the project's verify COMMAND, run by Worktree.Verify. It
	// repairs what has one right answer and then measures, so it may modify the
	// worktree.
	// Default: {"bin/verify"}.
	VerifyCmd []string

	// DefaultType maps issues with no `type:*` label to this Item.Type
	// value. When empty, those issues are excluded from ListEligible.
	DefaultType string

	// LabelPrefix is the prefix for SDK-managed labels. Default "flow:".
	LabelPrefix string

	// MaxCommentBytes — markdown artifacts longer than this auto-spill to
	// the flow-artifacts orphan branch. Default 60 KiB.
	MaxCommentBytes int

	// WorktreeDir is the ABSOLUTE path of the local git worktree. When empty,
	// New derives it from the binary's own location — the checkout the binary
	// lives in (flow.DeriveArenaRoot).
	//
	// A relative value is REFUSED rather than resolved, and there is no default
	// to fill it in with. Every consumer of this field inherits whatever it
	// holds — the arena identity, the claim state, gate and command discovery,
	// the verify command — so a relative value stored here is the process
	// working directory silently becoming four things at once, which is #286.
	WorktreeDir string

	// GateTimeout bounds one gate's work, clock starting at the spawn. A gate
	// that hits it reports OutcomeTimedOut — the one outcome worth retrying
	// unchanged — rather than hanging its runner. Progress does not extend it:
	// otherwise a wedged-but-chatty gate runs until something else kills it,
	// which is the failure the deadline exists to bound.
	//
	// Default: 10 minutes, which sits under DefaultStepBudget().Timeout so a
	// wedged gate is caught by its own deadline rather than by consuming the
	// whole step.
	//
	// This is the DECLARATION SITE ONLY until the thresholds manifest lands
	// (#38). When it does, the timeout moves there and this field goes, rather
	// than becoming a second copy.
	GateTimeout time.Duration
}

// withDefaults returns a copy of c with empty fields filled in.
func (c Config) withDefaults() Config {
	if c.LabelPrefix == "" {
		c.LabelPrefix = "flow:"
	}
	if c.MaxCommentBytes == 0 {
		c.MaxCommentBytes = 60 * 1024
	}
	// WorktreeDir is deliberately NOT filled in here. A default of "." is not a
	// location, and this is the moment it would become one stored in a config
	// field for every reader to inherit. New resolves it instead, through
	// resolveWorktreeDir, which derives or refuses.
	if len(c.VerifyCmd) == 0 {
		c.VerifyCmd = []string{"bin/verify"}
	}
	if c.GateTimeout == 0 {
		c.GateTimeout = 10 * time.Minute
	}
	return c
}

// resolveWorktreeDir turns the configured worktree into the absolute path every
// consumer of the field inherits: empty derives it from the binary's own
// location, absolute passes through, relative is refused.
//
// filepath.Abs is deliberately NOT applied to an explicit relative value.
// Resolving one would re-import the process working directory through the back
// door — the caller would have configured a path and got wherever the operator
// stood — which is the ambient dependency this whole field exists to be free
// of. A caller that means a directory says which one.
func resolveWorktreeDir(dir string) (string, error) {
	if dir == "" {
		root, err := flow.DeriveArenaRoot()
		if err != nil {
			return "", fmt.Errorf("github orchestrator: locate the worktree: %w", err)
		}
		return root, nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf(
			"github orchestrator: Config.WorktreeDir %q is relative — it must be an absolute "+
				"path, or empty to derive the checkout this binary lives in", dir)
	}
	return dir, nil
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
		return fmt.Errorf("github orchestrator: missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// authedContext returns ctx unchanged; reserved for future per-call auth
// injection if the go-github client API ever changes shape.
func (c Config) authedContext(ctx context.Context) context.Context { return ctx }
