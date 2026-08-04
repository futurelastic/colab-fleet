package service

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

func sessionEvent(id string, status fleet.Status) fleet.Event {
	return fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionState,
		Payload: fleet.SessionStatePayload{
			Ref:   fleet.SessionRef{Machine: "testbox", ID: id},
			State: fleet.InferredState(status, "test", nil),
		},
	}
}

// §7.3: the service assigns cursors, monotonically, and stamps its own epoch.
// A driver leaves both unset and must not be able to influence them.
func TestHubStampsCursorAndEpoch(t *testing.T) {
	h := newHub("testbox", "epoch-1")
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	h.publish(sessionEvent("a", fleet.StatusWorking))
	h.publish(sessionEvent("b", fleet.StatusIdle))

	first, second := <-sub.ch, <-sub.ch
	if first.Cursor != 1 || second.Cursor != 2 {
		t.Errorf("cursors = %d,%d; want 1,2 monotonic", first.Cursor, second.Cursor)
	}
	if first.Epoch != "epoch-1" || second.Epoch != "epoch-1" {
		t.Error("events must carry this instance's epoch")
	}
}

// §7.3: a subscriber whose epoch differs is told to resync. Its cursors refer
// to another instance's sequence and mean nothing here.
func TestChangedEpochForcesResync(t *testing.T) {
	h := newHub("testbox", "epoch-2")
	h.publish(sessionEvent("a", fleet.StatusIdle))
	_, backlog, needResync := h.add(ScopeFleet, driver.SubscribeFilter{}, 1, "epoch-1")
	if !needResync {
		t.Error("a cursor from another instance must not be honoured")
	}
	if len(backlog) != 0 {
		t.Error("no backlog may be replayed against a foreign cursor")
	}
}

// A cursor older than what is retained must announce a gap, never resume from
// the oldest event still held.
func TestExpiredCursorForcesResyncRatherThanSilentResume(t *testing.T) {
	h := newHub("testbox", "e")
	h.retention = 4
	for i := 0; i < 10; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
	}
	_, backlog, needResync := h.add(ScopeFleet, driver.SubscribeFilter{}, 1, "e")
	if !needResync {
		t.Fatal("cursor 1 fell off a 4-event window; that gap must be announced (§7.3)")
	}
	if len(backlog) != 0 {
		t.Error("a resync replaces the backlog; it does not accompany it")
	}
}

// A cursor still inside the window resumes exactly, with no gap and no resync.
func TestLiveCursorResumesWithoutResync(t *testing.T) {
	h := newHub("testbox", "e")
	for i := 0; i < 5; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
	}
	_, backlog, needResync := h.add(ScopeFleet, driver.SubscribeFilter{}, 3, "e")
	if needResync {
		t.Fatal("cursor 3 is retained; no resync is warranted")
	}
	if len(backlog) != 2 {
		t.Fatalf("backlog = %d events, want 2 (cursors 4 and 5)", len(backlog))
	}
	if backlog[0].Cursor != 4 {
		t.Errorf("backlog starts at %d, want 4 — resumption must not repeat what was seen", backlog[0].Cursor)
	}
}

// §7a asked what happens when a subscriber cannot keep up. It is marked, never
// silently skipped: a hole a subscriber cannot detect is the failure this
// whole design is organised against.
func TestSlowSubscriberIsMarkedNotSilentlySkipped(t *testing.T) {
	h := newHub("testbox", "e")
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	for i := 0; i < 200; i++ {
		h.publish(sessionEvent("a", fleet.StatusIdle))
	}
	h.mu.Lock()
	dropped := sub.dropped
	h.mu.Unlock()
	if !dropped {
		t.Error("a subscriber that overflowed must be marked so it can be resynced")
	}
}

// The union is what keeps watching proportional to demand (D4, one level up).
func TestUnionOfNamedFiltersStaysNarrow(t *testing.T) {
	h := newHub("testbox", "e")
	h.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"a", "b"}}, 0, "")
	h.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"b", "c"}}, 0, "")
	u := h.unionFilter()
	if len(u.Sessions) != 3 {
		t.Errorf("union = %v, want a+b+c deduplicated", u.Sessions)
	}
}

// One descriptive subscriber widens the union to everything, because a
// description cannot be served for less. Stated as a test so the cost is a
// known property rather than a surprise.
func TestOneDescriptiveSubscriberWidensTheUnion(t *testing.T) {
	h := newHub("testbox", "e")
	h.add(ScopeFleet, driver.SubscribeFilter{Sessions: []string{"a"}}, 0, "")
	h.add(ScopeFleet, driver.SubscribeFilter{CwdPrefix: "/work"}, 0, "")
	if u := h.unionFilter(); len(u.Sessions) != 0 {
		t.Errorf("union = %v; a descriptive subscriber must widen it to everything", u.Sessions)
	}
}

// Control events reach every subscriber regardless of filter — a subscriber
// that filtered away its own resync notice would be told nothing exactly when
// it most needs telling.
func TestControlEventsBypassFilters(t *testing.T) {
	f := driver.SubscribeFilter{Sessions: []string{"only-this"}}
	if !matchesEvent(f, fleet.Event{Kind: fleet.EventControlResync}) {
		t.Error("resync must not be filtered out")
	}
	if !matchesEvent(f, fleet.Event{Kind: fleet.EventSourceStatus}) {
		t.Error("source status must not be filtered out")
	}
	if matchesEvent(f, sessionEvent("other", fleet.StatusIdle)) {
		t.Error("a session event outside the filter should not be delivered")
	}
}

// End to end over HTTP: the framing question resolved in both directions.
func TestSSEStreamCarriesKindAsEventLineAndInPayload(t *testing.T) {
	svc := New("testbox")
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Publish after the subscriber is attached.
	deadline := time.Now().Add(2 * time.Second)
	for svc.events.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	svc.events.publish(sessionEvent("s1", fleet.StatusWorking))

	sc := bufio.NewScanner(resp.Body)
	var sawEventLine, sawKindInData, sawID bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			sawEventLine = line == "event: session.state"
		case strings.HasPrefix(line, "id: "):
			sawID = true
		case strings.HasPrefix(line, "data: "):
			sawKindInData = strings.Contains(line, `"kind":"session.state"`)
		}
		if sawEventLine && sawKindInData && sawID {
			return
		}
	}
	t.Errorf("stream did not carry kind both ways (event line=%v, data kind=%v, id=%v)",
		sawEventLine, sawKindInData, sawID)
}

// §13.1 in the event plane, from the serving side: a peer's scope=local
// subscription must not cause this service to open streams to its own peers,
// and must not receive peer events even if some other subscriber pulled them
// in. Without this, two mutually-configured machines hold streams to each
// other forever and relay the fleet back and forth.
func TestLocalScopeSubscriberNeitherPullsNorReceivesPeerEvents(t *testing.T) {
	h := newHub("testbox", "e")
	local, _, _ := h.add(ScopeLocal, driver.SubscribeFilter{}, 0, "")
	if h.wantsPeers() {
		t.Error("a local-scoped subscriber must not justify streaming from peers")
	}

	// Another subscriber does want the fleet.
	fleetSub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")
	if !h.wantsPeers() {
		t.Error("a fleet-scoped subscriber should enable peer streams")
	}

	peerEvent := sessionEvent("s1", fleet.StatusWorking)
	peerEvent.Machine = "peerbox"
	h.publish(peerEvent)

	select {
	case ev := <-local.ch:
		t.Errorf("local-scoped subscriber received a peer event: %+v", ev)
	default:
	}
	select {
	case <-fleetSub.ch:
	default:
		t.Error("fleet-scoped subscriber should have received it")
	}
}

// A relayed event keeps its origin through the hub, and gains this service's
// cursor — the approved split.
func TestHubStampsRelayedEventsWithoutLosingOrigin(t *testing.T) {
	h := newHub("testbox", "local-epoch")
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	relayed := sessionEvent("s1", fleet.StatusIdle)
	relayed.Machine = "peerbox"
	relayed.Origin = &fleet.EventOrigin{Cursor: 77, Epoch: "peer-epoch"}
	h.publish(relayed)

	ev := <-sub.ch
	if ev.Cursor != 1 || ev.Epoch != "local-epoch" {
		t.Errorf("cursor/epoch = %d/%q; ordering against everything else the caller "+
			"receives is this service's to assign", ev.Cursor, ev.Epoch)
	}
	if ev.Origin == nil || ev.Origin.Cursor != 77 || ev.Origin.Epoch != "peer-epoch" {
		t.Errorf("origin = %+v; the peer's own coordinates must survive", ev.Origin)
	}
	if ev.Machine != "peerbox" {
		t.Errorf("machine = %q, want the originating machine", ev.Machine)
	}
}

// The wire form must carry everything the in-memory event does. A field added
// to fleet.Event and forgotten in the envelope is invisible to every unit test
// that never crosses the encoder.
func TestSSEEnvelopeCarriesOrigin(t *testing.T) {
	svc := New("testbox")
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for svc.events.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	relayed := sessionEvent("s1", fleet.StatusIdle)
	relayed.Machine = "peerbox"
	relayed.Origin = &fleet.EventOrigin{Cursor: 99, Epoch: "peer-epoch"}
	svc.events.publish(relayed)

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			if !strings.Contains(line, `"origin"`) || !strings.Contains(line, `"cursor":99`) {
				t.Errorf("origin lost on the wire: %s", line)
			}
			return
		}
	}
	t.Error("no data frame received")
}
