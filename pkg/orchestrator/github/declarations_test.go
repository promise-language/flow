package github

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

// The listing a program reads is asked for. A project tool decides how to
// render a result from whether its stdout is a terminal, and this query's
// never is — so an entry point asked the bare question hands back the JSON it
// writes for programs, and a reader expecting lines finds no gate in it. That
// empty list is indistinguishable from an unbuilt checkout, and what the
// operator is told is to build tools that are already there.
func TestSupportedGates_AsksForTheMachineReadableListing(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, `case "$*" in
"--list --json") printf '{"gates":[{"name":"fit","summary":"a"},{"name":"integration","summary":"b"},{"name":"tested","summary":"c"},{"name":"not-a-gate","summary":"d"}]}\n' ;;
*) echo "gate: unknown flag" >&2; exit 2 ;;
esac`)

	got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
	if strings.Join(names2(got), ",") != "fit,integration,tested" {
		t.Errorf("SupportedGates() = %v, want the three the listing named (and not the unknown one)", names2(got))
	}
	// The contract's two are marked required; the project's own are not — the
	// same property the bare-name path has always had, over the other wire.
	for _, g := range got {
		wantRequired := g.Name == flow.GateFit || g.Name == flow.GateIntegration
		if g.Required != wantRequired {
			t.Errorf("gate %q required = %v, want %v", g.Name, g.Required, wantRequired)
		}
	}
}

// An entry point that predates the flag refuses it, and refusing it is not
// saying there are no gates. The question is asked again without it, which is
// what keeps every project that has not upgraded yet runnable — this one
// included, until #415 lands.
func TestSupportedGates_FallsBackWhenTheFlagIsRefused(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, `case "$*" in
"--list") printf 'fit\nintegration\ntested\n' ;;
*) echo "gate: unknown flag -json" >&2; exit 2 ;;
esac`)

	got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
	if strings.Join(names2(got), ",") != "fit,integration,tested" {
		t.Errorf("SupportedGates() = %v, want the three the entry point named", names2(got))
	}
}

// The fallback reads the JSON too, and that is the half of this that cannot
// retire with the second spawn: an entry point old enough to refuse --json
// still renders for a pipe, so the bare query answers in JSON on exactly the
// machines the fallback exists for.
func TestSupportedGates_FallbackReadsEitherForm(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, `case "$*" in
"--list") printf '{"gates":[{"name":"fit"},{"name":"integration"}]}\n' ;;
*) exit 2 ;;
esac`)

	got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
	if strings.Join(names2(got), ",") != "fit,integration" {
		t.Errorf("SupportedGates() = %v, want the two the listing named", names2(got))
	}
}

// The second spawn is a fallback, not a second question. An entry point that
// answers the flagged query is asked once: asking both every time would make
// every conformant project pay a process spawn at startup for a refusal path
// it never takes, and would hide an ordering mistake — asking the legacy form
// first works just as well until the day the fallback is removed (#414).
func TestSupportedGates_TheFlagIsNotAskedTwice(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	writeGateEntryPoint(t, dir, `printf 'asked\n' >> `+filepath.Join(dir, "asked")+`
case "$*" in
"--list --json") printf '{"gates":[{"name":"fit"},{"name":"integration"}]}\n' ;;
*) exit 2 ;;
esac`)

	got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
	if strings.Join(names2(got), ",") != "fit,integration" {
		t.Fatalf("SupportedGates() = %v, want the two the listing named", names2(got))
	}
	asked, err := os.ReadFile(filepath.Join(dir, "asked"))
	if err != nil {
		t.Fatalf("the entry point was never asked: %v", err)
	}
	if spawns := strings.Count(string(asked), "\n"); spawns != 1 {
		t.Errorf("the entry point was spawned %d times, want 1 — the legacy query is a fallback", spawns)
	}
}

// The deadline bounds the question, not each way of asking it. An entry point
// that never answers is killed at declarationTimeout and declares nothing —
// and the fallback does not get a window of its own, because a bound that
// renewed itself per attempt would let a startup query against a wedged entry
// point cost twice what the bound says it can, before any work begins.
func TestSupportedGates_OneDeadlineCoversBothAttempts(t *testing.T) {
	requireRealProcesses(t)
	dir := t.TempDir()
	// `exec` so the shell is replaced rather than left with a child holding
	// stdout: an orphan on that pipe outlives the kill, and the read waits for
	// it rather than for the deadline this test is about.
	writeGateEntryPoint(t, dir, `printf 'asked\n' >> `+filepath.Join(dir, "asked")+`
exec sleep 30`)

	start := time.Now()
	got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
	elapsed := time.Since(start)

	if len(got) != 0 {
		t.Errorf("SupportedGates() = %v, want none — the entry point never said what it supports", names2(got))
	}
	asked, err := os.ReadFile(filepath.Join(dir, "asked"))
	if err != nil {
		t.Fatalf("the entry point was never asked: %v", err)
	}
	if spawns := strings.Count(string(asked), "\n"); spawns != 1 {
		t.Errorf("the entry point ran %d times, want 1 — the deadline was spent on the first attempt, so the fallback must not start a second process", spawns)
	}
	// Halfway between the two answers: one deadline is what a correct run
	// costs, two is what the regression costs, and neither is near the bound.
	if bound := declarationTimeout + declarationTimeout/2; elapsed >= bound {
		t.Errorf("SupportedGates() took %v, want under %v — one deadline covers the query however many ways it is asked", elapsed, bound)
	}
}

// An answer this SDK cannot read declares NO gates, never the names that
// happen to survive in it. A partial set is worse than an empty one: it says
// `integration` is on a machine whose listing was cut off before the rest of
// it arrived, and the caller that acts on it discovers the truth at the first
// measurement.
//
// Exit 0 is the entry point saying it answered, so there is no second question
// to ask — the fallback is for a refused flag, not for a reply that did not
// parse.
func TestSupportedGates_AnAnswerItCannotReadDeclaresNothing(t *testing.T) {
	requireRealProcesses(t)
	for _, c := range []struct{ what, stdout string }{
		{"a listing truncated mid-write", `{"gates":[{"name":"fit"},{"name":"integrat`},
		{"an array where the listing object belongs", `[{"name":"fit"},{"name":"integration"}]`},
		{"an object carrying the names under other keys", `{"gate_names":["fit","integration"]}`},
	} {
		t.Run(c.what, func(t *testing.T) {
			dir := t.TempDir()
			writeGateEntryPoint(t, dir, `printf '%s' '`+c.stdout+`'`)

			if got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates(); len(got) != 0 {
				t.Errorf("SupportedGates() = %v, want none — %s is not a listing", names2(got), c.what)
			}
		})
	}
}

// Exit 0 is the entry point saying it answered, and an answer is not asked
// again — not when the listing declares no gates, and not when it is one this
// SDK cannot read.
//
// The fallback exists for a REFUSED FLAG. Widening it to "the reply was not
// what I hoped for" restores this defect from the other side: an entry point
// that answered the flagged query with an object the SDK failed to decode
// would be asked the bare question and have its HUMAN RENDERING read instead
// — the exact inversion the flagged query exists to delete, and silent,
// because the gate list that came back would look right.
//
// The bare query here answers with names, so a second ask shows up in the
// result and not only in the spawn count.
func TestSupportedGates_AnAnsweredQueryIsNotAskedAgain(t *testing.T) {
	requireRealProcesses(t)
	for _, c := range []struct{ what, stdout string }{
		{"a listing that declares no gates", `{"gates":[]}`},
		{"an answer this SDK cannot read", `not a listing`},
	} {
		t.Run(c.what, func(t *testing.T) {
			dir := t.TempDir()
			writeGateEntryPoint(t, dir, `printf 'asked\n' >> `+filepath.Join(dir, "asked")+`
case "$*" in
"--list --json") printf '%s' '`+c.stdout+`' ;;
*) printf 'fit\nintegration\n' ;;
esac`)

			got := (&Orchestrator{cfg: Config{WorktreeDir: dir}}).SupportedGates()
			if len(got) != 0 {
				t.Errorf("SupportedGates() = %v, want none — the entry point answered, and %s is what it said",
					names2(got), c.what)
			}
			asked, err := os.ReadFile(filepath.Join(dir, "asked"))
			if err != nil {
				t.Fatalf("the entry point was never asked: %v", err)
			}
			if spawns := strings.Count(string(asked), "\n"); spawns != 1 {
				t.Errorf("the entry point was spawned %d times, want 1 — the fallback is for a refused flag, not for an answer the SDK did not like", spawns)
			}
		})
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
