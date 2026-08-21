package tmux

import (
	"encoding/json"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Reading the runtime's own record for WHY a control channel failed (#69) —
// the record-side counterpart to controlchannel.go's footer read.
//
// # Why this exists, and why it waited
//
// controlchannel.go's footer gives a closed-set STATE — active, connecting,
// reconnecting, failed — read from chrome an agent cannot write into. It
// never carried a REASON, because the runtime's disconnection notice prints
// into the transcript, and the transcript is the forgeable region: the
// incident that motivated the footer-only rule was a supervisor grepping
// panes for that exact notice and classifying itself as disconnected because
// its own tool output contained the string it was searching for.
//
// Measured since (#69): the same notice also reaches the runtime's own
// durable record, structured rather than rendered —
// `{"type":"system","subtype":"informational","content":"Remote Control
// disconnected …"}` — which is the same class of artefact runtimerecord.go
// already reads for refusals (#56).
//
// # The filter is the safety property
//
// A substring search over the record is unsound: the store has at least one
// hit that is a USER-role entry holding captured command output, a region an
// agent's own actions populate. So Type and Subtype are checked BEFORE
// Content is ever compared against the phrase — that ordering is the whole
// property, exactly parallel to #56 keying on `isApiErrorMessage` rather
// than on message text. A reader that matched the phrase across entry types
// would reintroduce forgeability through a different door than the one the
// footer rule closed.
//
// # What is still NOT done here
//
// No transient/terminal classification. controlchannel.go's own comment
// already declined that inference once (#65) — the close codes are read out
// of the runtime binary and recorded there, but which ones are retryable was
// never measured. This file carries the runtime's own sentence, code number
// included when the runtime put one in it, and stops: the caller decides.

// controlDisconnectPhrase anchors the runtime's own disconnection notice.
// Matching this alone would not be safe — the type/subtype check in
// latestControlDisconnect is the actual safety property. The phrase only
// narrows a match, among everything else a system/informational entry can
// carry, to the ones that are actually about a disconnection.
const controlDisconnectPhrase = "Remote Control disconnected"

// controlDisconnectRecordEntry is the subset of one JSONL line this driver
// reads to decide whether it is the runtime's own disconnection notice.
//
// Content is decoded as a bare top-level string deliberately: only a
// `system`/`informational` entry is shaped that way in this store. A
// `user`-role entry carrying the same phrase inside captured command output
// nests it several levels down (`message.content[].content`), which does
// not unmarshal into this field at all — so on that entry Content decodes
// empty and the Type check below rejects it before Content is even
// considered. Failing to populate Content is therefore itself evidence the
// line is not one of these, not a reason to try some other shape.
type controlDisconnectRecordEntry struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// controlDisconnectFact is what the record's own words say about why a
// control channel failed.
type controlDisconnectFact struct {
	// text is the runtime's own sentence, in full.
	text string
	// at is the notice's own timestamp, as the runtime wrote it.
	at time.Time
}

// reasonText bounds the fact's text for display — a length cap only, never a
// sentence cut. apiErrorFact.reasonSentence() cuts at the first period
// because the load-bearing clause of an API error is reliably the first one;
// this notice does not have that shape — controlchannel.go's own doc comment
// quotes two real forms ("Couldn't reconnect … Retry, or start a fresh
// session without --resume" and "this session was ended or archived from
// another device or app (code 4090)"), and in each the detail that matters —
// the retry instruction, the close code — sits after where a sentence cut
// would land. Cutting there would silently throw away the one thing #69
// asked this field to carry.
func (f controlDisconnectFact) reasonText() string {
	const maxReasonBytes = 300
	s := strings.TrimSpace(f.text)
	if len(s) > maxReasonBytes {
		s = s[:maxReasonBytes]
	}
	return s
}

// latestControlDisconnect reads the tail of one runtime record and reports
// the most recent runtime-written disconnection notice it can find — the
// record-side counterpart to controlChannelOf's footer read.
//
// Walks backward like latestAPIError (#56): the answer sought is always the
// most recent matching line, and in practice that is within the first few
// lines scanned. Unlike latestAPIError, this does not stop at the first
// entry of any type it meets — it skips past everything that is not itself
// a matching system/informational entry, because there is no "history
// superseded by this one" hazard to guard against here: this function is not
// deciding a session's CURRENT state (controlChannelOf's footer read already
// did that), only explaining a state already decided.
func latestControlDisconnect(path string) (controlDisconnectFact, bool) {
	lines, ok := recordTail(path)
	if !ok {
		return controlDisconnectFact{}, false
	}

	inspected := 0
	for i := len(lines) - 1; i >= 0 && inspected < recordTailCandidates; i-- {
		inspected++
		var entry controlDisconnectRecordEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			// A torn or half-written line is not a reason to give up on the
			// ones before it (the same allowance latestAPIError and
			// conversation.go's readRecordEntry already make).
			continue
		}
		// The region check, ahead of the content check on purpose: an entry
		// that is not the runtime's own system/informational kind is
		// skipped before its Content is even compared against the phrase.
		// This ordering is the safety property #69 measured the need for.
		if entry.Type != "system" || entry.Subtype != "informational" {
			continue
		}
		if !strings.Contains(entry.Content, controlDisconnectPhrase) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			// Found the right entry but not a fact we can act on: honestly
			// unresolvable rather than a guessed time (the same call
			// latestAPIError makes for the same shape of gap).
			return controlDisconnectFact{}, false
		}
		return controlDisconnectFact{text: entry.Content, at: ts}, true
	}
	return controlDisconnectFact{}, false
}

// controlReasonFor asks the runtime's own record why one session's control
// channel failed — reusing its already-resolved Conversation the way
// recordFactFor does for #56, rather than looking one up again. Returns
// false whenever no record store is configured, this session's conversation
// could not be matched, or the record has no matching entry; every caller
// must leave ControlChannel.Reason empty in exactly those cases, never a
// guess.
func (d *Driver) controlReasonFor(s fleet.Session) (controlDisconnectFact, bool) {
	if d.conversations == nil || s.Conversation == nil || !s.Conversation.Known {
		return controlDisconnectFact{}, false
	}
	return latestControlDisconnect(d.conversations.recordPath(string(s.Cwd), s.Conversation.ID))
}

// upgradeControlChannelFromRecord applies #69's Reason upgrade to a bare
// SessionState that has no pre-resolved Conversation to reuse — State's own
// shape, unlike List's Session, the same split upgradeLastTurnFromRecord
// already makes for #56.
//
// A no-op whenever st.ControlChannel is nil or not Failed (nothing to
// explain), or no record store is configured, or the record cannot be
// matched, or it has no matching entry — in every one of those cases Reason
// is left exactly as classify.go built it: empty.
func (d *Driver) upgradeControlChannelFromRecord(st fleet.SessionState, cwd, name string, created time.Time, paneID string) fleet.SessionState {
	if st.ControlChannel == nil || st.ControlChannel.State != fleet.ControlChannelFailed || d.conversations == nil {
		return st
	}
	ref := d.conversations.lookup(conversationKey{pane: paneID, created: created}, cwd, name, created)
	if ref == nil || !ref.Known {
		return st
	}
	if fact, ok := latestControlDisconnect(d.conversations.recordPath(cwd, ref.ID)); ok {
		channel := *st.ControlChannel
		channel.Reason = fact.reasonText()
		st.ControlChannel = &channel
	}
	return st
}
