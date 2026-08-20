package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// Config wires the pieces an HTTP server needs beyond the Service itself.
type Config struct {
	// Token is the single bearer token this instance accepts. There is no
	// unauthenticated mode (api-http.md §5) — every request must present
	// Authorization: Bearer <Token>, loopback or not, dev or not.
	Token string

	// Principals is the per-identity authorization table (§6, auth.go).
	// When non-empty it is authoritative and Token/AllowLocalMutations/
	// AllowPeerRelay are ignored.
	//
	// The older fields remain for a single-machine or single-token
	// deployment, where a principal table is ceremony without benefit. They
	// are not a fallback that silently engages: if a table is configured,
	// it decides everything.
	Principals []Principal

	// AllowLocalMutations permits create/input/interrupt/close against
	// sessions ON THIS MACHINE. Defaults FALSE — §6 requirement 3 taken
	// literally: reads may be granted broadly, mutations are opt-in.
	//
	// The gate exists because authentication alone cannot express it. A
	// single shared token cannot distinguish a peer from a local
	// supervisor, so without a verb gate anything holding the token can
	// start processes here. §6: "a service that can start processes and
	// read files is a remote-execution surface regardless of intent."
	AllowLocalMutations bool

	// AllowPeerRelay permits forwarding a mutation to a PEER. Also
	// defaults FALSE, but it is a different question, and conflating the
	// two was defect D6.
	//
	// "May something mutate sessions on my machine" is about what this host
	// exposes. "May I relay a mutation to a peer" is about what this
	// instance may do as a client, and exposes nothing here — the peer is
	// the one taking the risk, and the peer already has its own gate.
	//
	// With one flag governing both, a hardened host could not act as a
	// full-featured client: relaying required opening this machine's own
	// sessions to mutation. That is backwards, and it was found by
	// deploying rather than by reasoning.
	AllowPeerRelay bool
}

// NewMux builds the routing skeleton of api-http.md §3 over svc, using
// Go's method+wildcard ServeMux patterns (no router dependency needed).
// Every handler does real request parsing, deadline/idempotency/auth
// enforcement, and error-envelope construction; none of them implement a
// working driver behind it — that boundary is the task's, and it is drawn
// here, not faked by half-real handlers.
func NewMux(svc *Service, cfg Config) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", withAuth(cfg, handleHealth(svc)))
	mux.HandleFunc("GET /v1/machines", withAuth(cfg, handleMachines(svc)))
	mux.HandleFunc("GET /v1/runtimes", withAuth(cfg, handleRuntimes(svc)))
	mux.HandleFunc("GET /v1/sessions", withAuth(cfg, handleListSessions(svc)))
	mux.HandleFunc("GET /v1/sessions/watch", withAuth(cfg, handleWatchSessions(svc)))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions", withAuth(cfg, mutating(svc, cfg, handleCreateSession(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}", withAuth(cfg, handleGetSession(svc)))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}/environment", withAuth(cfg, handleSessionEnvironment(svc)))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/input", withAuth(cfg, mutating(svc, cfg, handleSendInput(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/respond", withAuth(cfg, mutating(svc, cfg, handleRespond(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/interrupt", withAuth(cfg, mutating(svc, cfg, handleInterrupt(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/discard", withAuth(cfg, mutating(svc, cfg, handleDiscard(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/rename", withAuth(cfg, mutating(svc, cfg, handleRename(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/keys", withAuth(cfg, mutating(svc, cfg, handleKeys(svc))))
	mux.HandleFunc("DELETE /v1/machines/{machine}/sessions/{id}", withAuth(cfg, mutating(svc, cfg, handleClose(svc))))
	mux.HandleFunc("GET /v1/events", withAuth(cfg, handleEvents(svc)))

	return mux
}

// withAuth enforces api-http.md §5: no request is served without a bearer
// token, ever — not for loopback, not for development. cfg.Token is a
// single shared secret appropriate to a small, statically-configured fleet
// (§7.2); it is not a multi-tenant credential store, and this middleware
// does not pretend otherwise. Per-peer, per-verb authorization (§6.3) is
// future work once more than one peer identity exists to distinguish.
func withAuth(cfg Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.Principals) > 0 {
			p, ok := cfg.principalFor(r)
			if !ok {
				writeError(w, &fleet.Error{
					Kind:    fleet.ErrorUnauthorized,
					Message: "unrecognised credential",
				})
				return
			}
			next(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		want := "Bearer " + cfg.Token
		if cfg.Token == "" || r.Header.Get("Authorization") != want {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnauthorized,
				Message: "missing or invalid Authorization bearer token",
			})
			return
		}
		next(w, r)
	}
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalOf returns the resolved principal, and whether a table was in use.
func principalOf(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(Principal)
	return p, ok
}

// mutating gates the verbs that change something behind Config.AllowMutations
// (§6 requirement 3). A refusal here is unauthorized, not unsupported: the
// driver is perfectly capable, and this instance is configured not to permit
// it. Reporting "unsupported" would tell a req something false about the
// runtime and invite it to give up permanently on a capability that exists.
func mutating(svc *Service, cfg Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := fleet.MachineId(r.PathValue("machine"))

		// With a principal table, §6 requirement 3 is expressible as
		// written: per verb, per identity. D6's host/client split survives
		// as the difference between a verb grant and GrantRelay.
		if p, ok := principalOf(r); ok {
			need := grantForVerb(r)
			if target != "" && target != svc.Self() {
				need = GrantRelay
			}
			if !p.Allows(need) {
				logDenied(p, need, target, r)
				writeError(w, &fleet.Error{
					Kind: fleet.ErrorUnauthorized,
					Message: "principal " + p.Name + " does not hold the " +
						string(need) + " grant (§6)",
					Machine: target,
				})
				return
			}
			// Run first, then log what actually happened. Logging the
			// authorization decision as though it were the outcome made a
			// destroy and a refusal indistinguishable in the trail.
			aw := &auditWriter{ResponseWriter: w}
			next(aw, r)
			logMutation(p, need, target, r, aw.status)
			return
		}

		allowed, why := cfg.AllowLocalMutations,
			"this host does not permit mutation of its own sessions (§6; FLEET_ALLOW_MUTATIONS)"
		if target != "" && target != svc.Self() {
			allowed, why = cfg.AllowPeerRelay,
				"this instance does not relay mutations to peers (§6; FLEET_ALLOW_RELAY). "+
					"Note this is a separate grant from mutating local sessions: a hardened "+
					"host may still be a full-featured client"
		}
		if !allowed {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnauthorized, Message: why, Machine: target})
			return
		}
		next(w, r)
	}
}

// callerFrom derives the authority a request is made on behalf of (§6, §13).
//
// Credential is the bearer token the req actually presented — not this
// service's own. That is the whole of §13's "proxying does not launder
// authorization": when this service forwards to a peer, the peer sees the
// token of whoever started the request, and authorizes them rather than the
// machine that relayed it.
//
// Principal is provenance, not identity, and the difference is worth being
// honest about. While the fleet authenticates with one shared token (§7.2),
// nothing distinguishes one bearer from another, so the most specific true
// statement available is where the request came from. It is recorded for the
// audit trail §6 requirement 4 asks for — "actor, verb, target, outcome" —
// and it should be read as an address, because that is all it is. Real
// per-peer identity is §6's outstanding work; when it arrives, it lands
// here and nothing above this function changes.
func callerFrom(r *http.Request) fleet.Caller {
	// A resolved principal is an identity; the address below is only what
	// is available when no table is configured.
	if p, ok := principalOf(r); ok {
		return callerFor(p, r)
	}
	origin := r.RemoteAddr
	if host, _, err := net.SplitHostPort(origin); err == nil {
		origin = host
	}
	return fleet.Caller{
		Principal:  "addr:" + origin,
		Credential: bearerOf(r),
	}
}

// requestFrom builds the caller-side context of an operation (§2.6): who is
// asking, and what they believe about the target.
//
// Expect.StartedAt is read from ?startedAt=, and its absence is meaningful
// rather than incidental: a caller that supplies it gets §5.4's real
// guarantee — "destroy the session I looked at" — while a caller that omits it
// gets the weaker one a driver can offer from its own sightings, and is told
// which it got when the operation refuses.
func requestFrom(r *http.Request) fleet.Request {
	req := fleet.Request{Caller: callerFrom(r)}
	if raw := r.URL.Query().Get("startedAt"); raw != "" {
		if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			req.Expect.StartedAt = &ts
		} else if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			req.Expect.StartedAt = &ts
		}
	}
	return req
}

// bearerOf extracts the presented token. It deliberately does not validate —
// withAuth has already done that, and a second opinion here could only
// disagree.
func bearerOf(r *http.Request) string {
	const p = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return v[len(p):]
	}
	return ""
}

func writeError(w http.ResponseWriter, e *fleet.Error) {
	w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Kind.DefaultHTTPStatus())
	_ = json.NewEncoder(w).Encode(fleet.ErrorEnvelope{Error: *e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDriverError maps a Go-level driver error to the wire error kind
// api-http.md §2 defines. Only reached for a driver-level failure — a
// refusal (DeliveryReceipt.Outcome == "refused") is not an error at all
// and is written as an ordinary 200 by its own handler.
func writeDriverError(w http.ResponseWriter, machine fleet.MachineId, deadline time.Duration, err error) {
	// A peer that already classified this failure has said something this
	// service cannot improve on: adopt its kind rather than re-deriving one.
	//
	// This is §13.2's rule — "adopt a peer's SourceStatus; never
	// re-synthesize it" — applied to errors, and it was found the same way,
	// by watching a correct answer get worse in transit. The peer returned
	// conflict (§5.4: the caller's belief is stale); re-classification here
	// turned it into invalid, telling the caller to fix its syntax when what
	// it should do is re-read and decide.
	var relayed *fleet.Error
	if errors.As(err, &relayed) && relayed.Kind != "" {
		writeError(w, relayed)
		return
	}

	switch {
	case errors.Is(err, fleet.ErrNoSuchSession):
		// The machine answered, and there is no such session. Distinct from
		// unreachable, which is the machine not answering at all — see
		// fleet.ErrorUnreachable's comment for why conflating them is the
		// worst mistake a client of this API can make.
		writeError(w, &fleet.Error{Kind: fleet.ErrorNotFound, Message: err.Error(), Machine: machine})
	case errors.Is(err, fleet.ErrAmbiguousTarget):
		// §5.4: the caller's belief and the world disagree. Well-formed
		// request, conflicting state — 409, not 400.
		writeError(w, &fleet.Error{Kind: fleet.ErrorConflict, Message: err.Error(), Machine: machine})
	case isUnsupported(err):
		writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported, Message: err.Error(), Machine: machine})
	case isDeadlineExceeded(err):
		writeError(w, &fleet.Error{
			Kind:      fleet.ErrorUnreachable,
			Message:   "no answer within " + deadline.String(),
			Machine:   machine,
			Retryable: true,
		})
	default:
		writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error(), Machine: machine})
	}
}

// parseDeadline reads Fleet-Deadline-Ms (api-http.md §3.3). Absent or
// malformed, it returns 0, meaning "no caller-supplied bound" — the
// driver's own declared deadline applies unmodified (effectiveDeadline).
func parseDeadline(r *http.Request) time.Duration {
	raw := r.Header.Get("Fleet-Deadline-Ms")
	if raw == "" {
		return 0
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func handleHealth(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"epoch": svc.epoch,
			// The event plane is real now, so this reports the actual
			// high-water mark. It used to be a hardcoded 0 with a comment
			// explaining that no event bus existed — true when written, and
			// left behind when one did. A health endpoint that reports a
			// constant is worse than one that omits the field: a subscriber
			// comparing its cursor against this would conclude it had run
			// ahead of the service.
			"cursor":    svc.events.currentCursor(),
			"startedAt": svc.startedAt.Format(time.RFC3339Nano),
			// Which code this is. See fleet.Build for the incident that
			// added it; the short version is that two services disagreeing
			// is a different problem from two services being different
			// vintages, and nothing could previously tell them apart.
			"build":   svc.build,
			"drivers": svc.driverSummaries(),
			// counters is the read path #9 asked for onto the registry #44
			// built (internal/drivers/tmux/counters.go): an integer per
			// named fact, keyed by runtime. It is deliberately read here
			// rather than logged periodically — the F57 flap this issue
			// opens with was found by curling a read surface five times and
			// comparing counts, and a pull endpoint composes with the
			// poller that already exists on this response (the dashboard
			// already hits /v1/health); a log line only pays off with
			// someone tailing it at the moment it happens.
			//
			// startedAt above is the divisor this needs and already had:
			// every count here resets to zero on every restart, so read
			// alone it cannot distinguish a quiet machine from a young one.
			// A reader who divides by time-since-startedAt gets a rate;
			// one who does not is the reader #9's regression scenario
			// describes — looking at a healthy-looking number that is
			// actually just recent.
			"counters": svc.counterSnapshot(),
		})
	}
}

func handleMachines(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		col, err := svc.ListMachines(r.Context(), requestFrom(r), parseDeadline(r))
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, col)
	}
}

func handleRuntimes(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		col, err := svc.ListRuntimes(r.Context())
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, col)
	}
}

func handleListSessions(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		scope := Scope(q.Get("scope"))
		if scope == "" {
			scope = ScopeFleet // client default (api-http.md §3.2)
		}
		if scope != ScopeLocal && scope != ScopeFleet {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "scope must be 'local' or 'fleet'"})
			return
		}

		filter := driver.ListFilter{
			Status:    fleet.Status(q.Get("status")),
			Agent:     fleet.AgentId(q.Get("agent")),
			CwdPrefix: q.Get("cwdPrefix"),
		}

		// Read the sequence position BEFORE enumerating, so a client that
		// watches from it re-applies a few changes rather than missing any.
		// See fleet.FeedPosition for why the overlap is the safe direction and
		// why an unwatched service withholds the number entirely.
		cursor, epoch, resumable := svc.FeedPosition()

		col, err := svc.ListSessions(r.Context(), requestFrom(r), scope, filter, parseDeadline(r))
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		if resumable {
			col = col.WithFeed(cursor, epoch)
		}
		writeJSON(w, http.StatusOK, col)
	}
}

// Long-poll bounds. The default is long enough that an idle fleet costs one
// request every half minute, short enough to sit inside the timeouts every
// proxy and HTTP client has whether or not their operator remembers them.
const (
	watchDefaultWait = 25 * time.Second
	watchMaxWait     = 60 * time.Second
)

// watchResponse is the long poll's body.
//
// Cursor is what to send as the next `since` — not "the service's current
// cursor", which would silently skip anything stamped between the last event
// in this batch and the read of that field.
type watchResponse struct {
	Cursor int64           `json:"cursor"`
	Epoch  string          `json:"epoch"`
	Events []eventEnvelope `json:"events"`
}

// handleWatchSessions is the change-feed as ordinary request/response
// (api-http.md §4.1).
//
// # Why a second transport rather than a second feed
//
// Everything here is the same hub, the same cursor sequence, the same event
// vocabulary and the same envelope as GET /v1/events. What differs is only how
// the bytes reach the caller: a consumer maintaining a materialized mirror
// wants a request it can retry, log and reason about, and one that survives
// its own restart without a stream-reconnect state machine. Nothing is
// expressible in one transport and not the other, deliberately — the moment
// they diverge, two answers to the same question exist.
//
// # A stale cursor is an answer, not a fault
//
// control.resync arrives IN BAND, as the first (and only) event of the batch,
// with an ordinary 200. Same rule as a refused input: the request was well
// formed, and what the service has to say about it is domain information a
// caller must act on, not an exception to retry. A 4xx here would train a
// client to retry the one thing it must instead re-list after.
func handleWatchSessions(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		scope := ScopeFleet
		switch q.Get("scope") {
		case "", string(ScopeFleet):
		case string(ScopeLocal):
			// §13.1 in the event plane: a peer asking us for scope=local must
			// not make us open streams back to our own peers.
			scope = ScopeLocal
		default:
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "scope must be 'local' or 'fleet'"})
			return
		}

		filter := driver.SubscribeFilter{Sessions: q["session"], CwdPrefix: q.Get("cwdPrefix")}

		var since int64
		if raw := q.Get("since"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid,
					Message: "since must be a non-negative cursor from a previous response"})
				return
			}
			since = n
		}
		// An omitted epoch means "the instance I was already talking to",
		// matching how the SSE handler reads a browser's Last-Event-ID. A
		// caller that has one should send it; one that has a cursor and no
		// epoch is asserting continuity, and if that assertion is wrong the
		// epoch check below turns it into a resync rather than a bad resume.
		fromEpoch := q.Get("epoch")
		if since > 0 && fromEpoch == "" {
			fromEpoch = svc.Epoch()
		}

		wait := watchWait(r)

		ch, backlog, needResync, cancel := svc.Events(r.Context(), scope, filter, since, fromEpoch)
		defer cancel()

		epoch := svc.Epoch()

		if needResync {
			reason := fleet.ResyncCursorExpired
			if fromEpoch != epoch {
				reason = fleet.ResyncEpochChanged
			}
			// Cursor stays at what the caller sent. The resync is not a
			// position to resume from — the caller re-lists, and the listing
			// carries the position it should watch from next.
			writeJSON(w, http.StatusOK, watchResponse{
				Cursor: since, Epoch: epoch,
				Events: []eventEnvelope{{
					Epoch: epoch, Machine: svc.Self(), Kind: fleet.EventControlResync,
					Payload: fleet.ControlResyncPayload{Reason: reason},
				}},
			})
			return
		}

		events := make([]eventEnvelope, 0, len(backlog))
		for _, ev := range backlog {
			events = append(events, envelopeOf(ev))
		}

		if len(events) == 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			case ev, open := <-ch:
				if open {
					events = append(events, envelopeOf(ev))
					// Take whatever else is already waiting, so a busy fleet
					// answers one poll with a batch instead of one poll per
					// event. Nothing BLOCKS here: this drains what has already
					// arrived and stops.
					events = drainReady(ch, events)
				}
			}
		}

		writeJSON(w, http.StatusOK, watchResponse{
			Cursor: resumeCursor(since, events),
			Epoch:  epoch,
			Events: events,
		})
	}
}

// watchWait resolves how long this poll may block: the caller's `wait`, capped,
// and shortened further by Fleet-Deadline-Ms if that is smaller — §3.3's rule
// that a caller may always shorten and never extend.
func watchWait(r *http.Request) time.Duration {
	wait := watchDefaultWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms >= 0 {
			wait = time.Duration(ms) * time.Millisecond
		}
	}
	if wait > watchMaxWait {
		wait = watchMaxWait
	}
	if d := parseDeadline(r); d > 0 && d < wait {
		wait = d
	}
	return wait
}

// drainReady appends everything already queued for this subscriber without
// waiting for more.
func drainReady(ch <-chan fleet.Event, into []eventEnvelope) []eventEnvelope {
	for {
		select {
		case ev, open := <-ch:
			if !open {
				return into
			}
			into = append(into, envelopeOf(ev))
		default:
			return into
		}
	}
}

// resumeCursor is what the caller sends as its next `since`.
//
// The last event in the batch, or — for an empty batch — exactly what the
// caller already had. Not the service's current cursor: an empty batch means
// nothing SELECTED by this filter arrived, while the sequence may well have
// advanced for somebody else, and advancing this caller's cursor past events
// it never saw is the silent gap the whole design refuses.
func resumeCursor(since int64, events []eventEnvelope) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Cursor > 0 {
			return events[i].Cursor
		}
	}
	return since
}

func envelopeOf(ev fleet.Event) eventEnvelope {
	return eventEnvelope{
		Cursor: ev.Cursor, Epoch: ev.Epoch, Machine: ev.Machine,
		Kind: ev.Kind, Payload: ev.Payload, Origin: ev.Origin,
	}
}

type createSessionBody struct {
	Runtime    fleet.RuntimeId    `json:"runtime"`
	Cwd        fleet.AbsolutePath `json:"cwd"`
	Agent      fleet.AgentId      `json:"agent"`
	Model      string             `json:"model"`
	Effort     string             `json:"effort"`
	Name       string             `json:"name"`
	Prompt     string             `json:"prompt"`
	ContextRef fleet.AbsolutePath `json:"contextRef"`

	// Marker and RemoteControl close the gap that made an API-created
	// session a different KIND of session from a launcher-created one. See
	// fleet.SessionSpec for both. RemoteControl is a pointer because
	// "absent" and "false" must not be the same request.
	Marker        string `json:"marker"`
	RemoteControl *bool  `json:"remoteControl"`

	// TrustCwd is the caller's consent to the runtime's folder-trust question
	// about Cwd. See fleet.SessionSpec.TrustCwd for what it means, and
	// handleCreateSession for why it needs a second grant.
	TrustCwd bool `json:"trustCwd"`

	// Env, Resume, PermissionMode and Consents close the gap that kept a
	// supervisor driving the substrate directly instead of this API: a session
	// created here could not carry its identity, continue a conversation, or be
	// started in a non-default permission posture. See fleet.SessionSpec for
	// each, and handleCreateSession for which of them need the send grant.
	Env            map[string]string  `json:"env"`
	Resume         string             `json:"resume"`
	PermissionMode string             `json:"permissionMode"`
	Consents       []fleet.PromptKind `json:"consents"`
}

// createNeedsSend names the part of a create body that requires the send grant,
// or "" when the body asks for nothing beyond starting a session.
//
// It returns the NAME rather than a boolean so the refusal can say which field
// caused it. A caller told only "you need send" re-reads the whole request
// guessing; one told "consents requires it" fixes it in a line.
func createNeedsSend(body createSessionBody) string {
	switch {
	case body.TrustCwd:
		return "trustCwd"
	case len(body.Consents) > 0:
		return "consents"
	case body.PermissionMode != "":
		return "permissionMode"
	}
	return ""
}

func handleCreateSession(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))

		// Idempotency-Key is required, not optional (§10, api-http.md
		// §3.3) — a create without one is rejected before any driver is
		// even consulted.
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "Idempotency-Key header is required (§10)", Machine: machine})
			return
		}

		var body createSessionBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}

		// Two things in this body ask for more than a create.
		//
		// A CONSENT (`trustCwd`, `consents`) asks the driver to produce a
		// KEYPRESS on the caller's behalf, which is what `respond` does and what
		// `send` grants (see grantForVerb: answering a prompt shares that grant
		// because it has the same blast radius).
		//
		// A non-default `permissionMode` asks for a session that ACTS WITHOUT
		// ASKING — and raises an acceptance screen that then has to be answered.
		// A principal permitted only to start sessions must not be able to start
		// that one; between "may start a session" and "may start a session that
		// needs no permission for anything", the second is plainly the larger
		// authority.
		//
		// Folding either into `create` would be the sort of quiet privilege
		// widening that is invisible until it is someone's incident: nobody
		// reviewing a grants table would see it.
		//
		// Only checked when this machine is the one serving the create. A
		// relayed create is authorized by the PEER, against the same
		// credential, under its own table — §13's "proxying does not launder
		// authorization" — and a second opinion here could only disagree with
		// the host that actually holds the session.
		if elevated := createNeedsSend(body); elevated != "" && (machine == "" || machine == svc.Self()) {
			if p, ok := principalOf(r); ok && !p.Allows(GrantSend) {
				writeError(w, &fleet.Error{
					Kind: fleet.ErrorUnauthorized,
					Message: "principal " + p.Name + " does not hold the " +
						string(GrantSend) + " grant, which " + elevated + " requires (§6)",
					Machine: machine,
				})
				return
			}
		}

		d, resErr := svc.resolveSessionDriver(machine, body.Runtime)
		if resErr != nil {
			writeError(w, resErr)
			return
		}

		spec := fleet.SessionSpec{
			// Machine is filled from the URL path, not the request body
			// (session.go's SessionSpec doc comment) — the wire body
			// deliberately does not repeat a value the path already
			// carries.
			Machine: machine, Runtime: body.Runtime, Cwd: body.Cwd,
			Agent: body.Agent, Model: body.Model, Effort: body.Effort,
			Name: body.Name, Prompt: body.Prompt, ContextRef: body.ContextRef,
			Marker: body.Marker, RemoteControl: body.RemoteControl,
			TrustCwd: body.TrustCwd, Env: body.Env, Resume: body.Resume,
			PermissionMode: body.PermissionMode, Consents: body.Consents,
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ref, err := d.Create(ctx, requestFrom(r), key, spec)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}

		state, _ := d.State(ctx, requestFrom(r), ref)
		writeJSON(w, http.StatusCreated, fleet.Session{
			SessionRef: ref, Runtime: spec.Runtime, Cwd: spec.Cwd,
			Agent: spec.Agent, Model: spec.Model, State: state,
		})
	}
}

// handleSessionEnvironment reports what a session's process actually received
// (fleet.SessionEnvironment).
//
// It answers "did this session get what a launcher-created one gets" from a
// read rather than from an ssh session and two process-manager unit files, and
// it is the reason the login-shell wrapping is an accepted dependency rather
// than an invisible one: the service still does not own the shell startup file,
// but it can now say what came out of it.
//
// A driver that cannot answer is reported as such, not as an empty
// environment — §5.7, and doubly so here, where "no variables" and "we did not
// look" would otherwise be the same response.
func handleSessionEnvironment(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")

		d, resErr := svc.resolveSessionDriver(machine, fleet.RuntimeId(r.URL.Query().Get("runtime")))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		reporter, ok := d.(driver.EnvironmentReporter)
		if !ok {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot report a session's environment",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		env, err := reporter.Environment(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, env)
	}
}

func handleGetSession(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		d, resErr := svc.resolveSessionDriver(machine, runtimeHint)
		if resErr != nil {
			writeError(w, resErr)
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ref := fleet.SessionRef{Machine: machine, ID: id}

		// Answered from a listing rather than from State(), because State()
		// returns only a SessionState and this endpoint must return a
		// Session (api-http.md §3.3: cwd, agent, model, startedAt).
		//
		// The omission was not cosmetic. `startedAt` is what a caller quotes
		// back to make a destroy corroborable (§5.4), so a response without
		// it left the strong guarantee unreachable through the very endpoint
		// a caller would read before destroying something — a guarantee is
		// only as reachable as the data needed to invoke it.
		//
		// The cost is one enumeration, which is a constant number of
		// subprocess spawns on the driver that motivated that design; a
		// per-session query would not be cheaper.
		req := requestFrom(r)
		if col, err := d.List(ctx, req, driver.ListFilter{}); err == nil {
			for _, s := range col.Items() {
				if s.ID == id {
					writeJSON(w, http.StatusOK, s)
					return
				}
			}
		}

		// Not in the listing: fall through to the driver's own answer, which
		// distinguishes "looked and it is gone" from "could not look" (§5.7).
		state, err := d.State(ctx, req, ref)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, fleet.Session{SessionRef: ref, State: state})
	}
}

func handleSendInput(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		var body struct {
			Text   string `json:"text"`
			Submit bool   `json:"submit"`
			// ResumeIfStranded completes a delivery this service already made
			// and could not confirm. It never submits text the service did not
			// place there — see driver.SendOptions.
			ResumeIfStranded bool `json:"resumeIfStranded,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}

		d, resErr := svc.resolveSessionDriver(machine, runtimeHint)
		if resErr != nil {
			writeError(w, resErr)
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		receipt, err := d.Send(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id}, body.Text, driver.SendOptions{Submit: body.Submit, ResumeIfStranded: body.ResumeIfStranded})
		if err != nil {
			// A refusal from the driver is not this branch — Send returns
			// it as a DeliveryReceipt value, not an error. Only a
			// driver-level failure (unsupported, deadline exceeded, ...)
			// reaches here.
			writeDriverError(w, machine, deadline, err)
			return
		}
		// api-http.md §3.3: a refusal is 200, not an HTTP error — this is
		// the same 200 whether Outcome is submitted, queued, refused, or
		// unknown.
		writeJSON(w, http.StatusOK, receipt)
	}
}

// handleRespond answers a prompt a session is blocked on (§3).
//
// A refusal is a 200 carrying an outcome, exactly as for input (api-http.md
// §3.3): "this session is not at a prompt" is a fact about the session, not a
// fault in the request, and mapping it to 4xx would train callers to retry it.
func handleRespond(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		d, ferr := svc.resolveSessionDriver(machine, fleet.RuntimeId(r.URL.Query().Get("runtime")))
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		var body fleet.Response
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed body: " + err.Error()})
			return
		}
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		receipt, err := d.Respond(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id}, body)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

func handleInterrupt(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		d, resErr := svc.resolveSessionDriver(machine, runtimeHint)
		if resErr != nil {
			writeError(w, resErr)
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := d.Interrupt(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleClose(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		d, resErr := svc.resolveSessionDriver(machine, runtimeHint)
		if resErr != nil {
			writeError(w, resErr)
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := d.Close(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleDiscard(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		d, resErr := svc.resolveSessionDriver(machine, fleet.RuntimeId(r.URL.Query().Get("runtime")))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		ack, err := d.Discard(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id},
			r.URL.Query().Get("expect"))
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleRename(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")
		runtimeHint := fleet.RuntimeId(r.URL.Query().Get("runtime"))

		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "malformed JSON body", Machine: machine})
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: "rename needs a non-empty name", Machine: machine})
			return
		}

		d, resErr := svc.resolveSessionDriver(machine, runtimeHint)
		if resErr != nil {
			writeError(w, resErr)
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		req := requestFrom(r)
		ack, err := d.Rename(ctx, req, fleet.SessionRef{Machine: machine, ID: id}, body.Name)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}

		// Announce it. A subscriber filtering by id would otherwise see the old
		// id go quiet and a stranger appear — indistinguishable from a session
		// dying and another being created, which is exactly the wrong reading.
		svc.events.publish(fleet.Event{
			Kind:    fleet.EventSessionRenamed,
			Machine: machine,
			Payload: fleet.SessionRenamed{
				Machine: machine, From: id, To: body.Name, StartedAt: req.Expect.StartedAt,
			},
		})
		writeJSON(w, http.StatusAccepted, ack)
	}
}

// handleKeys delivers one raw key event to a session's screen (api-http.md
// §3.3).
//
// # Why this is not a flag on respond
//
// `respond` refuses whenever the driver sees no prompt, and that refusal is the
// whole of its safety: a keypress delivered to a session that is not asking
// anything is consumed invisibly by whatever it is doing. The screens this
// endpoint exists for are exactly the ones the classifier does not recognise,
// so folding it into `respond` would mean relaxing that check for precisely the
// case it was written to exclude. A separate route pays for its own safety —
// `expect`, and the driver's refusals — instead of spending `respond`'s.
//
// A refusal is a 200 carrying an outcome, as for input and respond. A stale or
// missing `expect` is a 409: the request is well formed and the caller's belief
// is out of date (§5.4), which is the same answer `discard` gives for the same
// reason.
func handleKeys(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		machine := fleet.MachineId(r.PathValue("machine"))
		id := r.PathValue("id")

		var body struct {
			Key fleet.KeyName `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			// The decoder rejects an unknown key name itself (fleet.KeyName's
			// UnmarshalJSON), which is why this message names the vocabulary:
			// a caller told only "malformed body" would go looking at its JSON.
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorInvalid,
				Message: "malformed body: " + err.Error() + " (keys this API delivers: " +
					keyVocabulary() + ")",
				Machine: machine,
			})
			return
		}
		if !body.Key.Valid() {
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorInvalid,
				Message: "key is required; one of " + keyVocabulary(),
				Machine: machine,
			})
			return
		}

		d, resErr := svc.resolveSessionDriver(machine, fleet.RuntimeId(r.URL.Query().Get("runtime")))
		if resErr != nil {
			writeError(w, resErr)
			return
		}
		sender, ok := d.(driver.KeySender)
		if !ok {
			// §5.6: a driver that cannot press a key says so, and nothing here
			// approximates one out of `input` — which would break input's own
			// guarantee that a message never becomes a keystroke.
			writeError(w, &fleet.Error{
				Kind:    fleet.ErrorUnsupported,
				Message: "this runtime cannot deliver a raw key event",
				Machine: machine,
			})
			return
		}

		deadline := effectiveDeadline(d.Capabilities().DeadlineMs, parseDeadline(r))
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()

		receipt, err := sender.Keys(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id},
			body.Key, r.URL.Query().Get("expect"))
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

func keyVocabulary() string {
	names := fleet.KeyNames()
	out := make([]string, 0, len(names))
	for _, k := range names {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// handleEvents streams events as Server-Sent Events (api-http.md §4).
//
// # The framing question, resolved
//
// fleet.Event's doc comment recorded an open question: does Kind travel as the
// SSE "event:" line, as a JSON property inside "data:", or both? Both.
//
//   - "event:" so a browser EventSource can addEventListener by kind, which
//     is the entire reason SSE has the field;
//   - "kind" inside the payload so a client reading the stream as newline
//     framed JSON — every non-browser consumer — does not have to parse SSE
//     framing to learn what it received;
//   - "id:" carries the cursor, so a reconnecting EventSource sends
//     Last-Event-ID automatically and resumption works without the client
//     writing any resumption code.
//
// Duplicating kind is redundancy chosen deliberately: the alternative is a
// stream that is only usable from one kind of client, and the cost is a short
// string per event.
func handleEvents(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported,
				Message: "this server cannot stream"})
			return
		}

		q := r.URL.Query()
		filter := driver.SubscribeFilter{
			Sessions:  q["session"],
			CwdPrefix: q.Get("cwdPrefix"),
		}
		fromCursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
		fromEpoch := q.Get("epoch")
		// A browser reconnecting supplies Last-Event-ID rather than a query
		// parameter; honour it so resumption needs no client code.
		if lastID := r.Header.Get("Last-Event-ID"); lastID != "" && fromCursor == 0 {
			fromCursor, _ = strconv.ParseInt(lastID, 10, 64)
			if fromEpoch == "" {
				fromEpoch = svc.Epoch()
			}
		}

		// §13.1: a proxied subscription asks for the peer's LOCAL view, and
		// a peer receiving it answers for itself without forwarding. The
		// default is fleet, matching plural reads (api-http.md §3.2).
		scope := ScopeFleet
		if q.Get("scope") == string(ScopeLocal) {
			scope = ScopeLocal
		}
		ch, backlog, needResync, cancel := svc.Events(r.Context(), scope, filter, fromCursor, fromEpoch)
		defer cancel()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Fleet-Clock", time.Now().Format(time.RFC3339Nano))
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		if needResync {
			// §7.3: announce the gap. A subscriber told this refetches
			// state; one silently resumed from an arbitrary point believes
			// it has a complete history and does not.
			reason := fleet.ResyncCursorExpired
			if fromEpoch != svc.Epoch() {
				reason = fleet.ResyncEpochChanged
			}
			writeSSE(w, flusher, fleet.Event{
				Epoch: svc.Epoch(), Machine: svc.Self(), Kind: fleet.EventControlResync,
				Payload: fleet.ControlResyncPayload{Reason: reason},
			})
		}
		for _, ev := range backlog {
			writeSSE(w, flusher, ev)
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, open := <-ch:
				if !open {
					return
				}
				writeSSE(w, flusher, ev)
			}
		}
	}
}

// writeSSE encodes one event. Encoding failure ends the stream rather than
// emitting a partial frame: a truncated event is worse than a closed
// connection, because the client cannot tell it happened.
func writeSSE(w http.ResponseWriter, f http.Flusher, ev fleet.Event) {
	body, err := json.Marshal(envelopeOf(ev))
	if err != nil {
		return
	}
	if ev.Cursor > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.Cursor)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, body)
	f.Flush()
}

// eventEnvelope is fleet.Event's wire form, shared by BOTH transports: it is
// what an SSE frame's "data:" line carries and what an entry of the long
// poll's "events" array is.
//
// One shape, deliberately. The long poll is a transport for the event
// vocabulary, not a second event model, and the moment it grew an envelope of
// its own the two would begin answering the same question differently. It
// exists rather than marshalling fleet.Event directly so the JSON shape is
// stated in one place and cannot drift with the in-memory type.
type eventEnvelope struct {
	Cursor  int64           `json:"cursor"`
	Epoch   string          `json:"epoch"`
	Machine fleet.MachineId `json:"machine"`
	Kind    fleet.EventKind `json:"kind"`
	Payload any             `json:"payload,omitempty"`
	// Origin is the relayed event's coordinates in its originating
	// service's sequence. Omitting it here once cost a live test its
	// provenance while every unit test still passed — a separate wire type
	// keeps the stream's shape in one place, but only if it is kept in step
	// with what it encodes.
	Origin *fleet.EventOrigin `json:"origin,omitempty"`
}
