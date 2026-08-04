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
