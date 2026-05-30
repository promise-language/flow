package common

import (
	"fmt"
	"os"
	"strings"
)

// RunPrecommit enforces fast, test-free invariants before a commit. It is the
// body of bin/precommit, which .githooks/pre-commit execs. Keep it sub-second:
// anything that needs to run tests belongs in bin/verify, which the developer
// runs explicitly — putting tests here makes commits slow and gets the hook
// disabled, which kills the whole gate.
//
// It runs two checks: no staged built binaries under bin/, and that the author
// and committer identities are GitHub noreply addresses (see noreplyDomain).
// Grow it with forbidden-pattern scans and ratcheted-baseline checks.
func RunPrecommit(repoRoot string) error {
	if err := checkNoStagedBinaries(repoRoot); err != nil {
		return err
	}
	if err := checkNoreplyIdentity(repoRoot); err != nil {
		return err
	}
	return nil
}

func checkNoStagedBinaries(repoRoot string) error {
	// --name-status so we can ignore deletions: removing a previously-tracked
	// path under bin/ (e.g. converting an old script to a built tool) is fine;
	// only adding or modifying a binary there is the offense.
	staged, err := RunOutputIn(repoRoot, "git", "diff", "--cached", "--name-status")
	if err != nil {
		return fmt.Errorf("listing staged files: %w", err)
	}
	var offenders []string
	for _, line := range strings.Split(staged, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || strings.HasPrefix(fields[0], "D") {
			continue
		}
		f := fields[len(fields)-1] // path (last field; handles rename's dst too)
		if f == "bin" || strings.HasPrefix(f, "bin/") {
			offenders = append(offenders, f)
		}
	}
	if len(offenders) > 0 {
		fmt.Fprintln(os.Stderr, "pre-commit: refusing to commit built binaries:")
		for _, f := range offenders {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
		fmt.Fprintf(os.Stderr, "bin/ is gitignored and built by ./make. Unstage with:\n  git reset HEAD %s\n",
			strings.Join(offenders, " "))
		return fmt.Errorf("%d staged binary path(s)", len(offenders))
	}
	return nil
}

// noreplyDomain is the only email domain permitted for commit identities. Using
// a GitHub noreply address keeps a personal email out of the public history.
const noreplyDomain = "@users.noreply.github.com"

// checkNoreplyIdentity refuses the commit unless both the author and committer
// emails are GitHub noreply addresses. It reads the identities via 'git var',
// which resolves them exactly as the impending commit will — honoring
// GIT_AUTHOR_EMAIL / GIT_COMMITTER_EMAIL env vars and user.email config alike —
// so the check matches what would actually be recorded.
func checkNoreplyIdentity(repoRoot string) error {
	roles := []struct{ label, gitVar string }{
		{"author", "GIT_AUTHOR_IDENT"},
		{"committer", "GIT_COMMITTER_IDENT"},
	}
	for _, r := range roles {
		ident, err := RunOutputIn(repoRoot, "git", "var", r.gitVar)
		if err != nil {
			return fmt.Errorf("reading %s identity: %w", r.label, err)
		}
		email := identEmail(ident)
		if !strings.HasSuffix(strings.ToLower(email), noreplyDomain) {
			fmt.Fprintf(os.Stderr, "pre-commit: %s identity %q is not a %s address.\n", r.label, email, noreplyDomain)
			fmt.Fprintf(os.Stderr, "Set a GitHub noreply email, e.g.:\n  git config user.email \"<id>+<user>%s\"\n", noreplyDomain)
			return fmt.Errorf("%s email %q lacks %s", r.label, email, noreplyDomain)
		}
	}
	return nil
}

// identEmail extracts the address from a git ident string of the form
// "Name <email> <timestamp> <tz>". It returns "" if no <…> field is present.
func identEmail(ident string) string {
	open := strings.IndexByte(ident, '<')
	close := strings.IndexByte(ident, '>')
	if open < 0 || close < open {
		return ""
	}
	return ident[open+1 : close]
}
