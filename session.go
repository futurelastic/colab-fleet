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

	// ContextRef is a path, never inline content (§5.3). It must never
	// reach a command line — anything that matches processes by argv can
	// otherwise match and terminate a session whose prompt merely contains
	// the string being hunted for.
	ContextRef AbsolutePath `json:"contextRef,omitempty"`
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

	State SessionState `json:"state"`
}
