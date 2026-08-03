package fleet

import "errors"

// PinSupport is the per-field breakdown of DriverCapabilities.SupportsPin
// (§4.3): a driver may be able to pin a model without being able to pin an
// effort level, or vice versa.
type PinSupport struct {
	Model  bool `json:"model"`
	Effort bool `json:"effort"`
	Agent  bool `json:"agent"`
}

// DriverCapabilities is what a driver declares about itself (§4.3). Callers
// must consult this (GET /v1/runtimes, api-http.md §3.1) and degrade rather
// than assume (§5.6) — a driver never silently emulates a capability it
// lacks.
type DriverCapabilities struct {
	// ObservesState reports whether the driver can report status without
	// inference.
	ObservesState bool `json:"observesState"`
	// ConfirmsDelivery reports whether the driver can distinguish
	// "submitted" from "queued".
	ConfirmsDelivery bool `json:"confirmsDelivery"`
	// SupportsResume reports whether sessions survive a service restart.
	SupportsResume bool `json:"supportsResume"`

	SupportsPin PinSupport `json:"supportsPin"`

	// DeadlineMs is mandatory (§4.4): "a driver that can block without a
	// bound is a specification violation, not a slow driver." Measured
	// directly against a stopped peer, an undeadlined call was still
	// blocked with no result after seven seconds. Zero or negative fails
	// Validate.
	DeadlineMs int64 `json:"deadlineMs"`
}

// ErrNoDeadline is returned by Validate when DeadlineMs is not a positive
// number — §4.4's rule made mechanically checkable rather than only stated
// in prose.
var ErrNoDeadline = errors.New("fleet: DriverCapabilities.DeadlineMs must be > 0 (§4.4: every driver declares a deadline)")

// Validate enforces §4.4. Intended to run once, at driver registration
// (see internal/service.Service.RegisterLocalDriver /
// RegisterPeerDriver), so an undeadlined driver is rejected before it ever
// serves a request rather than discovered by a caller blocking forever.
func (c DriverCapabilities) Validate() error {
	if c.DeadlineMs <= 0 {
		return ErrNoDeadline
	}
	return nil
}
