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
	mux.HandleFunc("POST /v1/machines/{machine}/sessions", withAuth(cfg, mutating(svc, cfg, handleCreateSession(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}", withAuth(cfg, handleGetSession(svc)))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/input", withAuth(cfg, mutating(svc, cfg, handleSendInput(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/respond", withAuth(cfg, mutating(svc, cfg, handleRespond(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/interrupt", withAuth(cfg, mutating(svc, cfg, handleInterrupt(svc))))
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
			logMutation(p, need, target, r)
			next(w, r)
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
			// No event bus exists in this skeleton (see fleet.Event's doc
			// comment on unresolved SSE framing) — cursor is always 0,
			// honestly, rather than a fabricated counter.
			"cursor":    int64(0),
			"startedAt": svc.startedAt.Format(time.RFC3339Nano),
			"drivers":   svc.driverSummaries(),
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

		col, err := svc.ListSessions(r.Context(), requestFrom(r), scope, filter, parseDeadline(r))
		if err != nil {
			writeError(w, &fleet.Error{Kind: fleet.ErrorInvalid, Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, col)
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
		state, err := d.State(ctx, requestFrom(r), ref)
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

		receipt, err := d.Send(ctx, requestFrom(r), fleet.SessionRef{Machine: machine, ID: id}, body.Text, driver.SendOptions{Submit: body.Submit})
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
	body, err := json.Marshal(sseEnvelope{
		Cursor: ev.Cursor, Epoch: ev.Epoch, Machine: ev.Machine,
		Kind: ev.Kind, Payload: ev.Payload, Origin: ev.Origin,
	})
	if err != nil {
		return
	}
	if ev.Cursor > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.Cursor)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, body)
	f.Flush()
}

// sseEnvelope is fleet.Event's wire form. It exists rather than marshalling
// fleet.Event directly so the JSON shape of the stream is stated in one place
// and cannot drift with the in-memory type.
type sseEnvelope struct {
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
