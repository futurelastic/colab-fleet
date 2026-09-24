package tmux

import (
	"time"
)

// The draft rule (#180): never clear or submit text sitting in a session's
// composer unless
//
//	(a) this driver's own record proves it is the driver's own stranded text, or
//	(b) the caller passes that composer's current digest, proving it saw it.
//
// Otherwise refuse and keep it — it may be a person's draft. A flag on the
// request (resumeIfStranded, replaceIfStranded) is a wish, never proof.
//
// (a) has two forms. The live stranded record, while it lasts; and, once it
// has lapsed or been replaced, a TOMBSTONE of it: the same text and digest,
// kept longer and never used for anything but this proof. The tombstone is
// what keeps #135 working — a retry that outlives the live record's
// retention still finds the composer holding the driver's own text and can
// clear it — without the unconditional clear #135 once meant (#180 M2).

const (
	// tombstoneRetention bounds how long a lapsed record still proves the
	// composer's text is this driver's own.
	tombstoneRetention = 24 * time.Hour
	// tombstonesPerSession bounds how many a session keeps: the newest win.
	tombstonesPerSession = 8
)

// strandedTombstone is what is left of a stranded record once it lapsed or
// was replaced by a newer strand for the same session. Never written for a
// record forgotten because its text was submitted or discarded — that text
// is gone, and a tombstone for it could only ever match somebody else's.
type strandedTombstone struct {
	Text           string    `json:"text"`
	Cwd            string    `json:"cwd"`
	ComposerDigest string    `json:"composerDigest,omitempty"`
	At             time.Time `json:"at"`
}

// buryLocked records rec as a tombstone for id. d.mu must be held.
func (d *Driver) buryLocked(id string, rec strandedRecord) {
	if d.tombstones == nil {
		d.tombstones = map[string][]strandedTombstone{}
	}
	list := append(d.tombstones[id], strandedTombstone{
		Text: rec.Text, Cwd: rec.Cwd, ComposerDigest: rec.ComposerDigest, At: d.now(),
	})
	if len(list) > tombstonesPerSession {
		list = list[len(list)-tombstonesPerSession:]
	}
	d.tombstones[id] = list
}

// sweepTombstonesLocked drops tombstones past their retention. d.mu must be
// held.
func (d *Driver) sweepTombstonesLocked() {
	now := d.now()
	for id, list := range d.tombstones {
		kept := list[:0]
		for _, t := range list {
			if now.Sub(t.At) <= tombstoneRetention {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(d.tombstones, id)
		} else {
			d.tombstones[id] = kept
		}
	}
}

// tombstoneProves reports whether one of this driver's tombstones for the
// session proves the composer's current content is the driver's own text:
// the same composer digest it recorded, or the composer rendering exactly
// the text it recorded.
func (d *Driver) tombstoneProves(id, cwd, pending string, sc screen) bool {
	d.mu.Lock()
	d.sweepTombstonesLocked()
	list := append([]strandedTombstone(nil), d.tombstones[id]...)
	d.mu.Unlock()
	for _, t := range list {
		if t.Cwd != cwd {
			continue
		}
		if composerDigestMatches(t.ComposerDigest, pending) || composerMatchesText(sc, t.Text, false) {
			return true
		}
	}
	return false
}

// draftProof is which half of the draft rule allowed touching a composer.
type draftProof string

const (
	proofNone      draftProof = ""
	proofRecord    draftProof = "this driver's own stranded record"
	proofTombstone draftProof = "this driver's own record of a strand that has since lapsed"
	proofExpect    draftProof = "the caller's expect digest, matching the composer as it is now"
)

// expectProves reports whether the caller's expect digest matches the
// composer's current content.
func expectProves(expect, pending string) bool {
	return expect != "" && composerDigestMatches(expect, pending)
}

// mayClearUnrecorded applies the draft rule to a composer this driver holds
// no live record for. mismatch is true when the caller passed an expect digest
// that does not match — worth saying, because it means the composer changed
// since the caller looked.
func (d *Driver) mayClearUnrecorded(id, cwd, pending string, sc screen, expect string) (proof draftProof, mismatch bool) {
	if expectProves(expect, pending) {
		return proofExpect, false
	}
	if d.tombstoneProves(id, cwd, pending, sc) {
		return proofTombstone, false
	}
	return proofNone, expect != ""
}

// recordProves reports whether the live stranded record proves the
// composer's current content is exactly the text it recorded: its digest
// still matches, or the composer renders the recorded text itself.
func recordProves(rec strandedRecord, pending string, sc screen) bool {
	return composerDigestMatches(rec.ComposerDigest, pending) || composerMatchesText(sc, rec.Text, false)
}
