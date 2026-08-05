package fleet

// AttachHint describes how a human's terminal can be put in front of a
// session (§2.8).
//
// # Why this exists at all, and why it took a migration plan to notice
//
// The point of this service is that a supervisor can stop knowing what tmux
// is. Everything else got there — state, input, prompts, lifecycle, events —
// and then one job was left holding the substrate: a human wanting to watch or
// take over a session. A supervisor that must run `tmux attach` has not
// actually been freed of tmux; it has been freed of it everywhere except the
// one place its users touch.
//
// So this is not a convenience field. It is the difference between a driver
// boundary and a leak.
//
// # Why a hint rather than an operation
//
// There is no `attach` in §3, deliberately. Attaching gives a terminal to a
// PERSON, and a person is not on the other end of this API — an HTTP request
// is. A service that "attached" could only mean "attached something of its
// own", which is either useless or an impersonation.
//
// What a client actually needs is the local invocation, so it can compose the
// remoteness itself. That composition is the client's business and not ours,
// for the same reason §7.2 says a peer's address is one the operator confirmed
// rather than the peer's own idea of its name: **this machine does not know
// how you reach it.** It knows how a terminal already on it would attach.
//
// # Why the argv rather than a shell string
//
// A command string invites a caller to interpolate a session id into a shell,
// and session ids here contain emoji, spaces and anything else an operator
// typed. Argv is exec-able as-is, and never has quoting rules.
type AttachHint struct {
	// Kind names the mechanism in the abstract, so a client can decide
	// whether it understands the hint before reading the rest. "multiplexer"
	// is the only value today; unknown kinds must be treated as unsupported
	// rather than guessed at (§5.6).
	Kind string `json:"kind"`

	// Target is the substrate-native handle — for a multiplexer, the session
	// name. Exposed so a client can display it and recognise the same session
	// in a tool that predates this service, which every adopting supervisor
	// has.
	Target string `json:"target,omitempty"`

	// Command is the argv to run ON THIS SESSION'S MACHINE to attach an
	// interactive terminal. Empty means the driver has no answer, which is a
	// real answer and not an error (§5.7).
	Command []string `json:"command,omitempty"`

	// ReadOnly is the same attachment without the ability to type into it —
	// what a supervisor should offer for "watch" as distinct from "take
	// over". Attaching read-write to somebody's live session is a way to
	// corrupt it by leaning on a keyboard, and a client that cannot tell the
	// two apart will offer the dangerous one.
	ReadOnly []string `json:"readOnly,omitempty"`

	// Shared reports that attaching does not evict any other viewer. False
	// means attaching may take the session away from whoever has it, which a
	// supervisor must be able to warn about before doing it on a human's
	// behalf.
	Shared bool `json:"shared,omitempty"`
}
