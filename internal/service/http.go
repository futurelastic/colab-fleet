package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
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

	// AllowMutations opens the verbs that change something: create, input,
	// interrupt, close. It defaults to FALSE, and that default is §6
	// requirement 3 taken literally — "`list` and `state` from a peer may
	// be permitted by default; `close`, `interrupt` and `create` are opt-in
	// per peer."
	//
	// The gate exists because authentication alone does not express this.
	// A single shared token cannot distinguish a peer from a local
	// supervisor, so without a verb gate, anything holding the token can
	// start processes on this machine. §6 is explicit that "a service that
	// can start processes and read files is a remote-execution surface
	// regardless of intent", and the first thing a fleet exposes across
	// machines should not be process creation.
	//
	// This is coarser than §6 asks for — it is per-service, not per-peer —
	// and deliberately so: per-peer authorization needs more than one peer
	// identity to distinguish, which a single shared token does not
	// provide. Coarse and closed beats fine-grained and unimplemented.
	AllowMutations bool
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
	mux.HandleFunc("POST /v1/machines/{machine}/sessions", withAuth(cfg, mutating(cfg, handleCreateSession(svc))))
	mux.HandleFunc("GET /v1/machines/{machine}/sessions/{id}", withAuth(cfg, handleGetSession(svc)))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/input", withAuth(cfg, mutating(cfg, handleSendInput(svc))))
	mux.HandleFunc("POST /v1/machines/{machine}/sessions/{id}/interrupt", withAuth(cfg, mutating(cfg, handleInterrupt(svc))))
	mux.HandleFunc("DELETE /v1/machines/{machine}/sessions/{id}", withAuth(cfg, mutating(cfg, handleClose(svc))))
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

// mutating gates the verbs that change something behind Config.AllowMutations
// (§6 requirement 3). A refusal here is unauthorized, not unsupported: the
// driver is perfectly capable, and this instance is configured not to permit
// it. Reporting "unsupported" would tell a caller something false about the
// runtime and invite it to give up permanently on a capability that exists.
func mutating(cfg Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.AllowMutations {
			writeError(w, &fleet.Error{
				Kind: fleet.ErrorUnauthorized,
				Message: "this instance is configured read-only; create, input, " +
					"interrupt and close are opt-in (§6)",
			})
			return
		}
		next(w, r)
	}
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
	switch {
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
		col, err := svc.ListMachines(r.Context(), parseDeadline(r))
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

		col, err := svc.ListSessions(r.Context(), scope, filter, parseDeadline(r))
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

		ref, err := d.Create(ctx, key, spec)
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}

		state, _ := d.State(ctx, ref)
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
		state, err := d.State(ctx, ref)
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

		receipt, err := d.Send(ctx, fleet.SessionRef{Machine: machine, ID: id}, body.Text, driver.SendOptions{Submit: body.Submit})
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

		ack, err := d.Interrupt(ctx, fleet.SessionRef{Machine: machine, ID: id})
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

		ack, err := d.Close(ctx, fleet.SessionRef{Machine: machine, ID: id})
		if err != nil {
			writeDriverError(w, machine, deadline, err)
			return
		}
		writeJSON(w, http.StatusAccepted, ack)
	}
}

func handleEvents(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Real SSE streaming (§4) is not implemented in this skeleton: no
		// driver here supports Subscribe (internal/drivers/stub always
		// returns driver.ErrUnsupported), and the wire framing question
		// noted on fleet.Event (does Kind travel as the SSE "event:" line,
		// a JSON field, or both?) is unresolved. Routed and answers
		// honestly rather than silently 404ing or hanging on an open
		// connection that never sends anything.
		writeError(w, &fleet.Error{Kind: fleet.ErrorUnsupported, Message: "event subscription is not implemented in this skeleton"})
	}
}
