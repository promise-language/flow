package flow

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAgentQuestion_JSONShape(t *testing.T) {
	q := AgentQuestion{
		Text:        "Which framework?",
		Header:      "Framework",
		Format:      FormatChoice,
		Options:     []string{"React", "Vue", "Svelte"},
		MultiSelect: false,
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out AgentQuestion
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Text != q.Text || out.Format != q.Format || len(out.Options) != 3 {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, q)
	}
}

func TestQuestion_EmbedsFlattenInJSON(t *testing.T) {
	now := time.Now().UTC().Round(time.Second)
	q := Question{
		ID:            "q-1",
		AgentQuestion: AgentQuestion{Text: "Proceed?", Format: FormatYesNo},
		UserAnswer:    UserAnswer{Answer: "yes", AnsweredAt: &now},
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The embedded fields should flatten — top-level keys "id", "text",
	// "format", "answer", "answered_at".
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, k := range []string{"id", "text", "format", "answer", "answered_at"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("expected flattened key %q in JSON, got keys %v", k, mapKeys(raw))
		}
	}

	var out Question
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal back: %v", err)
	}
	if out.ID != q.ID || out.Text != q.Text || out.Answer != q.Answer {
		t.Errorf("round-trip mismatch: %+v vs %+v", out, q)
	}
}

func TestAskHelpers(t *testing.T) {
	if q := AskText("Why", "Why this approach?"); q.Format != FormatText || q.Header != "Why" {
		t.Errorf("AskText = %+v", q)
	}
	if q := AskYesNo("Ship", "Ship it?"); q.Format != FormatYesNo {
		t.Errorf("AskYesNo format = %q, want yes_no", q.Format)
	}
	if q := AskChoice("Lib", "Which?", "a", "b"); q.Format != FormatChoice || q.MultiSelect {
		t.Errorf("AskChoice = %+v", q)
	}
	if q := AskMultiChoice("Tags", "Tags?", "x", "y"); q.Format != FormatChoice || !q.MultiSelect {
		t.Errorf("AskMultiChoice = %+v", q)
	}
}

func TestErrQuestion_Error(t *testing.T) {
	cases := []struct {
		name string
		err  ErrQuestion
		want string
	}{
		{"empty", ErrQuestion{}, "question: (empty)"},
		{"single", ErrQuestion{Questions: []AgentQuestion{{Text: "Continue?"}}}, "question: Continue?"},
		{"multiple", ErrQuestion{Questions: []AgentQuestion{
			{Text: "First?"}, {Text: "Second?"},
		}}, "questions: 2 pending (first: First?)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestItemState_PendingQuestions(t *testing.T) {
	now := time.Now()
	state := &ItemState{
		Questions: []Question{
			{ID: "q1", AgentQuestion: AgentQuestion{Text: "Open"}},
			{ID: "q2", AgentQuestion: AgentQuestion{Text: "Answered"},
				UserAnswer: UserAnswer{Answer: "yes", AnsweredAt: &now}},
			{ID: "q3", AgentQuestion: AgentQuestion{Text: "Also open"}},
		},
	}
	pending := state.PendingQuestions()
	if len(pending) != 2 {
		t.Fatalf("PendingQuestions len = %d, want 2", len(pending))
	}
	if pending[0].ID != "q1" || pending[1].ID != "q3" {
		t.Errorf("PendingQuestions ids = [%s,%s], want [q1,q3]", pending[0].ID, pending[1].ID)
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
