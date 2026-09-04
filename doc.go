// Package flow is the open-source Go SDK for declarative, stateless-per-step
// automation against task-tracking systems.
//
// Flow authors register artifacts (handler-produced durable item state) and
// signals (orchestrator-observed durable boolean state), declare one or more
// flows as ordered lists of steps, and embed the SDK's CLI via cli.Run.
//
// The SDK talks to an Orchestrator, and which one is pluggable; the reference
// implementation is pkg/orchestrator/github, which keeps its state on GitHub
// Issues. Where an orchestrator stores anything is its own business — a store
// is something an orchestrator uses, not something it is. See
// docs/orchestrator.md for the boundary.
package flow
