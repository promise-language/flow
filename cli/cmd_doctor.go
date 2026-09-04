package cli

import (
	"context"
	"fmt"
	"strings"
)

// Output glyphs visually distinct from the rest of the SDK output so the
// terminal scanline conveys pass/fail at a glance.
const (
	glyphOK   = "✅" // ✅ white heavy check mark on green square
	glyphFail = "❌" // ❌ heavy multiplication X
)

// Doctor is the optional preflight hook a Backend may implement. If the
// configured Backend satisfies this, `doctor` calls it; otherwise the
// command does a minimal end-to-end check (list eligible items).
type Doctor interface {
	Doctor(ctx context.Context) error
}

func (app *App) cmdDoctor(ctx context.Context, args []string) int {
	if !app.rejectArgs("doctor", args) {
		return 2
	}
	if d, ok := app.Orchestrator.(Doctor); ok {
		if err := d.Doctor(ctx); err != nil {
			fmt.Fprintf(app.Err, "%s doctor: %s\n", glyphFail, err)
			return 1
		}
		fmt.Fprintf(app.Out, "%s doctor: OK\n", glyphOK)
		app.reportCapabilities()
		return 0
	}
	// Fallback: ask for the auto-selectable set as a connectivity probe.
	if _, err := app.Orchestrator.ListAutoSelectable(ctx, nil); err != nil {
		fmt.Fprintf(app.Err, "%s doctor: orchestrator.ListAutoSelectable failed: %s\n", glyphFail, err)
		return 1
	}
	fmt.Fprintf(app.Out, "%s doctor: OK (no orchestrator Doctor probe; probed via ListAutoSelectable)\n", glyphOK)
	app.reportCapabilities()
	return 0
}

// reportCapabilities prints what this orchestrator declares.
//
// There is nothing here about OPTIONAL capabilities any more: every method is
// required, so the only thing worth reporting is what it says it can run.
func (app *App) reportCapabilities() {
	gates := app.Orchestrator.SupportedGates()
	names := make([]string, 0, len(gates))
	for _, g := range gates {
		names = append(names, string(g.Name))
	}
	fmt.Fprintf(app.Out, "  gates: %s\n", strings.Join(names, ", "))
	cmds := app.Orchestrator.SupportedCommands()
	cnames := make([]string, 0, len(cmds))
	for _, c := range cmds {
		cnames = append(cnames, string(c.Name))
	}
	fmt.Fprintf(app.Out, "  commands: %s\n", strings.Join(cnames, ", "))
	if app.CarryThrough {
		fmt.Fprintf(app.Out, "  carry-through: enabled — carries to merge (not independent review)\n")
	}
}
