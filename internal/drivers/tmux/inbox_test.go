package tmux

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// newInboxTestDriver mirrors newProcessIdentityTestDriver (processidentity_test.go)
// plus the two seams this file's own inbox path adds. mux and ps stay
// separate test doubles for the same reason processidentity_test.go's own
// comment gives: a fake built for the multiplexer must not silently answer
// for the OS process table, and neither may silently answer for a socket
// dial.
func newInboxTestDriver(mux *fakeMux, ps *fakePS, resolver InboxResolver, dial inboxDialFunc) *Driver {
	return newInboxTestDriverWith(mux, ps, resolver, dial)
}

// newInboxTestDriverWith is newInboxTestDriver plus extra options — chiefly the
// receiver's transcript store, without which the inbox has nothing to confirm
// against and (#184) is not used.
func newInboxTestDriverWith(mux *fakeMux, ps *fakePS, resolver InboxResolver, dial inboxDialFunc, extra ...Option) *Driver {
	opts := []Option{
		withExec(mux.exec),
		withPSExec(ps.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	}
	if resolver != nil {
		opts = append(opts, WithInboxResolver(resolver))
	}
	if dial != nil {
		opts = append(opts, withInboxDial(dial))
	}
	opts = append(opts, extra...)
	return New("testbox", opts...)
}

// pipeDialer returns an inboxDialFunc that hands back one end of a
// net.Pipe and runs onServer against the other end in its own goroutine —
// the same pattern internal/inboxclient's own test file uses, reproduced
// here because it is a test double for THIS package's dial seam, not a
// caller of that package's own tests.
func pipeDialer(t *testing.T, onServer func(server net.Conn)) inboxDialFunc {
	t.Helper()
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		client, server := net.Pipe()
		go onServer(server)
		return client, nil
	}
}

// readTwoLinesNoReply builds an onServer func (see pipeDialer) that reads
// the two request lines — auth, then message, each its own blocking Write
// on the client side over net.Pipe's synchronous semantics, so each needs
// its own Read here — and never writes anything back. #144: this is what
// #143 measured a real inbox actually does even on a fully successful
// delivery (zero bytes over a 12-second window), so it is the realistic
// double now, not a special case.
func readTwoLinesNoReply(server net.Conn) {
	defer server.Close()
	reader := bufio.NewReader(server)
	if _, err := reader.ReadString('\n'); err != nil { // auth line
		return
	}
	if _, err := reader.ReadString('\n'); err != nil { // message line
		return
	}
	// No reply written — see #144's own doc comment on inboxclient.Deliver.
}

// closeBeforeReading builds an onServer func that closes immediately without
// reading anything, simulating a dial that succeeded but a write that then
// fails — the case #144 added a pane fallback for.
func closeBeforeReading(server net.Conn) {
	server.Close()
}

// TestCapabilities_DeliversToInbox_ReflectsWhetherAResolverIsConfigured is
// colab-fleet #122's own acceptance surface: an operator must be able to
// tell whether #119's path is wired without inferring it from a receipt's
// wording. This proves the declared capability actually tracks the one
// thing that decides sendViaInbox's first branch — d.inboxResolver == nil —
// rather than defaulting true, defaulting false regardless of wiring, or
// drifting from that check some other way a receipt-reading test would not
// catch.
func TestCapabilities_DeliversToInbox_ReflectsWhetherAResolverIsConfigured(t *testing.T) {
	withoutResolver := newInboxTestDriver(twoSessions(), &fakePS{}, nil, nil)
	if withoutResolver.Capabilities().DeliversToInbox {
		t.Error("DeliversToInbox = true with no WithInboxResolver call — #119's path cannot be live")
	}

	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{}, false, nil
	}
	withResolver := newInboxTestDriver(twoSessions(), &fakePS{}, resolver, nil)
	if !withResolver.Capabilities().DeliversToInbox {
		t.Error("DeliversToInbox = false with WithInboxResolver configured — an operator would wrongly conclude the path is unreachable")
	}
}

func TestSend_NoInboxResolverConfigured_BehavesExactlyAsBeforeIssue119(t *testing.T) {
	// #119's own contract: a Driver that never calls WithInboxResolver must
	// behave exactly as it did before this file existed. This is the same
	// fixture and assertion as TestSendDeliversWhenComposerIsEmpty
	// (tmux_test.go), reproduced here as a direct, local statement of that
	// contract rather than relying on the reader to infer it from a diff.
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	d := newInboxTestDriver(f, ps, nil, nil)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q, want queued (pane path, unchanged)", got.Outcome)
	}
	assertNoInboxCounters(t, d)
}

func TestSend_InboxCapabilityAbsent_FallsBackToPane(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{}, false, nil // no inbox for this target
	}
	d := newInboxTestDriver(f, ps, resolver, nil)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
	assertInboxExit(t, d, counterInboxFallbackResolverDeclined, 0, 0)
}

func TestSend_InboxResolverError_TreatedAsCapabilityAbsent(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{}, false, errors.New("credential store unreadable")
	}
	d := newInboxTestDriver(f, ps, resolver, nil)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q, want queued — a resolver error is capability-absent, not a refusal", got.Outcome)
	}
	assertInboxExit(t, d, counterInboxFallbackResolverError, 0, 0)
}

// TestSend_InboxIdentityVerificationFails_RefusesWithoutTouchingThePane is
// #116's central scenario applied to #119: the identity resolved cleanly,
// but by the time this driver is about to write, the pid has been recycled.
// The honest answer is a refusal, and the pane must never be touched as a
// fallback — falling back here would be delivering on the very best-guess
// #116 exists to forbid, just via a different surface.
func TestSend_InboxIdentityVerificationFails_RefusesWithoutTouchingThePane(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	startedAt := time.Date(2026, 8, 26, 10, 15, 23, 0, time.Local)
	ps.set(100, startedAt)
	ps.set(200, time.Now())

	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		// Simulate the process exiting and the kernel recycling pid 100
		// in the gap between Resolve (already done by the time this
		// resolver runs) and Verify (about to run right after).
		ps.set(100, startedAt.Add(5*time.Minute))
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
	}
	d := newInboxTestDriver(f, ps, resolver, nil)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
	if got.Reason == "" {
		t.Error("a refusal must explain itself (§2.4)")
	}
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && (c[0] == "paste-buffer" || c[0] == "load-buffer" || c[0] == "send-keys") {
			t.Fatalf("identity-refused delivery must never touch the pane, saw: %v", c)
		}
	}
	assertInboxExit(t, d, counterInboxRefusedIdentityUnverified, 0, 0)
}

// TestMapInboxOutcome_EveryValueSurfacesDistinctly is #119's own central
// assertion — the six-value vocabulary must reach a caller unflattened —
// exercised directly against the mapping function rather than through a
// live delivery. #144: inboxclient.Deliver itself only ever produces
// OutcomeDelivered today (no reply address to observe the other five over,
// #120), so this is no longer reachable end to end through Send; it stays a
// direct unit test of mapInboxOutcome so the mapping itself is still proven
// exhaustive and ready for whenever #120 lets Deliver produce the rest.
func TestMapInboxOutcome_EveryValueSurfacesDistinctly(t *testing.T) {
	cases := []struct {
		wire inboxclient.Outcome
		want fleet.Outcome
	}{
		{inboxclient.OutcomeDelivered, fleet.OutcomeDelivered},
		{inboxclient.OutcomeHeld, fleet.OutcomeHeld},
		{inboxclient.OutcomeDenied, fleet.OutcomeDenied},
		{inboxclient.OutcomeExpired, fleet.OutcomeExpired},
		{inboxclient.OutcomeRefused, fleet.OutcomeRefused},
		{inboxclient.OutcomeDropped, fleet.OutcomeDropped},
	}
	for _, tc := range cases {
		t.Run(string(tc.wire), func(t *testing.T) {
			if got := mapInboxOutcome(tc.wire); got != tc.want {
				t.Fatalf("mapInboxOutcome(%q) = %q, want %q", tc.wire, got, tc.want)
			}
		})
	}
}

// TestSend_InboxDeliverySucceeds_ReportsDeliveredWithoutTouchingPane is
// #144's own central case: a dial that succeeds and a write that succeeds,
// against a double that never writes anything back — exactly what #143
// measured a real inbox does even on a delivery that fully succeeds. Before
// #144 this hung waiting on a response line for the full round-trip timeout
// and then reported OutcomeUnknown; it must now report OutcomeDelivered
// promptly, and the pane must never be touched.
func TestSend_InboxDeliverySucceeds_ReportsDeliveredWithoutTouchingPane(t *testing.T) {
	// #148: the address now carries the target's permission-mode class. Without
	// it there is nothing to attest and this path is unavailable by design —
	// see TestSend_InboxWithoutModeClass_FallsBackToPane below.
	//
	// #184: and a delivery is reported delivered only when the receiver's own
	// transcript records it, so this receiver does.
	rcv := newInboxReceiver(t)
	d, f := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverRecordsEnvelope))

	start := time.Now()
	got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= inboxRoundTripTimeout {
		t.Errorf("Send took %s, at or beyond the round-trip timeout — it waited on a response line #144 says never arrives", elapsed)
	}
	if got.Outcome != fleet.OutcomeDelivered {
		t.Fatalf("outcome = %q (%s), want delivered", got.Outcome, got.Reason)
	}
	if got.RouteOf() != fleet.RouteInbox {
		t.Errorf("receipt route = %q, want inbox", got.RouteOf())
	}
	if paneTouched(f) {
		t.Fatalf("an inbox delivery must never also touch the pane, saw: %v", f.callsSnapshot())
	}
	assertInboxExit(t, d, counterInboxWritten, 1, 0)
	if c := d.Counters(); c[counterInboxConfirmed] != 1 || c[counterInboxConfirmedByEnvelope] != 1 {
		t.Errorf("confirmed=%d by_envelope=%d, want 1 and 1", c[counterInboxConfirmed], c[counterInboxConfirmedByEnvelope])
	}
}

// TestSend_InboxDialFails_FallsBackToPane is #144's fix for the gap #143's
// investigation named by exact quote: "the call site returns as soon as
// sendViaInbox reports ok=true, so a dial that succeeds then fails never
// reaches the pane." This covers the dial-failure half — before #144 this
// was a final, permanent OutcomeRefused with no pane attempt at all.
func TestSend_InboxDialFails_FallsBackToPane(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		// #150: a class is required to reach the dial at all — since #148,
		// without one this fixture fell back on the class check instead, and
		// passed only because both fallbacks land on the pane.
		return InboxAddress{
			Network: "unix", Socket: "/irrelevant", Token: "tok",
			ModeClass: inboxclient.ModeBypass,
		}, true, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	rcv := newInboxReceiver(t)
	d := newInboxTestDriverWith(f, ps, resolver, dial, rcv.options()...)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
	if got.RouteOf() != fleet.RouteTerminal {
		t.Errorf("receipt route = %q, want terminal", got.RouteOf())
	}
	assertInboxExit(t, d, counterInboxFallbackDialFailed, 1, 0)
}

// TestSend_InboxWriteFails_FallsBackToPane covers #143's other half of the
// same gap: a dial that succeeds and a write that then fails. Before #144
// this was a final, permanent OutcomeUnknown with no pane attempt at all.
func TestSend_InboxWriteFails_FallsBackToPane(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		// #150: a class is required to reach the write at all — since #148,
		// without one this fixture fell back on the class check instead, and
		// passed only because both fallbacks land on the pane.
		return InboxAddress{
			Network: "unix", Socket: "/irrelevant", Token: "tok",
			ModeClass: inboxclient.ModeBypass,
		}, true, nil
	}
	rcv := newInboxReceiver(t)
	d := newInboxTestDriverWith(f, ps, resolver, pipeDialer(t, closeBeforeReading), rcv.options()...)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
	// #184: a write that put not one byte on the connection is the only write
	// failure that may fall back; see TestSend_InboxPartialWrite_UnknownNoPane
	// for the one that may not.
	assertInboxExit(t, d, counterInboxFallbackWriteFailed, 1, 0)
}

// TestSend_InboxSkippedForPaneOnlyShapes proves inboxEligible's own three
// exclusions actually gate the driver call, not just the helper in
// isolation: a resolver that records whether it was ever invoked, for each
// of the three flag shapes #119 excludes.
func TestSend_InboxSkippedForPaneOnlyShapes(t *testing.T) {
	cases := []struct {
		name string
		opts driver.SendOptions
	}{
		{"land-without-submit", driver.SendOptions{Submit: false}},
		{"resume-if-stranded", driver.SendOptions{Submit: true, ResumeIfStranded: true}},
		{"replace-if-stranded", driver.SendOptions{Submit: true, ReplaceIfStranded: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := twoSessions()
			ps := &fakePS{}
			ps.set(100, time.Now())
			ps.set(200, time.Now())
			called := false
			resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
				called = true
				return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
			}
			d := newInboxTestDriver(f, ps, resolver, nil)

			// alpha's composer is empty, so a stranded-flag call has no
			// stranded record to act on; either shape is expected to reach
			// the pane path's own ordinary handling, never the inbox.
			_, _ = d.Send(context.Background(), testCaller,
				fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", tc.opts)
			if called {
				t.Errorf("%s: the inbox resolver was called; #119 excludes this shape entirely", tc.name)
			}
			assertNoInboxCounters(t, d)
		})
	}
}

// TestSend_InboxNoSuchSession_FallsThroughToTheSamePaneRefusal proves
// sendViaInbox does not duplicate the pane path's own "no session"/"dead
// pane" reporting: when #116 cannot resolve an identity at all, Send falls
// through and the caller sees exactly the refusal the pane path already
// gives — never a second, differently-worded one from this file.
func TestSend_InboxNoSuchSession_FallsThroughToTheSamePaneRefusal(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	called := false
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		called = true
		return InboxAddress{}, false, nil
	}
	d := newInboxTestDriver(f, ps, resolver, nil)

	got, _ := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "does-not-exist"}, "hello", driver.SendOptions{Submit: true})
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
	if called {
		t.Error("the inbox resolver was called for an identity #116 never resolved")
	}
	assertInboxExit(t, d, counterInboxFallbackIdentityUnresolved, 0, 0)
}

// TestSend_InboxWithoutModeClass_FallsBackToPane is colab-fleet #148's own
// regression test, and the one that actually proves the bug fixed.
//
// Before this change the resolver's address carried no permission-mode class,
// this driver asserted none, and the receiving runtime held every message sent
// to a session running with permission prompts bypassed — parked for a human
// who was never coming, then dropped at the receiver's own hold deadline —
// while this call reported `delivered`. #148 measured 206 such sends against
// 62 real ones, and one session unreachable for three and a half days.
//
// The fix is not a better receipt: there is no reply address to read one over
// (#120). It is refusing to claim the capability at all when the one fact that
// makes it work is missing. So the assertion here is deliberately about the
// PANE being used — an outcome of `delivered` on this test would mean the
// regression is back.
func TestSend_InboxWithoutModeClass_FallsBackToPane(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
	}
	dialled := false
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialled = true
		return nil, errors.New("this dial must never happen")
	}
	d := newInboxTestDriver(f, ps, resolver, dial)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeDelivered {
		t.Fatal("reported delivered for a send this service could not attest — #148's exact regression")
	}
	if dialled {
		t.Error("dialled the inbox for a send that could never be attested")
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
	assertInboxExit(t, d, counterInboxFallbackNoModeClass, 1, 0)
}

// TestSend_InboxUnattestableText_FallsBackToPane covers the second half of
// Attest's refusal: a class IS known, but the text cannot be wrapped in a form
// guaranteed to survive the receiver's byte-for-byte rebuild check. The
// receiver discards an envelope that does not rebuild identically, which loses
// the asserted class and lands the message right back in the held state — so
// an unwrappable body has to take the pane path too.
func TestSend_InboxUnattestableText_FallsBackToPane(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{
			Network: "unix", Socket: "/irrelevant", Token: "tok",
			ModeClass: inboxclient.ModeBypass,
		}, true, nil
	}
	dialled := false
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialled = true
		return nil, errors.New("this dial must never happen")
	}
	d := newInboxTestDriver(f, ps, resolver, dial)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "compare a < b first", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeDelivered {
		t.Fatal("reported delivered for text that cannot be attested losslessly")
	}
	if dialled {
		t.Error("dialled the inbox for a send that could never be attested")
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
	assertInboxExit(t, d, counterInboxFallbackBodyUnattestable, 1, 1)
}

// TestSend_InboxAttestedEnvelopeReachesTheWire proves the bytes that leave this
// driver are the envelope, not the bare text — the assertion the whole fix
// rests on. A driver that resolved a class, reported delivered, and still wrote
// an unwrapped body would pass every other test in this file and reproduce
// #148 exactly.
func TestSend_InboxAttestedEnvelopeReachesTheWire(t *testing.T) {
	rcv := newInboxReceiver(t)
	d, _ := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModePrompting), rcv.dialer(receiverRecordsEnvelope))

	if _, err := d.Send(context.Background(), testCaller, alphaRef, "hello", driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}

	if rcv.received() != 1 {
		t.Fatalf("the receiver was given %d messages, want 1", rcv.received())
	}
	want, ok := inboxclient.Attest("hello", inboxclient.ModePrompting, "")
	if !ok {
		t.Fatal("Attest refused a plain body")
	}
	if got := rcv.messages[0]; got != want {
		t.Errorf("the message on the wire is not the attested envelope:\n got %q\nwant %q", got, want)
	}
}

// colab-fleet #158: the sender label must reach the wire INSIDE the envelope —
// as the sender-name attribute, with a relayOfHuman declaration as the body's
// first line — and the send must still be attested and delivered.
func TestSend_InboxCarriesTheSenderLabel(t *testing.T) {
	rcv := newInboxReceiver(t)
	d, _ := newRoutedDriver(t, rcv, attestableResolver(inboxclient.ModeBypass), rcv.dialer(receiverRecordsEnvelope))

	from := &fleet.MessageFrom{Agent: "agent-a", Session: "s-158", Machine: "entrybox", RelayOfHuman: true}
	got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", driver.SendOptions{Submit: true, From: from})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeDelivered {
		t.Fatalf("outcome = %q (%s), want delivered — a label must never cost the inbox path", got.Outcome, got.Reason)
	}

	want, ok := inboxclient.Attest(driver.RelayDeclaration+"\nhello", inboxclient.ModeBypass, "agent-a · s-158 · entrybox")
	if !ok {
		t.Fatal("Attest refused a labelled plain body")
	}
	if rcv.received() != 1 || rcv.messages[0] != want {
		t.Errorf("the message on the wire is not the labelled envelope:\n got %q\nwant %q", rcv.messages, want)
	}
	if !strings.Contains(want, `from-name="agent-a · s-158 · entrybox" from-mode="bypass"`) {
		t.Errorf("envelope lacks the sender-name attribute before the mode attribute: %q", want)
	}
}

// colab-fleet #158: the terminal path has no envelope, so the same label goes
// on as the first line of the pasted text.
func TestSend_PaneFallbackCarriesTheSenderLabelAsFirstLine(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	// A successful submit empties the fake's composer, taking the pasted text
	// with it; a swallowed submit leaves it where this test can read it.
	f.swallowSubmit = true
	d := newInboxTestDriver(f, ps, nil, nil) // no inbox: the pane path

	from := &fleet.MessageFrom{Agent: "agent-a", Session: "s-158", Machine: "entrybox", RelayOfHuman: true}
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true, From: from})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("a labelled pane send was refused: %s", got.Reason)
	}
	want := "[from: agent-a · s-158 · entrybox]\n" + driver.RelayDeclaration + "\nhello"
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, pasted := range f.pasteLog {
		if strings.HasPrefix(pasted, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("no pane received the labelled text %q; pasted = %q", want, f.pasteLog)
	}
}

// Unlabelled pane sends are unchanged by #158.
func TestPaneLabelled_NilFromIsIdentity(t *testing.T) {
	if got := paneLabelled("hello", nil); got != "hello" {
		t.Errorf("paneLabelled(nil) = %q, want the text unchanged", got)
	}
	if got := paneLabelled("hello", &fleet.MessageFrom{Agent: "‍​"}); got != "hello" {
		t.Errorf("a label that normalises to nothing added a line: %q", got)
	}
}

// colab-fleet #158: the label is added AFTER the #53 guard has judged the
// caller's own text. Prefixing first would push a runtime-syntax line off the
// first line and past the guard — a label must never be a way around it.
func TestSend_SenderLabelDoesNotDefuseTheRuntimeSyntaxGuard(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	d := newInboxTestDriver(f, ps, nil, nil)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "!ls -la",
		driver.SendOptions{Submit: true, From: &fleet.MessageFrom{Agent: "agent-a", Machine: "entrybox"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Errorf("outcome = %q, want refused — the label must not carry runtime syntax past the guard", got.Outcome)
	}
}

// inboxExitCounters is every exit of sendViaInbox past the nil-resolver check
// (#150; #184 added no_transcript and unknown_partial_write). Each call that
// increments counterInboxAttempted must end in exactly one of these, which is
// what assertInboxExit checks. counterInboxWritten is a complete write and is
// itself the sum of counterInboxConfirmed and counterInboxUnconfirmed.
var inboxExitCounters = []string{
	counterInboxFallbackIdentityUnresolved,
	counterInboxErrorIdentityResolve,
	counterInboxFallbackResolverError,
	counterInboxFallbackResolverDeclined,
	counterInboxRefusedIdentityUnverified,
	counterInboxFallbackNoModeClass,
	counterInboxFallbackBodyUnattestable,
	counterInboxFallbackNoTranscript,
	counterInboxFallbackDialFailed,
	counterInboxFallbackWriteFailed,
	counterInboxUnknownPartialWrite,
	counterInboxWritten,
}

// assertInboxExit checks that one inbox attempt was counted, that it ended in
// exactly the exit want and no other, and what the two measurement counters
// #150's decision reads came to. It reads Counters(), the same map the health
// response serves.
func assertInboxExit(t *testing.T, d *Driver, want string, checked, lookalike int64) {
	t.Helper()
	got := d.Counters()
	if got[counterInboxAttempted] != 1 {
		t.Errorf("%s = %d, want 1", counterInboxAttempted, got[counterInboxAttempted])
	}
	for _, name := range inboxExitCounters {
		var n int64
		if name == want {
			n = 1
		}
		if got[name] != n {
			t.Errorf("%s = %d, want %d (expected exit: %s)", name, got[name], n, want)
		}
	}
	if got[counterInboxAttestChecked] != checked {
		t.Errorf("%s = %d, want %d", counterInboxAttestChecked, got[counterInboxAttestChecked], checked)
	}
	if got[counterInboxAttestBodyLookalike] != lookalike {
		t.Errorf("%s = %d, want %d", counterInboxAttestBodyLookalike, got[counterInboxAttestBodyLookalike], lookalike)
	}
}

// assertNoInboxCounters checks that a send which never tried the inbox left no
// inbox.* key at all — a zero would claim a measurement that was never taken.
func assertNoInboxCounters(t *testing.T, d *Driver) {
	t.Helper()
	for name, n := range d.Counters() {
		if strings.HasPrefix(name, "inbox.") {
			t.Errorf("%s = %d present, but this send never tried the inbox path", name, n)
		}
	}
}

// TestSend_InboxNoClassAndLookalikeBody_CountsBothFacts is the reason the body
// is counted apart from the class (#150): Attest refuses the missing class
// first, so a count keyed only on the refusal reason would never see this
// body's lookalike, and the rate #150 reads would come out clean while the
// index still omits classes.
func TestSend_InboxNoClassAndLookalikeBody_CountsBothFacts(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("this dial must never happen")
	}
	d := newInboxTestDriver(f, ps, resolver, dial)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "compare a < b first", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Errorf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
	assertInboxExit(t, d, counterInboxFallbackNoModeClass, 1, 1)
}

// TestSend_InboxRelayDeclaration_DoesNotCountAsLookalike proves the body the
// counter inspects is the body Attest is given — the #158 declaration line
// included — so a relayed plain message is not miscounted as unattestable.
func TestSend_InboxRelayDeclaration_DoesNotCountAsLookalike(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{
			Network: "unix", Socket: "/irrelevant", Token: "tok",
			ModeClass: inboxclient.ModeBypass,
		}, true, nil
	}
	rcv := newInboxReceiver(t)
	d := newInboxTestDriverWith(f, ps, resolver, rcv.dialer(receiverRecordsEnvelope), rcv.options()...)

	from := &fleet.MessageFrom{Agent: "agent-a", Session: "s-150", Machine: "entrybox", RelayOfHuman: true}
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true, From: from})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", got.Outcome)
	}
	assertInboxExit(t, d, counterInboxWritten, 1, 0)
}

// TestSendViaInbox_IdentityResolveError_Counted covers the one exit that
// returns an error rather than falling back: the multiplexer itself failing
// while identity is resolved. Called directly because Send's own earlier
// multiplexer use would fail first and never reach this branch.
func TestSendViaInbox_IdentityResolveError_Counted(t *testing.T) {
	f := twoSessions()
	f.failList = true
	ps := &fakePS{}
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		t.Error("the resolver was called although identity never resolved")
		return InboxAddress{}, false, nil
	}
	d := newInboxTestDriver(f, ps, resolver, nil)

	res, err := d.sendViaInbox(context.Background(), fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err == nil {
		t.Fatalf("sendViaInbox err = nil (%+v), want the multiplexer failure", res)
	}
	assertInboxExit(t, d, counterInboxErrorIdentityResolve, 0, 0)
}
