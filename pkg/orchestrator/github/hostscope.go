package github

import (
	"context"
	"fmt"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/hostscope"
)

// acquireHostScope is the seam this file is tested through: a package var, the
// cacheDir pattern, and deliberately NOT an environment variable — an
// environment variable is never an input (docs/org/cli-guide.md § 2).
//
// A test that took the developer's real host-scope exclusion would block every
// other arena on their machine for as long as it ran, and one that raced
// against a real run would serialize with it. main_test.go redirects it for the
// whole package so no test in it can.
var acquireHostScope = hostscope.Acquire

// holdHostScope takes the host-scope exclusion when the project declared this
// gate needs it, and reports how long the queue took.
//
// It returns a no-op release and a zero wait when the gate declares nothing,
// which is the ordinary case and must stay free: a formatting check has nothing
// in common with a test suite in what it costs, and the declaration is what
// keeps the cheap one out of a queue it never belonged in
// (docs/gates-and-commands.md § Two scopes).
//
// A GATE THIS MACHINE DOES NOT DECLARE AT ALL TAKES NOTHING. That is not a hole
// in the exclusion: the declarations are what the gate entry point said it has,
// so a name absent from them is a name this machine could not have been told
// anything about — and refusing here would turn "the tools are not built" into
// "the machine is wedged", naming the wrong problem. The gate's own spawn is
// what reports an entry point that cannot run it.
//
// AN EXCLUSION THAT CANNOT BE TAKEN IS AN ERROR, NEVER A FREE PASS. The whole
// reason the declaration exists is that a measurement taken beside a peer
// reports the machine as much as the code, so proceeding without it would
// produce exactly the failure the declaration was made to prevent — and produce
// it silently, with nothing in the report to say the exclusion was skipped.
func (w *worktree) holdHostScope(ctx context.Context, name flow.GateName) (func(), time.Duration, error) {
	if !hostScopedGate(w.gates, name) {
		return func() {}, 0, nil
	}
	release, waited, err := acquireHostScope(ctx, w.b.arena())
	if err != nil {
		return nil, waited, fmt.Errorf(
			"gate %s declares host scope and the exclusion could not be taken, so it was not run: %w", name, err)
	}
	return release, waited, nil
}

// NOTHING HERE SERVES A CommandName YET, and the gap is the channel rather than
// the rule. docs/gates-and-commands.md § Two scopes binds all three
// CommandNames to the exclusion; what a project has no way to say is WHICH of
// them needs it, because SupportedCommands is a directory listing
// (discoverCommands) and a directory entry carries no fields. Reading a
// command listing is #434's, and CommandRun.Waited is where its answer lands.

// hostScopedGate reports whether defs declares name host-scoped.
//
// Exact-match on the full name, instance and all: `tested:go` and `tested:wasm`
// are separately declared because they are separately runnable, and a project
// that wants one serialized and not the other is making an ordinary choice
// about its own suites.
func hostScopedGate(defs []flow.GateDef, name flow.GateName) bool {
	for _, d := range defs {
		if d.Name == name {
			return d.HostScope
		}
	}
	return false
}
