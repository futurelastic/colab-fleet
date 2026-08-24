package fleet

import (
	"encoding/json"
	"errors"
)

// IdentityAssertion states whether the identity a machine last asserted for a
// session is the one the runtime still carries (colab-fleet #102).
//
// # Why this exists (colab-fleet #102, on the back of #96/#97)
//
// #97 measured a rename that returned 202, read back correct for roughly half
// an hour, then silently reverted — id, name and attach target all restored,
// with nothing in any response saying so for that whole window. #97's own fix
// made the disagreement detectable and self-repairing: a driver's List now
// compares what it just enumerated against a durable record of what it last
// asserted, and puts a name back when the two disagree. But the only place
// that fact surfaced was a prose sentence appended to SessionState.Evidence —
// a field §2.3 tells every caller never to parse. A fact that exists, is
// durable, and is reachable only by pattern-matching prose is not on the
// wire; this type is.
//
// # Four states, the same §5.7 discipline as RuntimeSurfaceRef
//
//	the field absent          this machine has asserted no identity for this
//	                          session at all — an adopted, foreign or
//	                          cold-store session it never named, or a driver
//	                          with no state store. Never a claim the
//	                          identity agrees; that claim is Drifted false.
//	Drifted nil + Evidence    an identity WAS asserted, but no read has yet
//	                          matched the durable record to a live run to
//	                          corroborate it. Clears on the next read. Never
//	                          read as "no drift".
//	Drifted false + Evidence  settled, as of THIS read: the runtime carries
//	                          exactly what this machine asserted.
//	Drifted true + Carried    this read found them disagreeing — #97's
//	                          defect, machine-readable.
//
// The third state exists for a real, observed case, not for symmetry: a
// session's asserted identity is recorded the instant a Create or Rename
// resolves it, before any read has had a chance to corroborate it against a
// live run — matched by (pane, created) so a rename does not orphan the
// match. A record still missing that pairing cannot yet be told apart from a
// drifted one, so it is reported as unresolved rather than guessed either
// way, and the gap closes on the very next read.
//
// # Not latched, unlike RuntimeSurfaceRef
//
// RuntimeSurfaceRef's Known true is an identity that, once corroborated,
// stays true for the life of the record even if the surface later goes
// quiet — a health question belongs to SessionState.ControlChannel instead.
// Drifted here is the opposite: it is a claim about THIS read alone. The
// runtime's name for a session can change again the moment after a read
// reports agreement, and the next read is what says so — never a cached
// verdict from an earlier one.
//
// # One source, which is why there is no Source field
//
// PinResult and RuntimeSurfaceRef each fork a real question — observed
// against declared, observed against derived. Here there is exactly one
// source in every state: Asserted comes from this machine's own durable
// record, Carried from the live enumeration in the same read. A field with
// one possible value teaches a caller nothing and only invites a later
// driver to invent a second meaning for it.
//
// # Contested is not a state here
//
// A driver that gives up repairing a drift — because the wanted name is live
// under another session, or because it has already tried and failed as many
// times as it will — is reporting a fact about its own REPAIR POLICY, not
// about what this read observed. Folding that into this type would report a
// decision about a future action as if it were an observation of the
// present, the same category of error Session's own Agent/Model fields are
// documented to avoid. It is also not a stable fact: a name taken by another
// session becomes free the moment that session closes, so "contested" would
// be a claim that can expire with nobody told. The operational need is met
// elsewhere — a driver-side counter, and Evidence prose naming how many
// times a repair has already been attempted. Adding a field to a published
// wire contract later is additive and breaks nobody; retracting one is not,
// so the narrower shape is the one that ships.
type IdentityAssertion struct {
	// Asserted is the identity this machine last asserted for this session —
	// at a create, or at a rename this machine itself issued. Always
	// non-empty when this field is present at all, enforced by coherent(),
	// the same way ResumeOutcome enforces its own Requested.
	Asserted string `json:"asserted"`

	// Drifted is nil while an asserted identity has not yet been
	// corroborated against a live read, false once a read finds the runtime
	// carrying exactly Asserted, true once one finds it carrying something
	// else. Nil is never the way to say nobody asserted anything — that is
	// the field being absent on Session, one level up.
	Drifted *bool `json:"drifted"`

	// Carried is what the runtime carries right now, when it disagrees with
	// Asserted. Required when Drifted is true, absent in every other state,
	// and never equal to Asserted.
	Carried string `json:"carried,omitempty"`

	// AssertedAt is when Asserted was last SET — a create or a rename this
	// machine itself issued, never bumped merely by a repair attempt.
	// Optional in every state: a record predating this field simply carries
	// none. §11 applies — this is one machine's own clock, not safe to
	// compare against a Timestamp another machine stamped.
	AssertedAt *Timestamp `json:"assertedAt,omitempty"`

	// Evidence is prose for humans, present in every state. Do not parse it;
	// branch on Drifted being nil, false or true.
	Evidence string `json:"evidence"`
}

// IdentityHeld records that, as of this read, the runtime carries exactly
// the identity this machine last asserted.
func IdentityHeld(asserted string, assertedAt Timestamp, evidence string) *IdentityAssertion {
	return &IdentityAssertion{
		Asserted: asserted, Drifted: boolPtr(false),
		AssertedAt: assertedAtPtr(assertedAt), Evidence: evidence,
	}
}

// IdentityDrifted records that this read found the runtime carrying a
// different identity from the one this machine last asserted — colab-fleet
// #97's defect, made machine-readable.
func IdentityDrifted(asserted, carried string, assertedAt Timestamp, evidence string) *IdentityAssertion {
	return &IdentityAssertion{
		Asserted: asserted, Drifted: boolPtr(true), Carried: carried,
		AssertedAt: assertedAtPtr(assertedAt), Evidence: evidence,
	}
}

// IdentityUncorroborated records that this machine asserted an identity and
// no read has yet matched the durable record to a live run — a real,
// temporary answer, never a stand-in for "no drift".
func IdentityUncorroborated(asserted string, assertedAt Timestamp, evidence string) *IdentityAssertion {
	return &IdentityAssertion{
		Asserted: asserted, AssertedAt: assertedAtPtr(assertedAt), Evidence: evidence,
	}
}

func assertedAtPtr(t Timestamp) *Timestamp {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ErrIdentityAssertionIncoherent is returned when an IdentityAssertion would
// state something it cannot support — no asserted identity, no evidence, a
// Carried value without Drifted true, a Drifted-true value with no Carried,
// or Carried equal to Asserted.
var ErrIdentityAssertionIncoherent = errors.New(
	"fleet: an IdentityAssertion must carry the asserted identity and evidence in every state, " +
		"and Carried only when Drifted is true and it differs from Asserted")

func (a IdentityAssertion) coherent() bool {
	if a.Asserted == "" || a.Evidence == "" {
		return false
	}
	if a.Drifted == nil || !*a.Drifted {
		return a.Carried == ""
	}
	return a.Carried != "" && a.Carried != a.Asserted
}

type identityAssertionWire IdentityAssertion

// MarshalJSON refuses an incoherent value rather than emitting one — the
// same reasoning ResumeOutcome's and RuntimeSurfaceRef's own MarshalJSON
// give: a caller reading a partially-filled assertion would see a real one
// with some fields happening to be empty, not a malformed one.
func (a IdentityAssertion) MarshalJSON() ([]byte, error) {
	if !a.coherent() {
		return nil, ErrIdentityAssertionIncoherent
	}
	return json.Marshal(identityAssertionWire(a))
}

// UnmarshalJSON applies the same rule to what a peer sends. A federated read
// is not a more trustworthy source than our own encoder.
func (a *IdentityAssertion) UnmarshalJSON(b []byte) error {
	var wire identityAssertionWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := IdentityAssertion(wire)
	if !got.coherent() {
		return ErrIdentityAssertionIncoherent
	}
	*a = got
	return nil
}
