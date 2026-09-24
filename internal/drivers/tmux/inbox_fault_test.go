package tmux

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// #184's headline acceptance: two delivery paths must never both deliver one
// message. A fault is injected at every step an inbox send passes through —
// before anything is written, mid-write, after a complete write nothing
// confirms — and after each the message must have reached a receiver AT MOST
// ONCE, by any path. A fault that leaves the message possibly delivered must
// then survive every kind of second attempt.
//
// "Possibly delivered" counts a write that broke part-way: the receiver was
// given bytes, and a sender cannot prove a partial line is discarded, so it is
// one delivery that might have happened. The pane is the other place a message
// can arrive. The sum of the two is what must stay at or below one.

type faultKnobs struct {
	noResolver   bool
	resolver     InboxResolver
	dial         func(rcv *inboxReceiver, cancel context.CancelFunc) inboxDialFunc
	noTranscript bool
	noProcess    bool
	flipAt       int // verify the process identity as changed from this `ps` call on
	text         string
}

type faultRow struct {
	name         string
	knobs        faultKnobs
	wantOutcome  fleet.Outcome
	wantRoute    fleet.Route
	wantPossible int // deliveries by any path, counting a partial write as one
	// wantHeld is true when the outcome leaves the message possibly in a
	// receiver's hands, so every follow-up must be answered without a write.
	wantHeld bool
}

func possibleDeliveries(rcv *inboxReceiver, f *fakeMux) int {
	f.mu.Lock()
	pane := len(f.pasteLog)
	f.mu.Unlock()
	n := rcv.received() + pane
	if rcv.received() == 0 && rcv.bytesReceived() > 0 {
		n++ // a broken write: the receiver was given something
	}
	return n
}

func TestSend_InboxFaultMatrix_AtMostOneDelivery(t *testing.T) {
	record := func(rcv *inboxReceiver, _ context.CancelFunc) inboxDialFunc {
		return rcv.dialer(receiverRecordsEnvelope)
	}
	silent := func(rcv *inboxReceiver, _ context.CancelFunc) inboxDialFunc { return rcv.dialer(receiverSilent) }
	budget := func(n int) func(*inboxReceiver, context.CancelFunc) inboxDialFunc {
		return func(rcv *inboxReceiver, _ context.CancelFunc) inboxDialFunc { return rcv.budgetDialer(n) }
	}
	declined := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) { return InboxAddress{}, false, nil }
	resolverErr := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{}, false, errors.New("index unreadable")
	}

	rows := []faultRow{
		// --- faults BEFORE any byte is written: the terminal may carry it ---
		{"the index has no entry", faultKnobs{resolver: declined, dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"the index cannot be read", faultKnobs{resolver: resolverErr, dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"no inbox configured", faultKnobs{noResolver: true, dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"the process cannot be identified", faultKnobs{noProcess: true, dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"the index names no class", faultKnobs{resolver: attestableResolver(""), dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"the text cannot be attested", faultKnobs{text: "has a ＜ lookalike", dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"no transcript to confirm against", faultKnobs{noTranscript: true, dial: record}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"the socket cannot be dialled", faultKnobs{dial: func(*inboxReceiver, context.CancelFunc) inboxDialFunc {
			return func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
		}}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},
		{"the write puts nothing on the socket", faultKnobs{dial: func(*inboxReceiver, context.CancelFunc) inboxDialFunc {
			return pipeDialer(t, closeBeforeReading)
		}}, fleet.OutcomeQueued, fleet.RouteTerminal, 1, false},

		// --- identity faults: final refusals, nothing sent on either path ---
		{"identity fails the first verification", faultKnobs{flipAt: 2, dial: record}, fleet.OutcomeRefused, fleet.RouteInbox, 0, false},
		{"identity fails the second verification", faultKnobs{flipAt: 3, dial: record}, fleet.OutcomeRefused, fleet.RouteInbox, 0, false},

		// --- faults AFTER a byte is written: unknown, never a terminal resend ---
		{"the write breaks inside the auth line", faultKnobs{dial: budget(3)}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},
		{"the write breaks one byte into the message", faultKnobs{dial: budget(31)}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},
		{"the write breaks part-way through the message", faultKnobs{dial: budget(60)}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},
		{"a complete write the receiver never records", faultKnobs{dial: silent}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},
		{"a complete write; only another sender's envelope is recorded", faultKnobs{dial: func(rcv *inboxReceiver, _ context.CancelFunc) inboxDialFunc {
			return rcv.dialerFunc(func(content string) {
				other, _ := inboxclient.Attest(innerBody(content), inboxclient.ModeBypass, "someone-else")
				rcv.recordPeerMessage(other)
			})
		}}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},
		{"a complete write; the transcript cannot be read afterwards", faultKnobs{dial: func(rcv *inboxReceiver, _ context.CancelFunc) inboxDialFunc {
			return rcv.dialerFunc(func(string) { _ = os.Remove(rcv.transcript) })
		}}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},
		{"a complete write; the caller gives up while waiting", faultKnobs{dial: func(rcv *inboxReceiver, cancel context.CancelFunc) inboxDialFunc {
			return rcv.dialerFunc(func(string) { cancel() })
		}}, fleet.OutcomeUnknown, fleet.RouteInbox, 1, true},

		// --- the message arrives: exactly one delivery ---
		{"a complete write the receiver records", faultKnobs{dial: record}, fleet.OutcomeDelivered, fleet.RouteInbox, 1, false},
		{"a complete write recorded without its wrapper", faultKnobs{dial: func(rcv *inboxReceiver, _ context.CancelFunc) inboxDialFunc {
			return rcv.dialer(receiverRecordsBody)
		}}, fleet.OutcomeDelivered, fleet.RouteInbox, 1, false},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			k := row.knobs
			text := k.text
			if text == "" {
				text = "the payload"
			}
			rcv := newInboxReceiver(t)
			f := twoSessions()
			ps := &fakePS{}
			if !k.noProcess {
				ps.set(100, time.Now())
			}
			ps.set(200, time.Now())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			resolver := k.resolver
			if resolver == nil && !k.noResolver {
				resolver = attestableResolver(inboxclient.ModeBypass)
			}
			var extra []Option
			if !k.noTranscript {
				extra = rcv.options()
			}
			d := newInboxTestDriverWith(f, ps, resolver, k.dial(rcv, cancel), extra...)
			if k.flipAt > 0 {
				d.psRun = (&flipPS{flipAt: k.flipAt, before: time.Unix(1785700000, 0), after: time.Unix(1785700999, 0)}).exec
			}

			got, err := d.Send(ctx, testCaller, alphaRef, text, routeOpts(fleet.RouteAuto))
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != row.wantOutcome || got.RouteOf() != row.wantRoute {
				t.Fatalf("got outcome %q route %q (%s), want %q via %q",
					got.Outcome, got.RouteOf(), got.Reason, row.wantOutcome, row.wantRoute)
			}
			if n := possibleDeliveries(rcv, f); n != row.wantPossible {
				t.Fatalf("the message may have reached a receiver %d times (socket %d, broken write %v, pane %d), want %d",
					n, rcv.received(), rcv.bytesReceived() > 0 && rcv.received() == 0, len(f.pasteLog), row.wantPossible)
			}
			if n := possibleDeliveries(rcv, f); n > 1 {
				t.Fatalf("duplicate delivery: %d", n)
			}

			if !row.wantHeld {
				return
			}
			// The message may be in a receiver's hands. Every second attempt
			// must be answered without writing anything, on either path.
			for _, opts := range []driver.SendOptions{
				{Submit: true, ResumeIfStranded: true},
				routeOpts(fleet.RouteAuto),
				routeOpts(fleet.RouteTerminal),
				routeOpts(fleet.RouteInbox),
			} {
				again, err := d.Send(context.Background(), testCaller, alphaRef, text, opts)
				if err != nil {
					t.Fatal(err)
				}
				if again.RouteOf() != fleet.RouteInbox {
					t.Errorf("follow-up %+v answered %+v, want the inbox's account of it", opts, again)
				}
				if n := possibleDeliveries(rcv, f); n != row.wantPossible {
					t.Fatalf("a follow-up (%+v) took the deliveries from %d to %d", opts, row.wantPossible, n)
				}
			}
			if f.pasteLogLen() != 0 {
				t.Errorf("the pane received text after an inbox write that could have delivered it")
			}
		})
	}
}
