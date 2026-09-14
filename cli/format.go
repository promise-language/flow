package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

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

// openBlockers is the predicate behind every "what does this item still wait
// on?" report: the blockers still open, listed the way blockerDisplays lists
// them for `list`, so the filter to unfinished blockers exists once. Returns
// nothing for a block of any other kind — the kind decides, not the list, since
// a park-derived block names no items — and nothing when every declared blocker
// has finished, because printing those would send the operator to work
// something already done.
//
// It takes the two fields rather than a result or an item: a result reports the
// block a dispatch stopped on and an item reports the block it carries right
// now, and both answer this question with the same pair.
func openBlockers(kind flow.BlockKind, blockers []flow.Blocker) []string {
	if kind != flow.WaitsOnItems {
		return nil
	}
	return blockerDisplays(blockers)
}

// blockPayloadOf projects the four block fields onto the payload `list` and
// `status` share. Two callers, one projection: the listing reads them off an
// ItemInfo and `status` off the loaded Item, and a second literal would let the
// two commands answer differently about the same item at the same moment.
func blockPayloadOf(blocked bool, kind flow.BlockKind, reason string, blockers []flow.Blocker) blockPayload {
	return blockPayload{
		Blocked:     blocked,
		BlockKind:   string(kind),
		BlockReason: reason,
		BlockedBy:   blockerDisplays(blockers),
	}
}

// blockedByLine renders the `blocked by:` line for a set openBlockers returned.
// Returns "" for an empty set, so a caller can print unconditionally.
func blockedByLine(open []string) string {
	if len(open) == 0 {
		return ""
	}
	return "blocked by: " + strings.Join(open, ", ")
}

// parkKindFacts is what the display says about one member of the park
// vocabulary: the kind in a person's words, and the act that resumes it.
//
// ONE TABLE, TWO READERS — the listing's work mark reads `words`, and the park
// narration under `resolve` and `status` reads `resume`. They are the same
// question asked twice ("what stopped, and what unsticks it"), and a second
// table is how the two come to answer differently about one kind.
//
// The words come from the KIND, never from the reason prose: the kind is a
// closed set with a fixed meaning per member, while the reason is one line for
// a person and nothing parses it (docs/orchestrator.md § `ItemInfo`).
type parkKindFacts struct {
	words string
	// resume names the act as an operator would type it. %[1]s is the binary,
	// so the line names the binary that actually produced it rather than a
	// placeholder — the rule every refusal is already under (selfPath).
	resume string
}

// parkKindTable is keyed by every member of flow.AllParkKinds().
// TestParkKindTable_CoversEveryKind walks that enumerator, so a kind added
// later is a test failure here rather than a silently blank work mark.
var parkKindTable = map[flow.ParkKind]parkKindFacts{
	flow.ParkBlocked: {
		words:  "blocked",
		resume: "`%[1]s status` — a person must clear what it waits on",
	},
	flow.ParkQuestion: {
		words:  "waiting on an answer",
		resume: "`%[1]s answer` (answers the question the step parked on)",
	},
	flow.ParkTreasurerRefused: {
		words:  "budget exhausted",
		resume: "`%[1]s grant` (tops up the parked axis)",
	},
	flow.ParkStepDidNotComplete: {
		words:  "did not complete",
		resume: "`%[1]s resolve` (a re-dispatch is what finishes the job it left)",
	},
	flow.ParkInfraTransient: {
		words:  "infrastructure",
		resume: "`%[1]s resolve` (once the runner is back)",
	},
	flow.ParkRemoteUnreachable: {
		words:  "remote unreachable",
		resume: "`%[1]s resolve` (once the remote is reachable)",
	},
	flow.ParkRefused: {
		words:  "refused",
		resume: "`%[1]s status` — the answer is the same until the environment changes",
	},
	flow.ParkWriteContract: {
		words:  "write contract",
		resume: "`%[1]s status` — widen the contract or revert the changes, then `%[1]s resolve`",
	},
	flow.ParkAccountExhausted: {
		words:  "account exhausted",
		resume: "`%[1]s resolve` (once the allowance returns)",
	},
}

// parkKindWords renders a park kind for a person. An empty kind is not a park
// and renders "", so a caller can print unconditionally; a kind this SDK does
// not recognise renders itself rather than nothing, because a value that came
// from somewhere is worth showing even when the vocabulary has moved on.
func parkKindWords(kind flow.ParkKind) string {
	if kind == "" {
		return ""
	}
	if f, ok := parkKindTable[kind]; ok {
		return f.words
	}
	return string(kind)
}

// resumingAct renders the one line a park owes an operator: what to do next.
// Returns "" for a kind with no entry, so an unrecognised kind prints no line
// rather than a wrong one — docs/cli.md holds a refusal to naming the act that
// clears it, and inventing one would be worse than omitting it.
func resumingAct(kind flow.ParkKind, binary string) string {
	f, ok := parkKindTable[kind]
	if !ok || f.resume == "" {
		return ""
	}
	return fmt.Sprintf(f.resume, binary)
}

// alignedRows writes a table with every cell padded to its column's width, so a
// column can be followed down the page. Rows are ragged-tolerant: a row with
// fewer cells simply ends.
//
// WIDTH IS MEASURED IN RUNES, not bytes. The absent-cell marker "—" and the
// clip marker "…" are three bytes each, so a byte count pads a row carrying one
// three columns too far and produces exactly the wave this replaces.
//
// maxWidth bounds a column's influence, not its content: a cell wider than its
// bound is written WHOLE and is simply left out of the width computation, so
// the one item carrying twenty labels shifts its own later cells and nobody
// else's. That is the whole reason this exists rather than text/tabwriter,
// which aligns on the widest cell with no way to except one. Nothing is clipped
// here and no second placeholder rule is introduced — bounding what a cell may
// CLAIM is not bounding what it may SAY.
//
// A zero bound means unbounded. The last cell of a row is never padded, so the
// flexible column costs no trailing whitespace.
func alignedRows(w io.Writer, rows [][]string, maxWidth []int) {
	widths := make([]int, 0, 8)
	for _, row := range rows {
		for i, cell := range row {
			n := utf8.RuneCountInString(cell)
			for len(widths) <= i {
				widths = append(widths, 0)
			}
			if i < len(maxWidth) && maxWidth[i] > 0 && n > maxWidth[i] {
				continue // over its bound: written whole, but sets no column
			}
			if n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i > 0 {
				b.WriteString(columnGap)
			}
			b.WriteString(cell)
			if i == len(row)-1 {
				continue
			}
			for pad := widths[i] - utf8.RuneCountInString(cell); pad > 0; pad-- {
				b.WriteByte(' ')
			}
		}
		fmt.Fprintln(w, b.String())
	}
}

// columnGap separates two aligned cells. Two spaces: one reads as a word break
// inside a cell that already carries them.
const columnGap = "  "

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
