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

// AssumableRoles returns the roles from decls whose every required capability
// appears in detected, in declaration order. Capability is the ceiling: a role
// whose account cannot back it is simply not in the answer.
//
// A FREE FUNCTION over []RoleDecl rather than a *Flow method, because its first
// caller derives assumable roles before a flow exists — a binary reads what its
// account can do and the flow it then builds is the consequence, so a method
// hung on *Flow could not be called at the one moment the question is asked.
// One with no consumer would be a declaration nothing reads.
//
// A role requiring NO capability is covered by every account, including one
// with nothing detected. That is the honest reading of "all of its required
// capabilities are held", and it is why ValidateGraph refuses a capability-less
// declaration rather than this quietly treating it as unassumable.
func AssumableRoles(decls []RoleDecl, detected []Capability) []RoleName {
	var out []RoleName
	for _, d := range decls {
		covered := true
		for _, need := range d.Capabilities {
			if !slices.Contains(detected, need) {
				covered = false
				break
			}
		}
		if covered {
			out = append(out, d.Name)
		}
	}
	return out
}
