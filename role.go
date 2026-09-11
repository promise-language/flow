package flow

import "slices"

// Capability is what an ACCOUNT can do on the repository — a verifiable fact,
// detected from the backend and never declared (docs/resolution.md § Accounts,
// capabilities and roles: "an account's word for what it may do is worth
// exactly what the backend will actually permit").
//
// Closed at three, and the closure is load-bearing in a way the open RoleName
// vocabulary is not. A role declaration is written against this set, so an open
// one would let a flow require a capability no backend could ever report: the
// role would simply never be assumable, every item tagged with it would sit
// awaiting nobody, and nothing would name the typo.
type Capability string

const (
	// CapPush — the account may push commits to the repository.
	CapPush Capability = "push"
	// CapMerge — the account may land a pull request. Distinct from push
	// rather than implied by it: a protected default branch is precisely the
	// configuration in which an account that can push cannot merge, and it is
	// the configuration a repository with maintainers has.
	CapMerge Capability = "merge"
	// CapApprove — the account's review counts as an approval.
	CapApprove Capability = "approve"
)

// AllCapabilities returns every capability, in declaration order. Consumers
// enumerate it rather than mirroring the set, which is how two copies of one
// vocabulary drift.
func AllCapabilities() []Capability { return []Capability{CapPush, CapMerge, CapApprove} }

// Valid reports whether c is one of the three.
//
// The empty capability is not one, and unlike the step-model axes there is no
// defaulting to hide behind: a role that requires nothing carries an EMPTY set,
// never a set holding an empty member.
func (c Capability) Valid() bool { return slices.Contains(AllCapabilities(), c) }

// RoleDecl is one role as a flow declares it: the name its steps are tagged
// with, and the capabilities an account must hold to assume it
// (docs/flow-registration.md § Roles).
type RoleDecl struct {
	Name         RoleName
	Capabilities []Capability
}

// Missing returns the capabilities the role requires that detected does not
// hold, in declaration order. Empty means the account backs the role.
//
// It is THE predicate for "can this account back this role", and it answers
// with the difference rather than a bool because its readers need both: a
// selection asks whether the set is empty, and a report that says a role is
// out of reach has to name what is missing, or it sends the operator to work
// out from the declaration what the report already knew.
//
// A role requiring NO capability has nothing missing for every account,
// including one with nothing detected. That is the honest reading of "all of
// its required capabilities are held", and it is why ValidateGraph refuses a
// capability-less declaration rather than this quietly treating it as
// unbackable.
func (d RoleDecl) Missing(detected []Capability) []Capability {
	var missing []Capability
	for _, need := range d.Capabilities {
		if !slices.Contains(detected, need) {
			missing = append(missing, need)
		}
	}
	return missing
}

// AssumableRoles returns the roles from decls the detected capabilities back,
// in declaration order — the roles with nothing Missing. Capability is the
// ceiling: a role whose account cannot back it is simply not in the answer.
//
// The SET form of the one predicate, for a caller that wants the roles rather
// than what each is missing. It is written on top of Missing rather than
// beside it, so the two cannot disagree about what "backs" means. A FREE
// FUNCTION over []RoleDecl rather than a *Flow method, so it can be asked of
// any declaration list, a flow's or a caller's own.
func AssumableRoles(decls []RoleDecl, detected []Capability) []RoleName {
	var out []RoleName
	for _, d := range decls {
		if len(d.Missing(detected)) == 0 {
			out = append(out, d.Name)
		}
	}
	return out
}
