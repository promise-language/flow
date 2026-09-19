package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

// cmdRelease drops a claim. Which one it drops has three answers, and they are
// the three ways a claim can be addressed at all (docs/cli.md § Releasing):
//
//   - `release <item-id>` — the claim that item carries, resolved through the
//     one id→ref route `claim` and `status` also use. For an item this arena
//     holds it IS the bare form; for one another arena holds, the backend
//     refuses without --force, because nothing here can read that arena's tree.
//   - `release` — the claim the lease file names.
//   - `release` where the lease file cannot be READ — a record nothing can
//     parse names no item, so the ref is zero and the backend takes apart the
//     local record alone. Without this the one command whose job is to drop a
//     claim stopped on the parse error before it reached the clearing, and the
//     only recovery left was deleting the file by hand (#212).
func (app *App) cmdRelease(ctx context.Context, args []string) int {
	fs := app.newFlagSet("release")
	force := fs.Bool("force", false,
		"release despite the worktree preconditions, or a claim this arena does not hold (audited)")
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 1 {
		return app.usageError("release: unexpected argument %q (release takes an optional item id)", fs.Arg(1))
	}

	// The same three the claim override builds, for the same reason: --force is
	// ONE thing, and a release that overrode a different set from the claim
	// that must follow it would refuse the tree it just handed on.
	var overrides []flow.ClaimOverride
	if *force {
		overrides = append(overrides, flow.OverrideDirtyTree, flow.OverrideAlreadyHeld, flow.OverrideStaleBase)
	}

	var ref flow.ItemRef
	switch {
	case fs.NArg() == 1:
		resolved, err := app.resolveClaimRef(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(app.Err, "release:", err)
			return 1
		}
		ref = resolved
	default:
		claim, err := app.Orchestrator.LookupActiveClaim(ctx)
		switch {
		case err != nil:
			// Not a failure to report and stop on: it is the state this command
			// exists to get the arena out of. The error is printed because it
			// names the path, which is the only diagnosis available — and then
			// the release goes ahead against a ref that names nothing.
			fmt.Fprintln(app.Err, "release: the lease record cannot be read:", err)
		case claim == nil:
			fmt.Fprintln(app.Err, "release: no active claim")
			return 1
		default:
			ref = claim.ItemRef
		}
	}

	if err := app.Orchestrator.Release(ctx, ref, overrides); err != nil {
		// A release is refused through the same typed carrier a claim is
		// (docs/cli.md § Releasing), and rendered through the same formatter —
		// so the failing check and its verbatim output reach the operator, who
		// otherwise gets "refused" without being told WHAT is in the way, and
		// the override line when there is one to name.
		var refused flow.ErrClaimRefused
		if errors.As(err, &refused) {
			fmt.Fprintln(app.Err, formatClaimRefusal("release", refused))
			return 1
		}
		fmt.Fprintln(app.Err, "release:", err)
		return 1
	}
	if len(ref.Ref) == 0 {
		// Said plainly, because the operator asked to release an item and no
		// item was released: the arena is free, and whatever ownership the
		// unreadable record pointed at is still recorded wherever it was.
		fmt.Fprintln(app.Out, "cleared the unreadable lease record — this arena now holds nothing")
		fmt.Fprintln(app.Out, "ownership recorded on the item was NOT touched; check it with: list --scope open")
		return 0
	}
	fmt.Fprintf(app.Out, "released %s\n", ref.Display)
	return 0
}
