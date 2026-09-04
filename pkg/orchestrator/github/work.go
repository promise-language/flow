package github

import (
	"context"
	"strconv"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// This orchestrator's work-in-progress store is the worktree-local `.flow/work`
// tree, the same per-clone place its claim state lives. See
// the Orchestrator work-in-progress methods for the contract, and clistate for
// the files.
//
// Nothing here touches `outward`, which is the only route this package has to
// GitHub. That structural fact IS the "never published" guarantee the contract
// asks for — and it is what makes the store able to hold the one text that has
// nowhere else to go: prose a disclosure guard refused.
//
// Records are keyed by the issue number and the StepId. Release already clears
// the claim state through clistate.Clear, which removes the work tree with it,
// so a released claim leaves no reasoning behind — and Finalize releases
// through the same path.

// workItemKey is the item half of a record's key: the issue number, which is
// this orchestrator's item identity everywhere else too.
func (b *Orchestrator) workItemKey(ref flow.ItemRef) (string, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(issueNum), nil
}

// SaveWorkInProgress stores what a step worked out, against that step's result
// id on this issue.
func (b *Orchestrator) SaveWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId, body string) error {
	item, err := b.workItemKey(ref)
	if err != nil {
		return err
	}
	return clistate.SaveWork(item, string(step), body)
}

// LoadWorkInProgress returns what this step stashed against this issue, or ""
// when there is none — including when a record on disk names another issue or
// another step.
func (b *Orchestrator) LoadWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId) (string, error) {
	item, err := b.workItemKey(ref)
	if err != nil {
		return "", err
	}
	return clistate.LoadWork(item, string(step))
}

// ClearWorkInProgress drops the record for this step. Idempotent.
func (b *Orchestrator) ClearWorkInProgress(ctx context.Context, ref flow.ItemRef, step flow.StepId) error {
	item, err := b.workItemKey(ref)
	if err != nil {
		return err
	}
	return clistate.ClearWork(item, string(step))
}
