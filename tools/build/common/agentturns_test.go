package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The list is a claim about THIS repository, so the first test is that the
// claim is true right now. A drifted list is not a ratchet: it either refuses
// every commit or approves a site nobody looked at.
func TestApprovedAgentTurnsMatchesTheRepository(t *testing.T) {
	if err := checkApprovedAgentTurns(repoRoot(t)); err != nil {
		t.Fatalf("the approved-turn list does not match the tree:\n%v", err)
	}
}

// The point of the ratchet: a turn requested somewhere new is refused, and the
// refusal says who approves one and where the list lives.
func TestAgentTurns_RefusesAnUnapprovedSite(t *testing.T) {
	root := treeWith(t, "cli/cmd_doctor.go", `package cli

func (app *App) checkAgent(ctx context.Context) check {
	resp, err := app.Agent.Run(ctx, flow.AgentRequest{Prompt: "Reply with the single word: ok"})
	_, _ = resp, err
	return check{}
}
`)
	err := checkAgentTurns(root, map[string]int{})
	if err == nil {
		t.Fatal("a new agent turn in a mechanical command was allowed")
	}
	for _, want := range []string{"cli/cmd_doctor.go (*App).checkAgent", "MAINTAINER", "approvedAgentTurns"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// An approved site may not quietly grow: two turns where one was approved is a
// new spend path, whatever function it sits in.
func TestAgentTurns_RefusesAnExtraTurnAtAnApprovedSite(t *testing.T) {
	root := treeWith(t, "issue/steps.go", `package issue

func (b *builder) stepPlan(ctx flow.StepCtx) error {
	b.runAgent(ctx, flow.AgentRequest{Prompt: "one"})
	b.runAgent(ctx, flow.AgentRequest{Prompt: "two"})
	return nil
}
`)
	if err := checkAgentTurns(root, map[string]int{"issue/steps.go (*builder).stepPlan": 1}); err == nil {
		t.Fatal("a second turn at an approved site was allowed")
	}
}

// A list that outlives the call it approved stops being exact, and an exact
// list is the whole mechanism. Removing an entry is self-service, and the
// message says so.
func TestAgentTurns_RefusesAStaleEntry(t *testing.T) {
	root := treeWith(t, "issue/steps.go", "package issue\n")
	err := checkAgentTurns(root, map[string]int{"issue/steps.go (*builder).stepPlan": 1})
	if err == nil {
		t.Fatal("an entry approving a call that no longer exists was accepted")
	}
	if !strings.Contains(err.Error(), "ordinary upkeep") {
		t.Errorf("a stale entry should read as upkeep, not as a violation:\n%v", err)
	}
}

// Both shapes of the same act are caught: building the payload, and calling the
// chokepoint with one somebody else built.
func TestAgentTurns_CatchesBothTheCallAndThePayload(t *testing.T) {
	for _, src := range []string{
		"package p\n\nfunc spend(ctx flow.StepCtx, req flow.AgentRequest) { ctx.Agent().Run(ctx.Context(), req) }\n",
		"package p\n\nfunc build() flow.AgentRequest { return flow.AgentRequest{Prompt: \"x\"} }\n",
	} {
		root := treeWith(t, "cli/sneaky.go", src)
		if err := checkAgentTurns(root, map[string]int{}); err == nil {
			t.Errorf("unapproved turn was allowed:\n%s", src)
		}
	}
}

// Tests are not scanned: a test cannot reach the real agent — the claude client
// refuses to spawn from a test process and every test agent here is a stub — so
// pinning them would be churn without a risk removed.
func TestAgentTurns_IgnoresTests(t *testing.T) {
	root := treeWith(t, "cli/thing_test.go",
		"package cli\n\nfunc TestX(t *testing.T) { app.Agent.Run(ctx, flow.AgentRequest{}) }\n")
	if err := checkAgentTurns(root, map[string]int{}); err != nil {
		t.Fatalf("a test file was scanned: %v", err)
	}
}

// treeWith writes one file into a temp repo root and returns the root.
func treeWith(t *testing.T, rel, body string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return root
}

// repoRoot is three levels up from tools/build/common.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return abs
}
