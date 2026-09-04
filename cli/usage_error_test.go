package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// Every malformed invocation prints exactly two lines on stderr: what was
// wrong, and where the usage is. The line COUNT is the assertion that kills
// the stdlib's flag dump — a substring check on the first line would pass with
// twenty lines of flag definitions underneath it.
func checkUsageError(t *testing.T, what, stdout, stderr string, code int, wantReason string) {
	t.Helper()
	if code != 2 {
		t.Errorf("%s: exit = %d, want 2", what, code)
	}
	if stdout != "" {
		t.Errorf("%s: stdout = %q, want empty", what, stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%s: stderr = %q, want exactly 2 lines (reason + pointer), got %d", what, stderr, len(lines))
	}
	if lines[0] != wantReason {
		t.Errorf("%s: reason line = %q, want %q", what, lines[0], wantReason)
	}
	checkPointerLine(t, what, lines[1])
	if strings.Contains(stderr, "usage:") {
		t.Errorf("%s: stderr = %q, want no usage dump", what, stderr)
	}
}

// The pointer names the ACTUAL binary — an absolute path (or one abbreviated
// to ~), never a placeholder and never a bare command name.
func checkPointerLine(t *testing.T, what, line string) {
	t.Helper()
	const prefix, suffix = "run `", " --help` for usage"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		t.Fatalf("%s: pointer line = %q, want %q…%q", what, line, prefix, suffix)
	}
	path := line[len(prefix) : len(line)-len(suffix)]
	switch {
	case path == "<binary>":
		t.Errorf("%s: pointer names a placeholder %q", what, path)
	case !strings.HasPrefix(path, "~") && !filepath.IsAbs(path):
		t.Errorf("%s: pointer path = %q, want absolute or ~-rooted", what, path)
	case !strings.ContainsRune(path, filepath.Separator):
		t.Errorf("%s: pointer path = %q, want a path, not a bare command name", what, path)
	}
}

// Every command rejects an unrecognised flag the same way.
func TestUnknownFlag_NamedOnEveryCommand(t *testing.T) {
	for _, cmd := range []string{"doctor", "list", "claim", "release", "status", "run-step", "resolve", "grant"} {
		t.Run(cmd, func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, []string{cmd, "--bogus"})
			checkUsageError(t, cmd+" --bogus", out.String(), errBuf.String(), code,
				cmd+": use of unknown flag --bogus")
		})
	}
}

// The flag is quoted back as the operator spelled it — their own dash prefix,
// and without the value they attached to it.
func TestUnknownFlag_SpellingVariants(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"--bogus", "--bogus"},
		{"-bogus", "-bogus"},
		{"--bogus=1", "--bogus"},
		{"-bogus=1", "-bogus"},
		{"--bogus=a=b", "--bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, []string{"list", tc.arg})
			checkUsageError(t, "list "+tc.arg, out.String(), errBuf.String(), code,
				"list: use of unknown flag "+tc.want)
		})
	}
}

// A KNOWN flag with an unusable value is the same kind of failure, and takes
// the same shape — the walk lets it through and fs.Parse catches it.
func TestUnknownFlag_BadFlagValue(t *testing.T) {
	app, out, errBuf := newArgparseApp(t)
	code := app.cmdGrant(t.Context(), []string{"--invocations", "abc", "plan"})
	checkUsageError(t, "grant --invocations abc", out.String(), errBuf.String(), code,
		`grant: invalid value "abc" for flag -invocations: parse error`)
}

// A non-bool flag left without a value at the end of the line.
func TestUnknownFlag_MissingFlagValue(t *testing.T) {
	app, out, errBuf := newArgparseApp(t)
	code := app.cmdGrant(t.Context(), []string{"--invocations"})
	checkUsageError(t, "grant --invocations", out.String(), errBuf.String(), code,
		"grant: flag needs an argument: -invocations")
}

func TestUsageError_UnknownCommand(t *testing.T) {
	app, out, errBuf := newArgparseApp(t)
	app.Name = "issue"
	code := RunWithArgs(*app, []string{"frobnicate"})
	checkUsageError(t, "frobnicate", out.String(), errBuf.String(), code,
		`issue: unknown command "frobnicate"`)
}

func TestUsageError_NoCommand(t *testing.T) {
	app, out, errBuf := newArgparseApp(t)
	app.Name = "issue"
	code := RunWithArgs(*app, nil)
	checkUsageError(t, "(no args)", out.String(), errBuf.String(), code,
		"issue: no command given")
}

// Wrong positional arity, on each of the three shapes: takes none, takes
// exactly one, takes an optional one.
func TestUsageError_Arity(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"doctor", "oops"}, `doctor: unexpected argument "oops" (this command takes no arguments)`},
		{[]string{"list", "oops"}, `list: unexpected argument "oops" (this command takes no arguments)`},
		{[]string{"run-step", "oops"}, `run-step: unexpected argument "oops" (this command takes no arguments)`},
		{[]string{"claim"}, "claim: missing item id (e.g., `claim 42`)"},
		{[]string{"claim", "42", "extra"}, `claim: unexpected argument "extra" (claim takes exactly one item id)`},
		{[]string{"status", "42", "extra"}, `status: unexpected argument "extra" (status takes an optional item id)`},
		{[]string{"resolve", "42", "extra"}, `resolve: unexpected argument "extra" (resolve takes an optional item id)`},
		{[]string{"grant", "plan", "extra"}, `grant: unexpected argument "extra" (grant takes at most one step id)`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, tc.args)
			checkUsageError(t, strings.Join(tc.args, " "), out.String(), errBuf.String(), code, tc.want)
		})
	}
}

// Contradictory options: --json --human on every command that takes them, and
// grant's --all against an explicit step id.
func TestUsageError_ContradictoryOptions(t *testing.T) {
	for _, cmd := range []string{"list", "status", "grant", "resolve", "run-step"} {
		t.Run(cmd, func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, []string{cmd, "--json", "--human"})
			checkUsageError(t, cmd+" --json --human", out.String(), errBuf.String(), code,
				cmd+": --json and --human are mutually exclusive")
		})
	}
	t.Run("grant --all with a step id", func(t *testing.T) {
		app, out, errBuf := newArgparseApp(t)
		code := RunWithArgs(*app, []string{"grant", "--all", "plan"})
		checkUsageError(t, "grant --all plan", out.String(), errBuf.String(), code,
			`grant: --all sweeps every pending step; it cannot be combined with the step id "plan"`)
	})
	t.Run("grant negative amount", func(t *testing.T) {
		app, out, errBuf := newArgparseApp(t)
		code := RunWithArgs(*app, []string{"grant", "--invocations", "-1", "plan"})
		checkUsageError(t, "grant --invocations -1", out.String(), errBuf.String(), code,
			"grant: --invocations / --prompts / --cost / --timeout must be >= 0")
	})
	t.Run("grant empty step id", func(t *testing.T) {
		app, out, errBuf := newArgparseApp(t)
		code := RunWithArgs(*app, []string{"grant", " "})
		checkUsageError(t, `grant " "`, out.String(), errBuf.String(), code, "grant: empty step id")
	})
}

// Naming a step id with no amount is malformed in the same way, and reaches
// the shared shape from inside grant's planner rather than from parseArgs.
func TestUsageError_GrantWithoutAnAmount(t *testing.T) {
	app, out, errBuf, _ := grantTestSetup(t)
	code := app.cmdGrant(t.Context(), []string{"plan"})
	checkUsageError(t, "grant plan", out.String(), errBuf.String(), code,
		"grant: at least one of --invocations / --prompts / --cost / --timeout must be set")
}

// The other half of the rule: usage IS printed when it is asked for — on
// stdout, exit 0, with stderr left empty.
func TestHelp_StillPrintsUsage(t *testing.T) {
	invocations := [][]string{
		{"--help"}, {"-h"}, {"help"},
		{"help", "grant"},
		{"grant", "--help"}, {"resolve", "--help"}, {"claim", "-h"},
	}
	for _, args := range invocations {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, args)
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "usage:") {
				t.Errorf("stdout = %q, want usage text", out.String())
			}
			if errBuf.Len() != 0 {
				t.Errorf("stderr = %q, want empty", errBuf.String())
			}
		})
	}
}

func TestAbbreviateHome(t *testing.T) {
	cases := []struct {
		name, path, home, want string
	}{
		{"under home", "/home/djabi/prog/flow/bin/issue", "/home/djabi", "~/prog/flow/bin/issue"},
		{"home itself", "/home/djabi", "/home/djabi", "~"},
		{"trailing separator on home", "/home/djabi/bin", "/home/djabi/", "~/bin"},
		{"sibling sharing the prefix", "/home/djabiXtra/bin", "/home/djabi", "/home/djabiXtra/bin"},
		{"outside home", "/usr/local/bin/issue", "/home/djabi", "/usr/local/bin/issue"},
		{"no home known", "/usr/local/bin/issue", "", "/usr/local/bin/issue"},
		{"home is the root", "/usr/local/bin/issue", "/", "/usr/local/bin/issue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := abbreviateHome(tc.path, tc.home); got != tc.want {
				t.Errorf("abbreviateHome(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
			}
		})
	}
}

// selfPath resolves THIS process's own executable, so the pointer sends the
// reader to the binary that actually produced the error.
func TestSelfPath_NamesThisExecutable(t *testing.T) {
	got := selfPath("fallback")
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Base(exe)) {
		t.Errorf("selfPath = %q, want it to end in %q", got, filepath.Base(exe))
	}
	if !filepath.IsAbs(got) && !strings.HasPrefix(got, "~") {
		t.Errorf("selfPath = %q, want absolute or ~-rooted", got)
	}
}

// The rejection is position-independent. Every existing unknown-flag case puts
// the bad flag first; the walk has to keep checking after a positional too, or
// `grant plan --bogus` runs the grant and the operator never learns the flag
// did nothing.
func TestUnknownFlag_AfterAPositional(t *testing.T) {
	cases := [][]string{
		{"grant", "plan", "--bogus"},
		{"grant", "--invocations", "3", "plan", "--bogus"},
		{"status", "42", "--bogus"},
		{"resolve", "42", "--bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, args)
			checkUsageError(t, strings.Join(args, " "), out.String(), errBuf.String(), code,
				args[0]+": use of unknown flag --bogus")
		})
	}
}

// "--" wins over the unknown-flag check. The check lives in the same walk that
// handles the terminator, so ordering them wrong is a one-line mistake — and
// it would leave an operator whose argument is literally spelled "--bogus"
// with no way to pass it at all.
func TestUnknownFlag_TerminatorEndsFlagChecking(t *testing.T) {
	app, out, errBuf := newArgparseApp(t)
	code := RunWithArgs(*app, []string{"list", "--", "--bogus"})
	checkUsageError(t, "list -- --bogus", out.String(), errBuf.String(), code,
		`list: unexpected argument "--bogus" (this command takes no arguments)`)
}

// Everything quoted back is operator input, and none of it is a format string.
// usageError takes (format, args...) precisely so a "%" the operator typed
// survives; a message assembled by concatenation, or a wrapper that rewrites
// the format, prints "%!d(MISSING)" at them instead.
func TestUsageError_OperatorTextIsNotAFormatString(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"100%d"}, `issue: unknown command "100%d"`},
		{[]string{"list", "--bogus%s"}, "list: use of unknown flag --bogus%s"},
		{[]string{"list", "100%d"}, `list: unexpected argument "100%d" (this command takes no arguments)`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			app, out, errBuf := newArgparseApp(t)
			app.Name = "issue"
			code := RunWithArgs(*app, tc.args)
			checkUsageError(t, strings.Join(tc.args, " "), out.String(), errBuf.String(), code, tc.want)
		})
	}
}

// Exit 2 "before the command takes any action" is a claim about side effects,
// not just about the exit code: an unknown flag on `claim` must leave the item
// unclaimed. The fake backend would happily claim it, so a rejection moved
// after the action would still exit 2 and pass every message assertion.
func TestUnknownFlag_ClaimsNothing(t *testing.T) {
	be := fake.New()
	be.AddItem("42", flow.Item{Type: "task", Title: "42"})
	app := &App{
		Orchestrator: be,
		Agent:        &stubAgent{name: "stub"},
		Artifacts:    []flow.ArtifactDef{flow.Artifact("plan", flow.ArtifactMarkdown)},
		Flows:        []*flow.Flow{newDummyFlow("x")},
		Out:          &bytes.Buffer{},
		Err:          &bytes.Buffer{},
	}
	out, errBuf := app.Out.(*bytes.Buffer), app.Err.(*bytes.Buffer)

	code := RunWithArgs(*app, []string{"claim", "42", "--bogus"})
	checkUsageError(t, "claim 42 --bogus", out.String(), errBuf.String(), code,
		"claim: use of unknown flag --bogus")

	claim, err := be.LookupActiveClaim(context.Background())
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if claim != nil {
		t.Errorf("item %q was claimed; a malformed invocation must act on nothing", claim.ItemRef.Display)
	}
}
