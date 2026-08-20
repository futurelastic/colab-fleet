package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/drivers/stub"
)

// feedDriver is a driver whose Subscribe can be made to fail a chosen number
// of times before working, so the pump's retry and its announcements can be
// observed rather than reasoned about.
type feedDriver struct {
	stub.Driver

	mu        sync.Mutex
	attempts  int
	failFirst int
	failWith  error

	events chan fleet.Event
}

func newFeedDriver(failFirst int, failWith error) *feedDriver {
	return &feedDriver{
		Driver:    stub.Driver{DeadlineMs: 200},
		failFirst: failFirst,
		failWith:  failWith,
		events:    make(chan fleet.Event, 8),
	}
}

func (d *feedDriver) Attempts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

func (d *feedDriver) Subscribe(ctx context.Context, req fleet.Request, f driver.SubscribeFilter) (driver.EventStream, error) {
	d.mu.Lock()
	d.attempts++
	n := d.attempts
	d.mu.Unlock()
	if n <= d.failFirst {
		return nil, d.failWith
	}
	return &feedStream{events: d.events}, nil
}

type feedStream struct{ events chan fleet.Event }

func (s *feedStream) Next(ctx context.Context) (fleet.Event, error) {
	select {
	case <-ctx.Done():
		return fleet.Event{}, ctx.Err()
	case ev := <-s.events:
		return ev, nil
	}
}

func (s *feedStream) Close() error { return nil }

// newFeedServer is newTestServer's sibling for the feed tests: a driver that
// can actually stream, so the fixture does not spend every batch announcing
// that it cannot.
func newFeedServer(t *testing.T) (*Service, *httptest.Server, *feedDriver) {
	t.Helper()
	svc := New("test-machine")
	d := newFeedDriver(0, nil)
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	return svc, httptestServer(t, svc), d
}

func watchOnce(t *testing.T, srv string, q url.Values) watchResponse {
	t.Helper()
	req := authedRequest(t, http.MethodGet, srv+"/v1/sessions/watch?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d, body %s", resp.StatusCode, body)
	}
	var out watchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode watch response: %v (body %s)", err, body)
	}
	return out
}

func waitForSubscriber(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for svc.events.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.events.subscriberCount() == 0 {
		t.Fatal("no subscriber attached in time")
	}
}

// The long poll returns the events it has and, with them, the cursor to send
// next — which is the last event's, not the service's current one.
func TestWatchReturnsABatchAndTheCursorToResumeFrom(t *testing.T) {
	svc, srv, _ := newFeedServer(t)

	go func() {
		waitForSubscriber(t, svc)
		svc.events.publish(sessionEvent("s1", fleet.StatusWorking))
		svc.events.publish(sessionEvent("s2", fleet.StatusIdle))
	}()

	got := watchOnce(t, srv.URL, url.Values{"wait": {"3000"}})
	if len(got.Events) == 0 {
		t.Fatal("a poll that saw an event must not return empty")
	}
	last := got.Events[len(got.Events)-1]
	if got.Cursor != last.Cursor {
		t.Errorf("cursor = %d; want the last event's cursor %d, so nothing between is skipped",
			got.Cursor, last.Cursor)
	}
	if got.Epoch != svc.Epoch() {
		t.Errorf("epoch = %q, want this instance's %q", got.Epoch, svc.Epoch())
	}
	if last.Kind != fleet.EventSessionState {
		t.Errorf("kind = %q; the long poll carries the same vocabulary as the stream", last.Kind)
	}
}

// An idle fleet answers with an empty batch and hands the caller's own cursor
// straight back. Returning the service's current cursor instead would advance
// this caller past events it never saw, because the sequence advances for
// everybody while a filter selects for one.
func TestWatchEmptyBatchReturnsTheCallersOwnCursor(t *testing.T) {
	svc, srv, _ := newFeedServer(t)
	svc.events.publish(sessionEvent("other", fleet.StatusIdle))
	svc.events.publish(sessionEvent("other", fleet.StatusWorking))

	got := watchOnce(t, srv.URL, url.Values{
		"since":   {"1"},
		"session": {"nobody"},
		"wait":    {"60"},
	})
	if len(got.Events) != 0 {
		t.Fatalf("nothing matched that filter; got %d events", len(got.Events))
	}
	if got.Cursor != 1 {
		t.Errorf("cursor = %d, want the caller's own 1 back untouched", got.Cursor)
	}
}

// A cursor that fell out of the retained window is a domain answer, not a
// fault: 200, with control.resync in the batch. A 4xx would train the client to
// retry the one thing it must instead re-list after.
func TestWatchStaleCursorIsAnInBandResyncNotAnHTTPError(t *testing.T) {
	svc, srv := newTestServer(t)
	svc.events.retention = 4
	for i := 0; i < 10; i++ {
		svc.events.publish(sessionEvent("s1", fleet.StatusIdle))
	}

	got := watchOnce(t, srv.URL, url.Values{"since": {"1"}, "epoch": {svc.Epoch()}, "wait": {"0"}})
	if len(got.Events) != 1 || got.Events[0].Kind != fleet.EventControlResync {
		t.Fatalf("want exactly one control.resync, got %+v", got.Events)
	}
	if got.Cursor != 1 {
		t.Errorf("cursor = %d; a resync is not a position to resume from, so the "+
			"caller's own value comes back and the re-listing supplies the next one", got.Cursor)
	}
	payload, _ := json.Marshal(got.Events[0].Payload)
	if want := `{"reason":"cursor_expired"}`; string(payload) != want {
		t.Errorf("payload = %s, want %s", payload, want)
	}
}

// Another instance's cursors mean nothing here, and saying "cursor_expired"
// would send the caller looking for a slowness problem it does not have.
func TestWatchForeignEpochResyncsAsEpochChanged(t *testing.T) {
	_, srv := newTestServer(t)

	got := watchOnce(t, srv.URL, url.Values{"since": {"5"}, "epoch": {"some-other-instance"}, "wait": {"0"}})
	if len(got.Events) != 1 {
		t.Fatalf("want a resync, got %+v", got.Events)
	}
	payload, _ := json.Marshal(got.Events[0].Payload)
	if want := `{"reason":"epoch_changed"}`; string(payload) != want {
		t.Errorf("payload = %s, want %s", payload, want)
	}
}

// A caller ahead of the sequence is announced, not left to block forever on a
// cursor this service will never reach in that epoch.
func TestWatchCursorAheadOfTheSequenceIsAnnouncedNotAwaited(t *testing.T) {
	svc, srv := newTestServer(t)
	svc.events.publish(sessionEvent("s1", fleet.StatusIdle))

	done := make(chan watchResponse, 1)
	go func() {
		done <- watchOnce(t, srv.URL, url.Values{
			"since": {"999"}, "epoch": {svc.Epoch()}, "wait": {"5000"},
		})
	}()
	select {
	case got := <-done:
		if len(got.Events) != 1 || got.Events[0].Kind != fleet.EventControlResync {
			t.Fatalf("want a resync, got %+v", got.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cursor this service never stamped must be answered, not waited on")
	}
}

// Both transports carry one envelope. A field that reaches the stream and not
// the poll is a second event model growing quietly.
func TestWatchAndStreamCarryTheSameEnvelopeFields(t *testing.T) {
	svc, srv, _ := newFeedServer(t)

	go func() {
		waitForSubscriber(t, svc)
		relayed := sessionEvent("s1", fleet.StatusIdle)
		relayed.Machine = "peerbox"
		relayed.Origin = &fleet.EventOrigin{Cursor: 99, Epoch: "peer-epoch"}
		svc.events.publish(relayed)
	}()

	got := watchOnce(t, srv.URL, url.Values{"wait": {"3000"}})
	if len(got.Events) != 1 {
		t.Fatalf("want one event, got %d", len(got.Events))
	}
	ev := got.Events[0]
	if ev.Machine != "peerbox" || ev.Origin == nil || ev.Origin.Cursor != 99 {
		t.Errorf("origin lost on the poll transport: %+v", ev)
	}
}

// The snapshot's own position is what makes list-then-watch safe. Withheld
// while nothing is being observed, because the cursor is frozen then and a
// client watching from it would skip everything before the first subscription.
func TestListingWithholdsAFeedPositionWhileNothingIsWatched(t *testing.T) {
	_, srv := newTestServer(t)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Feed *fleet.FeedPosition `json:"feed"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Feed != nil {
		t.Errorf("feed = %+v; an unwatched service's cursor is not a resume point", *envelope.Feed)
	}
}

func TestListingCarriesAFeedPositionWhileTheStreamIsLive(t *testing.T) {
	svc := New("testbox")
	d := newFeedDriver(0, nil)
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	srv := httptestServer(t, svc)

	_, _, _, cancel := svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
	defer cancel()
	waitForStream(t, svc, true)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Feed *fleet.FeedPosition `json:"feed"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Feed == nil {
		t.Fatal("a watched service must publish where its snapshot sits")
	}
	if envelope.Feed.Epoch != svc.Epoch() {
		t.Errorf("epoch = %q, want %q", envelope.Feed.Epoch, svc.Epoch())
	}
}

// The teardown-on-last-departure that a long poll triggers once per cycle.
// Each teardown makes the next subscription re-seed a baseline, which absorbs
// everything that changed in the gap and reports none of it.
func TestStreamLingersPastTheLastSubscriberThenStops(t *testing.T) {
	svc := New("testbox")
	svc.events.linger = 80 * time.Millisecond
	d := newFeedDriver(0, nil)
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}

	_, _, _, cancel := svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
	waitForAttempts(t, d, 1)
	cancel()

	if !svc.events.streamLive() {
		t.Fatal("the stream was torn down the instant the last subscriber left")
	}
	waitForStream(t, svc, false)

	if got := d.Attempts(); got != 1 {
		t.Errorf("driver was subscribed %d times; one continuous subscription was expected", got)
	}
}

// A subscriber arriving inside the window keeps ONE subscription, which is the
// whole point: consecutive polls must not each re-seed a baseline.
func TestASubscriberInsideTheLingerWindowKeepsOneSubscription(t *testing.T) {
	svc := New("testbox")
	svc.events.linger = 2 * time.Second
	d := newFeedDriver(0, nil)
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}

	for i := 0; i < 3; i++ {
		_, _, _, cancel := svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
		waitForAttempts(t, d, 1)
		cancel()
	}
	if got := d.Attempts(); got != 1 {
		t.Errorf("driver was subscribed %d times across 3 poll cycles; want 1", got)
	}
}

// The measured hole: a machine that is momentarily empty refuses a
// subscription, the pump used to return for good, and the subscriber then held
// an open, healthy-looking, permanently empty stream.
func TestPumpRetriesATransientRefusalAndAnnouncesBothEdges(t *testing.T) {
	svc := New("testbox")
	d := newFeedDriver(1, fmt.Errorf("nothing to attach to yet: %w", driver.ErrNotReady))
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}

	ch, _, _, cancel := svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
	defer cancel()

	var kinds []fleet.EventKind
	var sawDegraded, sawRestored, sawResync bool
	deadline := time.After(5 * time.Second)
	for !(sawDegraded && sawRestored && sawResync) {
		select {
		case ev := <-ch:
			kinds = append(kinds, ev.Kind)
			switch p := ev.Payload.(type) {
			case fleet.SourceStatus:
				if p.Status == fleet.SourceDegraded {
					sawDegraded = true
				}
				if p.Status == fleet.SourceOK && sawDegraded {
					sawRestored = true
				}
			case fleet.ControlResyncPayload:
				if p.Reason == fleet.ResyncFeedGap {
					sawResync = true
				}
			}
		case <-deadline:
			t.Fatalf("degraded=%v restored=%v resync=%v after %v; attempts=%d",
				sawDegraded, sawRestored, sawResync, kinds, d.Attempts())
		}
	}
	if d.Attempts() < 2 {
		t.Errorf("attempts = %d; a transient refusal must be retried", d.Attempts())
	}
}

// The one error that must NOT be retried — and must still be said out loud.
// "This machine reports no events" and "this machine has nothing to report"
// are different facts, and a subscriber that cannot tell them apart waits
// forever on the wrong one.
func TestPumpAnnouncesAnUnsupportedSubstrateOnceAndStopsAsking(t *testing.T) {
	svc := New("testbox")
	d := newFeedDriver(99, driver.ErrUnsupported)
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}

	ch, _, _, cancel := svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
	defer cancel()

	select {
	case ev := <-ch:
		src, ok := ev.Payload.(fleet.SourceStatus)
		if !ok || src.Status != fleet.SourceDegraded {
			t.Fatalf("want a degraded source status, got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an unsupported feed must be announced, not left as silence")
	}

	time.Sleep(2 * pumpBackoffMin)
	if got := d.Attempts(); got != 1 {
		t.Errorf("attempts = %d; ErrUnsupported describes the substrate, so asking "+
			"again cannot change the answer", got)
	}
}

// A cursor is only worth publishing if it means the same thing to the endpoint
// that hands it out and the one that consumes it.
func TestSnapshotCursorFeedsStraightIntoAWatch(t *testing.T) {
	svc := New("testbox")
	d := newFeedDriver(0, nil)
	if err := svc.RegisterLocalDriver("fake", d); err != nil {
		t.Fatalf("RegisterLocalDriver: %v", err)
	}
	srv := httptestServer(t, svc)

	_, _, _, cancel := svc.Events(context.Background(), ScopeFleet, driver.SubscribeFilter{}, 0, "")
	defer cancel()
	waitForStream(t, svc, true)

	req := authedRequest(t, http.MethodGet, srv.URL+"/v1/sessions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var envelope struct {
		Feed *fleet.FeedPosition `json:"feed"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Feed == nil {
		t.Fatalf("no feed position to resume from: %v %s", err, body)
	}

	go func() {
		waitForSubscriber(t, svc)
		svc.events.publish(sessionEvent("s1", fleet.StatusWorking))
	}()

	got := watchOnce(t, srv.URL, url.Values{
		"since": {strconv.FormatInt(envelope.Feed.Cursor, 10)},
		"epoch": {envelope.Feed.Epoch},
		"wait":  {"3000"},
	})
	if len(got.Events) == 0 {
		t.Fatal("watching from the snapshot's own cursor delivered nothing")
	}
	for _, ev := range got.Events {
		if ev.Kind == fleet.EventControlResync {
			t.Errorf("a cursor this service just published must not be stale: %+v", ev.Payload)
		}
	}
	if got.Cursor <= envelope.Feed.Cursor {
		t.Errorf("cursor did not advance: %d then %d", envelope.Feed.Cursor, got.Cursor)
	}
}

// waitForAttempts blocks until the pump has actually reached the driver.
// streamLive() flips synchronously in ensureStream, before the goroutine that
// subscribes has run, so it is the wrong thing to count subscriptions against.
func waitForAttempts(t *testing.T, d *feedDriver, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for d.Attempts() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if d.Attempts() < want {
		t.Fatalf("driver subscribed %d times, want at least %d", d.Attempts(), want)
	}
}

func waitForStream(t *testing.T, svc *Service, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for svc.events.streamLive() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.events.streamLive() != want {
		t.Fatalf("stream live = %v, want %v", svc.events.streamLive(), want)
	}
}

// httptestServer wraps a Service the tests built themselves, for the cases
// that need a specific driver rather than newTestServer's stub.
func httptestServer(t *testing.T, svc *Service) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewMux(svc, Config{Token: testToken, AllowLocalMutations: true}))
	t.Cleanup(srv.Close)
	return srv
}
