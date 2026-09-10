package flow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestInvocationResult_JSONRoundTrip(t *testing.T) {
	in := InvocationResult{
		Flow:         "implement",
		InvocationID: "inv-1",
		Item:         "owner/repo#42",
		Step:         "write plan",
		Status:       "done",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out InvocationResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round-trip: %+v != %+v", out, in)
	}
	// A result that did not stop on items carries neither block field, and one
	// that classified nothing carries no classification: the wire is
	// byte-identical to what it was before any of them existed.
	for _, key := range []string{"block_kind", "blocked_by", "item_scoped", "redispatch_may_clear"} {
		if strings.Contains(string(b), key) {
			t.Errorf("JSON %s carries %q on a result that neither stopped on items nor classified anything", b, key)
		}
	}
}

// A stop on the item's own blockers carries the kind and the declared
// blockers, each with its status, as data — never only prose.
func TestInvocationResult_BlockedOnItemsRoundTrip(t *testing.T) {
	in := InvocationResult{
		Flow:      "implement",
		Item:      "owner/repo#42",
		Step:      "plan",
		Status:    "blocked",
		Reason:    "waiting on unfinished dependencies",
		BlockKind: WaitsOnItems,
		BlockedBy: []Blocker{
			{Ref: ItemRef{OrchestratorName: "github", Display: "owner/repo#7", Ref: json.RawMessage(`{"issue":7}`)}, Status: StatusTerminal},
			{Ref: ItemRef{OrchestratorName: "github", Display: "owner/repo#8", Ref: json.RawMessage(`{"issue":8}`)}, Status: StatusOpen},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"block_kind":"waits-on-items"`, `"blocked_by":[`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON %s lacks %s", b, key)
		}
	}
	var out InvocationResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round-trip:\n  out = %+v\n  in  = %+v", out, in)
	}
}

func TestInvocationResult_WithPark(t *testing.T) {
	in := InvocationResult{
		Flow:   "implement",
		Item:   "owner/repo#42",
		Step:   "write plan",
		Status: "parked",
		Park: &ParkRequest{
			Kind:   ParkTreasurerRefused,
			Step:   "write plan",
			Axis:   AxisInvocations,
			Reason: "ran 3 times without resolving",
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out InvocationResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Park == nil {
		t.Fatalf("Park nil after round-trip")
	}
	if out.Park.Kind != ParkTreasurerRefused || out.Park.Axis != AxisInvocations {
		t.Errorf("Park = %+v, want kind=treasurer-refused axis=invocations", out.Park)
	}
}

func TestClaim_JSONRoundTrip(t *testing.T) {
	in := Claim{
		OrchestratorName: "github",
		ItemRef: ItemRef{
			OrchestratorName: "github",
			Display:          "owner/repo#42",
			Ref:              json.RawMessage(`{"owner":"o","repo":"r","number":42}`),
		},
		Arena:   Arena{Host: "build01", Id: "/w/repo"},
		Account: "alice",
		Token:   json.RawMessage(`{"state_comment_id":12345}`),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Claim
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.OrchestratorName != in.OrchestratorName || out.Account != in.Account {
		t.Errorf("round-trip: %+v != %+v", out, in)
	}
	// The arena is what the lease binds to, so a claim that lost it on the way
	// through the file would leave the holder unidentifiable wherever one
	// account runs more than one arena.
	if out.Arena != in.Arena {
		t.Errorf("round-trip Arena = %+v, want %+v", out.Arena, in.Arena)
	}
}

func TestAgentRequestResponse_JSONShape(t *testing.T) {
	// Round-trip sanity: marshaling a populated request/response shouldn't fail.
	req := AgentRequest{Prompt: "hi", Model: "claude-opus-4-7", Effort: "high"}
	if _, err := json.Marshal(req); err != nil {
		t.Errorf("AgentRequest marshal: %v", err)
	}
	resp := AgentResponse{
		LastText:        "ok",
		CostUSD:         0.42,
		SessionID:       "sess-1",
		DurationSeconds: 12.5,
		Failure:         nil,
	}
	if _, err := json.Marshal(resp); err != nil {
		t.Errorf("AgentResponse marshal: %v", err)
	}
}

// ptr returns a pointer to v. Test helper for *float64 fields.
func ptr(v float64) *float64 { return &v }

func TestInvocationResult_CostZeroVsAbsent(t *testing.T) {
	withCost := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "done",
		CostUSD: ptr(0.0),
	}
	withoutCost := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "done",
	}
	bWith, err := json.Marshal(withCost)
	if err != nil {
		t.Fatalf("Marshal withCost: %v", err)
	}
	bWithout, err := json.Marshal(withoutCost)
	if err != nil {
		t.Fatalf("Marshal withoutCost: %v", err)
	}
	if string(bWith) == string(bWithout) {
		t.Errorf("CostUSD=&0.0 and CostUSD=nil must produce different JSON;\ngot: %s", bWith)
	}
	// The zero-cost case must contain the field.
	if !strings.Contains(string(bWith), `"cost_usd":0`) {
		t.Errorf("CostUSD=&0.0 must serialise as cost_usd:0; got %s", bWith)
	}
	// The absent case must NOT contain the field.
	if strings.Contains(string(bWithout), `"cost_usd"`) {
		t.Errorf("CostUSD=nil must omit cost_usd; got %s", bWithout)
	}
}

// ItemScoped is a pointer for the reason CostUSD is: a present false is a
// classification — "this arena is the problem", which is what every run-step
// refusal carries — while absent means nothing classified the stop. A plain
// bool with omitempty would put those two on the wire identically, and the
// arena scope the field exists to deliver would arrive as no scope at all.
func TestInvocationResult_ScopeArenaVsUnclassified(t *testing.T) {
	arena := false
	scoped := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "failed",
		ItemScoped: &arena,
	}
	unclassified := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "failed",
	}
	bScoped, err := json.Marshal(scoped)
	if err != nil {
		t.Fatalf("Marshal scoped: %v", err)
	}
	bUnclassified, err := json.Marshal(unclassified)
	if err != nil {
		t.Fatalf("Marshal unclassified: %v", err)
	}
	if !strings.Contains(string(bScoped), `"item_scoped":false`) {
		t.Errorf("ItemScoped=&false must serialise as item_scoped:false; got %s", bScoped)
	}
	// Absent stays absent: a result that classifies nothing is byte-for-byte
	// what it was before the field existed.
	if strings.Contains(string(bUnclassified), `"item_scoped"`) {
		t.Errorf("ItemScoped=nil must omit item_scoped; got %s", bUnclassified)
	}
	var out InvocationResult
	if err := json.Unmarshal(bScoped, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.ItemScoped == nil || *out.ItemScoped {
		t.Errorf("ItemScoped round-tripped to %v, want a present false", out.ItemScoped)
	}
}

// RedispatchMayClear is three-state for the reason ItemScoped is: a present
// false — "re-dispatching this is a loop" — is the classification a scheduler
// most needs, and a plain bool with omitempty would put it on the wire
// identically to absent. So &true, &false, and nil must each serialise
// distinctly, and &false must round-trip to a present false rather than nil.
func TestInvocationResult_RedispatchTrueVsFalseVsAbsent(t *testing.T) {
	yes, no := true, false
	mayClear := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "parked",
		RedispatchMayClear: &yes,
	}
	cannotClear := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "parked",
		RedispatchMayClear: &no,
	}
	unclassified := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "parked",
	}
	bYes, err := json.Marshal(mayClear)
	if err != nil {
		t.Fatalf("Marshal &true: %v", err)
	}
	bNo, err := json.Marshal(cannotClear)
	if err != nil {
		t.Fatalf("Marshal &false: %v", err)
	}
	bNil, err := json.Marshal(unclassified)
	if err != nil {
		t.Fatalf("Marshal nil: %v", err)
	}
	if !strings.Contains(string(bYes), `"redispatch_may_clear":true`) {
		t.Errorf("RedispatchMayClear=&true must serialise as redispatch_may_clear:true; got %s", bYes)
	}
	if !strings.Contains(string(bNo), `"redispatch_may_clear":false`) {
		t.Errorf("RedispatchMayClear=&false must serialise as redispatch_may_clear:false; got %s", bNo)
	}
	// Absent stays absent: a result that classifies nothing is byte-for-byte
	// what it was before the field existed.
	if strings.Contains(string(bNil), `"redispatch_may_clear"`) {
		t.Errorf("RedispatchMayClear=nil must omit redispatch_may_clear; got %s", bNil)
	}
	var out InvocationResult
	if err := json.Unmarshal(bNo, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.RedispatchMayClear == nil || *out.RedispatchMayClear {
		t.Errorf("RedispatchMayClear round-tripped to %v, want a present false", out.RedispatchMayClear)
	}
	if err := json.Unmarshal(bYes, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.RedispatchMayClear == nil || !*out.RedispatchMayClear {
		t.Errorf("RedispatchMayClear round-tripped to %v, want a present true", out.RedispatchMayClear)
	}
}

func TestInvocationResult_WithDurationAndCost(t *testing.T) {
	in := InvocationResult{
		Flow:            "implement",
		InvocationID:    "inv-1",
		Item:            "owner/repo#42",
		Step:            "write plan",
		Status:          "done",
		DurationSeconds: 82.5,
		CostUSD:         ptr(0.34),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out InvocationResult
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.DurationSeconds != in.DurationSeconds {
		t.Errorf("DurationSeconds = %v, want %v", out.DurationSeconds, in.DurationSeconds)
	}
	if out.CostUSD == nil || *out.CostUSD != *in.CostUSD {
		t.Errorf("CostUSD = %v, want %v", out.CostUSD, in.CostUSD)
	}
}

func TestInvocationResult_DurationOmittedWhenZero(t *testing.T) {
	in := InvocationResult{
		Flow: "f", Item: "i", Step: "s", Status: "done",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), `"duration_seconds"`) {
		t.Errorf("zero DurationSeconds must be omitted; got %s", b)
	}
}

// GrantClearsPark is the single rule every orchestrator applies in Grant. The
// cases that matter: only a `treasurer-refused` park on THIS step clears, and
// only when the offending axis actually has room afterwards — a token grant
// must leave the park standing.
//
// The cap comes from the PARK's own snapshot of the axis (ParkRequest.Axes),
// not from the ledger row: an orchestrator holds no policy and could not
// recompute the cap. Consumption comes from the row, which is where the meter
// lives. Each case below therefore states both.
func TestGrantClearsPark(t *testing.T) {
	refused := func(step StepId, axis BudgetAxis, used, granted float64) *ParkRequest {
		return &ParkRequest{
			Kind: ParkTreasurerRefused, Step: step, Axis: axis,
			Axes: []AxisReport{NewAxisReport(axis, used, granted)},
		}
	}
	tests := []struct {
		name string
		park *ParkRequest
		step StepId
		post LedgerRow
		g    Grant
		want bool
	}{
		{
			name: "no park",
			step: "plan",
			want: false,
		},
		{
			name: "question park is never cleared by a grant",
			park: &ParkRequest{Kind: ParkQuestion, Step: "plan"},
			step: "plan",
			g:    Grant{Invocations: 99},
			want: false,
		},
		{
			name: "a park of some other kind on the right step is left alone",
			park: &ParkRequest{Kind: ParkStepDidNotComplete, Step: "plan"},
			step: "plan",
			g:    Grant{Invocations: 99},
			want: false,
		},
		{
			name: "park on a different step",
			park: refused("implementation", AxisInvocations, 3, 3),
			step: "plan",
			post: LedgerRow{Step: "plan", Dispatches: 3},
			g:    Grant{Invocations: 1},
			want: false,
		},
		{
			name: "invocations now have headroom",
			park: refused("plan", AxisInvocations, 3, 3),
			step: "plan",
			post: LedgerRow{Step: "plan", Dispatches: 3},
			g:    Grant{Invocations: 1},
			want: true,
		},
		{
			name: "invocations still at the cap",
			park: refused("plan", AxisInvocations, 4, 4),
			step: "plan",
			// A dispatch landed between the park and the grant: the row is at
			// 5 and one more invocation does not reach it.
			post: LedgerRow{Step: "plan", Dispatches: 5},
			g:    Grant{Invocations: 1},
			want: false,
		},
		{
			name: "a grant on an axis the park did not refuse on clears nothing",
			park: refused("plan", AxisCost, 12.40, 10.00),
			step: "plan",
			post: LedgerRow{Step: "plan", CostUSD: 12.40},
			g:    Grant{Invocations: 10},
			want: false,
		},
		{
			name: "cost grant too small to clear the cap",
			park: refused("plan", AxisCost, 12.40, 10.01),
			step: "plan",
			post: LedgerRow{Step: "plan", CostUSD: 12.40},
			g:    Grant{CostUSD: 0.01},
			want: false,
		},
		{
			name: "cost grant clears the cap",
			park: refused("plan", AxisCost, 12.40, 10.01),
			step: "plan",
			post: LedgerRow{Step: "plan", CostUSD: 12.40},
			g:    Grant{CostUSD: 2.40},
			want: true,
		},
		{
			name: "prompts clear against the run's own snapshot",
			park: refused("plan", AxisPrompts, 40, 40),
			step: "plan",
			// The ledger keeps no prompt counter — the cap is per-invocation —
			// so the park's snapshot is the only record of what was burned.
			post: LedgerRow{Step: "plan"},
			g:    Grant{PromptsPerInvocation: 10},
			want: true,
		},
		{
			name: "prompt grant too small",
			park: refused("plan", AxisPrompts, 50, 40),
			step: "plan",
			post: LedgerRow{Step: "plan"},
			g:    Grant{PromptsPerInvocation: 5},
			want: false,
		},
		{
			name: "timeout clears on any added time",
			park: refused("plan", AxisTimeout, 3600, 3600),
			step: "plan",
			g:    Grant{TimeoutAdd: 60},
			want: true,
		},
		{
			name: "timeout park with no added time",
			park: refused("plan", AxisTimeout, 3600, 3600),
			step: "plan",
			g:    Grant{Invocations: 5},
			want: false,
		},
		{
			name: "refused park with no axis recorded",
			park: &ParkRequest{Kind: ParkTreasurerRefused, Step: "plan"},
			step: "plan",
			g:    Grant{Invocations: 9},
			want: false,
		},
		{
			name: "refused park whose axis carries no snapshot",
			park: &ParkRequest{Kind: ParkTreasurerRefused, Step: "plan", Axis: AxisCost},
			step: "plan",
			post: LedgerRow{Step: "plan", CostUSD: 12.40},
			g:    Grant{CostUSD: 100},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GrantClearsPark(tt.park, tt.step, tt.post, tt.g); got != tt.want {
				t.Errorf("GrantClearsPark() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatAxes(t *testing.T) {
	tests := []struct {
		name string
		axes []AxisReport
		want string
	}{
		{"empty", nil, ""},
		{"single axis", []AxisReport{NewAxisReport(AxisInvocations, 3, 3)}, "3/3 inv (flat)"},
		{"mixed exhausted and not", []AxisReport{
			NewAxisReport(AxisInvocations, 3, 3),
			NewAxisReport(AxisPrompts, 1, 2),
			NewAxisReport(AxisCost, 11.18, 10),
			NewAxisReport(AxisTimeout, 0, 10800),
		}, "3/3 inv (flat) · 1/2 prompts · $11.18/$10.00 (flat) · 0s/3h0m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatAxes(tt.axes); got != tt.want {
				t.Errorf("FormatAxes() = %q, want %q", got, tt.want)
			}
		})
	}
}
