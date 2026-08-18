package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"strings"
)

// fakeCtl is a control-mode client that never spawns anything. Tests push
// notifications into it to simulate the multiplexer.
type fakeCtl struct {
	session string
	notes   chan ctlNote
	closed  bool
	mu      sync.Mutex
}

func (c *fakeCtl) Notes() <-chan ctlNote { return c.notes }
func (c *fakeCtl) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.notes)
	}
	return nil
}
func (c *fakeCtl) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// ctlRegistry records every client the driver opened, so tests can assert
// on how many were opened and that all were reaped.
type ctlRegistry struct {
	mu      sync.Mutex
	opened  []*fakeCtl
	failFor map[string]bool
}

func (r *ctlRegistry) dial(ctx context.Context, bin, session string) (ctlConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failFor[session] {
		return nil, errors.New("cannot attach")
	}
	c := &fakeCtl{session: session, notes: make(chan ctlNote, 16)}
	r.opened = append(r.opened, c)
	return c, nil
}

func (r *ctlRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opened)
}

// contentFor returns the CONTENT client for a session.
//
// Index 0 is always the lifecycle client — Subscribe dials it first, and it
// may well be attached to the same session a content client is. It filters
// for lifecycle notifications only, so firing %output at it is correctly a
// no-op. Skipping it here is not a workaround; a helper that returned the
// first name match would silently test nothing.
func (r *ctlRegistry) contentFor(name string) *fakeCtl {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.opened {
		if i == 0 {
			continue
		}
		if c.session == name {
			return c
		}
	}
	return nil
}

func (r *ctlRegistry) allClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.opened {
		if !c.isClosed() {
			return false
		}
	}
	return true
}

func newSubDriver(f *fakeMux, r *ctlRegistry) *Driver {
	return New("testbox",
		withExec(f.exec),
		withCtlDialer(r.dial),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
	)
}

// fire pushes a notification that the driver should treat as a change
// trigger.
func fire(c *fakeCtl, name string) {
	if c == nil {
		return
	}
	select {
	case c.notes <- ctlNote{Name: name}:
	default:
	}
}

func nextWithin(t *testing.T, s driver.EventStream, d time.Duration) (fleet.Event, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		return fleet.Event{}, false
	}
	return ev, true
}

// The cost property that justified the design: O(subscribers), not
// O(sessions). One lifecycle client plus one content client per MATCHING
// session — not one per session on the machine.
func TestSubscribeOpensOneClientPerMatchingSessionPlusLifecycle(t *testing.T) {
	f := twoSessions()
	// Add sessions that do NOT match the filter.
	for i := 0; i < 8; i++ {
		id := "%" + intToStr(200+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "other" + intToStr(i), paneID: id, cwd: "/elsewhere", pid: 900 + i, created: 1785600009,
		})
		f.captures[id] = idleFixtureFor("other")
	}
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{CwdPrefix: "/work/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 1 lifecycle + 1 matching content client. Ten sessions exist.
	if got := r.count(); got != 2 {
		t.Errorf("opened %d control clients for 10 sessions with 1 matching; "+
			"want 2 (lifecycle + matching). Cost must be O(subscribers), not O(sessions)", got)
	}
}

// A content notification triggers a re-read, and a real status change is
// emitted as an event.
func TestSubscribeEmitsStateChangeOnTrigger(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// alpha was idle at subscribe time; make it working.
	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event after a content notification")
	}
	if ev.Kind != fleet.EventSessionState {
		t.Fatalf("want session.state, got %q", ev.Kind)
	}
	p, ok := ev.Payload.(fleet.SessionStatePayload)
	if !ok {
		t.Fatalf("payload type %T, want SessionStatePayload", ev.Payload)
	}
	if p.Ref.ID != "alpha💬" || p.State.Status != fleet.StatusWorking {
		t.Errorf("got %q -> %q, want alpha💬 -> working", p.Ref.ID, p.State.Status)
	}
}

// §7.3's fields are the service's to assign; a driver must not invent them.
func TestSubscribeLeavesCursorAndEpochToTheService(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	s, _ := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	defer s.Close()

	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event")
	}
	if ev.Cursor != 0 || ev.Epoch != "" {
		t.Errorf("driver stamped cursor=%d epoch=%q; §7.3 assigns these per "+
			"service instance and a driver has neither", ev.Cursor, ev.Epoch)
	}
	if ev.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", ev.Machine)
	}
}

// A session appearing and disappearing must produce created/closed, driven
// by the GLOBAL lifecycle notifications rather than by any per-session
// client.
func TestSubscribeEmitsCreatedAndClosed(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lifecycle := r.opened[0] // the first client opened is the lifecycle one

	f.addSession(fakeSession{
		name: "gamma", paneID: "%9", cwd: "/work/gamma", pid: 300, created: 1785600002,
	}, idleFixtureFor("gamma"))
	fire(lifecycle, "sessions-changed")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event after a lifecycle notification")
	}
	if ev.Kind != fleet.EventSessionCreated {
		t.Fatalf("want session.created, got %q", ev.Kind)
	}
	if sess, ok := ev.Payload.(fleet.Session); !ok || sess.ID != "gamma" {
		t.Errorf("payload = %+v, want session gamma", ev.Payload)
	}

	// Now remove it.
	f.dropLastSession()
	fire(lifecycle, "sessions-changed")

	ev, ok = nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event after removal")
	}
	if ev.Kind != fleet.EventSessionClosed {
		t.Fatalf("want session.closed, got %q", ev.Kind)
	}
}

// §5.7 on the event stream. A failed read must not be diffed as though it
// had succeeded, or every unreadable session emits a spurious "closed" and
// subscribers conclude the fleet died.
func TestSubscribeDoesNotReportClosedWhenTheReadFailed(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lifecycle := r.opened[0]
	f.setFailList(true)
	fire(lifecycle, "sessions-changed")

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("a failed read should still say something, not go silent")
	}
	if ev.Kind == fleet.EventSessionClosed {
		t.Fatal("a failed read was diffed as sessions disappearing (§5.7)")
	}
	if ev.Kind != fleet.EventSourceStatus {
		t.Fatalf("want source.status, got %q", ev.Kind)
	}
	src, ok := ev.Payload.(fleet.SourceStatus)
	if !ok || src.Status == fleet.SourceOK {
		t.Errorf("payload = %+v, want a non-ok SourceStatus", ev.Payload)
	}
}

// Every client a subscription opens must be reaped, or each subscribe call
// leaks an attached process — the exact failure the demand-driven design
// exists to avoid.
func TestCloseReapsEveryControlClient(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if r.count() != 3 { // lifecycle + 2 sessions
		t.Fatalf("expected 3 clients, got %d", r.count())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.allClosed() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !r.allClosed() {
		t.Error("Close left control clients attached")
	}
	// Idempotent: a double close must not panic on an already-closed channel.
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned %v", err)
	}
}

// The documented edge: control mode has no unattached form, so with zero
// sessions there is nothing to attach a lifecycle client to. That must be an
// honest refusal, not a silent stream that never delivers.
func TestSubscribeRefusesWhenThereIsNothingToAttachTo(t *testing.T) {
	f := &fakeMux{captures: map[string]string{}}
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	_, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err == nil {
		t.Fatal("want an error when there is no session to attach to")
	}
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("want ErrUnsupported wrapped, got %v", err)
	}
}

func TestParseNoteRejectsNonNotifications(t *testing.T) {
	cases := []struct {
		line string
		want bool
		name string
	}{
		{"%output %169 hello", true, "output"},
		{"%sessions-changed", true, "sessions-changed"},
		{"%unlinked-window-add @177", true, "unlinked-window-add"},
		{"not a notification", false, ""},
		{"", false, ""},
		{"%", false, ""},
	}
	for _, tc := range cases {
		got, ok := parseNote(tc.line)
		if ok != tc.want {
			t.Errorf("parseNote(%q) ok = %v, want %v", tc.line, ok, tc.want)
			continue
		}
		if ok && got.Name != tc.name {
			t.Errorf("parseNote(%q) name = %q, want %q", tc.line, got.Name, tc.name)
		}
	}
}

// The scoping asymmetry measured against the real substrate, pinned as a
// property of the classification helpers so a future edit cannot quietly
// reclassify a global notification as a per-attachment one.
func TestNotificationClassification(t *testing.T) {
	global := []string{"sessions-changed", "unlinked-window-add", "unlinked-window-close"}
	for _, n := range global {
		if !isLifecycleNote(n) {
			t.Errorf("%q is delivered fleet-wide and must drive lifecycle", n)
		}
	}
	perAttachment := []string{"output", "subscription-changed"}
	for _, n := range perAttachment {
		if !isContentNote(n) {
			t.Errorf("%q is a content notification", n)
		}
		if isLifecycleNote(n) {
			t.Errorf("%q is scoped to the attached session; it cannot be trusted "+
				"as a fleet-wide lifecycle signal", n)
		}
	}
}

// The lifecycle client is hosted by a session the driver neither owns nor
// created. When that session exits, the client dies with it — and the stream
// must not quietly stop delivering. It must say it is degraded, and
// re-attach.
func TestLifecycleClientSurvivesItsHostSessionDying(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before := r.count()
	// Kill the host: closing the client closes its notification channel,
	// which is exactly what happens when the session it is attached to
	// exits.
	_ = r.opened[0].Close()

	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("stream went silent when the lifecycle client died; a deaf stream " +
			"is indistinguishable from a quiet fleet")
	}
	if ev.Kind != fleet.EventSourceStatus {
		t.Fatalf("want source.status, got %q", ev.Kind)
	}
	src, ok := ev.Payload.(fleet.SourceStatus)
	if !ok || src.Status != fleet.SourceDegraded {
		t.Errorf("payload = %+v, want a degraded SourceStatus", ev.Payload)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.count() > before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("no re-attach: still %d clients", r.count())
}

// D4's whole point: naming a session costs one connection, describing a
// directory costs one per match. A caller that knows what it wants should not
// pay for what it does not.
func TestNamingSessionsCostsOneClientEach(t *testing.T) {
	f := twoSessions()
	for i := 0; i < 38; i++ {
		id := "%" + intToStr(300+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "bulk" + intToStr(i), paneID: id, cwd: "/work/alpha", pid: 700 + i, created: 1785600020,
		})
		f.captures[id] = idleFixtureFor("bulk")
	}
	// Every one of those 40 sessions shares /work/alpha, so a prefix filter
	// would attach to all of them.
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	s, err := d.Subscribe(context.Background(), testCaller,
		driver.SubscribeFilter{Sessions: []string{"alpha💬", "bulk7"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if got := r.count(); got != 3 {
		t.Errorf("opened %d clients for 2 named sessions out of 40; want 3 "+
			"(lifecycle + one per named session). Filter granularity IS the cost", got)
	}
}

// Events for sessions outside the filter must not be delivered, even though
// lifecycle notifications arrive fleet-wide.
func TestNamedSubscriptionIgnoresOtherSessions(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)
	s, err := d.Subscribe(context.Background(), testCaller,
		driver.SubscribeFilter{Sessions: []string{"alpha💬"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lifecycle := r.opened[0]

	// A session appears that the caller did not ask about.
	f.addSession(fakeSession{
		name: "unrelated", paneID: "%77", cwd: "/elsewhere", pid: 400, created: 1785600030,
	}, idleFixtureFor("unrelated"))
	fire(lifecycle, "sessions-changed")

	if ev, ok := nextWithin(t, s, 1500*time.Millisecond); ok {
		t.Errorf("delivered %q for a session outside the filter: %+v", ev.Kind, ev.Payload)
	}

	// ...and one it did ask about still comes through.
	f.setCapture("%1", fixtureWorking)
	fire(lifecycle, "sessions-changed")
	ev, ok := nextWithin(t, s, 3*time.Second)
	if !ok {
		t.Fatal("no event for the named session")
	}
	if ev.Kind != fleet.EventSessionState {
		t.Errorf("kind = %q, want session.state", ev.Kind)
	}
}

// Both selectors narrow, and they compose with AND — the same rule ListFilter
// follows, so callers do not have to remember two conventions.
func TestFilterSelectorsCompose(t *testing.T) {
	cases := []struct {
		name   string
		filter driver.SubscribeFilter
		id     string
		cwd    string
		want   bool
	}{
		{"zero value matches everything", driver.SubscribeFilter{}, "a", "/x", true},
		{"id only", driver.SubscribeFilter{Sessions: []string{"a"}}, "a", "/x", true},
		{"id only, miss", driver.SubscribeFilter{Sessions: []string{"a"}}, "b", "/x", false},
		{"prefix only", driver.SubscribeFilter{CwdPrefix: "/x"}, "a", "/x/y", true},
		{"both, both satisfied", driver.SubscribeFilter{Sessions: []string{"a"}, CwdPrefix: "/x"}, "a", "/x/y", true},
		{"both, prefix fails", driver.SubscribeFilter{Sessions: []string{"a"}, CwdPrefix: "/z"}, "a", "/x/y", false},
		{"both, id fails", driver.SubscribeFilter{Sessions: []string{"a"}, CwdPrefix: "/x"}, "b", "/x/y", false},
	}
	for _, tc := range cases {
		if got := tc.filter.Matches(tc.id, tc.cwd); got != tc.want {
			t.Errorf("%s: Matches(%q,%q) = %v, want %v", tc.name, tc.id, tc.cwd, got, tc.want)
		}
	}
}

// A subscription's cost is paid by a multiplexer server that other tools
// share. One forgotten subscriber once held 62 clients on a 69-session host,
// exhausted the server's descriptors, and made every new attach fail while
// every session was alive — so this bound is a safety property, not a tuning
// knob.
func TestSubscribeCapsContentClients(t *testing.T) {
	f := twoSessions()
	for i := 0; i < 60; i++ {
		id := "%" + intToStr(500+i)
		f.sessions = append(f.sessions, fakeSession{
			name: "big" + intToStr(i), paneID: id, cwd: "/w", pid: 5000 + i, created: 1785600002,
		})
		f.captures[id] = idleFixtureFor("big" + intToStr(i))
	}
	d := newTestDriver(f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := d.Subscribe(ctx, testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stream.Close()

	es, ok := stream.(*eventStream)
	if !ok {
		t.Fatalf("unexpected stream type %T", stream)
	}
	es.mu.Lock()
	n := len(es.conns)
	es.mu.Unlock()

	// conns also holds the single lifecycle client, which is per subscription
	// rather than per session and is not what the cap governs.
	if n > maxContentClients+1 {
		t.Errorf("opened %d clients for %d sessions; cap is %d(+1 lifecycle) — an unbounded "+
			"subscription exhausts a server other tools depend on", n, len(f.sessions), maxContentClients)
	}
	if n == 0 {
		t.Error("capped to nothing; a subscription with no content clients loses its change triggers")
	}
}

// The account blocking is one fact about the machine, and a supervisor should
// be told once. Before this, 48 sessions each discovered it by being dispatched
// work and stalling — every discovery costing a session that was already sent.
func TestSubscribeAnnouncesAndRetractsAnAccountBlock(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const rule = "────────────────────"
	f.setCapture("%1", "transcript\n  ⎿  You've hit your weekly limit · resets Aug 10 at 12am (Asia/Tokyo)\n"+
		rule+"\n❯ \n"+rule+"\n")
	fire(r.contentFor("alpha💬"), "output")

	p, ok := awaitQuota(t, s)
	if !ok {
		t.Fatal("the account started refusing work and the stream never said so")
	}
	if !p.Blocked || p.Quota == nil || !strings.Contains(p.Quota.ResetHint, "aug 10") {
		t.Errorf("payload = %+v, want blocked with a reset hint", p)
	}
	if p.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", p.Machine)
	}

	// Recovery is a positive statement, not an absence to infer.
	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	p, ok = awaitQuota(t, s)
	if !ok {
		t.Fatal("the account recovered and the stream never retracted the block")
	}
	if p.Blocked || p.Quota != nil {
		t.Errorf("payload = %+v, want an explicit unblocked", p)
	}
}

// A subscriber connecting mid-outage must not wait for a transition that has
// already happened.
func TestSubscribeAnnouncesABlockAlreadyInForce(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}
	d := newSubDriver(f, r)

	const rule = "────────────────────"
	f.setCapture("%1", "transcript\n  ⎿  You've hit your weekly limit · resets Aug 10\n"+rule+"\n❯ \n"+rule+"\n")
	if _, err := d.List(context.Background(), testCaller, driver.ListFilter{}); err != nil {
		t.Fatal(err)
	}

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fire(r.contentFor("alpha💬"), "output")

	p, ok := awaitQuota(t, s)
	if !ok || !p.Blocked {
		t.Fatalf("a subscriber joining during an outage was told nothing (got %+v, %v)", p, ok)
	}
}

func awaitQuota(t *testing.T, s driver.EventStream) (fleet.MachineQuotaPayload, bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		ev, ok := nextWithin(t, s, time.Until(deadline))
		if !ok {
			return fleet.MachineQuotaPayload{}, false
		}
		if ev.Kind == fleet.EventMachineQuota {
			p, ok := ev.Payload.(fleet.MachineQuotaPayload)
			if !ok {
				t.Fatalf("payload type %T, want MachineQuotaPayload", ev.Payload)
			}
			return p, true
		}
	}
	return fleet.MachineQuotaPayload{}, false
}

// #12: the machine-level event fires once at an actual credential
// transition — not on the seed read that merely establishes a baseline,
// not a second time for a later trigger that changed nothing further, and
// not once per session (this fixture carries two).
func TestSubscribeAnnouncesACredentialTransitionOnlyOnce(t *testing.T) {
	f := twoSessions()
	r := &ctlRegistry{}

	credPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := time.Unix(1785600500, 0)
	if err := os.Chtimes(credPath, baseline, baseline); err != nil {
		t.Fatal(err)
	}

	d := New("testbox",
		withExec(f.exec),
		withCtlDialer(r.dial),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithCredentialPath(credPath),
	)

	s, err := d.Subscribe(context.Background(), testCaller, driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A trigger with nothing changed about the credential store: the seed
	// read already saw this generation, so this must not read as a
	// transition.
	f.setCapture("%2", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")
	if _, ok := awaitAccount(t, s, 500*time.Millisecond); ok {
		t.Fatal("machine.account fired with no credential change — the seed read must not count as a transition")
	}

	// The actual transition.
	rotated := time.Unix(1785700000, 0)
	if err := os.Chtimes(credPath, rotated, rotated); err != nil {
		t.Fatal(err)
	}
	f.setCapture("%1", fixtureWorking)
	fire(r.contentFor("alpha💬"), "output")

	p, ok := awaitAccount(t, s, 4*time.Second)
	if !ok {
		t.Fatal("the credential material changed and the stream never said so")
	}
	if p.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", p.Machine)
	}
	if !p.Generation.Equal(rotated) {
		t.Errorf("Generation = %v, want %v", p.Generation, rotated)
	}

	// A second trigger with nothing further changed must not re-announce —
	// once per transition, not once per read that happens to notice it.
	f.setCapture("%2", fixtureUnsent)
	fire(r.contentFor("beta"), "output")
	if _, ok := awaitAccount(t, s, 500*time.Millisecond); ok {
		t.Fatal("machine.account fired a second time with no further change — a transition is reported once")
	}
}

func awaitAccount(t *testing.T, s driver.EventStream, d time.Duration) (fleet.MachineAccountPayload, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ev, ok := nextWithin(t, s, time.Until(deadline))
		if !ok {
			return fleet.MachineAccountPayload{}, false
		}
		if ev.Kind == fleet.EventMachineAccount {
			p, ok := ev.Payload.(fleet.MachineAccountPayload)
			if !ok {
				t.Fatalf("payload type %T, want MachineAccountPayload", ev.Payload)
			}
			return p, true
		}
	}
	return fleet.MachineAccountPayload{}, false
}
