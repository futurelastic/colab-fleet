package fleet

// ClosedSession is the record a service keeps of one session after it ended
// — a tombstone, read through GET /v1/sessions/closed (api-http.md §3.2).
//
// # Why a service keeps these at all
//
// The live list answers "what is running"; the event feed answers "what
// changed while I was watching". Neither answers "which sessions ran here in
// the last N days, and when did each end" — the feed's window is in memory,
// is not persisted, and only advances while something is subscribed. A
// consumer that wants that answer would otherwise have to keep its own copy
// of session state, which is exactly the second copy of the truth this
// service exists to make unnecessary.
//
// This is deliberately much smaller than a persisted event window: one record
// per ended session, kept for a bounded retention period, carrying only the
// metadata the live record already carried. Content is never included.
//
// # closedAt is a bound, and ClosedBy says which kind
//
// A session closed through this service ends at a moment the service
// observed, and closedAt is that moment. A session that ended on its own —
// the process exited, a human killed it from a terminal — is learned about
// the next time the service reads its machine completely: from a live event
// stream, a listing, or its own start-up sweep. Its closedAt is when that
// absence was OBSERVED, and the true end lies somewhere after LastSeenAt and
// no later than closedAt. The two are reported side by side rather than one
// being presented as the other (§5.2: inference is never presented as
// observation).
type ClosedSession struct {
	SessionRef

	Runtime RuntimeId    `json:"runtime,omitempty"`
	Cwd     AbsolutePath `json:"cwd,omitempty"`

	// StartedAt is the same field the live record carries (§5.4's identity
	// pairing with ID), copied from the last sighting. Absent when the
	// runtime never reported it.
	StartedAt *Timestamp `json:"startedAt,omitempty"`

	// Conversation is the runtime's own conversation record as last seen,
	// present only when it was known (ConversationRef.Known).
	Conversation *ConversationRef `json:"conversation,omitempty"`

	// LastSeenAt is the last time this service recorded the session as
	// present. For a session ClosedBy absent, the end lies after it.
	LastSeenAt Timestamp `json:"lastSeenAt"`

	// ClosedAt is when the end was observed — exact for ClosedByClose, an
	// upper bound for ClosedByAbsent.
	ClosedAt Timestamp `json:"closedAt"`

	ClosedBy ClosureKind `json:"closedBy"`

	// Evidence says, in a sentence, how the end was learned.
	Evidence string `json:"evidence"`
}

// ClosureKind says how a service learned a session ended.
type ClosureKind string

const (
	// ClosedByClose: a close request through this service was accepted by
	// the driver. ClosedAt is the moment it was accepted.
	ClosedByClose ClosureKind = "close"
	// ClosedByAbsent: the session was missing from a complete read of its
	// runtime (or its id now names a session that started later). ClosedAt
	// is when the absence was observed; the end lies after LastSeenAt.
	ClosedByAbsent ClosureKind = "absent"
)
