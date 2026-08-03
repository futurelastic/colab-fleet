package fleet

// Ack is what interrupt() and close() return (§3's operations table).
//
// The spec names this type but never gives it a shape — unlike
// DeliveryReceipt, which is fully specified. This is the shape this
// transcription settled on, matching the HTTP wire's own description of
// these two calls (api-http.md §3.3): both return 202 Accepted and "express
// intent" only; confirmation of what actually happened arrives later as a
// state change on the event stream (§4). Ack therefore carries only whether
// the request was accepted for processing — never a status of its own,
// since a driver that reported one here would be promising synchronous
// completion, which §5.6 ("degrade, never emulate") forbids a driver from
// promising when it cannot deliver it.
type Ack struct {
	Accepted bool `json:"accepted"`
}
