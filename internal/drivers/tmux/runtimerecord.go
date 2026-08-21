package tmux

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Reading the runtime's own record of a REFUSAL, rather than the screen it
// painted about one (#56).
//
// # What the record says, measured rather than assumed
//
// A refusal is not a distinct event type in the record. It is an ordinary
// `type: "assistant"` entry the runtime writes IN PLACE of a real reply, with
// four extra fields set: `isApiErrorMessage: true`, `error` (a closed set
// measured on a live fleet: "rate_limit", "server_error",
// "authentication_failed", "oauth_org_not_allowed"), `apiErrorStatus` (the
// HTTP status, when the failure came from one), and a `message.content[0].text`
// carrying the runtime's own prose — the SAME sentence the screen renders
// ("You've hit your session limit · resets 9:50pm (Asia/Saigon)" for
// `rate_limit`; "API Error: 529 Overloaded ... usually temporary — try again
// in a moment" for a retryable `server_error`).
//
// `rate_limit` is specifically the account-level condition §2.3 calls
// `quota_blocked`. The other three categories are turn failures of a
// different kind — real, worth `LastTurn`, but never `Quota`.
//
// # Written at the refusal, not deferred to some later completion
//
// Measured across sampled records: the `system/turn_duration` entry that
// closes out a turn lands one to five milliseconds after the error entry —
// the error entry effectively IS the turn's terminal write, not a fact
// buffered until later. A refusal that ends the session is therefore not at
// risk of going unrecorded, which was this issue's own "measurement first"
// worry.
//
// # No native retryable field
//
// There is nothing in the record that says "this is temporary" as a
// boolean — only the same prose the screen already carries. So this file
// does not invent a status-code-based inference (lastTurnFailed's own
// comment already explains why that would be the wrong move); it applies
// the SAME word rule (retryableWords) to the record's text instead of a
// live, window-bounded screen scan. The source moves; the rule does not.

const (
	// recordTailBytes bounds how much of a record file this driver reads
	// looking for the most recent assistant entry. Deliberately smaller than
	// conversation.go's identity read: that one reads the top of a file
	// once and caches it forever; this one may run every read cycle for
	// every session currently reporting quota_blocked or a failed last
	// turn, so it stays cheap by design rather than by luck. A record whose
	// final line does not fit in this window is honestly unresolvable
	// (recordUnavailable) rather than silently wrong — the driver already
	// has a screen-derived fallback for exactly that case.
	recordTailBytes = 256 << 10 // 256 KiB

	// recordTailCandidates bounds how many decoded lines within the tail
	// window this driver is willing to inspect before giving up. The
	// answer is always the LAST assistant-type line, which in practice is
	// within the first few lines scanned backward; this is a defensive
	// ceiling, not an expected depth.
	recordTailCandidates = 200
)

// recordVerdict is what latestAPIError concluded about a record's most
// recent assistant-role entry.
type recordVerdict int

const (
	// recordUnavailable: the record could not be opened, or nothing
	// decodable was found in the tail window. Not a claim about the
	// session — §5.7's rule applied to this source the way it already
	// applies to a failed pane capture: absence of a usable read is a
	// different fact from a positive answer either way, and a caller
	// must fall back rather than treat this as "no error".
	recordUnavailable recordVerdict = iota

	// recordCleanTurn: the most recent assistant entry carries no error.
	// This is POSITIVE evidence the account is not refusing work and the
	// last turn did not fail — the durable counterpart to `sawWorking`,
	// and the signal #56 asks for to take a session OUT of quota_blocked
	// without waiting for some other pane to be caught mid-spinner.
	recordCleanTurn

	// recordAPIError: the most recent assistant entry is the runtime's own
	// report of a refusal. apiErrorFact carries what it said.
	recordAPIError
)

// apiErrorFact is what the record says about its most recent refusal.
type apiErrorFact struct {
	// category is the runtime's own error string ("rate_limit" is quota;
	// anything else is a turn failure of a different kind).
	category string
	// text is the runtime's own prose, in full — deliberately NOT trimmed
	// to one sentence here. lastTurnFailed's Retryable check reads
	// retryableWords against the WHOLE joined screen text, and a 529's
	// "usually temporary" clause sits after the first period the sentence
	// cut would stop at ("API Error: 529 Overloaded. ... usually temporary
	// — try again in a moment."). Trimming here would silently make every
	// retryable server error read as not-retryable. Callers trim for
	// display (trimToSentence) and check retryableWords on this, in that
	// order — never the reverse.
	text string
	// at is the refusal's own timestamp, as the runtime wrote it — not
	// when this driver happened to read the file.
	at time.Time
}

// reasonSentence is fact.text cut to one sentence for display — the record
// counterpart to lastTurnFailed's own trim, applied at the same point
// (after retryableWords has already looked at the full text).
func (f apiErrorFact) reasonSentence() string {
	return trimToSentence(f.text, 200)
}

// retryable applies the same word rule lastTurnFailed uses on the screen,
// to this fact's full text.
func (f apiErrorFact) retryable() bool {
	return retryableWords(strings.ToLower(f.text))
}

// apiErrorRecordEntry is the subset of one JSONL line this driver reads to
// decide whether it is a refusal. Everything else in the line — model
// details, usage counters, request ids — is deliberately not decoded: this
// answers "did the runtime report a failure, and what did it say", nothing
// about the conversation's content.
type apiErrorRecordEntry struct {
	Type              string `json:"type"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Timestamp         string `json:"timestamp"`
	Message           struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// latestAPIError reads the tail of one runtime record and reports what its
// most recent assistant-role entry says.
//
// The most recent assistant entry answers both questions this driver ever
// asks of a record: whether the account is STILL refusing work (only true
// while that entry is itself an unanswered `rate_limit` refusal — anything
// after it, success or a different kind of failure, means something has
// happened since), and how the session's LAST TURN ended.
// recordTail reads the last recordTailBytes of one runtime record and
// returns it as decoded lines, oldest first — the same window and the same
// torn-first-line allowance every reader of this store needs, factored out
// so latestAPIError (#56) and latestControlDisconnect (#69) cannot drift
// apart on it. ok is false whenever the file could not be opened or stat'd;
// an empty record (ok true, zero lines) is a different, legitimate fact from
// that.
func recordTail(path string) (lines []string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	start := int64(0)
	torn := false
	if info.Size() > recordTailBytes {
		start = info.Size() - recordTailBytes
		torn = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if torn && len(lines) > 0 {
		// The first line after a mid-file seek may be a torn fragment of
		// whatever line straddled the seek point. The record is
		// append-only, so dropping one candidate costs nothing a real
		// answer would have needed — the line we actually want is later
		// (closer to EOF), never this one.
		lines = lines[1:]
	}
	return lines, true
}

func latestAPIError(path string) (apiErrorFact, recordVerdict) {
	lines, ok := recordTail(path)
	if !ok {
		return apiErrorFact{}, recordUnavailable
	}

	inspected := 0
	for i := len(lines) - 1; i >= 0 && inspected < recordTailCandidates; i-- {
		inspected++
		var entry apiErrorRecordEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			// A torn or half-written line is not a reason to give up on
			// the ones before it (conversation.go's readRecordEntry makes
			// the same allowance for the same reason: the runtime appends,
			// and a reader may arrive mid-write).
			continue
		}
		if entry.Type != "assistant" {
			continue
		}
		// The most recent assistant-role entry, whichever it is, answers
		// the question. Stop here regardless of what it says — an OLDER
		// assistant entry is history superseded by this one, exactly the
		// replay hazard usageLimit and lastTurnFailed already guard
		// against on the screen side.
		if !entry.IsAPIErrorMessage {
			return apiErrorFact{}, recordCleanTurn
		}
		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			// The record found the right line but not a fact we can act
			// on: honestly unresolvable rather than a guessed time.
			return apiErrorFact{}, recordUnavailable
		}
		var text string
		if len(entry.Message.Content) > 0 {
			text = entry.Message.Content[0].Text
		}
		return apiErrorFact{
			category: entry.Error,
			text:     text,
			at:       ts,
		}, recordAPIError
	}
	return apiErrorFact{}, recordUnavailable
}

// resetHint extracts the runtime's own reset time out of this fact's text,
// the same rule usageLimit applies to a screen line — display it, never
// parse it further.
func (f apiErrorFact) resetHintText() string {
	h, _ := resetHintIn(strings.ToLower(f.text))
	return h
}

// recordFactFor asks the runtime's own record about one session already
// carried in a fleet.Session — reusing its already-resolved Conversation
// rather than looking one up again (#56). recordUnavailable whenever no
// record store is configured, this session's conversation could not be
// matched, or the matched record could not be read; every caller must keep
// its screen-derived fallback in exactly those cases, not only the ones it
// happens to check for.
func (d *Driver) recordFactFor(s fleet.Session) (apiErrorFact, recordVerdict) {
	if d.conversations == nil || s.Conversation == nil || !s.Conversation.Known {
		return apiErrorFact{}, recordUnavailable
	}
	return latestAPIError(d.conversations.recordPath(string(s.Cwd), s.Conversation.ID))
}

// upgradeLastTurnFromRecord applies #56's LastTurn upgrade to a bare
// SessionState that has no pre-resolved Conversation to reuse — State's own
// shape, unlike List's Session. It performs its own conversation lookup,
// keyed the same way conversation.go keys one (pane + creation time); that
// lookup memoises successes, so calling it again for a session List has
// already resolved this cycle is a cache read, not a rescan.
//
// A no-op whenever st.LastTurn is nil (the screen flagged nothing this
// read) or no record store is configured or the record cannot be matched —
// in every one of those cases the screen-derived TurnEnd, or its absence,
// is left exactly as classify.go built it.
func (d *Driver) upgradeLastTurnFromRecord(st fleet.SessionState, cwd, name string, created time.Time, paneID string) fleet.SessionState {
	if st.LastTurn == nil || d.conversations == nil {
		return st
	}
	ref := d.conversations.lookup(conversationKey{pane: paneID, created: created}, cwd, name, created)
	if ref == nil || !ref.Known {
		return st
	}
	switch fact, verdict := latestAPIError(d.conversations.recordPath(cwd, ref.ID)); verdict {
	case recordAPIError:
		st.LastTurn = &fleet.TurnEnd{
			Outcome:   "failed",
			Reason:    fact.reasonSentence(),
			Retryable: fact.retryable(),
		}
	case recordCleanTurn:
		// The durable record says the last turn actually succeeded — the
		// screen's "api error" match was history a window scan cannot
		// tell from the present. #56's argument for Quota, arriving at
		// LastTurn instead.
		st.LastTurn = nil
	}
	return st
}
