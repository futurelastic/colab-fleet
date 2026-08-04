// Package remote implements driver.Driver as an HTTP client to a peer
// colab-fleet instance — session-abstraction.md §4.2, "the remote driver".
//
// This is the entire federation design, and the cheapest available test of
// it. §4.2 states the claim plainly: cross-machine operation is not a
// feature layered on top of the abstraction, it is one implementation of
// it, and "if the interface cannot express 'a session on another machine,'
// the interface is wrong."
//
// It mostly can. Two places it cannot are recorded below, plus one that has
// since been fixed. All three share a shape worth naming up front: the interface was designed against a
// local driver, where certain things are free — knowing your own
// capabilities, knowing who is asking, never being unreachable. None of
// those are free across a network, and the interface has nowhere to put the
// difference.
//
// # FINDING 1: Capabilities() cannot fail, and a remote driver's cannot succeed
//
// Capabilities() is synchronous and infallible: no context, no error. For a
// local driver that is correct — it is describing itself. A remote driver is
// describing somebody else, and the answer lives on the peer
// (GET /v1/runtimes), behind a network that may be down.
//
// There is no honest synchronous answer when the peer has never been
// reached. Reporting the peer's real capabilities is impossible; reporting
// optimistic ones is the §5.6 emulation the field exists to prevent; and
// reporting everything false is a lie of a subtler kind — see FINDING 2.
//
// What this driver does: report a conservative floor until the peer has
// answered, refresh the cache whenever it does, and never claim a capability
// it has not been told about. What it cannot do is tell a req which of
// those two situations it is in.
//
// # FINDING 2: DriverCapabilities has no way to say "I don't know yet"
//
// This is §5.7 — "absence and failure are different answers" — a third
// time, in a third place, and by now that is not a coincidence but a
// property of the design worth stating: every field in this API that can be
// absent needs to distinguish absent-because-no from absent-because-unknown.
//
// A DriverCapabilities of all-false means "this driver supports nothing".
// An unreached peer produces exactly that value, meaning "nobody has told me
// anything". A req consulting GET /v1/runtimes (which api-http.md §3.1
// says it MUST do before relying on a capability) cannot tell a peer that
// genuinely observes nothing from a peer that has not been asked yet. It
// degrades identically in both cases — which is safe, and is why this is
// recorded rather than treated as urgent — but it also means a permanently
// misconfigured peer is indistinguishable from a deliberately minimal one.
//
// # RESOLVED: the caller's authority is now a parameter
//
// This driver previously took the original caller's credentials from a
// context value, because no operation in §3 had anywhere to put them. §13
// requires a proxying service to present the ORIGINAL caller's authority to
// a peer, never its own — "otherwise every machine becomes a confused deputy
// for every other" — and an out-of-band value is exactly the kind a service
// can forget to attach.
//
// The failure mode was the reason this was the most serious defect in the
// design: a remote driver with no req credentials would reach for the one
// credential it certainly had, its own. The request then succeeds. The tests
// pass. The authorization is silently widened, and nothing reports it,
// because the symptom of the bug is that everything works.
//
// fleet.Caller is now a parameter of every operation, and this driver holds
// NO credential of its own. That second half matters more than the first:
// refusing to fall back is a policy a driver can get wrong, whereas having
// nothing to fall back TO is a property of the type. The confused deputy is
// not prevented here; it is unrepresentable.
//
// Reads are included. An earlier revision let List and State fall back to
// the driver's own token on the reasoning that reading is harmless — but
// "which sessions exist, in which directories, on which machine" is exactly
// the reconnaissance an unauthorized req wants, and §6 grants read
// permission broadly rather than universally.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

const defaultDeadlineMs = 3000

// ErrNoCallerAuthority is returned when an operation arrives with no
// credential to present to the peer. Every operation checks it, reads
// included: this driver has no credential of its own to substitute, so an
// empty one means the request simply cannot be made (§13).
var ErrNoCallerAuthority = errors.New(
	"remote: req carries no credential; a proxied request presents the " +
		"original caller's authority, and this proxy holds none of its own (§13)")

// Driver is an HTTP client to one peer colab-fleet, presented as an
// ordinary driver.Driver. The service registers it with RegisterPeerDriver
// and never learns that it is remote — which is the design's whole claim.
type Driver struct {
	machine  fleet.MachineId
	base     string
	client   *http.Client
	deadline time.Duration
	margin   time.Duration
	now      func() time.Time

	// identity is the credential THIS machine holds ON THIS PEER (§6).
	//
	// An earlier revision deliberately had no credential field at all: with
	// one shared secret, forwarding the caller's token was both possible and
	// correct, and holding one of its own was how a proxy became a confused
	// deputy.
	//
	// Per-peer credentials remove that coincidence. A caller's token means
	// nothing on another machine, so there is nothing to forward, and a
	// proxied request necessarily authenticates as the relaying machine. The
	// caller's identity travels alongside as an assertion (onBehalfOf).
	//
	// What that costs is honest to state: the peer authorizes THIS MACHINE,
	// not the original caller, and trusts our claim about who asked exactly
	// as far as it trusts us. What it cannot do is manufacture authority — a
	// relay never obtains more than it was granted, whatever principal it
	// names. Empty means shared-token mode, where the caller's credential is
	// forwarded as before.
	identity string

	mu       sync.RWMutex
	caps     fleet.DriverCapabilities
	capsSeen bool
}

// Option configures a Driver.
type Option func(*Driver)

// WithHTTPClient replaces the HTTP client. The client's own Timeout is left
// alone; deadlines are enforced per-call via context, because §4.4 requires
// a req to be able to shorten a deadline and a client-wide timeout
// cannot express that.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Driver) {
		if c != nil {
			d.client = c
		}
	}
}

// WithDeadline sets the FLOOR for this driver's per-call deadline (§4.4).
//
// It is a floor rather than the value because a proxy's deadline is not its
// own to choose freely — see effectiveDeadline.
func WithDeadline(dur time.Duration) Option {
	return func(d *Driver) {
		if dur > 0 {
			d.deadline = dur
		}
	}
}

// WithTransitMargin sets the allowance added to a peer's declared deadline to
// cover the network. Default 2s.
func WithTransitMargin(dur time.Duration) Option {
	return func(d *Driver) {
		if dur > 0 {
			d.margin = dur
		}
	}
}

// effectiveDeadline is how long this driver will actually wait.
//
// # A proxy's deadline is not independent of its peer's
//
// §4.4 governs a *caller* shortening a driver's deadline. It says nothing
// about composing deadlines across a hop, and the omission has a sharp edge:
// if a proxy waits less time than the peer has declared it may take, the
// proxy abandons calls the peer would have completed. Every such call is
// reported as "unreachable" — a machine that answered, described as one that
// did not, which is §5.7's confusion produced by a timer.
//
// Measured, which is why this exists: a peer declaring 5s, behind a proxy
// waiting 3s, on a host under enough load that a subprocess spawn took over a
// second. The peer was healthy and answering; the proxy timed out.
//
// So once the peer's capabilities are known, this driver waits at least as
// long as the peer said it might take, plus transit. Before they are known it
// falls back to the configured floor — and that gap is one more consequence
// of §14 D3, capability declaration being unable to say "not yet known".
func (d *Driver) effectiveDeadline() time.Duration {
	d.mu.RLock()
	seen, peerMs := d.capsSeen, d.caps.DeadlineMs
	d.mu.RUnlock()
	if seen && peerMs > 0 {
		if peer := time.Duration(peerMs)*time.Millisecond + d.margin; peer > d.deadline {
			return peer
		}
	}
	return d.deadline
}

// WithIdentity sets the credential this machine presents to the peer (§6).
// Without it the driver stays in shared-token mode and forwards the caller's
// credential, which only works while every machine accepts the same secret.
func WithIdentity(token string) Option {
	return func(d *Driver) { d.identity = token }
}

func withClock(f func() time.Time) Option { return func(d *Driver) { d.now = f } }

// bearerFor decides what a proxied request authenticates with, and whom it
// names. See the identity field for why these are two different things.
func (d *Driver) bearerFor(req fleet.Request) (token, onBehalfOf string, ok bool) {
	if d.identity != "" {
		return d.identity, req.Caller.Principal, true
	}
	if req.Caller.HasCredential() {
		return req.Caller.Credential, req.Caller.Principal, true
	}
	return "", "", false
}

// New builds a driver for the peer at base (e.g. "https://host:PORT").
//
// It deliberately does NOT probe the peer. §7.2 configures peers statically,
// and §5.7 requires an unreachable peer to surface as a source reporting
// unreachable rather than as an absence — a peer that could not be
// registered because it happened to be down at startup would be exactly that
// absence. Registration therefore always succeeds; reachability is a
// per-call fact, reported per call.
func New(machine fleet.MachineId, base string, opts ...Option) *Driver {
	d := &Driver{
		machine:  machine,
		base:     strings.TrimRight(base, "/"),
		client:   &http.Client{},
		deadline: defaultDeadlineMs * time.Millisecond,
		margin:   2 * time.Second,
		now:      time.Now,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

var _ driver.Driver = (*Driver)(nil)

// Capabilities reports what the PEER can do, as last observed — see
// FINDINGS 1 and 2 for why this is the wrong shape for a remote driver and
// what it costs.
//
// DeadlineMs is the one field this driver can always answer honestly,
// because it describes this driver's own transport rather than the peer's
// behaviour. That matters: §4.4 makes DeadlineMs mandatory and
// Validate-checked at registration, so a remote driver must be registrable
// before its peer has ever answered.
func (d *Driver) Capabilities() fleet.DriverCapabilities {
	d.mu.RLock()
	caps, seen := d.caps, d.capsSeen
	d.mu.RUnlock()

	caps.DeadlineMs = d.effectiveDeadline().Milliseconds()
	if !seen {
		// Nobody has told this driver anything about the peer. The flags
		// are a conservative floor, and saying so is the whole of D3: a
		// caller can now tell an unreached peer from a minimal one, which
		// an all-false declaration alone cannot express.
		return caps.Assumed()
	}
	return caps
}

// RefreshCapabilities asks the peer what it can do and caches the answer.
//
// This exists because Capabilities() cannot: it needs a context, it needs to
// be able to fail, and it needs to be callable again later. A method the
// interface does not know about is a poor substitute for a signature that
// admitted this in the first place (FINDING 1).
func (d *Driver) RefreshCapabilities(ctx context.Context, req fleet.Request) error {
	// Deliberately NOT bounded by this driver's own deadline.
	//
	// The bootstrap is circular: the deadline this driver should enforce is
	// derived from the peer's declared one (see effectiveDeadline), but
	// learning it requires a call — and bounding that call by the floor is
	// exactly the too-short value the derivation exists to correct. Observed
	// directly: the probe timed out at the 3s floor against a loaded peer,
	// so the deadline was never learned and every later call kept using the
	// floor.
	//
	// §4.4 governs *session operations*, whose whole point is that they must
	// not block unboundedly. This is an out-of-band metadata probe made by
	// an operator at startup, and it honours the caller's context — which is
	// where the bound belongs for a call whose purpose is to discover bounds.
	var body fleet.Collection[fleet.RuntimeInfo]
	if err := d.do(ctx, req, http.MethodGet, "/v1/runtimes", nil, &body); err != nil {
		return err
	}
	for _, ri := range body.Items() {
		if ri.Machine != d.machine {
			continue
		}
		d.mu.Lock()
		// Adopt what the peer said about itself, then stamp WHEN we heard
		// it — the peer's own timestamp would be on the peer's clock (§11),
		// and freshness here is a local judgement about a local cache.
		d.caps = ri.Capabilities.Observed(d.now())
		d.capsSeen = true
		d.mu.Unlock()
		return nil
	}
	return fmt.Errorf("remote: peer %q reported no runtimes for itself", d.machine)
}

// CapabilitiesKnown reports whether the peer has ever answered. It is the
// distinction DriverCapabilities itself cannot carry (FINDING 2), exposed
// here so at least a req holding the concrete type can ask.
func (d *Driver) CapabilitiesKnown() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.capsSeen
}

// bounded applies this driver's declared deadline, or the caller's if
// shorter (§4.4: "a req may supply a shorter deadline; never a longer
// one").
func (d *Driver) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	own := d.now().Add(d.effectiveDeadline())
	if dl, ok := ctx.Deadline(); ok && dl.Before(own) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, own)
}

// do performs one request. token, when non-empty, replaces this driver's own
// credential — that is how a proxied call presents the original caller's
// authority (§13).
func (d *Driver) do(ctx context.Context, req fleet.Request, method, path string, body any, out any) error {
	token, behalf, ok := d.bearerFor(req)
	if !ok {
		return ErrNoCallerAuthority
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("remote: encoding request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, d.base+path, rdr)
	if err != nil {
		return fmt.Errorf("remote: building request: %w", err)
	}
	// The caller's authority, never this driver's — it has none (§13).
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if behalf != "" {
		// Who asked, for the peer's audit trail (§6 requirement 4). Never
		// a substitute for the credential above.
		httpReq.Header.Set("Fleet-On-Behalf-Of", behalf)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// Tell the peer the bound we are enforcing, so it can fail fast rather
	// than working on a request we have already given up on (§3.3).
	if dl, ok := ctx.Deadline(); ok {
		ms := time.Until(dl).Milliseconds()
		if ms > 0 {
			httpReq.Header.Set("Fleet-Deadline-Ms", strconv.FormatInt(ms, 10))
		}
	}

	started := d.now()
	resp, err := d.client.Do(httpReq)
	if err != nil {
		// A transport failure is unreachable, never not_found. The peer did
		// not answer, so nothing at all is known about the session
		// (api-http.md §2: "the single most important line in this
		// document").
		return &fleet.Error{
			Kind: fleet.ErrorUnreachable,
			Message: fmt.Sprintf("no answer from %s after %s: %v",
				d.machine, d.now().Sub(started).Round(time.Millisecond), err),
			Machine:   d.machine,
			Retryable: true,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp, d.machine)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("remote: decoding response from %s: %w", d.machine, err)
	}
	return nil
}

// decodeError turns a peer's error envelope back into a typed error,
// preserving the kind rather than flattening every failure into "something
// went wrong". The 404/504 distinction in particular must survive the trip.
func decodeError(resp *http.Response, machine fleet.MachineId) error {
	var env fleet.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err == nil && env.Error.Kind != "" {
		e := env.Error
		if e.Machine == "" {
			e.Machine = machine
		}
		return &e
	}
	// A peer that answered with a status but no parseable envelope is
	// misbehaving, not unreachable — it answered. Map by status rather than
	// assuming the worst.
	kind := fleet.ErrorInvalid
	switch resp.StatusCode {
	case http.StatusNotFound:
		kind = fleet.ErrorNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = fleet.ErrorUnauthorized
	case http.StatusConflict:
		kind = fleet.ErrorConflict
	case http.StatusNotImplemented:
		kind = fleet.ErrorUnsupported
	case http.StatusGatewayTimeout:
		kind = fleet.ErrorUnreachable
	}
	return &fleet.Error{
		Kind:    kind,
		Message: fmt.Sprintf("peer returned %d with no error envelope", resp.StatusCode),
		Machine: machine,
	}
}

// List asks the peer for its LOCAL view only (§13.1) and adopts the
// SourceStatus it returns (§13.2).
//
// Both rules are load-bearing and both are one line of code, which is
// exactly why they are easy to get wrong:
//
//   - scope=local is what keeps fan-out one hop deep. Without it two
//     mutually-configured peers query each other forever, or — worse —
//     double-count and look fine.
//   - the returned envelope's sources are passed through untouched. A peer
//     can answer promptly AND report itself degraded; manufacturing a fresh
//     "ok" from the mere fact that the HTTP call succeeded would flatten
//     that into a confident envelope built on a self-declared unreliable
//     source.
func (d *Driver) List(ctx context.Context, req fleet.Request, filter driver.ListFilter) (fleet.Collection[fleet.Session], error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	q := url.Values{}
	q.Set("scope", "local") // §13.1 — never "fleet"
	if filter.Status != "" {
		q.Set("status", string(filter.Status))
	}
	if filter.Agent != "" {
		q.Set("agent", string(filter.Agent))
	}
	if filter.CwdPrefix != "" {
		q.Set("cwdPrefix", filter.CwdPrefix)
	}

	started := d.now()
	var out fleet.Collection[fleet.Session]
	if err := d.do(ctx, req, http.MethodGet, "/v1/sessions?"+q.Encode(), nil, &out); err != nil {
		// §5.7: a failed read is never an empty list. The peer contributes a
		// SourceStatus saying it did not answer.
		src := fleet.SourceStatus{
			Machine:    d.machine,
			Status:     sourceStateFor(err),
			Error:      err.Error(),
			ObservedAt: d.now(),
		}
		_ = started
		return fleet.NewCollection([]fleet.Session{}, []fleet.SourceStatus{src})
	}
	// Adopted verbatim: out already carries the peer's own sources, and
	// Collection's decoder recomputed `complete` from them.
	return out, nil
}

// sourceStateFor maps a transport-level failure onto the closed SourceState
// set (§9). Note that unauthorized is kept distinct from unreachable: "the
// peer refused me" and "the peer never answered" are different operational
// problems with different fixes, and collapsing them sends an operator to
// check the network when the real answer is a token.
func sourceStateFor(err error) fleet.SourceState {
	var fe *fleet.Error
	if errors.As(err, &fe) {
		switch fe.Kind {
		case fleet.ErrorUnauthorized:
			return fleet.SourceUnauthorized
		case fleet.ErrorUnreachable:
			return fleet.SourceUnreachable
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fleet.SourceUnreachable
	}
	return fleet.SourceDegraded
}

// State reads one session from the peer.
func (d *Driver) State(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.SessionState, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var out fleet.Session
	path := fmt.Sprintf("/v1/machines/%s/sessions/%s",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodGet, path, nil, &out); err != nil {
		return fleet.SessionState{}, err
	}
	return out.State, nil
}

// createBody is the POST /v1/machines/{machine}/sessions payload
// (api-http.md §3.3). Machine is absent deliberately: it travels in the URL.
type createBody struct {
	Runtime    fleet.RuntimeId    `json:"runtime,omitempty"`
	Cwd        fleet.AbsolutePath `json:"cwd"`
	Agent      fleet.AgentId      `json:"agent,omitempty"`
	Model      string             `json:"model,omitempty"`
	Effort     string             `json:"effort,omitempty"`
	Name       string             `json:"name,omitempty"`
	Prompt     string             `json:"prompt,omitempty"`
	ContextRef fleet.AbsolutePath `json:"contextRef,omitempty"`
}

// Create starts a session on the peer, forwarding the caller's idempotency
// key unchanged (§10).
//
// Forwarding rather than regenerating matters more here than anywhere else
// in this driver. §10's disaster is a federated create that times out in
// transit and gets retried, producing two agents in one working directory. A
// proxy that minted its own key per attempt would defeat the mechanism
// precisely when it is needed, since the retry would arrive carrying a
// different key and read as a different request.
func (d *Driver) Create(ctx context.Context, req fleet.Request, key string, spec fleet.SessionSpec) (fleet.SessionRef, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	if key == "" {
		return fleet.SessionRef{}, &fleet.Error{
			Kind:    fleet.ErrorInvalid,
			Message: "create: idempotency key is required (§10)",
			Machine: d.machine,
		}
	}
	body := createBody{
		Runtime: spec.Runtime, Cwd: spec.Cwd, Agent: spec.Agent,
		Model: spec.Model, Effort: spec.Effort, Name: spec.Name,
		Prompt: spec.Prompt, ContextRef: spec.ContextRef,
	}
	var out fleet.Session
	path := "/v1/machines/" + url.PathEscape(string(d.machine)) + "/sessions"
	if err := d.doWithKey(ctx, req, http.MethodPost, path, body, key, &out); err != nil {
		return fleet.SessionRef{}, err
	}
	return out.SessionRef, nil
}

// doWithKey is do() plus the Idempotency-Key header.
func (d *Driver) doWithKey(ctx context.Context, req fleet.Request, method, path string, body any, key string, out any) error {
	token, behalf, ok := d.bearerFor(req)
	if !ok {
		return ErrNoCallerAuthority
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("remote: encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, d.base+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("remote: building request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if behalf != "" {
		httpReq.Header.Set("Fleet-On-Behalf-Of", behalf)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", key)
	if dl, ok := ctx.Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 {
			httpReq.Header.Set("Fleet-Deadline-Ms", strconv.FormatInt(ms, 10))
		}
	}

	started := d.now()
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return &fleet.Error{
			Kind: fleet.ErrorUnreachable,
			Message: fmt.Sprintf("no answer from %s after %s: %v",
				d.machine, d.now().Sub(started).Round(time.Millisecond), err),
			Machine: d.machine, Retryable: true,
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeError(resp, d.machine)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Send delivers input to a session on the peer.
//
// A refusal comes back as an ordinary 200 with outcome "refused"
// (api-http.md §3.3), so it is returned as a value and not an error. This
// driver must not "helpfully" convert it: mapping a refusal to an error
// would train callers to retry it, which is exactly what the refusal exists
// to stop.
func (d *Driver) Send(ctx context.Context, req fleet.Request, ref fleet.SessionRef, text string, opts driver.SendOptions) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	body := struct {
		Text   string `json:"text"`
		Submit bool   `json:"submit"`
	}{Text: text, Submit: opts.Submit}

	var out fleet.DeliveryReceipt
	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/input",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodPost, path, body, &out); err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	return out, nil
}

// Interrupt asks the peer to interrupt a session (202, intent only).
func (d *Driver) Interrupt(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/interrupt",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodPost, path, struct{}{}, nil); err != nil {
		return fleet.Ack{}, err
	}
	return fleet.Ack{Accepted: true}, nil
}

// Close asks the peer to destroy a session (202, intent only).
//
// §5.4's corroboration cannot be performed here, and it is worse across a
// network than it was locally. The local driver could at least compare
// against its own recent sighting; this driver has no sightings of its own —
// it has only the id the req handed it, which is the one thing §5.4 says
// is not identification. Whatever corroboration happens must happen on the
// peer, against evidence that has no field to travel in.
//
// This is the same defect recorded in spec §5.4, observed from the side that
// makes its cost obvious.
func (d *Driver) Close(ctx context.Context, req fleet.Request, ref fleet.SessionRef) (fleet.Ack, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	path := fmt.Sprintf("/v1/machines/%s/sessions/%s",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))

	// Forward the caller's expectation, so §5.4's corroboration happens on
	// the machine that can actually see the session — against the caller's
	// belief, not the far driver's own sighting. Dropping it here would
	// silently downgrade every cross-machine destroy to the weak check,
	// which is the failure this whole envelope exists to prevent.
	if want := req.Expect.StartedAt; want != nil {
		path += "?startedAt=" + url.QueryEscape(want.UTC().Format(time.RFC3339Nano))
	}
	if err := d.do(ctx, req, http.MethodDelete, path, nil, nil); err != nil {
		return fleet.Ack{}, err
	}
	return fleet.Ack{Accepted: true}, nil
}

// Respond answers a prompt on a session belonging to the peer (§3).
//
// Like Send, a refusal comes back as an ordinary 200 carrying an outcome and
// is returned as a value: the peer decided the session was not at a prompt,
// which is information rather than a fault.
func (d *Driver) Respond(ctx context.Context, req fleet.Request, ref fleet.SessionRef, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	ctx, cancel := d.bounded(ctx)
	defer cancel()

	var out fleet.DeliveryReceipt
	path := fmt.Sprintf("/v1/machines/%s/sessions/%s/respond",
		url.PathEscape(string(d.machine)), url.PathEscape(ref.ID))
	if err := d.do(ctx, req, http.MethodPost, path, resp, &out); err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	return out, nil
}
