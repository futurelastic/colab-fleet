package tmux

import (
	"bufio"
	"context"
	"encoding/json"
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
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	// #148: the address now carries the target's permission-mode class. Without
	// it there is nothing to attest and this path is unavailable by design —
	// see TestSend_InboxWithoutModeClass_FallsBackToPane below.
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{
			Network: "unix", Socket: "/irrelevant", Token: "tok",
			ModeClass: inboxclient.ModeBypass,
		}, true, nil
	}
	d := newInboxTestDriver(f, ps, resolver, pipeDialer(t, readTwoLinesNoReply))

	start := time.Now()
	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= inboxRoundTripTimeout {
		t.Errorf("Send took %s, at or beyond the round-trip timeout — it waited on a response line #144 says never arrives", elapsed)
	}
	if got.Outcome != fleet.OutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered", got.Outcome)
	}
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && (c[0] == "paste-buffer" || c[0] == "load-buffer" || c[0] == "send-keys") {
			t.Fatalf("an inbox delivery must never also touch the pane, saw: %v", c)
		}
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
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	d := newInboxTestDriver(f, ps, resolver, dial)

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
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
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
	}
	d := newInboxTestDriver(f, ps, resolver, pipeDialer(t, closeBeforeReading))

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %q, want queued (fell through to the pane path)", got.Outcome)
	}
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
}

// TestSend_InboxAttestedEnvelopeReachesTheWire proves the bytes that leave this
// driver are the envelope, not the bare text — the assertion the whole fix
// rests on. A driver that resolved a class, reported delivered, and still wrote
// an unwrapped body would pass every other test in this file and reproduce
// #148 exactly.
func TestSend_InboxAttestedEnvelopeReachesTheWire(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{
			Network: "unix", Socket: "/irrelevant", Token: "tok",
			ModeClass: inboxclient.ModePrompting,
		}, true, nil
	}
	lines := make(chan string, 2)
	dial := pipeDialer(t, func(server net.Conn) {
		defer server.Close()
		reader := bufio.NewReader(server)
		for i := 0; i < 2; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			lines <- line
		}
	})
	d := newInboxTestDriver(f, ps, resolver, dial)

	if _, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}

	var message string
	for i := 0; i < 2; i++ {
		select {
		case line := <-lines:
			if strings.Contains(line, `"user"`) {
				message = line
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the request lines")
		}
	}
	if message == "" {
		t.Fatal("no message line reached the wire")
	}
	want, ok := inboxclient.Attest("hello", inboxclient.ModePrompting)
	if !ok {
		t.Fatal("Attest refused a plain body")
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, string(encoded[1:len(encoded)-1])) {
		t.Errorf("the message line does not carry the attested envelope:\n%s", message)
	}
}
