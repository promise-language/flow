package flow

import (
	"encoding/json"
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
