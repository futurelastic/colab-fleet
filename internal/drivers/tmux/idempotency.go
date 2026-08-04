package tmux

import (
	"context"
	"sync"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// Durable idempotency (§10, defect D5).
//
// # Why in-memory was not merely incomplete
//
// §10 requires retention to "outlive the caller's retry window", and a service
// restart falls inside that window — it is one very good reason a reply went
// missing in the first place. So keys held only in memory failed exactly when
// the mechanism was needed, on a driver that correctly declares
// supportsResume: sessions survived the restart and the record of why they
// were created did not.
//
// # Intent is written before the side effect
//
// Persisting a key AFTER creating the session closes the restart window and
// leaves a smaller one: crash between the session starting and the key being
// recorded, and a retry creates a second agent in the same working directory.
// That is the same disaster, through a narrower door.
//
// So a key is recorded as PENDING before anything is started, and completed
// with the resulting reference afterwards. A pending record found at startup
// means exactly one thing — this driver was interrupted mid-create — and the
// honest response is to look for what it may have started rather than assume
// either way:
//
//   - a session matching the recorded name exists: adopt it and complete the
//     record. The caller retrying gets the session it already has.
//   - nothing matches: the create did not take effect, and proceeding is safe
//     because there is nothing to duplicate.
//
// Both branches are safe, which is the property worth having. Neither guesses.
//
// # What is deliberately not solved
//
// Two processes writing the same state directory would race, and nothing here
// prevents it. One service instance per machine is the design (§13: "a
// machine-local service"), and a second one would already be fighting over
// ports and control clients. A lock file would give the illusion of addressing
// a problem this shape does not have.

const idemFileName = "idempotency"

// idemPhase distinguishes a key that reserved a create from one that completed.
type idemPhase string

const (
	idemPending  idemPhase = "pending"
	idemComplete idemPhase = "complete"
)

type idemRecord struct {
	Phase   idemPhase `json:"phase"`
	Machine string    `json:"machine,omitempty"`
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	// Cwd is kept for a pending record so an interrupted create can be
	// recognised by more than its name (§5.4's lesson: a name is not an
	// identity).
	Cwd string    `json:"cwd,omitempty"`
	At  time.Time `json:"at"`
}

type idemFile struct {
	Keys map[string]idemRecord `json:"keys"`
}

// idemStore is the driver's key table, backed by a file when one is configured.
type idemStore struct {
	mu        sync.Mutex
	store     *state.Store
	retention time.Duration
	now       func() time.Time
	keys      map[string]idemRecord
}

func newIdemStore(st *state.Store, retention time.Duration, now func() time.Time) (*idemStore, error) {
	s := &idemStore{store: st, retention: retention, now: now, keys: map[string]idemRecord{}}
	if st == nil {
		return s, nil
	}
	var f idemFile
	if _, err := st.Load(idemFileName, &f); err != nil {
		return nil, err
	}
	if f.Keys != nil {
		s.keys = f.Keys
	}
	s.sweepLocked(now())
	return s, s.saveLocked()
}

// lookup returns a completed reference for a key, or a pending record that a
// caller must resolve first.
func (s *idemStore) lookup(key string) (fleet.SessionRef, idemRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	rec, ok := s.keys[key]
	if !ok {
		return fleet.SessionRef{}, idemRecord{}, false
	}
	if rec.Phase == idemComplete {
		return fleet.SessionRef{Machine: fleet.MachineId(rec.Machine), ID: rec.ID, Name: rec.Name}, rec, true
	}
	return fleet.SessionRef{}, rec, true
}

// reserve records the intent to create, before anything is started.
func (s *idemStore) reserve(key, name, cwd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key] = idemRecord{Phase: idemPending, Name: name, Cwd: cwd, At: s.now()}
	return s.saveLocked()
}

// complete records what the create produced.
func (s *idemStore) complete(key string, ref fleet.SessionRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key] = idemRecord{
		Phase: idemComplete, Machine: string(ref.Machine), ID: ref.ID,
		Name: ref.Name, At: s.now(),
	}
	return s.saveLocked()
}

// release drops a reservation whose create demonstrably failed, so a caller
// retrying is not blocked by a record of something that never happened.
func (s *idemStore) release(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, key)
	return s.saveLocked()
}

func (s *idemStore) sweepLocked(now time.Time) {
	for k, r := range s.keys {
		if now.Sub(r.At) > s.retention {
			delete(s.keys, k)
		}
	}
}

func (s *idemStore) saveLocked() error {
	if s.store == nil {
		return nil
	}
	return s.store.Save(idemFileName, idemFile{Keys: s.keys})
}

// resolvePending decides what an interrupted create actually did, by looking
// for what it may have started. See this file's doc comment for why both
// outcomes are safe.
func (d *Driver) resolvePending(ctx context.Context, key string, rec idemRecord) (fleet.SessionRef, bool) {
	rows, _, err := d.enumerate(ctx)
	if err != nil {
		return fleet.SessionRef{}, false
	}
	for _, r := range rows {
		if r.session != rec.Name {
			continue
		}
		// Corroborate on more than the name (§5.4): a recycled name with a
		// different working directory is a different session, and adopting
		// it would hand a caller something it never asked for.
		if rec.Cwd != "" && r.cwd != rec.Cwd {
			continue
		}
		ref := fleet.SessionRef{Machine: d.machine, ID: r.session, Name: r.session}
		_ = d.idem.complete(key, ref)
		return ref, true
	}
	return fleet.SessionRef{}, false
}
