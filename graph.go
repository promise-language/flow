package flow

import "fmt"

// ValidateGraph checks a flow's declared routes whole, which is the only level
// at which most of them are checkable: a single registration cannot know
// whether its successors exist, whether anything reaches it, or whether the
// flow can ever end from it. Registration refuses what one declaration can be
// wrong about on its own (see Flow.prepareStep); this refuses the rest, once
// every declaration is in and before any item is claimed —
// docs/flow-registration.md § Startup validation.
//
// The checks, in the order a reader wants them:
//
//  1. Exactly one entry is declared. A second is already refused at
//     registration, so what this catches is zero — unknowable until now.
//  2. Every id in every Next names a registered lifecycle item.
//  3. Every signal wait declares exactly one successor. A wait elects nothing,
//     so its route is static and there is exactly one of it.
//  4. Every item is reachable from the entry. An unroutable step is a step
//     that will never run, declared as though it will.
//  5. Finalization is reachable from every item — a step from which no
//     sequence of declared routes could ever end the flow is a dead end, and
//     an item that reaches one can never finish.
//  6. The roles line up: every declared role names at least one capability,
//     every step carries a role, and every tag names a declared role.
//
// Every error names the flow, the step's description and its result id: a
// description alone is display text, and an id alone is not what the reader
// wrote down.
func (f *Flow) ValidateGraph() error {
	if f.entry == nil {
		return fmt.Errorf("flow %q declares no entry step: exactly one lifecycle item must be registered with StepConfig{Entry: true}", f.name)
	}
	for _, s := range f.steps {
		for _, id := range s.next {
			if _, ok := f.stepByResult[id]; !ok {
				return fmt.Errorf("flow %q: step %s declares successor %q, which names no registered lifecycle item",
					f.name, s.describe(), id)
			}
		}
	}
	for _, s := range f.steps {
		if s.kind != stepAwait {
			continue
		}
		if len(s.next) != 1 {
			return fmt.Errorf("flow %q: signal wait %s declares %d successors, want exactly one — a wait elects nothing, so its route is static",
				f.name, s.describe(), len(s.next))
		}
	}
	if err := f.validateReachableFromEntry(); err != nil {
		return err
	}
	if err := f.validateFinalizationReachable(); err != nil {
		return err
	}
	return f.validateRoles()
}

// validateRoles checks the role declarations and the step tags against each
// other, which is the level at which either is checkable: a declaration cannot
// know whether anything is tagged with it, and a tag cannot know whether the
// declaration it names arrived.
//
// Three refusals, in the order a reader wants them — first the declarations
// themselves, then the steps against them:
//
//   - A declared role naming NO capability is refused. Every account covers it
//     (AssumableRoles: a role requiring nothing is required of nothing), so the
//     steps it tags are performable by a read-only account — a role boundary
//     written down and enforcing nothing, which is worse than no boundary
//     because the declaration says otherwise. docs/flow-registration.md § Startup
//     validation puts the check here rather than at Flow.Role: only now is it
//     knowable that no later call added the capabilities.
//   - A step carrying NO role is refused. StepConfig.Role is "required on
//     steps", and an untagged step is nobody's move: no runner's assumable set
//     matches it, so the item awaits a role that does not exist.
//   - A tag naming NO declaration is refused as ErrUnknownRole, carrying the
//     declared set. This is the check the declaration surface exists for: the
//     vocabulary is open, so a typo is indistinguishable from a role whose
//     runner has not arrived, and the item sits unofferable forever with
//     nothing naming why (docs/resolution.md § Whose move it is).
//
// Signal waits are exempt from the tag checks. A wait belongs to no role — it
// has no handler and performs nothing — and AwaitSignal already panics on a
// role given to one, so the exemption here is the same rule read from the other
// side.
func (f *Flow) validateRoles() error {
	for _, r := range f.roles {
		if len(r.Capabilities) == 0 {
			return fmt.Errorf("flow %q: declared role %q names no capability — every account would cover it, so the steps it tags would be performable by an account that can do nothing; declare what the role requires, from %v",
				f.name, r.Name, AllCapabilities())
		}
	}
	for _, s := range f.steps {
		if s.kind == stepAwait {
			continue
		}
		if s.role == "" {
			return fmt.Errorf("flow %q: step %s declares no Role — every step is tagged with exactly one declared role, and an untagged step is nobody's move; declared roles are %v",
				f.name, s.describe(), f.RoleNames())
		}
		if !f.DeclaresRole(s.role) {
			return fmt.Errorf("flow %q: step %s: %w", f.name, s.describe(),
				ErrUnknownRole{Role: s.role, Declared: f.RoleNames()})
		}
	}
	return nil
}

// validateReachableFromEntry walks Next forward from the entry and refuses the
// first registered step the walk never arrives at.
func (f *Flow) validateReachableFromEntry() error {
	seen := map[*step]bool{f.entry: true}
	queue := []*step{f.entry}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, id := range s.next {
			// Every id resolves: ValidateGraph checked that before calling.
			succ := f.stepByResult[id]
			if seen[succ] {
				continue
			}
			seen[succ] = true
			queue = append(queue, succ)
		}
	}
	// Registration order, so the first offender reported is the first one
	// written — which is where a reader starts looking.
	for _, s := range f.steps {
		if !seen[s] {
			return fmt.Errorf("flow %q: step %s is not reachable from the entry step %s — no sequence of declared routes arrives at it",
				f.name, s.describe(), f.entry.describe())
		}
	}
	return nil
}

// validateFinalizationReachable walks Next BACKWARD from every step that may
// finalize, and refuses the first registered step the reverse walk never
// arrives at. A step in a cycle with no finalizing exit is caught here: the
// cycle is reachable from the entry, and nothing in it is reachable from a
// finalizer.
func (f *Flow) validateFinalizationReachable() error {
	predecessors := map[*step][]*step{}
	for _, s := range f.steps {
		for _, id := range s.next {
			succ := f.stepByResult[id]
			predecessors[succ] = append(predecessors[succ], s)
		}
	}
	reaches := map[*step]bool{}
	var queue []*step
	for _, s := range f.steps {
		if len(s.mayFinalize) > 0 {
			reaches[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, pred := range predecessors[s] {
			if reaches[pred] {
				continue
			}
			reaches[pred] = true
			queue = append(queue, pred)
		}
	}
	for _, s := range f.steps {
		if !reaches[s] {
			return fmt.Errorf("flow %q: step %s cannot reach finalization — no sequence of declared routes from it ends the flow, so an item that arrives here can never finish",
				f.name, s.describe())
		}
	}
	return nil
}

// describe renders one step for an error message: its description, which is
// what a reader recognises, and its result id, which is what they can act on.
func (s *step) describe() string {
	return fmt.Sprintf("%q (%s)", s.description, s.resultName())
}
