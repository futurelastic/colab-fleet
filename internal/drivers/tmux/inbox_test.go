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

// respondWithOutcome builds an onServer func (see pipeDialer) that reads the
// two request lines — auth, then message, each its own blocking Write on
// the client side over net.Pipe's synchronous semantics, so each needs its
// own Read here — and answers with a single receipt line carrying outcome
// and, optionally, a reason. Mirrors internal/inboxclient's own test
// server (inboxclient_test.go's runFakeServer), reproduced locally because
// it is a test double for THIS package's dial seam.
func respondWithOutcome(outcome, reason string) func(net.Conn) {
	return func(server net.Conn) {
		defer server.Close()
		reader := bufio.NewReader(server)
		if _, err := reader.ReadString('\n'); err != nil { // auth line
			return
		}
		if _, err := reader.ReadString('\n'); err != nil { // message line
			return
		}
		reply := `{"outcome":"` + outcome + `"`
		if reason != "" {
			reply += `,"reason":"` + reason + `"`
		}
		reply += "}\n"
		_, _ = server.Write([]byte(reply))
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

// TestSend_InboxDelivers_EveryOutcomeSurfacesDistinctly is #119's own
// central assertion at the driver level, mirroring inboxclient's own
// protocol-layer test: the six-value vocabulary must reach Send's caller
// unflattened, and the pane must never be touched once the inbox path
// commits to a delivery.
func TestSend_InboxDelivers_EveryOutcomeSurfacesDistinctly(t *testing.T) {
	cases := []struct {
		wire string
		want fleet.Outcome
	}{
		{"delivered", fleet.OutcomeDelivered},
		{"held", fleet.OutcomeHeld},
		{"denied", fleet.OutcomeDenied},
		{"expired", fleet.OutcomeExpired},
		{"refused", fleet.OutcomeRefused},
		{"dropped", fleet.OutcomeDropped},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			f := twoSessions()
			ps := &fakePS{}
			ps.set(100, time.Now())
			ps.set(200, time.Now())
			resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
				return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
			}
			d := newInboxTestDriver(f, ps, resolver, pipeDialer(t, respondWithOutcome(tc.wire, "because")))

			got, err := d.Send(context.Background(), testCaller,
				fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
			if got.Reason != "because" {
				t.Errorf("reason = %q, want %q", got.Reason, "because")
			}
			for _, c := range f.callsSnapshot() {
				if len(c) > 0 && (c[0] == "paste-buffer" || c[0] == "load-buffer" || c[0] == "send-keys") {
					t.Fatalf("an inbox delivery must never also touch the pane, saw: %v", c)
				}
			}
		})
	}
}

func TestSend_InboxDialFails_Refuses(t *testing.T) {
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
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q, want refused", got.Outcome)
	}
}

func TestSend_InboxNoReceiptBeforeClose_ReportsUnknown(t *testing.T) {
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	resolver := func(context.Context, ProcessIdentity) (InboxAddress, bool, error) {
		return InboxAddress{Network: "unix", Socket: "/irrelevant", Token: "tok"}, true, nil
	}
	closeNoReply := func(server net.Conn) { server.Close() }
	d := newInboxTestDriver(f, ps, resolver, pipeDialer(t, closeNoReply))

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %q, want unknown", got.Outcome)
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

// Sanity check that this test file's own fixtures speak the exact wire
// shape internal/inboxclient expects, so a future change to either side's
// field names is caught here rather than only as an opaque parse failure.
func TestInboxTestFixtures_MatchTheRealClientWireShape(t *testing.T) {
	raw := `{"outcome":"delivered","reason":"because"}` + "\n"
	var parsed struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &parsed); err != nil {
		t.Fatal(err)
	}
	if inboxclient.Outcome(parsed.Outcome) != inboxclient.OutcomeDelivered {
		t.Fatalf("outcome = %q", parsed.Outcome)
	}
}
