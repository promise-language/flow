package github

import (
	"strings"
	"testing"
	"time"

	"github.com/promise-language/flow"
)

func TestRenderStateComment_RoundTrip(t *testing.T) {
	doc := stateDoc{
		Flow:     "implement",
		Schema:   stateSchemaVersion,
		SeededAt: time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC),
		Artifacts: []stateArtifactDoc{
			{
				Id:                 "plan",
				Type:               "markdown",
				Required:           true,
				Resolved:           true,
				ResolvedBy:         "https://github.com/o/r/issues/42#issuecomment-12345",
				ProducedAt:         time.Date(2026, 5, 26, 15, 10, 0, 0, time.UTC),
				ResolvedByPrincipal: "claude-opus-4-7",
				Version:            1,
				GrantedInvocations: 3,
				GrantedCostUSD:     10,
				Invocations:        1,
				CostUSDSpent:       0.42,
			},
		},
		Signals: []stateSignalDoc{
			{Id: "pr-open", Set: true, ObservedAt: time.Date(2026, 5, 26, 15, 42, 0, 0, time.UTC), ObservedVia: "side-effect"},
			{Id: "pr-merged", Set: false},
		},
	}

	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	if !strings.Contains(body, "<!-- flow:state-v1 begin owner=alice -->") {
		t.Errorf("missing begin marker; body:\n%s", body)
	}
	if !strings.Contains(body, "<!-- flow:state-v1 end -->") {
		t.Errorf("missing end marker; body:\n%s", body)
	}
	if !strings.Contains(body, "```yaml") {
		t.Errorf("missing yaml fence; body:\n%s", body)
	}

	got, owner, found, err := extractStateDoc(body)
	if err != nil {
		t.Fatalf("extractStateDoc: %v", err)
	}
	if !found {
		t.Fatal("extractStateDoc found=false on rendered body")
	}
	if owner != "alice" {
		t.Errorf("owner = %q, want alice", owner)
	}
	if got.Flow != "implement" || got.Schema != stateSchemaVersion {
		t.Errorf("doc top = %+v, want flow=implement schema=1", got)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Id != "plan" {
		t.Errorf("artifacts = %+v, want one entry id=plan", got.Artifacts)
	}
	if got.Artifacts[0].GrantedInvocations != 3 {
		t.Errorf("granted_invocations = %d, want 3", got.Artifacts[0].GrantedInvocations)
	}
	if len(got.Signals) != 2 || got.Signals[0].Id != "pr-open" || !got.Signals[0].Set {
		t.Errorf("signals = %+v, want pr-open set=true first", got.Signals)
	}
}

func TestExtractStateDoc_NoMarkerReturnsNotFound(t *testing.T) {
	body := "Just a regular comment, no markers."
	doc, _, found, err := extractStateDoc(body)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Errorf("found should be false when markers absent")
	}
	if doc != nil {
		t.Errorf("doc should be nil; got %+v", doc)
	}
}

func TestExtractStateDoc_MissingEndIsError(t *testing.T) {
	body := "<!-- flow:state-v1 begin owner=alice -->\n```yaml\nflow: x\n```\n"
	_, _, _, err := extractStateDoc(body)
	if err == nil {
		t.Errorf("expected error for missing end marker")
	}
}

func TestArtifactTypeStringSymmetric(t *testing.T) {
	types := []flow.ArtifactType{
		flow.ArtifactFlag, flow.ArtifactCommitHash, flow.ArtifactMarkdown,
		flow.ArtifactJSON, flow.ArtifactFile, flow.ArtifactPatch,
	}
	for _, tt := range types {
		s := artifactTypeString(tt)
		if s == "" {
			t.Errorf("artifactTypeString(%v) = empty", tt)
		}
		if got := artifactTypeFromString(s); got != tt {
			t.Errorf("round-trip %v → %q → %v", tt, s, got)
		}
	}
}

func TestRecordFromArtifactDoc(t *testing.T) {
	d := stateArtifactDoc{
		Id:                 "plan",
		Type:               "markdown",
		Required:           true,
		Resolved:           true,
		ResolvedBy:         "url1",
		ResolvedByPrincipal: "alice",
		Version:            2,
		GrantedInvocations: 5,
		GrantedCostUSD:     10,
		Invocations:        3,
		CostUSDSpent:       2.5,
	}
	rec := recordFromArtifactDoc(d)
	if rec.Id != "plan" || rec.Type != flow.ArtifactMarkdown {
		t.Errorf("rec = %+v, want id=plan type=markdown", rec)
	}
	if rec.ResolvedBy != "alice" {
		t.Errorf("ResolvedBy = %q, want alice (principal preferred over url)", rec.ResolvedBy)
	}
	if rec.GrantedInvocations != 5 || rec.CostUSDSpent != 2.5 {
		t.Errorf("budget fields lost: %+v", rec)
	}
}
