package fleet

import (
	"encoding/json"
	"errors"
)

// ResumeOutcome states whether a session's creation asked the runtime to
// resume a conversation and, once that can be told, whether it actually did
// (colab-fleet #72).
//
// # Why this exists
//
// A create's resume request is a single argv flag with no receipt. Measured
// under a concurrent burst (#72): the runtime can silently ignore it and
// start a fresh conversation instead — no refusal, no degraded status, the
// created session reporting a perfectly healthy start on a conversation
// nobody asked for. Every other write surface in this API is built the
// other way round: Send's DeliveryReceipt reports "unknown" rather than
// claiming delivery it cannot confirm, the raw-key route refuses an
// uncorroborated screen, a capability read distinguishes "observed" from
// "assumed". A create that quietly downgrades resume-to-fresh is the one
// write that did not carry this discipline, and the recovery this API
// exists to make survivable is exactly a concurrent burst by construction —
// which is precisely when being wrong quietly costs the most.
//
// # Three states, the same §5.7 discipline as ConversationRef
//
//	field absent (nil)       no resume was requested at creation. Not a
//	                         claim that one was requested and honoured.
//	Honoured nil + Evidence  a resume WAS requested, but the session's own
//	                         Conversation has not resolved yet — too early
//	                         to say either way, never read as a "no".
//	Honoured true/false      the actual conversation has resolved, and this
//	                         states whether it is the one that was asked
//	                         for.
//
// This is deliberately its own type rather than a field folded into
// ConversationRef: "which record identifies this session" and "did the
// create that made this session get what it asked for" are different
// questions, and a session with no resume request in its history still
// answers the first one every time it is listed.
type ResumeOutcome struct {
	// Requested is the conversation id creation asked the runtime to
	// resume. Always non-empty when this field is present at all —
	// enforced by coherent(), the same way ConversationRef enforces its
	// own Known/ID/Source triple.
	Requested string `json:"requested"`

	// Honoured is nil until the session's own Conversation resolves.
	// True means Conversation.ID matches Requested; false means the
	// runtime started a fresh conversation instead of the one asked for —
	// the defect #72 measured.
	Honoured *bool `json:"honoured"`

	// Evidence is prose for humans, present either way — the same
	// discipline ConversationRef holds itself to: "matches" and "does not
	// match, and here is what it got instead" are different strengths of
	// the same answer, and a caller deciding whether to trust a resumed
	// session is entitled to know which one this is.
	//
	// Do not parse it. Branch on Honoured being nil, true or false.
	Evidence string `json:"evidence"`
}

// ResumeResolved states a definite answer: the session's own conversation
// has resolved, and honoured says whether it is the one creation asked for.
func ResumeResolved(requested string, honoured bool, evidence string) *ResumeOutcome {
	return &ResumeOutcome{Requested: requested, Honoured: &honoured, Evidence: evidence}
}

// ResumeUnresolved records that a resume was requested but the session's own
// conversation has not resolved enough to say whether it was honoured — a
// real, temporary answer, never a stand-in for "no".
func ResumeUnresolved(requested, evidence string) *ResumeOutcome {
	return &ResumeOutcome{Requested: requested, Evidence: evidence}
}

// ErrResumeOutcomeIncoherent is returned when a ResumeOutcome would state
// something it cannot support — no requested id, or no evidence either way.
var ErrResumeOutcomeIncoherent = errors.New(
	"fleet: a ResumeOutcome must carry the requested id and evidence either way")

func (r ResumeOutcome) coherent() bool {
	return r.Requested != "" && r.Evidence != ""
}

type resumeOutcomeWire ResumeOutcome

// MarshalJSON refuses an incoherent outcome rather than emitting one, the
// same reasoning ConversationRef's own MarshalJSON gives: a caller reading
// an empty Requested would not see "malformed", it would see a resume to
// nothing in particular, and comparing two of those concludes two unrelated
// sessions share an outcome.
func (r ResumeOutcome) MarshalJSON() ([]byte, error) {
	if !r.coherent() {
		return nil, ErrResumeOutcomeIncoherent
	}
	return json.Marshal(resumeOutcomeWire(r))
}

// UnmarshalJSON applies the same rule to what a peer sends — a federated
// read is not a more trustworthy source than our own encoder.
func (r *ResumeOutcome) UnmarshalJSON(b []byte) error {
	var wire resumeOutcomeWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := ResumeOutcome(wire)
	if !got.coherent() {
		return ErrResumeOutcomeIncoherent
	}
	*r = got
	return nil
}
