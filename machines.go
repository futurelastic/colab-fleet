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
	// Peer is this service's own standing on that machine — whether it lists
	// this service back and what it grants the credential this service
	// presents there (colab-fleet #154). Federation is two hand-kept halves
	// of configuration, and before this the only way to learn they disagreed
	// was a 403 at the moment of need.
	//
	// Always present. See PeerStanding for how "not listed" and "nobody
	// could tell" stay different answers.
	Peer PeerStanding `json:"peer"`
}

// PeerStanding is this service's registration on one machine, as that
// machine itself reported it (colab-fleet #154).
//
// It is gathered on the peer probe that already learns build and
// maxInputBytes, by asking the peer's whoami about the credential this
// service presents there. Three answers must not collapse into each other:
//
//   - observed, listsMeBack false — the peer answered and does not list us.
//   - observed, grantsToMe [] — the peer answered and our credential holds
//     nothing there, or matches no principal at all. A real negative.
//   - assumed — nobody could tell: the peer was never reached, answered too
//     long ago (the same staleness bound as capabilities), runs a build that
//     predates this read, or this service presents no credential of its own
//     to it. listsMeBack is null and grantsToMe is [] — a floor, exactly as an
//     unreached peer's capabilities are, never a claim of absence.
type PeerStanding struct {
	// ListsMeBack is whether that machine's own peer roster names this
	// service. Null when it did not say — including a peer credential without
	// `read` there, since the roster is what `read` guards.
	ListsMeBack *bool `json:"listsMeBack"`
	// GrantsToMe is every grant that machine attaches to the credential this
	// service presents to it. Never another principal's.
	GrantsToMe []string `json:"grantsToMe"`
	// Source is CapabilitySource's provenance, reused for the same fact.
	Source CapabilitySource `json:"source"`
	// ObservedAt is when the answer arrived, on this machine's clock. Absent
	// under "assumed".
	ObservedAt *Timestamp `json:"observedAt,omitempty"`
}

// AssumedPeerStanding is the conservative floor: nothing confirmed.
func AssumedPeerStanding() PeerStanding {
	return PeerStanding{GrantsToMe: []string{}, Source: CapabilitiesAssumed}
}

// RuntimeInfo is one entry of GET /v1/runtimes (api-http.md §3.1).
type RuntimeInfo struct {
	Machine      MachineId          `json:"machine"`
	Runtime      RuntimeId          `json:"runtime"`
	Capabilities DriverCapabilities `json:"capabilities"`
}
