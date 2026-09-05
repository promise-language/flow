package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/promise-language/flow"
)

// ErrAgentOutsideStep is returned by an agent turn requested from anywhere but
// a step dispatch. Exported so a caller can tell this refusal — a program
// error, identical on every retry — from an agent or transport failure, which
// is not.
var ErrAgentOutsideStep = errors.New("agent turn requested outside a step handler")

// outsideStepAgent is what App.Agent becomes at startup validation. It answers
// Name() and refuses Run().
//
// The refusal is the runtime half of docs/agent.md § Nothing mechanical may
// spend. The static half — which step handlers may request a turn at all — is
// the agent-spend allowlist the commit gate enforces (tools/build/common/
// agentspend.go); this is what catches the same mistake at runtime, in a
// binary built from a tree that never went through that gate.
//
// It exists because the mistake is easy and quiet: App.Agent is right there on
// the struct, a command reaches it in one field access, and a turn spent by a
// mechanical command looks exactly like a turn spent by a step until the bill
// arrives. `doctor` did precisely this — a probe turn on every run, on every
// machine, forever.
//
// The real agent is not hidden, only unreachable by accident: newStepCtx
// unwraps it through agentImpl() when it builds the metered chokepoint a step
// handler is handed.
type outsideStepAgent struct{ inner flow.Agent }

func (a *outsideStepAgent) Name() string { return a.inner.Name() }

func (a *outsideStepAgent) Run(context.Context, flow.AgentRequest) (*flow.AgentResponse, error) {
	return nil, fmt.Errorf(
		"%w: ctx.Agent() inside a step handler is the ONLY route that may spend agent budget. "+
			"A turn requested anywhere else is billed with nobody having asked for work, against no budget, "+
			"and with no artifact to show for it — a mechanical command (doctor, list, status) runs on every "+
			"machine, in CI, and before every item, so a turn there is a standing charge that an operator "+
			"eventually silences by turning the command off. Check what you need without a turn, or do the "+
			"work in a step. See docs/agent.md",
		ErrAgentOutsideStep)
}

// agentImpl returns the agent a step dispatch may actually run: the one the
// binary supplied, unwrapped from the refusal above. Nothing outside
// newStepCtx has a reason to call it.
func (app *App) agentImpl() flow.Agent {
	if g, ok := app.Agent.(*outsideStepAgent); ok {
		return g.inner
	}
	return app.Agent
}
