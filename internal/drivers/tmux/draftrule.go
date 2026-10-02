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

	// Provisional (#240) marks the one tombstone that is NOT left by a lapsed
	// record: it is left by a delivery whose submit was confirmed by evidence
	// that does not show a turn started (the runtime queueing the text, or the
	// composer reading empty). Such text is not certainly gone — a runtime that
	// queued it can hand it back to the composer, unsent, with nothing in this
	// driver's memory to say whose it is. The entry is the memory.
	//
	// It proves the composer's text is this driver's own under the same two
	// tests as any tombstone, with one more condition: when a transcript was
	// resolved, it must not show a user turn carrying the text since TranscriptOffset
	// (see tombstoneProves) — text the runtime ran is gone, and a person
	// recalling it from history is not this driver's delivery.
	Provisional      bool   `json:"provisional,omitempty"`
	TranscriptPath   string `json:"transcriptPath,omitempty"`
	TranscriptOffset int64  `json:"transcriptOffset,omitempty"`
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
	ok, _ := d.tombstoneProvesKind(id, cwd, pending, sc)
	return ok
}

// tombstoneProvesKind is tombstoneProves plus whether the proof was a
// provisional entry (#240) — a delivery confirmed only weakly, whose text has
// since come back to the composer.
func (d *Driver) tombstoneProvesKind(id, cwd, pending string, sc screen) (proved, provisional bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tombstoneProvesKindLocked(id, cwd, pending, sc)
}

// tombstoneProvesKindLocked is tombstoneProvesKind for a caller already
// holding d.mu (List and State read state under it).
func (d *Driver) tombstoneProvesKindLocked(id, cwd, pending string, sc screen) (proved, provisional bool) {
	d.sweepTombstonesLocked()
	for _, t := range d.tombstones[id] {
		if t.Cwd != cwd {
			continue
		}
		// A provisional digest is empty by construction: the composer was empty
		// when it was made. composerDigestMatches never matches an empty digest
		// against text, so such an entry proves by its text alone.
		if !(composerDigestMatches(t.ComposerDigest, pending) || composerMatchesText(sc, t.Text, false)) {
			continue
		}
		if t.Provisional && t.TranscriptPath != "" &&
			transcriptUserTurnSince(t.TranscriptPath, t.TranscriptOffset, t.Text) {
			// The runtime ran this text. What the composer holds now is the same
			// words typed or recalled again, and nothing here says whose.
			continue
		}
		return true, t.Provisional
	}
	return false, false
}

// ownDeliveryLocked reports whether this driver's own memory proves the text
// in this session's composer is a message it delivered (#240): the live
// stranded record, or a tombstone — provisional or lapsed. It is the
// read-side twin of the draft rule's proof (a), for a caller that wants to SAY
// so (SessionState.StrandedDelivery) rather than act on it. d.mu must be held.
func (d *Driver) ownDeliveryLocked(id, cwd string, c paneCapture, captured bool) bool {
	if !captured {
		return false
	}
	sc := c.screen()
	pending, scan := composerText(sc)
	if scan != composerFound || pending == "" {
		return false
	}
	d.sweepStrandedLocked()
	if rec, ok := d.stranded[id]; ok && rec.Cwd == cwd && recordProves(rec, pending, sc) {
		return true
	}
	ok, _ := d.tombstoneProvesKindLocked(id, cwd, pending, sc)
	return ok
}

// noteProvisional records a delivery whose submit was confirmed without proof
// that a turn started (#240). See strandedTombstone.Provisional. src/srcOK is
// the transcript resolved BEFORE the submit keystroke, as at every other
// submit site; an unresolved one leaves the entry without its "was it run"
// check, which makes it proof for the draft rule on the composer's words
// alone — the same strength as a lapsed record's tombstone.
func (d *Driver) noteProvisional(id, cwd, text string, src transcriptSource, srcOK bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tombstones == nil {
		d.tombstones = map[string][]strandedTombstone{}
	}
	t := strandedTombstone{Text: text, Cwd: cwd, At: d.now(), Provisional: true}
	if srcOK {
		t.TranscriptPath, t.TranscriptOffset = src.path, src.offset
	}
	// One entry per (cwd, text): a repeat of the same message moves the entry
	// forward instead of crowding earlier tombstones out.
	list := make([]strandedTombstone, 0, len(d.tombstones[id])+1)
	for _, old := range d.tombstones[id] {
		if old.Provisional && old.Cwd == cwd && old.Text == text {
			continue
		}
		list = append(list, old)
	}
	list = append(list, t)
	if len(list) > tombstonesPerSession {
		list = list[len(list)-tombstonesPerSession:]
	}
	d.tombstones[id] = list
	d.counters.incr(counterStrandedProvisionalKept)
	d.saveStrandedLocked()
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
