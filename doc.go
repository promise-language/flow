// Package flow is the open-source Go SDK for declarative, stateless-per-step
// automation against task-tracking systems.
//
// Flow authors register artifacts (handler-produced durable item state) and
// signals (backend-observed durable boolean state), declare one or more flows
// as ordered lists of steps, and embed the SDK's CLI via cli.Run.
//
// The SDK is backend-pluggable; the reference backend is pkg/orchestrator/github,
// which stores state in GitHub Issues. See docs/design.md for the full
// architecture.
package flow
