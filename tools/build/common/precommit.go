package common

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	if err := checkNoAgentExec(repoRoot); err != nil {
		return err
	}
	return nil
}

// agentExec matches a call that would run the agent binary as a subprocess.
var agentExec = regexp.MustCompile(`exec\.Command(?:Context)?\([^)]*"claude"`)

// checkNoAgentExec refuses a commit that lets anything but the claude package
// spawn the agent, and refuses any test that spawns it at all.
//
// A test must never run the agent. It costs money, it takes as long as a model
// takes, and it makes the gate's runtime a function of account state rather
// than of the tree — a red gate then says nothing about the change. This is not
// hypothetical: `discoverAPIBase` ran `claude config get apiBaseUrl` on the path
// every resolve took, `config` is not a subcommand, so the argv was taken as a
// PROMPT and each caller spawned a full agent turn — unbounded, untimed, and
// reached from `go test`, which took bin/verify from two seconds to minutes.
//
// Correcting those arguments would not have been the fix. The defect was that a
// path from reading a config value to starting an agent turn existed at all, so
// the rule is structural: only the package whose job is invoking the agent may
// invoke it, and no test may. Everywhere else, what cannot be learned by
// reading is not learned.
//
// It is a pattern scan, not a proof — a binary named through a variable slips
// past. It catches the honest case, which is the one that keeps happening.
func checkNoAgentExec(repoRoot string) error {
	var bad []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil
		}
		isTest := strings.HasSuffix(rel, "_test.go")
		// The claude package is the one place the agent is invoked from. Its
		// own tests are not exempt: a test there would run the agent too.
		if !isTest && strings.HasPrefix(filepath.ToSlash(rel), "claude/") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if agentExec.MatchString(line) {
				kind := "outside the claude package"
				if isTest {
					kind = "from a test"
				}
				bad = append(bad, fmt.Sprintf("  %s:%d (%s)", rel, i+1, kind))
			}
		}
		return nil
	})
	if err != nil || len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("the agent binary is spawned where it must not be:\n%s\n\nA test must never run the agent, and nothing but the claude package may spawn it.\nWhat cannot be learned by reading configuration is not learned here.", strings.Join(bad, "\n"))
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
