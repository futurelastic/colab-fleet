package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Outcome is the result of a Send call (§2.4). Delivery is not
// fire-and-forget and not a boolean.
type Outcome string

const (
	// OutcomeSubmitted means the driver confirmed the agent received the
	// input.
	OutcomeSubmitted Outcome = "submitted"
	// OutcomeQueued means the driver accepted the input but submission is
	// unconfirmed.
	OutcomeQueued Outcome = "queued"
	// OutcomeRefused means the driver declined; Reason explains why. This
	// is the important one (§2.4): a driver is expected to protect a
	// session from input that would corrupt it, and that protection
	// belongs in the contract, not in each caller's memory of a past
	// incident.
	OutcomeRefused Outcome = "refused"
	// OutcomeUnknown means the input was sent but the outcome is
	// unverifiable.
	OutcomeUnknown Outcome = "unknown"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeSubmitted, OutcomeQueued, OutcomeRefused, OutcomeUnknown:
		return true
	default:
		return false
	}
}

func (o Outcome) MarshalJSON() ([]byte, error) {
	if !o.valid() {
		return nil, fmt.Errorf("fleet: %q is not a valid Outcome", string(o))
	}
	return json.Marshal(string(o))
}

func (o *Outcome) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v := Outcome(raw)
	if !v.valid() {
		return fmt.Errorf("fleet: %q is not a valid Outcome", raw)
	}
	*o = v
	return nil
}

// DeliveryReceipt is the result of send() (§2.4). Reason is populated on
// OutcomeRefused explaining why (e.g. "prompt holds unsent input") and is
// otherwise informational.
//
// A refusal is carried as an ordinary 200 response on the wire
// (api-http.md §3.3), never an HTTP error: it is an expected domain outcome
// carrying structured information, not a fault. Mapping it to 4xx would
// train clients to treat it as an exception and retry — exactly the
// behaviour the refusal exists to prevent.
type DeliveryReceipt struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}

// PromptDelivery is what became of a prompt a create carried (§2.1's initial
// prompt), reported on the session rather than in the create's own response
// because it cannot be known when that response is written.
//
// # Why the create response could not carry a DeliveryReceipt
//
// send() returns one synchronously because delivery has finished by the time
// it answers. A create's prompt is delivered AFTER the process starts and
// after the runtime has painted a composer to receive it — Create returns
// 201 while that is still ahead of it. So the receipt has to resolve later,
// which is the shape ConversationRef and ResumeOutcome already exist for.
//
// # The ambiguity this closes, which was measured
//
// A session created with a prompt, polled about twelve seconds later, read
// `status: idle, evidence: "interface painted, composer empty, no turn
// yet"`. That classification is exactly right for "up and waiting for work
// with nothing sent" — and indistinguishable from "up and waiting, and an
// accepted prompt has not been delivered yet". The caller concluded the
// prompt was lost and re-sent it four times. `starting` does not help; it
// names an earlier shape, a session too young to have painted a composer at
// all. And the natural client loop — create, then poll until idle or
// waiting_input — returns on exactly this window by construction
// (colab-fleet #86).
//
// # Three states (§5.7)
//
//	the field absent       this create carried no prompt. Never a claim that
//	                       one was carried and delivered.
//	Outcome nil + Evidence a prompt was accepted at creation and its delivery
//	                       has not resolved. A real, temporary answer — while
//	                       this state holds, `idle` is not evidence of loss
//	                       and a caller must not re-send.
//	Outcome set + Evidence resolved, in the same closed set send() answers
//	                       with.
//
// # Why no Reason field beside Outcome
//
// DeliveryReceipt carries Outcome + Reason because a refusal needs
// explaining and a success does not. This type needs prose in every state —
// including the unresolved one, which has no receipt to draw a Reason from —
// so it carries the family's Evidence instead, and a driver holding a
// DeliveryReceipt copies that receipt's Reason into it. One prose field, not
// two.
type PromptDelivery struct {
	// Outcome is nil until delivery resolves. Once set it is the same value
	// send() would have returned for the same delivery.
	Outcome *Outcome `json:"outcome"`

	// Evidence is prose for humans, present in every state. Do not parse it
	// (§2.3); branch on Outcome being nil or set.
	Evidence string `json:"evidence"`
}

// PromptDelivered states a definite answer: delivery has resolved to
// outcome.
func PromptDelivered(outcome Outcome, evidence string) *PromptDelivery {
	return &PromptDelivery{Outcome: &outcome, Evidence: evidence}
}

// PromptPending records that a prompt was accepted at creation and its
// delivery has not resolved yet — a real, temporary answer, never a
// stand-in for "no prompt was sent" (the absent field) or for a loss.
func PromptPending(evidence string) *PromptDelivery {
	return &PromptDelivery{Evidence: evidence}
}

// ErrPromptDeliveryIncoherent is returned when a PromptDelivery would state
// something it cannot support — an outcome outside the closed set, or no
// evidence at all.
var ErrPromptDeliveryIncoherent = errors.New(
	"fleet: a PromptDelivery must carry evidence in every state, and a valid outcome when one is set")

func (p PromptDelivery) coherent() bool {
	if p.Evidence == "" {
		return false
	}
	return p.Outcome == nil || p.Outcome.valid()
}

type promptDeliveryWire PromptDelivery

// MarshalJSON refuses an incoherent value rather than emitting one — the
// same reasoning ConversationRef and ResumeOutcome both give: an outcome
// outside the closed set would silently decode as a fourth, unnamed state on
// the other end.
func (p PromptDelivery) MarshalJSON() ([]byte, error) {
	if !p.coherent() {
		return nil, ErrPromptDeliveryIncoherent
	}
	return json.Marshal(promptDeliveryWire(p))
}

// UnmarshalJSON applies the same rule to what a peer sends. A federated read
// is not a more trustworthy source than our own encoder.
func (p *PromptDelivery) UnmarshalJSON(b []byte) error {
	var wire promptDeliveryWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	got := PromptDelivery(wire)
	if !got.coherent() {
		return ErrPromptDeliveryIncoherent
	}
	*p = got
	return nil
}
