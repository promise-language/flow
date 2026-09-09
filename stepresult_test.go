package flow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// artifactStep is a lifecycle item shaped like a registered artifact step: one
// declared successor, one permitted disposition. Every refusal below is a
// refusal against THIS declaration, which is the whole point of Elect — an
// election is only legal in terms of the step that made it.
func artifactStep() LifecycleItem {
	return LifecycleItem{
		Description: "write plan",
		Kind:        LifecycleArtifact,
		ArtifactId:  "plan",
		Next:        []StepId{"impl"},
		MayFinalize: []Disposition{DispositionResolved},
	}
}

func signalStep() LifecycleItem {
	return LifecycleItem{
		Description: "create pull request",
		Kind:        LifecycleSignal,
		SignalId:    "pr-open",
		Next:        []StepId{"impl"},
	}
}

func waitStep() LifecycleItem {
	return LifecycleItem{
		Description: "await merge",
		Kind:        LifecycleAwait,
		SignalId:    "pr-merged",
		Next:        []StepId{"impl"},
	}
}

// elect is the shape a handler's tail produces, without a StepCtx: the
// constructors on StepCtx build exactly this.
func elect(next StepId, message string) StepResult {
	return StepResult{Route: Route{Next: next}, Message: message}
}

func electFinal(d Disposition, message string) StepResult {
	return StepResult{Route: Route{Finalize: d}, Message: message}
}

// The zero result is a handler that decided nothing. It is not a malformed
// election but the absence of one, so it reads as the step not doing its job —
// which is what makes it a park rather than a failure at the call site.
func TestElect_ZeroResultDidNotComplete(t *testing.T) {
	var incomplete ErrStepDidNotComplete
	_, err := StepResult{}.Elect(artifactStep(), ArtifactMarkdown)
	if !errors.As(err, &incomplete) {
		t.Fatalf("err = %v, want ErrStepDidNotComplete", err)
	}
	if incomplete.Step != "write plan" || incomplete.Result != "plan" {
		t.Errorf("err names %q/%q, want the description and the result id", incomplete.Step, incomplete.Result)
	}
	if !strings.Contains(err.Error(), "completed without producing") {
		t.Errorf("message = %q, want it to say the step produced nothing", err.Error())
	}
}

// A route is one of two shapes. Both halves set is not an election with extra
// information — it is two elections, and Route.Valid is the one place that is
// decided.
func TestElect_BothRouteHalvesRefused(t *testing.T) {
	res := StepResult{Route: Route{Next: "impl", Finalize: DispositionResolved}, Message: "why"}
	_, err := res.Elect(artifactStep(), ArtifactMarkdown)
	if err == nil || !strings.Contains(err.Error(), "one or the other") {
		t.Fatalf("err = %v, want the both-halves refusal", err)
	}
}

// Neither half is the zero route, so it refuses as "did not complete" — the
// same answer, because a handler that set neither decided nothing.
func TestElect_NeitherRouteHalfIsIncompletion(t *testing.T) {
	var incomplete ErrStepDidNotComplete
	res := StepResult{Message: "why"}
	if _, err := res.Elect(artifactStep(), ArtifactMarkdown); !errors.As(err, &incomplete) {
		t.Fatalf("err = %v, want ErrStepDidNotComplete", err)
	}
}

func TestElect_UndeclaredDispositionRefused(t *testing.T) {
	res := electFinal("nonsense", "why").Markdown("body")
	_, err := res.Elect(artifactStep(), ArtifactMarkdown)
	if err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("err = %v, want the disposition-vocabulary refusal", err)
	}
}

// docs/resolution.md § Routing: "Every election carries its message." An
// election with none hands the successor a task with no brief.
func TestElect_EmptyMessageRefused(t *testing.T) {
	for name, res := range map[string]StepResult{
		"successor":    elect("impl", "").Markdown("body"),
		"finalization": electFinal(DispositionResolved, "   ").Markdown("body"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := res.Elect(artifactStep(), ArtifactMarkdown)
			if err == nil || !strings.Contains(err.Error(), "empty message") {
				t.Fatalf("err = %v, want the empty-message refusal", err)
			}
		})
	}
}

// A handler cannot elect a route the step does not declare, and the refusal
// names both sides: what was elected, and what could have been.
func TestElect_UndeclaredSuccessor(t *testing.T) {
	var notDeclared ErrRouteNotDeclared
	res := elect("review", "carry on").Markdown("body")
	_, err := res.Elect(artifactStep(), ArtifactMarkdown)
	if !errors.As(err, &notDeclared) {
		t.Fatalf("err = %v, want ErrRouteNotDeclared", err)
	}
	if notDeclared.Elected.Next != "review" {
		t.Errorf("err names elected %q, want review", notDeclared.Elected.Next)
	}
	if len(notDeclared.Next) != 1 || notDeclared.Next[0] != "impl" {
		t.Errorf("err names declared %v, want [impl]", notDeclared.Next)
	}
	if !strings.Contains(err.Error(), "review") || !strings.Contains(err.Error(), "impl") {
		t.Errorf("message = %q, want both what was elected and what was declared", err.Error())
	}
}

func TestElect_UndeclaredFinalization(t *testing.T) {
	var notDeclared ErrRouteNotDeclared
	li := artifactStep()
	li.MayFinalize = nil
	res := electFinal(DispositionResolved, "done").Markdown("body")
	_, err := res.Elect(li, ArtifactMarkdown)
	if !errors.As(err, &notDeclared) {
		t.Fatalf("err = %v, want ErrRouteNotDeclared", err)
	}
	if notDeclared.Elected.Finalize != DispositionResolved {
		t.Errorf("err names elected %q, want resolved", notDeclared.Elected.Finalize)
	}
	if !strings.Contains(err.Error(), "does not declare") {
		t.Errorf("message = %q, want it to say the step does not declare it", err.Error())
	}
}

// Signals are never handler-writable, so a payload on a signal step or a wait
// is a result the step cannot own.
func TestElect_PayloadOnSignalStepRefused(t *testing.T) {
	for name, li := range map[string]LifecycleItem{
		"signal step": signalStep(),
		"signal wait": waitStep(),
	} {
		t.Run(name, func(t *testing.T) {
			var notWritable ErrSignalNotWritable
			res := elect("impl", "the request is open").Markdown("body")
			if _, err := res.Elect(li, ArtifactMarkdown); !errors.As(err, &notWritable) {
				t.Fatalf("err = %v, want ErrSignalNotWritable", err)
			}
		})
	}
}

// A signal step's election carries no payload, and that is a complete
// election: the signal is the orchestrator's observation.
func TestElect_SignalStepWithoutPayloadIsComplete(t *testing.T) {
	body, err := elect("impl", "the request is open").Elect(signalStep(), 0)
	if err != nil {
		t.Fatalf("Elect: %v", err)
	}
	if body.Type != 0 || body.Markdown != "" {
		t.Errorf("body = %+v, want the zero body — a signal step produces none", body)
	}
}

// An artifact step owes exactly one result, so an election without one is the
// same incompletion as an election with no route.
func TestElect_ArtifactStepWithoutPayload(t *testing.T) {
	var incomplete ErrStepDidNotComplete
	res := elect("impl", "carry on")
	if _, err := res.Elect(artifactStep(), ArtifactMarkdown); !errors.As(err, &incomplete) {
		t.Fatalf("err = %v, want ErrStepDidNotComplete", err)
	}
}

// Each payload against a type that is not the declared one.
func TestElect_PayloadTypeMismatch(t *testing.T) {
	base := elect("impl", "carry on")
	cases := map[string]struct {
		res      StepResult
		declared ArtifactType
		got      ArtifactType
	}{
		"flag on markdown":        {base.Flag(), ArtifactMarkdown, ArtifactFlag},
		"commit hash on markdown": {base.CommitHash("deadbeef"), ArtifactMarkdown, ArtifactCommitHash},
		"markdown on flag":        {base.Markdown("body"), ArtifactFlag, ArtifactMarkdown},
		"json on markdown":        {base.JSON(json.RawMessage(`{}`)), ArtifactMarkdown, ArtifactJSON},
		"file on patch":           {base.File("n", []byte("c")), ArtifactPatch, ArtifactFile},
		"patch on commit hash":    {base.Patch(PatchBody{}), ArtifactCommitHash, ArtifactPatch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var mismatch ErrTypeMismatch
			_, err := tc.res.Elect(artifactStep(), tc.declared)
			if !errors.As(err, &mismatch) {
				t.Fatalf("err = %v, want ErrTypeMismatch", err)
			}
			if mismatch.Expected != tc.declared || mismatch.Got != tc.got {
				t.Errorf("err = %+v, want expected=%v got=%v", mismatch, tc.declared, tc.got)
			}
		})
	}
}

// An EMPTY PatchBody is a legal payload and must reach the orchestrator: a
// backend whose patches live server-side attaches the diff out of band, and a
// handler whose work is already committed has an empty diff by definition.
// "No payload" and "empty payload" are different facts.
func TestElect_EmptyPatchBodyIsAPayload(t *testing.T) {
	body, err := elect("impl", "the change is committed").Patch(PatchBody{}).
		Elect(artifactStep(), ArtifactPatch)
	if err != nil {
		t.Fatalf("Elect: %v", err)
	}
	if body.Type != ArtifactPatch {
		t.Fatalf("body = %+v, want a patch", body)
	}
	if len(body.Patch.Diff) != 0 {
		t.Errorf("diff = %q, want the empty body carried through unchanged", body.Patch.Diff)
	}
}

// The happy case, once per payload kind: the declared type, carried through.
func TestElect_EveryPayloadKindCapturesItsBody(t *testing.T) {
	base := elect("impl", "carry on")
	cases := map[string]struct {
		res      StepResult
		declared ArtifactType
		check    func(*testing.T, ArtifactBody)
	}{
		"flag": {base.Flag(), ArtifactFlag, func(t *testing.T, b ArtifactBody) {}},
		"commit hash": {base.CommitHash("deadbeef"), ArtifactCommitHash, func(t *testing.T, b ArtifactBody) {
			if b.CommitHash != "deadbeef" {
				t.Errorf("commit = %q", b.CommitHash)
			}
		}},
		"markdown": {base.Markdown("the plan"), ArtifactMarkdown, func(t *testing.T, b ArtifactBody) {
			if b.Markdown != "the plan" {
				t.Errorf("markdown = %q", b.Markdown)
			}
		}},
		"json": {base.JSON(json.RawMessage(`{"a":1}`)), ArtifactJSON, func(t *testing.T, b ArtifactBody) {
			if string(b.JSON) != `{"a":1}` {
				t.Errorf("json = %q", b.JSON)
			}
		}},
		"file": {base.File("report.txt", []byte("bytes")), ArtifactFile, func(t *testing.T, b ArtifactBody) {
			if b.File.Name != "report.txt" || string(b.File.Content) != "bytes" {
				t.Errorf("file = %+v", b.File)
			}
		}},
		"patch": {base.Patch(PatchBody{Diff: []byte("diff")}), ArtifactPatch, func(t *testing.T, b ArtifactBody) {
			if string(b.Patch.Diff) != "diff" {
				t.Errorf("patch = %+v", b.Patch)
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body, err := tc.res.Elect(artifactStep(), tc.declared)
			if err != nil {
				t.Fatalf("Elect: %v", err)
			}
			if body.Type != tc.declared {
				t.Fatalf("body type = %v, want %v", body.Type, tc.declared)
			}
			tc.check(t, body)
		})
	}
}

// A finalizing election is checked against MayFinalize and captures its payload
// like any other: the item ends, and the step still owes its result.
func TestElect_DeclaredFinalizationCaptures(t *testing.T) {
	body, err := electFinal(DispositionResolved, "the change is merged").Markdown("the plan").
		Elect(artifactStep(), ArtifactMarkdown)
	if err != nil {
		t.Fatalf("Elect: %v", err)
	}
	if body.Markdown != "the plan" {
		t.Errorf("body = %+v, want the payload captured", body)
	}
}

// The extenders are value receivers, so they compose in either order and
// neither loses what the other set.
func TestStepResult_ExtendersComposeInEitherOrder(t *testing.T) {
	first := elect("impl", "carry on").WithNote("watch the parser").Markdown("the plan")
	second := elect("impl", "carry on").Markdown("the plan").WithNote("watch the parser")
	for name, res := range map[string]StepResult{"note first": first, "payload first": second} {
		t.Run(name, func(t *testing.T) {
			if res.Note != "watch the parser" {
				t.Errorf("note = %q", res.Note)
			}
			if res.Payload == nil || res.Payload.Markdown != "the plan" {
				t.Errorf("payload = %+v", res.Payload)
			}
			if res.Route.Next != "impl" || res.Message != "carry on" {
				t.Errorf("route/message = %+v / %q", res.Route, res.Message)
			}
		})
	}
}

// An extender returns a new value rather than editing the one it was called
// on: a handler that built a result and then extended a copy must not find its
// original changed underneath it.
func TestStepResult_ExtendersDoNotMutateTheOriginal(t *testing.T) {
	base := elect("impl", "carry on")
	_ = base.WithNote("a note").Markdown("the plan")
	if base.Note != "" || base.Payload != nil {
		t.Errorf("base = %+v, want it unchanged by the extended copy", base)
	}
}
