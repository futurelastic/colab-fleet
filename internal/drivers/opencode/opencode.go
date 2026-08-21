// Package opencode implements driver.Driver over opencode
// (github.com/sst/opencode), a third-party open-source coding agent this
// driver launches as a subprocess and talks to over HTTP.
//
// This is colab-fleet's second local driver, and its whole reason to exist
// is colab-fleet issue #55: the first driver against a runtime genuinely
// different from the incumbent multiplexer-driven one, chosen because it
// answers "is this session busy" from a structured API rather than a
// terminal screen. Where the tmux driver infers, this one observes — see
// Capabilities, and state.go's classify.
//
// # We launch it, so discovery is not a problem here
//
// opencode's `serve` subcommand takes --port and --hostname directly, and
// its `acp` subcommand (Agent Client Protocol — a structured stdio channel)
// starts the SAME HTTP server underneath with the same flags. One
// integration therefore covers both surfaces; this driver only needs the
// HTTP one. Because this driver is the one that execs the process, there is
// no port file, no environment variable and no mDNS advertisement to trust:
// the port is chosen by binding an ephemeral listener and closing it
// (process.go's freePort), and the credential is generated here and handed
// to the child only through its environment (process.go's New). --mdns is
// never passed — it defaults the bind to 0.0.0.0, which nothing here wants.
//
// # This driver's session universe is what it has cached, not a runtime enumeration
//
// GET /session (list every session) is documented as returning the whole
// collection, unfiltered when no `directory` query parameter is given.
// Measured against a real server, it did not: a query with no filter,
// against a session this very driver had just created and could
// immediately read back with GET /session/{id}, returned zero results.
// The endpoint appears to be scoped to some notion of a "current project"
// this driver never set, which is not documented anywhere the OpenAPI
// description exposes — see List's doc comment for the measurement.
//
// So this driver never calls it. Every session it knows about is cached
// locally from the moment it was created or last confirmed by a direct
// by-id read (the seen map, written by Create and by State's existence
// check) — the same shape as tmux driver's own observed map, arrived at
// for an unrelated reason. List answers entirely from that cache plus one
// call to the status endpoint, never from a runtime-side enumeration.
//
// That has one direct consequence: SupportsResume is false. This driver's
// cache does not survive its own process restarting, so a restarted driver
// reports none of its pre-restart sessions — even though opencode's own
// SQLite store still has them — until they are recreated or a future
// revision adds a way to rediscover them (by-id GET works regardless of
// which process created a session, so this is a real gap to close, not a
// structural one).
//
// # Two traps from #55's measured findings, both handled in state.go
//
// The status endpoint (GET /session/status) omits idle sessions from its
// response map entirely — measured live, not read from source. A read that
// fails (network error, or the runtime rejecting this driver's own
// credential) renders as exactly the same empty-looking absence at the
// HTTP layer as "everyone is idle" would, and §5.7 forbids conflating them.
// See client.go's do, which turns a transport failure or a 401 into a Go
// error rather than an empty successful body, and state.go's classify,
// which is never even called unless the read that fed it demonstrably
// succeeded.
//
// The runtime's own status union also has no member for what a
// multi-session listing must do when it can enumerate *which* sessions
// exist but not *how busy* they are (session list succeeded, status read
// failed): see List's use of fleet.UnknownState per session in that case,
// which is the finer-grained sibling of the same rule.
//
// # What this driver deliberately does not attempt
//
// Respond, Discard, Rename and Keys all return driver.ErrUnsupported.
// opencode has no boot-time enumerated-menu prompt matching
// fleet.SessionPrompt's shape — its blocking questions are tool-permission
// approvals and, in a newer API, structured "question" replies, neither of
// which is a safe fit for §2.3's model without real design work of its own
// (a session-abstraction concept, not a wire mapping exercise). Inventing
// one to make the method non-empty would be exactly the emulation §5.6
// forbids. Likewise Subscribe: opencode publishes a global SSE event bus
// (GET /event) that could in principle back a real Subscribe, but wiring
// its event vocabulary honestly onto SubscribeFilter and fleet.Event is
// its own piece of work and is left undone here rather than half-mapped.
// KeySender is not implemented at all — this substrate has no screen for a
// raw key to land on, and DeliversRawKeys: false says so structurally
// rather than through a stub that always refuses.
package opencode

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// DefaultRuntime is the runtime id this driver registers under.
const DefaultRuntime fleet.RuntimeId = "opencode"

const (
	defaultBin        = "opencode"
	defaultUsername   = "colab-fleet"
	defaultDeadlineMs = 8000
	readyTimeout      = 10 * time.Second
	readyPollInterval = 100 * time.Millisecond
)

// Driver implements driver.Driver over one opencode server this driver
// itself owns the lifecycle of.
type Driver struct {
	machine  fleet.MachineId
	runtime  fleet.RuntimeId
	bin      string
	workdir  string
	deadline time.Duration
	now      func() time.Time

	baseURL  string
	username string
	// password is this driver's own credential for its own opencode
	// server — never a caller's, and a local driver ignores
	// fleet.Caller.Credential entirely (see caller.go: "a local driver
	// typically uses the first [Principal] and ignores the second
	// [Credential]"). Generated once, held only in memory, and passed to
	// the child process through its environment — never argv, never a
	// file this driver writes (Boss's provider ruling on #55). NEVER
	// logged.
	password string
	client   *http.Client

	proc *process // nil when this Driver was built with WithBaseURL for tests

	mu   sync.RWMutex
	seen map[string]knownSession // id -> what this driver has locally cached; see package doc's scope boundary

	idemMu sync.Mutex
	idem   map[string]idemEntry
}

// knownSession is what this driver remembers locally about a session it has
// created or directly read — the cache List reads from instead of the
// runtime's own (unreliable, see List's doc comment) bulk listing endpoint,
// and the memory State's existence check writes back to.
type knownSession struct {
	cwd       fleet.AbsolutePath
	name      string
	agent     string
	startedAt time.Time
}

type idemEntry struct {
	ref     fleet.SessionRef
	created time.Time
}

// idempotencyRetention mirrors the tmux driver's window: long enough to
// cover a caller's realistic retry, short enough not to grow without
// bound. In-memory only, deliberately — see the package doc's SupportsResume
// note; persisting this without also persisting session recovery would be
// half of #10's guarantee, which is worse than honestly declaring neither.
const idempotencyRetention = 10 * time.Minute

// Option configures a Driver.
type Option func(*Driver)

// WithBinary overrides the executable resolved by Probe / New. Empty (the
// default) resolves "opencode" on PATH.
func WithBinary(bin string) Option { return func(d *Driver) { d.bin = bin } }

// WithWorkdir sets the child process's working directory. Empty inherits
// this process's own — opencode does not need to run from any particular
// directory since every session names its own via ?directory=.
func WithWorkdir(dir string) Option { return func(d *Driver) { d.workdir = dir } }

// WithDeadline sets DriverCapabilities.DeadlineMs (§4.4). Non-positive
// values are ignored, so a zero-value Option never produces an
// undeadlined driver.
func WithDeadline(ms int64) Option {
	return func(d *Driver) {
		if ms > 0 {
			d.deadline = time.Duration(ms) * time.Millisecond
		}
	}
}

// WithBaseURL points this driver at an already-running server instead of
// spawning one, and skips Probe entirely. This is the test seam: paired
// with WithHTTPClient it lets state_test.go and client_test.go exercise
// every mapping decision against an httptest.Server, satisfying the
// provider ruling on #55 that the bulk of this driver's tests must not
// require a real opencode install or a paid credential.
func WithBaseURL(url string) Option { return func(d *Driver) { d.baseURL = url } }

// WithHTTPClient replaces the HTTP client. See WithBaseURL.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Driver) {
		if c != nil {
			d.client = c
		}
	}
}

// WithCredential sets the Basic auth identity this driver presents to its
// own server. See WithBaseURL — production callers never need this,
// because New generates a fresh credential and starts the server with it
// in the same call.
func WithCredential(username, password string) Option {
	return func(d *Driver) {
		if username != "" {
			d.username = username
		}
		d.password = password
	}
}

func withClock(f func() time.Time) Option { return func(d *Driver) { d.now = f } }

// New builds a Driver. Unless WithBaseURL was supplied, it probes for the
// opencode binary (never a startup crash when it is absent — see Probe),
// picks a port, generates a credential, starts the server, and waits for
// it to answer before returning.
//
// A non-nil error here is itself the "absent install is a first-class
// answer" contract discharged: it is returned to the caller (ordinarily
// cmd/colab-fleetd/main.go) to decide what to do — log and continue
// without this runtime, in that caller's case — rather than this package
// ever calling log.Fatal or panicking on a machine that simply does not
// have opencode installed.
func New(ctx context.Context, machine fleet.MachineId, opts ...Option) (*Driver, error) {
	d := &Driver{
		machine:  machine,
		runtime:  DefaultRuntime,
		bin:      defaultBin,
		deadline: defaultDeadlineMs * time.Millisecond,
		now:      time.Now,
		username: defaultUsername,
		client:   &http.Client{},
		seen:     make(map[string]knownSession),
		idem:     make(map[string]idemEntry),
	}
	for _, o := range opts {
		o(d)
	}

	if d.baseURL != "" {
		// Test/advanced seam: the caller already has a server (real or
		// fake) and told us where it is. Nothing to spawn or probe.
		return d, nil
	}

	p, err := startProcess(ctx, d.bin, d.workdir, d.username)
	if err != nil {
		return nil, err
	}
	d.proc = p
	d.baseURL = p.baseURL
	d.password = p.password
	return d, nil
}

// Shutdown stops the opencode server this Driver started. A no-op for a
// Driver built with WithBaseURL, which never started one.
func (d *Driver) Shutdown() error {
	if d.proc == nil {
		return nil
	}
	return d.proc.stop()
}

var _ driver.Driver = (*Driver)(nil)

// Capabilities declares what this driver can do (§4.3, #55).
//
// ObservesState is true — the first local driver in this repository able
// to say so honestly, and the entire point of #55: GET /session/status
// returns a structured three-variant union, not a screen a classifier
// pattern-matches. DeliversRawKeys is false because this substrate has no
// screen for a raw key to land on — degrade, not emulate (§5.6). SupportsPin
// reflects Create's own refusals: Model and Agent are genuinely honoured
// (opencode's create body carries both), Effort is not — there is no
// analogous parameter, and Create refuses rather than silently drop it.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	return fleet.DriverCapabilities{
		ObservesState:    true,
		DeliversRawKeys:  false,
		ConfirmsDelivery: false,
		SupportsResume:   false,
		SupportsPin: fleet.PinSupport{
			Model:  true,
			Effort: false,
			Agent:  true,
		},
		DeadlineMs: d.deadline.Milliseconds(),
		// A local driver is describing itself — no network between the
		// claim and its subject (same reasoning as the tmux driver's
		// Capabilities).
		Source: fleet.CapabilitiesObserved,
	}
}

// bounded applies this driver's declared deadline, or the caller's if
// shorter (§4.4).
func (d *Driver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	own := d.now().Add(d.deadline)
	if dl, ok := ctx.Deadline(); ok && dl.Before(own) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, own)
}

// matchesFilter is the same predicate the tmux driver applies — kept
// identical rather than shared, since the two packages otherwise have no
// dependency on each other and one three-field predicate is not worth an
// import for.
func matchesFilter(s fleet.Session, f driver.ListFilter) bool {
	if f.Status != "" && s.State.Status != f.Status {
		return false
	}
	if f.Agent != "" && s.Agent != f.Agent {
		return false
	}
	if f.CwdPrefix != "" && !strings.HasPrefix(string(s.Cwd), f.CwdPrefix) {
		return false
	}
	return true
}

// idemLookup returns a previously-created session for key, if the record
// has not expired.
func (d *Driver) idemLookup(key string) (fleet.SessionRef, bool) {
	d.idemMu.Lock()
	defer d.idemMu.Unlock()
	e, ok := d.idem[key]
	if !ok || d.now().Sub(e.created) > idempotencyRetention {
		return fleet.SessionRef{}, false
	}
	return e.ref, true
}

func (d *Driver) idemStore(key string, ref fleet.SessionRef) {
	d.idemMu.Lock()
	defer d.idemMu.Unlock()
	d.idem[key] = idemEntry{ref: ref, created: d.now()}
}

// markSeen records what this driver knows about id — see the package doc's
// scope-boundary note and knownSession's own doc comment.
func (d *Driver) markSeen(id string, info knownSession) {
	d.mu.Lock()
	d.seen[id] = info
	d.mu.Unlock()
}

func (d *Driver) wasSeen(id string) (knownSession, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	info, ok := d.seen[id]
	return info, ok
}

// forgetSeen removes id from this driver's cache — the counterpart
// markSeen never had (colab-fleet #78). List answers entirely from this
// cache (see knownIDs' own comment on why: the runtime's own bulk listing
// is measured unreliable, not a design this driver chose for its own
// sake), so an id nothing ever prunes from it is an id List reports
// forever, however confidently the runtime itself says otherwise.
//
// Called only once this driver has the runtime's own confirmation the
// session is gone — a successful DELETE, or a 404 on a read that expected
// to find it — never speculatively: forgetting an id this driver might
// still need to corroborate a later call against (§5.4) would trade one
// bug for a worse one.
func (d *Driver) forgetSeen(id string) {
	d.mu.Lock()
	delete(d.seen, id)
	d.mu.Unlock()
}

// knownIDs returns a snapshot of every session this driver has cached,
// keyed by id — used by List, which builds its answer from this cache
// rather than the runtime's own unreliable bulk listing (see List's doc
// comment).
func (d *Driver) knownIDs() map[string]knownSession {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]knownSession, len(d.seen))
	for id, info := range d.seen {
		out[id] = info
	}
	return out
}
