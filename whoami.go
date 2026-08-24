package fleet

// GrantReport answers GET /v1/whoami (api-http.md §5, colab-fleet #106): what
// the presented credential is authorized to do, on the machine named.
//
// It exists because every other way to learn this is trial and refusal:
// attempt the call, read whichever precondition failed first, fix it, and
// discover the next one on the next attempt. That cost doubles for a relayed
// operation, which needs one grant on the machine receiving the call and a
// second on the machine performing it (session-abstraction.md §7.7) — this
// route answers the first directly and is honest about why it cannot answer
// the second the same way.
type GrantReport struct {
	// Principal is the caller's own resolved identity, exactly as it
	// appears in the audit trail (Caller.Principal). Never another
	// principal's — this route reports on the credential that presented it,
	// nothing else configured on this machine.
	Principal string `json:"principal"`
	// Machine is the machine this report describes: the one that answered,
	// when Source is "observed"; the one named and never reached, when
	// Source is "assumed".
	Machine MachineId `json:"machine"`
	// Grants is every verb this credential currently holds on Machine. An
	// empty list is a real answer, not an omission — "this credential holds
	// nothing here" is exactly what a principal with no grants needs to be
	// able to learn about itself.
	Grants []string `json:"grants"`
	// Source is CapabilitySource's provenance shape, reused rather than
	// reinvented for a second domain: "observed" means Machine's own table
	// was consulted directly, just now, to produce Grants. "assumed" means
	// Machine names a peer this service has no mechanism to ask — grants are
	// per-machine configuration, not a runtime fact this service probes and
	// caches the way it does DriverCapabilities, so Grants is always empty
	// under "assumed" and must be read the same way an unreached peer's
	// capabilities are: a conservative floor, never that peer's real answer.
	Source CapabilitySource `json:"source"`
}
