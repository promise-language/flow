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

// Every subcommand (incl. aliases) handles --help/-help/-h/--h by printing its
// usage and exiting 0 WITHOUT executing. The exit-0 is the tell: without
// interception these args would fail flag parsing (claim -> 2) or run the
// command (run-step with no claim -> 1).
func TestPerCommandHelp_PrintsAndDoesNotExecute(t *testing.T) {
	commands := []string{
		"doctor", "list", "claim", "lease", "release", "reseed",
		"status", "grant", "run-step", "resolve", "run-all",
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
