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

	// Pane is the id of this session's active pane at the last read.
	// Together with Created (above) it identifies a session RUN rather than
	// a session NAME — the same pairing conversationKey already uses
	// (conversation.go), for the same reason: a rename changes the name and
	// the map key that goes with it, but not the pane the session actually
	// runs in. Empty for a record this driver has not yet corroborated
	// against a live read (e.g. the stub Create writes before the session
	// has ever been enumerated) — every reader below treats an empty Pane
	// as "cannot corroborate", never as a match against another blank one.
	Pane string `json:"pane,omitempty"`

	// Name is the name THIS DRIVER last asserted for this session — set at
	// Create and at Rename, never re-derived from a read. The map key this
	// record sits under is whatever the runtime carried at the last read;
	// Name is what this driver believes it should carry. The two agreeing
	// is the ordinary case; identityDrift is what runs when they do not
	// (colab-fleet #97: a rename that is accepted and then, with no request
	// asking for it, no longer holds).
	Name string `json:"name,omitempty"`

	// Marker and MarkerApplied are colab-fleet #96's fact: whether THIS
	// driver appended marker to Name, recorded at the instant it decided
	// so rather than guessed afterwards from the string. Both empty/false
	// together means either no marker was ever asked for, or this record
	// predates the field — markerStateFor treats that the same as no
	// record at all (markerUnknown), never as a claim of "confirmed
	// absent".
	Marker        string `json:"marker,omitempty"`
	MarkerApplied bool   `json:"markerApplied,omitempty"`

	// Reasserts counts how many times a List has already put Name back
	// after finding the runtime disagreeing with it (colab-fleet #97).
	// Durable, unlike the in-memory futile map Discard uses for a similar
	// bound (tmux.go) — a second actor on the machine that keeps reverting
	// a name is not a fact this service's own restart should forget, so the
	// count that stops the retries must survive one. Bounded by
	// maxNameReasserts (tmux.go): a repair already proven not to hold is
	// not attempted forever.
	Reasserts int `json:"reasserts,omitempty"`

	// NameAssertedAt is when Name was last SET — at a Create or a Rename,
	// never bumped by a reassert attempt. Evidence prose only; nothing in
	// this package branches on it.
	NameAssertedAt time.Time `json:"nameAssertedAt,omitempty"`
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
//
// prior is the caller's own already-loaded snapshot (List loads it once, for
// this and for identityDrift both — colab-fleet #96/#97 — rather than
// reading the store twice per listing).
func (d *Driver) noteSessionSet(rows []paneRow, prior map[string]sessionRecord) {
	if d.store == nil {
		return
	}
	now := d.now()
	next := make(map[string]sessionRecord, len(rows))
	changed := len(prior) != len(rows)
	// Indexed once, for the case below where a row's own key carries no
	// record: the record for its RUN may still exist, filed under a
	// different key (colab-fleet #97 — a rename, or the runtime undoing
	// one, moves what name a session answers to without starting a new
	// run).
	byPaneCreated := indexByPaneCreated(prior)
	for _, r := range rows {
		rec, had := prior[r.session]
		switch {
		case had && rec.Created.IsZero():
			// A stub a Create wrote before this session was ever
			// enumerated (colab-fleet #96/#97: Name/Marker/MarkerApplied
			// were already known then; Created/Pane were not). Fill in
			// what only a live read can supply; keep what Create already
			// asserted.
			rec.Created = r.created
			rec.Cwd = r.cwd
			rec.Pane = r.paneID
			changed = true
		case had && rec.Created.Equal(r.created):
			// Same id, same start time: this is the session we remembered.
			if rec.Pane != r.paneID {
				rec.Pane = r.paneID
				changed = true
			}
		case had:
			// The id is remembered but now holds something that started at
			// a different time: a recycled name (§5.4). Nothing about the
			// old occupant — including any asserted-name fact — describes
			// this one.
			changed = true
			rec = sessionRecord{Created: r.created, Cwd: r.cwd, Pane: r.paneID, FirstSeen: now}
		default:
			// No record under this exact key. Before minting a fresh one,
			// look for the SAME RUN under a DIFFERENT key — see
			// byPaneCreated's own comment.
			if carried, ok := byPaneCreated[paneCreatedOf(r.paneID, r.created)]; ok {
				rec = carried
				rec.Cwd = r.cwd
			} else {
				rec = sessionRecord{Created: r.created, Cwd: r.cwd, Pane: r.paneID, FirstSeen: now}
			}
			changed = true
		}
		rec.LastSeen = now
		next[r.session] = rec
	}
	if !changed {
		return
	}
	d.saveRecords(next)
}

// paneCreated identifies a session RUN rather than a session NAME — the
// same pairing conversationKey already uses (conversation.go), and for the
// identical reason: it survives a rename, where a map keyed by name does
// not. Created is reduced to Unix seconds so it can be a map key; every
// Created this package compares is already parsed from the runtime's own
// integer timestamp (parseRows), so this loses no precision anything here
// relies on.
type paneCreated struct {
	pane    string
	created int64
}

func paneCreatedOf(pane string, created time.Time) paneCreated {
	return paneCreated{pane: pane, created: created.Unix()}
}

// indexByPaneCreated builds a (pane, created) → record lookup over records
// that carry enough to be found this way. A record with an empty Pane
// (written before this field existed, or a stub Create wrote that no List
// has corroborated yet) is omitted rather than indexed under a zero key,
// which would let two unrelated blank records collide.
func indexByPaneCreated(recs map[string]sessionRecord) map[paneCreated]sessionRecord {
	out := make(map[paneCreated]sessionRecord, len(recs))
	for _, rec := range recs {
		if rec.Pane == "" {
			continue
		}
		out[paneCreatedOf(rec.Pane, rec.Created)] = rec
	}
	return out
}

// nameDrift is one live session whose name right now disagrees with the
// name this driver last asserted for it — colab-fleet #97's defect, made
// detectable rather than only measurable after the fact.
type nameDrift struct {
	live paneRow
	want string // the name this driver asserted; always != live.session
	rec  sessionRecord
}

// identityDrift compares what enumerate() just found against what this
// driver last asserted, matching by (pane, created) rather than by name so
// a rename — or a second actor on the machine undoing one — does not make
// the record unfindable (colab-fleet #96/#97). A row with no asserted-name
// record, or one whose asserted name already agrees with what is live, is
// not drift.
func identityDrift(rows []paneRow, prior map[string]sessionRecord) []nameDrift {
	if len(prior) == 0 {
		return nil
	}
	idx := indexByPaneCreated(prior)
	var out []nameDrift
	for _, r := range rows {
		rec, ok := idx[paneCreatedOf(r.paneID, r.created)]
		if !ok || rec.Name == "" || rec.Name == r.session {
			continue
		}
		out = append(out, nameDrift{live: r, want: rec.Name, rec: rec})
	}
	return out
}

// driftSentence is the one wording for "this machine asserted X, the runtime
// now carries Y", used by both channels that report it — SessionState.
// Evidence's prose (§2.3, tmux.go's List) and IdentityAssertion.Evidence
// (colab-fleet #102, identityAssertionFor below) — so the two can never
// disagree about the same read. The wording, and the absence of a leading
// separator, are unchanged from what tmux.go published before #102: List's
// own call site supplies its own "; " so the published evidence string is
// byte-identical to what it was.
func driftSentence(asserted, carried string) string {
	return fmt.Sprintf(
		"this machine last asserted %q for this session and the runtime now carries %q",
		asserted, carried)
}

// heldSentence and uncorroboratedSentence are driftSentence's siblings for
// IdentityAssertion's other two present-but-not-drifted states — colab-fleet
// #102. Neither has a prose precedent to stay byte-identical to; both follow
// driftSentence's voice.
func heldSentence(asserted string) string {
	return fmt.Sprintf("this machine asserted %q for this session and the runtime carries it", asserted)
}

func uncorroboratedSentence(asserted string) string {
	return fmt.Sprintf(
		"this machine asserted %q for this session and no read has yet matched the record to a live run; not a claim the runtime disagrees",
		asserted)
}

// identityAssertionFor builds colab-fleet #102's machine-readable field from
// exactly the facts List already has in hand for this row — no detection of
// its own. byRun is indexByPaneCreated(prior), passed in rather than
// recomputed, because List builds it once per listing; prior is the same map
// identityDrift itself reads, used here only for the uncorroborated stub
// case indexByPaneCreated's own Pane=="" skip excludes from byRun.
//
// The run-match (byRun) is tried first, deliberately: it is the match that
// survives a rename, the entire reason indexByPaneCreated exists, and it
// must never be shadowed by the name-keyed fallback below it.
func identityAssertionFor(r paneRow, prior map[string]sessionRecord, byRun map[paneCreated]sessionRecord) *fleet.IdentityAssertion {
	if rec, ok := byRun[paneCreatedOf(r.paneID, r.created)]; ok && rec.Name != "" {
		if rec.Name == r.session {
			return fleet.IdentityHeld(rec.Name, rec.NameAssertedAt, heldSentence(rec.Name))
		}
		evidence := driftSentence(rec.Name, r.session)
		if rec.Reasserts > 0 {
			evidence += fmt.Sprintf("; this machine has already put that name back %d time(s)", rec.Reasserts)
		}
		return fleet.IdentityDrifted(rec.Name, r.session, rec.NameAssertedAt, evidence)
	}
	// No run match — either nothing was ever asserted for this session, or a
	// Create just wrote a stub (Name set, Pane not yet corroborated by a
	// List). Distinguish those by looking the live name up directly: a stub
	// is keyed under the name Create resolved, same as every other record,
	// until the first List's noteSessionSet fills in Pane/Created.
	if stub, ok := prior[r.session]; ok && stub.Name != "" && stub.Pane == "" {
		return fleet.IdentityUncorroborated(stub.Name, stub.NameAssertedAt, uncorroboratedSentence(stub.Name))
	}
	return nil
}

// markerStateFor answers colab-fleet #96 exactly, when this driver has a
// record to answer from: whether the marker resolveName would apply to
// name is already there because THIS driver put it there — never guessed
// from the string. markerUnknown (naming.go's zero value) is the honest
// answer whenever there is nothing to go on: no state store, no marker
// asked for, no record under this exact string, or a record made for a
// different marker.
func (d *Driver) markerStateFor(name, marker string) markerState {
	if d.store == nil || marker == "" {
		return markerUnknown
	}
	rec, ok := d.loadRecords()[name]
	if !ok || rec.Marker != marker {
		return markerUnknown
	}
	if rec.MarkerApplied {
		return markerApplied
	}
	return markerAbsent
}

// identityAssertionForCreate answers colab-fleet #102 for every Create-family
// return — a fresh create, an idempotent replay of one already completed, or
// an adopted pending recovery. None of the three enumerate the runtime live,
// so none may claim more than "asserted, not yet corroborated": that
// stronger claim (held or drifted) is List's alone to make, once an actual
// read has matched this record against a live run (identityAssertionFor,
// above). Reads the durable record rather than reconstructing the values
// from the caller's own arguments, for the same "one fact, not two that can
// drift" reason noteCreateRecord's own comment already states for
// pins/surface/prompt.
func (d *Driver) identityAssertionForCreate(name string) *fleet.IdentityAssertion {
	if d.store == nil {
		return nil
	}
	rec, ok := d.loadRecords()[name]
	if !ok || rec.Name == "" {
		return nil
	}
	return fleet.IdentityUncorroborated(rec.Name, rec.NameAssertedAt, uncorroboratedSentence(rec.Name))
}

// noteAssertedName records the identity a Create just resolved — colab-fleet
// #96's marker fact, and the first half of #97's durable record (the second
// half is a Rename actually changing it; see noteRenamed). Merged into
// whatever the store already holds under key, so a later List's own
// Created/Pane/LastSeen fields (noteSessionSet, above) never race this and
// clobber it, or vice versa.
func (d *Driver) noteAssertedName(key, cwd, name, marker string, applied bool) {
	if d.store == nil {
		return
	}
	recs := d.loadRecords()
	rec := recs[key] // zero value if this is the session's first record
	now := d.now()
	rec.Cwd = cwd
	rec.Name = name
	rec.Marker = marker
	rec.MarkerApplied = applied
	rec.Reasserts = 0
	rec.NameAssertedAt = now
	if rec.FirstSeen.IsZero() {
		rec.FirstSeen = now
	}
	rec.LastSeen = now
	recs[key] = rec
	d.saveRecords(recs)
}

// noteRenamed writes colab-fleet #97's durable half: a Rename this driver
// just issued, moved from the record's old key to the new one so the next
// read finds it either way — under the new key if the rename held, or via
// identityDrift's (pane, created) match if a second actor on the machine
// put the old name back.
//
// Marker/MarkerApplied are cleared rather than carried across: the caller
// dictated the whole new string, so whatever this driver knew about the OLD
// name's marker is not a fact about this one.
func (d *Driver) noteRenamed(from, to, cwd, pane string, created time.Time) {
	if d.store == nil {
		return
	}
	recs := d.loadRecords()
	rec := recs[from]
	delete(recs, from)
	now := d.now()
	rec.Cwd = cwd
	rec.Pane = pane
	rec.Created = created
	rec.Name = to
	rec.Marker = ""
	rec.MarkerApplied = false
	rec.Reasserts = 0
	rec.NameAssertedAt = now
	if rec.FirstSeen.IsZero() {
		rec.FirstSeen = now
	}
	rec.LastSeen = now
	recs[to] = rec
	d.saveRecords(recs)
}

// recordReassertAttempt persists what reassertNames just did for one drift
// entry — durable, because a second actor that keeps reverting a name must
// not get a fresh budget of attempts every time this service restarts.
// fromKey is the live name the drift was detected against; on success the
// record moves to the name that is now live (cur.Name), the same shape
// noteRenamed already uses.
func (d *Driver) recordReassertAttempt(fromKey string, fallback sessionRecord, succeeded bool) {
	if d.store == nil {
		return
	}
	recs := d.loadRecords()
	cur, ok := recs[fromKey]
	if !ok {
		cur = fallback
	}
	cur.Reasserts++
	cur.LastSeen = d.now()
	if succeeded {
		delete(recs, fromKey)
		recs[cur.Name] = cur
	} else {
		recs[fromKey] = cur
	}
	d.saveRecords(recs)
}

// recordContested persists that reassertNames will not try again for this
// record: either the wanted name is itself live under a different session
// right now, or maxNameReasserts is already spent. Setting Reasserts to the
// bound — not merely leaving its last value — means a restart does not
// re-earn attempts already proven futile, same durability reasoning as
// recordReassertAttempt.
func (d *Driver) recordContested(key string, fallback sessionRecord) {
	if d.store == nil {
		return
	}
	recs := d.loadRecords()
	cur, ok := recs[key]
	if !ok {
		cur = fallback
	}
	cur.Reasserts = maxNameReasserts
	cur.LastSeen = d.now()
	recs[key] = cur
	d.saveRecords(recs)
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
