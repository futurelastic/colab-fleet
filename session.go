package fleet

import "time"

// Timestamp is a point in time as stamped by one machine's own clock (§11).
// Every response carries the stamping machine's current clock reading
// alongside it (Fleet-Clock, api-http.md §1); callers compute skew rather
// than assume synchronisation. Durations computed by one machine (Since,
// silence timers) are internally consistent even when clocks disagree;
// comparing raw Timestamps across machines is not safe.
type Timestamp = time.Time

// PermissionModeBypass is the one non-default permission posture this fleet's
// runtime has: the agent stops asking before it acts.
//
// A closed set of one, named rather than left as a free string, so an
// unrecognised mode is refused at the boundary instead of being handed to a CLI
// that may or may not understand it — §2.3's rule for statuses, applied to an
// input a caller controls.
const PermissionModeBypass = "bypass"

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

	// Env is variables the session's process must carry.
	//
	// # Why a session needs any, and why the API not having them was disqualifying
	//
	// An agent inside a session has to be able to identify itself to the tooling
	// around it — which session am I, where is the context staged for me, which
	// prior conversation am I re-attaching to. A supervisor answers that by
	// exporting variables into the session at creation. Without a field for it,
	// a session created through this API is a different kind of session from one
	// the supervisor starts itself, which is the whole class of defect §2.1's
	// first-class-session work exists to close.
	//
	// # Values never reach a command line (§5.3)
	//
	// The obvious mechanism — the multiplexer's own per-session `-e NAME=value`
	// flag — puts every value in an argv that any process on the machine can
	// read. That is precisely the rule §5.3 states for prompts and context, and
	// nothing about an environment variable makes it safer; the opposite, since
	// a credential is far likelier to arrive here than in a prompt.
	//
	// So values are staged in a file the session reads and unlinks, exactly as
	// ContextRef is a path rather than content. A driver that cannot deliver
	// them out of band must REFUSE the create rather than start a session
	// missing them — a session that comes up without its identity looks healthy
	// and fails later, somewhere else.
	//
	// # The shape a value may have, and why it is bounded
	//
	// Names must look like environment variable names; values may not contain a
	// newline or a NUL. The bound is honest rather than incidental: the staging
	// format is line-oriented, and a value with an embedded newline would
	// silently become two variables — the same fabrication-out-of-value-content
	// this driver already had to defend against when recording an environment.
	// A refused create says so; a truncated one does not.
	Env map[string]string `json:"env,omitempty"`

	// Resume names a prior conversation this session continues.
	//
	// Distinct from DriverCapabilities.SupportsResume, which answers a different
	// question — whether sessions survive a service restart. A caller reading
	// that as "I can continue a conversation" would be wrong with nothing to
	// correct it, which is why this is a field of its own rather than a
	// capability flag.
	//
	// It is a HINT like Agent and Model: a runtime with no such notion must say
	// so at creation rather than start a fresh session that merely looks right.
	//
	// A resumed session commonly meets the runtime's resume chooser on the way
	// up. That question is deliberately NOT in Consents — see PromptKind and the
	// driver's own note: the option that means "the conversation I named" cannot
	// be identified from the option text, and a consent that guesses would pick
	// somebody's other session.
	Resume string `json:"resume,omitempty"`

	// PermissionMode requests a runtime permission posture other than the
	// default. The only value this fleet's runtime has is "bypass" — the mode
	// in which the agent stops asking before acting.
	//
	// It is deliberately not a bool. A boolean field named for today's single
	// dangerous mode ages into a lie the moment a second mode exists, and the
	// closed set makes an unrecognised value a refusal rather than a silent
	// default — §2.3's discipline applied to an input.
	//
	// The service requires the `send` grant for this on top of `create`: a
	// session in this mode acts without asking, and the mode also raises an
	// acceptance screen that has to be answered. A principal permitted only to
	// start sessions must not be able to start THAT one.
	PermissionMode string `json:"permissionMode,omitempty"`

	// Consents lists the boot questions the caller answers in advance, so the
	// driver may clear them instead of leaving a new session parked in front of
	// one.
	//
	// # Why a list, and why the kinds are named
	//
	// TrustCwd (below) shipped first and covers exactly one question. The moment
	// a second arrived — the acceptance screen a non-default PermissionMode
	// raises — it was clear the shape was wrong: one boolean per question means
	// a new field per runtime screen forever, and a caller cannot express "these
	// two, not that third one".
	//
	// Naming the kinds keeps the safety property that matters. A consent is
	// scoped to a question the driver can RECOGNISE (PromptKind), the option is
	// then found by reading the runtime's own option text, and an unrecognised
	// or ambiguously-worded screen is answered not at all. Nothing here is a
	// standing permission to answer whatever appears.
	//
	// Not every kind is consentable: see the driver. A question whose
	// affirmative option cannot be identified from its own text has no safe
	// consent, and offering one would be offering a coin flip.
	Consents []PromptKind `json:"consents,omitempty"`

	// TrustCwd carries the caller's consent to the runtime's own question
	// about Cwd — "is this a project you created or one you trust?" — so the
	// driver may answer it on the caller's behalf instead of leaving the new
	// session parked in front of it.
	//
	// Superseded by Consents, and kept because it shipped: it means exactly
	// `consents: ["folder-trust"]`, and a caller sending both is not in
	// conflict — the union is taken.
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

// ConsentsTo reports whether the caller consented, in this create request, to
// having a question of this kind answered on its behalf.
//
// It is the one place the older TrustCwd boolean and the newer Consents list are
// reconciled, so no driver has to remember that two spellings of one consent
// exist. The union is taken deliberately: a caller sending both said the same
// thing twice, which is agreement, not conflict.
func (s SessionSpec) ConsentsTo(kind PromptKind) bool {
	if kind == PromptFolderTrust && s.TrustCwd {
		return true
	}
	for _, k := range s.Consents {
		if k == kind {
			return true
		}
	}
	return false
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
