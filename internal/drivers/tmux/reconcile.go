package tmux

import (
	"context"
	"fmt"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// §12's reconciliation, made capable of its own job.
//
// # What was wrong with the previous implementation
//
// §12 says startup is reconciliation rather than initialisation, and requires
// every session found to be classified as **adopted** (matched with
// confidence), **orphaned** (exists, no record) or **vanished** (record
// exists, nothing found).
//
// The previous implementation enumerated faithfully and reported honestly —
// and could not produce two of those three answers. With no persisted records,
// everything found is orphaned by definition and nothing can ever be vanished.
// It looked implemented. It was structurally incapable of the distinction it
// exists to make, which is a worse failure than being absent, because absence
// is visible.
//
// # Records are written when the set changes, not on every read
//
// A read happens on every event trigger, many times a minute on a busy
// machine. Persisting each one would turn a cheap enumeration into a write
// amplifier for no benefit: what reconciliation needs is which sessions exist
// and when they were first seen, and that only changes when a session appears
// or disappears.
//
// # Rule 4 is absolute and is enforced by having no mechanism
//
// "Never destroy anything during reconciliation." This file contains no path
// that can destroy a session — not a guarded one, not a configurable one. A
// session this service cannot explain is a session for a human to look at, and
// the safest way to honour that is to give the code no way to do otherwise.

const sessionsFileName = "sessions"

type sessionRecord struct {
	Created   time.Time `json:"created"`
	Cwd       string    `json:"cwd"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`

	// Status and StatusSince persist §8's `since` across a restart.
	//
	// Without them a restarted service stamps `now` the first time it
	// classifies, and every long-held state reads as freshly entered — a
	// 14-hour abandonment becomes "unchanged for 10m", which is the age of
	// the PROCESS. That number is what a rescue ladder and a sweep key on,
	// the error points the wrong way (always too fresh), and restarts are
	// how this service is deployed.
	//
	// A restored value is second-hand: this instance did not observe it, and
	// §5.2 forbids presenting inference as observation. So it is carried, and
	// the state says where the number came from.
	Status      string    `json:"status,omitempty"`
	StatusSince time.Time `json:"statusSince,omitempty"`
}

type sessionsFile struct {
	Sessions map[string]sessionRecord `json:"sessions"`
}

// Reconciliation is §12's classification of what exists against what was
// remembered.
type Reconciliation struct {
	// Adopted matched a persisted record and resumes normal management.
	Adopted []fleet.Session
	// Orphaned exists with no record. §12 requires it surfaced with
	// inferred confidence and whatever identifying evidence there is —
	// never dropped from listings, never destroyed.
	Orphaned []fleet.Session
	// Vanished was recorded and is no longer present. Marked dead, with
	// evidence noting it disappeared while unobserved.
	Vanished []fleet.Session
	// Live is everything found, whatever its classification.
	Live fleet.Collection[fleet.Session]
}

// Reconcile performs §12's startup reconciliation.
func (d *Driver) Reconcile(ctx context.Context) (Reconciliation, error) {
	// Read what was remembered BEFORE looking at what exists.
	//
	// An ordinary read records the live set when it changes, so enumerating
	// first would have reconciliation compare the world against records its
	// own first read had just written — every session adopted, nothing ever
	// orphaned or vanished. The bug is quiet and total: the classification
	// still runs and still produces an answer, and the answer is that
	// everything is fine.
	prior := d.loadRecords()

	live, err := d.List(ctx, fleet.SystemRequest(), driver.ListFilter{})
	if err != nil {
		return Reconciliation{}, err
	}
	out := Reconciliation{Live: live}
	now := d.now()
	seen := make(map[string]sessionRecord, len(live.Items()))

	ctxB, cancel := d.bounded(ctx)
	rows, _, rowErr := d.enumerate(ctxB)
	cancel()
	created := map[string]time.Time{}
	cwds := map[string]string{}
	if rowErr == nil {
		for _, r := range rows {
			created[r.session] = r.created
			cwds[r.session] = r.cwd
		}
	}

	for _, s := range live.Items() {
		rec, had := prior[s.ID]
		start, cwd := created[s.ID], cwds[s.ID]

		switch {
		case had && rec.Created.Equal(start):
			// Same id, same start time: this is the session we remembered.
			rec.LastSeen = now
			seen[s.ID] = rec
			out.Adopted = append(out.Adopted, s)

		case had:
			// The id is remembered but now holds something that started at
			// a different time. Two facts, not one: the old session is gone
			// and a new one is here under a recycled name (§5.4). Reporting
			// only "adopted" would quietly attribute one session's history
			// to another.
			out.Vanished = append(out.Vanished, vanishedSession(d.machine, s.ID, rec,
				"id was reused by a session that started later"))
			s.State = fleet.InferredState(s.State.Status,
				s.State.Evidence+"; id reused since this service last recorded it", nil)
			out.Orphaned = append(out.Orphaned, s)
			seen[s.ID] = sessionRecord{Created: start, Cwd: cwd, FirstSeen: now, LastSeen: now}

		default:
			s.State = fleet.InferredState(s.State.Status,
				s.State.Evidence+"; no prior record — this service did not start it", nil)
			out.Orphaned = append(out.Orphaned, s)
			seen[s.ID] = sessionRecord{Created: start, Cwd: cwd, FirstSeen: now, LastSeen: now}
		}
	}

	for id, rec := range prior {
		if _, still := seen[id]; still {
			continue
		}
		out.Vanished = append(out.Vanished, vanishedSession(d.machine, id, rec,
			"present at "+rec.LastSeen.Format(time.RFC3339)+", absent now"))
	}

	// Vanished records are dropped only after being reported. Keeping them
	// forever would make every restart re-announce the same disappearance;
	// dropping them before reporting would lose it entirely.
	d.saveRecords(seen)
	return out, nil
}

func vanishedSession(machine fleet.MachineId, id string, rec sessionRecord, why string) fleet.Session {
	started := rec.Created
	return fleet.Session{
		SessionRef: fleet.SessionRef{Machine: machine, ID: id, Name: id},
		Cwd:        fleet.AbsolutePath(rec.Cwd),
		StartedAt:  &started,
		State: fleet.InferredState(fleet.StatusDead,
			"disappeared while unobserved: "+why, nil),
	}
}

func (d *Driver) loadRecords() map[string]sessionRecord {
	var f sessionsFile
	if _, err := d.store.Load(sessionsFileName, &f); err != nil || f.Sessions == nil {
		return map[string]sessionRecord{}
	}
	return f.Sessions
}

func (d *Driver) saveRecords(recs map[string]sessionRecord) {
	_ = d.store.Save(sessionsFileName, sessionsFile{Sessions: recs})
}

// noteSessionSet records the live set when it CHANGES, so reconciliation has
// something to compare against without turning every read into a write.
func (d *Driver) noteSessionSet(rows []paneRow) {
	if d.store == nil {
		return
	}
	prior := d.loadRecords()
	now := d.now()
	next := make(map[string]sessionRecord, len(rows))
	changed := len(prior) != len(rows)
	for _, r := range rows {
		rec, had := prior[r.session]
		if !had || !rec.Created.Equal(r.created) {
			changed = true
			rec = sessionRecord{Created: r.created, Cwd: r.cwd, FirstSeen: now}
		}
		rec.LastSeen = now
		next[r.session] = rec
	}
	if !changed {
		return
	}
	d.saveRecords(next)
}

// noteStatuses persists each session's status and the time it was first seen
// to hold, so §8's `since` survives a restart.
//
// Written only when something CHANGED, exactly like noteSessionSet: a status
// that has not moved needs no write, and a fleet at rest must not turn every
// read into a disk write. Status transitions are rare — they are the same
// events the event plane publishes — so this costs a write per transition
// rather than per read.
func (d *Driver) noteStatuses(obs map[string]observation) {
	if d.store == nil {
		return
	}
	prior := d.loadRecords()
	next := make(map[string]sessionRecord, len(prior))
	changed := false
	for id, rec := range prior {
		o, live := obs[id]
		if !live {
			// Keep the record: a session missing from THIS read may simply
			// have been filtered out, and §12 decides what absence means, not
			// this function.
			next[id] = rec
			continue
		}
		if rec.Status != string(o.status) || !rec.StatusSince.Equal(o.statusSince) {
			rec.Status = string(o.status)
			rec.StatusSince = o.statusSince
			changed = true
		}
		next[id] = rec
	}
	if !changed {
		return
	}
	d.saveRecords(next)
}

// String renders a reconciliation for a startup log line.
func (r Reconciliation) String() string {
	return fmt.Sprintf("adopted=%d orphaned=%d vanished=%d",
		len(r.Adopted), len(r.Orphaned), len(r.Vanished))
}
