package github

import (
	"context"
	"strconv"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/clistate"
)

// This backend's work-in-progress store is the worktree-local `.flow/work`
// tree, the same per-clone place its claim state lives. See
// flow.WorkInProgress for the contract and clistate for the files.
//
// Nothing here touches `outward`, which is the only route this package has to
// GitHub. That structural fact IS the "never published" guarantee the contract
// asks for — and it is what makes the store able to hold the one text that has
// nowhere else to go: prose a disclosure guard refused.
//
// Records are keyed by the issue number and the step's result id. Release
// already clears the claim state through clistate.Clear, which removes the work
// tree with it, so a released claim leaves no reasoning behind. This backend
// implements flow.Finalizer (see claim.go), which releases through the same
// path.

// workItemKey is the item half of a record's key: the issue number, which is
// this backend's item identity everywhere else too.
func (b *Backend) workItemKey(ref flow.ItemRef) (string, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(issueNum), nil
}

// SaveWorkInProgress stores what a step worked out, against that step's result
// id on this issue.
func (b *Backend) SaveWorkInProgress(ctx context.Context, claim flow.Claim, step, body string) error {
	item, err := b.workItemKey(claim.ItemRef)
	if err != nil {
		return err
	}
	return clistate.SaveWork(item, step, body)
}

// LoadWorkInProgress returns what this step stashed against this issue, or ""
// when there is none — including when a record on disk names another issue or
// another step.
func (b *Backend) LoadWorkInProgress(ctx context.Context, claim flow.Claim, step string) (string, error) {
	item, err := b.workItemKey(claim.ItemRef)
	if err != nil {
		return "", err
	}
	return clistate.LoadWork(item, step)
}

// ClearWorkInProgress drops the record for this step. Idempotent.
func (b *Backend) ClearWorkInProgress(ctx context.Context, claim flow.Claim, step string) error {
	item, err := b.workItemKey(claim.ItemRef)
	if err != nil {
		return err
	}
	return clistate.ClearWork(item, step)
}
