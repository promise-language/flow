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
	if !got.Artifacts[0].Resolved || got.Artifacts[0].Version != 1 {
		t.Errorf("artifact = %+v, want resolved at version 1", got.Artifacts[0])
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

// The record is the artifact VALUE and its provenance, and nothing else: the
// counters are the ledger's, and a projection carrying its own copy of them
// would be a second answer to what a step has spent.
func TestRecordFromArtifactDoc(t *testing.T) {
	d := stateArtifactDoc{
		Id:                  "plan",
		Type:                "markdown",
		Resolved:            true,
		ResolvedBy:          "url1",
		ResolvedByPrincipal: "alice",
		Version:             2,
	}
	rec := recordFromArtifactDoc(d)
	if rec.Id != "plan" || rec.Type != flow.ArtifactMarkdown {
		t.Errorf("rec = %+v, want id=plan type=markdown", rec)
	}
	if !rec.Resolved || rec.Version != 2 {
		t.Errorf("rec = %+v, want resolved at version 2", rec)
	}
	if rec.ResolvedBy != "alice" {
		t.Errorf("ResolvedBy = %q, want alice (principal preferred over url)", rec.ResolvedBy)
	}
}

// The park field is the machine-readable copy Load returns, so it has to
// survive a render/extract round trip through the state comment.
func TestRenderStateComment_ParkRoundTrip(t *testing.T) {
	doc := stateDoc{
		Flow:     "implement",
		Schema:   stateSchemaVersion,
		SeededAt: time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC),
		Artifacts: []stateArtifactDoc{
			{Id: "plan", Type: "markdown", Resolved: true, Version: 1},
		},
		Park: parkDocFromRequest(flow.ParkRequest{
			Kind:   flow.ParkTreasurerRefused,
			Step:   "plan",
			Axis:   flow.AxisInvocations,
			Reason: `ran 3 times without completing "plan"`,
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
	if req.Kind != flow.ParkTreasurerRefused || req.Step != "plan" || req.Axis != flow.AxisInvocations {
		t.Errorf("park = %+v, want treasurer-refused on plan/invocations", req)
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
			Kind:   flow.ParkTreasurerRefused,
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

// --- The journal and the ledger on the wire ---

// The journal is what position is derived from, so it has to survive a
// render/extract round trip exactly: in order, with every field a reader acts
// on. The three shapes an entry has are all here — an artifact result, a signal
// result (the observation itself), and a finalizing election.
func TestRenderStateComment_JournalRoundTrip(t *testing.T) {
	at := time.Date(2026, 5, 26, 15, 10, 0, 0, time.UTC)
	entries := []flow.JournalEntry{
		{
			Step: "plan", Execution: 1,
			Result: flow.ArtifactBody{Type: flow.ArtifactMarkdown, Markdown: "the plan"},
			Route:  flow.Route{Next: "impl"},
			Awaits: flow.Awaits{Role: "contributor"},
			// A standing note is addressed to every subsequent step rather than
			// only the next one, so it has to survive alongside the message.
			Message: "the plan is written", Note: "the base branch is release-2",
			By: "ann", Role: "contributor", At: at,
			Spend: flow.Spend{CostUSD: 1.25, Duration: 90 * time.Second},
		},
		{
			Step: "pr-open", Execution: 1,
			Route:  flow.Route{Next: "pr-merged"},
			Awaits: flow.Awaits{Signal: "pr-merged"},
			By:     "ann", Role: "contributor", At: at.Add(time.Hour),
		},
		{
			Step: "impl", Execution: 2,
			Result: flow.ArtifactBody{Type: flow.ArtifactCommitHash, CommitHash: "0123456789abcdef0123456789abcdef01234567"},
			Route:  flow.Route{Finalize: flow.DispositionResolved},
			By:     "bo", Role: "maintainer", At: at.Add(2 * time.Hour),
		},
	}
	doc := stateDoc{Flow: "implement", Schema: stateSchemaVersion, SeededAt: at}
	for _, e := range entries {
		doc.Journal = append(doc.Journal, journalEntryDocOf(e, ""))
	}

	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	got, _, found, err := extractStateDoc(body)
	if err != nil || !found {
		t.Fatalf("extractStateDoc: found=%v err=%v", found, err)
	}
	if len(got.Journal) != len(entries) {
		t.Fatalf("journal has %d entries after the round trip, want %d", len(got.Journal), len(entries))
	}
	for i, want := range entries {
		back := journalEntryFromDoc(got.Journal[i])
		if back.Step != want.Step || back.Execution != want.Execution {
			t.Errorf("entry %d identity = %s/%d, want %s/%d", i, back.Step, back.Execution, want.Step, want.Execution)
		}
		if back.Route != want.Route {
			t.Errorf("entry %d route = %+v, want %+v", i, back.Route, want.Route)
		}
		if back.Awaits != want.Awaits {
			t.Errorf("entry %d awaits = %+v, want %+v", i, back.Awaits, want.Awaits)
		}
		if back.Message != want.Message || back.Note != want.Note {
			t.Errorf("entry %d message/note = %q/%q, want %q/%q", i, back.Message, back.Note, want.Message, want.Note)
		}
		if back.By != want.By || back.Role != want.Role || !back.At.Equal(want.At) {
			t.Errorf("entry %d provenance = %s/%s/%s, want %s/%s/%s",
				i, back.By, back.Role, back.At, want.By, want.Role, want.At)
		}
		if back.Spend != want.Spend {
			t.Errorf("entry %d spend = %+v, want %+v", i, back.Spend, want.Spend)
		}
		if back.Result.Type != want.Result.Type || back.Result.CommitHash != want.Result.CommitHash {
			t.Errorf("entry %d result = %+v, want type %v hash %q",
				i, back.Result, want.Result.Type, want.Result.CommitHash)
		}
	}
	// The wire's `type` exists for a reader with no flow, and a signal result
	// is spelled apart from every artifact type.
	if got.Journal[1].Type != journalSignalType {
		t.Errorf("signal entry type = %q, want %q", got.Journal[1].Type, journalSignalType)
	}
	// An awaited signal is `signal:<id>` on the wire; a role is the bare name.
	if got.Journal[1].Awaits != awaitsSignalPrefix+"pr-merged" {
		t.Errorf("awaits = %q, want %q", got.Journal[1].Awaits, awaitsSignalPrefix+"pr-merged")
	}
	if got.Journal[0].Awaits != "contributor" {
		t.Errorf("awaits = %q, want the bare role name", got.Journal[0].Awaits)
	}
}

// An empty journal carries no `journal:` key at all, and reads back as an empty
// slice rather than a one-element list of zeroes.
func TestRenderStateComment_EmptyJournalRoundTrip(t *testing.T) {
	doc := stateDoc{Flow: "implement", Schema: stateSchemaVersion}
	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	if strings.Contains(body, "journal:") {
		t.Errorf("empty journal wrote a journal key:\n%s", body)
	}
	got, _, found, err := extractStateDoc(body)
	if err != nil || !found {
		t.Fatalf("extractStateDoc: found=%v err=%v", found, err)
	}
	if len(got.Journal) != 0 {
		t.Errorf("journal = %+v after the round trip, want empty", got.Journal)
	}
}

// The ledger is the treasurer's durable record, and active and waiting time are
// separate facts on the wire for the same reason they are separate in the row.
func TestRenderStateComment_LedgerRoundTrip(t *testing.T) {
	at := time.Date(2026, 5, 26, 15, 10, 0, 0, time.UTC)
	doc := stateDoc{
		Flow: "implement", Schema: stateSchemaVersion,
		Ledger: stateLedgerDoc{
			Steps: map[string]stateLedgerRowDoc{
				"plan": {
					Dispatches: 3, Resumptions: 1, CostUSDSpent: 4.25,
					DurationSeconds: 90, WaitingSeconds: 600,
					Granted: []stateLedgerGrantDoc{
						{Axis: string(flow.AxisInvocations), Amount: 2, At: at},
						{Axis: string(flow.AxisCost), Amount: 5.5, At: at},
					},
					LastRunAt: at,
				},
				// A signal step owns a row too: rows are keyed by StepId, not
				// by an artifact.
				"pr-open": {Dispatches: 1},
			},
			TotalCostUSD: 4.25, TotalDurationSeconds: 90, TotalWaitingSeconds: 600,
		},
	}

	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	got, _, found, err := extractStateDoc(body)
	if err != nil || !found {
		t.Fatalf("extractStateDoc: found=%v err=%v", found, err)
	}
	l := ledgerFromDoc(got.Ledger)
	row := l.Row("plan")
	if row.Step != "plan" || row.Dispatches != 3 || row.Resumptions != 1 || row.CostUSD != 4.25 {
		t.Errorf("row = %+v, want plan 3/1/$4.25", row)
	}
	if row.Active != 90*time.Second || row.Waiting != 10*time.Minute {
		t.Errorf("active/waiting = %v/%v, want 1m30s/10m", row.Active, row.Waiting)
	}
	if got := row.GrantedOn(flow.AxisInvocations); got != 2 {
		t.Errorf("GrantedOn(invocations) = %v, want 2", got)
	}
	if got := row.GrantedOn(flow.AxisCost); got != 5.5 {
		t.Errorf("GrantedOn(cost) = %v, want 5.5", got)
	}
	if !row.LastRunAt.Equal(at) {
		t.Errorf("LastRunAt = %v, want %v", row.LastRunAt, at)
	}
	if l.Row("pr-open").Dispatches != 1 {
		t.Errorf("the signal step's row was lost: %+v", l.Steps)
	}
	if l.TotalCostUSD != 4.25 || l.TotalActive != 90*time.Second || l.TotalWaiting != 10*time.Minute {
		t.Errorf("totals = %v/%v/%v, want 4.25/1m30s/10m", l.TotalCostUSD, l.TotalActive, l.TotalWaiting)
	}
}

// An untouched ledger writes no key and reads back as the zero value — not as a
// ledger with one empty row.
func TestRenderStateComment_EmptyLedgerRoundTrip(t *testing.T) {
	doc := stateDoc{Flow: "implement", Schema: stateSchemaVersion}
	body, err := renderStateComment("alice", doc)
	if err != nil {
		t.Fatalf("renderStateComment: %v", err)
	}
	got, _, found, err := extractStateDoc(body)
	if err != nil || !found {
		t.Fatalf("extractStateDoc: found=%v err=%v", found, err)
	}
	l := ledgerFromDoc(got.Ledger)
	if len(l.Steps) != 0 || l.TotalCostUSD != 0 || l.TotalActive != 0 || l.TotalWaiting != 0 {
		t.Errorf("ledger = %+v after the round trip, want the zero value", l)
	}
}

// The wire carries one `awaits` string, and a role and a signal are different
// kinds of wait: the prefix is the only thing that tells them apart.
func TestAwaitsStringRoundTrip(t *testing.T) {
	cases := []flow.Awaits{
		{},
		{Role: "contributor"},
		{Signal: "pr-merged"},
	}
	for _, want := range cases {
		if got := awaitsFromString(awaitsString(want)); got != want {
			t.Errorf("round-trip %+v → %q → %+v", want, awaitsString(want), got)
		}
	}
	// The Account half is never on the wire: it is read from the journal, and a
	// stored copy would be a second answer to who holds the role.
	if got := awaitsString(flow.Awaits{Role: "contributor", Account: "ann"}); got != "contributor" {
		t.Errorf("awaitsString carried the account: %q", got)
	}
}
