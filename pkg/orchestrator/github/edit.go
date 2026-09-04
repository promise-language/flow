package github

import (
	"context"
	"fmt"
	"slices"

	"github.com/promise-language/flow"
)

// Edit opens a staged edit on the issue. NOTHING CHANGES UNTIL Commit, and
// opening one is not a lock: another writer may land first, and Commit is where
// that is discovered.
func (b *Orchestrator) Edit(ctx context.Context, ref flow.ItemRef) (flow.ItemEditor, error) {
	issueNum, err := b.issueNumber(ref)
	if err != nil {
		return nil, err
	}
	issue, err := b.out.GetIssue(ctx, issueNum)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", issueNum, err)
	}
	return &editor{
		b:        b,
		ref:      ref,
		issueNum: issueNum,
		// The state as it was when the edit opened. Commit compares against a
		// fresh read: the caller staged changes against THIS, and if a field
		// they are changing has moved since, applying anyway would silently
		// overwrite whoever landed first.
		openedTitle:  issue.GetTitle(),
		openedBody:   issue.GetBody(),
		openedLabels: labelNamesOf(issue.Labels),
	}, nil
}

// editor is the transaction. Every setter stages; Commit applies all of it or
// none of it.
type editor struct {
	b        *Orchestrator
	ref      flow.ItemRef
	issueNum int

	openedTitle  string
	openedBody   string
	openedLabels []string

	title, body *string
	addTags     []flow.TagId
	delTags     []flow.TagId
	addBlockers []flow.ItemRef
	delBlockers []flow.ItemRef
	manual      *bool
}

func (e *editor) SetTitle(t string) { e.title = &t }
func (e *editor) SetBody(b string)  { e.body = &b }
func (e *editor) SetManual(m bool)  { e.manual = &m }

func (e *editor) AddTag(t flow.TagId)    { e.addTags = append(e.addTags, t) }
func (e *editor) RemoveTag(t flow.TagId) { e.delTags = append(e.delTags, t) }

func (e *editor) AddBlocker(ref flow.ItemRef)    { e.addBlockers = append(e.addBlockers, ref) }
func (e *editor) RemoveBlocker(ref flow.ItemRef) { e.delBlockers = append(e.delBlockers, ref) }

// Commit applies every staged change, OR NONE OF THEM.
//
// Title, body, tags and the manual flag are ONE PATCH: the endpoint takes
// title, body and labels together, so they land atomically and a caller never
// has to ask which half succeeded.
//
// Blockers go through a different endpoint, so a stage mixing them with the
// PATCH fields is REFUSED here rather than applied by halves — refusing is an
// answer, a partial success is not.
//
// Because `labels` REPLACES the set, Commit re-reads and applies the deltas to
// what is actually there. And because another writer may have landed in
// between, it refuses when a field it is changing has moved since Edit opened.
func (e *editor) Commit(ctx context.Context) error {
	touchesPatch := e.title != nil || e.body != nil || len(e.addTags) > 0 || len(e.delTags) > 0 || e.manual != nil
	touchesBlockers := len(e.addBlockers) > 0 || len(e.delBlockers) > 0
	if touchesPatch && touchesBlockers {
		return fmt.Errorf(
			"github: this edit stages both item fields and blockers, which land through different endpoints "+
				"and cannot be written together — split it into two edits: %w", flow.ErrUnsupported)
	}

	for _, t := range slices.Concat(e.addTags, e.delTags) {
		if !t.Valid() {
			return fmt.Errorf("github: %q is not a valid tag (non-empty, single-line, no edge whitespace)", string(t))
		}
	}
	for _, t := range e.delTags {
		// An orchestrator MUST refuse to remove a marker it maintains itself:
		// the owner, binary, seeded, park and manual markers follow from Claim,
		// seeding, Park and this editor, and a caller able to delete one
		// directly could make an item report a state no operation put it in.
		if e.b.labels.Maintained(string(t)) {
			return fmt.Errorf(
				"github: %q is a marker this orchestrator maintains itself and cannot be removed directly — "+
					"it follows from the operation that set it", string(t))
		}
		if string(t) == e.b.labels.Binary(e.b.cfg.BinaryName) {
			return fmt.Errorf("github: %q is the binary marker seeding maintains and cannot be removed directly", string(t))
		}
	}

	switch {
	case touchesBlockers:
		return e.commitBlockers(ctx)
	case touchesPatch:
		return e.commitFields(ctx)
	}
	return nil
}

// commitFields writes title, body, labels and the manual marker in one PATCH.
func (e *editor) commitFields(ctx context.Context) error {
	issue, err := e.b.out.GetIssue(ctx, e.issueNum)
	if err != nil {
		return fmt.Errorf("github: re-read issue %d before commit: %w", e.issueNum, err)
	}

	// A field this edit is changing that moved since Edit opened is a lost
	// update waiting to happen. Refuse rather than overwrite: the caller staged
	// their change against what they read.
	if e.title != nil && issue.GetTitle() != e.openedTitle {
		return fmt.Errorf("github: the title of issue #%d changed since this edit opened — re-read and re-stage: %w",
			e.issueNum, flow.ErrUnavailable)
	}
	if e.body != nil && issue.GetBody() != e.openedBody {
		return fmt.Errorf("github: the body of issue #%d changed since this edit opened — re-read and re-stage: %w",
			e.issueNum, flow.ErrUnavailable)
	}

	current := labelNamesOf(issue.Labels)
	var labelsOut []string
	if len(e.addTags) > 0 || len(e.delTags) > 0 || e.manual != nil {
		// A label this edit touches that moved since Edit opened is the same
		// lost update. Labels the edit does NOT touch may move freely — that is
		// exactly what applying deltas to the current set is for.
		for _, t := range slices.Concat(e.addTags, e.delTags) {
			if slices.Contains(current, string(t)) != slices.Contains(e.openedLabels, string(t)) {
				return fmt.Errorf("github: label %q on issue #%d changed since this edit opened — re-read and re-stage: %w",
					string(t), e.issueNum, flow.ErrUnavailable)
			}
		}
		labelsOut = slices.Clone(current)
		for _, t := range e.addTags {
			if !slices.Contains(labelsOut, string(t)) {
				labelsOut = append(labelsOut, string(t))
			}
		}
		for _, t := range e.delTags {
			labelsOut = slices.DeleteFunc(labelsOut, func(x string) bool { return x == string(t) })
		}
		if e.manual != nil {
			marker := e.b.labels.Manual()
			labelsOut = slices.DeleteFunc(labelsOut, func(x string) bool { return x == marker })
			if *e.manual {
				labelsOut = append(labelsOut, marker)
			}
		}
	}

	var title, body *flow.Text
	if e.title != nil {
		// Origin operator: an edit is what a person asked for, and it is the
		// party proposing it that states who stands behind the bytes.
		title = &flow.Text{Origin: flow.OriginOperator, Body: *e.title}
	}
	if e.body != nil {
		body = &flow.Text{Origin: flow.OriginOperator, Body: *e.body}
	}
	if err := e.b.out.EditIssue(ctx, e.issueNum, title, body, labelsOut); err != nil {
		return err
	}

	// Setting manual RESOLVES ANY UNRESOLVED PARK — the operator's `run-step`
	// IS the resume — and clearing it returns the item to automatic dispatch.
	// This follows the PATCH: the marker is the record, and a park cleared
	// against a marker that failed to land would leave the item dispatchable
	// with nobody driving it.
	if e.manual != nil && *e.manual {
		if err := e.b.clearPark(ctx, e.ref); err != nil {
			return err
		}
	}
	return nil
}

// clearPark drops the item's park record and the label that advertises it.
// An item with no state comment has no park to clear, which is not an error.
func (b *Orchestrator) clearPark(ctx context.Context, ref flow.ItemRef) error {
	var cleared string
	err := b.mutateStateDoc(ctx, ref, "clearPark", func(doc *stateDoc) error {
		cleared = parkLabel(b.labels, parkRequestFromDoc(doc.Park))
		doc.Park = nil
		return nil
	})
	if err != nil {
		if isNoStateComment(err) {
			return nil
		}
		return err
	}
	b.removeParkLabel(ctx, ref, cleared)
	return nil
}

// commitBlockers applies the staged dependency changes.
//
// Every blocker is RESOLVED first, and one that names nothing is refused: an
// identifier naming nothing is a typo, and accepting it blocks the item forever
// on something that does not exist. Self-reference and cycles are refused with
// it — a ring leaves every item in it blocked, each one correctly, with no
// blocker ever finishing, and nothing reports it because every individual item
// is in a defensible state.
func (e *editor) commitBlockers(ctx context.Context) error {
	type target struct {
		num int
		id  int64
	}
	adds := make([]target, 0, len(e.addBlockers))
	for _, ref := range e.addBlockers {
		num, err := e.b.issueNumber(ref)
		if err != nil {
			return fmt.Errorf("github: blocker ref: %w", err)
		}
		if num == e.issueNum {
			return fmt.Errorf("github: issue #%d named as its own blocker — self-reference is a cycle of length one", e.issueNum)
		}
		iss, err := e.b.out.GetIssue(ctx, num)
		if err != nil {
			return fmt.Errorf("github: blocker #%d cannot be resolved, so it will never finish: %w", num, err)
		}
		adds = append(adds, target{num: num, id: iss.GetID()})
	}
	// Detection costs a traversal, and it is a traversal already being made:
	// deriving blockedness reads each blocker's status anyway.
	for _, a := range adds {
		if reaches, err := e.b.reaches(ctx, a.num, e.issueNum, map[int]bool{}); err != nil {
			return err
		} else if reaches {
			return fmt.Errorf(
				"github: blocker #%d closes a cycle back to #%d — every item in the ring would be blocked and none would ever clear",
				a.num, e.issueNum)
		}
	}

	dels := make([]target, 0, len(e.delBlockers))
	for _, ref := range e.delBlockers {
		num, err := e.b.issueNumber(ref)
		if err != nil {
			return fmt.Errorf("github: blocker ref: %w", err)
		}
		iss, err := e.b.out.GetIssue(ctx, num)
		if err != nil {
			// Retracting a blocker that cannot be read is not an error worth
			// refusing an edit over: removing one that is not recorded changes
			// nothing.
			continue
		}
		dels = append(dels, target{num: num, id: iss.GetID()})
	}

	for _, a := range adds {
		if err := e.b.out.AddBlockedBy(ctx, e.issueNum, a.id); err != nil {
			return fmt.Errorf("github: record #%d as a blocker of #%d: %w", a.num, e.issueNum, err)
		}
	}
	for _, d := range dels {
		if err := e.b.out.RemoveBlockedBy(ctx, e.issueNum, d.id); err != nil {
			return fmt.Errorf("github: retract #%d as a blocker of #%d: %w", d.num, e.issueNum, err)
		}
	}
	return nil
}

// reaches reports whether `from` can reach `to` by following blocker edges.
// Bounded by `seen`, so a ring already present in the store does not hang the
// traversal that is trying to refuse a new one.
func (b *Orchestrator) reaches(ctx context.Context, from, to int, seen map[int]bool) (bool, error) {
	if from == to {
		return true, nil
	}
	if seen[from] {
		return false, nil
	}
	seen[from] = true
	deps, err := b.out.ListBlockedBy(ctx, from)
	if err != nil {
		return false, fmt.Errorf("github: walk blockers of #%d: %w", from, err)
	}
	for _, d := range deps {
		ok, err := b.reaches(ctx, d.GetNumber(), to, seen)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

var _ flow.ItemEditor = (*editor)(nil)
