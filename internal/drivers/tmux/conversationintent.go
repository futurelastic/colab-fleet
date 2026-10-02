package tmux

import (
	"fmt"
	"time"

	fleet "github.com/futurelastic/muster"
)

// Remembering what a create asked the runtime to START as a new conversation
// (muster #224) — the mirror of resumeintent.go's own note, for the
// mirror question: resumeIntents lets a later List say whether a REQUESTED
// RESUME was honoured; this lets one say whether a REQUESTED ID was.
//
// # Why the create's own answer is not enough by itself
//
// The `201` this driver returns for a captured id (see conversationCapturedRef)
// is built from what THIS create asked the runtime to do, before the runtime
// has necessarily written anything durable — conversation.go's own derivation
// needs a record on disk, which for a session created moments ago may not
// exist yet. Without a note surviving past the create call, every List
// between the 201 and the record landing would report Conversation absent
// (nobody has looked successfully yet) or unresolved — a caller-visible
// regression from what the create itself already reported, and the exact
// shape of defect #72 fixed for resume, arriving here from a different
// direction: not a downgrade to a wrong answer, but a downgrade to no answer.
//
// # Why a disagreement must still be found
//
// A caller-chosen id is a REQUEST like resume, not a guarantee: a runtime
// with no understanding of the launch flag this driver passes might start a
// fresh conversation under an id of its own choosing anyway. Reporting the
// requested id forever, with no way to notice that, would repeat #72's
// silent-downgrade defect in the opposite direction — a caller believing its
// request took effect purely because nothing ever said otherwise (§5.7).
//
// Retention, shape and persistence mirror resumeIntentRecord exactly, for the
// same reasons; see resumeintent.go's own note.

// conversationIntentRetention bounds how long this driver remembers a
// create's requested id. A session's own conversation ordinarily resolves
// within moments of creation (conversation.go's derive runs on every
// listing), so this only has to survive long enough for a caller to check —
// not forever. Same value as resumeIntentRetention, for the same reason.
const conversationIntentRetention = 30 * time.Minute

// conversationIntentRecord is what noteConversationIntent persists: the
// conversation id a create asked the runtime to START, and enough beside it
// (§5.4) to tell a live session from one that merely recycled the same name.
type conversationIntentRecord struct {
	Requested string    `json:"requested"`
	Cwd       string    `json:"cwd"`
	At        time.Time `json:"at"`
}

// conversationIntentFile is the durable document, one entry per session with
// a requested id still worth checking. Its own file, never folded into
// resume-intent or idempotency.json — each is a different concern with a
// different shape, the same split stranded's own file makes.
type conversationIntentFile struct {
	Records map[string]conversationIntentRecord `json:"records"`
}

const conversationIntentFileName = "conversation-intent"

// noteConversationIntent records that a create asked the runtime to start a
// conversation under a caller-chosen id, before there is any way to tell
// whether the runtime agreed. Called only when spec.ConversationId is set.
func (d *Driver) noteConversationIntent(id, cwd, requested string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conversationIntents == nil {
		d.conversationIntents = map[string]conversationIntentRecord{}
	}
	d.conversationIntents[id] = conversationIntentRecord{Requested: requested, Cwd: cwd, At: d.now()}
	d.saveConversationIntentsLocked()
}

// conversationIntentFor reports what a session's create asked to start,
// if anything and if the record has not expired or been made for a
// different working directory (§5.4 again: an id match alone is not enough).
func (d *Driver) conversationIntentFor(id, cwd string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepConversationIntentsLocked()
	rec, ok := d.conversationIntents[id]
	if !ok || rec.Cwd != cwd {
		return "", false
	}
	return rec.Requested, true
}

// sweepConversationIntentsLocked drops records older than
// conversationIntentRetention. Caller holds d.mu.
func (d *Driver) sweepConversationIntentsLocked() {
	if len(d.conversationIntents) == 0 {
		return
	}
	now := d.now()
	for id, rec := range d.conversationIntents {
		if now.Sub(rec.At) > conversationIntentRetention {
			delete(d.conversationIntents, id)
		}
	}
}

func (d *Driver) saveConversationIntentsLocked() {
	if d.store == nil {
		return
	}
	_ = d.store.Save(conversationIntentFileName, conversationIntentFile{Records: d.conversationIntents})
}

// loadConversationIntents restores conversation-intent records at startup,
// sweeping anything already past conversationIntentRetention — the same
// "sweep on load" shape idemStore, loadStranded and loadResumeIntents already
// use.
func (d *Driver) loadConversationIntents() {
	if d.store == nil {
		return
	}
	var f conversationIntentFile
	found, err := d.store.Load(conversationIntentFileName, &f)
	if err != nil || !found || len(f.Records) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conversationIntents = f.Records
	d.sweepConversationIntentsLocked()
	d.saveConversationIntentsLocked()
}

// conversationCapturedRef is the ConversationRef a create carrying
// ConversationId reports before anything the runtime wrote can be read back —
// used at every Create return site that answers for such a create (the main
// success path and both idempotent-replay branches), so all three report the
// SAME fact the same way rather than three hand-written literals drifting
// apart. Returns nil for an ordinary create — the fact ConversationRef's own
// MarshalJSON already relies on: a nil field is genuinely absent on the wire,
// never a captured value with an empty id.
func conversationCapturedRef(id string) *fleet.ConversationRef {
	if id == "" {
		return nil
	}
	return fleet.ResolvedConversation(id, fleet.ConversationCaptured,
		"the id this create asked the runtime to start a new conversation under")
}

// conversationIntentOutcome reconciles a create's requested id against what
// this session's own conversation actually resolved to (resolved may be nil —
// nobody looked — or Known false — looked, could not tell yet; conv.go's
// lookup answers both). It is called on every List/State read for a session
// with a live intent, which is why it must keep answering the captured value
// rather than falling back to "nobody looked" the moment the create's own
// direct knowledge is no longer the freshest thing being asked — see this
// file's own package doc for why that fallback would itself be the #72
// defect, arriving from the opposite direction.
func conversationIntentOutcome(requested string, resolved *fleet.ConversationRef) *fleet.ConversationRef {
	if resolved == nil || !resolved.Known {
		evidence := "the runtime has not yet written a record this driver can read"
		if resolved != nil {
			evidence = resolved.Evidence
		}
		return fleet.ResolvedConversation(requested, fleet.ConversationCaptured,
			"the id this create asked the runtime to start a new conversation under; "+evidence)
	}
	if resolved.ID == requested {
		return resolved
	}
	// Two independent facts about the same session disagree: the id this
	// create asked for, and the id the runtime's own record actually names.
	// Named loudly, never silently overwritten — the exact invariant #202
	// already holds for a replaced process's stale conversation, applied
	// here to a requested id instead of a prior one.
	return fleet.UnresolvedConversation(fmt.Sprintf(
		"this session's create asked the runtime to start conversation %s under a caller-chosen id; "+
			"the runtime's own record instead names %s, so the request was not honoured",
		requested, resolved.ID))
}
