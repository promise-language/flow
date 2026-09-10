package flow

import (
	"strings"
	"testing"
)

func noopHandler(StepCtx) (StepResult, error) { return StepResult{}, nil }

func TestNewFlow_AddStepRegistersInOrder(t *testing.T) {
	f := NewFlow("implement", []ItemType{"task"})
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddStep("implement", "impl", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{Prompts: PromptsAgent})

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

func TestInRemit_Universal(t *testing.T) {
	f := NewFlow("any", nil)
	if !f.InRemit("anything") {
		t.Errorf("empty types should put every type in the remit")
	}
}

func TestInRemit_Filtered(t *testing.T) {
	f := NewFlow("limited", []ItemType{"task", "bug"})
	if !f.InRemit("task") {
		t.Errorf("a declared type should be in the remit")
	}
	if f.InRemit("epic") {
		t.Errorf("an undeclared type should be outside the remit")
	}
}

func TestAddStep_PanicsOnDuplicateName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate step name")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("plan", "plan-a", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddStep("plan", "plan-b", noopHandler, StepConfig{Prompts: PromptsAgent}) // duplicate name
}

func TestAddStep_PanicsOnDuplicateResult(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate artifact result")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("step-a", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddStep("step-b", "plan", noopHandler, StepConfig{Prompts: PromptsAgent}) // duplicate result
}

func TestAddStep_PanicsOnNilHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil handler")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("plan", "plan", nil, StepConfig{Prompts: PromptsAgent})
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
	return ArtifactRecord{Id: id, Type: t, Resolved: true}
}

func TestIsReady_RequireSignal(t *testing.T) {
	f := NewFlow("merge", nil)
	f.RequireSignal("pr-open")
	f.AddStep("merge-step", "merge-commit", noopHandler, StepConfig{Prompts: PromptsAgent})

	state := &Item{Signals: map[SignalId]SignalState{}}
	if f.IsReady(state) {
		t.Errorf("IsReady should be false when precondition signal unset")
	}
	state.Signals["pr-open"] = SignalState{Set: true}
	if !f.IsReady(state) {
		t.Errorf("IsReady should be true once precondition signal set")
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
		Prompts:     PromptsNone,
	}
	f.AddStep("implement the change", "impl", noopHandler, cfg)

	check := func(what string, li LifecycleItem) {
		t.Helper()
		if li.Role != "contributor" {
			t.Errorf("%s: Role = %q, want contributor", what, li.Role)
		}
		if li.Prompts != PromptsNone {
			t.Errorf("%s: Prompts = %q, want %q", what, li.Prompts, PromptsNone)
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

// Every defaulting field's zero value stays legal and means what the code does
// today: nothing established, nothing verified, a handler-produced result, no
// route, no finalization, not the entry. Prompts is the one field with no
// default — a step declares it, and only the wait registers with the zero
// StepConfig.
func TestStepConfig_ZeroValueNormalisesToTheLoosestMembers(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{})

	for _, li := range f.Items() {
		if li.Capture != CaptureReturned {
			t.Errorf("%s: Capture = %q, want %q", li.Description, li.Capture, CaptureReturned)
		}
		if li.Needs != NeedsAny {
			t.Errorf("%s: Needs = %q, want %q", li.Description, li.Needs, NeedsAny)
		}
		if li.Leaves != LeavesAsFound {
			t.Errorf("%s: Leaves = %q, want %q", li.Description, li.Leaves, LeavesAsFound)
		}
		if li.Role != "" {
			t.Errorf("%s: Role = %q, want empty", li.Description, li.Role)
		}
		if li.Entry {
			t.Errorf("%s: Entry = true, want false", li.Description)
		}
		if len(li.Next) != 0 {
			t.Errorf("%s: Next = %v, want none", li.Description, li.Next)
		}
		if len(li.MayFinalize) != 0 {
			t.Errorf("%s: MayFinalize = %v, want none", li.Description, li.MayFinalize)
		}
		if li.Writes != (WriteContract{}) {
			t.Errorf("%s: Writes = %+v, want the zero contract", li.Description, li.Writes)
		}
		if !li.Required {
			t.Errorf("%s: Required = false; every lifecycle item is required", li.Description)
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
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Entry: true})
	mustPanic(t, "already does", func() {
		f.AddStep("implement", "impl", noopHandler, StepConfig{Prompts: PromptsAgent, Entry: true})
	})
}

// The second entry is refused whichever registrar declares it: the invariant
// is one entry per GRAPH, not one per kind of lifecycle item.
func TestAwaitSignal_PanicsOnSecondEntry(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Entry: true})
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
		Prompts:     PromptsAgent,
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
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Capture: "guessed"})
	})
}

func TestAddStep_PanicsOnUnknownNeeds(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "Needs", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Needs: "as-found"})
	})
}

func TestAddStep_PanicsOnUnknownLeaves(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "Leaves", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Leaves: "any"})
	})
}

// Prompts is REQUIRED on a step, and its absence is a defect of its own: none
// as a zero value would publish a guarantee no author wrote, and agent as one
// would leave the property carried by accident, which is what makes a
// property unreliable (docs/flow-registration.md § Step configuration). Both
// registrars that take a handler refuse the omission, naming themselves and
// the step, and the omission is named apart from an unknown value so the
// reader is not sent looking for a typo that is not there.
func TestRegistration_PanicsWhenAStepDeclaresNoPrompts(t *testing.T) {
	cases := []struct {
		registrar string
		declare   func(*Flow)
	}{
		{"flow.AddStep:", func(f *Flow) { f.AddStep("write plan", "plan", noopHandler, StepConfig{}) }},
		{"flow.AddSignalStep:", func(f *Flow) { f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.registrar, func(t *testing.T) {
			f := NewFlow("x", nil)
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("a step registered with no Prompts; every default would be wrong, so the omission must panic")
				}
				msg, _ := r.(string)
				for _, want := range []string{tc.registrar, "declares no Prompts", "agent", "none"} {
					if !strings.Contains(msg, want) {
						t.Errorf("panic = %q, want it to mention %q", msg, want)
					}
				}
			}()
			tc.declare(f)
		})
	}
}

// An out-of-vocabulary value is refused the way an unknown Capture, Needs or
// Leaves is: naming the field and the vocabulary.
func TestAddStep_PanicsOnUnknownPrompts(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "Prompts \"mechanical\", which is not one of [agent none]", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: "mechanical"})
	})
}

// A wait dispatches nothing, so it has no prompt policy: given one it is
// refused where it is written, as a Role or a MayFinalize on a wait is, and
// without one it registers — the zero StepConfig is legal there and nowhere
// else.
func TestAwaitSignal_PanicsOnPromptsAndRegistersWithout(t *testing.T) {
	for _, p := range AllPromptPolicies() {
		t.Run(string(p), func(t *testing.T) {
			f := NewFlow("x", nil)
			mustPanic(t, "dispatches nothing, so it has no prompt policy", func() {
				f.AwaitSignal("await merge", "pr-merged", StepConfig{Prompts: p})
			})
		})
	}
	f := NewFlow("x", nil)
	f.AwaitSignal("await merge", "pr-merged", StepConfig{}) // must not panic
	li, ok := f.ItemByResult("pr-merged")
	if !ok {
		t.Fatal("ItemByResult missing for the registered wait")
	}
	if li.Prompts != "" {
		t.Errorf("a wait's Prompts = %q, want empty: it declares none", li.Prompts)
	}
}

// Mechanical is the ONE definition of "dispatching this invokes no agent": a
// step declaring none, and a wait, which dispatches nothing at all. Every
// consumer — the chokepoint, the envelope, a driver's pacing — reads it, so it
// is pinned here rather than re-derived in each of their tests.
func TestLifecycleItem_Mechanical(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddStep("open branch", "branch", noopHandler, StepConfig{Prompts: PromptsNone})
	f.AddSignalStep("create pr", "pr-open", noopHandler, StepConfig{Prompts: PromptsAgent})
	f.AddSignalStep("merge", "pr-merge", noopHandler, StepConfig{Prompts: PromptsNone})
	f.AwaitSignal("await merge", "pr-merged", StepConfig{})

	want := map[StepId]bool{
		"plan":      false,
		"branch":    true,
		"pr-open":   false,
		"pr-merge":  true,
		"pr-merged": true,
	}
	items := f.Items()
	if len(items) != len(want) {
		t.Fatalf("Items len = %d, want %d", len(items), len(want))
	}
	for _, li := range items {
		if got := li.Mechanical(); got != want[li.Result()] {
			t.Errorf("%s (kind %d, Prompts %q): Mechanical() = %v, want %v",
				li.Result(), li.Kind, li.Prompts, got, want[li.Result()])
		}
	}
}

func TestAddStep_PanicsOnUnknownDisposition(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "not one of", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{
			Prompts:     PromptsAgent,
			MayFinalize: []Disposition{"abandoned"},
		})
	})
}

func TestAddStep_PanicsOnDuplicateDisposition(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "twice in MayFinalize", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{
			Prompts:     PromptsAgent,
			MayFinalize: []Disposition{DispositionResolved, DispositionResolved},
		})
	})
}

func TestAddStep_PanicsOnEmptyNextId(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "empty successor id", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Next: []StepId{""}})
	})
}

func TestAddStep_PanicsOnDuplicateNextId(t *testing.T) {
	f := NewFlow("x", nil)
	mustPanic(t, "twice in Next", func() {
		f.AddStep("write plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent, Next: []StepId{"impl", "impl"}})
	})
}

// The negative companion: everything the invariants above refuse, declared
// legally, registers without complaint. Without it a check that panicked on
// every registration would still pass all of them.
func TestAddStep_FullyPopulatedLegalConfigDoesNotPanic(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("write plan", "plan", noopHandler, StepConfig{
		Prompts:     PromptsNone,
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
		Prompts: PromptsAgent,
		Role:    "contributor",
		Needs:   NeedsItemBranch,
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
	// Each registrar gets its own config: a step must carry Prompts and a wait
	// must not, and a shared config would trip one of those refusals instead
	// of the second-entry one the case is about.
	cases := []struct {
		registrar string
		cfg       StepConfig
		declare   func(*Flow, StepConfig)
	}{
		{"flow.AddStep:", StepConfig{Entry: true, Prompts: PromptsAgent},
			func(f *Flow, cfg StepConfig) { f.AddStep("second", "two", noopHandler, cfg) }},
		{"flow.AddSignalStep:", StepConfig{Entry: true, Prompts: PromptsAgent},
			func(f *Flow, cfg StepConfig) { f.AddSignalStep("second", "two", noopHandler, cfg) }},
		{"flow.AwaitSignal:", StepConfig{Entry: true},
			func(f *Flow, cfg StepConfig) { f.AwaitSignal("second", "two", cfg) }},
	}
	for _, tc := range cases {
		t.Run(tc.registrar, func(t *testing.T) {
			f := NewFlow("x", nil)
			f.AddStep("first", "one", noopHandler, StepConfig{Prompts: PromptsAgent, Entry: true})
			mustPanic(t, tc.registrar, func() { tc.declare(f, tc.cfg) })
		})
	}
}

// The step label is a DESCRIPTION, not a name: display text, never an identity.
func TestLifecycleItem_CarriesTheDescription(t *testing.T) {
	f := NewFlow("resolve", nil)
	f.AddStep("write the implementation plan", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
	li, ok := f.Item("write the implementation plan")
	if !ok {
		t.Fatal("Item did not find the step by its description")
	}
	if li.Description != "write the implementation plan" {
		t.Errorf("Description = %q, want the registered description", li.Description)
	}
	if li.Result() != "plan" {
		t.Errorf("Result = %q, want the result id — the step's identity", li.Result())
	}
}

func TestAddStep_PanicsOnEmptyDescription(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on an empty step description")
		}
		if !strings.Contains(r.(string), "empty step description") {
			t.Errorf("panic = %v, want it to name the empty description", r)
		}
	}()
	NewFlow("x", nil).AddStep("", "plan", noopHandler, StepConfig{Prompts: PromptsAgent})
}
