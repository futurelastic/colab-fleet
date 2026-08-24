package service

import (
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// This file closes colab-fleet #103: session.renamed used to fire exactly
// once, at accept time, and never revisit that fact. #97 measured a rename
// that returned 202, read back correct for roughly half an hour, then
// silently reverted — id, name and attach target all restored — with
// nothing on the event stream saying so for that whole window. A subscriber
// that re-keyed on the accept-time event, as api-http.md §3.3 tells it to,
// held a name that had already stopped being true.
//
// # Why this watches the passive event flow instead of polling a driver
//
// #97's own ADR (docs/adr/97-identity-in-record.md) rejected a background
// poller inside the tmux driver for the identical reason a poller here would
// be wrong too: "adding one for a defect that only matters when a caller is
// actually reading is disproportionate." This package does not add one
// either. It watches session.created/session.closed — events this service
// already publishes, driven by the driver's own edge-triggered
// re-enumeration (internal/drivers/tmux/subscribe.go) — for the specific
// signature #97 measured: the renamed id stops resolving, and the old id's
// identity, matched by StartedAt rather than by name alone (SessionRef's own
// rule, §5.4), comes back. Nothing here calls a driver directly; everything
// it knows, it learns from events already flowing through this hub.
//
// # Why the two ids are never mistaken for each other
//
// A closed event alone is exactly as consistent with an ordinary DELETE of
// the freshly-renamed session as it is with a revert. This does not guess
// between them: it reports the ambiguous case as RenameUnconfirmed and
// reserves RenameContested for the one signature that is actually
// unambiguous — the OLD id's identity, StartedAt and all, reappearing after
// the new one stopped resolving.
//
// # What is NOT closed here
//
// This bookkeeping is in-memory, scoped to one hub, and does not survive a
// restart of this process — a rename whose corroboration window was still
// open when this service restarted gets no follow-up event at all, silently,
// which is the exact failure this file otherwise exists to prevent. Closing
// that needs the durable, machine-readable identity record colab-fleet #102
// asks for; this file is deliberately not reaching into that scope.

// renameCorroborationWindow bounds how long the hub keeps watching a rename
// before it is willing to call it corroborated. #97 measured a revert
// manifesting about thirty-five minutes after accept; this margins that by
// roughly another twenty-five rather than tuning to the one data point on
// hand — the same "measured, then margined rather than trusted exactly" move
// defaultLinger's own doc comment makes for a much shorter window.
const renameCorroborationWindow = 60 * time.Minute

// renameKey addresses one pending rename by the id a subscriber would have
// re-keyed onto — the only id that can still be watched, since From stops
// existing (from this service's point of view) the moment accept fires.
type renameKey struct {
	machine fleet.MachineId
	to      string
}

// pendingRename is what this hub still needs to learn before it can say
// whether a rename held. Guarded by hub.renameMu, never hub.mu — see
// hub.publish's own note on why the two must stay separate locks.
type pendingRename struct {
	from      string
	startedAt *fleet.Timestamp

	// sawFromRecreated is set the moment a session.created event reports the
	// OLD id back, with a StartedAt matching this rename's own — #97's
	// unambiguous revert signature. Only meaningful once sawToClosed-time
	// arrives; recorded eagerly because session.created(from) is observed to
	// arrive before the paired session.closed(to) within one diff pass
	// (internal/drivers/tmux/subscribe.go processes cur.Items() before it
	// walks what went missing), but nothing here depends on that ordering for
	// SAFETY — the worst a reordering costs is under-reporting a contested
	// rename as unconfirmed (finalizeRenameLocked's ordering fallback), never
	// the reverse.
	sawFromRecreated bool

	// gapObserved records that this machine's event feed had a hole
	// somewhere in the window — a source.status degraded, or a
	// control.resync. A hole means "corroborated" would be a claim this hub
	// did not actually earn: it did not watch continuously, so it cannot
	// swear nothing happened in the gap.
	gapObserved bool

	resolved bool
	timer    *time.Timer
}

// renamePlane is the corroboration bookkeeping half of the hub — a separate
// lock and a separate type so it can be read and reasoned about apart from
// the retention/fan-out machinery it lives beside.
type renamePlane struct {
	mu      sync.Mutex
	pending map[renameKey]*pendingRename
	window  time.Duration
}

func newRenamePlane() renamePlane {
	return renamePlane{
		pending: map[renameKey]*pendingRename{},
		window:  renameCorroborationWindow,
	}
}

// watchRename registers corroboration bookkeeping for a rename this hub just
// published the RenameAccepted event for. Called from Service.publishRename,
// after that publish — never before, so a subscriber can never observe a
// pending watch for a rename it has not yet been told about.
func (h *hub) watchRename(machine fleet.MachineId, from, to string, startedAt *fleet.Timestamp) {
	if from == "" || to == "" || from == to {
		// Nothing to watch: a same-id "rename" (if a driver ever accepts
		// one) leaves no old identity that could plausibly reappear, and a
		// blank id cannot be matched against anything later.
		return
	}
	key := renameKey{machine: machine, to: to}
	p := &pendingRename{from: from, startedAt: startedAt}

	h.rename.mu.Lock()
	// A second rename landing on the same `to` before the first resolved is
	// only reachable if `to` was freed and reused — the earlier watch no
	// longer describes anything a subscriber could still be holding, so it
	// is replaced rather than layered under a second timer racing it.
	if old, exists := h.rename.pending[key]; exists && old.timer != nil {
		old.timer.Stop()
	}
	h.rename.pending[key] = p
	// p.timer is set while still holding the lock, and deliberately not
	// after — resolveRenameWindow reads it under this same lock, and with a
	// short enough window (tests use milliseconds) the callback can fire
	// before an unlocked assignment would have landed. Setting it here makes
	// the write happen-before any read the callback can perform.
	p.timer = time.AfterFunc(h.rename.window, func() { h.resolveRenameWindow(key) })
	h.rename.mu.Unlock()
}

// resolveRenameWindow is the corroboration window's own deadline firing: no
// session.closed for `to` arrived in time, so this hub is willing to call it
// corroborated — unless the feed itself had a gap during the window, in
// which case that is not a claim this hub earned (see gapObserved).
func (h *hub) resolveRenameWindow(key renameKey) {
	h.rename.mu.Lock()
	p, found := h.rename.pending[key]
	if !found || p.resolved {
		h.rename.mu.Unlock()
		return
	}
	verdict := fleet.RenameCorroborated
	if p.gapObserved {
		verdict = fleet.RenameUnconfirmed
	}
	ev := finalizeRenameLocked(key, p, verdict)
	h.rename.mu.Unlock()

	h.publish(ev)
}

// observeRenameCorroboration is called from publish, for every event this
// hub stamps, AFTER h.mu is released (publish's own note explains why: a
// resolved rename recurses into publish to announce itself, and h.mu is not
// reentrant). It never blocks on anything but its own renamePlane lock.
func (h *hub) observeRenameCorroboration(ev fleet.Event) []fleet.Event {
	switch ev.Kind {
	case fleet.EventSessionCreated:
		sess, ok := ev.Payload.(fleet.Session)
		if !ok {
			return nil
		}
		h.rename.mu.Lock()
		for key, p := range h.rename.pending {
			if key.machine != ev.Machine || p.resolved || p.sawFromRecreated {
				continue
			}
			if sess.ID == p.from && startedAtMatches(p.startedAt, sess.StartedAt) {
				p.sawFromRecreated = true
			}
		}
		h.rename.mu.Unlock()
		return nil

	case fleet.EventSessionClosed:
		payload, ok := ev.Payload.(fleet.SessionStatePayload)
		if !ok {
			return nil
		}
		key := renameKey{machine: ev.Machine, to: payload.Ref.ID}
		h.rename.mu.Lock()
		p, found := h.rename.pending[key]
		if !found || p.resolved {
			h.rename.mu.Unlock()
			return nil
		}
		verdict := fleet.RenameUnconfirmed
		if p.sawFromRecreated {
			verdict = fleet.RenameContested
		}
		followUp := finalizeRenameLocked(key, p, verdict)
		h.rename.mu.Unlock()
		return []fleet.Event{followUp}

	case fleet.EventSourceStatus:
		status, ok := ev.Payload.(fleet.SourceStatus)
		if !ok || status.Status != fleet.SourceDegraded {
			return nil
		}
		h.markRenameGap(ev.Machine)
		return nil

	case fleet.EventControlResync:
		// Any resync means this hub's own sequence had a hole — reachable
		// through more causes than a degraded source (§7.3: an expired
		// cursor, a changed epoch), but every one of them means an event
		// this corroboration watch might have needed could have been missed
		// rather than genuinely absent. Treated the same as a degraded
		// source: a hole is a hole regardless of which of §7.3's reasons
		// produced it.
		h.markRenameGap(ev.Machine)
		return nil
	}
	return nil
}

// markRenameGap flags every still-pending rename for one machine as having
// an unwatched hole in its window. It does not resolve anything itself —
// gapObserved is only consulted at the window's own deadline
// (resolveRenameWindow) or when a genuine session.closed(to) arrives; a gap
// alone says nothing about whether the rename held, only that this hub
// cannot swear to it.
func (h *hub) markRenameGap(machine fleet.MachineId) {
	h.rename.mu.Lock()
	defer h.rename.mu.Unlock()
	for key, p := range h.rename.pending {
		if key.machine == machine && !p.resolved {
			p.gapObserved = true
		}
	}
}

// finalizeRenameLocked stops the watch and builds the follow-up event.
// Called with h.rename.mu held; the caller publishes the result after
// releasing it.
func finalizeRenameLocked(key renameKey, p *pendingRename, verdict fleet.RenameCorroboration) fleet.Event {
	p.resolved = true
	if p.timer != nil {
		p.timer.Stop() // a no-op if this IS the timer callback firing; safe either way
	}
	return fleet.Event{
		Machine: key.machine,
		Kind:    fleet.EventSessionRenamed,
		Payload: fleet.SessionRenamed{
			Machine:       key.machine,
			From:          p.from,
			To:            key.to,
			StartedAt:     p.startedAt,
			Corroboration: verdict,
		},
	}
}

// publishRename is http.go's one entry point for announcing a rename,
// replacing what used to be a direct svc.events.publish call there. It
// emits the accept-time event at the same moment it always fired — a
// subscriber learns of the rename exactly as fast as before — and then
// registers this hub's corroboration watch for what follows.
func (s *Service) publishRename(machine fleet.MachineId, from, to string, startedAt *fleet.Timestamp) {
	s.events.publish(fleet.Event{
		Machine: machine,
		Kind:    fleet.EventSessionRenamed,
		Payload: fleet.SessionRenamed{
			Machine:       machine,
			From:          from,
			To:            to,
			StartedAt:     startedAt,
			Corroboration: fleet.RenameAccepted,
		},
	})
	s.events.watchRename(machine, from, to, startedAt)
}

// startedAtMatches answers colab-fleet #103's own corroboration question the
// same way §5.4 already answers every other "is this really the same
// session" question: never on the id alone. Either side unknown means no —
// an absent StartedAt cannot corroborate, it can only fail to contradict,
// and this rename's own accept event already told the truth about that by
// omitting StartedAt from what it published (Service.publishRename).
func startedAtMatches(want, got *fleet.Timestamp) bool {
	if want == nil || got == nil {
		return false
	}
	return want.Equal(*got)
}
