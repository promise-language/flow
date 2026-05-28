package flow

import "context"

// Telemetry is the optional progress-reporting boundary for a flow binary.
// When App.Telemetry is non-nil, the orchestrator forwards every
// StepCtx.Notify call to StepProgress so the surrounding system (a tracker
// UI, a journal, a status dashboard) can surface live sub-phase progress.
//
// IMPORTANT — Telemetry is NOT a liveness signal. StepProgress fires at
// sub-phase boundaries chosen by the handler ("running verify round 2",
// "capturing patch"). It is intentional that long-running steps emit no
// telemetry for minutes at a time while the agent works. The SDK and any
// downstream consumer MUST NOT derive "is this step still alive?" from
// StepProgress call density. Doing so reliably misclassifies healthy
// long-running steps as stalled.
//
// Step liveness is observed at three independent layers:
//
//  1. The agent. Agent.Run blocks for the duration of one real turn; if
//     claude / the remote runner dies, the Agent impl surfaces it via
//     AgentResponse.Failure (with Failure.Transient set for infra causes).
//  2. The runner. A flow binary running in a runner sandbox is a child
//     process; the runner's process-watchdog catches binary crashes and
//     clears the lease.
//  3. The backend / dispatching server. The dispatcher observes substrate-
//     specific signals (e.g. tracker watches claude's stdout for activity)
//     and fails turns that go silent past a configured threshold.
//
// Each layer covers a failure mode the others cannot see. Do not cross-
// wire them by treating Notify density as a liveness proxy.
type Telemetry interface {
	// StepProgress reports a sub-phase progress event for the given claim.
	// step is the lifecycle item name (e.g. "implementation"); detail is a
	// short human-readable phrase ("running verify round 2", "capturing
	// patch", or "" for "just starting").
	StepProgress(ctx context.Context, claim Claim, step, detail string)
}
