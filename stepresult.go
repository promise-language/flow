package flow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// StepResult is how a handler completes: the route it elects, the message that
// travels with the election, an optional standing note, and — on an artifact
// step — the payload to capture (docs/step-handler.md § Electing the route).
//
// A handler does not write anything mid-run. It returns this, and the SDK
// captures the result and records the route together: "result and route land
// together or not at all" (docs/resolution.md § The journal).
//
// The fields are exported because the orchestrator reads them at capture time;
// they are WRITTEN through ctx.Next / ctx.Finalize and the extenders below, so
// a handler never has to know which field an election lives in.
type StepResult struct {
	// Route is the election: a declared successor, or a permitted finalization
	// with its disposition. Exactly one of the two (journal.go § Route).
	Route Route
	// Message is why the successor is being run — what was found, what is
	// expected of it, what changed — or, on a finalizing election, the closing
	// reasons. Never empty: "an election with an empty message hands the
	// successor a task with no brief, and the journal a decision with no
	// reason" (docs/resolution.md § Routing).
	Message string
	// Note is an optional standing note, addressed to every subsequent step
	// rather than only the next one. It informs; it never binds.
	Note string
	// Payload is the artifact value an artifact step produces, and nil on a
	// step that produces none.
	//
	// A POINTER, and that is the whole reason this field is not a plain
	// ArtifactBody: an EMPTY PatchBody is a legal payload that must reach the
	// orchestrator — a backend whose patches live server-side attaches the diff
	// out of band, and a handler whose work is already committed has an empty
	// `git diff HEAD` by definition. "No payload" and "empty payload" are
	// different facts, and a value type could not tell them apart.
	Payload *ArtifactBody
}

// WithNote returns the result carrying a standing note. Value receiver, so it
// composes in either order with the payload extenders and neither can be
// mistaken for a mutation of the result the handler already built.
func (r StepResult) WithNote(note string) StepResult {
	r.Note = note
	return r
}

// The six payload extenders, one per ArtifactType. Each returns the extended
// result, so a handler tail reads as one expression:
//
//	return ctx.Next(StepImplement, "the plan is written").Markdown(plan), nil
//
// A payload whose type is not the step's declared one is refused at capture
// (Elect → ErrTypeMismatch); nothing is journaled and nothing is published.

func (r StepResult) Flag() StepResult {
	return r.withPayload(ArtifactBody{Type: ArtifactFlag})
}

func (r StepResult) CommitHash(sha string) StepResult {
	return r.withPayload(ArtifactBody{Type: ArtifactCommitHash, CommitHash: sha})
}

func (r StepResult) Markdown(body string) StepResult {
	return r.withPayload(ArtifactBody{Type: ArtifactMarkdown, Markdown: body})
}

func (r StepResult) JSON(body json.RawMessage) StepResult {
	return r.withPayload(ArtifactBody{Type: ArtifactJSON, JSON: body})
}

func (r StepResult) File(name string, content []byte) StepResult {
	return r.withPayload(ArtifactBody{Type: ArtifactFile, File: FileBody{Name: name, Content: content}})
}

func (r StepResult) Patch(body PatchBody) StepResult {
	return r.withPayload(ArtifactBody{Type: ArtifactPatch, Patch: body})
}

func (r StepResult) withPayload(b ArtifactBody) StepResult {
	r.Payload = &b
	return r
}

// Elect checks one election against the step that made it and returns the body
// to capture. It is the single place an election is checked — the SDK's whole
// answer to "may this step complete this way", so a route, a message and a
// payload cannot be judged by three different rules.
//
// `declared` is the artifact type the step's result id is declared with; it is
// meaningless on a signal step or a wait, which own no artifact.
//
// The order is the order a reader wants the refusals in: did the step decide at
// all, is the decision one of the two shapes, does it carry its brief, is it
// within the declaration, and only then whether the payload fits.
//
// Nothing here writes. A refused election journals nothing and publishes
// nothing — the caller reports it, and the step is dispatched again or the item
// parks.
func (r StepResult) Elect(li LifecycleItem, declared ArtifactType) (ArtifactBody, error) {
	notComplete := ErrStepDidNotComplete{Step: li.Description, Result: string(li.Result())}
	// A zero result is a handler that returned without deciding anything: no
	// route, no message, no payload. It is not a malformed election but the
	// absence of one, so it reads as the step not having done its job.
	if r.Route == (Route{}) {
		return ArtifactBody{}, notComplete
	}
	if err := r.Route.Valid(); err != nil {
		return ArtifactBody{}, fmt.Errorf("step %q: %w", li.Description, err)
	}
	if strings.TrimSpace(r.Message) == "" {
		return ArtifactBody{}, fmt.Errorf(
			"step %q elected %s with an empty message: every election carries its message — "+
				"the successor is told why it is being run, and the journal why the decision was made",
			li.Description, describeRoute(r.Route))
	}
	if !r.declared(li) {
		return ArtifactBody{}, ErrRouteNotDeclared{
			Step:        li.Description,
			Elected:     r.Route,
			Next:        li.Next,
			MayFinalize: li.MayFinalize,
		}
	}
	// Signals are never handler-writable, so a signal step or a wait carrying a
	// payload has produced something it cannot own.
	if li.Kind != LifecycleArtifact {
		if r.Payload != nil {
			return ArtifactBody{}, ErrSignalNotWritable{Step: li.Description, Signal: li.SignalId}
		}
		return ArtifactBody{}, nil
	}
	// An artifact step owes exactly one result. A completion without one is the
	// same failure as a completion without a route: the step said it was done
	// and produced nothing.
	if r.Payload == nil {
		return ArtifactBody{}, notComplete
	}
	if r.Payload.Type != declared {
		return ArtifactBody{}, ErrTypeMismatch{
			Step:     li.Description,
			Expected: declared,
			Got:      r.Payload.Type,
		}
	}
	return *r.Payload, nil
}

// declared reports whether the election is one the step wrote down.
func (r StepResult) declared(li LifecycleItem) bool {
	if r.Route.Finalizes() {
		return slices.Contains(li.MayFinalize, r.Route.Finalize)
	}
	return slices.Contains(li.Next, r.Route.Next)
}

// describeRoute names an election for a message, in the terms it was made in.
func describeRoute(rt Route) string {
	if rt.Finalizes() {
		return fmt.Sprintf("finalization %q", rt.Finalize)
	}
	return fmt.Sprintf("successor %q", rt.Next)
}
