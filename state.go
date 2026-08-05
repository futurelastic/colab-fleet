package fleet

import (
	"encoding/json"
	"fmt"
)

// Status is a session's lifecycle state (§2.3, §8). It is a closed set:
// decoding an unrecognised or empty string is an error, never a silent
// default. A silently-defaulted zero value would be indistinguishable from
// an explicit answer — precisely the confusion §5.2 ("uncertainty travels")
// and §5.7 ("absence and failure are different answers") exist to forbid,
// applied here to a single field rather than a plural response.
type Status string

const (
	StatusStarting     Status = "starting"
	StatusWorking      Status = "working"
	StatusWaitingInput Status = "waiting_input"
	StatusIdle         Status = "idle"
	StatusQuotaBlocked Status = "quota_blocked"
	StatusDead         Status = "dead"
	// StatusUnknown is a valid answer, not an error (§2.3): the driver
	// could not determine the session's state and says so, rather than
	// guessing.
	StatusUnknown Status = "unknown"
)

func (s Status) valid() bool {
	switch s {
	case StatusStarting, StatusWorking, StatusWaitingInput, StatusIdle,
		StatusQuotaBlocked, StatusDead, StatusUnknown:
		return true
	default:
		return false
	}
}

// MarshalJSON rejects an invalid Status rather than emitting one silently.
// StatusUnknown included: it is the real value "unknown", never an empty
// string.
func (s Status) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid Status", string(s))
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON rejects any value outside the closed set in §2.3, including
// the empty string — an absent or empty status is not the same fact as
// "unknown" and must not be silently coerced into it.
func (s *Status) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := Status(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid Status", raw)
	}
	*s = v
	return nil
}

// Confidence separates knowing from guessing (§2.3). Also a closed set, for
// the same reason as Status.
type Confidence string

const (
	// ConfidenceObserved means a driver read a structured status from an
	// API.
	ConfidenceObserved Confidence = "observed"
	// ConfidenceInferred means a driver guessed from terminal output,
	// process tables, or file mtimes. Both are legitimate; collapsing the
	// distinction is how a precise runtime's answer gets flattened to an
	// imprecise one's (§2.3) — the interface would then destroy the exact
	// advantage it exists to expose.
	ConfidenceInferred Confidence = "inferred"
)

func (c Confidence) valid() bool {
	return c == ConfidenceObserved || c == ConfidenceInferred
}

func (c Confidence) MarshalJSON() ([]byte, error) {
	if !c.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid Confidence", string(c))
	}
	return json.Marshal(string(c))
}

func (c *Confidence) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := Confidence(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid Confidence", raw)
	}
	*c = v
	return nil
}

// SessionState is a driver's answer to "what is this session doing" (§2.3).
//
// Since is the time the status was first observed to hold, not the time it
// began (§8) — for inferred states those differ, sometimes by a lot. Nil
// means the driver has no opinion on when the status started; a driver that
// knows must say so, not synthesize a value it doesn't have (§5.2 again,
// applied to a single field).
// WaitingReason says WHY a session is `waiting_input`, when the driver can
// tell.
//
// # Why this had to exist the moment there was a second reason
//
// `waiting_input` began meaning one thing — blocked on a question — and a
// caller could branch on `prompt` being present. Adding usage limits gave it a
// third meaning with no prompt attached, and at that moment the status became
// ambiguous in a way only the evidence prose resolved:
//
//	waiting_input, no prompt → holds unsent text? or out of quota?
//
// Those need OPPOSITE handling. Text sitting unsent should be discarded or
// left alone, and sending more is the one thing that must not happen. A
// quota block is cleared by waiting or switching accounts, and nothing a
// caller sends will help. A client that cannot tell them apart will do the
// wrong one roughly half the time.
//
// Evidence cannot be the discriminator: §2.3 says it is prose for humans and
// must not be parsed, and this project has already paid twice for matching
// sentences that later changed.
//
// # Absent means unclassified, not "no reason"
//
// §5.7 again. A driver that knows why says so; one that does not leaves this
// empty, and a caller must treat empty as "go look" rather than as any
// particular cause.
type WaitingReason string

const (
	// WaitingPrompt: a question is on screen. `Prompt` carries it.
	WaitingPrompt WaitingReason = "prompt"
	// WaitingUnsentInput: the composer holds text nobody submitted. `Since`
	// is the age, and the age is what separates somebody mid-thought from
	// text nobody is coming back for.
	WaitingUnsentInput WaitingReason = "unsent-input"
	// WaitingUsageLimit: the account is out of quota. Sending achieves
	// nothing; this clears with time or a different account.
	WaitingUsageLimit WaitingReason = "usage-limit"
)

// TurnEnd says how the most recent turn FINISHED, when the screen says
// anything about it.
//
// # Why this is not a status
//
// A session whose turn died on a transient server error looks exactly like one
// that finished its work: the error prints, the spinner settles into its
// finished form, the composer empties. Both are `idle`, and `idle` is honest —
// the session is up and will accept input.
//
// What is missing is not the current state but a fact about the LAST TURN, and
// no status member carries it without lying about the present. `waiting_input`
// is the tempting hack and is wrong twice: nothing is being asked, and no human
// is needed — any caller resumes the session by sending anything at all.
//
// So the status stays `idle` and gains a footnote. A supervisor can then tell
// "finished, ready for the next thing" from "its work died and nobody noticed",
// which is the distinction `idle` had been collapsing.
//
// # Why not read the evidence string
//
// `Evidence` is prose for humans and explicitly not to be parsed. A caller that
// must ACT on this needs a field, or it ends up pattern-matching sentences this
// project keeps rewriting.
type TurnEnd struct {
	// Outcome is "failed" when the screen shows the turn ending in an error.
	// Absent otherwise: "it worked" is the unremarkable case, and recording it
	// would make every session carry a field nobody reads.
	Outcome string `json:"outcome"`

	// Reason is the runtime's own words, trimmed. For humans and logs; do not
	// branch on it.
	Reason string `json:"reason,omitempty"`

	// Retryable is true when the runtime itself called the failure temporary —
	// the difference between "poke it and the work continues" and "somebody
	// needs to look". Taken from what the screen SAYS, not inferred from an
	// error code we decided to interpret.
	Retryable bool `json:"retryable,omitempty"`
}

type SessionState struct {
	Status     Status     `json:"status"`
	Confidence Confidence `json:"confidence"`
	Evidence   string     `json:"evidence"`
	Since      *Timestamp `json:"since,omitempty"`

	// Prompt is the question this session is blocked on, when it is blocked
	// on one (§2.3, answered via §3's respond). Nil otherwise.
	//
	// It lives on the state rather than beside it so that every path which
	// reports state also reports the question: a single-session read, a
	// listing, and — the one that matters most — an event. A subscriber
	// learns that a session became blocked AND what it is asking in the same
	// message, instead of having to turn around and ask.
	//
	// Evidence names the highlighted option in prose; this is the structured
	// form a client can render as buttons and submit by index.
	Prompt *SessionPrompt `json:"prompt,omitempty"`

	// WaitingOn says why the session is `waiting_input`, when the driver can
	// tell. Empty for every other status, and empty on waiting_input means
	// unclassified — see WaitingReason.
	WaitingOn WaitingReason `json:"waitingOn,omitempty"`

	// LastTurn reports how the most recent turn ended, when the screen says
	// anything about it. Nil is the ordinary case and means nothing was said —
	// NOT that the turn succeeded (§5.7: absence is not a finding).
	LastTurn *TurnEnd `json:"lastTurn,omitempty"`
}

// ObservedState constructs a SessionState a driver reports from a
// structured read (§2.3). Prefer this and InferredState over a bare struct
// literal: a hand-filled literal can typo Confidence into a value that lies
// about how the driver actually learned the status, and these two
// constructors are the only places that decision needs to be made
// correctly.
func ObservedState(status Status, evidence string, since *Timestamp) SessionState {
	return SessionState{Status: status, Confidence: ConfidenceObserved, Evidence: evidence, Since: since}
}

// InferredState constructs a SessionState a driver guessed at (§2.3). See
// ObservedState.
func InferredState(status Status, evidence string, since *Timestamp) SessionState {
	return SessionState{Status: status, Confidence: ConfidenceInferred, Evidence: evidence, Since: since}
}

// UnknownState constructs the §2.3 "a real answer, not an error" state. A
// driver that could not determine the session's status calls this rather
// than guessing at StatusIdle or StatusDead.
//
// Confidence is still a parameter here, not fixed: a driver whose API
// explicitly returned "I don't know" is ConfidenceObserved about its own
// ignorance; a driver that merely timed out trying to infer is
// ConfidenceInferred. The spec does not say StatusUnknown implies either —
// see the package doc's findings list.
func UnknownState(confidence Confidence, evidence string) SessionState {
	return SessionState{Status: StatusUnknown, Confidence: confidence, Evidence: evidence}
}
