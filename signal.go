package flow

import "time"

// SignalId names a backend-observed durable boolean. Independent namespace
// from ArtifactId.
type SignalId string

// SignalDef — centralized declaration in App.Signals. Signals are always
// boolean state; no value-type discriminator.
type SignalDef struct {
	Id          SignalId
	Description string // for docs / UI / startup error messages; not load-bearing
}

// Signal returns a SignalDef. Convenience constructor.
func Signal(id SignalId, description string) SignalDef {
	return SignalDef{Id: id, Description: description}
}

// SignalState — per-item record of whether a signal is set, when observed,
// and by whom.
type SignalState struct {
	Set        bool
	ObservedAt time.Time
	By         string // backend-specific principal; display-only
}
