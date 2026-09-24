package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// #184's cross-path ledger. The property under test: once an inbox write could
// not be confirmed, NOTHING later — a resumeIfStranded retry, a fresh auto send,
// a forced terminal send, an explicit inbox request — puts the same text in
// front of the receiver again. Every follow-up here asserts on the receivers,
// not on the receipts: how many times the message reached a session.

// unconfirmedRig sends "hello" to a receiver that reads it and records nothing,
// so the send ends unknown with a ledger entry.
func unconfirmedRig(t *testing.T, extra ...Option) (*Driver, *fakeMux, *inboxReceiver) {
	t.Helper()
	return unconfirmedRigFrom(t, nil, extra...)
}

// unconfirmedRigFrom is unconfirmedRig with the first send carrying `from`.
func unconfirmedRigFrom(t *testing.T, from *fleet.MessageFrom, extra ...Option) (*Driver, *fakeMux, *inboxReceiver) {
	t.Helper()
	rcv := newInboxReceiver(t)
	d, f := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverSilent), extra...)
	opts := routeOpts(fleet.RouteAuto)
	opts.From = from
	first, err := d.Send(context.Background(), testCaller, alphaRef, "hello", opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown || first.RouteOf() != fleet.RouteInbox {
		t.Fatalf("setup: first send = %+v, want unknown on the inbox", first)
	}
	if deliveries(rcv, f) != 1 {
		t.Fatalf("setup: %d deliveries after the first send, want 1", deliveries(rcv, f))
	}
	return d, f, rcv
}

// Every way of trying again, and each must leave the delivery count at one.
func TestLedger_InboxUnknown_NoFollowUpReachesEitherPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts driver.SendOptions
	}{
		{"a resumeIfStranded retry — the documented way to try again", driver.SendOptions{Submit: true, ResumeIfStranded: true}},
		{"a fresh auto send", routeOpts(fleet.RouteAuto)},
		{"a forced terminal send", routeOpts(fleet.RouteTerminal)},
		{"an explicit inbox request", routeOpts(fleet.RouteInbox)},
		{"a send with the text placed but not submitted", driver.SendOptions{Submit: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f, rcv := unconfirmedRig(t)
			got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeUnknown || got.RouteOf() != fleet.RouteInbox {
				t.Errorf("follow-up = %+v, want the earlier send's unknown, on the inbox", got)
			}
			if !strings.Contains(got.Reason, "NOT been sent again") {
				t.Errorf("reason = %q, want it to say the text was not sent again", got.Reason)
			}
			if n := deliveries(rcv, f); n != 1 {
				t.Fatalf("%d deliveries after the follow-up, want still 1 (socket %d, pane %d)", n, rcv.received(), len(f.pasteLog))
			}
			if d.Counters()[counterRouteGuardInboxUnconfirmed] != 1 {
				t.Errorf("route.guard.inbox_unconfirmed = %d, want 1", d.Counters()[counterRouteGuardInboxUnconfirmed])
			}
		})
	}
}

// The hold is on ONE delivery: different text, or the same text from a different
// sender, is a different message and goes through.
func TestLedger_DifferentTextOrSender_NotBlocked(t *testing.T) {
	d, f, rcv := unconfirmedRig(t)
	rcv2 := rcv // the same receiver: it stays silent, so both sends end unknown
	_ = rcv2

	got, err := d.Send(context.Background(), testCaller, alphaRef, "something else entirely", routeOpts(fleet.RouteAuto))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("different text was held by the ledger: %+v", got)
	}
	if n := deliveries(rcv, f); n != 2 {
		t.Fatalf("%d deliveries after a different message, want 2", n)
	}

	from := &fleet.MessageFrom{Agent: "another-agent"}
	got, err = d.Send(context.Background(), testCaller, alphaRef, "hello", driver.SendOptions{Submit: true, From: from})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("the same text from another sender was held by the ledger: %+v", got)
	}
	if n := deliveries(rcv, f); n != 3 {
		t.Fatalf("%d deliveries after the same text from another sender, want 3", n)
	}
}

// #191: the machine a request entered through is where it arrived, not who sent
// it, and a caller that fails over to the peer changes it while everything else
// stays the same. The retry must meet the entry the first send left.
func TestLedger_RetryThroughAnotherMachine_IsHeld(t *testing.T) {
	first := &fleet.MessageFrom{Agent: "planner", Session: "s-1", Machine: "machine-a"}
	d, f, rcv := unconfirmedRigFrom(t, first)

	for _, tc := range []struct {
		name string
		opts driver.SendOptions
	}{
		{"a fresh auto send", routeOpts(fleet.RouteAuto)},
		{"a resumeIfStranded retry", driver.SendOptions{Submit: true, ResumeIfStranded: true}},
		{"a forced terminal send", routeOpts(fleet.RouteTerminal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry := tc.opts
			retry.From = &fleet.MessageFrom{Agent: "planner", Session: "s-1", Machine: "machine-b"}
			got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", retry)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "NOT been sent again") {
				t.Fatalf("retry through the other machine = %+v, want the earlier send's unknown", got)
			}
			if n := deliveries(rcv, f); n != 1 {
				t.Fatalf("%d deliveries after a retry through the other machine, want still 1", n)
			}
		})
	}
	if d.Counters()[counterRouteGuardInboxUnconfirmed] != 3 {
		t.Errorf("route.guard.inbox_unconfirmed = %d, want 3", d.Counters()[counterRouteGuardInboxUnconfirmed])
	}
}

// The other half of #191: leaving the machine out must not make different
// senders one. A different agent or session is a different message; and a
// message that named nothing, labelled by the service with only its machine, is
// not the same as an unlabelled one — a human relay's words go to the terminal
// with no label, and an anonymous peer's identical text must not hold them.
func TestLedger_SenderIdentityStillSeparatesMessages(t *testing.T) {
	anonymous := &fleet.MessageFrom{Machine: "machine-a"}
	d, f, rcv := unconfirmedRigFrom(t, anonymous)

	// The same anonymous sender through the other machine: held.
	retry := routeOpts(fleet.RouteAuto)
	retry.From = &fleet.MessageFrom{Machine: "machine-b"}
	got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", retry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("an anonymous retry through the other machine was not held: %+v", got)
	}
	if n := deliveries(rcv, f); n != 1 {
		t.Fatalf("%d deliveries after the anonymous retry, want 1", n)
	}

	// An unlabelled send (a human relay: no `from`, terminal): not held.
	got, err = d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(fleet.RouteTerminal))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("an unlabelled send was held for an anonymous peer's identical text: %+v", got)
	}
	if n := deliveries(rcv, f); n != 2 {
		t.Fatalf("%d deliveries after the unlabelled send, want 2", n)
	}

	// A named sender: not held either.
	named := routeOpts(fleet.RouteTerminal)
	named.From = &fleet.MessageFrom{Agent: "planner", Machine: "machine-b"}
	got, err = d.Send(context.Background(), testCaller, alphaRef, "hello", named)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("a named sender was held for an anonymous peer's identical text: %+v", got)
	}
}

// deliveryKey, unit-sized: what is in the key and what is not.
func TestDeliveryKey(t *testing.T) {
	from := func(agent, session string, machine fleet.MachineId, relay bool) *fleet.MessageFrom {
		return &fleet.MessageFrom{Agent: agent, Session: session, Machine: machine, RelayOfHuman: relay}
	}
	base := deliveryKey("hello", from("planner", "s-1", "machine-a", false))

	for _, tc := range []struct {
		name string
		key  string
		same bool
	}{
		{"the same sender entering through another machine", deliveryKey("hello", from("planner", "s-1", "machine-b", false)), true},
		{"the same sender with no machine stamped", deliveryKey("hello", from("planner", "s-1", "", false)), true},
		{"a different agent", deliveryKey("hello", from("reviewer", "s-1", "machine-a", false)), false},
		{"a different session", deliveryKey("hello", from("planner", "s-2", "machine-a", false)), false},
		{"different text", deliveryKey("goodbye", from("planner", "s-1", "machine-a", false)), false},
		{"a human-relay declaration", deliveryKey("hello", from("planner", "s-1", "machine-a", true)), false},
		{"no sender at all", deliveryKey("hello", nil), false},
	} {
		if got := tc.key == base; got != tc.same {
			t.Errorf("%s: key equal = %v, want %v", tc.name, got, tc.same)
		}
	}

	anon := deliveryKey("hello", from("", "", "machine-a", false))
	if anon != deliveryKey("hello", from("", "", "machine-b", false)) {
		t.Error("an anonymous sender through two machines should be one delivery")
	}
	if anon == deliveryKey("hello", nil) {
		t.Error("an anonymous labelled sender must not equal an unlabelled one")
	}
	if deliveryKey("hello", &fleet.MessageFrom{}) != deliveryKey("hello", nil) {
		t.Error("a `from` that renders no label is unlabelled, and should key like nil")
	}
}

// The ledger holds unconfirmedPerSession entries for a session (ADR 184, The
// cross-path ledger): one more distinct unconfirmed message evicts the oldest,
// and a retry of the evicted one is no longer held. That is a known limit, pinned
// here so a change to it is a decision rather than an accident.
func TestLedger_BoundEvictsTheOldest(t *testing.T) {
	rcv := newInboxReceiver(t)
	d, _ := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverSilent))

	total := unconfirmedPerSession + 1
	for i := 0; i < total; i++ {
		got, err := d.Send(context.Background(), testCaller, alphaRef, fmt.Sprintf("message %d", i), routeOpts(fleet.RouteAuto))
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeUnknown || got.RouteOf() != fleet.RouteInbox {
			t.Fatalf("message %d = %+v, want unknown on the inbox", i, got)
		}
	}
	if _, held := d.unconfirmedFor("alpha💬", deliveryKey("message 0", nil)); held {
		t.Errorf("the oldest of %d unconfirmed messages is still held; the bound is %d", total, unconfirmedPerSession)
	}
	for i := 1; i < total; i++ {
		if _, held := d.unconfirmedFor("alpha💬", deliveryKey(fmt.Sprintf("message %d", i), nil)); !held {
			t.Errorf("message %d fell out of the ledger before the bound of %d was exceeded", i, unconfirmedPerSession)
		}
	}
}

// A follow-up that finds the earlier write recorded AFTER ALL says so — and
// writes nothing. The hold clears with it: the delivery is done.
func TestLedger_LateConfirmation_DeliveredWithoutWriting(t *testing.T) {
	d, f, rcv := unconfirmedRig(t)
	rcv.recordPeerMessage(rcv.messages[0]) // the receiver got round to recording it

	got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeDelivered || got.RouteOf() != fleet.RouteInbox {
		t.Fatalf("follow-up = %+v, want delivered on the inbox", got)
	}
	if n := deliveries(rcv, f); n != 1 {
		t.Fatalf("%d deliveries after a late confirmation, want still 1", n)
	}
	if d.Counters()[counterRouteGuardLateConfirmed] != 1 {
		t.Errorf("route.guard.late_confirmed = %d, want 1", d.Counters()[counterRouteGuardLateConfirmed])
	}
	if _, held := d.unconfirmedFor("alpha💬", deliveryKey("hello", nil)); held {
		t.Error("the ledger still holds a delivery that has been confirmed")
	}
}

// The ledger is persisted with the stranded records: a restart must not turn
// "written, unconfirmed" back into "never sent". And it lapses like they do.
func TestLedger_SurvivesARestartAndExpires(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1785760000, 0)
	clock := func() time.Time { return now }

	_, f, rcv := unconfirmedRig(t, WithState(st), withClock(clock))

	build := func(at time.Time) *Driver {
		now = at
		ps := &fakePS{}
		ps.set(100, time.Now())
		ps.set(200, time.Now())
		return newInboxTestDriverWith(f, ps, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverSilent),
			append(rcv.options(), WithState(st), withClock(clock))...)
	}

	restarted := build(now.Add(time.Minute))
	got, err := restarted.Send(context.Background(), testCaller, alphaRef, "hello", driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("after a restart the follow-up = %+v, want the earlier send's unknown", got)
	}
	if n := deliveries(rcv, f); n != 1 {
		t.Fatalf("%d deliveries across the restart, want 1", n)
	}

	lapsed := build(now.Add(strandedRetention + time.Minute))
	got, err = lapsed.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(fleet.RouteAuto))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("a lapsed entry still held the message: %+v", got)
	}
	if n := deliveries(rcv, f); n != 2 {
		t.Fatalf("%d deliveries after the entry lapsed, want 2", n)
	}
}

// §5.4: an id is recyclable. An entry made for one session must not gate an
// unrelated one that later took the same name.
func TestLedger_RecycledSessionIsNotBlocked(t *testing.T) {
	d, f, rcv := unconfirmedRig(t)
	f.mu.Lock()
	f.sessions[0].cwd = "/work/somewhere-else"
	f.mu.Unlock()

	got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(fleet.RouteAuto))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Reason, "NOT been sent again") {
		t.Fatalf("an entry for another session's working directory held this one: %+v", got)
	}
	if _, held := d.unconfirmedFor("alpha💬", deliveryKey("hello", nil)); held {
		t.Error("the entry for a session that no longer exists was kept")
	}
	_ = rcv
}

// The other direction. A terminal delivery that could not be confirmed leaves the
// text in the composer with a stranded record, and the same text must not ALSO
// be sent as a peer message: it would arrive once, and again when the composer is
// submitted.
func TestSend_TerminalUnknown_FollowUpNeverTakesInbox(t *testing.T) {
	rcv := newInboxReceiver(t)
	d, f := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverRecordsEnvelope))
	f.swallowSubmit = true // the composer takes the paste and the submit never registers

	first, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(fleet.RouteTerminal))
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown || first.RouteOf() != fleet.RouteTerminal {
		t.Fatalf("setup: first send = %+v, want an unconfirmed terminal delivery", first)
	}

	for _, route := range []fleet.Route{fleet.RouteAuto, fleet.RouteInbox} {
		got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(route))
		if err != nil {
			t.Fatal(err)
		}
		if rcv.dialCount() != 0 || rcv.received() != 0 {
			t.Fatalf("route %s: the inbox was used for text already stranded in the composer", route)
		}
		switch route {
		case fleet.RouteAuto:
			if got.RouteOf() != fleet.RouteTerminal {
				t.Errorf("auto follow-up = %+v, want the terminal path to answer", got)
			}
		case fleet.RouteInbox:
			if got.Outcome != fleet.OutcomeRefused || got.RouteOf() != fleet.RouteInbox {
				t.Errorf("inbox follow-up = %+v, want a refusal naming the inbox", got)
			}
		}
	}
	if n := d.Counters()[counterRouteGuardTerminalUnconfirm]; n != 2 {
		t.Errorf("route.guard.terminal_unconfirmed = %d, want 2", n)
	}
}
