package cli

import (
	"fmt"
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
