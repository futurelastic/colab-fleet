// Package inboxclient speaks the wire protocol colab-fleet #115 measured
// empirically against a target session's own inbox — a small, undocumented,
// third-party surface (see that issue): newline-delimited JSON, an auth line
// carrying a per-session token, then a message line, then one response line
// carrying the delivery's own receipt.
//
// This package knows only the bytes. It never dials anything itself, never
// resolves a socket path, and never reads a credential off disk — a caller
// hands it an already-open net.Conn and the token to send, and gets back a
// receipt or an error. Where either endpoint of that connection lives, and
// where its token comes from, is machine-local knowledge this repository
// does not commit (CLAUDE.local.md; the precedent is internal/probe's own
// #118 spike test, which generalizes "a directory, one file per address"
// rather than naming the real one). Keeping that knowledge out of this
// package, not just out of its comments, is what makes it safe to test with
// an in-memory net.Pipe and never touch a real path.
package inboxclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Outcome is the closed set of receipts colab-fleet #115 observed the inbox
// return, transcribed verbatim. #117's ruling requires a caller to surface
// these honestly rather than flatten them into whatever vocabulary it had
// before this package existed — see fleet.Outcome's own #119 additions,
// which this type maps onto one-for-one, never many-to-one.
type Outcome string

const (
	OutcomeDelivered Outcome = "delivered"
	OutcomeHeld      Outcome = "held"
	OutcomeDenied    Outcome = "denied"
	OutcomeExpired   Outcome = "expired"
	OutcomeRefused   Outcome = "refused"
	OutcomeDropped   Outcome = "dropped"
)

func (o Outcome) valid() bool {
	switch o {
	case OutcomeDelivered, OutcomeHeld, OutcomeDenied, OutcomeExpired, OutcomeRefused, OutcomeDropped:
		return true
	default:
		return false
	}
}

// FirstLineDeadline is #115's own measured guard, transcribed: a connection
// that sends no complete first line inside a few seconds is closed by the
// far end, silently — no error, no close reason. This package's own default
// round-trip budget is pinned to the same window a caller cannot usefully
// exceed: waiting longer than the far end will wait buys nothing but a
// slower failure.
const FirstLineDeadline = 3 * time.Second

// Receipt is one resolved answer from the inbox: an Outcome from the closed
// set above, plus whatever prose the inbox itself attached.
type Receipt struct {
	Outcome Outcome
	Reason  string
}

// authLine and messageLine are this package's own encoding of the two
// request lines #115 measured — field names are this package's choice, not
// a transcription of the real wire shape, which #117's ruling authorises
// this service to hold but this PUBLIC repo does not commit (see the
// package doc comment).
type authLine struct {
	Token string `json:"token"`
}

type messageLine struct {
	Text string `json:"text"`
}

type receiptLine struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// ErrNoReceipt means the connection closed, or the deadline passed, before a
// complete response line arrived — the delivery's own outcome, as opposed to
// the transport's, is unresolved. A caller reports this the same way it
// reports any other unresolved delivery (fleet.OutcomeUnknown): sent, but
// what happened to it cannot be said.
var ErrNoReceipt = errors.New("inboxclient: connection ended before a receipt line arrived")

// Deliver writes one auth line and one message line to conn, then reads and
// parses exactly one response line. timeout bounds the whole exchange
// (FirstLineDeadline if zero or negative); the caller's ctx deadline, if
// tighter, wins — this package never waits past either.
//
// conn is never closed here — opening and closing it is the caller's own
// concern (dialing needs a socket path and a network this package is not
// told, see the doc comment), and a caller reusing a connection across calls
// would need to keep owning it regardless.
func Deliver(conn net.Conn, token, text string, timeout time.Duration) (Receipt, error) {
	if timeout <= 0 {
		timeout = FirstLineDeadline
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Receipt{}, fmt.Errorf("inboxclient: setting deadline: %w", err)
	}

	enc := json.NewEncoder(conn) // Encode always appends exactly one '\n'
	if err := enc.Encode(authLine{Token: token}); err != nil {
		return Receipt{}, fmt.Errorf("inboxclient: writing auth line: %w", err)
	}
	if err := enc.Encode(messageLine{Text: text}); err != nil {
		return Receipt{}, fmt.Errorf("inboxclient: writing message line: %w", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if err == nil {
			err = ErrNoReceipt
		}
		return Receipt{}, fmt.Errorf("%w: %v", ErrNoReceipt, err)
	}
	// A non-empty line was read even when err != nil (e.g. the far end
	// closed right after writing it, with no trailing '\n') — that is a
	// complete receipt arriving without a clean read, not an incomplete
	// one, so it is parsed rather than discarded.

	var parsed receiptLine
	if jerr := json.Unmarshal([]byte(line), &parsed); jerr != nil {
		return Receipt{}, fmt.Errorf("inboxclient: unparseable receipt line %q: %w", line, jerr)
	}
	outcome := Outcome(parsed.Outcome)
	if !outcome.valid() {
		return Receipt{}, fmt.Errorf("inboxclient: unrecognised receipt outcome %q", parsed.Outcome)
	}
	return Receipt{Outcome: outcome, Reason: parsed.Reason}, nil
}
