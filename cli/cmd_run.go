package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

func (app *App) cmdRun(ctx context.Context, args []string) int {
	claim, err := LoadActiveClaim()
	if err != nil {
		fmt.Fprintln(app.Err, "run:", err)
		return 1
	}
	if claim == nil {
		fmt.Fprintln(app.Err, "run: no active lease (run `lease <id>` first)")
		return 1
	}

	res, err := RunOne(ctx, app, *claim)
	if err != nil {
		fmt.Fprintln(app.Err, "run:", err)
		return 1
	}

	enc := json.NewEncoder(app.Out)
	if err := enc.Encode(res); err != nil {
		fmt.Fprintln(app.Err, "run: encode result:", err)
		return 1
	}
	switch res.Status {
	case "failed":
		return 1
	default:
		return 0
	}
}
