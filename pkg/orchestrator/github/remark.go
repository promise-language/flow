package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

// Remark records one piece of prose on the issue.
//
// AN ORDINARY, UNMARKED COMMENT — the same shape PostAnswer posts. The
// machine-readable writes on an issue all carry an HTML marker
// (`<!-- flow:… -->`) and the readers select on it, so a comment without one is
// invisible to them and cannot be mistaken for state. That is also what makes
// a remark readable: it appears in the thread exactly where a person writing
// the same sentence by hand would have put it.
//
// No claim — the party recording a remark is not the party holding the item.
//
// The origin is OriginOperator: the CLI is the only caller, and a person typed
// it. A step-authored remark would carry OriginAgent, and the origin would have
// to become a parameter — the guard decides differently about the two, and
// nothing here may answer that question on a caller's behalf.
func (b *Orchestrator) Remark(ctx context.Context, ref flow.ItemRef, text string) error {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return err
	}
	// Refused before publishing: an empty comment is a disclosure that cannot
	// be taken back, and it says nothing when it arrives.
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("github: a remark on issue #%d must carry text", issueNum)
	}
	if _, err := b.out.CreateComment(ctx, flow.ActRemark, issueNum,
		flow.Text{Origin: flow.OriginOperator, Body: text}); err != nil {
		return fmt.Errorf("post remark comment: %w", err)
	}
	return nil
}
