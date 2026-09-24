package fleet

// DeliveryLane is which delivery lane a session's input is taking (#185).
//
// It exists because two things a caller cares about are otherwise invisible:
// whether an optional external delivery module is carrying this session's
// input, and — when it is not — why not. A caller never has to read it to
// send: /input works the same either way. It is for an operator asking "is the
// module I enabled actually wired to this session?".
//
// # Absent is not "terminal"
//
// The field is nil when nothing has been configured or nothing has been probed
// for this session: a machine with no module enabled, a session this machine
// did not launch itself (an adopted or foreign one stays on the built-in lane
// by construction), or a peer built before the field existed. A consumer reads
// nil as "not stated", never as "terminal" — the same rule Session.Attach and
// DeliveryReceipt.Delivery follow.
type DeliveryLane struct {
	// Lane is the delivery module's name while that module reports the
	// session's lane live, and "terminal" otherwise — including while the
	// module is unavailable, unhealthy, or has not yet answered the session's
	// first attach. It is the lane an `auto` send would take right now.
	Lane string `json:"lane"`

	// ClientConnected is true only while a module holds a live channel to this
	// session's agent process. False alongside Lane "terminal" is the ordinary
	// fallback; false alongside a module name never occurs.
	ClientConnected bool `json:"clientConnected"`

	// Evidence is prose for humans: what this answer rests on (the module's
	// last attach result, or the reason there is no lane). Do not parse it.
	Evidence string `json:"evidence"`

	// Since is when Lane last changed value.
	Since Timestamp `json:"since"`
}

// DeliveryLaneTerminal is DeliveryLane.Lane's value for the built-in path.
const DeliveryLaneTerminal = "terminal"

// Values of DeliveryModuleStatus.Status.
const (
	// DeliveryModuleStarting: the helper has been spawned (or is about to be)
	// and has not yet completed its handshake.
	DeliveryModuleStarting = "starting"
	// DeliveryModuleAvailable: the handshake succeeded and the last health
	// probe was good. Sessions may be offered lanes.
	DeliveryModuleAvailable = "available"
	// DeliveryModuleUnavailable: the helper exited, broke its pipe, missed a
	// deadline or failed health. Every session falls back to the built-in
	// path until it comes back; the service restarts it with backoff.
	DeliveryModuleUnavailable = "unavailable"
	// DeliveryModuleDisabled: the helper was refused — an unsupported
	// protocol, a missing operation, a malformed handshake. It stays off until
	// the service restarts.
	DeliveryModuleDisabled = "disabled"
)

// DeliveryModuleStatus is the wiring of one enabled delivery module, as one
// machine's driver reports it on GET /v1/runtimes (#185).
type DeliveryModuleStatus struct {
	// Name is the module's name: the file name in the modules directory, and
	// the value a caller writes in a request's `route` to force it.
	Name string `json:"name"`

	// Status is one of the DeliveryModule* values.
	Status string `json:"status"`

	// Reason says why Status is not "available"; empty when it is.
	Reason string `json:"reason,omitempty"`

	// Protocol, Version and Platform are what the module reported about
	// itself. Zero/empty means it has not completed a handshake.
	Protocol int    `json:"protocol,omitempty"`
	Version  string `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`

	// PeerCheck is the module's own statement that it can verify who is on the
	// other end of its channel. A module reporting false is treated as not
	// live for every session (#185, the maintainer's ruling): nothing is ever
	// delivered without that check. Nil means the module has not said.
	PeerCheck *bool `json:"peerCheck,omitempty"`

	// Lanes counts sessions by lane state (prepared, connecting, live,
	// degraded, gone). Absent when the module holds no lanes.
	Lanes map[string]int `json:"lanes,omitempty"`
}
