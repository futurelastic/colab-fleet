package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// RuntimeSurfaceRef names where a session is reachable on a surface the
// RUNTIME operates — one that is neither a terminal on the session's own
// machine nor this service's own HTTP API, and whose identifier is neither
// this service's session id nor the conversation id.
//
// # Why this needs a field of its own, next to two that nearly fit
//
// A session can be created reachable from somewhere other than the machine
// running it (SessionSpec.RemoteControl), and measured on a live fleet it
// really is: the runtime registers the session on its own surface, unasked,
// moments after the process starts. Nothing in any response said so, and
// nothing said how to reach it (colab-fleet #85).
//
// The two neighbouring fields answer different questions and must not be
// made to answer this one:
//
//   - AttachHint (§2.8) is how a HUMAN'S TERMINAL, already on this session's
//     machine, takes the session over. Its own doc comment declines to
//     describe any remote form on purpose — "this machine does not know how
//     you reach it" — and its Command field is argv, which a hosted surface
//     has no use for.
//   - ConversationRef (§2.9) names the runtime's record of the TRANSCRIPT.
//     That is a different identifier with a different lifetime, and
//     conflating the two is a recorded source of false negatives elsewhere
//     in this fleet.
//
// What is left is an identity question — "what is this session's address on
// that surface" — and identity is what the ConversationRef family is for.
// So this takes that shape rather than inventing a fourth.
//
// # Four states, because "not yet" and "never" are different answers (§5.7)
//
//	the field absent           nobody looked: this driver does not report a
//	                           runtime surface, or its runtime operates none.
//	                           Ask DriverCapabilities.ReportsRuntimeSurface,
//	                           which is the field that tells those apart.
//	Known nil + Evidence       a surface WAS requested at creation and has
//	                           not resolved. The runtime registers
//	                           asynchronously, after the process starts, so
//	                           this is what a create legitimately returns.
//	                           Never read as "no".
//	Known false + Evidence     settled: this session has no such surface —
//	                           the create opted out, or the runtime
//	                           declined. A caller polling for one may stop.
//	Known true + Kind + Target the address, and Source says how it was
//	                           learned.
//
// The pointer, rather than ConversationRef's plain bool, is ResumeOutcome's
// shape and is here for a reason ConversationRef does not have: for a
// conversation, "not yet" and "could not tell" are the same operational fact
// — look again in a moment, the lookup is cheap and idempotent. Here they
// are opposites. "The create opted out of remote control" never clears,
// however long anyone polls; "the runtime has not registered it yet" clears
// in seconds. A caller that cannot branch on that difference either polls
// forever on a session that will never have a surface, or gives up on one
// that would have had one.
//
// # Identity, not liveness
//
// Known true means this driver has established that the runtime brought the
// surface up under this identifier. It is NOT a claim the surface is
// reachable right now — that is SessionState.ControlChannel's job (§2.3),
// which reports what the runtime says about the channel's health and
// already distinguishes failed from reconnecting from active. A surface
// that has gone quiet is still addressed by the same identifier, and
// unresolving it on every dropped connection would make an identity flicker
// with a health signal.
type RuntimeSurfaceRef struct {
	// Known is nil while a requested surface has not resolved, false when
	// it is settled that there is none, true when the address below is
	// real. It is never the way to say "nobody looked" — that is the
	// absent field.
	Known *bool `json:"known"`

	// Kind names the mechanism in the abstract so a client can decide
	// whether it understands the address before reading Target. Required
	// when Known is true, absent otherwise. Unknown kinds must be treated
	// as unsupported rather than guessed at (§5.6) — the same rule
	// AttachHint.Kind carries, and the reason this is a free string rather
	// than a strictly-decoded set: a peer of a later vintage must be able
	// to name a mechanism this build has never heard of without the whole
	// session failing to decode.
	Kind string `json:"kind,omitempty"`

	// Target is the identifier the session is addressed by ON that surface
	// — opaque here, deliberately. This service does not resolve it into a
	// URL: it does not know how a caller reaches that surface, for exactly
	// the reason AttachHint gives for publishing local argv and no remote
	// form, and for the reason §7.2 requires a peer's address to be one an
	// operator confirmed rather than the peer's own idea of its name. A
	// client that understands Kind knows how to resolve Target; one that
	// does not must not be handed a URL this machine invented. Required
	// when Known is true.
	Target string `json:"target,omitempty"`

	// Source is how the address was learned. Required when Known is true.
	Source RuntimeSurfaceSource `json:"source,omitempty"`

	// Evidence is prose for humans, present in every state — the
	// discipline ConversationRef and ResumeOutcome both hold. Do not parse
	// it (§2.3); branch on Known being nil, false or true.
	//
	// Note what it may and may not say. In the unresolved state it MAY
	// name the identifier the runtime was ASKED to register under, as
	// prose. That identifier must never be promoted into Target while
	// Known is not true: a value the service supplied and a value the
	// runtime confirmed are different facts, and reporting the first as
	// the second is colab-fleet #84 arriving in a second field.
	Evidence string `json:"evidence"`
}

// RuntimeSurfaceSource is how an address was learned. A closed set, decoded
// strictly, for the same reason as ConversationSource — and both members
// are named now though only one is produced today, so a new member is not a
// wire break for an older peer.
type RuntimeSurfaceSource string

const (
	// RuntimeSurfaceObserved means the runtime reported the address and
	// the driver read that report, with nothing to match.
	RuntimeSurfaceObserved RuntimeSurfaceSource = "observed"

	// RuntimeSurfaceDerived means the driver matched rather than read: it
	// supplied the identifier at creation and then corroborated, from the
	// runtime's own report about itself, that the surface came up under
	// it. The match is not a guess — but it is still a match, and it can
	// be wrong in ways a dictated value cannot. Exactly the strength
	// ConversationDerived describes, in the same words and for the same
	// reason.
	RuntimeSurfaceDerived RuntimeSurfaceSource = "derived"
)

func (s RuntimeSurfaceSource) valid() bool {
	return s == RuntimeSurfaceObserved || s == RuntimeSurfaceDerived
}

func (s RuntimeSurfaceSource) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid RuntimeSurfaceSource", string(s))
	}
	return json.Marshal(string(s))
}

func (s *RuntimeSurfaceSource) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := RuntimeSurfaceSource(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid RuntimeSurfaceSource", raw)
	}
	*s = v
	return nil
}

// RuntimeSurfaceControlChannel is the mechanism kind for a surface reached
// over the runtime's own remote-control channel — the same channel
// SessionState.ControlChannel reports the health of.
const RuntimeSurfaceControlChannel = "control-channel"

// ResolvedRuntimeSurface names a session's confirmed address on a
// runtime-operated surface, and how that was learned.
func ResolvedRuntimeSurface(kind, target string, source RuntimeSurfaceSource, evidence string) *RuntimeSurfaceRef {
	return &RuntimeSurfaceRef{Known: boolPtr(true), Kind: kind, Target: target, Source: source, Evidence: evidence}
}

// NoRuntimeSurface records that it is settled this session has no such
// surface — the create opted out, or the runtime declined. A real, final
// answer: a caller polling for one may stop.
func NoRuntimeSurface(evidence string) *RuntimeSurfaceRef {
	return &RuntimeSurfaceRef{Known: boolPtr(false), Evidence: evidence}
}

// PendingRuntimeSurface records that a surface was requested at creation
// and has not resolved yet — a real, temporary answer, never a stand-in for
// "no".
func PendingRuntimeSurface(evidence string) *RuntimeSurfaceRef {
	return &RuntimeSurfaceRef{Evidence: evidence}
}

func boolPtr(b bool) *bool { return &b }

// ErrRuntimeSurfaceIncoherent is returned when a RuntimeSurfaceRef would
// state something it cannot support — a kind/target/source with Known not
// true, or Known true with any of the three missing, or no evidence at all.
var ErrRuntimeSurfaceIncoherent = errors.New(
	"fleet: a RuntimeSurfaceRef must carry a kind, a target and a source when known, none of them otherwise, and evidence in every state")

func (r RuntimeSurfaceRef) coherent() bool {
	if r.Evidence == "" {
		return false
	}
	if r.Known == nil || !*r.Known {
		return r.Kind == "" && r.Target == "" && r.Source == ""
	}
	return r.Kind != "" && r.Target != "" && r.Source.valid()
}

type runtimeSurfaceRefWire RuntimeSurfaceRef

// MarshalJSON refuses an incoherent value rather than emitting one — the
// same reasoning ConversationRef's own MarshalJSON gives: a caller reading
// a partially-filled ref would see a real one with some fields happening to
// be empty, not a malformed one.
func (r RuntimeSurfaceRef) MarshalJSON() ([]byte, error) {
	if !r.coherent() {
		return nil, ErrRuntimeSurfaceIncoherent
	}
	return json.Marshal(runtimeSurfaceRefWire(r))
}

// UnmarshalJSON applies the same rule to what a peer sends. A federated
// read is not a more trustworthy source than our own encoder.
func (r *RuntimeSurfaceRef) UnmarshalJSON(b []byte) error {
	var wire runtimeSurfaceRefWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := RuntimeSurfaceRef(wire)
	if !got.coherent() {
		return ErrRuntimeSurfaceIncoherent
	}
	*r = got
	return nil
}
