package fleet

import (
	"encoding/json"
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
