package fleet

import (
	"errors"
	"time"
)

// Turn is one thing a session's agent said: the text the runtime recorded as
// written by the agent itself, and when.
//
// # What a Turn is, and what it is not (muster #258)
//
// This is the one place this API returns content a SESSION produced rather
// than content the RUNTIME reported about itself (session-abstraction.md §5.8,
// narrowed by docs/adr/258-assistant-turns-read.md). The boundary is the whole
// of the type, so it is stated here where a future field would be added:
//
//   - IN: the text blocks of a record entry the runtime wrote as an assistant
//     message of the session's own agent.
//   - OUT, always: tool calls and tool results, file contents the agent read,
//     the messages a human or another session sent in, system and hook output,
//     the agent's reasoning blocks, a sub-agent's chatter, and the runtime's own
//     synthetic notices (an API error is the runtime speaking, not the agent).
//
// A field that would carry any OUT category is not an extension of this type;
// it is a new ruling. There is deliberately no Kind, no Role and no Raw here:
// each would be a place for the next category to slip in unreviewed.
type Turn struct {
	// At is the runtime's own timestamp on the entry, not the time it was
	// read. UTC.
	At time.Time `json:"at"`

	// Text is what the agent wrote, bounded by MaxTurnTextBytes.
	Text string `json:"text"`

	// Truncated is true when Text was cut at MaxTurnTextBytes. Absent
	// otherwise, so the common turn carries no extra field.
	Truncated bool `json:"truncated,omitempty"`
}

// TurnsPage is one read of a session's assistant turns, oldest first.
type TurnsPage struct {
	// Turns is in the order the agent wrote them. Empty (never null) when
	// there is nothing new, which is a normal answer to a poll.
	Turns []Turn `json:"turns"`

	// Next is an opaque cursor: pass it back as `since` to read what the agent
	// wrote after this page. It is returned even when Turns is empty, so a
	// poller always has a place to resume from. Do not parse it.
	Next string `json:"next"`
}

// TurnsQuery is what a caller asks of a session's assistant turns.
type TurnsQuery struct {
	// Since is a cursor from a previous TurnsPage.Next. Empty means "the most
	// recent Limit turns".
	Since string

	// Limit bounds the page. Zero means DefaultTurnsLimit; more than
	// MaxTurnsLimit is refused rather than silently clamped, because a caller
	// that asked for 500 and got 100 would conclude the session had written
	// 100.
	Limit int
}

const (
	// DefaultTurnsLimit is the page size when the caller names none.
	DefaultTurnsLimit = 20

	// MaxTurnsLimit is the largest page a caller may ask for.
	MaxTurnsLimit = 100

	// MaxTurnTextBytes bounds one Turn's Text. A single assistant entry can be
	// as large as the agent chose to write, and a response is the wrong place
	// to find out how large.
	MaxTurnTextBytes = 64 << 10
)

// ErrNoTurnRecord is returned when the session exists but the runtime's record
// of its conversation could not be identified or opened: a session created a
// moment ago whose record is not written yet, one resumed in a way the record
// cannot be tied back to, or a record the service cannot read. It is the
// absence of a source, not an empty conversation — a conversation with no agent
// text is a TurnsPage with no Turns (§5.7) — and the service maps it to
// not_found with Retryable set.
var ErrNoTurnRecord = errors.New("fleet: no readable conversation record could be identified for this session")
