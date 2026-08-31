package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promise-language/flow"
	"github.com/promise-language/flow/pkg/backend/fake"
)

// reseedTestSetup builds an App with an active claim and a seeded artifact,
// following the grantTestSetup pattern.
func reseedTestSetup(t *testing.T) (*App, *fake.Backend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	a := &stubAgent{name: "stub"}
	app, be, claim := testApp(t, func(f *flow.Flow) {
		f.AddStep("write plan", "plan", func(ctx flow.StepCtx) error {
			return ctx.ResolveMarkdown("the plan")
		}, flow.StepConfig{})
	}, a)

	if err := be.SeedState(context.Background(), claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown},
	}); err != nil {
		t.Fatalf("SeedState: %v", err)
	}

	var out, errBuf bytes.Buffer
	app.Out = &out
	app.Err = &errBuf
	return app, be, &out, &errBuf
}

func TestCmdReseed_NoForce_RefusesWithPreview(t *testing.T) {
	app, _, _, errBuf := reseedTestSetup(t)

	code := app.cmdReseed(context.Background(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"would discard", "--force"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("stderr = %q, want %q", errBuf.String(), want)
		}
	}
}

func TestCmdReseed_Force_ClearsState(t *testing.T) {
	app, be, out, errBuf := reseedTestSetup(t)

	code := app.cmdReseed(context.Background(), []string{"--force"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "reseeded") {
		t.Errorf("stdout = %q, want 'reseeded'", out.String())
	}

	// Prove the seed was cleared: SeedState should succeed again.
	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if err := be.SeedState(context.Background(), *claim, []flow.ArtifactSpec{
		{Id: "plan", Type: flow.ArtifactMarkdown},
	}); err != nil {
		t.Fatalf("SeedState after reseed should succeed, got: %v", err)
	}
}

func TestCmdReseed_NoClaim(t *testing.T) {
	app, be, _, errBuf := reseedTestSetup(t)

	claim, err := be.LookupActiveClaim(context.Background(), "alice")
	if err != nil {
		t.Fatalf("LookupActiveClaim: %v", err)
	}
	if err := be.Release(context.Background(), *claim); err != nil {
		t.Fatalf("Release: %v", err)
	}

	code := app.cmdReseed(context.Background(), []string{"--force"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no active claim") {
		t.Errorf("stderr = %q, want 'no active claim'", errBuf.String())
	}
}

// unsupportedReseedBackend wraps a real backend but overrides ResetSeed to
// return ErrResetSeedUnsupported.
type unsupportedReseedBackend struct {
	*fake.Backend
}

func (b *unsupportedReseedBackend) ResetSeed(ctx context.Context, claim flow.Claim) error {
	return flow.ErrResetSeedUnsupported
}

func TestCmdReseed_UnsupportedBackend(t *testing.T) {
	app, _, _, errBuf := reseedTestSetup(t)
	app.Backend = &unsupportedReseedBackend{app.Backend.(*fake.Backend)}

	code := app.cmdReseed(context.Background(), []string{"--force"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "does not support reseed") {
		t.Errorf("stderr = %q, want 'does not support reseed'", errBuf.String())
	}
}

// errorReseedBackend wraps a real backend but overrides ResetSeed to return a
// generic error.
type errorReseedBackend struct {
	*fake.Backend
}

func (b *errorReseedBackend) ResetSeed(ctx context.Context, claim flow.Claim) error {
	return errors.New("kaboom: storage unavailable")
}

func TestCmdReseed_BackendError(t *testing.T) {
	app, _, _, errBuf := reseedTestSetup(t)
	app.Backend = &errorReseedBackend{app.Backend.(*fake.Backend)}

	code := app.cmdReseed(context.Background(), []string{"--force"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "kaboom: storage unavailable") {
		t.Errorf("stderr = %q, want error text", errBuf.String())
	}
}

func TestCmdReseed_UnexpectedArg(t *testing.T) {
	app, _, _, errBuf := reseedTestSetup(t)

	code := app.cmdReseed(context.Background(), []string{"42"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Errorf("stderr = %q, want 'unexpected argument'", errBuf.String())
	}
}
