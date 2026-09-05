package common

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// A request for an agent turn takes one of two shapes in source, and this scan
// recognises both:
//
//   - a call to Run on something that reaches the agent — ctx.Agent().Run(…),
//     app.Agent.Run(…) — which is the chokepoint itself;
//   - a flow.AgentRequest literal, which is the payload a turn is spawned from.
//     One built in one place and dispatched in another is still a turn somebody
//     asked for, and that is the shape a new spend path takes when the call
//     itself is hidden behind a helper.
//
// A literal that IS the argument of such a call is one site, not two.
//
// Neither shape matches a function that FORWARDS a request it was handed
// (cli's meteredAgent, which meters and passes through). Metering a turn
// somebody else asked for is not asking for one, and pinning the wrapper would
// say the SDK's own plumbing needs approval to exist.

// approvedAgentTurns is the closed list of places that may ask for an agent
// turn, and how many times each does. It is a RATCHET, not documentation: a
// commit that introduces a turn anywhere else is refused.
//
// Every entry is a step of resolving an item — work a person asked for, against
// a budget, producing an artifact. Nothing mechanical is on this list and
// nothing mechanical may join it: `doctor`, `list` and `status` run before every
// item, in CI, and on every machine an operator touches, so a turn on one of
// those paths is a standing charge nobody asked for. `doctor` carried exactly
// such a turn — a capped, tool-free probe on every run — which is why this list
// exists rather than a convention.
//
// ADDING AN ENTRY IS THE MAINTAINER'S DECISION, NOT THE COMMITTER'S. Removing
// one when the call goes away is ordinary upkeep and needs nobody's approval.
var approvedAgentTurns = map[string]int{
	"issue/steps.go (*builder).stepPlan":              1,
	"issue/steps.go (*builder).stepImplement":         1,
	"issue/steps.go (*builder).commitWithRepair":      2,
	"issue/steps.go (*builder).producingMarkdownStep": 1,
	"issue/steps.go (*builder).stepOpenPR":            1,
	"issue/steps.go (*builder).runAgent":              1,
	"issue/steps.go (*builder).resolveQuestion":       1,
	"issue/steps.go (*builder).agentMarkdownStep":     1,
	"issue/steps.go (*builder).resolveMarkdown":       1,
}

// checkApprovedAgentTurns refuses a commit that asks for an agent turn anywhere
// but an approved call site, and refuses a stale list.
//
// The rule is the same one checkNoAgentExec enforces one level down, at the
// place a turn becomes a process: a turn costs money and takes as long as a
// model takes, so it belongs only where somebody asked for work. The two checks
// sit at different levels because the mistake arrives at both — spawning the
// binary directly, and reaching the SDK's own agent from a command that should
// have answered by reading.
//
// Tests are not scanned. A test cannot reach the real agent — the claude client
// refuses to spawn from a test process, and every test agent in this repo is a
// stub — so pinning test call sites would add churn without removing a risk.
//
// It is a pattern scan over syntax, not a proof: a request built through a
// helper that takes the fields apart slips past. It catches the honest case,
// which is the one that keeps happening.
func checkApprovedAgentTurns(repoRoot string) error {
	return checkAgentTurns(repoRoot, approvedAgentTurns)
}

// checkAgentTurns is the check against a given list, so a test can exercise the
// rule without editing the real one — the real list is a fact about this
// repository, and a test that had to change it would be testing nothing.
func checkAgentTurns(repoRoot string, approved map[string]int) error {
	found, err := scanAgentTurns(repoRoot)
	if err != nil {
		return err
	}

	var added, stale []string
	for site, n := range found {
		if ok := approved[site]; n > ok {
			added = append(added, fmt.Sprintf("  %s — asks for %d turn(s), %d approved", site, n, ok))
		}
	}
	for site, ok := range approved {
		if n := found[site]; n < ok {
			stale = append(stale, fmt.Sprintf("  %s — approved for %d, found %d", site, ok, n))
		}
	}
	sort.Strings(added)
	sort.Strings(stale)

	switch {
	case len(added) > 0:
		return fmt.Errorf("an agent turn is requested where it has not been approved:\n%s\n\n"+
			"Every turn is billed and takes as long as a model takes, so one belongs only where somebody "+
			"asked for work: a step of resolving an item, against a budget, producing an artifact. "+
			"Nothing mechanical may spend — a command that runs before every item, in CI, and on every "+
			"machine (doctor, list, status) turns a turn into a standing charge nobody asked for.\n"+
			"If this call really is a step of resolving an item, the MAINTAINER approves it by adding it to "+
			"approvedAgentTurns in tools/build/common/agentturns.go. Do not add yourself to that list.",
			strings.Join(added, "\n"))
	case len(stale) > 0:
		return fmt.Errorf("the approved agent-turn list no longer matches the code:\n%s\n\n"+
			"A call was removed or moved. Dropping the entry is ordinary upkeep — edit approvedAgentTurns in "+
			"tools/build/common/agentturns.go to match. An exact list is what makes an added turn visible; "+
			"one that drifts stops being a ratchet.",
			strings.Join(stale, "\n"))
	}
	return nil
}

// scanAgentTurns counts the requests for a turn in every non-test Go file,
// keyed by "<path> <enclosing function>". A request outside any function is
// keyed by the file alone, so it cannot be silently ignored.
func scanAgentTurns(repoRoot string) (map[string]int, error) {
	found := map[string]int{}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			// A file that does not parse is not this gate's business: `go
			// build` in bin/verify says so far better than a scanner can.
			return nil
		}
		for _, pos := range turnSites(file) {
			found[filepath.ToSlash(rel)+" "+enclosingFunc(file, pos)]++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning for agent turns: %w", err)
	}
	return found, nil
}

// turnSites returns the position of every request for a turn in one file.
func turnSites(file *ast.File) []token.Pos {
	// The literals that belong to a chokepoint call, so a call and the payload
	// written inside it count once between them rather than twice.
	dispatched := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isAgentRunCall(call) {
			return true
		}
		for _, arg := range call.Args {
			ast.Inspect(arg, func(inner ast.Node) bool {
				if lit, isLit := inner.(*ast.CompositeLit); isLit && isAgentRequestLit(lit) {
					dispatched[lit] = true
				}
				return true
			})
		}
		return true
	})

	var sites []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if isAgentRunCall(node) {
				sites = append(sites, node.Pos())
			}
		case *ast.CompositeLit:
			if isAgentRequestLit(node) && !dispatched[node] {
				sites = append(sites, node.Pos())
			}
		}
		return true
	})
	return sites
}

// isAgentRunCall reports whether a call is Run() on something that reaches the
// agent: ctx.Agent().Run(…), app.Agent.Run(…), agent.Run(…).
func isAgentRunCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Run" && mentionsAgent(sel.X)
}

// mentionsAgent reports whether the receiver expression names the agent
// anywhere along its chain. Textual on purpose: the scan does not type-check,
// and a value called anything else is not the honest case this catches.
func mentionsAgent(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "agent") {
			found = true
		}
		return !found
	})
	return found
}

// isAgentRequestLit reports whether a composite literal builds an AgentRequest,
// written either qualified (flow.AgentRequest{…}) or bare inside the package.
func isAgentRequestLit(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == "AgentRequest"
	case *ast.SelectorExpr:
		return t.Sel.Name == "AgentRequest"
	}
	return false
}

// enclosingFunc names the function containing pos — "(*builder).runAgent" for a
// method, "renderPrompt" for a plain function — or "" when the match sits
// outside every function body (a package-level var, say), which keys the site
// by its file and still has to be approved.
func enclosingFunc(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || pos < fn.Pos() || pos > fn.End() {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return fn.Name.Name
		}
		return "(" + typeString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
	}
	return ""
}

// typeString renders a receiver type as it is written: "*builder", "App".
func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver: Foo[T]
		return typeString(t.X)
	case *ast.IndexListExpr:
		return typeString(t.X)
	}
	return "?"
}
