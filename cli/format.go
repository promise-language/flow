package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/promise-language/flow"
)

// formatDurationCompact renders a duration as the two largest non-zero units,
// never three. Examples: "3d 4h", "2h 24m", "14m02s", "5m", "42s", "<1s".
func formatDurationCompact(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return "<1s"
	}

	totalSecs := int(d.Seconds())
	days := totalSecs / 86400
	hours := (totalSecs % 86400) / 3600
	mins := (totalSecs % 3600) / 60
	secs := totalSecs % 60

	// Largest two units only.
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case mins > 0 && secs > 0:
		return fmt.Sprintf("%dm%02ds", mins, secs)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// blockedByLine renders the `blocked by:` line for a result that stopped on
// the item's own blockedness: the blockers still open, listed the way
// blockerDisplays lists them for `list`, so the filter to unfinished blockers
// exists once. Returns "" when the result did not stop on items, and when every
// declared blocker has finished — printing those would send the operator to
// work something already done.
func blockedByLine(r flow.InvocationResult) string {
	if r.BlockKind != flow.WaitsOnItems {
		return ""
	}
	open := blockerDisplays(r.BlockedBy)
	if len(open) == 0 {
		return ""
	}
	return "blocked by: " + strings.Join(open, ", ")
}

// formatResultSuffix renders the duration/cost parenthetical for a step
// outcome line. Returns "" when neither field is present.
func formatResultSuffix(r flow.InvocationResult) string {
	if r.DurationSeconds == 0 && r.CostUSD == nil {
		return ""
	}
	dur := formatDurationCompact(time.Duration(r.DurationSeconds * float64(time.Second)))
	if r.CostUSD != nil {
		return fmt.Sprintf("(%s, $%.2f)", dur, *r.CostUSD)
	}
	return fmt.Sprintf("(%s)", dur)
}
