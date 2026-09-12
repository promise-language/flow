package common

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// GitHub is reached through exactly one seam, and this is what makes that true
// rather than customary.
//
// docs/resolution.md § One seam per outside service: "The single route is
// enforced, not observed." The seam meters, names a rate limit as a rate limit,
// serialises the writers of one document and carries what need not be bought
// again — and every one of those properties is worth exactly as much as the
// guarantee that nothing goes around it. A single `github.NewClient` in a later
// change exempts itself from all of it while leaving every test passing.
//
// So the invariant is enforced the way agent spend already is (agentturns.go):
// a ratchet of approved sites, refused at the commit gate, because "the mistake
// is easy and quiet".
//
// FOUR SHAPES ARE REFUSED, and the asymmetry between them is deliberate:
//
//   - Importing go-github outside pkg/orchestrator/github. The IMPORT, not
//     NewClient: a file that imports the library is one line from a client, and
//     the six in-package files that use it only for option structs and
//     github.Ptr are what make the import look innocuous. Inside the package a
//     CLIENT is still refused outside outward.go, and a dot-import is refused
//     everywhere — it puts NewClient in scope unqualified and out of reach.
//
//   - Naming api.github.com in a literal, anywhere. There is no honest reason:
//     the seam's client carries the host, and a test server's URL is what
//     replaces it.
//
//   - Spawning `gh`, anywhere including the seam. `gh` used to open and merge
//     pull requests and read the authenticated login; all three are API calls
//     now, because both routes authenticate as the same account against the same
//     limits and only one of them can be metered. What survives is the one named
//     exception below.
//
//   - Spawning `git push` outside outward.go. Not "anywhere", because unlike
//     `gh` there is no second transport for it to be: pushing IS the seam's own
//     publication (docs/disclosure.md lists the diff and the commit messages on
//     the surface), and the rule is that it happens there and nowhere else.

// approvedOutsideRoutes is the closed list of sites that may reach outside the
// seam, keyed "<path> <enclosing func>", with what the exception is for.
//
// ONE ENTRY, and the division that keeps it at one: `gh` is how flow OBTAINS A
// CREDENTIAL; go-github is how flow TALKS TO GITHUB. Nothing in go-github can
// produce a token — it consumes one — and `gh auth token` reads the store the
// operator already logged into, so flow inherits an existing login instead of
// asking for a second one or keeping a secret of its own. It runs once, at
// client construction, and never per request.
//
// The alternatives are worse, which is why this is an exception rather than a
// gap to close later. Requiring $GITHUB_TOKEN makes an environment variable
// load-bearing for every invocation — the shape docs/org/cli-guide.md rules
// against — and invites a token belonging to a different account than the one
// the operator's `gh` session holds, silently splitting the identity the whole
// role model rests on. Reading gh's keychain directly reimplements a store flow
// does not own, per platform. Implementing an authentication flow is a project.
//
// ADDING AN ENTRY IS THE MAINTAINER'S DECISION, NOT THE COMMITTER'S. Removing
// one when the call goes away is ordinary upkeep and needs nobody's approval.
var approvedOutsideRoutes = map[string]string{
	"pkg/orchestrator/github/outward.go ghAuthToken": "obtains the operator's credential from the store `gh auth login` populated, once at client construction, never per request",
}

// seamPackage is the directory the GitHub seam lives in, and outwardFile the
// one file inside it that may hold the client.
const (
	seamPackage = "pkg/orchestrator/github/"
	outwardFile = "pkg/orchestrator/github/outward.go"

	// checkerFile is this file. See scanOutsideRoutes for why it is skipped.
	checkerFile = "tools/build/common/outsideroutes.go"
)

// checkOutsideServiceRoutes refuses a commit that reaches GitHub outside the
// seam, and refuses a stale approved list.
//
// Tests are not scanned. The package's own tests build clients on purpose —
// that is what pointing go-github at an httptest server means — and a rule that
// forbade it would forbid testing the seam.
//
// It is a pattern scan over syntax, not a proof: a client built through a helper
// in another module, or a command name assembled from pieces, slips past. It
// catches the honest case, which is the one that keeps happening.
func checkOutsideServiceRoutes(repoRoot string) error {
	return checkServiceRoutes(repoRoot, approvedOutsideRoutes)
}

// checkServiceRoutes is the check against a given list, so a test can exercise
// the rule without editing the real one — the real list is a fact about this
// repository, and a test that had to change it would be testing nothing.
func checkServiceRoutes(repoRoot string, approved map[string]string) error {
	found, err := scanOutsideRoutes(repoRoot)
	if err != nil {
		return err
	}

	var added, stale []string
	for site, reports := range found {
		if _, ok := approved[site]; ok {
			continue
		}
		for _, r := range reports {
			added = append(added, "  "+r)
		}
	}
	for site := range approved {
		if len(found[site]) == 0 {
			stale = append(stale, fmt.Sprintf("  %s — approved, but nothing there reaches outside the seam", site))
		}
	}
	sort.Strings(added)
	sort.Strings(stale)

	switch {
	case len(added) > 0:
		return fmt.Errorf("a second route to GitHub:\n%s\n\n"+
			"Every outside service is reached through exactly one seam, and the single route is enforced "+
			"rather than observed (docs/resolution.md § One seam per outside service). The seam meters, "+
			"names a rate limit as a rate limit, serialises the writers of one document and caches what "+
			"need not be bought again — a route around it has none of that and disables all of it.\n"+
			"Add a method to pkg/orchestrator/github/outward.go and call that. If this really cannot go "+
			"through the seam, the MAINTAINER names it in approvedOutsideRoutes in "+
			"tools/build/common/outsideroutes.go, with what the exception is for. Exceptions are named, "+
			"and never categorical. Do not add yourself to that list.",
			strings.Join(added, "\n"))
	case len(stale) > 0:
		return fmt.Errorf("the approved outside-route list no longer matches the code:\n%s\n\n"+
			"A call was removed or moved. Dropping the entry is ordinary upkeep — edit "+
			"approvedOutsideRoutes in tools/build/common/outsideroutes.go to match. An exact list is what "+
			"makes an added route visible; one that drifts stops being a ratchet.",
			strings.Join(stale, "\n"))
	}
	return nil
}

// scanOutsideRoutes reports every route out of the seam in every non-test Go
// file of every module, keyed by "<path> <enclosing function>" — the same key
// shape approvedAgentTurns uses, so one idiom covers both ratchets. A route
// outside any function is keyed by its file alone, so it cannot be silently
// ignored.
func scanOutsideRoutes(repoRoot string) (map[string][]string, error) {
	found := map[string][]string{}
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
		slash := filepath.ToSlash(rel)
		if slash == checkerFile {
			// The checker names the strings it forbids — that is what a
			// literal scan IS — so scanning itself would make the rule
			// impossible to express. The exemption is narrow by construction:
			// it names one file, in the build module, which builds no binary
			// that talks to GitHub.
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			// A file that does not parse is not this gate's business: `go
			// build` in bin/verify says so far better than a scanner can.
			return nil
		}
		for _, v := range outsideRouteViolations(slash, file) {
			key := strings.TrimSpace(slash + " " + enclosingFunc(file, v.pos))
			found[key] = append(found[key], fset.Position(v.pos).String()+": "+v.what)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning for routes to GitHub: %w", err)
	}
	return found, nil
}

type outsideRoute struct {
	pos  token.Pos
	what string
}

// outsideRouteViolations reports every place one file could reach GitHub
// without passing the seam. name is the file's slash-separated path from the
// repository root, which is what decides whether it is inside the seam.
func outsideRouteViolations(name string, file *ast.File) []outsideRoute {
	var found []outsideRoute
	report := func(pos token.Pos, format string, args ...any) {
		found = append(found, outsideRoute{pos: pos, what: fmt.Sprintf(format, args...)})
	}

	inSeam := strings.HasPrefix(name, seamPackage)
	isOutward := name == outwardFile

	// Whatever local name this file gave the client library, if any. Following
	// the import rather than the identifier `github` is what makes aliasing it
	// — a one-word edit — no way around the check.
	clientPkg := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.HasPrefix(path, "github.com/google/go-github/") {
			continue
		}
		local := "github" // the library's own package name
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == "." {
			report(imp.Pos(), "%s dot-imports the client library, which puts NewClient in scope "+
				"unqualified and out of this check's reach", name)
			continue
		}
		if !inSeam {
			report(imp.Pos(), "%s imports the GitHub client library; only %s may, and a file that "+
				"imports it is one line from a client", name, seamPackage)
			continue
		}
		clientPkg[local] = true
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			pkg, ok := node.X.(*ast.Ident)
			if !ok || !clientPkg[pkg.Name] || isOutward {
				return true
			}
			// The type itself, and every constructor of one: go-github names
			// them all New* (NewClient, NewTokenClient, NewEnterpriseClient,
			// NewClientWithEnvProxy).
			if node.Sel.Name == "Client" || strings.HasPrefix(node.Sel.Name, "New") {
				report(node.Pos(), "%s holds a second GitHub client (%s.%s); the only one lives in %s",
					name, pkg.Name, node.Sel.Name, outwardFile)
			}
		case *ast.CallExpr:
			if !spawnShaped(node) || isOutward {
				return true
			}
			pushRoute(report, name, stringLiterals(node.Args))
		case *ast.CompositeLit:
			// `args := []string{"push", …}` spawns git exactly as well as
			// `run(ctx, "push", …)` does. A MAP keyed "push" is not a command
			// line, which is what keeps p["push"] out of this.
			if !isStringSlice(node.Type) || isOutward {
				return true
			}
			pushRoute(report, name, stringLiterals(node.Elts))
		case *ast.BasicLit:
			// `gh` and the host are refused ANYWHERE, in any position: unlike
			// push there is no shape that makes either innocent, and reading
			// only call arguments would miss a name held in a var and spawned
			// one line later.
			if node.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			if strings.Contains(v, "api.github.com") {
				report(node.Pos(), "%s names api.github.com; the seam's client carries the host", name)
			}
			if v == "gh" {
				report(node.Pos(), "%s names `gh`; flow talks to GitHub through the client and spawns "+
					"`gh` only to obtain a credential", name)
			}
		}
		return true
	})
	return found
}

// pushRoute reports a `push` among the words a call would spawn.
func pushRoute(report func(token.Pos, string, ...any), name string, lits []*ast.BasicLit) {
	for _, lit := range lits {
		if v, err := strconv.Unquote(lit.Value); err == nil && v == "push" {
			report(lit.Pos(), "%s runs `git push` outside the seam; a push is a publication and goes "+
				"through %s", name, outwardFile)
		}
	}
}

// stringLiterals picks the string literals out of a list of expressions. A
// key/value's KEY is skipped: it names a field or a map entry, never an
// argument.
func stringLiterals(exprs []ast.Expr) []*ast.BasicLit {
	var out []*ast.BasicLit
	for _, e := range exprs {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			e = kv.Value
		}
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, lit)
		}
	}
	return out
}

// spawnShaped reports whether a call is the kind that turns a string into a
// process: the os/exec constructors and the runner seams this repository spawns
// through. The CALLEE's shape is what keeps flow.Artifact("push", …) and
// CapPush = "push" from being read as command lines.
func spawnShaped(call *ast.CallExpr) bool {
	var fn string
	switch f := call.Fun.(type) {
	case *ast.Ident:
		fn = f.Name
	case *ast.SelectorExpr:
		fn = f.Sel.Name
	default:
		return false
	}
	switch fn {
	case "Command", "CommandContext", "LookPath", "run", "runner":
		return true
	}
	return false
}

// isStringSlice reports whether a composite literal's type is written []string
// — an argument vector, as opposed to a map whose keys happen to be command
// names.
func isStringSlice(t ast.Expr) bool {
	arr, ok := t.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "string"
}
