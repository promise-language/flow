package github

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// Commands are read from the machine: one main per command under
// tools/build/cmd. A directory that is not one of the three contract names is
// not a command — the same directory holds this project's other tools, and
// declaring `guard` or `make` as a command would offer a caller something no
// Run can dispatch.
func TestSupportedCommands_ReadsTheCommandDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"verify", "setup", "gate", "guard", "make", "run"} {
		if err := os.MkdirAll(filepath.Join(dir, "tools", "build", "cmd", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file, not a directory: one main per command means a directory.
	if err := os.WriteFile(filepath.Join(dir, "tools", "build", "cmd", "cleanup"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := names((&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedCommands())
	want := []string{"setup", "verify"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SupportedCommands() = %v, want %v", got, want)
	}
}

// No command directory is a machine with no commands, reported as such. It is
// the case `doctor` exists to name: a checkout that never built its tools, or a
// worktree pointed somewhere unexpected.
func TestSupportedCommands_MissingDirectoryDeclaresNothing(t *testing.T) {
	got := (&Orchestrator{cfg: Config{WorktreeDir: t.TempDir()}}).SupportedCommands()
	if len(got) != 0 {
		t.Errorf("SupportedCommands() = %v, want none — there is no command directory", got)
	}
	if flow.HasCommand(got, flow.CommandVerify) {
		t.Error("verify was declared on a machine that does not have it")
	}
}

// Gates come from the entry point itself, one name per line. Nothing here holds
// a list of what this project can measure, so nothing here can go stale.
func TestSupportedGates_AsksTheEntryPoint(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, "printf 'fit\nintegration\ntested\nnot-a-gate\n\n'")

	got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
	if strings.Join(names2(got), ",") != "fit,integration,tested" {
		t.Errorf("SupportedGates() = %v, want the three the entry point named (and not the unknown one)",
			names2(got))
	}
	// The contract's two are marked required; the project's own are not.
	for _, g := range got {
		wantRequired := g.Name == flow.GateFit || g.Name == flow.GateIntegration
		if g.Required != wantRequired {
			t.Errorf("gate %q required = %v, want %v", g.Name, g.Required, wantRequired)
		}
	}
}

// An absent entry point is a machine with no gates. Reporting it as such is
// what lets `doctor` say so — and what stops a step being dispatched against a
// measurement that could never have happened.
func TestSupportedGates_AbsentEntryPointDeclaresNothing(t *testing.T) {
	requireRealProcesses(t)
	got := (&Orchestrator{cfg: Config{WorktreeDir: t.TempDir()}}).SupportedGates()
	if len(got) != 0 {
		t.Errorf("SupportedGates() = %v, want none — there is no gate entry point", got)
	}
}

// An entry point that runs and fails has not said what it supports. Its output
// is not a list, and reading one out of it would declare gates on the strength
// of an error message.
func TestSupportedGates_FailingEntryPointDeclaresNothing(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, "printf 'fit\n'; exit 1")

	if got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates(); len(got) != 0 {
		t.Errorf("SupportedGates() = %v, want none — the entry point failed", got)
	}
}

// The answer is read once. A binary whose idea of what it can run changed
// mid-run would have startup validation and `doctor` disagreeing about the same
// machine, with no way to tell which looked.
func TestDeclarations_AreReadOncePerOrchestrator(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, "printf 'fit\n' >> "+filepath.Join(dir, "asked")+"; printf 'fit\nintegration\n'")

	b := &Orchestrator{cfg: Config{WorktreeDir: dir}}
	for i := 0; i < 3; i++ {
		b.SupportedGates()
	}
	asked, err := os.ReadFile(filepath.Join(dir, "asked"))
	if err != nil {
		t.Fatalf("the entry point was never asked: %v", err)
	}
	if lines := strings.Count(string(asked), "\n"); lines != 1 {
		t.Errorf("the entry point was asked %d times, want 1", lines)
	}
}

// writeGateEntryPoint installs a bin/gate that answers --list with body.
func writeGateEntryPoint(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "gate"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the entry point is a shell script")
	}
}

func names(defs []flow.CommandDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, string(d.Name))
	}
	return out
}

func names2(defs []flow.GateDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, string(d.Name))
	}
	return out
}
