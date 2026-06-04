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

	// Backend implements the storage + worktree boundary. Required.
	Backend flow.Backend

	// Agent is what ctx.Agent() returns to handlers, wrapped in the
	// SDK-metered chokepoint. Required.
	Agent flow.Agent

	// Artifacts is the closed set of artifact definitions all flows in this
	// binary may reference. Required (non-empty).
	Artifacts []flow.ArtifactDef

	// Signals declares the backend-observed signals this binary cares
	// about. Optional; every referenced signal must appear in the backend's
	// SupportedSignals().
	Signals []flow.SignalDef

	// Telemetry is the optional sink for StepCtx.Notify calls. When nil,
	// Notify is a no-op. NOT a liveness signal — see flow.Telemetry's
	// docstring.
	Telemetry flow.Telemetry

	// Preflight is an optional cross-flow gate run on every RunOne
	// dispatch, AFTER Backend.LoadState and BEFORE flow selection.
	// Returning a non-nil error short-circuits the invocation with
	// status="skipped". See flow.PreflightFunc for the contract and
	// flow.ChainPreflight for composing multiple checks.
	Preflight flow.PreflightFunc

	// Flows is the ordered list of flow variants. cli.App picks the first
	// flow whose Types() match item.Type AND RequireSignal preconditions
	// are satisfied AND has at least one pending lifecycle item.
	Flows []*flow.Flow

	// Owner overrides the principal recorded on claims. When empty, falls
	// back to $USER (or "anonymous" if unset). Backends may apply their
	// own canonicalization (e.g., github uses the authenticated gh login).
	Owner string

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
	if app.Owner == "" {
		app.Owner = deriveOwner()
	}
	if err := app.validate(); err != nil {
		fmt.Fprintln(app.Err, "startup error:", err)
		return 2
	}

	if len(args) == 0 {
		fmt.Fprintln(app.Err, usage(app.Name))
		return 2
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
	case "claim", "lease":
		return app.cmdClaim(ctx, rest)
	case "release":
		return app.cmdRelease(ctx, rest)
	case "status":
		return app.cmdStatus(ctx, rest)
	case "grant":
		return app.cmdGrant(ctx, rest)
	case "run-step":
		return app.cmdRun(ctx, rest)
	case "resolve", "run-all":
		return app.cmdResolve(ctx, rest)
	default:
		fmt.Fprintln(app.Err, "unknown command:", cmd)
		fmt.Fprintln(app.Err, usage(app.Name))
		return 2
	}
}

// validate is the startup gate. Refuses to start (returns a named error) if
// the App is malformed in any way the SDK can detect.
func (app *App) validate() error {
	if app.Backend == nil {
		return errors.New("App.Backend is required")
	}
	if app.Agent == nil {
		return errors.New("App.Agent is required")
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

	// Every declared signal must be supported by the backend.
	supported := map[flow.SignalId]struct{}{}
	for _, sd := range app.Backend.SupportedSignals() {
		supported[sd.Id] = struct{}{}
	}
	for _, sd := range app.Signals {
		if _, ok := supported[sd.Id]; !ok {
			return fmt.Errorf("signal %q declared but not in Backend.SupportedSignals() (backend %q)", sd.Id, app.Backend.Name())
		}
	}

	// Every declared artifact must be in the backend's canonical schema — by
	// id AND type. Symmetric with the signal check above: a flow whose declared
	// artifact the backend cannot persist (unknown id, or a type that
	// disagrees with the backend's schema) could never finalize the step that
	// produces it, so refuse at startup (exit 2, every invocation) instead of
	// discovering it at resolve-time after the step has already run. The
	// per-flow loop below guarantees every step result is in App.Artifacts, so
	// checking the declared set here transitively covers every step result.
	recordable := map[flow.ArtifactId]flow.ArtifactDef{}
	for _, ad := range app.Backend.SupportedArtifacts() {
		recordable[ad.Id] = ad
	}
	for _, ad := range app.Artifacts {
		def, ok := recordable[ad.Id]
		if !ok {
			return fmt.Errorf("artifact %q declared but not in Backend.SupportedArtifacts() (backend %q)", ad.Id, app.Backend.Name())
		}
		if def.Type != ad.Type {
			return fmt.Errorf("artifact %q declared with type %v but Backend %q records it as %v", ad.Id, ad.Type, app.Backend.Name(), def.Type)
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

func deriveOwner() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "anonymous"
}

func usage(bin string) string {
	return fmt.Sprintf(`%[1]s — flow binary

usage:
  %[1]s doctor                       verify backend prereqs
  %[1]s list                         list items this flow can process
  %[1]s claim <item-id>              acquire a claim on an item (alias: lease)
  %[1]s run-step                     advance ONE lifecycle item (one prompt → one artifact)
  %[1]s resolve [<item-id>]          run ALL steps until finalized or parked (alias: run-all).
                                     With <item-id>, claims it first; else uses the active claim.
  %[1]s status [<item-id>]           read-only lifecycle checklist. With <item-id>, inspects
                                     that item from the tracker without claiming it.
  %[1]s grant <artifact-id> [--invocations N] [--cost USD] [--prompts N] [--timeout SECONDS]
                                     extend a parked step's budget. <artifact-id> is
                                     the id from AddStep (e.g. "plan"), NOT the step
                                     name (e.g. "write plan")
  %[1]s release                      drop the claim`, bin)
}

// newFlagSet returns a FlagSet configured for the strict-parsing convention:
// errors print to app.Err, unknown flags are rejected. Caller registers any
// flags, calls fs.Parse, then validates fs.NArg() against the expected
// positional count.
func (app *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(app.Err)
	return fs
}

// parseInterspersed parses args allowing flags to appear in any position
// relative to positionals — the convention every modern CLI uses, but one the
// stdlib flag package deliberately does not implement. Walks args once,
// peeling flag tokens (and their values) off into a flags slice and pushing
// the rest into positionals, then calls fs.Parse(flags, "--", positionals...)
// so the stdlib parser sees the only order it understands. Unknown flags fall
// through to fs.Parse's own "flag provided but not defined" error so existing
// strict-parsing behavior is preserved. Bool flags are detected via the
// FlagSet's Lookup + IsBoolFlag so e.g. `claim <id> --force` is parsed
// correctly (no slurping the next token).
func parseInterspersed(fs *flag.FlagSet, args []string) error {
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
		// Strip 1 or 2 leading dashes to get the flag name.
		name := a[1:]
		if name[0] == '-' {
			name = name[1:]
		}
		// "--name=value" is self-contained; never consume a follow-on token.
		if strings.IndexByte(name, '=') >= 0 {
			flags = append(flags, a)
			continue
		}
		flags = append(flags, a)
		// Peek next token only when the flag exists AND is NOT a bool flag —
		// mirrors the stdlib's own internal logic in (*FlagSet).parseOne.
		// Unknown flags: do not consume the next token here; let fs.Parse
		// report the "flag provided but not defined" error itself.
		if fl := fs.Lookup(name); fl != nil {
			if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	if len(positionals) > 0 {
		flags = append(flags, "--")
		flags = append(flags, positionals...)
	}
	return fs.Parse(flags)
}

// rejectArgs parses args for a command that takes no flags and no
// positionals. Returns true on success; on rejection writes to app.Err and
// returns false (caller exits with code 2).
func (app *App) rejectArgs(name string, args []string) bool {
	fs := app.newFlagSet(name)
	if err := fs.Parse(args); err != nil {
		return false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(app.Err, "%s: unexpected argument %q (this command takes no arguments)\n", name, fs.Arg(0))
		return false
	}
	return true
}
