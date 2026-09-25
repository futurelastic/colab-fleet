package tmux

import (
	"context"
	"fmt"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Identifying a conversation from the runtime's own per-process record (#182).
//
// # The gap this closes, measured
//
// The name-and-date derivation in conversation.go is correct for a session
// that began a conversation and wrong for one that CONTINUED one. A resumed
// session continues a record that began before the session existed, and the
// rule that eliminates records older than the session eliminates exactly that
// one — measured on a live fleet, 18 of 75 sessions read `known: false` with
// "all N records carrying this session's name were created before this session
// existed, so none of them can be its own". Nearly all were resumed sessions.
// One more read `ambiguous`.
//
// # What replaces the elimination, and what does not
//
// The runtime keeps one small record per running process, filed under the
// process's pid, that names the conversation the process is in. That is the
// runtime stating its own current conversation rather than this service
// inferring it from a title, so it needs no date rule and it does not care
// whether the session was created with a name at all. It is the same record
// terminal path v2 already reads to confirm a delivery (terminalpath2_transcript.go);
// this file reuses that reader, the working-directory check and the
// start-time corroboration rather than making a second resolver beside it.
//
// It is corroborated the same way and for the same reason: the record's own
// start time must equal the start time of the process running under that pid
// NOW. A pid is a number the kernel recycles, and a record a dead process left
// behind is exactly as well-formed as a live one. Without that check a reused
// pid would hand a session somebody else's conversation, confidently, which is
// the failure this whole field exists to prevent.
//
// What is kept, unchanged, is the name-and-date derivation. It is the fallback
// when no record can be used, and it is also asked when a record CAN be used,
// because two independent sources that disagree are a finding to report, not a
// tie to break. Neither is ever silently preferred over the other.
//
// # Why the wire label stays "derived"
//
// ConversationSource is a closed set decoded strictly, and this service
// federates between builds of different vintages: a member an older peer does
// not know does not degrade one field on that peer, it fails the whole session
// read. So a per-process answer is labelled "derived" like every other answer
// this driver matches rather than is told, and the EVIDENCE names which rule
// answered. A distinct label is a later change that has to land on every peer's
// decoder first.

// liveConversation is what the per-process record said about one session's
// process. id is set exactly when the record was usable AND its start time
// corroborated the live process; otherwise it is empty and evidence says why
// the record was not used.
type liveConversation struct {
	id       string
	evidence string
	// startedAt is the start time of the process the record was corroborated
	// against, set exactly when id is (#202). The store keeps it beside the
	// answer so a later read can tell a recycled pid from the same process
	// without asking the OS again.
	startedAt time.Time
}

// liveConversationSource asks the per-process record. It is handed to the
// store as a function rather than a value because the answer costs a file read
// and an OS query, and the store answers most lookups from its cache without
// wanting either.
type liveConversationSource func() liveConversation

// liveConversationSource builds the per-process source for one session, or nil
// when this driver was never pointed at the runtime's per-process records — in
// which case a lookup is exactly the name-and-date derivation it always was.
//
// The record is read first and the OS is asked only if there is a record worth
// corroborating. A fleet's listing is dominated by such questions; a session
// whose runtime wrote no record costs one failed file open and no `ps`.
func (d *Driver) liveConversationSource(ctx context.Context, pid int, cwd string) liveConversationSource {
	if d.processSessionsRoot == "" {
		return nil
	}
	return func() liveConversation {
		if pid <= 0 {
			return liveConversation{evidence: "the multiplexer reported no usable pid for this session"}
		}
		rec, why := d.processRecordFor(pid, cwd)
		if why != "" {
			return liveConversation{evidence: why}
		}
		started, err := d.processStartedAt(ctx, pid)
		if err != nil {
			return liveConversation{evidence: fmt.Sprintf("the start time of the process now running under pid %d could not be read, "+
				"so the per-process record could not be corroborated", pid)}
		}
		live, why := corroborateProcessRecord(rec, ProcessIdentity{PID: pid, StartedAt: started})
		if why != "" {
			return liveConversation{evidence: why}
		}
		return liveConversation{id: live.sessionID, evidence: live.evidence}
	}
}

// liveConversationFrom is liveConversationSource for a caller that has ALREADY
// resolved and corroborated the per-process record for this session — the
// transcript path does, in resolveLiveProcessSessionID, before it asks for the
// conversation. Handing that answer through, rather than asking again, is one
// `ps` fewer on a cache miss and, more to the point, one answer instead of two:
// the send and the listing must never disagree about what the record said.
//
// nil when no per-process root is configured, so an unconfigured driver's
// lookup stays exactly the name-and-date derivation.
func (d *Driver) liveConversationFrom(live liveProcessIdentity, ok bool) liveConversationSource {
	if d.processSessionsRoot == "" {
		return nil
	}
	return func() liveConversation {
		if !ok {
			return liveConversation{evidence: "the per-process record could not be used to identify this session's process"}
		}
		return liveConversation{id: live.sessionID, evidence: live.evidence, startedAt: live.process.startedAt}
	}
}

// reconcileConversation combines what the name-and-date derivation found with
// what the per-process record said. derived is never nil: derive always
// answers, resolved or not.
func reconcileConversation(derived *fleet.ConversationRef, live liveConversation) *fleet.ConversationRef {
	if live.id == "" {
		// No record to go on. Fall back to the derivation exactly as it
		// answered, and say why the record was not used — a reader looking at
		// an unknown session is entitled to know both sources were asked.
		if derived.Known {
			return fleet.ResolvedConversation(derived.ID, derived.Source,
				derived.Evidence+"; the runtime's per-process record was not used: "+live.evidence)
		}
		return fleet.UnresolvedConversation(
			derived.Evidence + "; the runtime's per-process record could not be used either: " + live.evidence)
	}
	switch {
	case !derived.Known:
		return fleet.ResolvedConversation(live.id, fleet.ConversationDerived,
			"per-process record, start time corroborated ("+live.evidence+"); the name-based derivation could not answer: "+
				derived.Evidence)
	case derived.ID == live.id:
		return fleet.ResolvedConversation(live.id, fleet.ConversationDerived,
			"per-process record, start time corroborated ("+live.evidence+"); the name-based derivation names the same conversation")
	default:
		// Two independent sources, two different answers. Choosing either
		// would be a guess shaped like a reading, so name both and choose
		// neither.
		return fleet.UnresolvedConversation(fmt.Sprintf(
			"two sources name different conversations and neither is chosen: the runtime's per-process record "+
				"(start time corroborated) names %s, while the record carrying this session's name is %s",
			live.id, derived.ID))
	}
}
