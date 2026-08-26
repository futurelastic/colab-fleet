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
}

// RuntimeInfo is one entry of GET /v1/runtimes (api-http.md §3.1).
type RuntimeInfo struct {
	Machine      MachineId          `json:"machine"`
	Runtime      RuntimeId          `json:"runtime"`
	Capabilities DriverCapabilities `json:"capabilities"`
}
