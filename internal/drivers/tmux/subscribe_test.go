package tmux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
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

	s, err := d.Subscribe(context.Background(), driver.SubscribeFilter{CwdPrefix: "/work/alpha"})
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

	s, err := d.Subscribe(context.Background(), driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// alpha was idle at subscribe time; make it working.
	f.captures["%1"] = fixtureWorking
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
	s, _ := d.Subscribe(context.Background(), driver.SubscribeFilter{})
	defer s.Close()

	f.captures["%1"] = fixtureWorking
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

	s, err := d.Subscribe(context.Background(), driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lifecycle := r.opened[0] // the first client opened is the lifecycle one

	f.sessions = append(f.sessions, fakeSession{
		name: "gamma", paneID: "%9", cwd: "/work/gamma", pid: 300, created: 1785600002,
	})
	f.captures["%9"] = idleFixtureFor("gamma")
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
	f.sessions = f.sessions[:len(f.sessions)-1]
	delete(f.captures, "%9")
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

	s, err := d.Subscribe(context.Background(), driver.SubscribeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lifecycle := r.opened[0]
	f.failList = true
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

	s, err := d.Subscribe(context.Background(), driver.SubscribeFilter{})
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

	_, err := d.Subscribe(context.Background(), driver.SubscribeFilter{})
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

	s, err := d.Subscribe(context.Background(), driver.SubscribeFilter{})
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
