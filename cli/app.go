// Package cli wires the program-level CLI for a flow binary. A binary's
// main() builds a cli.App and calls cli.Run(app); the SDK dispatches
// claim|run-step|release|status|grant|doctor|list against os.Args.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/promise-language/flow"
)

// App is the program-level wiring a binary supplies to cli.Run.
type App struct {
	// Name is the optional binary name (e.g., "implement"). When empty,
	// derived from os.Args[0]. Used by the backend for binary-scoped
	// labeling and by status output.
	Name string

	// Orchestrator is what the SDK talks to: it leases items to arenas, holds
	// their state, runs gates and commands in a worktree, and lands what those
	// produce. Required.
	Orchestrator flow.Orchestrator

	// Agent is what ctx.Agent() returns to handlers, wrapped in the
	// SDK-metered chokepoint. Required.
	Agent flow.Agent

	// Artifacts is the closed set of artifact definitions all flows in this
	// binary may reference. Required (non-empty).
	Artifacts []flow.ArtifactDef

	// Signals declares the orchestrator-observed signals this binary cares
	// about. Optional; every referenced signal must appear in the
	// orchestrator's SupportedSignals().
	Signals []flow.SignalDef

	// Gates declares the gates this binary's flows ask for beyond the two every
	// orchestrator must have. Optional; every entry must appear in the
	// orchestrator's SupportedGates(), and startup refuses one that does not —
	// the same rule signals and artifacts are already under, for the same
	// reason: a flow naming a gate nothing can run should fail before an item
	// is claimed.
	Gates []flow.GateName

	// Telemetry is the optional sink for StepCtx.Notify calls. When nil,
	// Notify is a no-op. NOT a liveness signal — see flow.Telemetry's
	// docstring.
	Telemetry flow.Telemetry

	// Preflight is an optional cross-flow gate run on every RunOne dispatch,
	// AFTER Orchestrator.Load and the terminal-done short-circuit, and BEFORE
	// seed / handler dispatch. Returning a non-nil error short-circuits the
	// invocation with status="skipped"; an error wrapping flow.ErrBlocked
	// reports status="blocked" instead, which the CLI exits non-zero on. See
	// flow.PreflightFunc and flow.ErrBlocked for the contract, and
	// flow.ChainPreflight for composing multiple checks.
	Preflight flow.PreflightFunc

	// Flows is the ordered list of flow variants. cli.App picks the first
	// flow whose Types() match item.Type AND RequireSignal preconditions
	// are satisfied AND has at least one pending lifecycle item.
	Flows []*flow.Flow

	// Quota reads the subscription windows `resolve` paces against. **Nil means
	// no pacing**, and that is what makes pacing safe to have at all: it reaches
	// live account state, so a caller that has not asked for it does not get it.
	//
	// A test must never set this. Pacing sleeps for as long as the account says,
	// so a test that paced would take a length of time nobody chose and would
	// make the gate's result a function of usage rather than of the tree — which
	// is what once took bin/verify past its ten-minute package timeout with the
	// tree perfectly sound.
	//
	// Run installs the real reader when it is nil, so the binary paces and a
	// unit test calling a command directly does not.
	Quota func() ([]windowUsage, error)

	// VerifyCmd is the project's verify command (e.g. "bin/verify --wasm" or
	// "make check"). It is the single source of truth a flow binary configures
	// once, here, instead of hardcoding it in each step handler and prompt:
	// handlers read it via StepCtx.VerifyCmd() to run the gate, and pass it into
	// the prompt context so shared, project-agnostic prompt fragments can refer
	// to it. Optional; empty means no verify command is configured.
	VerifyCmd string

	// Out / Err are the cli's output streams. nil means os.Stdout /
	// os.Stderr respectively.
	Out io.Writer
	Err io.Writer

	// CarryThrough is true when the binary runs both the contributor and
	// integration phases in one resolution. Reported by doctor.
	CarryThrough bool

	// Output selects how command results are rendered. The zero value
	// (OutputAuto) decides per invocation: JSON when stdout is piped or
	// redirected, human text on a terminal — so a tool driving this binary
	// never has to parse text meant for a person. --json / --human override it
	// for one command; $FLOW_OUTPUT overrides it for the process.
	Output OutputMode

	// resolved at startup
	artifactById map[flow.ArtifactId]flow.ArtifactDef
	signalById   map[flow.SignalId]flow.SignalDef
}

// Run is the binary's entry point. Parses argv, dispatches the matching
// command, returns an exit code. Idiomatic call site is
// `os.Exit(cli.Run(cli.App{...}))`.
func Run(app App) int {
	args := os.Args[1:]
	return RunWithArgs(app, args)
}

// RunWithArgs is Run with explicit argv. Useful for tests.
func RunWithArgs(app App, args []string) int {
	if app.Out == nil {
		app.Out = os.Stdout
	}
	if app.Err == nil {
		app.Err = os.Stderr
	}
	if app.Name == "" {
		app.Name = deriveBinaryName(os.Args)
	}
	if app.Quota == nil {
		app.Quota = readQuota
	}
	if err := app.validate(); err != nil {
		fmt.Fprintln(app.Err, "startup error:", err)
		return 2
	}

	if len(args) == 0 {
		return app.usageError("%s: no command given", app.Name)
	}

	cmd, rest := args[0], args[1:]

	// Top-level help: `<bin> help`, or a help flag in the command position
	// (-h / --h / -help / --help). `<bin> help <command>` shows that command's
	// usage, mirroring `<bin> <command> --help`.
	if cmd == "help" || isHelpArg(cmd) {
		if cmd == "help" && len(rest) > 0 && isKnownCommand(rest[0]) {
			fmt.Fprintln(app.Out, commandUsage(app.Name, rest[0]))
			return 0
		}
		fmt.Fprintln(app.Out, usage(app.Name))
		return 0
	}
	// Per-command help: a help flag anywhere after a known command (e.g.
	// `<bin> claim --help`) prints that command's usage and does NOT execute
	// it. Handled centrally so every subcommand supports it uniformly; an
	// unknown command still falls through to the dispatch error below.
	if isKnownCommand(cmd) && wantsHelp(rest) {
		fmt.Fprintln(app.Out, commandUsage(app.Name, cmd))
		return 0
	}

	ctx := context.Background()
	switch cmd {
	case "doctor":
		return app.cmdDoctor(ctx, rest)
	case "list":
		return app.cmdList(ctx, rest)
	case "claim":
		return app.cmdClaim(ctx, rest)
	case "release":
		return app.cmdRelease(ctx, rest)
	case "reseed":
		return app.cmdReseed(ctx, rest)
	case "status":
		return app.cmdStatus(ctx, rest)
	case "grant":
		return app.cmdGrant(ctx, rest)
	case "run-step":
		return app.cmdRun(ctx, rest)
	case "answer":
		return app.cmdAnswer(ctx, rest)
	case "resolve":
		return app.cmdResolve(ctx, rest)
	case "stale":
		return app.cmdStale(ctx, rest)
	default:
		return app.usageError("%s: unknown command %q", app.Name, cmd)
	}
}

// validate is the startup gate. Refuses to start (returns a named error) if
// the App is malformed in any way the SDK can detect.
func (app *App) validate() error {
	if app.Orchestrator == nil {
		return errors.New("App.Orchestrator is required")
	}
	if app.Agent == nil {
		return errors.New("App.Agent is required")
	}
	// Wrap the agent so the field cannot be spent from. From here on
	// App.Agent refuses Run(); the real agent is reached only by newStepCtx,
	// through the metered chokepoint it hands a step handler. See
	// outsideStepAgent. Idempotent: validate() may run more than once in a
	// process (tests build several Apps), and wrapping a wrapper would bury
	// the impl one layer deeper on each pass.
	if _, wrapped := app.Agent.(*outsideStepAgent); !wrapped {
		app.Agent = &outsideStepAgent{inner: app.Agent}
	}
	if len(app.Artifacts) == 0 {
		return errors.New("App.Artifacts is empty")
	}
	if len(app.Flows) == 0 {
		return errors.New("App.Flows is empty")
	}

	// Build the lookup maps and check for duplicate ids.
	app.artifactById = make(map[flow.ArtifactId]flow.ArtifactDef, len(app.Artifacts))
	for _, ad := range app.Artifacts {
		if ad.Id == "" {
			return errors.New("App.Artifacts contains an entry with empty Id")
		}
		if _, dup := app.artifactById[ad.Id]; dup {
			return fmt.Errorf("duplicate artifact id %q in App.Artifacts", ad.Id)
		}
		app.artifactById[ad.Id] = ad
	}
	app.signalById = make(map[flow.SignalId]flow.SignalDef, len(app.Signals))
	for _, sd := range app.Signals {
		if sd.Id == "" {
			return errors.New("App.Signals contains an entry with empty Id")
		}
		if _, dup := app.signalById[sd.Id]; dup {
			return fmt.Errorf("duplicate signal id %q in App.Signals", sd.Id)
		}
		app.signalById[sd.Id] = sd
	}

	// Every declared signal must be supported by the orchestrator.
	supported := map[flow.SignalId]struct{}{}
	for _, sd := range app.Orchestrator.SupportedSignals() {
		supported[sd.Id] = struct{}{}
	}
	for _, sd := range app.Signals {
		if _, ok := supported[sd.Id]; !ok {
			return fmt.Errorf("signal %q declared but not in Orchestrator.SupportedSignals() (orchestrator %q)", sd.Id, app.Orchestrator.Name())
		}
	}

	// Gates and commands are declared and validated at startup, exactly as
	// signals and artifacts are. A flow naming a gate nothing can run fails
	// BEFORE an item is claimed rather than part-way through one, when a step
	// has already burned a turn getting there.
	gates := app.Orchestrator.SupportedGates()
	for _, want := range flow.RequiredGates() {
		if !flow.HasGate(gates, want) {
			return fmt.Errorf("orchestrator %q does not declare the required gate %q", app.Orchestrator.Name(), want)
		}
	}
	commands := app.Orchestrator.SupportedCommands()
	for _, want := range flow.RequiredCommands() {
		if !flow.HasCommand(commands, want) {
			return fmt.Errorf("orchestrator %q does not declare the required command %q", app.Orchestrator.Name(), want)
		}
	}
	for _, gd := range gates {
		if !gd.Name.Valid() {
			return fmt.Errorf("orchestrator %q declares gate %q, which is not a valid gate name", app.Orchestrator.Name(), gd.Name)
		}
	}
	for _, cd := range commands {
		if !cd.Name.Valid() {
			return fmt.Errorf("orchestrator %q declares command %q, which is not one of the three command names", app.Orchestrator.Name(), cd.Name)
		}
	}
	// Every gate a flow names must be one the orchestrator can run.
	for _, name := range app.Gates {
		if !flow.HasGate(gates, name) {
			return fmt.Errorf("gate %q declared in App.Gates but not in Orchestrator.SupportedGates() (orchestrator %q)", name, app.Orchestrator.Name())
		}
	}

	// Every declared artifact must be in the orchestrator's canonical schema — by
	// id AND type. Symmetric with the signal check above: a flow whose declared
	// artifact the backend cannot persist (unknown id, or a type that
	// disagrees with the backend's schema) could never finalize the step that
	// produces it, so refuse at startup (exit 2, every invocation) instead of
	// discovering it at resolve-time after the step has already run. The
	// per-flow loop below guarantees every step result is in App.Artifacts, so
	// checking the declared set here transitively covers every step result.
	recordable := map[flow.ArtifactId]flow.ArtifactDef{}
	for _, ad := range app.Orchestrator.SupportedArtifacts() {
		recordable[ad.Id] = ad
	}
	for _, ad := range app.Artifacts {
		def, ok := recordable[ad.Id]
		if !ok {
			return fmt.Errorf("artifact %q declared but not in Orchestrator.SupportedArtifacts() (orchestrator %q)", ad.Id, app.Orchestrator.Name())
		}
		if def.Type != ad.Type {
			return fmt.Errorf("artifact %q declared with type %v but orchestrator %q records it as %v", ad.Id, ad.Type, app.Orchestrator.Name(), def.Type)
		}
	}

	// Per-flow validation.
	for _, f := range app.Flows {
		if f == nil {
			return errors.New("App.Flows contains a nil entry")
		}
		if len(f.Items()) == 0 {
			return fmt.Errorf("flow %q has zero lifecycle items", f.Name())
		}
		for _, li := range f.Items() {
			switch li.Kind {
			case flow.LifecycleArtifact:
				if _, ok := app.artifactById[li.ArtifactId]; !ok {
					return fmt.Errorf("flow %q step %q references unknown artifact %q", f.Name(), li.Name, li.ArtifactId)
				}
			case flow.LifecycleSignal, flow.LifecycleAwait:
				if _, ok := app.signalById[li.SignalId]; !ok {
					return fmt.Errorf("flow %q step %q references unknown signal %q (declare it in App.Signals)", f.Name(), li.Name, li.SignalId)
				}
			}
		}
		for _, sig := range f.RequireSignals() {
			if _, ok := app.signalById[sig]; !ok {
				return fmt.Errorf("flow %q RequireSignal(%q) is not declared in App.Signals", f.Name(), sig)
			}
		}
	}

	// Two flows shadowing each other (same Name + overlapping type sets) is
	// a misconfiguration the SDK can detect at startup.
	for i, a := range app.Flows {
		for _, b := range app.Flows[i+1:] {
			if a.Name() != b.Name() {
				continue
			}
			if flowTypesOverlap(a, b) {
				return fmt.Errorf("flows %q and %q share a name and overlapping types — ambiguous", a.Name(), b.Name())
			}
		}
	}
	return nil
}

func flowTypesOverlap(a, b *flow.Flow) bool {
	at, bt := a.Types(), b.Types()
	if len(at) == 0 || len(bt) == 0 {
		// Empty Types means "universal" — overlaps with any other set.
		return true
	}
	for _, t := range at {
		if slices.Contains(bt, t) {
			return true
		}
	}
	return false
}

func deriveBinaryName(argv []string) string {
	if len(argv) == 0 {
		return "flow"
	}
	base := argv[0]
	// Walk back to the last separator.
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' || base[i] == '\\' {
			return base[i+1:]
		}
	}
	return base
}

func usage(bin string) string {
	return fmt.Sprintf(`%[1]s — flow binary

usage:
  %[1]s doctor                       is this environment fit to be given an item?
  %[1]s list [--scope SCOPE] [--tag T] list items this flow can process
  %[1]s answer <item-id> <text>       answer a question a step is parked on
  %[1]s claim <item-id>              acquire a claim on an item
  %[1]s run-step                     advance ONE lifecycle item (one prompt → one artifact)
  %[1]s resolve [<item-id>]          run ALL steps until finalized or parked.
                                     With <item-id>, claims it first; else uses the active claim.
  %[1]s status [<item-id>]           read-only lifecycle checklist. With <item-id>, inspects
                                     that item without claiming it.
  %[1]s grant                        top up the parked step's parked axis (the usual case)
  %[1]s grant --all                  top up every pending step over its consumption
  %[1]s grant <step-id> [--invocations N] [--cost USD] [--prompts N] [--timeout SECONDS]
                                     additively extend one step's budget. <step-id> is the
                                     id from AddStep (e.g. "plan") — the first column of
                                     "status" — never the label (e.g. "write plan")
  %[1]s release                      drop the claim
  %[1]s reseed [--force]              clear seed state (artifacts, budgets, park) on the active claim
  %[1]s stale <step-id>               mark one resolved artifact stale for re-derivation

answer, status, list, grant, stale, run-step, and resolve print human-readable text on a terminal and
JSON when piped or redirected; --json / --human (or FLOW_OUTPUT=json|human)
force one. resolve's human text is its progress narration on stderr, which it
prints in both modes — in human mode it writes nothing to stdout at all.`, bin)
}

// usageError reports a malformed invocation and returns the exit code for one.
// It is the single shape every rejection of an invocation takes: one line
// naming what was wrong, one line saying where the usage is, and exit 2 before
// the command has done anything.
//
// It never prints the usage itself. A wall of flag definitions buries the one
// fact the operator needs, and printing it unprompted trains people to skip it
// on the occasion they did ask for it.
//
// The format string is passed to Fprintf unmodified — the newline is a
// separate Fprintln — because `go vet`'s printf analyzer only recognises a
// printf wrapper that forwards its format verbatim. Folding the "\n" in with
// `format+"\n"` silently turns the check off for every call site below.
func (app *App) usageError(format string, a ...any) int {
	fmt.Fprintf(app.Err, format, a...)
	fmt.Fprintln(app.Err)
	fmt.Fprintf(app.Err, "run `%s --help` for usage\n", selfPath(app.Name))
	return 2
}

// selfPath is how the pointer line names this binary: its own absolute path,
// with $HOME abbreviated to ~. Not a placeholder and not a bare command name —
// a machine carries one of these binaries per project plus whatever is on
// PATH, and a reader who did not choose the invocation (every agent that was
// handed it) cannot otherwise tell which one produced the error, or re-run it
// without guessing.
//
// Falls back to the derived binary name when the executable cannot be
// resolved: a pointer that names something beats no pointer at all.
func selfPath(fallback string) string {
	exe, err := os.Executable()
	if err != nil {
		return fallback
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return fallback
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return abs
	}
	return abbreviateHome(abs, home)
}

// abbreviateHome rewrites a path under home as "~/…". The match is at a
// separator boundary, so a sibling directory whose name merely starts with the
// home path — /home/djabiXtra next to /home/djabi — is left alone rather than
// mangled into "~Xtra".
func abbreviateHome(path, home string) string {
	sep := string(filepath.Separator)
	home = strings.TrimSuffix(home, sep)
	if home == "" {
		return path
	}
	switch {
	case path == home:
		return "~"
	case strings.HasPrefix(path, home+sep):
		return "~" + path[len(home):]
	}
	return path
}

// newFlagSet returns a FlagSet configured for the strict-parsing convention:
// unknown flags are rejected, and every rejection is reported by app.parseArgs
// through usageError. The FlagSet's own output is discarded and its Usage is a
// no-op, which is what suppresses the stdlib's default — a different message
// followed by a dump of every flag the command accepts.
//
// Caller registers any flags, calls app.parseArgs, then validates fs.NArg()
// against the expected positional count.
func (app *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseArgs parses args allowing flags to appear in any position relative to
// positionals — the convention every modern CLI uses, but one the stdlib flag
// package deliberately does not implement. Walks args once, peeling flag
// tokens (and their values) off into a flags slice and pushing the rest into
// positionals, then calls fs.Parse(flags, "--", positionals...) so the stdlib
// parser sees the only order it understands. Bool flags are detected via the
// FlagSet's Lookup + IsBoolFlag so e.g. `claim <id> --force` is parsed
// correctly (no slurping the next token).
//
// That same Lookup is where an unrecognised flag is rejected, by name: the
// walk already has to know whether the flag exists, and reporting it here is
// what lets the message quote the flag as the operator spelled it instead of
// the stdlib's normalized single-dash rendering. Returns false after reporting
// the failure; the caller exits 2.
func (app *App) parseArgs(fs *flag.FlagSet, args []string) bool {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		// Not a flag: bare "-" or anything not starting with "-" is positional.
		if len(a) < 2 || a[0] != '-' {
			positionals = append(positionals, a)
			continue
		}
		// Strip 1 or 2 leading dashes to get the flag name, and cut
		// "--name=value" at the "=" so the value is never mistaken for part of
		// the name. `spelled` keeps the operator's own dashes, minus any value,
		// so a refusal names the flag they typed.
		name := a[1:]
		if name[0] == '-' {
			name = name[1:]
		}
		name, _, hasValue := strings.Cut(name, "=")
		spelled, _, _ := strings.Cut(a, "=")

		fl := fs.Lookup(name)
		if fl == nil {
			app.usageError("%s: use of unknown flag %s", fs.Name(), spelled)
			return false
		}
		flags = append(flags, a)
		// "--name=value" is self-contained; never consume a follow-on token.
		if hasValue {
			continue
		}
		// Peek next token only when the flag is NOT a bool flag — mirrors the
		// stdlib's own internal logic in (*FlagSet).parseOne.
		if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if len(positionals) > 0 {
		flags = append(flags, "--")
		flags = append(flags, positionals...)
	}
	// Whatever the walk let through: a bad flag VALUE (`--invocations abc`), or
	// a flag left without one at the end of the line.
	if err := fs.Parse(flags); err != nil {
		app.usageError("%s: %s", fs.Name(), err)
		return false
	}
	return true
}

// rejectArgs parses args for a command that takes no flags and no
// positionals. Returns true on success; on rejection writes to app.Err and
// returns false (caller exits with code 2).
func (app *App) rejectArgs(name string, args []string) bool {
	fs := app.newFlagSet(name)
	if !app.parseArgs(fs, args) {
		return false
	}
	if fs.NArg() > 0 {
		app.usageError("%s: unexpected argument %q (this command takes no arguments)", name, fs.Arg(0))
		return false
	}
	return true
}
