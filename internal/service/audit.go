package service

import (
	"log"
	"net/http"

	fleet "github.com/godx-jp/colab-fleet"
)

// §6 requirement 4: "Log every remote-originated mutation — actor, verb,
// target, outcome. This is the audit trail that replaces 'it could only ever
// have been me.'"
//
// That requirement was unimplementable until principals existed. A shared
// token can name an address, and an audit line reading "something at
// 10.x.y.z closed a session" answers where from while never answering who —
// which is the only question an audit trail is asked.
//
// Denials are logged as well as grants. A refused attempt is the more
// interesting line of the two: permitted work is expected, and a pattern of
// refusals is the first sign that a credential is somewhere it should not be.

// actorOf composes the audit actor through the SAME path that builds
// Request.Caller, rather than reaching for Principal.Name directly.
//
// The first version did reach for the name, and a live relayed mutation logged
// actor="<the relaying machine>" — losing the assertion that exists precisely
// so the peer can record who asked. Two places deriving "who is acting" is one
// too many, and the one nobody reads during a test is the one that drifts.
func actorOf(p Principal, r *http.Request) string {
	return callerFor(p, r).Principal
}

// auditWriter records the status a handler actually produced, so the audit
// line can state the OUTCOME rather than the authorization decision.
//
// The first version logged before running the handler, so every attempt read
// `outcome=permitted`. Four discard attempts in one minute — two refused with
// 409, one performed, one an idempotent retry — produced four identical lines.
// §6 requirement 4 asks for "actor, verb, target, outcome", and an audit trail
// that cannot distinguish a refusal from a destruction is not one.
type auditWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// outcomeOf names what happened in words an operator reads at speed.
//
// A refusal is called refused rather than "error": these are mostly §5.4
// corroboration failures, which mean the caller's belief was stale — a working
// safety property, not a fault, and it should not read like one at 3am.
func outcomeOf(status int) string {
	switch {
	case status == 0:
		return "no-response"
	case status >= 200 && status < 300:
		return "performed"
	case status == http.StatusConflict:
		return "refused-stale"
	case status == http.StatusNotFound:
		return "no-such-session"
	case status >= 400 && status < 500:
		return "refused"
	default:
		return "failed"
	}
}

func logMutation(p Principal, g Grant, target fleet.MachineId, r *http.Request, status int) {
	if g == GrantRead {
		return // reads are not mutations; logging them would drown the signal
	}
	log.Printf("audit: actor=%q verb=%s target=%s/%s outcome=%s status=%d",
		actorOf(p, r), g, target, r.PathValue("id"), outcomeOf(status), status)
}

func logDenied(p Principal, g Grant, target fleet.MachineId, r *http.Request) {
	log.Printf("audit: actor=%q verb=%s target=%s/%s outcome=DENIED",
		actorOf(p, r), g, target, r.PathValue("id"))
}
