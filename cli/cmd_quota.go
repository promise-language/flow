package cli

import (
	"context"
	"fmt"
	"time"
)

// quotaPayload is what `quota` reports in JSON: the account whose allowance is
// being spent, each window's state, and when the figures were read.
type quotaPayload struct {
	// Account is the agent account the figures belong to. Figures printed
	// without an account say what is being spent without saying whose allowance
	// is spending it, and a machine may drive more than one.
	Account string `json:"account"`
	// ReadAt is when the reading was taken. The figures are served through a
	// machine-wide cache, so "now" is not the same as "when this was measured".
	ReadAt time.Time `json:"read_at"`
	// Failure is why a refresh could not be made, when figures are being served
	// stale. Empty when the reading is fresh.
	Failure string               `json:"failure,omitempty"`
	Windows []quotaWindowPayload `json:"windows"`
}

type quotaWindowPayload struct {
	Label string `json:"label"`
	// Used is the fraction of the window's allowance spent, or null when the
	// substrate did not publish one — which is not the same as zero.
	Used     *float64  `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
	// LengthSeconds is the window's span, from which a caller computes how far
	// through it the reading was taken.
	LengthSeconds float64 `json:"length_seconds"`
}

// cmdQuota reports the agent account's current quota state.
//
// It exists because `resolve` prints the block once, at the start, where it
// answers whether the run has headroom — and with the trailing prints gone, the
// question needs a way to be asked deliberately rather than by starting a run.
//
// A ONE-SHOT REPORT: the report IS the output, so it goes to stdout in the
// selected mode (docs/cli.md § One-shot reports). The human rendering is
// reportQuota, the very function `resolve` narrates with, so the two cannot
// come to describe the same windows differently.
func (app *App) cmdQuota(ctx context.Context, args []string) int {
	fs := app.newFlagSet("quota")
	of := addOutputFlags(fs)
	if !app.parseArgs(fs, args) {
		return 2
	}
	if fs.NArg() > 0 {
		return app.usageError("quota: unexpected argument %q (this command takes no arguments)", fs.Arg(0))
	}
	mode, ok := of.mode(app, "quota")
	if !ok {
		return 2
	}

	if mode == OutputHuman {
		// ONE READING, RENDERED AND JUDGED. reportQuota reports its own failure
		// in prose and carries on, which is right for narration beside a run
		// that is continuing anyway; here the reading IS the command, so a
		// reading that could not be taken is a command that could not complete
		// and the exit code has to say so. It returns the reason for exactly
		// that, rather than being asked a second time — on a machine with no
		// usable cache location every ask is a fetch, and a command that spends
		// nothing may not take two readings of one figure.
		if err := reportQuota(app.Out); err != nil {
			return 1
		}
		return 0
	}

	r, err := quotaNow()
	if err != nil {
		fmt.Fprintln(app.Err, "quota:", err)
		return 1
	}
	payload := quotaPayload{
		ReadAt:  r.ReadAt,
		Failure: r.Failure,
		Windows: make([]quotaWindowPayload, 0, len(r.Usage)),
	}
	// The account is reported even when it could not be established, as the
	// human rendering does: "unidentified" is an answer, and an absent field
	// would read as an account nobody thought to look for.
	if rec, aerr := agentAccount(); aerr == nil {
		payload.Account = rec.Display()
	}
	for _, win := range r.Usage {
		w := quotaWindowPayload{
			Label:         win.Label,
			ResetsAt:      win.ResetsAt,
			LengthSeconds: win.Length.Seconds(),
		}
		// A window the substrate published no figure for reports null, never
		// zero: an allowance nobody measured and an allowance nothing has been
		// spent from are opposite facts.
		if win.Used >= 0 {
			used := win.Used
			w.Used = &used
		}
		payload.Windows = append(payload.Windows, w)
	}
	return app.emit(mode, payload, func() {})
}
