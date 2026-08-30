package cli

import (
	"context"
	"fmt"

	"github.com/promise-language/flow"
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
	if d, ok := app.Backend.(Doctor); ok {
		if err := d.Doctor(ctx); err != nil {
			fmt.Fprintf(app.Err, "%s doctor: %s\n", glyphFail, err)
			return 1
		}
		fmt.Fprintf(app.Out, "%s doctor: OK\n", glyphOK)
		app.reportCapabilities()
		return 0
	}
	// Fallback: list eligible items as a connectivity probe.
	if _, err := app.Backend.ListEligible(ctx); err != nil {
		fmt.Fprintf(app.Err, "%s doctor: backend.ListEligible failed: %s\n", glyphFail, err)
		return 1
	}
	fmt.Fprintf(app.Out, "%s doctor: OK (no backend.Doctor; probed via ListEligible)\n", glyphOK)
	app.reportCapabilities()
	return 0
}

// reportCapabilities prints which optional Backend capabilities are available.
func (app *App) reportCapabilities() {
	_, hasInspector := app.Backend.(flow.StateInspector)
	if hasInspector {
		fmt.Fprintf(app.Out, "  status <id>: available (backend supports StateInspector)\n")
	} else {
		fmt.Fprintf(app.Out, "  status <id>: unavailable (backend does not support StateInspector; use claim first)\n")
	}
}
