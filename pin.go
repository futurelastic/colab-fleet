package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// PinOutcome states what a create asked the runtime to pin and, once that
// can be told, what it actually got — §2.1's "hints, not guarantees" made
// answerable instead of only asserted.
//
// # Why an echo was not enough, and was worse than nothing
//
// SessionSpec's own doc comment already states the rule: a driver that
// cannot honour a pin must say so at creation rather than silently
// substitute a default. Measured (colab-fleet #84): a pin whose value began
// with "-" failed an argv guard, the flag was never appended, and the
// create response echoed the REQUESTED value back — so the one caller in a
// position to notice was told the pin had been applied. An echo is not a
// weak answer; it is a fabricated one, indistinguishable from the strong
// answer it imitates.
//
// # Three states per pin, the same discipline ResumeOutcome holds (§5.7)
//
//	the field absent        no pin of this kind was requested at creation.
//	                        Never a claim that one was requested and honoured.
//	Honoured nil + Evidence a pin WAS requested and this driver cannot say
//	                        what the runtime applied — not yet, or not ever.
//	                        A real answer, never read as "dropped".
//	Honoured true/false     resolved: Source says how, Applied names what
//	                        the runtime is actually using when it can be named.
//
// The middle state is the honest report for a driver that passes a pin on a
// command line with no way to read back what came of it — every driver on
// this substrate today. It is deliberately not collapsed into a silence a
// caller would read as success.
type PinOutcome struct {
	Agent  *PinResult `json:"agent,omitempty"`
	Model  *PinResult `json:"model,omitempty"`
	Effort *PinResult `json:"effort,omitempty"`
}

// empty reports whether every field is absent, so a driver can return nil
// instead of a struct carrying three nil fields — the same rule every other
// optional field on Session already follows.
func (p *PinOutcome) empty() bool {
	return p == nil || (p.Agent == nil && p.Model == nil && p.Effort == nil)
}

// PinResult is one pin's request and its fate.
type PinResult struct {
	// Requested is the value the create asked for. Always non-empty when
	// this field is present at all — enforced by coherent(), the same way
	// ResumeOutcome enforces its own Requested.
	Requested string `json:"requested"`

	// Honoured is nil until the driver can compare the request against what
	// the runtime is using. True means the runtime is using Requested;
	// false means it substituted something else, which Applied names when
	// it can be named.
	Honoured *bool `json:"honoured"`

	// Applied is what the runtime is actually using, when the driver can
	// read it back. Empty is a real answer even alongside a resolved
	// Honoured: a driver may be able to establish that a value was NOT
	// honoured without being able to say what replaced it.
	//
	// Never populated from the request. A value that came from the caller
	// and a value that came from the runtime are different facts, and
	// putting the first one here is the entire defect this type closes.
	Applied string `json:"applied,omitempty"`

	// Source is how Honoured was established. Required when Honoured is
	// not nil, absent otherwise — the same rule ConversationRef applies to
	// its own Source.
	Source PinSource `json:"source,omitempty"`

	// Evidence is prose for humans, present in every state. Do not parse
	// it (§2.3); branch on Honoured being nil, true or false.
	Evidence string `json:"evidence"`
}

// PinSource is how a pin's fate was established. A closed set, decoded
// strictly, for the same reason as Status, Confidence and
// ConversationSource.
//
// Both members are named now though only one is produced today, for the
// reason ConversationSource's own comment gives at length: this service
// federates, peers run different vintages, and a strict decoder makes a NEW
// member a wire break for whichever peer is older.
type PinSource string

const (
	// PinObserved means the runtime reported what it is using and the
	// driver read that report — the strong answer.
	PinObserved PinSource = "observed"

	// PinDeclared means the driver established the fate from its own
	// knowledge of what it did, without a runtime report: it refused the
	// value, or it knows the substrate has no parameter to carry it.
	// Reserved; see the type comment for why it is named before it is
	// produced.
	PinDeclared PinSource = "declared"
)

func (s PinSource) valid() bool {
	return s == PinObserved || s == PinDeclared
}

func (s PinSource) MarshalJSON() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid PinSource", string(s))
	}
	return json.Marshal(string(s))
}

func (s *PinSource) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := PinSource(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid PinSource", raw)
	}
	*s = v
	return nil
}

// PinResolved states a definite answer: the driver can compare the request
// against what the runtime is using, and honoured says whether it matches.
func PinResolved(requested, applied string, honoured bool, source PinSource, evidence string) *PinResult {
	return &PinResult{Requested: requested, Applied: applied, Honoured: &honoured, Source: source, Evidence: evidence}
}

// PinUnresolved records that a pin was requested but this driver cannot say
// what the runtime applied — a real, standing answer for a driver with no
// way to read a pin back, never a stand-in for "dropped".
func PinUnresolved(requested, evidence string) *PinResult {
	return &PinResult{Requested: requested, Evidence: evidence}
}

// ErrPinResultIncoherent is returned when a PinResult would state something
// it cannot support — no requested value, an id with no source, or no
// evidence either way.
var ErrPinResultIncoherent = errors.New(
	"fleet: a PinResult must carry the requested value and evidence either way, and a source when honoured is known")

func (p PinResult) coherent() bool {
	if p.Requested == "" || p.Evidence == "" {
		return false
	}
	if p.Honoured == nil {
		return p.Source == "" && p.Applied == ""
	}
	return p.Source.valid()
}

type pinResultWire PinResult

// MarshalJSON refuses an incoherent value rather than emitting one — the
// same reasoning ConversationRef and ResumeOutcome both give.
func (p PinResult) MarshalJSON() ([]byte, error) {
	if !p.coherent() {
		return nil, ErrPinResultIncoherent
	}
	return json.Marshal(pinResultWire(p))
}

// UnmarshalJSON applies the same rule to what a peer sends. A federated
// read is not a more trustworthy source than our own encoder.
func (p *PinResult) UnmarshalJSON(b []byte) error {
	var wire pinResultWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := PinResult(wire)
	if !got.coherent() {
		return ErrPinResultIncoherent
	}
	*p = got
	return nil
}
