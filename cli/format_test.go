package cli

import (
	"slices"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

func TestFormatDurationCompact(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{500 * time.Millisecond, "<1s"},
		{1 * time.Millisecond, "<1s"},
		{42 * time.Second, "42s"},
		{1 * time.Second, "1s"},
		{5 * time.Minute, "5m"},
		{5*time.Minute + 30*time.Second, "5m30s"},
		{14*time.Minute + 2*time.Second, "14m02s"},
		{2*time.Hour + 24*time.Minute, "2h 24m"},
		{1 * time.Hour, "1h"},
		{3*24*time.Hour + 4*time.Hour, "3d 4h"},
		{7 * 24 * time.Hour, "7d"},
		// Sub-second remainder in larger durations is truncated, not rounded.
		{2*time.Hour + 24*time.Minute + 500*time.Millisecond, "2h 24m"},
		// Exactly one day.
		{24 * time.Hour, "1d"},
	}
	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			got := formatDurationCompact(tt.d)
			if got != tt.want {
				t.Errorf("formatDurationCompact(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func pf(v float64) *float64 { return &v }

func TestFormatResultSuffix(t *testing.T) {
	tests := []struct {
		name string
		r    flow.InvocationResult
		want string
	}{
		{
			name: "both fields",
			r:    flow.InvocationResult{DurationSeconds: 82.5, CostUSD: pf(0.34)},
			want: "(1m22s, $0.34)",
		},
		{
			name: "duration only",
			r:    flow.InvocationResult{DurationSeconds: 300},
			want: "(5m)",
		},
		{
			name: "cost zero",
			r:    flow.InvocationResult{DurationSeconds: 60, CostUSD: pf(0)},
			want: "(1m, $0.00)",
		},
		{
			name: "neither field",
			r:    flow.InvocationResult{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatResultSuffix(tt.r)
			if got != tt.want {
				t.Errorf("formatResultSuffix = %q, want %q", got, tt.want)
			}
		})
	}
}

// openBlockers sends the operator to what is still open, and only then: an
// item carries every blocker ever declared, statuses included, and a landed one
// is not somewhere to go.
func TestOpenBlockers(t *testing.T) {
	open := flow.Blocker{Ref: flow.ItemRef{Display: "3"}, Status: flow.StatusOpen}
	landed := flow.Blocker{Ref: flow.ItemRef{Display: "2"}, Status: flow.StatusTerminal}
	tests := []struct {
		name     string
		kind     flow.BlockKind
		blockers []flow.Blocker
		want     []string
	}{
		{
			name:     "waits on items lists only the open blockers",
			kind:     flow.WaitsOnItems,
			blockers: []flow.Blocker{landed, open},
			want:     []string{"3"},
		},
		{
			// Every declared blocker has finished: naming them would send the
			// operator to work something already done.
			name:     "waits on items with every blocker landed",
			kind:     flow.WaitsOnItems,
			blockers: []flow.Blocker{landed},
			want:     nil,
		},
		{
			// A block of another kind names no blockers, whatever the carrier
			// happens to hold: the kind decides, not the list.
			name:     "a block of another kind",
			kind:     flow.WaitsOnPerson,
			blockers: []flow.Blocker{open},
			want:     nil,
		},
		{
			// No block at all — the empty kind an unblocked item carries.
			name:     "no block",
			kind:     "",
			blockers: []flow.Blocker{open},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openBlockers(tt.kind, tt.blockers)
			if !slices.Equal(got, tt.want) {
				t.Errorf("openBlockers = %v, want %v", got, tt.want)
			}
		})
	}
}

// blockedByLine renders whatever set it is handed, and nothing for an empty
// one — so every caller can print it unconditionally.
func TestBlockedByLine(t *testing.T) {
	tests := []struct {
		name string
		open []string
		want string
	}{
		{name: "one blocker", open: []string{"3"}, want: "blocked by: 3"},
		{name: "several blockers", open: []string{"3", "4"}, want: "blocked by: 3, 4"},
		{name: "empty set", open: nil, want: ""},
		{name: "empty slice", open: []string{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := blockedByLine(tt.open)
			if got != tt.want {
				t.Errorf("blockedByLine = %q, want %q", got, tt.want)
			}
		})
	}
}
