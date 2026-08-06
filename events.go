package fleet

// EventKind is the closed set of SSE event names (api-http.md §4).
type EventKind string

const (
	EventSessionCreated EventKind = "session.created"
	EventSessionState   EventKind = "session.state"
	EventSessionClosed  EventKind = "session.closed"
	// EventSessionRenamed carries a session whose ID has CHANGED. A subscriber
	// filtering by id must re-key on this or it silently stops matching a
	// session that is still very much alive — which is why a rename cannot be
	// a quiet mutation. Without the event, a rename is indistinguishable from
	// a disappearance, and that is the one thing it must not be mistaken for.
	EventSessionRenamed EventKind = "session.renamed"
	EventSourceStatus   EventKind = "source.status"
	// EventMachineQuota reports that this machine's ACCOUNT started or
	// stopped refusing work — a fact about the machine, not about any one
	// session, and the only event here whose subject is not a session.
	//
	// It exists because of how a supervisor learned this without it. When an
	// account hit its weekly limit, 48 autopilot sessions each discovered it
	// separately, by being told to work and stalling; the supervisor recorded
	// 48 stall reasons and never formed the single conclusion that explained
	// all of them. Every one of those discoveries was a session that had
	// already been dispatched — the cost is paid before the fact is learned.
	//
	// One event, at the transition, lets a supervisor stop dispatching
	// instead of finding out N times that it should have. It is the earliest
	// honest signal available: the runtime gives no advance warning, so the
	// first refusal IS the notice (see the package findings on what was
	// searched for and not found).
	EventMachineQuota  EventKind = "machine.quota"
	EventControlResync EventKind = "control.resync"
)

// ResyncReason is control.resync's payload discriminant (api-http.md §4,
// session-abstraction.md §7.3).
type ResyncReason string

const (
	ResyncEpochChanged  ResyncReason = "epoch_changed"
	ResyncCursorExpired ResyncReason = "cursor_expired"
)

// SessionStatePayload is session.state's and session.closed's payload shape
// (api-http.md §4: "ref + SessionState", "ref + final state").
type SessionStatePayload struct {
	Ref   SessionRef   `json:"ref"`
	State SessionState `json:"state"`
}

// MachineQuotaPayload is machine.quota's payload.
//
// Blocked is explicit rather than implied by Quota being nil, so a recovery
// event is a positive statement ("this account works again") and not an
// absence a subscriber has to interpret — §5.7's rule applied to the event
// plane.
type MachineQuotaPayload struct {
	Machine MachineId   `json:"machine"`
	Blocked bool        `json:"blocked"`
	Quota   *QuotaBlock `json:"quota,omitempty"`
}

// ControlResyncPayload is control.resync's payload.
type ControlResyncPayload struct {
	Reason ResyncReason `json:"reason"`
}

// Event is one entry on the stream subscribe() opens (§3, §7.3). Every
// event carries a monotonic Cursor and the Epoch of the service instance
// that assigned it (§7.3) — a subscriber reconnecting with a stale Cursor
// or a changed Epoch gets EventControlResync instead of a silent gap.
//
// Payload's Go type varies by Kind:
//   - EventSessionCreated -> Session
//   - EventSessionState, EventSessionClosed -> SessionStatePayload
//   - EventSourceStatus -> SourceStatus
//   - EventMachineQuota -> MachineQuotaPayload
//   - EventControlResync -> ControlResyncPayload
//
// The spec does not pin down the exact SSE line framing — whether Kind
// travels as the SSE "event:" field, a JSON "kind" property inside "data:",
// or both (see the package doc's findings list). This type is the
// Go-native shape a driver or the service works with in memory;
// internal/service would own translating it to and from the wire if this
// skeleton implemented streaming, which it does not (see NewMux's
// /v1/events handler).
type Event struct {
	Cursor  int64     `json:"cursor"`
	Epoch   string    `json:"epoch"`
	Machine MachineId `json:"machine"`
	Kind    EventKind `json:"kind"`
	Payload any       `json:"payload"`

	// Origin carries the sequence coordinates the ORIGINATING service gave
	// this event, when it reached the caller through a proxy. Nil for
	// events a service produced itself.
	//
	// Cursor and Epoch above always belong to the service the caller is
	// talking to, so "resume from cursor N" is never ambiguous about whose
	// N. Origin preserves what the peer said about its own sequence, so a
	// caller that later talks to that peer directly can resume there
	// instead of refetching.
	//
	// This is the same split §13.2 uses for source status and F20 for error
	// kinds: adopt what the peer said about itself, add only what the
	// relaying service is uniquely positioned to know — here, the ordering
	// of this event against everything else the caller is receiving.
	//
	// The originating machine is Event.Machine and is not repeated here.
	Origin *EventOrigin `json:"origin,omitempty"`
}

// EventOrigin is a relayed event's coordinates in its originating service's
// own sequence (§7.3, §13).
type EventOrigin struct {
	Cursor int64  `json:"cursor"`
	Epoch  string `json:"epoch"`
}
