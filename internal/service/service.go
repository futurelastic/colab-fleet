// Package service is the machine-local colab-fleet instance:
// session-abstraction.md §13's "one service per machine, proxying to
// peers." Service owns local drivers (one per runtime, §4.1) and peer
// drivers (one per configured remote machine, §4.2), and NewMux wires the
// HTTP surface of docs/spec/api-http.md over it.
//
// Nothing here is a "real driver" — see internal/drivers/stub, the only
// driver wired up by cmd/colab-fleetd today. This package is the routing,
// deadline, idempotency, auth, and error-mapping skeleton the task asked
// for: real request parsing and real envelope construction, over a
// deliberately fake backend.
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// eventSequence is what §7.3 needs to survive a restart.
//
// Persisting the epoch is only honest if the cursor continues with it. An
// epoch says "this is the same sequence"; a service that kept its epoch while
// restarting its cursor at zero would hand subscribers numbers it had already
// used, which is worse than announcing a new instance.
//
// The retained event WINDOW is deliberately not persisted. A subscriber
// resuming from an old cursor therefore gets resync_required — correct, and
// announced as cursor_expired rather than epoch_changed, which is the truthful
// reason: the sequence continued, this service simply cannot replay that far
// back. Persisting the window would buy transparent restarts at the cost of
// durably storing every event, which is a much larger mechanism than the
// problem justifies.
type eventSequence struct {
	Epoch  string `json:"epoch"`
	Cursor int64  `json:"cursor"`
}

// Scope selects how far a plural query reaches (api-http.md §3.2).
type Scope string

const (
	// ScopeLocal restricts a query to this machine's own drivers only, and
	// must never recurse into peers (§13.1). This is what a peer receiving
	// a proxied call answers with — "a proxied request asks for the
	// peer's LOCAL view only."
	ScopeLocal Scope = "local"
	// ScopeFleet fans out to every configured peer, one hop deep, and
	// merges their answers (§13). The default for external clients
	// (api-http.md §3.2).
	ScopeFleet Scope = "fleet"
)

// Service is one machine's colab-fleet instance. A peer driver is, from
// this package's point of view, just another driver.Driver — the whole
// point of §4.2 is that federation needs no separate interface, only a
// second kind of registration.
type Service struct {
	self      fleet.MachineId
	epoch     string
	startedAt time.Time
	// build is read once at construction: it describes the binary, which
	// cannot change while the binary runs.
	build fleet.Build

	// peerCredential authorizes this service's OWN long-lived reads from
	// peers — specifically the event subscriptions the hub multiplexes.
	//
	// This is deliberately not the confused deputy D1 removed, and the
	// difference is worth being precise about. A unary proxied call has
	// exactly one caller, so it must present that caller's authority, and a
	// proxy holding its own credential could silently substitute it. A
	// multiplexed subscription has MANY callers at once and outlives any of
	// them: there is no single original caller whose authority it could
	// carry, so pretending otherwise would mean picking one subscriber's
	// credential and serving everyone else under it — which is worse than
	// acting openly as the service.
	//
	// So the service subscribes as itself. §6 permits reads broadly and
	// gates mutations, and a subscription is a read; nothing mutating ever
	// uses this. The residual widening is real and recorded as §14 D9: with
	// one shared token, "as itself" and "as any caller" are the same
	// authority, and only per-peer identity can separate them.
	peerCredential string

	state *state.Store

	// events is the service-wide event plane: §7.3's cursor and epoch live
	// here because they are per service instance, not per driver.
	events *hub

	mu    sync.RWMutex
	local map[fleet.RuntimeId]driver.Driver
	peers map[fleet.MachineId]driver.Driver
}

// New constructs a Service without durable state. See NewWithState.
func New(self fleet.MachineId) *Service {
	svc, _ := NewWithState(self, nil)
	return svc
}

// NewWithState constructs a Service that remembers across restarts (§7.3,
// §10, §12). A nil store means in-memory only, which is a legitimate
// configuration and not a degraded one.
//
// self is this machine's own identity, used to stamp SourceStatus entries this
// service produces directly — as opposed to adopting a peer's own self-report
// (§13.2).
func NewWithState(self fleet.MachineId, st *state.Store) (*Service, error) {
	svc := newService(self)
	svc.state = st
	if st == nil {
		return svc, nil
	}

	var seq eventSequence
	found, err := st.Load("events", &seq)
	if err != nil {
		return nil, err
	}
	if found && seq.Epoch != "" {
		svc.epoch = seq.Epoch
		svc.events.epoch = seq.Epoch
		svc.events.cursor = seq.Cursor
		svc.events.persist = func(c int64) {
			_ = st.Save("events", eventSequence{Epoch: seq.Epoch, Cursor: c})
		}
		return svc, nil
	}
	epoch := svc.epoch
	svc.events.persist = func(c int64) {
		_ = st.Save("events", eventSequence{Epoch: epoch, Cursor: c})
	}
	return svc, st.Save("events", eventSequence{Epoch: epoch})
}

func newService(self fleet.MachineId) *Service {
	now := time.Now()
	return &Service{
		self: self,
		// epoch identifies this service instance (§7.3): it changes on
		// every restart, exactly what a subscriber's stale epoch needs to
		// compare against. Derived from startedAt rather than a random id
		// — no dependency needed for a UUID, and it is human-legible in
		// GET /v1/health.
		epoch:     now.UTC().Format(time.RFC3339Nano),
		startedAt: now,
		build:     fleet.SelfBuild(),
		local:     make(map[fleet.RuntimeId]driver.Driver),
		peers:     make(map[fleet.MachineId]driver.Driver),
		events:    newHub(self, now.UTC().Format(time.RFC3339Nano)),
	}
}

// Events opens a subscription on this service's event plane (§7.3, §13).
//
// from* carry what the caller last saw. A caller resuming with a cursor this
// service can no longer supply, or with another instance's epoch, is told to
// resync rather than resumed from an arbitrary point — the whole of §7.3.
//
// The returned cancel must be called; it releases the subscriber and, when the
// last one leaves, stops the driver-side stream so an idle service watches
// nothing.
func (s *Service) Events(ctx context.Context, scope Scope, filter driver.SubscribeFilter, fromCursor int64, fromEpoch string) (<-chan fleet.Event, []fleet.Event, bool, func()) {
	sub, backlog, needResync := s.events.add(scope, filter, fromCursor, fromEpoch)
	s.ensureStream(ctx)
	return sub.ch, backlog, needResync, func() {
		s.events.remove(sub.id)
		s.ensureStream(ctx)
	}
}

// SetPeerCredential supplies the authority for this service's own peer
// subscriptions. See the field comment; without it, peer streams are refused
// and the fleet's event plane is silently local-only.
func (s *Service) SetPeerCredential(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerCredential = tok
}

func (s *Service) peerRequest() fleet.Request {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fleet.Request{Caller: fleet.Caller{
		Principal:  "system:" + string(s.self),
		Credential: s.peerCredential,
	}}
}

// Epoch reports this instance's epoch (§7.3).
func (s *Service) Epoch() string { return s.epoch }

// Self reports this machine's own id. The HTTP layer needs it to tell a
// request about this machine from one destined for a peer — two different
// permissions (§6, §14 D6).
func (s *Service) Self() fleet.MachineId { return s.self }

// RegisterLocalDriver adds a driver for one runtime on this machine. It
// rejects a driver whose declared capabilities fail Validate (§4.4) — an
// undeadlined driver is refused at registration, not discovered by a
// req blocking forever.
func (s *Service) RegisterLocalDriver(runtime fleet.RuntimeId, d driver.Driver) error {
	if err := d.Capabilities().Validate(); err != nil {
		return fmt.Errorf("service: registering local driver %q: %w", runtime, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.local[runtime] = d
	return nil
}

// RegisterPeerDriver adds a driver fronting one peer machine (§4.2; §7.2 —
// peers are statically configured, never discovered or announced).
func (s *Service) RegisterPeerDriver(machine fleet.MachineId, d driver.Driver) error {
	if err := d.Capabilities().Validate(); err != nil {
		return fmt.Errorf("service: registering peer driver %q: %w", machine, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers[machine] = d
	return nil
}

func (s *Service) localDrivers() []driver.Driver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]driver.Driver, 0, len(s.local))
	for _, d := range s.local {
		out = append(out, d)
	}
	return out
}

func (s *Service) peerDrivers() map[fleet.MachineId]driver.Driver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[fleet.MachineId]driver.Driver, len(s.peers))
	for m, d := range s.peers {
		out[m] = d
	}
	return out
}

// effectiveDeadline is api-http.md §3.3's rule: "a req may shorten a
// driver's declared deadlineMs, never extend it."
func effectiveDeadline(driverMs int64, req time.Duration) time.Duration {
	d := time.Duration(driverMs) * time.Millisecond
	if req > 0 && req < d {
		return req
	}
	return d
}

// ListSessions answers GET /v1/sessions (api-http.md §3.2). ScopeLocal
// never touches peers (§13.1); ScopeFleet asks every local driver plus
// every peer driver and merges the results into one envelope.
func (s *Service) ListSessions(ctx context.Context, req fleet.Request, scope Scope, filter driver.ListFilter, callerDeadline time.Duration) (fleet.Collection[fleet.Session], error) {
	var items []fleet.Session
	var sources []fleet.SourceStatus

	for _, d := range s.localDrivers() {
		its, srcs := s.callList(ctx, req, s.self, d, filter, callerDeadline)
		items = append(items, its...)
		sources = append(sources, srcs...)
	}

	if scope == ScopeFleet {
		for machine, d := range s.peerDrivers() {
			its, srcs := s.callList(ctx, req, machine, d, filter, callerDeadline)
			items = append(items, its...)
			sources = append(sources, srcs...)
		}
	}

	if len(sources) == 0 {
		// No drivers registered at all for this scope. Still must not
		// return a bare, sourceless Collection (§9) — the service itself
		// is the one source, honestly answering "nothing here."
		sources = []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: time.Now()}}
	}

	return fleet.NewCollection(items, sources)
}

// callList invokes one driver's List and folds its result into the
// aggregate envelope ListSessions is building.
//
// A local driver's own Collection already carries a SourceStatus (see
// driver.Driver.List's doc comment); for a peer's remote driver, that
// inner SourceStatus is the peer's own self-report and is adopted
// verbatim here — never re-synthesized as a fresh "ok" (§13.2). This
// function only manufactures a SourceStatus itself for the case a driver
// returns a bare Go error instead of ever reaching a Collection: deadline
// exceeded, unsupported, or anything else that short-circuited before
// building an envelope of its own.
func (s *Service) callList(ctx context.Context, req fleet.Request, machine fleet.MachineId, d driver.Driver, filter driver.ListFilter, callerDeadline time.Duration) ([]fleet.Session, []fleet.SourceStatus) {
	deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
	callCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	col, err := d.List(callCtx, req, filter)
	now := time.Now()

	if err == nil {
		if len(col.Sources()) > 0 {
			return col.Items(), col.Sources()
		}
		// Belt-and-braces: NewCollection already makes a sourceless
		// Collection hard to construct, but a driver could still return
		// one out of Collection[T]{}'s zero value without going through
		// it. Treat that as this driver's own report rather than
		// silently dropping it.
		return col.Items(), []fleet.SourceStatus{{Machine: machine, Status: fleet.SourceOK, ObservedAt: now}}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return nil, []fleet.SourceStatus{{
			Machine:    machine,
			Status:     fleet.SourceUnreachable,
			Error:      fmt.Sprintf("no answer within %s", deadline),
			ObservedAt: now,
		}}
	}

	// Anything else — including driver.ErrUnsupported — means this source
	// answered, just not usefully. SourceState's closed set (§9) has no
	// member for "reachable but this operation isn't supported"; degraded
	// is the nearest honest fit. Recorded as a finding (see the root
	// package's doc comment), not silently decided.
	return nil, []fleet.SourceStatus{{
		Machine:    machine,
		Status:     fleet.SourceDegraded,
		Error:      err.Error(),
		ObservedAt: now,
	}}
}

// ListMachines answers GET /v1/machines (api-http.md §3.1).
//
// The Driver interface (§3) has no health/ping operation — List is the
// cheapest existing operation to reuse as a liveness probe: if a driver
// answers within its deadline at all, whether with sessions or with
// ErrUnsupported, the machine it fronts is reachable. This is a real,
// working mechanism (not a fabricated one) built from an operation the
// interface already has, and it is a genuine limitation: a driver that
// always errs fast still reads as reachable here, because reachability and
// capability are different facts and this endpoint only ever asks about
// the first. See the root package's doc comment findings list.
func (s *Service) ListMachines(ctx context.Context, req fleet.Request, callerDeadline time.Duration) (fleet.Collection[fleet.MachineInfo], error) {
	now := time.Now()
	items := []fleet.MachineInfo{{Machine: s.self, Self: true, Status: fleet.SourceOK, ObservedAt: now}}
	sources := []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: now}}

	for machine, d := range s.peerDrivers() {
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
		callCtx, cancel := context.WithTimeout(ctx, deadline)
		_, err := d.List(callCtx, req, driver.ListFilter{})
		cancel()

		status := fleet.SourceOK
		errText := ""
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			status = fleet.SourceUnreachable
			errText = fmt.Sprintf("no answer within %s", deadline)
		}
		observed := time.Now()
		items = append(items, fleet.MachineInfo{Machine: machine, Self: false, Status: status, ObservedAt: observed})
		sources = append(sources, fleet.SourceStatus{Machine: machine, Status: status, Error: errText, ObservedAt: observed})
	}

	return fleet.NewCollection(items, sources)
}

// ListRuntimes answers GET /v1/runtimes (api-http.md §3.1), local drivers
// only — the wire example does not show a scope parameter for this
// endpoint, and this skeleton does not invent fanning it out to peers.
func (s *Service) ListRuntimes(ctx context.Context) (fleet.Collection[fleet.RuntimeInfo], error) {
	now := time.Now()
	var items []fleet.RuntimeInfo
	sources := []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: now}}

	s.mu.RLock()
	for rt, d := range s.local {
		items = append(items, fleet.RuntimeInfo{Machine: s.self, Runtime: rt, Capabilities: d.Capabilities()})
	}
	peers := make(map[fleet.MachineId]driver.Driver, len(s.peers))
	for m, d := range s.peers {
		peers[m] = d
	}
	s.mu.RUnlock()

	// Peers are included, and they are the whole reason this endpoint
	// matters. api-http.md §3.1 tells clients they MUST consult this before
	// relying on a capability — a rule that was unfollowable for peer
	// runtimes while this listed only local ones, which is exactly the case
	// where a caller cannot simply know.
	//
	// No network call is made. A peer driver answers from whatever it has
	// cached, and says so: §4.3's `source` reports `assumed` when nobody has
	// told it anything and `observed` when the peer has. That distinction is
	// what makes reporting a cache honest rather than misleading, and it is
	// why this endpoint can stay cheap.
	for m, d := range peers {
		caps := d.Capabilities()
		// The peer names its runtime in the same row this driver learned its
		// capabilities from; reporting "" was a placeholder nobody removed.
		// Empty remains possible and remains honest — it means the peer has
		// not answered yet, which the row's `source: assumed` also says.
		var rt fleet.RuntimeId
		if r, ok := d.(interface{ Runtime() fleet.RuntimeId }); ok {
			rt = r.Runtime()
		}
		items = append(items, fleet.RuntimeInfo{Machine: m, Runtime: rt, Capabilities: caps})
		st := fleet.SourceOK
		if caps.Source != fleet.CapabilitiesObserved {
			// Nothing was reached to produce this row. §5.7: that is not
			// the same as a peer that answered, and the envelope must not
			// present it as one.
			st = fleet.SourceDegraded
		}
		sources = append(sources, fleet.SourceStatus{
			Machine: m, Status: st, ObservedAt: now,
			Error: map[bool]string{true: "", false: "capabilities not yet obtained from this peer"}[st == fleet.SourceOK],
		})
	}
	return fleet.NewCollection(items, sources)
}

type driverSummary struct {
	Machine      fleet.MachineId          `json:"machine"`
	Runtime      fleet.RuntimeId          `json:"runtime"`
	Capabilities fleet.DriverCapabilities `json:"capabilities"`
}

func (s *Service) driverSummaries() []driverSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]driverSummary, 0, len(s.local))
	for rt, d := range s.local {
		out = append(out, driverSummary{Machine: s.self, Runtime: rt, Capabilities: d.Capabilities()})
	}
	return out
}

// counterSnapshot answers #9: a driver's own counters, read through
// driver.CounterReporter, keyed by runtime rather than merged into one flat
// map. Two local drivers are already possible (RegisterLocalDriver has no
// limit of one), and a name collision between them silently merging two
// different facts into one count would be a worse bug than the one this
// registry exists to prevent — see counters.go's own reasoning for why a
// count that cannot be told apart from another is not a count.
//
// A driver that does not implement the interface is left out entirely,
// never given a zeroed entry: an empty map would claim "nothing happened"
// about a driver that was never asked, which is not the same statement as
// #9's own distinction between "not measured" and "cannot be measured".
func (s *Service) counterSnapshot() map[fleet.RuntimeId]map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[fleet.RuntimeId]map[string]int64, len(s.local))
	for rt, d := range s.local {
		reporter, ok := d.(driver.CounterReporter)
		if !ok {
			continue
		}
		out[rt] = reporter.Counters()
	}
	return out
}

// ErrAmbiguousSession documents the finding recorded in the root package's
// doc comment: the single-session URL shape (api-http.md §3.3) carries no
// runtime segment, but SessionRef.ID is scoped to (machine, runtime), not
// machine alone (session-abstraction.md §2.2). Two runtimes on one machine
// can legally reuse the same id.
var ErrAmbiguousSession = errors.New("service: more than one local runtime is registered; pass ?runtime= to disambiguate (api-http.md §3.3)")

// resolveSessionDriver finds the Driver responsible for an existing or
// about-to-be-created session addressed by (machine, runtimeHint).
//
//   - machine == a configured peer: that peer's single remote driver
//     handles it — the peer resolves its own runtimes locally, and this
//     service never recurses into asking the peer to disambiguate (§13.1).
//   - machine == self, runtimeHint given: routes directly to that local
//     driver.
//   - machine == self, runtimeHint empty: resolves iff exactly one local
//     driver is registered; otherwise ErrAmbiguousSession, since the
//     spec's URL shape as written cannot otherwise say which runtime a
//     bare id belongs to.
func (s *Service) resolveSessionDriver(machine fleet.MachineId, runtimeHint fleet.RuntimeId) (driver.Driver, *fleet.Error) {
	if machine != s.self && machine != "" {
		s.mu.RLock()
		d, ok := s.peers[machine]
		s.mu.RUnlock()
		if !ok {
			return nil, &fleet.Error{Kind: fleet.ErrorNotFound, Message: "unknown machine", Machine: machine}
		}
		return d, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if runtimeHint != "" {
		d, ok := s.local[runtimeHint]
		if !ok {
			return nil, &fleet.Error{Kind: fleet.ErrorNotFound, Message: "unknown runtime", Machine: machine}
		}
		return d, nil
	}

	switch len(s.local) {
	case 0:
		return nil, &fleet.Error{Kind: fleet.ErrorNotFound, Message: "no local drivers registered", Machine: machine}
	case 1:
		for _, d := range s.local {
			return d, nil
		}
	}
	return nil, &fleet.Error{Kind: fleet.ErrorInvalid, Message: ErrAmbiguousSession.Error(), Machine: machine}
}
