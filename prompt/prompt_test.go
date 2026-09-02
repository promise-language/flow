package prompt

import (
	"strings"
	"testing"
)

func TestRender_FillsPartials(t *testing.T) {
	c := Context{ItemID: "T1", ItemType: "task", ItemTitle: "do it", VerifyCmd: "make check"}
	if err := c.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(c.ItemHeader, "T1") || !strings.Contains(c.ItemHeader, "do it") {
		t.Errorf("ItemHeader missing id/title: %q", c.ItemHeader)
	}
	if !strings.Contains(c.AskGuidance, "mcp__tracker__ask_user_question") {
		t.Errorf("AskGuidance missing MCP question tool: %q", c.AskGuidance)
	}
	// The plan-resolution partial must carry the proof-gated already-implemented
	// branch (the stall fix) with the precise terminal statuses, the ask-if-unsure
	// path, and the not-feasible path. (BLOCKED moved to the implement step.)
	for _, want := range []string{"ALREADY IMPLEMENTED", "PROOF", "duplicate", "cant_reproduce", "works_as_intended", "ask_user_question", "wontfix", "needs-attention"} {
		if !strings.Contains(c.PlanStepResolution, want) {
			t.Errorf("PlanStepResolution missing %q", want)
		}
	}
	if !strings.Contains(c.RebaseResolution, "make check") {
		t.Errorf("RebaseResolution should interpolate VerifyCmd: %q", c.RebaseResolution)
	}
	if !strings.Contains(c.RebaseResolution, "git rebase --continue") {
		t.Error("RebaseResolution missing the continue step")
	}
	for _, want := range []string{"duplicate fix", "git log --oneline", "DUPLICATE-WORK CANDIDATES", "mainline", "Absent"} {
		if !strings.Contains(c.RebaseResolution, want) {
			t.Errorf("RebaseResolution missing %q", want)
		}
	}
	if !strings.Contains(c.DeferCommit, "later step") {
		t.Errorf("DeferCommit missing later-step note: %q", c.DeferCommit)
	}
}

func TestRender_OmitsEmptyDescription(t *testing.T) {
	with := Context{ItemID: "T1", ItemTitle: "t", ItemDescription: "the details"}
	if err := with.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(with.ItemHeader, "Description:") || !strings.Contains(with.ItemHeader, "the details") {
		t.Errorf("expected the description section when present: %q", with.ItemHeader)
	}
	without := Context{ItemID: "T1", ItemTitle: "t"}
	if err := without.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(without.ItemHeader, "Description:") {
		t.Errorf("empty description should be omitted from the header: %q", without.ItemHeader)
	}
}

func TestRender_HonorsOverride(t *testing.T) {
	c := Context{ItemID: "T1", ItemTitle: "t", AskGuidance: "CUSTOM ASK"}
	if err := c.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if c.AskGuidance != "CUSTOM ASK" {
		t.Errorf("Render overwrote a caller-set partial: %q", c.AskGuidance)
	}
	// Non-overridden partials still render from their defaults.
	if !strings.Contains(c.ItemHeader, "T1") {
		t.Error("non-overridden ItemHeader should still render")
	}
}
