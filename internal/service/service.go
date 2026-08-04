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
)

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

	mu    sync.RWMutex
	local map[fleet.RuntimeId]driver.Driver
	peers map[fleet.MachineId]driver.Driver
}

// New constructs a Service. self is this machine's own identity, used to
// stamp SourceStatus entries this service produces directly — as opposed
// to adopting a peer's own self-report (§13.2).
func New(self fleet.MachineId) *Service {
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
		local:     make(map[fleet.RuntimeId]driver.Driver),
		peers:     make(map[fleet.MachineId]driver.Driver),
	}
}

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
		items = append(items, fleet.RuntimeInfo{Machine: m, Runtime: "", Capabilities: caps})
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
