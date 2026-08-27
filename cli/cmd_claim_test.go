package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// refResolverBackend wraps the fake backend with a flow.RefResolver fast path
// and records whether (and with what id) it was consulted.
type refResolverBackend struct {
	*fake.Backend
	called bool
	gotID  string
}

func (r *refResolverBackend) ResolveRef(ctx context.Context, id string) (flow.ItemRef, error) {
	r.called = true
	r.gotID = id
	return flow.ItemRef{BackendName: "fake", Display: id, Ref: json.RawMessage(`"` + id + `"`)}, nil
}

// When the backend implements RefResolver, resolveClaimRef uses it directly and
// never calls ListEligible — proven here by adding NO items to the fake yet
// still resolving a ref.
func TestResolveClaimRef_UsesRefResolver(t *testing.T) {
	be := &refResolverBackend{Backend: fake.New()}
	app := &App{Backend: be}

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
	be.AddItem(flow.Item{ID: "T0435", Type: "task", Title: "T0435"})
	app := &App{Backend: be}

	ref, err := app.resolveClaimRef(context.Background(), "T0435")
	if err != nil {
		t.Fatalf("resolveClaimRef: %v", err)
	}
	if ref.Display != "T0435" {
		t.Errorf("ref.Display = %q, want T0435", ref.Display)
	}
}

// The fallback path errors when no eligible item matches the id.
func TestResolveClaimRef_FallbackNoMatch(t *testing.T) {
	be := fake.New()
	app := &App{Backend: be}

	if _, err := app.resolveClaimRef(context.Background(), "T9999"); err == nil {
		t.Error("expected error when no eligible item matches")
	}
}

// recordingClaimBackend captures the `force` argument passed to Claim so the
// flag-after-positional test can assert that the bool flag is actually
// parsed (and not silently dropped).
type recordingClaimBackend struct {
	*fake.Backend
	lastForce bool
}

func (r *recordingClaimBackend) Claim(ctx context.Context, ref flow.ItemRef, owner string, force bool) (flow.Claim, error) {
	r.lastForce = force
	return r.Backend.Claim(ctx, ref, owner, force)
}

// T0484: `claim <id> --force` (bool flag after the positional) must parse and
// reach the backend as force=true. Prior to the interspersed-parsing fix the
// stdlib flag package stopped at the positional and reported --force as an
// "unexpected argument".
func TestCmdClaim_ForceAfterPositional(t *testing.T) {
	be := fake.New()
	be.AddItem(flow.Item{ID: "T0001", Type: "task", Title: "T0001"})
	wrapped := &recordingClaimBackend{Backend: be}

	app := &App{
		Backend: wrapped,
		Owner:   "alice",
		Out:     newDiscardWriter(),
		Err:     newDiscardWriter(),
	}

	code := app.cmdClaim(context.Background(), []string{"T0001", "--force"})
	if code != 0 {
		t.Fatalf("cmdClaim = %d, want 0", code)
	}
	if !wrapped.lastForce {
		t.Error("Backend.Claim received force=false, want force=true")
	}
}
