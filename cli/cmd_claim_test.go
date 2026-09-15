package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/orchestrator/fake"
)

// refResolverBackend wraps the fake backend with a flow.RefResolver fast path
// and records whether (and with what id) it was consulted.
type refResolverBackend struct {
	*fake.Orchestrator
	called bool
	gotID  string
}

func (r *refResolverBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	r.called = true
	r.gotID = id
	return flow.ItemRef{OrchestratorName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

// When the backend implements RefResolver, resolveClaimRef uses it directly and
// never calls ListEligible — proven here by adding NO items to the fake yet
// still resolving a ref.
func TestResolveClaimRef_UsesRefResolver(t *testing.T) {
	be := &refResolverBackend{Orchestrator: fake.New()}
	app := &App{Orchestrator: be}

	ref, err := app.resolveClaimRef(context.Background(), "T0435")
	if err != nil {
		t.Fatalf("resolveClaimRef: %v", err)
	}
	if !be.called {
		t.Fatal("expected RefResolver.ResolveRef to be used")
	}
	if be.gotID != "T0435" {
		t.Errorf("ResolveRef got id %q, want T0435", be.gotID)
	}
	if ref.Display != "T0435" {
		t.Errorf("ref.Display = %q, want T0435", ref.Display)
	}
}

// Without RefResolver, resolveClaimRef falls back to listing eligible items and
// matching the display string.
func TestResolveClaimRef_FallsBackToListMatch(t *testing.T) {
	be := fake.New()
	be.AddItem("T0435", flow.Item{Type: "task", Title: "T0435"})
	app := &App{Orchestrator: be}

	ref, err := app.resolveClaimRef(context.Background(), "T0435")
	if err != nil {
		t.Fatalf("resolveClaimRef: %v", err)
	}
	if ref.Display != "T0435" {
		t.Errorf("ref.Display = %q, want T0435", ref.Display)
	}
}

// ResolveRef is the ONE place a value enters the contract before it is an
// identity, so its refusal is the only way a bad id is caught. Matching on
// Display would resolve by substring and first match — AN item rather than THE
// item — which is why that path no longer exists.
func TestResolveClaimRef_ReportsTheOrchestratorsRefusal(t *testing.T) {
	app := &App{Orchestrator: refusingRefBackend{fake.New()}}

	_, err := app.resolveClaimRef(context.Background(), "T9999")
	if err == nil {
		t.Fatal("expected the orchestrator's refusal to reach the caller")
	}
	if !strings.Contains(err.Error(), "T9999") {
		t.Errorf("err = %v, want it to name the input that could not be resolved", err)
	}
}

type refusingRefBackend struct{ *fake.Orchestrator }

func (b refusingRefBackend) ResolveRef(_ context.Context, input string) (flow.ItemRef, error) {
	return flow.ItemRef{}, fmt.Errorf("no item named %q", input)
}

// recordingClaimBackend captures the overrides passed to Claim so the
// flag-after-positional test can assert that the flag is actually
// parsed (and not silently dropped).
// It also counts the calls, which is what a test about a refusal decided BEFORE
// the backend is reached asserts on: no lease was asked for at all.
type recordingClaimBackend struct {
	*fake.Orchestrator
	lastOverrides []flow.ClaimOverride
	claims        int
}

func (r *recordingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	r.lastOverrides = overrides
	r.claims++
	return r.Orchestrator.Claim(ctx, ref, overrides)
}

// T0484: `claim <id> --force` (bool flag after the positional) must parse and
// reach the backend as force=true. Prior to the interspersed-parsing fix the
// stdlib flag package stopped at the positional and reported --force as an
// "unexpected argument".
func TestCmdClaim_ForceAfterPositional(t *testing.T) {
	be := fake.New()
	be.AddItem("T0001", flow.Item{Type: "task", Title: "T0001"})
	wrapped := &recordingClaimBackend{Orchestrator: be}

	app := &App{
		Orchestrator: wrapped,
		Out:          newDiscardWriter(),
		Err:          newDiscardWriter(),
	}

	code := app.cmdClaim(context.Background(), []string{"T0001", "--force"})
	if code != 0 {
		t.Fatalf("cmdClaim = %d, want 0", code)
	}
	if !slices.Contains(wrapped.lastOverrides, flow.OverrideDirtyTree) {
		t.Errorf("Backend.Claim received overrides=%v, want OverrideDirtyTree", wrapped.lastOverrides)
	}
	if !slices.Contains(wrapped.lastOverrides, flow.OverrideAlreadyHeld) {
		t.Errorf("Backend.Claim received overrides=%v, want OverrideAlreadyHeld", wrapped.lastOverrides)
	}
	if !slices.Contains(wrapped.lastOverrides, flow.OverrideStaleBase) {
		t.Errorf("Backend.Claim received overrides=%v, want OverrideStaleBase", wrapped.lastOverrides)
	}
}

// --force-unadmitted must pass OverrideUnadmitted to Backend.Claim.
func TestCmdClaim_ForceUnadmittedFlag(t *testing.T) {
	be := fake.New()
	be.AddItem("T0001", flow.Item{Type: "task", Title: "T0001"})
	wrapped := &recordingClaimBackend{Orchestrator: be}

	app := &App{
		Orchestrator: wrapped,
		Out:          newDiscardWriter(),
		Err:          newDiscardWriter(),
	}

	code := app.cmdClaim(context.Background(), []string{"T0001", "--force-unadmitted"})
	if code != 0 {
		t.Fatalf("cmdClaim = %d, want 0", code)
	}
	if !slices.Contains(wrapped.lastOverrides, flow.OverrideUnadmitted) {
		t.Errorf("Backend.Claim received overrides=%v, want OverrideUnadmitted", wrapped.lastOverrides)
	}
	// --force-unadmitted alone must NOT include the dirty-tree or already-held overrides.
	if slices.Contains(wrapped.lastOverrides, flow.OverrideDirtyTree) {
		t.Errorf("OverrideDirtyTree present without --force; overrides=%v", wrapped.lastOverrides)
	}
}

// ---------------------------------------------------------------------------
// Open blockers WARN, and never refuse (docs/cli.md § Claiming). A claim is an
// arena reservation — this worktree, this item — and taking one does no work,
// so the dependency has nothing to stop here. It stops the next advance.
// ---------------------------------------------------------------------------

// claimEnv is one unclaimed item on a fresh backend, with both streams
// captured: the claim under test is the one cmdClaim mints.
type claimEnv struct {
	app *App
	be  *fake.Orchestrator
	out *bytes.Buffer
	err *bytes.Buffer
}

func newClaimEnv(t *testing.T) *claimEnv {
	t.Helper()
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "the item"})
	env := &claimEnv{be: be, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	env.app = &App{Orchestrator: be, Out: env.out, Err: env.err}
	return env
}

// The whole warning: exit 0, the result on stdout, and the narration on stderr
// naming the blocker still open — and not the one that has landed, which is
// nowhere to send anybody.
func TestCmdClaim_OpenBlockersWarnWithoutRefusing(t *testing.T) {
	env := newClaimEnv(t)
	env.be.AddItem("2", flow.Item{Type: "task", Title: "landed"})
	env.be.AddItem("3", flow.Item{Type: "task", Title: "still open"})
	env.be.SetStatus("2", flow.StatusTerminal, "done")
	blockOn(t, env.be, env.be.Ref("1"), env.be.Ref("2"))
	blockOn(t, env.be, env.be.Ref("1"), env.be.Ref("3"))

	code := env.app.cmdClaim(context.Background(), []string{"1"})
	if code != 0 {
		t.Fatalf("cmdClaim = %d, want 0 — open blockers do not refuse a claim; stderr=%q", code, env.err.String())
	}
	// The result is the claim, and it goes to stdout alone.
	if got := env.out.String(); !strings.Contains(got, "claimed 1 as ") {
		t.Errorf("stdout = %q, want the claim result", got)
	}
	warn := env.err.String()
	if !strings.Contains(warn, "blocked by: 3") {
		t.Errorf("stderr = %q, want the open blocker named", warn)
	}
	if strings.Contains(warn, "2") {
		t.Errorf("stderr = %q, names a blocker that has already landed", warn)
	}
	if !strings.Contains(warn, "the next advance will stop on them") {
		t.Errorf("stderr = %q, want it to say what the block will do", warn)
	}
	// And the lease is real: a warning is not a half-taken claim.
	if held, _ := env.be.LookupActiveClaim(context.Background()); held == nil {
		t.Error("no claim was taken — the warning must not stand in for the claim")
	}
}

func TestCmdClaim_UnblockedItemWarnsNothing(t *testing.T) {
	env := newClaimEnv(t)

	if code := env.app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("cmdClaim = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.err.String(); got != "" {
		t.Errorf("stderr = %q, want nothing for an item with no blockers", got)
	}
}

// Every declared blocker has finished: the item is workable, so a warning
// would send the operator to work something already done.
func TestCmdClaim_LandedBlockersWarnNothing(t *testing.T) {
	env := newClaimEnv(t)
	env.be.AddItem("2", flow.Item{Type: "task", Title: "landed"})
	env.be.SetStatus("2", flow.StatusTerminal, "done")
	blockOn(t, env.be, env.be.Ref("1"), env.be.Ref("2"))

	if code := env.app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("cmdClaim = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.err.String(); got != "" {
		t.Errorf("stderr = %q, want nothing once every blocker has landed", got)
	}
}

// A block NOBODY declared items for warns nothing. The kind decides — a
// park-derived block is somebody's move on this item, not somewhere else to go,
// and the next advance runs straight through it (blockedFromAdvancing). Saying
// "the next advance will stop on them" about it would be false twice over: no
// them, and no stop.
func TestCmdClaim_ParkDerivedBlockWarnsNothing(t *testing.T) {
	env := newClaimEnv(t)
	if err := env.be.Park(context.Background(), env.be.Ref("1"), treasurerRefused("plan", flow.AxisInvocations)); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if code := env.app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("cmdClaim = %d, want 0; stderr=%q", code, env.err.String())
	}
	// The item IS blocked — the backend derives waits-on-person from the park —
	// and the claim still says nothing about unfinished items.
	state, err := env.be.Load(context.Background(), env.be.Ref("1"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !state.Blocked || state.BlockKind != flow.WaitsOnPerson {
		t.Fatalf("item = blocked %v kind %q, want the park's waits-on-person — the test proves nothing otherwise",
			state.Blocked, state.BlockKind)
	}
	if got := env.err.String(); got != "" {
		t.Errorf("stderr = %q, want nothing — this block names no items to wait on", got)
	}
}

// unreadableBackend mints claims normally and then cannot read the item back:
// the FIRST read — the one that decides whose move it is, before any lease is
// taken — succeeds, and every read after it fails. That is where the warning
// read sits, and the only place a failing read is best-effort: a pre-claim read
// that fails refuses the claim rather than taking one blind.
type unreadableBackend struct {
	*fake.Orchestrator
	loads int
}

func (b *unreadableBackend) Load(ctx context.Context, ref flow.ItemRef) (*flow.Item, error) {
	b.loads++
	if b.loads == 1 {
		return b.Orchestrator.Load(ctx, ref)
	}
	return nil, errors.New("backend unavailable")
}

// itemlessBackend answers the read with no item and no error — the shape a
// third-party orchestrator can return, since Load's contract does not forbid
// it, and the one that dereferences to a panic if nothing checks.
type itemlessBackend struct{ *fake.Orchestrator }

func (b itemlessBackend) Load(context.Context, flow.ItemRef) (*flow.Item, error) {
	return nil, nil
}

// The warning is best-effort BECAUSE the lease is already minted: a read that
// comes back with no item — failed, or empty — prints nothing and changes
// nothing. Reporting it as a failure would leave an arena holding an item the
// operator was told they did not get, and panicking on it would leave the same
// arena holding an item behind a stack trace.
func TestCmdClaim_AWarningReadThatYieldsNoItemIsSilentAndStillSucceeds(t *testing.T) {
	tests := []struct {
		name string
		wrap func(*fake.Orchestrator) flow.Orchestrator
	}{
		{"the read fails", func(be *fake.Orchestrator) flow.Orchestrator { return &unreadableBackend{Orchestrator: be} }},
		{"the read returns no item", func(be *fake.Orchestrator) flow.Orchestrator { return itemlessBackend{be} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newClaimEnv(t)
			env.app.Orchestrator = tt.wrap(env.be)

			if code := env.app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
				t.Fatalf("cmdClaim = %d, want 0 — a warning read that yields nothing is not a failed claim; stderr=%q", code, env.err.String())
			}
			if got := env.out.String(); !strings.Contains(got, "claimed 1 as ") {
				t.Errorf("stdout = %q, want the claim result", got)
			}
			if got := env.err.String(); got != "" {
				t.Errorf("stderr = %q, want nothing — the read yielded nothing, and the claim did not fail", got)
			}
		})
	}
}

// refusingClaimBackend always returns an ErrClaimRefused from Claim.
type refusingClaimBackend struct {
	*fake.Orchestrator
	refusal flow.ErrClaimRefused
}

func (b *refusingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, overrides []flow.ClaimOverride) (flow.Claim, error) {
	return flow.Claim{}, b.refusal
}

// cmdClaim must render a typed refusal via formatClaimRefusal and exit 1.
func TestCmdClaim_RefusalRendering(t *testing.T) {
	be := fake.New()
	be.AddItem("T0001", flow.Item{Type: "task", Title: "T0001"})
	errBuf := &bytes.Buffer{}
	app := &App{
		Orchestrator: &refusingClaimBackend{
			Orchestrator: be,
			refusal: flow.ErrClaimRefused{
				Code:     "not-admitted",
				Reason:   "arena not admitted",
				Check:    "git-identity",
				Detail:   `author email "djabi@kmac" is not valid`,
				Override: "force-unadmitted",
			},
		},
		Out: newDiscardWriter(),
		Err: errBuf,
	}

	code := app.cmdClaim(context.Background(), []string{"T0001"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := errBuf.String()
	if !strings.Contains(got, "refused") {
		t.Errorf("expected 'refused' in output; got %q", got)
	}
	if !strings.Contains(got, `check "git-identity"`) {
		t.Errorf("expected check name in output; got %q", got)
	}
	if !strings.Contains(got, "author email") {
		t.Errorf("expected detail in output; got %q", got)
	}
	if !strings.Contains(got, "--force-unadmitted") {
		t.Errorf("expected override hint in output; got %q", got)
	}
}

// ---------------------------------------------------------------------------
// The report. `claim` is a one-shot report (docs/cli.md § Output): its result
// goes to stdout in the selected mode for EVERY outcome, and a refusal carries
// the code and the scope that were, until now, dropped at the boundary — so a
// driver running one claim per arena tells a lost race from a broken arena
// without parsing the rendered line (docs/cli.md § Claiming).
// ---------------------------------------------------------------------------

// jsonClaimEnv is newClaimEnv with the machine mode forced, so the report is
// the one a piped caller reads rather than the one a terminal does.
func newJSONClaimEnv(t *testing.T) *claimEnv {
	t.Helper()
	env := newClaimEnv(t)
	env.app.Output = OutputJSON
	return env
}

// decodeClaimReport reads the one object on stdout as a bare map, which is what
// pins the KEY SET: a decode into claimPayload would silently accept a renamed
// or missing key.
func decodeClaimReport(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\ngot: %q", err, out.String())
	}
	return m
}

// A claim that was taken reports itself, and classifies NOTHING: item_scoped is
// the scope of a stop, and a present false on a success would read as "this
// arena is the problem".
func TestCmdClaim_JSONReportsATakenClaim(t *testing.T) {
	env := newJSONClaimEnv(t)

	if code := env.app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("cmdClaim = %d, want 0; stderr=%q", code, env.err.String())
	}
	got := decodeClaimReport(t, env.out)
	if got["item"] != "1" {
		t.Errorf("item = %v, want 1", got["item"])
	}
	if got["claimed"] != true {
		t.Errorf("claimed = %v, want true", got["claimed"])
	}
	if got["account"] != "fake-account" {
		t.Errorf("account = %v, want fake-account", got["account"])
	}
	if _, present := got["item_scoped"]; present {
		t.Errorf("item_scoped is present on a claim that was taken: %v", got)
	}
}

// An ARENA-scoped refusal: every item would meet the same answer, and the
// caller must stop. The false has to be ON THE WIRE — omitted, it is
// indistinguishable from a refusal nobody classified, which is the same
// direction but not the same fact.
func TestCmdClaim_JSONReportsAnArenaScopedRefusal(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{
		Orchestrator: &refusingClaimBackend{
			Orchestrator: be,
			refusal: flow.ErrClaimRefused{
				Code:     "dirty-tree",
				Reason:   "worktree has uncommitted or untracked changes",
				Detail:   " M cli/cmd_claim.go\n?? scratch.txt",
				Check:    "clean-tree",
				Override: "force",
			},
		},
		Out: out, Err: errBuf, Output: OutputJSON,
	}

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), `"item_scoped": false`) {
		t.Errorf("a present false must serialise; got %s", out.String())
	}
	got := decodeClaimReport(t, out)
	if got["claimed"] != false {
		t.Errorf("claimed = %v, want false", got["claimed"])
	}
	if got["code"] != "dirty-tree" {
		t.Errorf("code = %v, want dirty-tree", got["code"])
	}
	if got["check"] != "clean-tree" {
		t.Errorf("check = %v, want clean-tree", got["check"])
	}
	if got["reason"] != "worktree has uncommitted or untracked changes" {
		t.Errorf("reason = %v", got["reason"])
	}
	// The failing check's own output, verbatim and unmodified — not the
	// two-space-indented rendering the human line wraps it in.
	if got["detail"] != " M cli/cmd_claim.go\n?? scratch.txt" {
		t.Errorf("detail = %q, want the porcelain verbatim", got["detail"])
	}
	// The flag's NAME, not the rendering of it. `--force` would make a consumer
	// strip dashes to get back to the value.
	if got["override"] != "force" {
		t.Errorf("override = %v, want force (no dashes)", got["override"])
	}
	// And the prose is still where an operator reads it.
	if !strings.Contains(errBuf.String(), `claim: refused — worktree has uncommitted`) {
		t.Errorf("stderr = %q, want the rendered refusal", errBuf.String())
	}
}

// An ITEM-scoped refusal: a different item might succeed. The codes that carry
// this are exactly the ones with no check name, which is why the check token in
// the rendered line could never have stood in for the classification.
func TestCmdClaim_JSONReportsAnItemScopedRefusal(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	out := &bytes.Buffer{}
	app := &App{
		Orchestrator: &refusingClaimBackend{
			Orchestrator: be,
			refusal: flow.ErrClaimRefused{
				Code:       "claim-race",
				ItemScoped: true,
				Reason:     "another arena took this item first",
			},
		},
		Out: out, Err: newDiscardWriter(), Output: OutputJSON,
	}

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := decodeClaimReport(t, out)
	if got["item_scoped"] != true {
		t.Errorf("item_scoped = %v, want true — a lost race is not a broken arena", got["item_scoped"])
	}
	if got["code"] != "claim-race" {
		t.Errorf("code = %v, want claim-race", got["code"])
	}
	for _, absent := range []string{"check", "override", "detail", "account"} {
		if _, present := got[absent]; present {
			t.Errorf("%s is present on a refusal that carries none: %v", absent, got)
		}
	}
}

// The refusal the SDK itself raises reaches the wire too, not only the
// backend's: `awaits-other-role` is item-scoped, so a fleet driver takes the
// next item rather than concluding the arena is broken.
func TestCmdClaim_JSONReportsTheRoleRefusalTheSDKRaises(t *testing.T) {
	be := fake.New()
	be.AddItem("1", awaitingMaintainer())
	be.SetCapabilities("", flow.CapPush)
	app, _ := handoffTestApp(t, be)
	out := &bytes.Buffer{}
	app.Out, app.Output = out, OutputJSON

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := decodeClaimReport(t, out)
	if got["code"] != "awaits-other-role" {
		t.Errorf("code = %v, want awaits-other-role", got["code"])
	}
	if got["item_scoped"] != true {
		t.Errorf("item_scoped = %v, want true — another item may await a role this run can take", got["item_scoped"])
	}
	if _, present := got["override"]; present {
		t.Errorf("override is present; no flag makes a role assumable: %v", got)
	}
}

// An awaited role the flow does not declare is a stop with no typed refusal
// behind it — so no `code` — but it is NOT unclassified: the item's own record
// is what is wrong, another item may be sound, and cmdResolve's auto-selection
// already moves on to the next ref for it
// (TestCmdResolve_AutoSelectSkipsAnItemWhoseAwaitedRoleIsUndeclared). Reported
// absent, a fleet driver obeying the documented "absent is false" would take
// the arena out of rotation over one corrupt marker — the same failure, moved
// outside the process.
func TestCmdClaim_JSONReportsAnUndeclaredRoleAsItemScoped(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1", Awaits: flow.Awaits{Role: "reviewer"}})
	be.SetCapabilities("", flow.CapPush)
	app, errBuf := handoffTestApp(t, be)
	out := &bytes.Buffer{}
	app.Out, app.Output = out, OutputJSON

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", code, errBuf.String())
	}
	got := decodeClaimReport(t, out)
	if got["claimed"] != false {
		t.Errorf("claimed = %v, want false", got["claimed"])
	}
	if !strings.Contains(got["reason"].(string), `role "reviewer" is not declared by this flow`) {
		t.Errorf("reason = %v, want the undeclared role named", got["reason"])
	}
	if _, present := got["code"]; present {
		t.Errorf("code is present on a stop that carries no typed refusal: %v", got)
	}
	if got["item_scoped"] != true {
		t.Errorf("item_scoped = %v, want true — a corrupt marker on one item is not a broken arena", got["item_scoped"])
	}
	// The command's name belongs to the human line, not to the report: a
	// consumer reads the reason, never the rendering of it.
	if strings.HasPrefix(got["reason"].(string), "claim:") {
		t.Errorf("reason = %q, carries the stderr prefix", got["reason"])
	}
	if !strings.HasPrefix(errBuf.String(), "claim: ") {
		t.Errorf("stderr = %q, want the command's name on the human line", errBuf.String())
	}
}

// An id nothing resolves stops before there is an item to name, so the report
// carries an empty `item` rather than the string that failed to address one.
func TestCmdClaim_JSONReportsAnUnresolvableId(t *testing.T) {
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{Orchestrator: refusingRefBackend{fake.New()}, Out: out, Err: errBuf, Output: OutputJSON}

	if code := app.cmdClaim(context.Background(), []string{"T9999"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := decodeClaimReport(t, out)
	if got["item"] != "" {
		t.Errorf("item = %v, want empty — nothing was resolved to name", got["item"])
	}
	if got["claimed"] != false {
		t.Errorf("claimed = %v, want false", got["claimed"])
	}
	// The reason VERBATIM — the backend's own account, with no "claim: " in
	// front of it. The command's name is the human line's rendering, and a
	// consumer that had to strip it would be parsing prose again.
	if got["reason"] != `no item named "T9999"` {
		t.Errorf("reason = %q, want the backend's account with no stderr prefix", got["reason"])
	}
	if !strings.HasPrefix(errBuf.String(), "claim: ") {
		t.Errorf("stderr = %q, want the command's name on the human line", errBuf.String())
	}
	if _, present := got["item_scoped"]; present {
		t.Errorf("item_scoped is present on a stop nothing classified: %v", got)
	}
}

// Human mode is unchanged on a refusal: prose on stderr, exit code as the
// signal, and STDOUT CARRIES NOTHING (docs/cli.md § Output). A failure payload
// on human stdout would put a refusal where a reader looks for a result.
func TestCmdClaim_HumanRefusalWritesNothingToStdout(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{
		Orchestrator: &refusingClaimBackend{
			Orchestrator: be,
			refusal:      flow.ErrClaimRefused{Code: "dirty-tree", Reason: "worktree is dirty", Check: "clean-tree"},
		},
		Out: out, Err: errBuf, Output: OutputHuman,
	}

	if code := app.cmdClaim(context.Background(), []string{"1"}); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing in human mode", out.String())
	}
	if !strings.Contains(errBuf.String(), `claim: refused — worktree is dirty (check "clean-tree")`) {
		t.Errorf("stderr = %q, want the rendered refusal", errBuf.String())
	}
}

// And human mode is unchanged on a success: the one line it always printed,
// byte for byte. The report is an addition to the machine channel, not a
// rewording of the operator's.
func TestCmdClaim_HumanSuccessLineIsUnchanged(t *testing.T) {
	env := newClaimEnv(t)
	env.app.Output = OutputHuman

	if code := env.app.cmdClaim(context.Background(), []string{"1"}); code != 0 {
		t.Fatalf("cmdClaim = %d, want 0; stderr=%q", code, env.err.String())
	}
	if got := env.out.String(); got != "claimed 1 as fake-account\n" {
		t.Errorf("stdout = %q, want the unchanged claim line", got)
	}
}

// Contradictory modes are a property of the INVOCATION, settled before the
// command acts: exit 2, nothing on stdout, and no lease asked for.
func TestCmdClaim_ContradictoryModesClaimNothing(t *testing.T) {
	be := fake.New()
	be.AddItem("1", flow.Item{Type: "task", Title: "1"})
	wrapped := &recordingClaimBackend{Orchestrator: be}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	app := &App{Orchestrator: wrapped, Out: out, Err: errBuf}

	if code := app.cmdClaim(context.Background(), []string{"1", "--json", "--human"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
	if !strings.Contains(errBuf.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want the mutual-exclusion refusal", errBuf.String())
	}
	if wrapped.claims != 0 {
		t.Errorf("Claim was called %d times; a malformed invocation takes no lease", wrapped.claims)
	}
}

// The mode is decided BEFORE the arity check. Answered the other way round,
// somebody who asked for both modes would be told they forgot the item id —
// and `claim --json --human` would report the flags as unrecognised, which is
// what TestUsage_NamesExactlyTheCommandsTakingOutputFlags reads as "this
// command has no output modes".
func TestCmdClaim_ContradictoryModesAreDecidedBeforeArity(t *testing.T) {
	errBuf := &bytes.Buffer{}
	app := &App{Orchestrator: fake.New(), Out: newDiscardWriter(), Err: errBuf}

	if code := app.cmdClaim(context.Background(), []string{"--json", "--human"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want the mutual-exclusion refusal", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "missing item id") {
		t.Errorf("stderr = %q, reports the wrong mistake", errBuf.String())
	}
}

// The key set is the machine contract. Both outcomes are pinned: what a taken
// claim carries, and what a refusal adds — the four things docs/cli.md
// § Claiming says a refusal carries, plus the scope.
func TestClaimPayload_KeySets(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload claimPayload
		want    []string
	}{
		{
			"taken",
			claimPayload{Item: "1", Claimed: true, Account: "fake-account"},
			[]string{"account", "claimed", "item"},
		},
		{
			"refused",
			refusedClaimPayload("1", flow.ErrClaimRefused{
				Code: "dirty-tree", Reason: "dirty", Detail: " M x", Check: "clean-tree", Override: "force",
			}),
			[]string{"check", "claimed", "code", "detail", "item", "item_scoped", "override", "reason"},
		},
		{
			// A refusal with nothing but a code and a reason drops the optional
			// keys and KEEPS item_scoped: the classification is never optional
			// on a refusal, only on a stop that carries no typed one.
			"refused, nothing optional",
			refusedClaimPayload("1", flow.ErrClaimRefused{Code: "claim-race", ItemScoped: true, Reason: "lost"}),
			[]string{"claimed", "code", "item", "item_scoped", "reason"},
		},
		{
			// A stop with no typed refusal behind it classifies nothing.
			"stopped, unclassified",
			claimPayload{Item: "1", Reason: "load item: the forge will not say"},
			[]string{"claimed", "item", "reason"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal %s: %v", b, err)
			}
			if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, c.want) {
				t.Errorf("keys = %v, want %v", got, c.want)
			}
		})
	}
}
