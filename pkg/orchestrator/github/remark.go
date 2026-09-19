package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/promise-language/flow"
)

// remarkCommentMarker opens every remark comment. It shares flowMarkerPrefix,
// which is the whole of its job: isFlowMachineComment matches on that prefix,
// so a marker added here needs no reader to be taught about it.
const remarkCommentMarker = "<!-- flow:remark -->"

// Remark records one piece of prose on the issue: an ordinary comment carrying
// the remark marker, and the text below it.
//
// THE MARKER IS WHAT KEEPS A REMARK FROM BEING READ AS AN ANSWER. The thread is
// the answer store — ReadAnswers takes every comment posted after a question's
// `asked-at` that carries no flow marker — so it is the one reader that selects
// on a marker's ABSENCE, and an unmarked remark recorded while a step waits for
// a human would clear that wait and resume the step on prose nobody offered as
// a reply. That is the failure isFlowMachineComment's own docstring records
// from the other side, reached by a different route.
//
// It costs the remark nothing a reader would notice: an HTML comment does not
// render, so it still appears in the thread exactly where a person writing the
// same sentence by hand would have put it. And the marker makes it no more
// readable than it was — nothing selects on `flow:remark`, and there is no
// remark store, no index and no id.
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
	// The guard sees the bytes that are posted, marker included — what is
	// examined is what is published (docs/disclosure.md § The rule).
	if _, err := b.out.CreateComment(ctx, flow.ActRemark, issueNum,
		flow.Text{Origin: flow.OriginOperator, Body: remarkCommentMarker + "\n" + text}); err != nil {
		return fmt.Errorf("post remark comment: %w", err)
	}
	return nil
}
