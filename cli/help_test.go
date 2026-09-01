package cli

import (
	"strings"
	"testing"
)

func TestIsHelpArg(t *testing.T) {
	for _, s := range []string{"-h", "--h", "-help", "--help"} {
		if !isHelpArg(s) {
			t.Errorf("want help flag: %q", s)
		}
	}
	for _, s := range []string{"", "-x", "help", "--helpme", "-hh", "h"} {
		if isHelpArg(s) {
			t.Errorf("want NOT help flag: %q", s)
		}
	}
}

// Top-level help works for `help` and every flag form/prefix.
func TestTopLevelHelp_AllForms(t *testing.T) {
	for _, tok := range []string{"help", "-h", "--h", "-help", "--help"} {
		app, out, _ := newArgparseApp(t)
		code := RunWithArgs(*app, []string{tok})
		if code != 0 {
			t.Errorf("%s: exit code = %d, want 0", tok, code)
		}
		if !strings.Contains(out.String(), "usage:") {
			t.Errorf("%s: out = %q, want usage text", tok, out.String())
		}
	}
}

// Every subcommand handles --help/-help/-h/--h by printing its usage and
// exiting 0 WITHOUT executing. The exit-0 is the tell: without interception
// these args would fail flag parsing (claim -> 2) or run the command
// (run-step with no claim -> 1).
func TestPerCommandHelp_PrintsAndDoesNotExecute(t *testing.T) {
	commands := []string{
		"doctor", "list", "claim", "release", "reseed",
		"status", "grant", "run-step", "resolve",
	}
	for _, cmd := range commands {
		for _, tok := range []string{"-h", "--h", "-help", "--help"} {
			app, out, errBuf := newArgparseApp(t)
			code := RunWithArgs(*app, []string{cmd, tok})
			if code != 0 {
				t.Errorf("%s %s: exit code = %d, want 0 (err=%q)", cmd, tok, code, errBuf.String())
			}
			if !strings.Contains(out.String(), "usage:") {
				t.Errorf("%s %s: out = %q, want usage text", cmd, tok, out.String())
			}
		}
	}
}

// A help flag after a positional is still caught (e.g. `grant plan --help`).
func TestPerCommandHelp_AfterPositional(t *testing.T) {
	app, out, _ := newArgparseApp(t)
	code := RunWithArgs(*app, []string{"grant", "plan", "--help"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "grant") || !strings.Contains(out.String(), "usage:") {
		t.Errorf("out = %q, want grant usage", out.String())
	}
}

// An unknown command with a help flag is NOT treated as a help request — it
// still errors like any unknown command, rather than printing usage at exit 0.
func TestUnknownCommandWithHelpFlag_StillErrors(t *testing.T) {
	app, _, errBuf := newArgparseApp(t)
	code := RunWithArgs(*app, []string{"frobnicate", "--help"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Errorf("err = %q, want 'unknown command'", errBuf.String())
	}
}

// `<bin> help <command>` mirrors `<bin> <command> --help`.
func TestHelpSubcommand_ShowsCommandUsage(t *testing.T) {
	app, out, _ := newArgparseApp(t)
	code := RunWithArgs(*app, []string{"help", "claim"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "claim") || !strings.Contains(out.String(), "<item-id>") {
		t.Errorf("out = %q, want claim usage", out.String())
	}
}

// Removed aliases ("lease", "run-all") are no longer dispatched — they hit
// the unknown-command path: exit 2, stderr says "unknown command". This
// confirms the spec-required error shape (docs/cli.md:82).
func TestRemovedAliases_AreUnknownCommands(t *testing.T) {
	for _, cmd := range []string{"lease", "run-all"} {
		app, _, errBuf := newArgparseApp(t)
		code := RunWithArgs(*app, []string{cmd})
		if code != 2 {
			t.Errorf("%s: exit code = %d, want 2", cmd, code)
		}
		if !strings.Contains(errBuf.String(), "unknown command") {
			t.Errorf("%s: err = %q, want 'unknown command'", cmd, errBuf.String())
		}
	}
	// A help flag after a removed alias is NOT treated as help — still unknown.
	for _, cmd := range []string{"lease", "run-all"} {
		app, _, errBuf := newArgparseApp(t)
		code := RunWithArgs(*app, []string{cmd, "--help"})
		if code != 2 {
			t.Errorf("%s --help: exit code = %d, want 2", cmd, code)
		}
		if !strings.Contains(errBuf.String(), "unknown command") {
			t.Errorf("%s --help: err = %q, want 'unknown command'", cmd, errBuf.String())
		}
	}
}

// `<bin> help lease` and `<bin> help run-all` fall back to program usage
// (not to the claim/resolve help), confirming that the help subsystem no
// longer folds aliases onto canonical names.
func TestHelpSubcommand_RemovedAliasesFallBackToUsage(t *testing.T) {
	for _, alias := range []string{"lease", "run-all"} {
		app, out, _ := newArgparseApp(t)
		code := RunWithArgs(*app, []string{"help", alias})
		if code != 0 {
			t.Errorf("help %s: exit code = %d, want 0", alias, code)
		}
		got := out.String()
		if !strings.Contains(got, "usage:") {
			t.Errorf("help %s: out = %q, want program usage", alias, got)
		}
		// Must NOT contain per-command detail unique to claim or resolve.
		// "Acquires a claim (lease)" is in claim's detail, not program usage.
		if strings.Contains(got, "Acquires a claim (lease)") {
			t.Errorf("help %s: should not show claim's detail text", alias)
		}
		if strings.Contains(got, "Runs ALL steps until the item is finalized") {
			t.Errorf("help %s: should not show resolve's detail text", alias)
		}
	}
}

// `<bin> help <unknown>` falls back to the program usage (exit 0).
func TestHelpSubcommand_UnknownFallsBackToUsage(t *testing.T) {
	app, out, _ := newArgparseApp(t)
	code := RunWithArgs(*app, []string{"help", "frobnicate"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Errorf("out = %q, want program usage", out.String())
	}
}
