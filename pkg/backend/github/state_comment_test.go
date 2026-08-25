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
				Id:                  "plan",
				Type:                "markdown",
				Required:            true,
				Resolved:            true,
				ResolvedBy:          "https://github.com/o/r/issues/42#issuecomment-12345",
				ProducedAt:          time.Date(2026, 5, 26, 15, 10, 0, 0, time.UTC),
				ResolvedByPrincipal: "claude-opus-4-7",
				Version:             1,
				GrantedInvocations:  3,
				GrantedCostUSD:      10,
				Invocations:         1,
				CostUSDSpent:        0.42,
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
		Id:                  "plan",
		Type:                "markdown",
		Required:            true,
		Resolved:            true,
		ResolvedBy:          "url1",
		ResolvedByPrincipal: "alice",
		Version:             2,
		GrantedInvocations:  5,
		GrantedCostUSD:      10,
		Invocations:         3,
		CostUSDSpent:        2.5,
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

// The park field is the machine-readable copy LoadState returns, so it has to
// survive a render/extract round trip through the state comment.
func TestRenderStateComment_ParkRoundTrip(t *testing.T) {
	doc := stateDoc{
		Flow:     "implement",
		Schema:   stateSchemaVersion,
		SeededAt: time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC),
		Artifacts: []stateArtifactDoc{
			{Id: "plan", Type: "markdown", Required: true, GrantedInvocations: 3, Invocations: 3},
		},
		Park: parkDocFromRequest(flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   "plan",
			Axis:   flow.AxisInvocations,
			Reason: `ran 3 times without resolving "plan"`,
		}, time.Date(2026, 5, 26, 15, 20, 0, 0, time.UTC)),
	}

	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	got, _, found, err := extractStateDoc(body)
	if err != nil || !found {
		t.Fatalf("extractStateDoc: found=%v err=%v", found, err)
	}
	req := parkRequestFromDoc(got.Park)
	if req == nil {
		t.Fatal("park did not survive the round trip")
	}
	if req.Kind != flow.ParkBudgetExhausted || req.Step != "plan" || req.Axis != flow.AxisInvocations {
		t.Errorf("park = %+v, want budget-exhausted on plan/invocations", req)
	}
	if req.Reason == "" {
		t.Error("park reason was dropped")
	}
}

// An unparked item carries no park key at all, and reads back as nil rather
// than a zero-valued park.
func TestRenderStateComment_NoParkIsNil(t *testing.T) {
	doc := stateDoc{Flow: "implement", Schema: stateSchemaVersion}
	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	if strings.Contains(body, "park:") {
		t.Errorf("body carries a park key with no park: %s", body)
	}
	got, _, _, err := extractStateDoc(body)
	if err != nil {
		t.Fatalf("extractStateDoc: %v", err)
	}
	if parkRequestFromDoc(got.Park) != nil {
		t.Errorf("park = %+v, want nil", got.Park)
	}
}

// The axis report is the operator-facing half of a budget park, so it has to
// survive the state comment intact — a park read back an hour later must show
// the same full picture the run recorded, not just the axis that tripped.
func TestRenderStateComment_ParkAxisReportRoundTrip(t *testing.T) {
	doc := stateDoc{
		Flow:     "implement",
		Schema:   stateSchemaVersion,
		SeededAt: time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC),
		Park: parkDocFromRequest(flow.ParkRequest{
			Kind:   flow.ParkBudgetExhausted,
			Step:   "push",
			Axis:   flow.AxisCost,
			Reason: `spent $11.18 without resolving "push"`,
			Axes: []flow.AxisReport{
				flow.NewAxisReport(flow.AxisInvocations, 3, 3),
				flow.NewAxisReport(flow.AxisPrompts, 2, 2),
				flow.NewAxisReport(flow.AxisCost, 11.18, 10),
				flow.NewAxisReport(flow.AxisTimeout, 0, 10800),
			},
		}, time.Date(2026, 5, 26, 15, 20, 0, 0, time.UTC)),
	}

	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	got, _, found, err := extractStateDoc(body)
	if err != nil || !found {
		t.Fatalf("extractStateDoc: found=%v err=%v", found, err)
	}
	req := parkRequestFromDoc(got.Park)
	if req == nil {
		t.Fatal("park did not survive the round trip")
	}
	if len(req.Axes) != 4 {
		t.Fatalf("Axes = %+v, want all four", req.Axes)
	}
	byAxis := map[flow.BudgetAxis]flow.AxisReport{}
	for _, a := range req.Axes {
		byAxis[a.Axis] = a
	}
	cost := byAxis[flow.AxisCost]
	if cost.Used != 11.18 || cost.Granted != 10 || !cost.Exhausted {
		t.Errorf("cost = %+v, want 11.18/10 flat", cost)
	}
	// The zero-valued "used" on an axis with headroom must not be confused
	// with a missing axis: omitempty drops the field, the axis stays.
	to := byAxis[flow.AxisTimeout]
	if to.Axis != flow.AxisTimeout || to.Granted != 10800 || to.Exhausted {
		t.Errorf("timeout = %+v, want 0/10800 with headroom", to)
	}
}
