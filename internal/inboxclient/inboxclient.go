// Package inboxclient speaks the wire protocol colab-fleet #115 measured
// empirically against a target session's own inbox — a small, undocumented,
// third-party surface (see that issue): newline-delimited JSON, an auth line
// carrying a per-session token, then a message line carrying the text as a
// genuine user turn.
//
// #144 corrected this package's framing to match the runtime's own startup
// documentation exactly. #143 found the version that shipped before this was
// never validated against a real inbox and was wrong on both lines — its
// tests ran only over net.Pipe, against its own assumed shape, so nothing in
// the suite could have caught that. #144 also removed the response-line read
// the original design assumed: see Deliver's own doc comment for why.
//
// This package knows only the bytes. It never dials anything itself, never
// resolves a socket path, and never reads a credential off disk — a caller
// hands it an already-open net.Conn and the token to send, and gets back a
// receipt or an error. Where either endpoint of that connection lives, and
// where its token comes from, is machine-local knowledge this repository
// does not commit (CLAUDE.local.md; the precedent is internal/probe's own
// #118 spike test, which generalizes "a directory, one file per address"
// rather than naming the real one). The literal field values the runtime
// requires are likewise not repeated in prose here or anywhere else in this
// repo — CLAUDE.local.md says where to read them on a machine that has one.
package inboxclient

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Outcome is the closed set of receipts colab-fleet #115 originally observed
// the inbox return, transcribed verbatim. #117's ruling requires a caller to
// surface these honestly rather than flatten them into whatever vocabulary
// it had before this package existed — see fleet.Outcome's own #119
// additions, which this type maps onto one-for-one, never many-to-one.
//
// #144: Deliver itself currently only ever produces OutcomeDelivered (see
// its own doc comment) — a plain, unaddressed sender has no way to observe
// the other five today. They stay part of this closed set regardless,
// because #117's ruling is about the vocabulary a caller must be able to
// receive without flattening, not about what this package can prove right
// now; a future fix to #120 (the reply-address question) is what would let
// Deliver start producing them.
type Outcome string

const (
	OutcomeDelivered Outcome = "delivered"
	OutcomeHeld      Outcome = "held"
	OutcomeDenied    Outcome = "denied"
	OutcomeExpired   Outcome = "expired"
	OutcomeRefused   Outcome = "refused"
	OutcomeDropped   Outcome = "dropped"
)

// FirstLineDeadline is #115's own measured guard, transcribed: a connection
// that sends no complete first line inside a few seconds is closed by the
// far end, silently — no error, no close reason. This package's own default
// write budget is pinned to the same window a caller cannot usefully exceed:
// waiting longer than the far end will wait buys nothing but a slower
// failure.
const FirstLineDeadline = 3 * time.Second

// Receipt is Deliver's own answer for one call. #144: today this is always
// {Outcome: OutcomeDelivered} on a nil error — see Deliver's doc comment —
// but the shape stays a struct rather than a bare bool because a future fix
// to #120 would fill Reason and produce the other five Outcome values
// without changing this type or any caller's shape.
type Receipt struct {
	Outcome Outcome
	Reason  string
}

// authLine and messageLine are this package's transcription of the two
// request lines the runtime documents in its own startup output — corrected
// under #144 after #143 found the previous version wrong on both lines. Per
// CLAUDE.local.md this repository does not restate the runtime's own
// documentation as prose; read it on a machine that has one.
type authLine struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type messageLine struct {
	Type    string          `json:"type"`
	Message messageLineBody `json:"message"`
}

type messageLineBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Deliver writes one auth line and one message line to conn. timeout bounds
// the write (FirstLineDeadline if zero or negative); the caller's ctx
// deadline, if tighter, wins — this package never waits past either.
//
// # Why this does not wait for a response
//
// The version of this package #144 replaced read exactly one response line
// after writing, treating its absence as an unresolved outcome. #143
// measured, from a plain external process against a live session, that
// after a delivery that fully succeeded — the message genuinely arrived and
// the target session answered it — the sending connection receives zero
// bytes over a 12-second window, with or without a correlation id on the
// message. The runtime's own status appears to travel as a separate frame
// to a bound reply address, which a plain sender like this one does not
// have (#120, unresolved). So the old read was not waiting for a receipt
// this protocol ever intends to hand this kind of caller — it was a wait for
// something that does not arrive, and #144 deliberately does not design
// around #120 as though it were already solved.
//
// A clean two-line write is therefore reported OutcomeDelivered outright:
// the honest limit of what this package can know without a reply address is
// "the bytes reached the socket," and that is what it reports. #115 already
// recorded the one failure this cannot see through even in principle — an
// unparseable auth line is dropped silently by the far end, so a wrong
// token and a correct one are indistinguishable from outside. That is a
// property of the protocol itself, not a gap this package leaves open by no
// longer reading; nothing a plain sender could read would have told the two
// apart either.
//
// conn is never closed here — opening and closing it is the caller's own
// concern (dialing needs a socket path and a network this package is not
// told, see the package doc comment).
func Deliver(conn net.Conn, token, text string, timeout time.Duration) (Receipt, error) {
	if timeout <= 0 {
		timeout = FirstLineDeadline
	}
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return Receipt{}, fmt.Errorf("inboxclient: setting write deadline: %w", err)
	}

	enc := json.NewEncoder(conn) // Encode always appends exactly one '\n'
	if err := enc.Encode(authLine{Type: "auth", Token: token}); err != nil {
		return Receipt{}, fmt.Errorf("inboxclient: writing auth line: %w", err)
	}
	msg := messageLine{Type: "user", Message: messageLineBody{Role: "user", Content: text}}
	if err := enc.Encode(msg); err != nil {
		return Receipt{}, fmt.Errorf("inboxclient: writing message line: %w", err)
	}
	return Receipt{Outcome: OutcomeDelivered}, nil
}
