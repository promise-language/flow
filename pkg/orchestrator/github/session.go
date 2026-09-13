package github

import (
	"context"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// This orchestrator's agent-session store is the worktree-local `.flow/session`
// tree, the same per-clone place its claim state and its drafts live. See the
// Orchestrator agent-session methods for the contract, and clistate for the
// files.
//
// Nothing here touches `outward`, which is the only route this package has to
// GitHub. That structural fact IS the "never published" guarantee the contract
// asks for: the handle names a conversation holding the resolution's whole
// reasoning, and it never leaves the machine that opened it.
//
// Records are keyed by the issue number ALONE — the session belongs to the
// resolution, not to a step — through workItemKey, the same item key the draft
// records use rather than a second spelling of it. Release already clears the
// claim state through clistate.Clear, which removes the session tree with the
// draft tree, so a released claim leaves no conversation behind — and Finalize
// releases through the same path.

// SaveAgentSession stores the handle this resolution is holding, and the
// boundary already honoured, against this issue.
func (b *Orchestrator) SaveAgentSession(ctx context.Context, ref flow.ItemRef, s flow.AgentSession) error {
	item, err := b.workItemKey(ref)
	if err != nil {
		return err
	}
	return clistate.SaveSession(item, s.SessionID, string(s.Boundary))
}

// LoadAgentSession returns what this issue's resolution is holding, or the zero
// value when there is none — including when a record on disk names another
// issue.
func (b *Orchestrator) LoadAgentSession(ctx context.Context, ref flow.ItemRef) (flow.AgentSession, error) {
	item, err := b.workItemKey(ref)
	if err != nil {
		return flow.AgentSession{}, err
	}
	id, boundary, err := clistate.LoadSession(item)
	if err != nil {
		return flow.AgentSession{}, err
	}
	return flow.AgentSession{SessionID: id, Boundary: flow.StepId(boundary)}, nil
}

// ClearAgentSession drops this issue's record. Idempotent.
func (b *Orchestrator) ClearAgentSession(ctx context.Context, ref flow.ItemRef) error {
	item, err := b.workItemKey(ref)
	if err != nil {
		return err
	}
	return clistate.ClearSession(item)
}
