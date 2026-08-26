package fleet

// MachineInfo is one entry of GET /v1/machines (api-http.md §3.1).
type MachineInfo struct {
	Machine    MachineId   `json:"machine"`
	Self       bool        `json:"self"`
	Status     SourceState `json:"status"`
	ObservedAt Timestamp   `json:"observedAt"`
	// Build identifies the code this machine is running — colab-fleet #121.
	// For self it is always known (fleet.SelfBuild(), read once at startup).
	// For a peer it is whatever the last successful probe learned; absent a
	// probe yet, or a peer driver that cannot report one, this reads as the
	// zero value (Known: false) — never a plausible-looking default. See
	// fleet.Build's own doc comment for why an unknown build must never
	// compare equal to anything, including itself.
	Build Build `json:"build"`
	// MaxInputBytes is the effective limit this machine enforces on
	// `prompt` (create) and `text` (input) — colab-fleet #130, the same
	// ask-do-not-infer move #121 made for Build above: a caller sizing a
	// dispatch brief should be able to ask rather than discover the
	// boundary by exceeding it, and that matters more once the value is
	// machine-local and can differ across the fleet.
	//
	// For self this is always known and positive — every deployment has an
	// effective limit, configured or defaulted (cmd/colab-fleetd/config.go,
	// internal/service.Service.MaxInputBytes). For a peer it is whatever
	// the last successful probe learned; absent a probe yet, or a peer
	// driver that cannot report one, this reads as the zero value.
	//
	// Unlike Build, no separate Known flag is needed to tell "unknown" from
	// "a real answer": a real effective limit is never zero (SetMaxInputBytes
	// refuses non-positive values), so zero is unambiguous on its own —
	// §5.7's "absence and failure are different answers" holds here by
	// construction rather than by an extra field.
	MaxInputBytes int `json:"maxInputBytes,omitempty"`
}

// RuntimeInfo is one entry of GET /v1/runtimes (api-http.md §3.1).
type RuntimeInfo struct {
	Machine      MachineId          `json:"machine"`
	Runtime      RuntimeId          `json:"runtime"`
	Capabilities DriverCapabilities `json:"capabilities"`
}
