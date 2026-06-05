package cli

import (
	"testing"

	"github.com/promise-language/flow"
)

// statusFlowLine is the source of the "flow:" status line. The key invariant
// (the T-bug this guards): "(finalized)" must appear ONLY when the persistent
// Item.Finalized flag is set — never for a not-yet-seeded item. An unseeded
// item with no eligible step must read "(not seeded)", not "(finalized)" and
// not the misleading "(no eligible step)".
func TestStatusFlowLine(t *testing.T) {
	doFlow := flow.NewFlow("do", []flow.ItemType{"task"})

	seeded := map[flow.ArtifactId]flow.ArtifactRecord{
		"plan": {Id: "plan", Required: true},
	}
	unseeded := map[flow.ArtifactId]flow.ArtifactRecord{}

	tests := []struct {
		name      string
		finalized bool
		artifacts map[flow.ArtifactId]flow.ArtifactRecord
		eligible  *flow.Flow
		typeFlow  *flow.Flow
		want      string
	}{
		{
			name:      "finalized flag set",
			finalized: true,
			artifacts: seeded,
			typeFlow:  doFlow,
			want:      "do (finalized)",
		},
		{
			name:      "finalized flag set, no type flow",
			finalized: true,
			artifacts: seeded,
			typeFlow:  nil,
			want:      "finalized",
		},
		{
			// The bug: unseeded item is NOT finalized — must not read "(finalized)".
			name:      "unseeded item is not finalized",
			finalized: false,
			artifacts: unseeded,
			typeFlow:  doFlow,
			want:      "do (not seeded)",
		},
		{
			name:      "seeded but no eligible step",
			finalized: false,
			artifacts: seeded,
			typeFlow:  doFlow,
			want:      "do (no eligible step)",
		},
		{
			name:      "eligible step takes precedence",
			finalized: false,
			artifacts: unseeded,
			eligible:  doFlow,
			typeFlow:  doFlow,
			want:      "do",
		},
		{
			name:      "no matching flow",
			finalized: false,
			artifacts: unseeded,
			typeFlow:  nil,
			want:      "(no matching flow)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &flow.ItemState{
				Item:      flow.Item{Finalized: tt.finalized},
				Artifacts: tt.artifacts,
			}
			got := statusFlowLine(state, tt.eligible, tt.typeFlow)
			if got != tt.want {
				t.Errorf("statusFlowLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
