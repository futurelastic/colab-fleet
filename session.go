package fleet

import "time"

// Timestamp is a point in time as stamped by one machine's own clock (§11).
// Every response carries the stamping machine's current clock reading
// alongside it (Fleet-Clock, api-http.md §1); callers compute skew rather
// than assume synchronisation. Durations computed by one machine (Since,
// silence timers) are internally consistent even when clocks disagree;
// comparing raw Timestamps across machines is not safe.
type Timestamp = time.Time

// SessionSpec is what a caller supplies to start a session (§2.1).
//
// Machine names the target host. The HTTP wire form
// (POST /v1/machines/{machine}/sessions, api-http.md §3.3) carries the same
// value positionally in the URL and does not repeat it in the request body
// — internal/service fills this field in from the path before handing a
// SessionSpec to a Driver. A Driver instance is already scoped to one
// (machine, runtime) pair (§4), so Machine reaching Driver.Create is
// redundant information a driver MAY use to assert consistency, never
// information it needs in order to route.
type SessionSpec struct {
	Machine MachineId    `json:"machine"`
	Runtime RuntimeId    `json:"runtime"`
	Cwd     AbsolutePath `json:"cwd"`

	// Agent, Model and Effort are hints, not guarantees (§2.1). A driver
	// that cannot honour one must say so at creation (via a "refused"-style
	// outcome or an error) rather than silently substituting a default —
	// see §4.3's DriverCapabilities.SupportsPin.
	Agent  AgentId `json:"agent,omitempty"`
	Model  string  `json:"model,omitempty"`
	Effort string  `json:"effort,omitempty"`

	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt,omitempty"`

	// Marker is a session-type stamp appended to the resolved name.
	//
	// It exists because the name is, on some substrates, the only channel
	// there is: it is what every listing, every remote client and every
	// human sees, and tooling that groups sessions by type has nothing else
	// to key on. A session created without one is not broken, it is
	// invisible to that tooling — which is a difference a caller could not
	// previously ask for OR detect.
	//
	// The driver carries a marker, never stacks it: a name that already
	// ends in one keeps the one it has. The service assigns no vocabulary
	// of its own — what a marker MEANS is the caller's business.
	Marker string `json:"marker,omitempty"`

	// RemoteControl requests that the session be reachable by remote
	// clients rather than only from a terminal on the machine running it.
	//
	// Nil means "do whatever a first-class session on this substrate gets",
	// which is what an unaware caller wants and, on the tmux driver, means
	// enabled. It is a tri-state on purpose: the zero value of a bool would
	// silently mean "off", and the entire defect this field closes is that
	// sessions created through the API were quietly second-class — a
	// caller could not ask for the difference and could not detect it
	// afterwards.
	//
	// Like Agent, Model and Effort this is a HINT. A driver on a substrate
	// with no such notion must say so at creation rather than accept the
	// request and produce a session that cannot be reached.
	RemoteControl *bool `json:"remoteControl,omitempty"`

	// ContextRef is a path, never inline content (§5.3). It must never
	// reach a command line — anything that matches processes by argv can
	// otherwise match and terminate a session whose prompt merely contains
	// the string being hunted for.
	ContextRef AbsolutePath `json:"contextRef,omitempty"`

	// TrustCwd carries the caller's consent to the runtime's own question
	// about Cwd — "is this a project you created or one you trust?" — so the
	// driver may answer it on the caller's behalf instead of leaving the new
	// session parked in front of it.
	//
	// # Why the consent is a field on the create request
	//
	// A runtime that asks this asks it BEFORE the session can do anything, and
	// it asks nobody in particular: on a fleet there is no human at that
	// terminal. Every client then meets the same wall — the create returns 201,
	// the session boots, and the work never starts. Measured on a live fleet: a
	// session parked on this question for two days, reading as merely
	// `waiting_input`, while the machine it was on had a supervisor watching.
	//
	// The decision is not the service's to take, and prompt.go says why at
	// length: a session service that decided what to answer would have become a
	// supervisor (§1). But it is not the service's to WITHHOLD either. The
	// caller already named this directory in this request — the same caller,
	// the same act, the same blast radius — and it is the only party in the
	// exchange with standing to say "yes, I trust it".
	//
	// So the consent travels WITH the directory it is about. It is scoped to
	// exactly one question (PromptFolderTrust) on exactly one session, the one
	// being created; it is never a standing permission, and it authorizes
	// nothing about the tool-permission or bypass screens, which ask something
	// else entirely and are deliberately not reachable this way.
	//
	// The zero value is the safe one: absent means the driver answers nothing,
	// which is what every existing caller already gets.
	//
	// # It is a HINT, like Agent and Model
	//
	// A runtime with no such question honours this by having nothing to do. A
	// driver that cannot answer prompts at all leaves the session as it found
	// it — the caller learns which it got by reading the session's state, where
	// an unanswered question is still reported in full.
	TrustCwd bool `json:"trustCwd,omitempty"`
}

// SessionRef addresses a session (§2.2). Ids are machine-scoped and
// potentially recyclable (§5.4, §7.1): never treat an id match alone as
// identity, and never act destructively on one without corroborating at
// least one independent attribute (working directory, start time, name).
//
// ID is scoped to (Machine, runtime) — not to Machine alone. See the
// package doc's findings list for the consequence this has for the
// single-session HTTP URL shape, which carries no runtime segment.
type SessionRef struct {
	Machine MachineId `json:"machine"`
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
}

// SessionRenamed reports that a session's id changed (§3's rename).
//
// Both ids are present and neither is optional. A subscriber holding the old
// one has to recognise the event as being about ITS session, which needs From;
// and it has to re-key, which needs To.
//
// StartedAt is the identity that did NOT change, and it is why a mutable id is
// safe to reason about at all: §5.4 already tells callers an id is not
// identity and must never be acted on alone. A rename is that rule with a
// sharper edge — the id can now change under a caller who was already
// forbidden from trusting it.
type SessionRenamed struct {
	Machine   MachineId  `json:"machine"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	StartedAt *Timestamp `json:"startedAt,omitempty"`
}

// Session is a SessionRef plus its current details and state — the shape
// GET /v1/machines/{machine}/sessions/{id} returns (api-http.md §3.3), and
// the item type of Collection[Session] returned by List (see
// internal/driver.Driver.List's doc comment for why List must return this
// shape, embedded state included, rather than a bare []SessionRef).
type Session struct {
	SessionRef

	Runtime RuntimeId    `json:"runtime"`
	Cwd     AbsolutePath `json:"cwd"`
	Agent   AgentId      `json:"agent,omitempty"`
	Model   string       `json:"model,omitempty"`

	// StartedAt is when this session began, as observed by the machine
	// running it. It is what a caller sends back in Request.Expect to make
	// a destroy corroborable (§5.4) — without it in the read, the caller
	// has nothing to quote, and the strong guarantee is unreachable.
	//
	// Nil means the driver does not know, which is a real answer: not every
	// substrate records it.
	StartedAt *Timestamp `json:"startedAt,omitempty"`

	// Attach is how a human's terminal reaches this session, if the driver
	// knows (§2.8). Nil means it does not — a real answer, and the one a
	// driver over a substrate with no interactive attachment must give.
	//
	// It is on Session rather than SessionRef because a ref is an address and
	// this is a capability: two sessions with the same shape of id may be
	// attachable by entirely different means.
	Attach *AttachHint `json:"attach,omitempty"`

	State SessionState `json:"state"`
}
