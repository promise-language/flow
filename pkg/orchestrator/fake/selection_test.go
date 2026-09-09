package fake_test

import (
	"slices"
	"testing"
	"time"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// The fake's part of the selection contract: the order, the deferral, and the
// two axes as editable fields.

// addAged registers an item filed at `at` — the fake's only source of an age is
// its clock at AddItem time, so a test giving items distinct ages moves the
// clock between calls.
func addAged(b *fake.Orchestrator, id string, at time.Time, p flow.Priority, u flow.Urgency) flow.ItemRef {
	b.SetClock(func() time.Time { return at })
	it := newItem(id)
	it.Priority = p
	it.Urgency = u
	b.AddItem(id, it)
	return itemRef(id)
}

var (
	jan = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	feb = time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	mar = time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
)

// The ids are deliberately alphabetical against the selection order, so a
// listing that fell back to the fake's display-order sort would fail here.
func selectionSet(b *fake.Orchestrator) {
	addAged(b, "a-old-medium", jan, "", "")
	addAged(b, "b-new-critical", mar, flow.PriorityCritical, "")
	addAged(b, "c-next-low", mar, flow.PriorityLow, flow.UrgencyNext)
	addAged(b, "d-old-low", jan, flow.PriorityLow, "")
	addAged(b, "e-new-medium", feb, flow.PriorityMedium, flow.UrgencyDefault)
}

var fakeSelectionOrder = []string{
	"c-next-low",     // an instruction outranks every assessment
	"b-new-critical", // then the assessments, highest first
	"a-old-medium",   // medium: the unset item and the explicitly-medium one,
	"e-new-medium",   // oldest first between them
	"d-old-low",      // low is later work, never excluded work
}

func TestFake_ListAutoSelectable_ReturnsTheSelectionOrder(t *testing.T) {
	b := fake.New()
	selectionSet(b)

	refs, err := b.ListAutoSelectable(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	got := make([]string, 0, len(refs))
	for _, r := range refs {
		got = append(got, r.Display)
	}
	if !slices.Equal(got, fakeSelectionOrder) {
		t.Errorf("order = %v, want %v", got, fakeSelectionOrder)
	}
}

// A deferred item is ABSENT from the selectable set, whatever its priority
// says — not sorted last, where a fleet with spare capacity would still reach
// it.
func TestFake_ListAutoSelectable_OmitsADeferredItem(t *testing.T) {
	b := fake.New()
	selectionSet(b)
	addAged(b, "f-deferred-critical", jan, flow.PriorityCritical, flow.UrgencyDeferred)

	refs, err := b.ListAutoSelectable(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	got := make([]string, 0, len(refs))
	for _, r := range refs {
		got = append(got, r.Display)
	}
	if !slices.Equal(got, fakeSelectionOrder) {
		t.Errorf("order = %v, want %v with the deferred item absent", got, fakeSelectionOrder)
	}

	// And it is still reachable by name: deferring an item is not disabling it.
	if _, err := b.Load(t.Context(), itemRef("f-deferred-critical")); err != nil {
		t.Errorf("Load of a deferred item: %v — a deferred item is driven normally by anyone who names it", err)
	}
}

// Scope `auto` IS the selectable set, so it is listed in the order it will be
// taken in. The wider scopes keep the fake's own display order.
func TestFake_List_OrdersScopeAutoOnly(t *testing.T) {
	b := fake.New()
	selectionSet(b)
	acceptsAll := func(flow.ItemType) bool { return true }

	auto, err := b.List(t.Context(), flow.ScopeAuto, "test", acceptsAll, nil)
	if err != nil {
		t.Fatalf("List(auto): %v", err)
	}
	got := make([]string, 0, len(auto))
	for _, it := range auto {
		got = append(got, it.Ref.Display)
	}
	if !slices.Equal(got, fakeSelectionOrder) {
		t.Errorf("scope auto = %v, want the selection order %v", got, fakeSelectionOrder)
	}

	wide, err := b.List(t.Context(), flow.ScopeProcessable, "test", acceptsAll, nil)
	if err != nil {
		t.Fatalf("List(processable): %v", err)
	}
	gotWide := make([]string, 0, len(wide))
	for _, it := range wide {
		gotWide = append(gotWide, it.Ref.Display)
	}
	wantWide := slices.Clone(fakeSelectionOrder)
	slices.Sort(wantWide)
	if !slices.Equal(gotWide, wantWide) {
		t.Errorf("scope processable = %v, want the display order %v", gotWide, wantWide)
	}
}

// Deferral is not a rung of its own: a deferred item has passed every boundary
// `available` marks, and Urgency is what says which kind of `available` it is.
func TestFake_ADeferredItemReportsAvailableNotAuto(t *testing.T) {
	b := fake.New()
	ref := addAged(b, "deferred", jan, "", flow.UrgencyDeferred)
	plain := addAged(b, "plain", jan, "", "")
	acceptsAll := func(flow.ItemType) bool { return true }

	info, err := b.Get(t.Context(), ref, "test", acceptsAll, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Availability != flow.AvailAvailable {
		t.Errorf("Availability = %q, want %q", info.Availability, flow.AvailAvailable)
	}
	other, err := b.Get(t.Context(), plain, "test", acceptsAll, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if other.Availability != flow.AvailAuto {
		t.Errorf("Availability without the deferral = %q, want %q", other.Availability, flow.AvailAuto)
	}
}

// An item nothing has said anything about reports medium and default — never
// the empty value — through both reads.
func TestFake_ReportsTheNeutralValuesWhenNothingIsSet(t *testing.T) {
	b := fake.New()
	ref := addItem(b, "unset")

	info, err := b.Get(t.Context(), ref, "test", func(flow.ItemType) bool { return true }, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Priority != flow.PriorityMedium || info.Urgency != flow.UrgencyDefault {
		t.Errorf("Get = %q/%q, want medium/default", info.Priority, info.Urgency)
	}
	it, err := b.Load(t.Context(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if it.Priority != flow.PriorityMedium || it.Urgency != flow.UrgencyDefault {
		t.Errorf("Load = %q/%q, want medium/default", it.Priority, it.Urgency)
	}
}

// The editor round-trip: both axes set, read back, and cleared to the neutral
// value again. Clearing is a value like any other here — the fake stores a
// field, not a label, so there is no spelling to remove.
func TestFake_EditorSetsAndClearsBothAxes(t *testing.T) {
	b := fake.New()
	ref := addItem(b, "edited")

	edit := func(stage func(flow.ItemEditor)) error {
		ed, err := b.Edit(t.Context(), ref)
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		stage(ed)
		return ed.Commit(t.Context())
	}

	if err := edit(func(ed flow.ItemEditor) {
		ed.SetPriority(flow.PriorityCritical)
		ed.SetUrgency(flow.UrgencyDeferred)
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	it, err := b.Load(t.Context(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if it.Priority != flow.PriorityCritical || it.Urgency != flow.UrgencyDeferred {
		t.Fatalf("after setting = %q/%q, want critical/deferred", it.Priority, it.Urgency)
	}
	// And the deferral takes effect where it is meant to.
	refs, err := b.ListAutoSelectable(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("ListAutoSelectable: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("selectable = %v, want the deferred item out of the set", refs)
	}

	if err := edit(func(ed flow.ItemEditor) {
		ed.SetPriority(flow.PriorityMedium)
		ed.SetUrgency(flow.UrgencyDefault)
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	it, err = b.Load(t.Context(), ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if it.Priority != flow.PriorityMedium || it.Urgency != flow.UrgencyDefault {
		t.Errorf("after clearing = %q/%q, want medium/default", it.Priority, it.Urgency)
	}
	if refs, err := b.ListAutoSelectable(t.Context(), nil, nil); err != nil || len(refs) != 1 {
		t.Errorf("selectable = %v (err %v), want the item back in the set", refs, err)
	}
}

// A closed vocabulary needs a parameter that can be refused: a value naming no
// member is refused at Commit, and nothing is stored.
func TestFake_EditorRefusesAValueOutsideEitherVocabulary(t *testing.T) {
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
			b := fake.New()
			ref := addAged(b, "item", jan, flow.PriorityHigh, flow.UrgencyNext)
			ed, err := b.Edit(t.Context(), ref)
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			c.stage(ed)
			if err := ed.Commit(t.Context()); err == nil {
				t.Fatal("Commit accepted a value outside the vocabulary")
			}
			it, err := b.Load(t.Context(), ref)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if it.Priority != flow.PriorityHigh || it.Urgency != flow.UrgencyNext {
				t.Errorf("after a refused commit = %q/%q, want the item unchanged at high/next", it.Priority, it.Urgency)
			}
		})
	}
}
