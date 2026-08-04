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

func logMutation(p Principal, g Grant, target fleet.MachineId, r *http.Request) {
	if g == GrantRead {
		return // reads are not mutations; logging them would drown the signal
	}
	log.Printf("audit: actor=%q verb=%s target=%s/%s outcome=permitted",
		actorOf(p, r), g, target, r.PathValue("id"))
}

func logDenied(p Principal, g Grant, target fleet.MachineId, r *http.Request) {
	log.Printf("audit: actor=%q verb=%s target=%s/%s outcome=DENIED",
		actorOf(p, r), g, target, r.PathValue("id"))
}
