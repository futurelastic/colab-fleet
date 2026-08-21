package tmux

import (
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Remembering what a create asked the runtime to resume (colab-fleet #72),
// so a later listing can say whether it actually happened.
//
// # Why this has to be remembered at all
//
// spec.Resume is an argv value: once Create has built the command line, it
// is gone from anything this driver still holds unless something writes it
// down. Without a durable note of what was asked for, there is nothing to
// compare a session's ACTUAL resolved conversation (conversation.go) against,
// and "did the resume take" collapses into "what conversation does this
// session have now" — which is exactly the silent downgrade #72 measured.
//
// # Same shape as stranded, for the same reasons
//
// Durable when a state store is configured, in memory otherwise (honest for
// a throwaway instance, the same call noteStranded already makes); keyed on
// the session id with the cwd carried alongside for corroboration (§5.4: an
// id alone is recyclable, and a durable record can outlive the session it
// was written for); swept by age rather than kept forever, because a record
// nobody has checked in resumeIntentRetention is not evidence about a
// caller who is still watching, the same reasoning idemStore's sweep and
// strandedRetention already state.

// resumeIntentRetention bounds how long this driver remembers a create's
// resume request. A session's own conversation ordinarily resolves within
// moments of creation (conversation.go's derive runs on every listing), so
// this only has to survive long enough for a caller to check — not forever.
const resumeIntentRetention = 30 * time.Minute

// resumeIntentRecord is what noteResumeIntent persists: the conversation id
// a create asked the runtime to resume, and enough beside it (§5.4) to tell
// a live session from one that merely recycled the same name.
type resumeIntentRecord struct {
	Requested string    `json:"requested"`
	Cwd       string    `json:"cwd"`
	At        time.Time `json:"at"`
}

// resumeIntentFile is the durable document, one entry per session with a
// resume request still worth checking. Its own file, never folded into
// idempotency.json — a create key and a resume-honesty check are different
// concerns with different shapes, the same split stranded's own file makes.
type resumeIntentFile struct {
	Records map[string]resumeIntentRecord `json:"records"`
}

const resumeIntentFileName = "resume-intent"

// noteResumeIntent records that a create asked the runtime to resume a
// conversation, before there is any way to tell whether it worked. Called
// only when spec.Resume is set.
func (d *Driver) noteResumeIntent(id, cwd, requested string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.resumeIntents == nil {
		d.resumeIntents = map[string]resumeIntentRecord{}
	}
	d.resumeIntents[id] = resumeIntentRecord{Requested: requested, Cwd: cwd, At: d.now()}
	d.saveResumeIntentsLocked()
}

// resumeIntentFor reports what a session's create asked to resume, if
// anything and if the record has not expired or been made for a different
// working directory (§5.4 again: an id match alone is not enough).
func (d *Driver) resumeIntentFor(id, cwd string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepResumeIntentsLocked()
	rec, ok := d.resumeIntents[id]
	if !ok || rec.Cwd != cwd {
		return "", false
	}
	return rec.Requested, true
}

// sweepResumeIntentsLocked drops records older than resumeIntentRetention.
// Caller holds d.mu.
func (d *Driver) sweepResumeIntentsLocked() {
	if len(d.resumeIntents) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.resumeIntents {
		if now.Sub(rec.At) > resumeIntentRetention {
			delete(d.resumeIntents, id)
		}
	}
}

func (d *Driver) saveResumeIntentsLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(resumeIntentFileName, resumeIntentFile{Records: d.resumeIntents})
}

// loadResumeIntents restores resume-intent records at startup, sweeping
// anything already past resumeIntentRetention — the same "sweep on load"
// shape idemStore and loadStranded already use.
func (d *Driver) loadResumeIntents() {
	if d.store == nil {
		return
	}
	var f resumeIntentFile
	found, err := d.store.Load(resumeIntentFileName, &f)
	if err != nil || !found || len(f.Records) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resumeIntents = f.Records
	d.sweepResumeIntentsLocked()
	d.saveResumeIntentsLocked()
}

// resumeOutcomeFor turns a requested resume id and a session's resolved
// Conversation into a fleet.ResumeOutcome. conv may be nil (nobody looked)
// or Known false (looked, could not tell yet) — both are "too early to
// say", never read as a "no" (§5.7).
func resumeOutcomeFor(requested string, conv *fleet.ConversationRef) *fleet.ResumeOutcome {
	if conv == nil || !conv.Known {
		evidence := "the session's own conversation has not resolved yet"
		if conv != nil {
			evidence = conv.Evidence
		}
		return fleet.ResumeUnresolved(requested, evidence)
	}
	if conv.ID == requested {
		return fleet.ResumeResolved(requested, true,
			"the session's own conversation record is the one creation asked to resume")
	}
	return fleet.ResumeResolved(requested, false, "creation asked to resume "+requested+
		"; the session's own conversation record is "+conv.ID+" instead — the runtime "+
		"started a fresh conversation rather than honouring the request")
}
