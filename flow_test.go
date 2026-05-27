package flow

import "testing"

func noopHandler(StepCtx) error { return nil }

func TestNewFlow_AddStepRegistersInOrder(t *testing.T) {
	f := NewFlow("implement", []ItemType{"task"})
	f.AddStep("write plan", "plan", noopHandler)
	f.AddStep("implement", "impl", noopHandler)
	f.AddSignalStep("create pr", "pr-open", noopHandler)

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
	f.AddStep("plan", "plan-a", noopHandler)
	f.AddStep("plan", "plan-b", noopHandler) // duplicate name
}

func TestAddStep_PanicsOnDuplicateResult(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate artifact result")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("step-a", "plan", noopHandler)
	f.AddStep("step-b", "plan", noopHandler) // duplicate result
}

func TestAddStep_PanicsOnNilHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil handler")
		}
	}()
	f := NewFlow("x", nil)
	f.AddStep("plan", "plan", nil)
}

func TestAwaitSignal_AllowsNoHandler(t *testing.T) {
	f := NewFlow("x", nil)
	f.AwaitSignal("wait for merge", "pr-merged") // must not panic
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
	f.AddStep("write plan", "plan", noopHandler)
	f.AddStep("implement", "impl", noopHandler)
	f.AddStep("review", "review", noopHandler)

	state := &ItemState{
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
	f.AddStep("write plan", "plan", noopHandler)

	state := &ItemState{
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
	f.AddStep("write plan", "plan", noopHandler)
	f.AddStep("implement", "impl", noopHandler)

	state := &ItemState{
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
	f.AddStep("write plan", "plan", noopHandler)
	f.AddSignalStep("create pr", "pr-open", noopHandler)

	state := &ItemState{
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
	f.AwaitSignal("await merge", "pr-merged")

	state := &ItemState{Signals: map[SignalId]SignalState{}}
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
	f.AddStep("merge-step", "merge-commit", noopHandler)

	state := &ItemState{Signals: map[SignalId]SignalState{}}
	if f.IsReady(state) {
		t.Errorf("IsReady should be false when precondition signal unset")
	}
	state.Signals["pr-open"] = SignalState{Set: true}
	if !f.IsReady(state) {
		t.Errorf("IsReady should be true once precondition signal set")
	}
}

func TestIsDone_AllRequiredResolved(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("required", "req", noopHandler)
	f.AddStep("optional", "opt", noopHandler, Optional)

	state := &ItemState{Artifacts: map[ArtifactId]ArtifactRecord{}}
	if f.IsDone(state) {
		t.Errorf("IsDone should be false with required unresolved")
	}
	state.Artifacts["req"] = resolvedArtifact("req", ArtifactMarkdown)
	if !f.IsDone(state) {
		t.Errorf("IsDone should be true once all required resolved (optional missing)")
	}
}

func TestStepBudget_AppliesOptions(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("with-overrides", "art", noopHandler,
		MaxInvocations(5),
		MaxPromptsPerInvocation(3),
		MaxCostUSD(50),
	)
	f.AddStep("with-defaults", "def", noopHandler)

	got, ok := f.StepBudget("with-overrides")
	if !ok {
		t.Fatal("StepBudget missing for with-overrides")
	}
	if got.MaxInvocations != 5 || got.MaxPromptsPerInvocation != 3 || got.MaxCostUSD != 50 {
		t.Errorf("override budget = %+v, want {5,3,50,30m}", got)
	}
	if got.Timeout != DefaultStepBudget().Timeout {
		t.Errorf("Timeout = %v, want default %v", got.Timeout, DefaultStepBudget().Timeout)
	}

	dflt, _ := f.StepBudget("with-defaults")
	if dflt != DefaultStepBudget() {
		t.Errorf("unconfigured step should match default budget; got %+v", dflt)
	}
}

func TestSeedSpec_ResolvedBudget(t *testing.T) {
	f := NewFlow("x", nil)
	f.AddStep("a", "art-a", noopHandler, MaxInvocations(2))
	f.AddSignalStep("sig", "pr-open", noopHandler) // signal steps not in seed

	defs := map[ArtifactId]ArtifactDef{
		"art-a": {Id: "art-a", Type: ArtifactMarkdown},
	}
	specs := f.SeedSpec(defs)
	if len(specs) != 1 {
		t.Fatalf("SeedSpec len = %d, want 1 (signal steps excluded)", len(specs))
	}
	sp := specs[0]
	if sp.Id != "art-a" || sp.Type != ArtifactMarkdown || !sp.Required {
		t.Errorf("spec = %+v, want artifact 'art-a' markdown required", sp)
	}
	if sp.Budget.MaxInvocations != 2 {
		t.Errorf("budget.MaxInvocations = %d, want 2", sp.Budget.MaxInvocations)
	}
	if sp.Budget.Timeout != DefaultStepBudget().Timeout {
		t.Errorf("budget.Timeout = %v, want default %v", sp.Budget.Timeout, DefaultStepBudget().Timeout)
	}
}

func TestTerminalReason(t *testing.T) {
	f := NewFlow("merge", nil)
	f.RequireSignal("pr-open")
	f.AddStep("merge-step", "merge-commit", noopHandler)

	state := &ItemState{Signals: map[SignalId]SignalState{}}
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
