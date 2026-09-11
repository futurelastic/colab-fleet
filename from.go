package fleet

// MessageFrom is the optional `from` object on a send (POST …/input,
// colab-fleet #158): who a session-to-session message says it comes from.
// Nil, or absent on the wire, means unlabelled — the behaviour before #158.
//
// # What a receiver may believe, field by field
//
// Agent, Session and RelayOfHuman are the CALLER'S OWN STATEMENT. Under a
// single shared token nothing distinguishes one bearer from another (see
// Caller.Principal: provenance, not identity), so a service can carry these
// claims but cannot verify them. A label built from them must never read as
// if it had been checked.
//
// RelayOfHuman is a label, never authority. It adds one line of text saying
// the sender states it is relaying an instruction from the human operator; it
// must not change what the receiver is allowed to do, and no grant, policy or
// routing decision anywhere in this service reads it. If it ever unlocked
// anything, it would be exactly the laundering path the receiving runtime's
// own advisory on peer messages warns about. Making the statement verifiable
// would need a credential only a human holds — a separate design.
//
// Machine is NOT the caller's to set. A service stamps it with the machine
// where the request entered the fleet and ignores any value a client sends.
// It appears on the wire only on a relayed hop, where the entering machine
// forwards its own stamp to the peer that owns the session, and the peer
// accepts it only from a relayed request naming one of its configured peers.
// When that cannot be established the machine is omitted, never guessed.
type MessageFrom struct {
	Agent        string    `json:"agent,omitempty"`
	Session      string    `json:"session,omitempty"`
	RelayOfHuman bool      `json:"relayOfHuman,omitempty"`
	Machine      MachineId `json:"machine,omitempty"`
}
