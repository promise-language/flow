package github

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
)

// The two selection axes, as this orchestrator stores them: two labels, with
// their neutral values unspellable. These cover what the order is, what is left
// out of it, and what a label naming no member does.

// selectionFixture is the mixed set every ordering assertion below runs
// against. Its FILING ORDER IS NOT ITS SELECTION ORDER — issue 10 is filed
// first and taken fourth — so a read that returned the API's own order would
// fail rather than pass by coincidence.
//
//	#11  next + low        filed 2025-03-01   → first: an instruction outranks every assessment
//	#13  critical          filed 2025-05-01   → then the assessments, highest first
//	#14  high              filed 2025-02-01
//	#10  (nothing set)     filed 2025-01-01   → medium, and older than #15
//	#15  (nothing set)     filed 2025-01-02   → medium, younger
//	#12  low               filed 2025-01-05   → last, and NOT excluded: low is later work
func selectionFixture() []fixtureIssue {
	return []fixtureIssue{
		{num: 10, labels: []string{"flow:implement"}, created: "2025-01-01T00:00:00Z"},
		{num: 11, labels: []string{"flow:implement", "flow:priority:low", "flow:urgency:next"}, created: "2025-03-01T00:00:00Z"},
		{num: 12, labels: []string{"flow:implement", "flow:priority:low"}, created: "2025-01-05T00:00:00Z"},
		{num: 13, labels: []string{"flow:implement", "flow:priority:critical"}, created: "2025-05-01T00:00:00Z"},
		{num: 14, labels: []string{"flow:implement", "flow:priority:high"}, created: "2025-02-01T00:00:00Z"},
		{num: 15, labels: []string{"flow:implement"}, created: "2025-01-02T00:00:00Z"},
	}
}

var selectionOrder = []string{"o/r#11", "o/r#13", "o/r#14", "o/r#10", "o/r#15", "o/r#12"}

func displaysOf[T any](items []T, display func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, display(it))
	}
	return out
}

// ListAutoSelectable returns the documented order: urgency, then priority, then
// age. Age is what makes it an order at all — without a total tiebreak two
// callers reading the same set at the same moment start on different items.
func TestBackend_ListAutoSelectable_ReturnsTheSelectionOrder(t *testing.T) {
	b := discoveringOrchestrator(t, newGHMock(t), selectionFixture(), nil)

	refs, err := b.ListAutoSelectable(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	got := displaysOf(refs, func(r flow.ItemRef) string { return r.Display })
	if !slices.Equal(got, selectionOrder) {
		t.Errorf("order = %v, want %v", got, selectionOrder)
	}
}

// A deferred item is ABSENT, not sorted last: one sorted last is still an item
// a fleet with spare capacity reaches, and "do not start this unattended" is
// exactly what deferring it said. Its priority does not save it — `critical`
// here would otherwise have sorted it second.
func TestBackend_ListAutoSelectable_OmitsADeferredItem(t *testing.T) {
	fixture := append(selectionFixture(), fixtureIssue{
		num:     16,
		labels:  []string{"flow:implement", "flow:priority:critical", "flow:urgency:deferred"},
		created: "2025-01-01T00:00:00Z",
	})
	b := discoveringOrchestrator(t, newGHMock(t), fixture, nil)

	refs, err := b.ListAutoSelectable(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	got := displaysOf(refs, func(r flow.ItemRef) string { return r.Display })
	if slices.Contains(got, "o/r#16") {
		t.Errorf("order = %v, want the deferred item absent from the set, not sorted within it", got)
	}
	if !slices.Equal(got, selectionOrder) {
		t.Errorf("order = %v, want %v — dropping the deferred item must not disturb the rest", got, selectionOrder)
	}

	// At scope `auto` the listing IS the selectable set, so it leaves the same
	// item out. Two answers to "what runs next" that disagreed would send an
	// operator reading `list --scope auto` after an item no `resolve` will take.
	auto, err := b.List(t.Context(), flow.ScopeAuto, "implement", func(flow.ItemType) bool { return true }, nil)
	if err != nil {
		t.Fatalf("List(auto): %v", err)
	}
	listed := displaysOf(auto, func(i flow.ItemInfo) string { return i.Ref.Display })
	if !slices.Equal(listed, got) {
		t.Errorf("scope auto = %v, want the selectable set %v", listed, got)
	}
}

// A label whose suffix names no member is no label at all: the item is STILL
// RETURNED — priority orders, it never excludes — and it sorts where an
// unassessed item sorts, because silence and a misspelling are both "nothing
// has said otherwise".
func TestBackend_ListAutoSelectable_AMisspelledAxisLabelSortsAsTheNeutralValue(t *testing.T) {
	fixture := append(selectionFixture(), fixtureIssue{
		num:     9, // lower than every other, so the age/number tiebreaks are visible
		labels:  []string{"flow:implement", "flow:priority:hihg", "flow:urgency:soon"},
		created: "2025-01-01T00:00:00Z",
	})
	b := discoveringOrchestrator(t, newGHMock(t), fixture, nil)

	refs, err := b.ListAutoSelectable(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	got := displaysOf(refs, func(r flow.ItemRef) string { return r.Display })
	// Filed the same second as #10, which is medium/default; the issue number
	// is the total tiebreak, so it lands immediately before it.
	want := []string{"o/r#11", "o/r#13", "o/r#14", "o/r#9", "o/r#10", "o/r#15", "o/r#12"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// At scope `auto` the listing IS the selectable set, so it is reported in the
// order it will be taken in. At the wider scopes the order is not a contract:
// they are read by a person who scopes and sorts them for themselves.
func TestBackend_List_OrdersScopeAutoAndLeavesWiderScopesAlone(t *testing.T) {
	b := discoveringOrchestrator(t, newGHMock(t), selectionFixture(), nil)
	acceptsAll := func(flow.ItemType) bool { return true }

	auto, err := b.List(t.Context(), flow.ScopeAuto, "implement", acceptsAll, nil)
	if err != nil {
		t.Fatalf("List(auto): %v", err)
	}
	got := displaysOf(auto, func(i flow.ItemInfo) string { return i.Ref.Display })
	if !slices.Equal(got, selectionOrder) {
		t.Errorf("scope auto = %v, want the selection order %v", got, selectionOrder)
	}

	wide, err := b.List(t.Context(), flow.ScopeProcessable, "implement", acceptsAll, nil)
	if err != nil {
		t.Fatalf("List(processable): %v", err)
	}
	gotWide := displaysOf(wide, func(i flow.ItemInfo) string { return i.Ref.Display })
	wantWide := displaysOf(selectionFixture(), func(f fixtureIssue) string {
		return b.refFromIssue(f.num).Display
	})
	if !slices.Equal(gotWide, wantWide) {
		t.Errorf("scope processable = %v, want the API's own order %v", gotWide, wantWide)
	}
}

// An item nothing has said anything about reports medium and default — through
// List, through Get and through Load alike, because all three read the axes
// through the same pair of functions.
func TestBackend_ReportsTheNeutralValuesWhenNeitherLabelIsPresent(t *testing.T) {
	_, b := editingOrchestrator(t, "flow:implement")
	acceptsAll := func(flow.ItemType) bool { return true }

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", acceptsAll, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Priority != flow.PriorityMedium || info.Urgency != flow.UrgencyDefault {
		t.Errorf("Get = %q/%q, want medium/default", info.Priority, info.Urgency)
	}

	it, err := b.Load(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if it.Priority != flow.PriorityMedium || it.Urgency != flow.UrgencyDefault {
		t.Errorf("Load = %q/%q, want medium/default", it.Priority, it.Urgency)
	}
}

// And an item carrying the labels reports what they say — again through every
// read, so `list` and `status` cannot disagree about one item.
func TestBackend_ReportsBothAxesFromTheLabels(t *testing.T) {
	_, b := editingOrchestrator(t, "flow:implement", "flow:priority:critical", "flow:urgency:next")
	acceptsAll := func(flow.ItemType) bool { return true }

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", acceptsAll, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Priority != flow.PriorityCritical || info.Urgency != flow.UrgencyNext {
		t.Errorf("Get = %q/%q, want critical/next", info.Priority, info.Urgency)
	}

	it, err := b.Load(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if it.Priority != flow.PriorityCritical || it.Urgency != flow.UrgencyNext {
		t.Errorf("Load = %q/%q, want critical/next", it.Priority, it.Urgency)
	}
}

// A label naming no member reads as the neutral value everywhere too — one
// state, one spelling. So does a label spelling a neutral value, which the
// schema does not have.
func TestBackend_ReadsAnUnrecognizedAxisLabelAsTheNeutralValue(t *testing.T) {
	for _, labels := range [][]string{
		{"flow:implement", "flow:priority:hihg", "flow:urgency:soon"},
		{"flow:implement", "flow:priority:medium", "flow:urgency:default"},
	} {
		_, b := editingOrchestrator(t, labels...)
		info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true }, nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if info.Priority != flow.PriorityMedium || info.Urgency != flow.UrgencyDefault {
			t.Errorf("labels %v → %q/%q, want medium/default", labels, info.Priority, info.Urgency)
		}
	}
}

// A label naming no member does not MASK a real one. The readers keep looking
// under the prefix rather than answering on the first label they meet, so an
// issue that picked up a hand-typed `flow:priority:hihg` beside a written
// `flow:priority:critical` still reads as critical — otherwise a typo nobody
// can see demotes the item to where nothing had been said about it.
func TestBackend_AnUnrecognizedAxisLabelDoesNotMaskAValidOne(t *testing.T) {
	_, b := editingOrchestrator(t, "flow:implement",
		"flow:priority:hihg", "flow:priority:critical",
		"flow:urgency:soon", "flow:urgency:next")

	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", func(flow.ItemType) bool { return true }, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Priority != flow.PriorityCritical || info.Urgency != flow.UrgencyNext {
		t.Errorf("= %q/%q, want critical/next — the unrecognized label is no label, not an answer",
			info.Priority, info.Urgency)
	}
}

// Deferral is NOT a rung on the availability ladder. An opted-in, assigned,
// unblocked, deferred item has passed every boundary — what is true of it is
// that auto-selection will not take it, which is exactly the boundary
// `available` already marks.
func TestBackend_ADeferredItemReportsAvailableNotAuto(t *testing.T) {
	// An assigned, opted-in issue: every boundary below `auto` is passed, so
	// what the deferral changes is the last one and nothing else.
	assignedOrchestrator := func(labels ...string) *Orchestrator {
		mock := newGHMock(t)
		mock.issueLabels = labels
		mock.assignees = []string{"alice"}
		srv := mock.server()
		t.Cleanup(srv.Close)
		return newMockedOrchestrator(t, mock, srv)
	}
	acceptsAll := func(flow.ItemType) bool { return true }

	b := assignedOrchestrator("flow:implement", "flow:urgency:deferred")
	info, err := b.Get(t.Context(), b.refFromIssue(42), "implement", acceptsAll, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability != flow.AvailAvailable {
		t.Errorf("Availability = %q, want %q", info.Availability, flow.AvailAvailable)
	}
	if info.Urgency != flow.UrgencyDeferred {
		t.Errorf("Urgency = %q, want deferred — the field is what tells the two kinds of `available` apart", info.Urgency)
	}

	// The same item without the label reaches auto, so the assertion above is
	// about deferral and not about the fixture failing some other boundary.
	b2 := assignedOrchestrator("flow:implement")
	opted, err := b2.Get(t.Context(), b2.refFromIssue(42), "implement", acceptsAll, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if opted.Availability != flow.AvailAuto {
		t.Errorf("Availability without the deferral = %q, want %q", opted.Availability, flow.AvailAuto)
	}
}

// ---------------------------------------------------------------------------
// The editor: the typed setters, and the labels they own.
// ---------------------------------------------------------------------------

// Setting a non-neutral value writes exactly one label under the axis prefix —
// so re-setting replaces rather than accumulating, and an item never carries
// two priorities.
func TestEditor_SetsAndReplacesEachAxisLabel(t *testing.T) {
	for _, c := range []struct {
		name  string
		start []string
		stage func(flow.ItemEditor)
		want  string // the one label under the prefix, or "" for none
		pfx   string
	}{{
		name:  "priority written",
		stage: func(ed flow.ItemEditor) { ed.SetPriority(flow.PriorityHigh) },
		want:  "flow:priority:high",
		pfx:   "flow:priority:",
	}, {
		name:  "priority replaced",
		start: []string{"flow:priority:high"},
		stage: func(ed flow.ItemEditor) { ed.SetPriority(flow.PriorityCritical) },
		want:  "flow:priority:critical",
		pfx:   "flow:priority:",
	}, {
		// The neutral value is UNSPELLABLE: setting medium removes the label
		// rather than writing one, so no state is reachable both by a label and
		// by that label's absence.
		name:  "priority cleared to the neutral value",
		start: []string{"flow:priority:high"},
		stage: func(ed flow.ItemEditor) { ed.SetPriority(flow.PriorityMedium) },
		want:  "",
		pfx:   "flow:priority:",
	}, {
		name:  "urgency written",
		stage: func(ed flow.ItemEditor) { ed.SetUrgency(flow.UrgencyNext) },
		want:  "flow:urgency:next",
		pfx:   "flow:urgency:",
	}, {
		name:  "urgency replaced",
		start: []string{"flow:urgency:next"},
		stage: func(ed flow.ItemEditor) { ed.SetUrgency(flow.UrgencyDeferred) },
		want:  "flow:urgency:deferred",
		pfx:   "flow:urgency:",
	}, {
		name:  "urgency cleared to the neutral value",
		start: []string{"flow:urgency:deferred"},
		stage: func(ed flow.ItemEditor) { ed.SetUrgency(flow.UrgencyDefault) },
		want:  "",
		pfx:   "flow:urgency:",
	}} {
		t.Run(c.name, func(t *testing.T) {
			mock, b := editingOrchestrator(t, append([]string{"flow:implement"}, c.start...)...)
			ed, err := b.Edit(t.Context(), b.refFromIssue(42))
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			c.stage(ed)
			if err := ed.Commit(t.Context()); err != nil {
				t.Fatalf("Commit: %v", err)
			}

			var under []string
			for _, l := range mock.labelNames() {
				if strings.HasPrefix(l, c.pfx) {
					under = append(under, l)
				}
			}
			var want []string
			if c.want != "" {
				want = []string{c.want}
			}
			if !slices.Equal(under, want) {
				t.Errorf("labels under %q = %v, want %v (all labels %v)", c.pfx, under, want, mock.labelNames())
			}
			// The rest of the label set is left alone: the axes replace their
			// own prefix and nothing else.
			if !contains(mock.labelNames(), "flow:implement") {
				t.Errorf("labels = %v, want the binary marker untouched", mock.labelNames())
			}
		})
	}
}

// Both axes in ONE commit, against an item carrying both labels and an ordinary
// tag. The two setters write through the same label list, so one that rebuilt
// the list rather than adding to it would silently drop the other's work — and
// the item would report a value nobody set.
func TestEditor_SetsBothAxesInOneCommitAndLeavesEveryOtherLabelAlone(t *testing.T) {
	mock, b := editingOrchestrator(t, "flow:implement", "area:cli", "flow:priority:high", "flow:urgency:next")
	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.SetPriority(flow.PriorityCritical)
	// The neutral value on the other axis: its label is REMOVED, and removing
	// it must not take the priority the same commit just wrote.
	ed.SetUrgency(flow.UrgencyDefault)
	if err := ed.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := slices.Clone(mock.labelNames())
	want := []string{"area:cli", "flow:implement", "flow:priority:critical"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", mock.labelNames(), want)
	}
}

// A value outside the vocabulary is REFUSED, and nothing is written. A closed
// vocabulary needs a parameter that can be refused: stored, `flow:priority:hihg`
// would name nothing and leave the item sorting as though nobody had set it.
func TestEditor_RefusesAValueOutsideEitherVocabulary(t *testing.T) {
	for _, c := range []struct {
		name  string
		stage func(flow.ItemEditor)
	}{
		{"a misspelled priority", func(ed flow.ItemEditor) { ed.SetPriority("hihg") }},
		{"the empty priority", func(ed flow.ItemEditor) { ed.SetPriority("") }},
		{"a misspelled urgency", func(ed flow.ItemEditor) { ed.SetUrgency("soon") }},
		{"the empty urgency", func(ed flow.ItemEditor) { ed.SetUrgency("") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock, b := editingOrchestrator(t, "flow:implement")
			ed, err := b.Edit(t.Context(), b.refFromIssue(42))
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			c.stage(ed)
			if err := ed.Commit(t.Context()); err == nil {
				t.Fatal("Commit accepted a value outside the vocabulary")
			}
			mock.mu.Lock()
			wrote := append([]string(nil), mock.mutations...)
			mock.mu.Unlock()
			if len(wrote) != 0 {
				t.Errorf("mutations = %v, want none: a refused edit writes nothing", wrote)
			}
		})
	}
}

// The axes ride the same PATCH the other fields do, so staging one alongside a
// blocker hits the existing refusal: the two land through different endpoints
// and cannot be written together.
func TestEditor_RefusesAnAxisStagedWithABlocker(t *testing.T) {
	mock, b := editingOrchestrator(t, "flow:implement")
	mock.otherIssues = []int{43}
	ed, err := b.Edit(t.Context(), b.refFromIssue(42))
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	ed.SetPriority(flow.PriorityHigh)
	ed.AddBlocker(b.refFromIssue(43))
	err = ed.Commit(t.Context())
	if err == nil {
		t.Fatal("Commit wrote a patch field and a blocker together")
	}
	if !errors.Is(err, flow.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if contains(mock.labelNames(), "flow:priority:high") {
		t.Errorf("labels = %v, want nothing written", mock.labelNames())
	}
}

// An axis label is a marker this orchestrator maintains, not a binary name.
// otherBinaryLabel reads every `flow:`-prefixed label it does not recognise as
// another flow binary owning the item, so a label the axes introduced and it
// does not skip turns Claim into a permanent "owned by other flow binary"
// refusal — and auto-selection would hand the highest-priority item to a
// runner that then declines it, every time, for as long as the label is there.
func TestBackend_Claim_AnAxisLabelIsNotAnotherBinarysMarker(t *testing.T) {
	for _, label := range []string{
		"flow:priority:critical",
		"flow:priority:high",
		"flow:priority:low",
		"flow:urgency:next",
		"flow:urgency:deferred",
	} {
		t.Run(label, func(t *testing.T) {
			b, mock, rec := newClaimPrecondBackend(t)
			scriptCleanWorktree(rec)
			mock.issueLabels = []string{"flow:implement", label}

			if _, err := b.Claim(t.Context(), b.refFromIssue(42), nil); err != nil {
				var refused flow.ErrClaimRefused
				if errors.As(err, &refused) && refused.Code == "other-binary" {
					t.Fatalf("Claim refused %s as another binary's marker: %s", label, refused.Reason)
				}
				t.Fatalf("Claim: %v", err)
			}
		})
	}
}
