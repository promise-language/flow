package cli

import (
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
