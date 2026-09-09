package flow

import (
	"strings"
	"testing"
)

func noopHandler(StepCtx) error { return nil }

func TestNewFlow_AddStepRegistersInOrder(t *testing.T) {
	f := NewFlow("implement", []ItemType{"task"})
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
	f.AddStep("implement", "impl", noopHandler, StepConfig{})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{})

	got := f.Steps()
	want := []string{"write plan", "implement", "create pr"}
	if len(got) != len(want) {
		t.Fatalf("Steps len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Steps[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewFlow_AcceptsTypeUniversal(t *testing.T) {
	f := NewFlow("any", nil)
	if !f.AcceptsType("anything") {
		t.Errorf("empty types should accept any type")
	}
}

func TestNewFlow_AcceptsTypeFiltered(t *testing.T) {
	f := NewFlow("limited", []ItemType{"task", "bug"})
	if !f.AcceptsType("task") {
		t.Errorf("flow should accept declared type")
	}
	if f.AcceptsType("epic") {
		t.Errorf("flow should reject undeclared type")
	}
}

func TestAddStep_PanicsOnDuplicateName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate step name")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("plan", "plan-a", noopHandler, StepConfig{})
	f.AddStep("plan", "plan-b", noopHandler, StepConfig{}) // duplicate name
}

func TestAddStep_PanicsOnDuplicateResult(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate artifact result")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("step-a", "plan", noopHandler, StepConfig{})
	f.AddStep("step-b", "plan", noopHandler, StepConfig{}) // duplicate result
}

func TestAddStep_PanicsOnNilHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil handler")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("plan", "plan", nil, StepConfig{})
}

func TestAwaitSignal_AllowsNoHandler(t *testing.T) {
	f := NewFlow("x", nil)
	f.AwaitSignal("wait for merge", "pr-merged", StepConfig{}) // must not panic
	if len(f.Steps()) != 1 {
		t.Fatalf("Steps len = %d, want 1", len(f.Steps()))
	}
}

func TestRequireSignal_Records(t *testing.T) {
	f := NewFlow("merge", nil)
	f.RequireSignal("pr-open")
	got := f.RequireSignals()
	if len(got) != 1 || got[0] != "pr-open" {
		t.Errorf("RequireSignals() = %v, want [pr-open]", got)
	}
}

func resolvedArtifact(id ArtifactId, t ArtifactType) ArtifactRecord {
	return ArtifactRecord{Id: id, Type: t, Required: true, Resolved: true}
}

func TestDeriveNext_FirstUnresolved(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
	f.AddStep("implement", "impl", noopHandler, StepConfig{})
	f.AddStep("review", "review", noopHandler, StepConfig{})

	state := &Item{
		Artifacts: map[ArtifactId]ArtifactRecord{
			"plan": resolvedArtifact("plan", ArtifactMarkdown),
			// "impl" unresolved
		},
	}
	next, ok := f.DeriveNext(state)
	if !ok || next != "implement" {
		t.Errorf("DeriveNext = (%q, %v), want (\"implement\", true)", next, ok)
	}
}

func TestDeriveNext_StaleArtifactIsPending(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})

	state := &Item{
		Artifacts: map[ArtifactId]ArtifactRecord{
			"plan": {Id: "plan", Type: ArtifactMarkdown, Required: true, Resolved: true, Stale: true},
		},
	}
	next, ok := f.DeriveNext(state)
	if !ok || next != "write plan" {
		t.Errorf("stale artifact should be pending; got (%q, %v)", next, ok)
	}
}

func TestDeriveNext_AllResolvedReturnsFalse(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
	f.AddStep("implement", "impl", noopHandler, StepConfig{})

	state := &Item{
		Artifacts: map[ArtifactId]ArtifactRecord{
			"plan": resolvedArtifact("plan", ArtifactMarkdown),
			"impl": resolvedArtifact("impl", ArtifactPatch),
		},
	}
	if _, ok := f.DeriveNext(state); ok {
		t.Errorf("DeriveNext should return ok=false when all resolved")
	}
}

func TestDeriveNext_SignalStepPendingUntilSet(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{})

	state := &Item{
		Artifacts: map[ArtifactId]ArtifactRecord{
			"plan": resolvedArtifact("plan", ArtifactMarkdown),
		},
		Signals: map[SignalId]SignalState{},
	}
	next, ok := f.DeriveNext(state)
	if !ok || next != "create pr" {
		t.Errorf("DeriveNext = (%q, %v), want (\"create pr\", true)", next, ok)
	}

	// flip signal — step should complete
	state.Signals["pr-open"] = SignalState{Set: true}
	if _, ok := f.DeriveNext(state); ok {
		t.Errorf("DeriveNext should be done once signal set")
	}
}

func TestAwaitSignal_PendingUntilSet(t *testing.T) {
	f := NewFlow("observe", nil)
	f.AwaitSignal("await merge", "pr-merged", StepConfig{})

	state := &Item{Signals: map[SignalId]SignalState{}}
	next, ok := f.DeriveNext(state)
	if !ok || next != "await merge" {
		t.Errorf("await should be pending; got (%q, %v)", next, ok)
	}

	state.Signals["pr-merged"] = SignalState{Set: true}
	if _, ok := f.DeriveNext(state); ok {
		t.Errorf("await should complete once signal set")
	}
}

func TestIsReady_RequireSignal(t *testing.T) {
	f := NewFlow("merge", nil)
	f.RequireSignal("pr-open")
	f.AddStep("merge-step", "merge-commit", noopHandler, StepConfig{})

	state := &Item{Signals: map[SignalId]SignalState{}}
	if f.IsReady(state) {
		t.Errorf("IsReady should be false when precondition signal unset")
	}
	state.Signals["pr-open"] = SignalState{Set: true}
	if !f.IsReady(state) {
		t.Errorf("IsReady should be true once precondition signal set")
	}
}

func TestIsDone_EveryStepResolved(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("first", "one", noopHandler, StepConfig{})
	f.AddStep("second", "two", noopHandler, StepConfig{})

	state := &Item{Artifacts: map[ArtifactId]ArtifactRecord{}}
	if f.IsDone(state) {
		t.Errorf("IsDone should be false with both steps unresolved")
	}
	state.Artifacts["one"] = resolvedArtifact("one", ArtifactMarkdown)
	if f.IsDone(state) {
		t.Errorf("IsDone should still be false with the second step unresolved — there is no optional step")
	}
	state.Artifacts["two"] = resolvedArtifact("two", ArtifactMarkdown)
	if !f.IsDone(state) {
		t.Errorf("IsDone should be true once every step is resolved")
	}
}

// The operator's opt-out survives: it lives on the persisted RECORD, not in
// the declaration, so removing step optionality does not remove the ability to
// strike a step off one item's checklist.
func TestIsDone_UnresolvedRecordMarkedNotRequiredIsSkipped(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("first", "one", noopHandler, StepConfig{})
	f.AddStep("second", "two", noopHandler, StepConfig{})

	state := &Item{Artifacts: map[ArtifactId]ArtifactRecord{
		"one": resolvedArtifact("one", ArtifactMarkdown),
		"two": {Id: "two", Type: ArtifactMarkdown, Required: false, Resolved: false},
	}}
	if !f.IsDone(state) {
		t.Errorf("IsDone should be true: the unresolved record is marked not-required by the operator")
	}
}

func TestSeedSpec_ResolvesBudgetFromPolicy(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("a", "art-a", noopHandler, StepConfig{})
	f.AddStep("b", "art-b", noopHandler, StepConfig{})
	f.AddSignalStep("sig", "pr-open", noopHandler, StepConfig{}) // signal steps not in seed

	defs := map[ArtifactId]ArtifactDef{
		"art-a": {Id: "art-a", Type: ArtifactMarkdown},
		"art-b": {Id: "art-b", Type: ArtifactMarkdown},
	}
	// art-a is in the policy; art-b is not, and takes the package defaults.
	budgets := map[StepId]StepBudget{"art-a": {MaxInvocations: 2}}

	specs := f.SeedSpec(defs, budgets)
	if len(specs) != 2 {
		t.Fatalf("SeedSpec len = %d, want 2 (signal steps excluded)", len(specs))
	}
	byId := map[ArtifactId]ArtifactSpec{}
	for _, sp := range specs {
		byId[sp.Id] = sp
	}

	a := byId["art-a"]
	if a.Type != ArtifactMarkdown || !a.Required {
		t.Errorf("art-a spec = %+v, want markdown required", a)
	}
	if a.Budget.MaxInvocations != 2 {
		t.Errorf("art-a MaxInvocations = %d, want 2 (from policy)", a.Budget.MaxInvocations)
	}
	if a.Budget.Timeout != DefaultStepBudget().Timeout {
		t.Errorf("art-a Timeout = %v, want default %v (unset axis)", a.Budget.Timeout, DefaultStepBudget().Timeout)
	}

	if b := byId["art-b"]; b.Budget != DefaultStepBudget() {
		t.Errorf("art-b budget = %+v, want the package defaults whole (absent from policy)", b.Budget)
	}
}

func TestSeedSpec_NilPolicyIsAllDefaults(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("a", "art-a", noopHandler, StepConfig{})

	specs := f.SeedSpec(map[ArtifactId]ArtifactDef{"art-a": {Id: "art-a", Type: ArtifactMarkdown}}, nil)
	if len(specs) != 1 {
		t.Fatalf("SeedSpec len = %d, want 1", len(specs))
	}
	if specs[0].Budget != DefaultStepBudget() {
		t.Errorf("budget = %+v, want the package defaults whole", specs[0].Budget)
	}
}

func TestTerminalReason(t *testing.T) {
	f := NewFlow("merge", nil)
	f.RequireSignal("pr-open")
	f.AddStep("merge-step", "merge-commit", noopHandler, StepConfig{})

	state := &Item{Signals: map[SignalId]SignalState{}}
	if r := f.TerminalReason(state); r != "awaiting-preconditions" {
		t.Errorf("TerminalReason = %q, want awaiting-preconditions", r)
	}

	state.Signals["pr-open"] = SignalState{Set: true}
	if r := f.TerminalReason(state); r != "" {
		t.Errorf("TerminalReason = %q, want empty (still has pending step)", r)
	}

	state.Artifacts = map[ArtifactId]ArtifactRecord{
		"merge-commit": resolvedArtifact("merge-commit", ArtifactCommitHash),
	}
	if r := f.TerminalReason(state); r != "done" {
		t.Errorf("TerminalReason = %q, want done", r)
	}
}

// --- The declaration surface: what StepConfig carries onto LifecycleItem ---

// A fully-populated registration is legal, and every field survives the trip
// through the flow onto the orchestrator-facing view — by name lookup, by
// result lookup, and in the ordered list, because a field that arrived on one
// of the three and not the others would be a field only some callers can see.
func TestStepConfig_EveryFieldRoundTripsOntoLifecycleItem(t *testing.T) {
	f := NewFlow("x", nil)
	cfg := StepConfig{
		Role:        "contributor",
		Entry:       true,
		Next:        []StepId{"review", "pr-open"},
		MayFinalize: []Disposition{DispositionResolved, DispositionRejected},
		Capture:     CaptureTree,
		Needs:       NeedsItemBranch,
		Writes:      WriteContract{MayCommit: true, MayEditTree: true},
		Leaves:      LeavesItemBranch,
	}
	f.AddStep("implement the change", "impl", noopHandler, cfg)

	check := func(what string, li LifecycleItem) {
		t.Helper()
		if li.Role != "contributor" {
			t.Errorf("%s: Role = %q, want contributor", what, li.Role)
		}
		if !li.Entry {
			t.Errorf("%s: Entry = false, want true", what)
		}
		if len(li.Next) != 2 || li.Next[0] != "review" || li.Next[1] != "pr-open" {
			t.Errorf("%s: Next = %v, want [review pr-open]", what, li.Next)
		}
		if len(li.MayFinalize) != 2 || li.MayFinalize[0] != DispositionResolved || li.MayFinalize[1] != DispositionRejected {
			t.Errorf("%s: MayFinalize = %v, want [resolved rejected]", what, li.MayFinalize)
		}
		if li.Capture != CaptureTree {
			t.Errorf("%s: Capture = %q, want %q", what, li.Capture, CaptureTree)
		}
		if li.Needs != NeedsItemBranch {
			t.Errorf("%s: Needs = %q, want %q", what, li.Needs, NeedsItemBranch)
		}
		if li.Writes != (WriteContract{MayCommit: true, MayEditTree: true}) {
			t.Errorf("%s: Writes = %+v, want {MayCommit, MayEditTree}", what, li.Writes)
		}
		if li.Leaves != LeavesItemBranch {
			t.Errorf("%s: Leaves = %q, want %q", what, li.Leaves, LeavesItemBranch)
		}
	}

	li, ok := f.Item("implement the change")
	if !ok {
		t.Fatal("Item missing for the registered step")
	}
	check("Item", li)

	byResult, ok := f.ItemByResult("impl")
	if !ok {
		t.Fatal("ItemByResult missing for the registered step")
	}
	check("ItemByResult", byResult)

	items := f.Items()
	if len(items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(items))
	}
	check("Items", items[0])
}

// The zero value stays legal and means what the code does today: nothing
// established, nothing verified, a handler-produced result, no route, no
// finalization, not the entry.
func TestStepConfig_ZeroValueNormalisesToTheLoosestMembers(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{})

	for _, li := range f.Items() {
		if li.Capture != CaptureReturned {
			t.Errorf("%s: Capture = %q, want %q", li.Name, li.Capture, CaptureReturned)
		}
		if li.Needs != NeedsAny {
			t.Errorf("%s: Needs = %q, want %q", li.Name, li.Needs, NeedsAny)
		}
		if li.Leaves != LeavesAsFound {
			t.Errorf("%s: Leaves = %q, want %q", li.Name, li.Leaves, LeavesAsFound)
		}
		if li.Role != "" {
			t.Errorf("%s: Role = %q, want empty", li.Name, li.Role)
		}
		if li.Entry {
			t.Errorf("%s: Entry = true, want false", li.Name)
		}
		if len(li.Next) != 0 {
			t.Errorf("%s: Next = %v, want none", li.Name, li.Next)
		}
		if len(li.MayFinalize) != 0 {
			t.Errorf("%s: MayFinalize = %v, want none", li.Name, li.MayFinalize)
		}
		if li.Writes != (WriteContract{}) {
			t.Errorf("%s: Writes = %+v, want the zero contract", li.Name, li.Writes)
		}
		if !li.Required {
			t.Errorf("%s: Required = false; every lifecycle item is required", li.Name)
		}
	}
}

// --- Construction-time invariants ---

// mustPanic runs fn and fails unless it panicked with a message containing
// `want`. One helper because every registration invariant below is the same
// assertion over a different declaration, and a message check is what tells a
// panic on the declared defect apart from a panic on something else.
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic mentioning %q, got none", want)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, want) {
			t.Fatalf("panic = %q, want it to mention %q", msg, want)
		}
	}()
	fn()
}

func TestAddStep_PanicsOnSecondEntry(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Entry: true})
	mustPanic(t, "already does", func() {
		f.AddStep("implement", "impl", noopHandler, StepConfig{Entry: true})
	})
}

// The second entry is refused whichever registrar declares it: the invariant
// is one entry per GRAPH, not one per kind of lifecycle item.
func TestAwaitSignal_PanicsOnSecondEntry(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Entry: true})
	mustPanic(t, "already does", func() {
		f.AwaitSignal("await merge", "pr-merged", StepConfig{Entry: true})
	})
}

func TestAwaitSignal_PanicsOnRole(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "belong to no role", func() {
		f.AwaitSignal("await merge", "pr-merged", StepConfig{Role: "contributor"})
	})
}

// A wait elects nothing, so it cannot finalize — and the declaration would not
// merely be inert: ValidateGraph reads MayFinalize as "this item can end the
// flow", so a wait carrying one certifies finalize-reachability for a graph
// where nothing ever finalizes.
func TestAwaitSignal_PanicsOnMayFinalize(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "cannot finalize", func() {
		f.AwaitSignal("await merge", "pr-merged", StepConfig{
			Next:        []StepId{"plan"},
			MayFinalize: []Disposition{DispositionResolved},
		})
	})
}

// Next and MayFinalize are slices, and a slice handed across a boundary is
// shared unless it is copied. The graph is what startup validation certifies,
// so neither the caller that registered a step nor a caller reading the
// declaration back may still hold a handle that rewrites it.
func TestStepConfig_DeclaredSlicesAreCopiedInAndOut(t *testing.T) {
	f := NewFlow("x", nil)
	next := []StepId{"impl"}
	finals := []Disposition{DispositionResolved}
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Entry:       true,
		Next:        next,
		MayFinalize: finals,
	})

	// The caller still holds both slices.
	next[0] = "somewhere-else"
	finals[0] = DispositionRejected

	li, ok := f.ItemByResult("plan")
	if !ok {
		t.Fatal("ItemByResult missing for the registered step")
	}
	if li.Next[0] != "impl" {
		t.Errorf("Next[0] = %q after the caller mutated its slice, want impl", li.Next[0])
	}
	if li.MayFinalize[0] != DispositionResolved {
		t.Errorf("MayFinalize[0] = %q after the caller mutated its slice, want %q",
			li.MayFinalize[0], DispositionResolved)
	}

	// And the view handed back is itself a copy.
	li.Next[0] = "somewhere-else"
	li.MayFinalize[0] = DispositionRejected
	again, _ := f.ItemByResult("plan")
	if again.Next[0] != "impl" {
		t.Errorf("Next[0] = %q after mutating a returned LifecycleItem, want impl", again.Next[0])
	}
	if again.MayFinalize[0] != DispositionResolved {
		t.Errorf("MayFinalize[0] = %q after mutating a returned LifecycleItem, want %q",
			again.MayFinalize[0], DispositionResolved)
	}
}

func TestAddStep_PanicsOnUnknownCapture(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "Capture", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Capture: "guessed"})
	})
}

func TestAddStep_PanicsOnUnknownNeeds(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "Needs", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Needs: "as-found"})
	})
}

func TestAddStep_PanicsOnUnknownLeaves(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "Leaves", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Leaves: "any"})
	})
}

func TestAddStep_PanicsOnUnknownDisposition(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "not one of", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{
			MayFinalize: []Disposition{"abandoned"},
		})
	})
}

func TestAddStep_PanicsOnDuplicateDisposition(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "twice in MayFinalize", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{
			MayFinalize: []Disposition{DispositionResolved, DispositionResolved},
		})
	})
}

func TestAddStep_PanicsOnEmptyNextId(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "empty successor id", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Next: []StepId{""}})
	})
}

func TestAddStep_PanicsOnDuplicateNextId(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "twice in Next", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Next: []StepId{"impl", "impl"}})
	})
}

// The negative companion: everything the invariants above refuse, declared
// legally, registers without complaint. Without it a check that panicked on
// every registration would still pass all of them.
func TestAddStep_FullyPopulatedLegalConfigDoesNotPanic(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Role:        "contributor",
		Entry:       true,
		Next:        []StepId{"impl", "pr-open"},
		MayFinalize: []Disposition{DispositionResolved, DispositionRejected},
		Capture:     CaptureTree,
		Needs:       NeedsBase,
		Writes:      WriteContract{MayBranch: true},
		Leaves:      LeavesItemBranch,
	})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{
		Role:  "contributor",
		Needs: NeedsItemBranch,
	})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{Next: []StepId{"plan"}})
	if len(f.Steps()) != 3 {
		t.Fatalf("Steps len = %d, want 3", len(f.Steps()))
	}
}

// All three registrars share prepareStep, and the one thing that is NOT shared
// is the `registrar` argument naming the call that was made. Nothing else
// asserts it is passed correctly, so a copy-paste that hard-coded "AddStep" in
// all three would leave every other registration test green while sending a
// reader chasing a panic to the wrong declaration.
//
// The second-entry defect is the vehicle because it is the one invariant that
// needs a step already registered, which also proves prepareStep is reached
// from each registrar at all — the signal-step path through it is otherwise
// unexercised.
func TestRegistration_PanicNamesTheRegistrarThatWasCalled(t *testing.T) {
	cases := []struct {
		registrar string
		declare   func(*Flow, StepConfig)
	}{
		{"flow.AddStep:", func(f *Flow, cfg StepConfig) { f.AddStep("second", "two", noopHandler, cfg) }},
		{"flow.AddSignalStep:", func(f *Flow, cfg StepConfig) { f.AddSignalStep("second", "two", noopHandler, cfg) }},
		{"flow.AwaitSignal:", func(f *Flow, cfg StepConfig) { f.AwaitSignal("second", "two", cfg) }},
	}
	for _, tc := range cases {
		t.Run(tc.registrar, func(t *testing.T) {
			f := NewFlow("x", nil)
			f.AddStep("first", "one", noopHandler, StepConfig{Entry: true})
			mustPanic(t, tc.registrar, func() { tc.declare(f, StepConfig{Entry: true}) })
		})
	}
}
