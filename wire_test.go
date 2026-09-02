package flow

import (
	"encoding/json"
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
	if out != in {
		t.Errorf("round-trip: %+v != %+v", out, in)
	}
}

func TestInvocationResult_WithPark(t *testing.T) {
	in := InvocationResult{
		Flow:   "implement",
		Item:   "owner/repo#42",
		Step:   "write plan",
		Status: "parked",
		Park: &ParkRequest{
			Kind:   ParkBudgetExhausted,
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
	if out.Park.Kind != ParkBudgetExhausted || out.Park.Axis != AxisInvocations {
		t.Errorf("Park = %+v, want kind=budget-exhausted axis=invocations", out.Park)
	}
}

func TestClaim_JSONRoundTrip(t *testing.T) {
	in := Claim{
		BackendName: "github",
		ItemRef: ItemRef{
			BackendName: "github",
			Display:     "owner/repo#42",
			Ref:         json.RawMessage(`{"owner":"o","repo":"r","number":42}`),
		},
		Owner: "alice",
		Token: json.RawMessage(`{"state_comment_id":12345}`),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Claim
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.BackendName != in.BackendName || out.Owner != in.Owner {
		t.Errorf("round-trip: %+v != %+v", out, in)
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

// GrantClearsPark is the single rule every backend applies in Grant. The cases
// that matter: only a budget park on THIS step clears, and only when the axis
// actually has room afterwards — a token grant must leave the park standing.
func TestGrantClearsPark(t *testing.T) {
	budgetPark := func(step string, axis BudgetAxis) *ParkRequest {
		return &ParkRequest{Kind: ParkBudgetExhausted, Step: step, Axis: axis}
	}
	tests := []struct {
		name string
		park *ParkRequest
		key  string
		post ArtifactRecord
		g    Grant
		want bool
	}{
		{
			name: "no park",
			key:  "plan",
			want: false,
		},
		{
			name: "question park is never cleared by budget",
			park: &ParkRequest{Kind: ParkQuestion, Step: "plan"},
			key:  "plan",
			post: ArtifactRecord{GrantedInvocations: 99},
			g:    Grant{Invocations: 99},
			want: false,
		},
		{
			name: "park on a different step",
			park: budgetPark("implementation", AxisInvocations),
			key:  "plan",
			post: ArtifactRecord{Invocations: 3, GrantedInvocations: 4},
			g:    Grant{Invocations: 1},
			want: false,
		},
		{
			name: "invocations now have headroom",
			park: budgetPark("plan", AxisInvocations),
			key:  "plan",
			post: ArtifactRecord{Invocations: 3, GrantedInvocations: 4},
			g:    Grant{Invocations: 1},
			want: true,
		},
		{
			name: "invocations still at the cap",
			park: budgetPark("plan", AxisInvocations),
			key:  "plan",
			post: ArtifactRecord{Invocations: 4, GrantedInvocations: 4},
			g:    Grant{Invocations: 1},
			want: false,
		},
		{
			name: "cost grant too small to clear the cap",
			park: budgetPark("plan", AxisCost),
			key:  "plan",
			post: ArtifactRecord{CostUSDSpent: 12.40, GrantedCostUSD: 10.01},
			g:    Grant{CostUSD: 0.01},
			want: false,
		},
		{
			name: "cost grant clears the cap",
			park: budgetPark("plan", AxisCost),
			key:  "plan",
			post: ArtifactRecord{CostUSDSpent: 12.40, GrantedCostUSD: 22.40},
			g:    Grant{CostUSD: 12.39},
			want: true,
		},
		{
			name: "timeout clears on any added time",
			park: budgetPark("plan", AxisTimeout),
			key:  "plan",
			g:    Grant{TimeoutAdd: 60},
			want: true,
		},
		{
			name: "timeout park with no added time",
			park: budgetPark("plan", AxisTimeout),
			key:  "plan",
			g:    Grant{Invocations: 5},
			want: false,
		},
		{
			name: "budget park with no axis recorded",
			park: &ParkRequest{Kind: ParkBudgetExhausted, Step: "plan"},
			key:  "plan",
			post: ArtifactRecord{GrantedInvocations: 9},
			g:    Grant{Invocations: 9},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GrantClearsPark(tt.park, tt.key, tt.post, tt.g); got != tt.want {
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
