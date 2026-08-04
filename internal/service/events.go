package service

import (
	"context"
	"sync"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// The event plane: §7.3's cursor and epoch, §13's multiplexing, and the
// retention that makes a gap announceable instead of silent.
//
// # Who assigns what
//
// A driver produces events and leaves Cursor and Epoch unset, because §7.3
// assigns them per *service instance* and two drivers under one service must
// not mint competing sequences. The hub stamps them on the way through. That
// split is why a driver's event is not directly deliverable and why this type
// exists at all.
//
// # Retention exists so a gap can be announced
//
// A subscriber reconnects with the last cursor it saw. If that cursor is older
// than what is retained, or the epoch differs because this service restarted,
// the subscriber is told `resync_required` rather than quietly resumed from
// the oldest event still held. §7.3 is explicit about why: silently resuming
// produces a subscriber that believes it has a complete history and does not.
// Announced gaps are recoverable; silent ones are not.
//
// # Watching costs what the subscribers asked for
//
// The hub keeps ONE driver subscription, not one per client — two streams over
// the same driver would produce two events for one change, with different
// cursors, and deliver both to everyone.
//
// Its filter is the union of the active clients' filters, recomputed whenever
// a client arrives or leaves. That is D4's lesson applied one level up: if
// every client names the sessions it cares about, the union names them too and
// the driver attaches one connection per named session. If any client asks
// descriptively — a directory prefix, or nothing at all — the union widens to
// everything, because there is no honest way to serve a description for less.
// The cost of a vague subscriber is paid by the machine it subscribes to, and
// that is worth knowing rather than hiding.

const defaultRetention = 512

// hub multiplexes driver events, stamps them per §7.3, retains a window, and
// fans out to subscribers.
type hub struct {
	epoch     string
	retention int

	mu     sync.Mutex
	cursor int64
	ring   []fleet.Event
	subs   map[int]*subscriber
	nextID int

	// driver-side stream, kept alive only while somebody is listening
	streamCancel context.CancelFunc
	streamFilter driver.SubscribeFilter
	streamOn     bool
}

type subscriber struct {
	id     int
	ch     chan fleet.Event
	filter driver.SubscribeFilter
	// dropped records that this subscriber fell behind. Its next read gets
	// a resync rather than a hole it cannot detect.
	dropped bool
}

func newHub(epoch string) *hub {
	return &hub{epoch: epoch, retention: defaultRetention, subs: map[int]*subscriber{}}
}

// publish stamps an event and fans it out. Called by whatever is pumping a
// driver's stream.
func (h *hub) publish(ev fleet.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.cursor++
	ev.Cursor = h.cursor
	ev.Epoch = h.epoch

	h.ring = append(h.ring, ev)
	if len(h.ring) > h.retention {
		h.ring = h.ring[len(h.ring)-h.retention:]
	}

	for _, s := range h.subs {
		if !matchesEvent(s.filter, ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			// Full. §7a listed backpressure as open; this is the answer
			// the rest of the design forces: never drop silently. The
			// subscriber is marked, and its stream ends with a resync so
			// it refetches rather than continuing with a hole it has no
			// way to notice.
			s.dropped = true
		}
	}
}

// add registers a subscriber resuming from (cursor, epoch). It returns the
// backlog to replay and whether the subscriber must resync first.
func (h *hub) add(filter driver.SubscribeFilter, fromCursor int64, fromEpoch string) (*subscriber, []fleet.Event, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	s := &subscriber{id: h.nextID, ch: make(chan fleet.Event, 64), filter: filter}
	h.nextID++
	h.subs[s.id] = s

	// A fresh subscriber (no cursor) starts from now and needs no resync:
	// it is not claiming to have history.
	if fromCursor == 0 && fromEpoch == "" {
		return s, nil, false
	}
	// Different instance: every cursor it holds refers to another sequence.
	if fromEpoch != h.epoch {
		return s, nil, true
	}
	var backlog []fleet.Event
	oldest := int64(0)
	if len(h.ring) > 0 {
		oldest = h.ring[0].Cursor
	}
	if fromCursor < oldest-1 {
		return s, nil, true // fell off the retained window
	}
	for _, ev := range h.ring {
		if ev.Cursor > fromCursor && matchesEvent(filter, ev) {
			backlog = append(backlog, ev)
		}
	}
	return s, backlog, false
}

func (h *hub) remove(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(s.ch)
	}
}

func (h *hub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// unionFilter computes what the driver must watch to satisfy everyone.
//
// If every active subscriber names sessions, the union names them and the
// driver watches exactly those. One subscriber that asks descriptively widens
// the union to everything, because a description cannot be served for less.
func (h *hub) unionFilter() driver.SubscribeFilter {
	h.mu.Lock()
	defer h.mu.Unlock()

	seen := map[string]bool{}
	var ids []string
	for _, s := range h.subs {
		if len(s.filter.Sessions) == 0 {
			return driver.SubscribeFilter{} // somebody wants everything
		}
		for _, id := range s.filter.Sessions {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return driver.SubscribeFilter{Sessions: ids}
}

// matchesEvent applies a subscriber's filter to an already-stamped event.
func matchesEvent(f driver.SubscribeFilter, ev fleet.Event) bool {
	// Control events are never filtered out: a subscriber that filtered
	// away its own resync notice would be told nothing precisely when it
	// most needs telling.
	if ev.Kind == fleet.EventControlResync || ev.Kind == fleet.EventSourceStatus {
		return true
	}
	id, cwd := eventTarget(ev)
	if id == "" {
		return true
	}
	return f.Matches(id, cwd)
}

func eventTarget(ev fleet.Event) (id, cwd string) {
	switch p := ev.Payload.(type) {
	case fleet.Session:
		return p.ID, string(p.Cwd)
	case fleet.SessionStatePayload:
		return p.Ref.ID, ""
	}
	return "", ""
}

// ensureStream starts, restarts or stops the single driver-side subscription
// so that it watches exactly the union of what subscribers want.
//
// Restarting on every filter change is deliberate crudeness: the alternative
// is mutating a live subscription's filter, which the Driver interface does
// not offer and which would need its own resync story to be correct. A restart
// is visible, cheap at these scales, and cannot silently half-apply.
func (s *Service) ensureStream(ctx context.Context) {
	h := s.events
	want := h.unionFilter()
	listeners := h.subscriberCount()

	h.mu.Lock()
	on, cur, cancel := h.streamOn, h.streamFilter, h.streamCancel
	h.mu.Unlock()

	switch {
	case listeners == 0 && on:
		cancel()
		h.mu.Lock()
		h.streamOn, h.streamCancel = false, nil
		h.mu.Unlock()
		return
	case listeners == 0:
		return
	case on && sameFilter(cur, want):
		return
	case on:
		cancel()
	}

	streamCtx, streamCancel := context.WithCancel(context.WithoutCancel(ctx))
	h.mu.Lock()
	h.streamOn, h.streamFilter, h.streamCancel = true, want, streamCancel
	h.mu.Unlock()

	for _, d := range s.localDrivers() {
		go s.pump(streamCtx, d, want)
	}
}

// pump feeds one driver's events into the hub. A driver that cannot stream
// says so once; it is not retried in a loop, because ErrUnsupported is a
// statement about the substrate rather than a transient failure.
func (s *Service) pump(ctx context.Context, d driver.Driver, filter driver.SubscribeFilter) {
	stream, err := d.Subscribe(ctx, fleet.SystemRequest(), filter)
	if err != nil {
		return
	}
	defer stream.Close()
	for {
		ev, err := stream.Next(ctx)
		if err != nil {
			return
		}
		s.events.publish(ev)
	}
}

func sameFilter(a, b driver.SubscribeFilter) bool {
	if a.CwdPrefix != b.CwdPrefix || len(a.Sessions) != len(b.Sessions) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a.Sessions {
		seen[x] = true
	}
	for _, y := range b.Sessions {
		if !seen[y] {
			return false
		}
	}
	return true
}
