package service

import (
	"sort"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// Session labels are stored here, by the service — colab-fleet #153.
//
// # Why the service and not each driver
//
// A label is caller data the service keeps, like the event cursor: no driver
// observes it and no substrate has anywhere to put it. Storing it per driver
// would mean writing it once per runtime and growing the Driver interface for
// a fact every driver would handle identically.
//
// # How a record finds its session again
//
// A record is keyed by (runtime, id) and carries the session's startedAt when
// it was known. An id alone is not enough, for §5.4's reason: ids are
// recyclable, and a new session under an old id must not inherit somebody
// else's labels. So a record whose startedAt disagrees with the live session's
// is dropped, not attached. A rename moves the record to the new id when it is
// accepted; a rename that later reverts is caught by retain, which re-attaches
// an orphaned record to the live session with the same startedAt.
//
// # When records go away
//
// On a successful close through this service, and on retain — which runs only
// after an UNFILTERED listing a driver answered completely. A filtered or
// failed listing proves nothing about what is absent (§5.7), so it never
// deletes anything.

// labelStateName is this store's document in the state directory.
const labelStateName = "labels"

type labelRecord struct {
	Runtime   fleet.RuntimeId   `json:"runtime,omitempty"`
	ID        string            `json:"id"`
	StartedAt *fleet.Timestamp  `json:"startedAt,omitempty"`
	Labels    map[string]string `json:"labels"`
	// UpdatedAt is when this record last changed on this machine's clock.
	// retain never drops a record newer than the listing it reconciles
	// against: a create that finished while that listing ran is not missing,
	// it is merely later than the snapshot.
	UpdatedAt time.Time `json:"updatedAt"`
}

type labelKey struct {
	runtime fleet.RuntimeId
	id      string
}

type labelStore struct {
	mu   sync.Mutex
	recs map[labelKey]*labelRecord
	st   *state.Store
}

func newLabelStore() *labelStore {
	return &labelStore{recs: map[labelKey]*labelRecord{}}
}

// load adopts a persisted document. A store that will not parse is an error
// the operator sees — state.Store's own rule, not recovered from here.
func (ls *labelStore) load(st *state.Store) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.st = st
	var recs []labelRecord
	found, err := st.Load(labelStateName, &recs)
	if err != nil || !found {
		return err
	}
	for i := range recs {
		r := recs[i]
		if r.ID == "" || len(r.Labels) == 0 {
			continue
		}
		ls.recs[labelKey{r.Runtime, r.ID}] = &r
	}
	return nil
}

// saveLocked writes the whole document. Callers hold ls.mu. A failed write is
// deliberately not fatal to the request that caused it: the labels are in
// force for this process either way, and the next successful write carries
// them.
func (ls *labelStore) saveLocked() {
	if ls.st == nil {
		return
	}
	recs := make([]labelRecord, 0, len(ls.recs))
	for _, r := range ls.recs {
		recs = append(recs, *r)
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Runtime != recs[j].Runtime {
			return recs[i].Runtime < recs[j].Runtime
		}
		return recs[i].ID < recs[j].ID
	})
	_ = ls.st.Save(labelStateName, recs)
}

// findLocked returns the record for a live session, or nil. An empty runtime
// matches a record of any runtime carrying this id — the event plane knows a
// session's id but not always which local driver produced it. A record whose
// startedAt contradicts the live session's is a recycled id: it is removed,
// and nothing is returned.
func (ls *labelStore) findLocked(runtime fleet.RuntimeId, id string, startedAt *fleet.Timestamp) (labelKey, *labelRecord) {
	k := labelKey{runtime, id}
	r, ok := ls.recs[k]
	if !ok && runtime == "" {
		for rk, rr := range ls.recs {
			if rk.id == id {
				k, r, ok = rk, rr, true
				break
			}
		}
	}
	if !ok {
		return k, nil
	}
	if r.StartedAt != nil && startedAt != nil && !r.StartedAt.Equal(*startedAt) {
		delete(ls.recs, k)
		ls.saveLocked()
		return k, nil
	}
	return k, r
}

// get returns a copy of a session's labels, never nil.
func (ls *labelStore) get(runtime fleet.RuntimeId, id string, startedAt *fleet.Timestamp) map[string]string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	_, r := ls.findLocked(runtime, id, startedAt)
	if r == nil {
		return map[string]string{}
	}
	return fleet.CopyLabels(r.Labels)
}

// setIfAbsent stores a create's labels. "If absent" because a create retried
// under the same idempotency key answers with the SAME session, and must not
// overwrite labels written to it since.
func (ls *labelStore) setIfAbsent(runtime fleet.RuntimeId, id string, startedAt *fleet.Timestamp, labels map[string]string) (map[string]string, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if _, r := ls.findLocked(runtime, id, startedAt); r != nil {
		return fleet.CopyLabels(r.Labels), false
	}
	if len(labels) == 0 {
		return map[string]string{}, false
	}
	ls.recs[labelKey{runtime, id}] = &labelRecord{
		Runtime: runtime, ID: id, StartedAt: startedAt, Labels: fleet.CopyLabels(labels),
		UpdatedAt: time.Now(),
	}
	ls.saveLocked()
	return fleet.CopyLabels(labels), true
}

// merge applies a patch — a nil value deletes its key — and validates the
// RESULT, so a patch that removes keys can bring an over-full map back under
// the limit. On a validation failure nothing is changed.
func (ls *labelStore) merge(runtime fleet.RuntimeId, id string, startedAt *fleet.Timestamp, patch map[string]*string) (map[string]string, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	k, r := ls.findLocked(runtime, id, startedAt)
	next := map[string]string{}
	if r != nil {
		next = fleet.CopyLabels(r.Labels)
	}
	for key, v := range patch {
		if v == nil {
			delete(next, key)
			continue
		}
		next[key] = *v
	}
	if err := fleet.ValidateLabels(next); err != nil {
		return nil, err
	}
	switch {
	case len(next) == 0:
		delete(ls.recs, k)
	case r != nil:
		r.Labels = next
		r.UpdatedAt = time.Now()
		if r.StartedAt == nil {
			r.StartedAt = startedAt
		}
	default:
		ls.recs[labelKey{runtime, id}] = &labelRecord{Runtime: runtime, ID: id, StartedAt: startedAt, Labels: next, UpdatedAt: time.Now()}
	}
	ls.saveLocked()
	return fleet.CopyLabels(next), nil
}

// rekey follows an accepted rename.
func (ls *labelStore) rekey(runtime fleet.RuntimeId, from, to string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	k, r := ls.findLocked(runtime, from, nil)
	if r == nil || from == to {
		return
	}
	delete(ls.recs, k)
	r.ID = to
	r.UpdatedAt = time.Now()
	ls.recs[labelKey{k.runtime, to}] = r
	ls.saveLocked()
}

// remove forgets a session closed through this service.
func (ls *labelStore) remove(runtime fleet.RuntimeId, id string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if k, r := ls.findLocked(runtime, id, nil); r != nil {
		delete(ls.recs, k)
		ls.saveLocked()
	}
}

// retain reconciles one runtime's records against a COMPLETE listing of its
// live sessions. Callers must only pass an unfiltered listing the driver
// answered with an ok source — see the type's doc comment.
//
// An orphaned record whose startedAt equals a live, unlabelled session's is
// re-attached to it rather than dropped: that is a rename this service did not
// see through to its final id (a revert, or a name the driver adjusted).
func (ls *labelStore) retain(runtime fleet.RuntimeId, live []fleet.Session, listedAt time.Time) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	byID := make(map[string]fleet.Session, len(live))
	for _, s := range live {
		byID[s.ID] = s
	}
	changed := false
	var orphans []*labelRecord
	for k, r := range ls.recs {
		if k.runtime != runtime {
			continue
		}
		if _, ok := byID[k.id]; ok || r.UpdatedAt.After(listedAt) {
			continue
		}
		delete(ls.recs, k)
		orphans = append(orphans, r)
		changed = true
	}
	for _, r := range orphans {
		if r.StartedAt == nil {
			continue
		}
		for _, s := range live {
			if s.StartedAt == nil || !s.StartedAt.Equal(*r.StartedAt) {
				continue
			}
			k := labelKey{runtime, s.ID}
			if _, taken := ls.recs[k]; taken {
				break
			}
			r.ID = s.ID
			ls.recs[k] = r
			break
		}
	}
	if changed {
		ls.saveLocked()
	}
}
