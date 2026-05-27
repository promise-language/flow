package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// resolveToken returns a GitHub token, preferring the explicit override,
// then `gh auth token`, then GITHUB_TOKEN.
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

// resolveLogin returns the authenticated gh login (used as the claim
// owner when the caller didn't override). Falls back to "" when gh is
// absent — the cli/app.go layer derives a fallback from $USER.
func resolveLogin(ctx context.Context) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", nil
	}
	// `gh api user --jq .login` is the simplest path.
	out, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
