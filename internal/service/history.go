package service

import (
	"context"
	"sort"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// Closed-session records — colab-fleet #179. See fleet.ClosedSession for what
// a record promises; this file is how the service keeps them.
//
// # Two maps, one document
//
// A tombstone has to carry what the session looked like, and by the time a
// session is gone nothing can be asked about it any more. So the service keeps
// a small "seen" record per live local session (runtime, id, name, cwd,
// startedAt, conversation, last sighting), refreshed from every create and
// every listing it answers, and turns that record into a tombstone when the
// session ends. Both halves live in one state document, so a restart keeps
// both: a session that ended while the service was down is found missing by
// the first complete read after start-up, and still has its metadata.
//
// # Where an end is learned
//
//   - A close through this service that the driver accepted: exact.
//   - A complete, unfiltered listing of a runtime that no longer contains a
//     seen session: an upper bound (fleet.ClosedByAbsent). A filtered or
//     partial listing proves nothing about absence (§5.7) and never ends
//     anything — the same rule the label store's retain follows.
//   - A local session.closed event does NOT write a tombstone by itself: a
//     rename also retires the old id on the stream. It asks for a local
//     re-listing instead (requestSweep), which settles the question with the
//     rename-carry rule below and gives a timely closedAt whenever a stream
//     is live.
//
// # Renames and recycled ids
//
// A session missing under its old id and present under a new one is the same
// run, not an end: carried when exactly one newly-listed session of the same
// runtime has the missing record's startedAt. A seen id that now names a
// session with a DIFFERENT startedAt is §5.4's recycled id: the old occupant
// ended, and gets its tombstone.
//
// # Retention
//
// Tombstones older than the retention period are pruned on every write AND
// filtered on every read, so an idle service does not keep answering with
// records past their window. A seen record not sighted within the retention
// period is dropped too — it can only belong to a runtime no longer listed.

// historyStateName is this store's document in the state directory.
const historyStateName = "session-history"

// DefaultClosedRetention is how long a closed-session record is kept when the
// operator configures nothing.
const DefaultClosedRetention = 14 * 24 * time.Hour

// seenFlushInterval bounds how stale a persisted lastSeenAt may get. Writing
// on every listing would make every read a write; writing only on change
// would let a restart report a lastSeenAt from hours ago. Neither extreme is
// wrong, only wasteful or loose — this is the middle.
const seenFlushInterval = 5 * time.Minute

type seenRecord struct {
	Runtime      fleet.RuntimeId        `json:"runtime,omitempty"`
	ID           string                 `json:"id"`
	Name         string                 `json:"name,omitempty"`
	Cwd          fleet.AbsolutePath     `json:"cwd,omitempty"`
	StartedAt    *fleet.Timestamp       `json:"startedAt,omitempty"`
	Conversation *fleet.ConversationRef `json:"conversation,omitempty"`
	LastSeen     time.Time              `json:"lastSeen"`
}

type historyDoc struct {
	Seen   []seenRecord          `json:"seen"`
	Closed []fleet.ClosedSession `json:"closed"`
}

type historyStore struct {
	mu        sync.Mutex
	self      fleet.MachineId
	seen      map[labelKey]*seenRecord
	closed    []fleet.ClosedSession
	retention time.Duration
	st        *state.Store
	savedAt   time.Time
	now       func() time.Time
}

func newHistoryStore(self fleet.MachineId) *historyStore {
	return &historyStore{
		self:      self,
		seen:      map[labelKey]*seenRecord{},
		retention: DefaultClosedRetention,
		now:       time.Now,
	}
}

// load adopts a persisted document. A document that will not parse is an
// error the operator sees — state.Store's own rule.
func (h *historyStore) load(st *state.Store) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.st = st
	var doc historyDoc
	found, err := st.Load(historyStateName, &doc)
	if err != nil || !found {
		return err
	}
	for i := range doc.Seen {
		r := doc.Seen[i]
		if r.ID == "" {
			continue
		}
		h.seen[labelKey{r.Runtime, r.ID}] = &r
	}
	h.closed = doc.Closed
	h.savedAt = h.now()
	return nil
}

// setRetention changes how long tombstones are kept. Non-positive restores
// the default.
func (h *historyStore) setRetention(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d <= 0 {
		d = DefaultClosedRetention
	}
	h.retention = d
}

func (h *historyStore) pruneLocked(now time.Time) bool {
	cut := now.Add(-h.retention)
	changed := false
	kept := h.closed[:0]
	for _, c := range h.closed {
		if c.ClosedAt.Before(cut) {
			changed = true
			continue
		}
		kept = append(kept, c)
	}
	h.closed = kept
	for k, r := range h.seen {
		if r.LastSeen.Before(cut) {
			delete(h.seen, k)
			changed = true
		}
	}
	return changed
}

// saveLocked prunes and writes the whole document. A failed write is not
// fatal to the request that caused it — the record is in force for this
// process, and the next successful write carries it.
func (h *historyStore) saveLocked(now time.Time) {
	h.pruneLocked(now)
	h.savedAt = now
	if h.st == nil {
		return
	}
	doc := historyDoc{Seen: make([]seenRecord, 0, len(h.seen)), Closed: h.closed}
	if doc.Closed == nil {
		doc.Closed = []fleet.ClosedSession{}
	}
	for _, r := range h.seen {
		doc.Seen = append(doc.Seen, *r)
	}
	sort.Slice(doc.Seen, func(i, j int) bool {
		if doc.Seen[i].Runtime != doc.Seen[j].Runtime {
			return doc.Seen[i].Runtime < doc.Seen[j].Runtime
		}
		return doc.Seen[i].ID < doc.Seen[j].ID
	})
	_ = h.st.Save(historyStateName, doc)
}

func knownConversation(c *fleet.ConversationRef) *fleet.ConversationRef {
	if c == nil || !c.Known {
		return nil
	}
	cp := *c
	return &cp
}

// sightLocked records one live sighting. It reports whether anything a
// tombstone would carry changed — the only reason a sighting alone is worth
// a write.
func (h *historyStore) sightLocked(rt fleet.RuntimeId, s fleet.Session, at time.Time) bool {
	k := labelKey{rt, s.ID}
	conv := knownConversation(s.Conversation)
	r, had := h.seen[k]
	if !had {
		h.seen[k] = &seenRecord{Runtime: rt, ID: s.ID, Name: s.Name, Cwd: s.Cwd,
			StartedAt: s.StartedAt, Conversation: conv, LastSeen: at}
		return true
	}
	changed := false
	if s.Name != "" && s.Name != r.Name {
		r.Name, changed = s.Name, true
	}
	if s.Cwd != "" && s.Cwd != r.Cwd {
		r.Cwd, changed = s.Cwd, true
	}
	if s.StartedAt != nil && (r.StartedAt == nil || !r.StartedAt.Equal(*s.StartedAt)) {
		r.StartedAt, changed = s.StartedAt, true
	}
	if conv != nil && (r.Conversation == nil || r.Conversation.ID != conv.ID) {
		r.Conversation, changed = conv, true
	}
	if at.After(r.LastSeen) {
		r.LastSeen = at
	}
	return changed
}

func (h *historyStore) tombstoneLocked(r *seenRecord, at time.Time, by fleet.ClosureKind, evidence string) {
	name := r.Name
	if name == "" {
		name = r.ID
	}
	h.closed = append(h.closed, fleet.ClosedSession{
		SessionRef:   fleet.SessionRef{Machine: h.self, ID: r.ID, Name: name},
		Runtime:      r.Runtime,
		Cwd:          r.Cwd,
		StartedAt:    r.StartedAt,
		Conversation: r.Conversation,
		LastSeenAt:   r.LastSeen,
		ClosedAt:     at,
		ClosedBy:     by,
		Evidence:     evidence,
	})
}

// created records a session this service just started, so a close that
// arrives before any listing still has something to describe.
func (h *historyStore) created(rt fleet.RuntimeId, s fleet.Session) {
	if s.ID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.sightLocked(rt, s, now)
	h.saveLocked(now)
}

// renamed moves a seen record to its new id (a rename accepted through this
// service). A rename that later reverts is settled by observe's carry rule.
func (h *historyStore) renamed(rt fleet.RuntimeId, from, to string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.seen[labelKey{rt, from}]
	if !ok || from == to {
		return
	}
	delete(h.seen, labelKey{rt, from})
	r.ID, r.Name = to, to
	h.seen[labelKey{rt, to}] = r
	h.saveLocked(h.now())
}

// closedByRequest writes the tombstone for a close this service relayed to a
// local driver and the driver accepted.
func (h *historyStore) closedByRequest(rt fleet.RuntimeId, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	k := labelKey{rt, id}
	r, ok := h.seen[k]
	if !ok {
		// Never sighted by this service (it started before any listing and
		// was closed before one ran). The end is still a fact worth keeping;
		// only the metadata is thin, and it says so by being absent.
		r = &seenRecord{Runtime: rt, ID: id}
	}
	delete(h.seen, k)
	h.tombstoneLocked(r, now, fleet.ClosedByClose, "closed through this service")
	h.saveLocked(now)
}

// observe folds one local runtime's listing in. complete is true only for an
// unfiltered listing every source answered ok; only such a listing may end a
// session. listedAt is when the listing was requested: a record sighted after
// it (a create that finished while the listing ran) is not missing, merely
// later than the snapshot.
func (h *historyStore) observe(rt fleet.RuntimeId, items []fleet.Session, complete bool, listedAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	changed := false

	present := make(map[string]bool, len(items))
	fresh := make([]fleet.Session, 0)
	for _, s := range items {
		present[s.ID] = true
		// A close accepted by the driver that did not take: the session is
		// still here, so its tombstone was premature. Withdrawn rather than
		// left to contradict the live list.
		if h.withdrawLocked(rt, s) {
			changed = true
		}
		k := labelKey{rt, s.ID}
		r, had := h.seen[k]
		if !had {
			fresh = append(fresh, s)
			continue
		}
		if r.StartedAt != nil && s.StartedAt != nil && !r.StartedAt.Equal(*s.StartedAt) {
			// §5.4: the id now names a session that started at a different
			// time. Any listing showing that proves the old occupant ended —
			// complete or not — and the new one starts a record of its own.
			h.tombstoneLocked(r, now, fleet.ClosedByAbsent,
				"id is now held by a session that started at a different time")
			delete(h.seen, k)
			changed = true
		}
		if h.sightLocked(rt, s, now) {
			changed = true
		}
	}

	if complete {
		for k, r := range h.seen {
			if k.runtime != rt || present[k.id] || !r.LastSeen.Before(listedAt) {
				continue
			}
			if i := carryIndex(r, fresh); i >= 0 {
				// Same run under a new id — a rename, not an end.
				delete(h.seen, k)
				r.ID, r.Name = fresh[i].ID, fresh[i].Name
				h.seen[labelKey{rt, r.ID}] = r
				fresh = append(fresh[:i], fresh[i+1:]...)
				changed = true
				continue
			}
			h.tombstoneLocked(r, now, fleet.ClosedByAbsent,
				"absent from a complete listing of its runtime; it ended after lastSeenAt")
			delete(h.seen, k)
			changed = true
		}
	}

	for _, s := range fresh {
		h.sightLocked(rt, s, now)
		changed = true
	}

	if changed || now.Sub(h.savedAt) >= seenFlushInterval {
		h.saveLocked(now)
	}
}

// carryIndex finds the one newly-listed session that is r's run under a new
// id. Ambiguity carries nothing: two candidates with r's startedAt could each
// be it, and guessing would attribute one session's history to another.
func carryIndex(r *seenRecord, fresh []fleet.Session) int {
	if r.StartedAt == nil {
		return -1
	}
	found := -1
	for i, s := range fresh {
		if s.StartedAt != nil && s.StartedAt.Equal(*r.StartedAt) {
			if found >= 0 {
				return -1
			}
			found = i
		}
	}
	return found
}

// withdrawLocked removes a ClosedByClose tombstone for a session that is
// demonstrably still running (same runtime, id and startedAt).
func (h *historyStore) withdrawLocked(rt fleet.RuntimeId, s fleet.Session) bool {
	if s.StartedAt == nil {
		return false
	}
	for i := len(h.closed) - 1; i >= 0; i-- {
		c := h.closed[i]
		if c.ClosedBy == fleet.ClosedByClose && c.Runtime == rt && c.ID == s.ID &&
			c.StartedAt != nil && c.StartedAt.Equal(*s.StartedAt) {
			h.closed = append(h.closed[:i], h.closed[i+1:]...)
			return true
		}
	}
	return false
}

// list returns the retained tombstones that closed at or after since (zero
// means everything retained), newest first.
func (h *historyStore) list(since time.Time) []fleet.ClosedSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := h.now().Add(-h.retention)
	out := make([]fleet.ClosedSession, 0, len(h.closed))
	for _, c := range h.closed {
		if c.ClosedAt.Before(cut) || c.ClosedAt.Before(since) {
			continue
		}
		out = append(out, c)
	}
	sortClosed(out)
	return out
}

func sortClosed(items []fleet.ClosedSession) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].ClosedAt.After(items[j].ClosedAt) })
}

// ListClosedSessions answers GET /v1/sessions/closed. ScopeLocal reads this
// machine's own records only (§13.1); ScopeFleet adds every peer that can
// answer, and a peer that cannot contributes a SourceStatus saying so — never
// a silent absence (§9).
func (s *Service) ListClosedSessions(ctx context.Context, req fleet.Request, scope Scope, since time.Time, callerDeadline time.Duration) (fleet.Collection[fleet.ClosedSession], error) {
	items := s.history.list(since)
	sources := []fleet.SourceStatus{{Machine: s.self, Status: fleet.SourceOK, ObservedAt: time.Now()}}

	if scope == ScopeFleet {
		for machine, d := range s.peerDrivers() {
			cl, ok := d.(driver.ClosedLister)
			if !ok {
				sources = append(sources, fleet.SourceStatus{
					Machine: machine, Status: fleet.SourceDegraded,
					Error: "this peer's driver cannot report closed sessions", ObservedAt: time.Now(),
				})
				continue
			}
			deadline := effectiveDeadline(d.Capabilities().DeadlineMs, callerDeadline)
			callCtx, cancel := context.WithTimeout(ctx, deadline)
			col, err := cl.ListClosed(callCtx, req, since)
			cancel()
			if err != nil {
				sources = append(sources, fleet.SourceStatus{
					Machine: machine, Status: fleet.SourceUnreachable,
					Error: err.Error(), ObservedAt: time.Now(),
				})
				continue
			}
			items = append(items, col.Items()...)
			sources = append(sources, col.Sources()...)
		}
		sortClosed(items)
	}
	return fleet.NewCollection(items, sources)
}

// SetClosedRetention configures how long closed-session records are kept
// (#179). Call it at startup; non-positive restores the default.
func (s *Service) SetClosedRetention(d time.Duration) { s.history.setRetention(d) }

// SweepLocal lists every local runtime once, unfiltered, so the history store
// can notice sessions that ended unobserved. cmd/colab-fleetd calls it at
// start-up (sessions that ended while the service was down); requestSweep
// calls it when a local event stream reports a session gone.
func (s *Service) SweepLocal(ctx context.Context) {
	_, _ = s.ListSessions(ctx, fleet.SystemRequest(), ScopeLocal, driver.ListFilter{}, 0)
}

// requestSweep runs SweepLocal in the background, at most one at a time; a
// request arriving while one runs schedules exactly one more, so the last
// change is always read after it happened.
func (s *Service) requestSweep() {
	s.sweepMu.Lock()
	if s.sweeping {
		s.sweepAgain = true
		s.sweepMu.Unlock()
		return
	}
	s.sweeping = true
	s.sweepMu.Unlock()
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.SweepLocal(ctx)
			cancel()
			s.sweepMu.Lock()
			if !s.sweepAgain {
				s.sweeping = false
				s.sweepMu.Unlock()
				return
			}
			s.sweepAgain = false
			s.sweepMu.Unlock()
		}
	}()
}
