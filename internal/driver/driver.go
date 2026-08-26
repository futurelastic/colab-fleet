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

// ErrNotReady is returned by a Driver method the substrate CAN support but
// cannot serve at this moment. It is the transient sibling of ErrUnsupported,
// and the distinction is load-bearing rather than decorative.
//
// ErrUnsupported is a statement about the substrate: nothing will change by
// asking again, so a caller may give up permanently. ErrNotReady is a
// statement about right now, and a caller that treats it as the former stops
// asking forever on the strength of a condition that cleared seconds later.
//
// The measured case is Subscribe on the first driver: its control mode has no
// unattached form, so with no sessions on the machine there is nothing to
// attach a lifecycle client to. That was reported as ErrUnsupported, the
// service's stream pump read it as "this substrate cannot stream" and returned
// for good, and every subscriber then held an open, healthy-looking,
// permanently empty stream — a machine that started its first session five
// minutes later reported it to nobody. The substrate was never incapable; it
// was empty, which is a different fact and now has a different error.
var ErrNotReady = errors.New("driver: capability not available yet")

// SendOptions carries the optional fields of a Send call (the wire body's
// "submit", api-http.md §3.3).
type SendOptions struct {
	Submit bool

	// ResumeIfStranded completes a delivery this driver already made and
	// could not confirm.
	//
	// `send` refuses to append to a busy composer (§2.4), so when its own
	// confirm-before-submit times out the text is left sitting there and the
	// caller has no way back in: a second send is refused by the very rule
	// that protects it, and nothing else submits.
	//
	// With this set, a driver may submit what is in the composer ONLY if it
	// can establish the text is the text it delivered — from its own record,
	// not by reading the screen back, because a multi-line paste is collapsed
	// to a summary and cannot be compared (F49).
	//
	// It must never submit text it did not put there. Composer contents are
	// not evidence that anyone meant to send them: the runtime redraws the
	// last submitted message as a placeholder, and a human's half-typed line
	// looks the same to a screen reader as a finished one.
	ResumeIfStranded bool

	// ReplaceIfStranded (colab-fleet #112) clears a composer holding a
	// delivery this driver made and could not confirm, then delivers THIS
	// call's text in its place — the door out for a caller that wants
	// DIFFERENT text, not to finish the stranded one. ResumeIfStranded only
	// ever completes the SAME delivery; a caller that has decided it wants
	// something else has no use for it and, before this flag existed, no
	// other way in either.
	//
	// A driver may act on this ONLY when its own record — never a reading of
	// the composer, and never an inference from this flag alone — shows the
	// text currently there is the SAME text it placed and has not been
	// disturbed since (a corroborating digest, not merely "a record exists
	// for this session"). A human may have attached and typed something new
	// in the gap between the strand and this call, and a driver cannot
	// compare pasted bytes to rule that out (F49) — so this must never be
	// inferred from ResumeIfStranded being false, or from a record merely
	// existing; it is always an explicit, separate opt-in, and both flags
	// set together is a contradiction a driver refuses outright rather than
	// resolves by picking one.
	ReplaceIfStranded bool
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
	// the existing Session rather than creating a second session.
	//
	// # Why this returns fleet.Session, not fleet.SessionRef (colab-fleet
	// # #84, #85, #86)
	//
	// The driver is the only party in this service that knows what a create
	// actually did — which pin the runtime substituted or refused, whether a
	// surface it operates came up under this session, whether a create-time
	// prompt was delivered. A caller of Create that received only a
	// SessionRef had nothing to build the create response from except the
	// SessionSpec it was handed back, which is the caller's own request
	// wearing this service's voice: three separate, measured defects, all
	// the same shape — a response reporting what was REQUESTED because the
	// only other party in a position to report what was APPLIED had no
	// channel to say so.
	//
	// A driver that cannot fill a field of the returned Session leaves it at
	// its zero value — §5.7, the same discipline every other field on this
	// type already holds itself to. This is deliberately the same shape
	// List already returns per session, not a second, poorer shape: a
	// caller reading the 201 body and the first 200 body of the same
	// session should never be able to tell they came from different code.
	Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.Session, error)

	// Send delivers input (§3). Unlike Create, Send is not idempotent and
	// must not pretend to be (§10) — repeat delivery is a legitimate
	// req intent.
	Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts SendOptions) (fleet.DeliveryReceipt, error)

	// State reads current state (§3). May return fleet.StatusUnknown as an
	// ordinary, successful result (§2.3) — that is not the same thing as
	// this method returning an error.
	State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error)

	// Respond answers a prompt the session is blocked on (§3). A driver
	// must REFUSE when no prompt is present: a keypress delivered to a
	// session that is not asking anything is a stray input the caller never
	// intended, and on this substrate it lands in whatever the session was
	// doing.
	Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error)

	// Interrupt and Close express intent only (§3, api-http.md §3.3: both
	// wire to 202 Accepted); confirmation of what actually happened
	// arrives later as a state change on the event stream (§4), never as
	// this call's return value.
	Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error)
	Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error)

	// Discard removes unsent text from the composer WITHOUT submitting it.
	//
	// The missing verb between "run it" and "destroy the session that holds
	// it". `send` delivers and submits, `respond` answers a question, `close`
	// destroys the session — so a caller holding text that must never be
	// submitted had no safe move at all.
	//
	// expectDigest is SessionState.ComposerDigest as the caller last saw it.
	// A driver must refuse when it does not match what is there now: this
	// destroys somebody's typing, and a caller that has not seen the current
	// text has no business deleting it. Empty means the caller is discarding
	// blind and a driver may refuse outright.
	//
	// Discarding an already-empty composer succeeds. A caller retrying after a
	// timeout must not be told it failed for having previously worked.
	Discard(ctx context.Context, req fleet.Request, ref fleet.SessionRef, expectDigest string) (fleet.Ack, error)

	// Rename changes a session's id.
	//
	// Corroborated exactly as Close is, and for the same reason: it acts on one
	// specific session, and an id alone does not identify one (§5.4).
	//
	// A driver that cannot rename returns ErrUnsupported rather than
	// approximating one — §5.6, degrade never emulate. Renaming is not
	// universal across substrates, and on some the id is not a name at all.
	//
	// On success the service emits EventSessionRenamed so subscribers
	// filtering by id can re-key.
	Rename(ctx context.Context, req fleet.Request, ref fleet.SessionRef, to string) (fleet.Ack, error)

	// List returns every session this driver knows about in one call —
	// see the type-level doc comment above for why the signature is
	// Collection[Session], not []SessionRef.
	List(ctx context.Context, req fleet.Request, filter ListFilter) (fleet.Collection[fleet.Session], error)

	// Subscribe opens a live event stream (§3, §5.5). A driver that cannot
	// support subscriptions returns ErrUnsupported rather than emulating
	// one with polling underneath (§5.6).
	Subscribe(ctx context.Context, req fleet.Request, filter SubscribeFilter) (EventStream, error)
}

// EnvironmentReporter is an OPTIONAL capability: a driver that can say what
// environment a session's process actually received.
//
// Optional rather than part of Driver, because this is not a question every
// substrate can answer and adding it to the interface would force every driver
// to write a stub that says so. A service type-asserts for it and reports the
// absence honestly (§5.7), which is the same shape as any other capability
// nobody has claimed.
//
// The record must never carry variable VALUES — see fleet.SessionEnvironment.
// The reason the type exists at all is that the environment in question is
// the one holding credentials.
type EnvironmentReporter interface {
	Environment(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionEnvironment, error)
}

// KeySender is an OPTIONAL capability: a driver that can deliver a raw key
// event to a session's screen.
//
// Optional rather than part of Driver, the same trade EnvironmentReporter
// makes. Pressing a key is substrate-specific in a way list and send are not —
// a runtime driven by an API may have no notion of one — and putting it on the
// interface would force every driver to write a stub whose only content is
// that it cannot. A service type-asserts for it and reports the absence as
// `unsupported`, honestly, which is what §5.6 asks for instead of an emulation.
//
// # What a driver implementing this owes the caller
//
// A raw key goes to a screen nobody classified, so there is no prompt and no
// SessionPrompt.Nonce to check an answer against. Three obligations replace it,
// and a driver that skips any of them has built a way to press Enter on a
// session at random:
//
//  1. REFUSE on a stale expectDigest. It is SessionState.ScreenDigest as the
//     caller last saw it. Empty means the caller is pressing keys on a screen
//     it has not read, and must be refused outright — the same ruling `discard`
//     already makes about deleting text nobody looked at.
//  2. REFUSE when the composer holds unsent text. `Enter` there submits
//     somebody's half-typed message, which is the precise harm `send` refuses
//     to cause by appending to a busy composer; this must not become the way
//     around that.
//  3. CONFIRM, or say it could not. A key that landed on a dialog changes the
//     screen; a key the dialog swallowed does not. An unchanged screen is
//     reported as OutcomeUnknown with the reason saying so — never as
//     submitted, because a supervisor told a keypress landed stops trying.
//
// One key per call, and that is the interface, not a convenience. After the
// first key the screen is different, so every key after it in a batch would be
// delivered against a digest describing something that no longer exists — which
// is exactly what the digest was added to prevent.
type KeySender interface {
	Keys(ctx context.Context, req fleet.Request, ref fleet.SessionRef, key fleet.KeyName, expectDigest string) (fleet.DeliveryReceipt, error)
}

// CounterReporter is another OPTIONAL capability, same shape as
// EnvironmentReporter: a driver that keeps its own named counters (see
// internal/drivers/tmux/counters.go) and can hand back a snapshot of them.
//
// Optional for the same reason: not every substrate accumulates a count
// worth exposing, and forcing every driver to implement a method that
// returns an empty map would be a stub written for no reader.
//
// Counters is a plain synchronous read of in-memory state, unlike
// Environment — there is no substrate round trip to bound, so it takes
// neither a context nor a caller. The registry it reads already states its
// own constraint: an integer per name, never anything drawn from a
// session's screen. #9 is the first caller of this snapshot; it is read
// through GET /v1/health, next to startedAt, so a reader has the divisor
// that turns a count into a rate without a second call.
type CounterReporter interface {
	Counters() map[string]int64
}

// BuildReporter is another OPTIONAL capability, same shape as
// CounterReporter: a driver fronting a PEER machine that has learned, by
// probing that peer's own /v1/health, which code it is running.
//
// Optional rather than a Driver method because a LOCAL driver's build is
// this service's own — already known as fleet.SelfBuild() at construction,
// with nothing to probe — so forcing every driver to implement a method
// that would only ever repeat that same self-evident value is a stub
// written for no reader. Only a driver fronting a peer ever has something
// worth saying here, and only the remote driver implements it today.
//
// A driver that does not implement this interface must read as an unknown
// build (fleet.Build{}, Known: false) to its caller, never as a zero value
// that looks like a plausible answer — colab-fleet #121, and the same
// discipline fleet.Build's own doc comment states for §5.7.
type BuildReporter interface {
	Build() fleet.Build
}
