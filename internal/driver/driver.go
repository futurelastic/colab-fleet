// Package driver defines the Driver contract every runtime (local or
// remote) implements — session-abstraction.md §3's operations and §4's
// capability declaration, transcribed.
//
// This package is internal, alongside the service that consumes it: a
// third party writing only an HTTP client needs the wire types in the
// root fleet package, never this interface (see that package's doc
// comment).
package driver

import (
	"context"
	"errors"
	"strings"

	fleet "github.com/godx-jp/colab-fleet"
)

// ErrUnsupported is returned by a Driver method whose capability it lacks
// (§4.3). It maps to the wire "unsupported" error kind (api-http.md §2,
// HTTP 501). A driver must never silently emulate the capability instead
// (§5.6).
var ErrUnsupported = errors.New("driver: capability not supported")

// SendOptions carries the optional fields of a Send call (the wire body's
// "submit", api-http.md §3.3).
type SendOptions struct {
	Submit bool
}

// ListFilter narrows List to a subset of sessions (the query parameters of
// GET /v1/sessions, api-http.md §3.2). The zero value means no filter.
type ListFilter struct {
	Status    fleet.Status
	Agent     fleet.AgentId
	CwdPrefix string
}

// SubscribeFilter narrows which events a subscription receives (§3, §5.5).
//
// # Granularity is a cost parameter, not a convenience
//
// §5.5's amendment records why this type has the shape it does. On a
// substrate where watching a session's content costs a connection — as it
// does on the first driver, where content notifications are delivered only
// to a client attached to that session — the filter decides whether a
// subscription costs O(subscribers) or O(sessions).
//
// Naming sessions is therefore not sugar over CwdPrefix. A caller that can
// only describe what it wants ("everything under this directory") makes the
// driver attach to every match; a caller that can name it attaches to one.
// The earlier shape forced the first behaviour on every caller, including
// those who knew exactly which session they meant.
//
// Both fields narrow, and they compose with AND — the same rule ListFilter
// follows, so a caller does not have to remember two conventions. The zero
// value means no filter.
type SubscribeFilter struct {
	// Sessions names specific session ids on this machine. Empty means no
	// id constraint.
	//
	// Ids are recyclable (§5.4), and a subscription that names one inherits
	// that: if the session at this id dies and a new one takes the name,
	// the stream will carry events for the new one. This is safe rather
	// than surprising only because the discontinuity is ANNOUNCED — the
	// subscriber receives session.closed and then session.created, so it
	// can tell the difference. A stream that silently swapped subjects
	// would be §7.3's silent gap in another costume.
	Sessions []string

	CwdPrefix string
}

// Matches reports whether a session satisfies this filter.
func (f SubscribeFilter) Matches(id, cwd string) bool {
	if f.CwdPrefix != "" && !strings.HasPrefix(cwd, f.CwdPrefix) {
		return false
	}
	if len(f.Sessions) == 0 {
		return true
	}
	for _, want := range f.Sessions {
		if want == id {
			return true
		}
	}
	return false
}

// EventStream is a driver-owned handle for a live subscription (§3, §4).
// Next blocks until an event is available, ctx is cancelled, or the stream
// ends — a req is never expected to poll (§5.5).
type EventStream interface {
	Next(ctx context.Context) (fleet.Event, error)
	Close() error
}

// # Every operation carries the caller's authority
//
// Each method takes a fleet.Caller as its second argument, and that position
// is not cosmetic. §13 requires a proxying service to present the ORIGINAL
// caller's authority to a peer, never its own; §6 requires every
// remote-originated mutation to be logged with its actor. Neither is
// satisfiable if the operations have nowhere to carry a principal.
//
// An earlier revision passed this out of band in a context value, which a
// service could silently forget to attach — whereupon a remote driver's
// natural fallback was its own credential, the request succeeded, and the
// authorization was quietly widened. As a parameter it cannot be omitted,
// and a driver cannot compile without deciding what to do with it. That is
// the entire point of the position it occupies.
//
// A local driver may legitimately ignore Credential; it must still not
// invent a Principal it was not given. A remote driver must refuse rather
// than substitute — see internal/drivers/remote.
//
// Driver implements the §3 operations for one runtime on one machine (§4).
// A driver whose implementation is an HTTP client to a peer colab-fleet
// (§4.2, "the remote driver") satisfies this exact same interface — that
// is the entire federation design: if Driver cannot express "a session on
// another machine," Driver is wrong.
//
// # List must return a self-contained envelope, not a bare slice
//
// The spec's §3 operations table writes `list(filter?) -> SessionRef[]`,
// but that pseudocode does not survive transcription intact, for two
// independent reasons:
//
//  1. §9 requires every plural response to be a Collection envelope with
//     Sources, never a bare array — and api-http.md §3.2 confirms this
//     applies even to a single machine answering for itself alone
//     (scope=local still "carries exactly one SourceStatus").
//  2. §13.2 requires a service proxying a peer's answer to *adopt* that
//     peer's own self-reported SourceStatus rather than manufacture a
//     fresh "ok" from the mere fact that the call succeeded. A remote
//     driver's List, under the hood, receives a Collection (with the
//     peer's own SourceStatus already inside it) over HTTP; if List's
//     return type were a bare []Session, the remote driver would have no
//     choice but to unwrap and discard that SourceStatus to fit the
//     signature — which is exactly the bug §13.2 names.
//
// So List returns Collection[Session], not []SessionRef: full Session
// values (state included) so a req enumerating N sessions never has to
// follow up with N State() calls — measurement on the first two drivers
// found per-session subprocess spawns dominate cost at roughly 2 per
// session — and a Collection so a local driver's single self-report and a
// remote driver's relayed peer-report are the same shape at this
// interface, not two.
//
// A driver that implements List by looping State() internally has
// reproduced the cost this interface exists to avoid; that is a
// correct-looking bug, not a style nit.
type Driver interface {
	// Capabilities declares what this driver can do (§4.3). Must satisfy
	// fleet.DriverCapabilities.Validate (§4.4: DeadlineMs > 0) — enforced
	// at registration by the service, not here.
	Capabilities() fleet.DriverCapabilities

	// Create starts a session (§3). key is the caller-supplied idempotency
	// key (§10, api-http.md §3.3: "Idempotency-Key is required, not
	// optional") — a repeat key within the retention window must return
	// the existing SessionRef rather than creating a second session.
	Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.SessionRef, error)

	// Send delivers input (§3). Unlike Create, Send is not idempotent and
	// must not pretend to be (§10) — repeat delivery is a legitimate
	// req intent.
	Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts SendOptions) (fleet.DeliveryReceipt, error)

	// State reads current state (§3). May return fleet.StatusUnknown as an
	// ordinary, successful result (§2.3) — that is not the same thing as
	// this method returning an error.
	State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error)

	// Interrupt and Close express intent only (§3, api-http.md §3.3: both
	// wire to 202 Accepted); confirmation of what actually happened
	// arrives later as a state change on the event stream (§4), never as
	// this call's return value.
	Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error)
	Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error)

	// List returns every session this driver knows about in one call —
	// see the type-level doc comment above for why the signature is
	// Collection[Session], not []SessionRef.
	List(ctx context.Context, req fleet.Request, filter ListFilter) (fleet.Collection[fleet.Session], error)

	// Subscribe opens a live event stream (§3, §5.5). A driver that cannot
	// support subscriptions returns ErrUnsupported rather than emulating
	// one with polling underneath (§5.6).
	Subscribe(ctx context.Context, req fleet.Request, filter SubscribeFilter) (EventStream, error)
}
