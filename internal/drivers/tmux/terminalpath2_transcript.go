package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Terminal path v2, item c / D6: "screen-based confirmation is the weak
// link". confirmSubmitted (tmux.go) only ever asks the SCREEN whether the
// composer emptied or a marker cleared — evidence that the multiplexer
// redrew the pane, not that the runtime PROCESS treated the paste as a
// submitted turn. The runtime already writes that fact, unasked, to its own
// transcript (conversation.go documents the same file family this reads):
// a `"type":"user"` entry when it was idle, or a `"type":"queue-operation"`
// enqueue when it was busy. Measured for #180: nothing at all is
// written for a STRANDED delivery (29 of 29), the asymmetry this file turns into a positive signal instead
// of only a negative one.
//
// # Why this is a SEPARATE file from conversation.go rather than an addition to it
//
// conversation.go answers "which record belongs to this session" and
// deliberately reads nothing else from the file ("This whole path answers
// 'which record' without ever answering 'what is in it'" — its own doc
// comment). This file is the first thing in the package that DOES open a
// record and read message content, because confirming a submit is exactly
// the question conversation.go declined to answer. Keeping the two apart
// keeps that boundary legible in the diff, not just in prose.
//
// # The honest decision on Outcome (task requirement: decide, don't dodge)
//
// A transcript-confirmed send still reports OutcomeQueued, never
// OutcomeSubmitted, and ConfirmsDelivery stays false for it — unchanged from
// every other terminal-path outcome (docs/api.md's own note: "'submitted' is
// never returned by the terminal Send path"). The reasoning: a `type:user`
// transcript entry proves the RUNTIME PROCESS accepted the text as a turn —
// substantially stronger evidence than "the composer redrew empty", which is
// why it gets its own counter and is tried first — but it is still not an
// acknowledgement from the ADDRESSED PARTY (the agent) that it received or
// acted on the message, which is what this fleet reserves OutcomeSubmitted
// for (the inbox path's own receipt). Upgrading the outcome on the strength
// of this evidence would be exactly the "guess dressed as a reading" this
// driver's own §5.6 discipline exists to refuse. If that bar is ever revised,
// it should be revised for the whole fleet's Outcome vocabulary at once, not
// smuggled in through one driver's one confirmation path.
//
// # Fixtures are synthetic
//
// Every unit test against this file uses SYNTHETIC transcript lines its own
// tests construct, never a real transcript: a real one is somebody's
// conversation. The entry shapes they model were read from the runtime's
// own writer, and the live gate for #180 exercised them end to end.

// pastedTextMarkerPattern recognises a transcript entry whose content is
// ENTIRELY Claude Code's own collapsed-paste placeholder — the same textual
// shape the composer renders on screen for a paste too large to echo
// (markerCounts, tmux.go), which round-1's own background names as also
// appearing in the transcript for a large paste ("account for Claude Code's
// pasted-content wrapper for >800 chars / 5+ lines"). Anchored (^...$) after
// trimming: a marker embedded inside other prose is a false positive this
// driver has no way to attribute, and is deliberately left unmatched by this
// pattern (falling through to the plain substring/equality checks instead).
var pastedTextMarkerPattern = regexp.MustCompile(`^\[Pasted text #\d+(?: \+(\d+) lines?)?\]$`)

// parsePastedTextMarker reports the line count a collapsed-paste placeholder
// claims, when s (already trimmed) is ENTIRELY that placeholder. ok=false
// covers both "not a marker at all" and "a marker with no line count printed"
// — the second is legitimate (see markerCounts' own numberAfter, which
// treats a missing count as 0 rather than as absent) and is reported here as
// lines=0, ok=true, matching that same convention.
func parsePastedTextMarker(s string) (lines int, ok bool) {
	m := pastedTextMarkerPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	if m[1] == "" {
		return 0, true
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	return n, true
}

// pastedContentWrapperPattern strips the ONE wrapper the runtime is known to
// add around a pasted block it stores in full inside a transcript entry:
// `<pasted_content id="...">...</pasted_content>`, keeping the inner text in
// place. Unlike the composer's on-screen "[Pasted text #N]" placeholder
// (parsePastedTextMarker, below — a rendering artefact this driver has no
// proof the transcript ever repeats verbatim), the runtime needs the actual
// bytes in the transcript to act on them, so the plausible shape for a large
// paste there is the REAL text wrapped in an identifying tag, not a
// placeholder standing in for it.
var pastedContentOpenTag = regexp.MustCompile(`<pasted_content\s+id="[^"]*">`)

// pastedContentCloseTag: review fix. The runtime is measured (runtime 2.1.281,
// its own writer) to close this wrapper with the
// SAME id attribute repeated on the closing tag —
// `</pasted_content id="c702">`, not the plain `</pasted_content>` this
// pattern used to require. A closing tag missing that attribute is also
// accepted (never independently measured, but strictly weaker to allow, and
// nothing about matching it can make a real transcript match WORSE) — see
// TestConfrevRealPastedContentWrapperMatches / …LongPasteRealWrapperConfirmedByTranscript
// for the fixture this was verified against. Before this fix, EVERY
// transcript entry carrying a wrapped paste failed to normalise to its bare
// text at all (the trailing `</pasted_content id="c702">` survived intact,
// so an otherwise byte-identical turn never compared equal), which meant D6
// did nothing for exactly the messages most likely to strand — the ones long
// enough to trigger the wrapper in the first place — and every one of them
// fell through the full submitConfirmWindow to the weaker screen signal.
var pastedContentCloseTag = regexp.MustCompile(`</pasted_content(?:\s+id="[^"]*")?>`)

// normalizeTranscriptText prepares a transcript entry's own text for an
// EXACT comparison against sent, after stripping the specific, known extras
// the runtime is measured (or plausibly documented — see this function's own
// doc comment for what is which) to add on top of the bytes this driver
// actually delivered:
//
//   - the <pasted_content id="..."> wrapper tags, content kept;
//   - a tab expanded to 4 spaces (composerText and the runtime's own
//     rendering both do this; unverified whether the TRANSCRIPT does too, but
//     harmless to allow — a real 4-space run this driver's own text already
//     had collapses back to identical either way once whitespace is later
//     stripped by normalizeForMatch);
//   - a single trailing space — the wake key (Space, tmux.go) is pressed as
//     a real keystroke into the composer immediately before Enter, so the
//     runtime receives sent+" ", not sent, on every ordinary submit.
//
// normalizeForMatch's own NFC-compose-and-strip-whitespace pass runs last, so
// none of the above needs to worry about composing-form or interior
// whitespace differences — that is a separate, already-solved axis.
func normalizeTranscriptText(s string) string {
	s = pastedContentOpenTag.ReplaceAllString(s, "")
	s = pastedContentCloseTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.TrimSuffix(s, " ")
	return normalizeForMatch(s)
}

// transcriptTurnMatches reports whether a transcript entry's own extracted
// text corresponds to sent — the SAME text this driver pasted (already
// through paneLabelled's sender-label prefix, if any: Send always calls this
// with the LABELLED text, so a caller here never needs its own tolerance for
// a missing label — see the review fix below for why that used to be
// conflated with a much more dangerous tolerance).
//
// # Review fix: exact match after normalisation, not "contains"
//
// This used to accept ANY recorded turn containing normSent as a substring,
// with no minimum length and no check on which KIND of transcript entry it
// was reading from (extractTranscriptText's own isMeta/isCompactSummary/
// isSidechain/origin/promptSource filters, added by this same review pass,
// close the second half of that). The substring rule's own justification —
// "covers the sender-label line paneLabelled prepends" — was already moot
// for every real call in this package: Send always passes the LABELLED text
// as sent (see terminalpath2_transcript.go's callers in tmux.go), so
// recordedText and sent already carry the same label on both sides whenever
// this is called from production code, and a substring rule bought nothing
// there. What it did buy: a skill's transcript body containing the sent word
// "go" as a substring confirmed it; a compaction summary quoting an earlier,
// unrelated turn confirmed it; a bare "[Pasted text #1]" with no line count
// confirmed ANY sent text at all, because parsePastedTextMarker's own
// ok=true/lines=0 fell through the OLD "lines == 0 || ..." check
// unconditionally rather than only for a length-agreeing paste. Requiring
// exact equality (after the specific, named extras normalizeTranscriptText
// allows) removes all of those at once, at the cost of the one shape that
// was never real in the first place.
func transcriptTurnMatches(recordedText, sent string) bool {
	normSent := normalizeForMatch(sent)
	if normSent == "" {
		return false
	}
	if normalizeTranscriptText(recordedText) == normSent {
		return true
	}
	// The bare collapsed-paste-marker fallback: kept because whether the
	// transcript ever ACTUALLY stores this literal placeholder instead of
	// the real bytes is unverified (see this file's own top comment), not
	// because it is known to happen. Scoped tightly — the recorded text must
	// be ENTIRELY the marker (parsePastedTextMarker's own anchored pattern),
	// and a PRINTED line count must agree with sent's own newline count. A
	// marker with NO printed count (lines==0, ok==true) no longer matches
	// unconditionally: it now requires sent to carry no newline either — a
	// bare "[Pasted text #7]" is Claude Code's own summary for a paste with
	// nothing further to say about line count, which is what a single-line
	// paste's own marker looks like (colab-fleet round-1 background); it is
	// not a wildcard for "however many lines a completely unrelated send
	// happened to have".
	if lines, ok := parsePastedTextMarker(recordedText); ok {
		sentLines := strings.Count(sent, "\n")
		return lines == sentLines
	}
	return false
}

// extractTranscriptText parses one JSONL line and reports, when it is a
// candidate turn at all, which family it is ("user" or "queue-operation")
// and the plain text it carries. Deliberately tolerant of more than one
// content shape — see this file's own top comment for why the exact
// "queue-operation" shape is not independently verified here.
//
// # Review fix: the real queue-operation shape, and what is NOT a candidate
//
// Claude Code's own writer for a queue-operation entry (read from the
// runtime's own writer) puts the queued text in a top-level "content" field, only
// on operation=="enqueue" — never in a top-level "text" field, and never on
// "dequeue"/"remove"/"popAll". The old check matched on type containing
// "queue" (case-insensitive) with no operation check at all, and read only a
// top-level "text" — so a REAL enqueue entry was never recognised, and this
// driver could not distinguish an enqueue from Claude Code discarding the
// same queued text later under "remove" (the review's own probes: an
// enqueue and dequeue can both carry the same text; treating either as
// confirmation would fabricate a match at the wrong moment, or worse, at
// exactly the moment a queued instruction was WITHDRAWN rather than
// accepted). "content" is read FIRST now, as the verified real shape; the
// old top-level "text" stays as a second, tolerant fallback — costs nothing,
// and the one existing test exercising it stays honest about what it is
// (see TestExtractTranscriptTextQueueOperationTopLevelText's own comment).
//
// A "user" candidate is now also rejected outright when isMeta,
// isCompactSummary or isSidechain is true, or when an `origin` object names
// a kind other than "human", or when `promptSource` is present and names
// something other than "typed" or "queued" (round-1's own measurement: real
// human/agent sends carry origin.kind=human, promptSource=typed/queued) —
// closing the other half of the review's "loose text matching" finding: a
// skill body, a compaction summary quoting an earlier turn, or a
// cross-session peer message are all `type:"user"` entries in the same file,
// and none of them is a candidate for "did OUR delivery get accepted" no
// matter how its text compares.
func extractTranscriptText(line []byte) (kind, text string, ok bool) {
	kind, text, _, ok = extractTranscriptCandidate(line)
	return kind, text, ok
}

// extractTranscriptCandidate is extractTranscriptText plus the one extra
// field review-confirmation's anchoring fix needs: promptSource, verbatim
// off a "user" candidate ("typed", "queued", or "" when the field is
// absent — round-1's own measurement is that a real entry always carries
// one of the first two, but a caller must not assume that of every
// transcript this driver will ever read). A "queue-operation" candidate
// always reports promptSource "" — the field belongs to a "user" entry, not
// an enqueue.
//
// Kept as the internal, richer function with extractTranscriptText as a thin
// wrapper (rather than widening extractTranscriptText's own signature)
// because every existing test and caller already depends on the narrower
// three-value shape, and this driver's own copy-and-own discipline (§5.x)
// says do not reshape a signature everything already agrees on merely to
// grow one caller a field nothing else needs.
func extractTranscriptCandidate(line []byte) (kind, text, promptSource string, ok bool) {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return "", "", "", false
	}
	if truthy(obj["isMeta"]) || truthy(obj["isCompactSummary"]) || truthy(obj["isSidechain"]) {
		return "", "", "", false
	}
	t, _ := obj["type"].(string)
	isUser := t == "user"
	operation, _ := obj["operation"].(string)
	isEnqueue := t == "queue-operation" && operation == "enqueue"
	if !isUser && !isEnqueue {
		return "", "", "", false
	}
	if isUser {
		if origin, ok := obj["origin"].(map[string]any); ok {
			if k, _ := origin["kind"].(string); k != "" && k != "human" {
				return "", "", "", false
			}
		}
		if ps, ok := obj["promptSource"].(string); ok {
			promptSource = ps
			if ps != "" && ps != "typed" && ps != "queued" {
				return "", "", "", false
			}
		}
		// Shape 1 (documented, used by conversation.go's own readRecordEntry
		// for other fields on this same family of file): message.content,
		// either a plain string or an array of content blocks each carrying
		// "type" and "text".
		if msg, ok := obj["message"].(map[string]any); ok {
			if role, _ := msg["role"].(string); role != "" && role != "user" {
				// A message present but not authored as "user" is not this
				// delivery's own turn (an assistant turn logged under a
				// different type key would not reach here at all, but a
				// tolerant reader costs nothing to double-check).
				return "", "", "", false
			}
			switch content := msg["content"].(type) {
			case string:
				text = content
			case []any:
				var b strings.Builder
				for _, blockAny := range content {
					block, ok := blockAny.(map[string]any)
					if !ok {
						continue
					}
					if bt, _ := block["type"].(string); bt != "" && bt != "text" {
						continue
					}
					if s, ok := block["text"].(string); ok {
						b.WriteString(s)
					}
				}
				text = b.String()
			}
		}
	} else {
		// isEnqueue: the verified real shape reads a top-level "content"
		// first; "text" is kept only as a tolerant fallback for a shape this
		// driver has never actually observed (see this function's own doc
		// comment).
		if s, ok := obj["content"].(string); ok {
			text = s
		}
		if text == "" {
			if s, ok := obj["text"].(string); ok {
				text = s
			}
		}
	}
	if text == "" {
		return "", "", "", false
	}
	return t, text, promptSource, true
}

// truthy reports whether v — one field pulled out of a decoded JSON object —
// is JSON `true`. Every other shape (absent, false, a string, a number) is
// not, which is the right default for a field this driver reads only to
// EXCLUDE a candidate: absence must never itself exclude something, only an
// explicit true may.
func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// transcriptScanResult is transcriptTailScan's own three-way verdict — see
// its doc comment for what each value means and why "no match" was not
// enough on its own (review-confirmation's own finding).
type transcriptScanResult int

const (
	// transcriptScanSilent: nothing after offset was even a CANDIDATE turn
	// for this delivery — the ordinary, common case while a poll is still
	// waiting for the runtime to write anything at all.
	transcriptScanSilent transcriptScanResult = iota
	// transcriptScanMatched: a candidate turn attributable to THIS delivery
	// (see transcriptTailScan's own doc comment on the enqueue/dequeue
	// anchoring this requires) matched sent exactly.
	transcriptScanMatched
	// transcriptScanDifferentTurn: a candidate turn was recorded, attributable
	// to this delivery's own confirmation window, and it did NOT match sent —
	// evidence something else was submitted (a truncated echo, a merged
	// keystroke, ...), which must never be treated the same as "nothing
	// happened yet" (transcriptScanSilent): the caller's fallback for silence
	// is the screen-based signal, and treating a genuine disagreement as
	// silence is exactly what let that fallback report `queued` for text
	// that, per the transcript's OWN account, was never actually submitted.
	transcriptScanDifferentTurn
)

// transcriptTailMatches is transcriptTailScan collapsed to the boolean shape
// its own (pre-review) callers and tests already depend on — kept as a thin
// wrapper for exactly the same "do not reshape an agreed signature" reason
// extractTranscriptText wraps extractTranscriptCandidate.
func transcriptTailMatches(path string, offset int64, sent string) (bool, error) {
	result, err := transcriptTailScan(path, offset, sent)
	return result == transcriptScanMatched, err
}

// transcriptTailScan scans path from byte offset to EOF for a candidate turn
// matching sent, exactly once per call — the caller polls. offset is the
// file size recorded BEFORE the submit keystroke (resolveTranscriptSource /
// its caller), which is what makes "identical earlier text must not count"
// structural rather than a rule this function has to enforce itself: text
// already in the file before offset is never read at all.
//
// # Review fix: a dequeue is only attributed to THIS delivery if an enqueue
// for the same text was ALSO seen after offset
//
// A queue-operation "enqueue" and the "user"/promptSource=="queued" entry
// that later dequeues it can carry byte-identical text (round-1's own
// measurement: an ordinary re-ping, a repeated "continue"/"yes", or
// a consumer.s automatic resumeIfStranded retry all produce exactly this
// duplicate-content shape). Before this fix, a dequeue entry matched sent
// with no regard for WHICH enqueue it belonged to — so an identical message
// enqueued BEFORE this delivery's own offset, whose dequeue happened to land
// inside THIS delivery's confirmation window, confirmed a submit that never
// actually registered (this delivery's own Enter having been swallowed), and
// the caller's documented remedy then delivered a duplicate.
//
// queueDepth is the fix: it only ever counts enqueues seen strictly AFTER
// offset, and a dequeue may only be attributed to this delivery by consuming
// one of those. A dequeue seen while queueDepth is zero belongs to a turn
// that was already queued before this delivery even started — informative
// about something else, not about this delivery — so it is skipped
// entirely: neither a match nor a disagreement.
func transcriptTailScan(path string, offset int64, sent string) (transcriptScanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return transcriptScanSilent, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return transcriptScanSilent, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), recordLineLimit)
	queueDepth := 0
	sawDifferent := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		kind, text, promptSource, ok := extractTranscriptCandidate(line)
		if !ok {
			continue
		}
		matches := transcriptTurnMatches(text, sent)
		if kind == "queue-operation" {
			// Direct evidence a busy send registered — the fast path, tried
			// first, matching the pre-review behaviour exactly (an enqueue
			// this driver can see happened strictly after offset is, by
			// construction, a NEW event; there is no earlier-turn ambiguity
			// to anchor against here the way there is for its dequeue).
			if matches {
				return transcriptScanMatched, nil
			}
			queueDepth++
			continue
		}
		// kind == "user".
		if promptSource == "queued" {
			if queueDepth <= 0 {
				// This dequeue cannot be attributed to an enqueue this
				// delivery's own window has seen — see this function's own
				// doc comment. Not evidence about this delivery either way.
				continue
			}
			queueDepth--
		}
		if matches {
			return transcriptScanMatched, nil
		}
		sawDifferent = true
	}
	if err := sc.Err(); err != nil {
		return transcriptScanSilent, err
	}
	if sawDifferent {
		return transcriptScanDifferentTurn, nil
	}
	return transcriptScanSilent, nil
}

// processSessionRecord is the shape of one `~/.claude/sessions/<pid>.json`
// file — read for exactly four fields, the ones D6's fallback needs; every
// other field the real file carries (round-1 measured it also carries
// `startedAt`, `version`, `kind`, `entrypoint`, `pidDomain`, `tmux`,
// `messagingSocketPath`, `name`, `agent`, `status`, ...) is left unparsed on
// purpose — reading a field this driver does not use would be one more
// place a future rename of that field silently breaks something.
type processSessionRecord struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	// ProcStart is measured (for #180) to already be
	// rendered in exactly `ps -o lstart=`'s TEXTUAL layout — but, unlike
	// genuine `ps` output, this field crosses a process boundary (this file
	// is written by the runtime's own Node.js process, not read live from
	// `ps` by this driver), so it is exactly the case ParseProcessStartTime's
	// own doc comment names and warns against: "never for a value that may
	// have crossed a process, a machine, or a serialization boundary" —
	// colab-fleet #147 already found this same mistake once, for a
	// different field. Round-1 measured it directly for THIS field too: a
	// pid file's own procStart reads "Thu Sep 24 07:38:57 2026" for the same
	// process whose `ps -o lstart=` (parsed as local time, correctly) reads
	// "Thu Sep 24 16:38:57 2026" — a 9-hour gap consistent with the writer
	// rendering in UTC while this machine's local zone is UTC+9, not with
	// two different processes. Reusing ParseProcessStartTime (time.Local) on
	// THIS field silently mis-parsed it as local, so `recordedStart` never
	// equalled `identity.StartedAt` for ANY live pid on such a machine —
	// every send fell back to the screen-based signal, and D6's whole
	// pid-identity path never actually fired. See
	// parseProcessSessionRecordStartTime (terminalpath2_transcript.go),
	// which parses this field with the correct (UTC) zone instead.
	ProcStart string `json:"procStart"`
}

// parseProcessSessionRecordStartTime parses processSessionRecord.ProcStart —
// see that field's own doc comment for why ParseProcessStartTime (time.Local)
// is the wrong parser for it, even though the two share an identical textual
// layout.
func parseProcessSessionRecordStartTime(s string) (time.Time, error) {
	return time.ParseInLocation(psStartTimeLayout, s, time.UTC)
}

// readProcessSessionRecord reads and parses one process-sessions file. ok is
// false for anything short of a complete, well-formed record — a caller with
// a partial answer has no safe use for it (§5.4: never resume/verify
// identity on a guess).
func (d *Driver) readProcessSessionRecord(pid int) (processSessionRecord, bool) {
	if d.processSessionsRoot == "" {
		return processSessionRecord{}, false
	}
	b, err := os.ReadFile(filepath.Join(d.processSessionsRoot, fmt.Sprintf("%d.json", pid)))
	if err != nil {
		return processSessionRecord{}, false
	}
	var rec processSessionRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return processSessionRecord{}, false
	}
	if rec.PID != pid || rec.SessionID == "" || rec.CWD == "" || rec.ProcStart == "" {
		return processSessionRecord{}, false
	}
	return rec, true
}

// transcriptSource is what resolveTranscriptSource found: enough to poll a
// transcript file from a fixed starting offset, plus the evidence a receipt
// can quote when this signal is the one that decided the outcome.
type transcriptSource struct {
	path     string
	offset   int64
	evidence string
}

// liveProcessIdentity is what resolveLiveProcessSessionID established: the
// runtime's OWN, currently-running claim of which conversation this pane
// holds, verified against the same process generation #116's
// ResolveProcessIdentity already checks.
type liveProcessIdentity struct {
	sessionID string
	evidence  string
}

// resolveLiveProcessSessionID is D6's per-process identity fallback,
// factored out of resolveTranscriptSource so it can ALSO be used to
// cross-check a cached record-root lookup — see resolveTranscriptSource's
// own doc comment on the review's "stale transcript after /clear" finding
// for why a cache hit alone is no longer trusted unconditionally.
//
// ok=false covers every reason this driver could not corroborate a live
// sessionId for target right now: not configured, the process could not be
// resolved, its own session file disagrees on cwd, or its own recorded
// start time no longer matches the running process's generation (a
// recycled pid, or simply a malformed file) — §5.4 throughout: a partial
// answer is not a safe one to act on.
func (d *Driver) resolveLiveProcessSessionID(ctx context.Context, ref fleet.SessionRef, target *paneRow) (liveProcessIdentity, bool) {
	if d.processSessionsRoot == "" {
		return liveProcessIdentity{}, false
	}
	identity, err := d.ResolveProcessIdentity(ctx, ref)
	if err != nil {
		return liveProcessIdentity{}, false
	}
	rec, ok := d.readProcessSessionRecord(identity.PID)
	if !ok {
		return liveProcessIdentity{}, false
	}
	if rec.CWD != target.cwd {
		// §5.4: a record naming a different working directory is not
		// evidence about THIS session, whatever else it says — refuse
		// rather than guess it is merely stale.
		return liveProcessIdentity{}, false
	}
	// parseProcessSessionRecordStartTime, NOT ParseProcessStartTime — see
	// processSessionRecord.ProcStart's own doc comment for the review fix
	// this is (the field crosses a process boundary and is measured to be
	// rendered in UTC; parsing it as local silently broke this whole check
	// on any machine not itself running in UTC).
	recordedStart, err := parseProcessSessionRecordStartTime(rec.ProcStart)
	if err != nil || !recordedStart.Equal(identity.StartedAt) {
		// The pid was recycled since this file was written (or the file is
		// simply malformed) — VerifyProcessIdentity's own reasoning, applied
		// here instead of duplicated as a second check against the OS.
		return liveProcessIdentity{}, false
	}
	return liveProcessIdentity{
		sessionID: rec.SessionID,
		evidence: fmt.Sprintf("process-sessions file for pid %d (verified same process generation), "+
			"sessionId %s", identity.PID, rec.SessionID),
	}, true
}

// resolveTranscriptSource is D6's own two-step identity chain: try the
// record-root lookup every List already uses (d.conversations, keyed on the
// session's own NAME — conversation.go's own "reading our own input back out
// of a file somebody else wrote"); if that comes back unresolved — the
// ~18/75 "resumed sessions" gap round-1 measured, recorded as
// conversation.known=false — fall back to the runtime's own per-process
// identity file, corroborated against #116's process-identity verification
// (ResolveProcessIdentity) so a recycled pid's stale file is refused rather
// than trusted (§5.4).
//
// # Review fix: a cache hit is cross-checked, not trusted forever
//
// conversationStore's own "resolved" cache is keyed on (pane, created) for
// the WHOLE LIFE of a session (conversation.go's own doc comment: "resolved
// caches per SESSION, and only successes"). It was built to stop re-deriving
// an identity that had already been established — it was never built to
// notice the runtime regenerating its OWN session id under an otherwise
// unchanged tmux pane, which `/clear` and an in-pane runtime restart both do
// (round-1's own background note). Nothing about a cache keyed on the PANE
// changes when that happens, so every send after a `/clear` kept reading a
// stale, pre-clear transcript file forever — silently unknown, then a
// resume duplicating into the NEW conversation, matching the review's own
// reproduction.
//
// The fix does not remove the cache (it is still what makes most sends not
// pay ResolveProcessIdentity's cost of re-listing panes and running `ps`) —
// it cross-checks it, whenever the live per-process path CAN be resolved,
// against what the RUNNING PROCESS reports right now. Agreement is the
// common case and costs one extra, already-cheap file read; disagreement
// means the cache is stale, and the live identity wins.
//
// ok=false means neither step produced a file this driver could confirm is
// the right one — the caller's own responsibility from there is to fall back
// to the screen-based confirmSubmitted, per this file's own top comment.
func (d *Driver) resolveTranscriptSource(ctx context.Context, ref fleet.SessionRef, target *paneRow) (transcriptSource, bool) {
	live, liveOK := d.resolveLiveProcessSessionID(ctx, ref, target)

	if d.conversations != nil {
		key := conversationKey{pane: target.paneID, created: target.created}
		if conv := d.conversations.lookup(key, target.cwd, ref.ID, target.created); conv != nil && conv.Known {
			if !liveOK || live.sessionID == "" || live.sessionID == conv.ID {
				p := d.conversations.recordPath(target.cwd, conv.ID)
				if info, err := os.Stat(p); err == nil {
					return transcriptSource{
						path:     p,
						offset:   info.Size(),
						evidence: fmt.Sprintf("record-root lookup (%s): %s", conv.Source, conv.Evidence),
					}, true
				}
			} else {
				// The cache resolved to a DIFFERENT conversation id than the
				// runtime's own live identity reports right now — stale,
				// per this function's own doc comment. Fall through to the
				// live identity below rather than trusting the cache.
				d.counters.incr(counterTranscriptCacheStaleAfterClear)
			}
		}
	}

	// Fallback (or cache-was-stale override): the runtime's own per-process
	// identity, resolved above. Both the record root (to turn a sessionId
	// into a file path) and the process-sessions root (to learn the
	// sessionId at all) must be configured for this to produce anything —
	// see WithProcessSessionsRoot and WithRecordRoot's own off-by-default
	// contracts.
	if d.conversations == nil || !liveOK {
		return transcriptSource{}, false
	}
	p := d.conversations.recordPath(target.cwd, live.sessionID)
	info, err := os.Stat(p)
	if err != nil {
		return transcriptSource{}, false
	}
	return transcriptSource{path: p, offset: info.Size(), evidence: live.evidence}, true
}

// confirmSubmittedFromSource is confirmSubmittedV2's review-fixed
// replacement, split into two calls so the CALLER (Send, tmux.go) can
// resolve src BEFORE pressing the submit key rather than after — see this
// file's own doc comment on resolveTranscriptSource's caller contract below
// for why that ordering matters. src/srcOK is exactly
// resolveTranscriptSource's own return, taken at whatever point the caller
// resolved it.
//
// # Review fix: a resolved-but-silent transcript now falls back to the screen
//
// The exact "queue-operation" shape this driver parses (extractTranscriptText)
// is measured against real transcripts as of this review pass, but the field
// this driver is not independently certain about — whether EVERY busy-session
// enqueue is written inside submitConfirmWindow, versus this driver's own
// window simply being too short for a slow write — means a resolved
// transcript that stays silent through the whole poll is not, on its own,
// proof the delivery never registered. The OLD behaviour treated that
// silence as final and told the caller "unknown"; the caller's own
// documented remedy (resumeIfStranded) then re-pasted into a composer the
// runtime had ALREADY cleared (because it accepted the message and queued
// it) — the busy-session duplicate-delivery defect the review measured live,
// twice, on two different builds. Falling back to the pre-existing
// screen-based confirmSubmitted ONLY after the transcript's own window
// closes keeps the transcript as the FIRST, preferred signal (still tried
// exclusively for the first submitConfirmWindow, still what confirms most
// real sends per counterSubmitConfirmedByTranscript) while giving the
// composer-emptied/marker-cleared signal the chance to catch exactly the gap
// the transcript's own unverified shape might be missing —
// counterSubmitConfirmedByScreenAfterSilentTranscript is how a live rate for
// that gap becomes visible instead of staying a duplicate-delivery incident.
//
// # Review fix: a scanner error is "cannot tell", never silently "no match"
//
// transcriptTailMatches can fail outright (not merely reach EOF unmatched) —
// one transcript line exceeding recordLineLimit is bufio.Scanner's own
// "token too long". The old code discarded that error (`matched, _ :=`),
// making an unreadable transcript line indistinguishable from one that was
// read in full and simply disagreed. Counting it separately
// (counterTranscriptScannerUnreadable) does not change what this loop does
// with it — there is nothing safer to try than keep polling until the
// window closes, the same as an ordinary non-match — but it stops silently
// misfiling "this driver could not read what it was looking at" as if it
// were positive evidence of disagreement.
//
// Returns the same bool confirmSubmitted always has, plus which family of
// evidence decided it, for the receipt to quote.
func (d *Driver) confirmSubmittedFromSource(ctx context.Context, target *paneRow, sent string, key pasteKey, atCount int, src transcriptSource, srcOK bool) (bool, string) {
	if !srcOK {
		confirmed := d.confirmSubmitted(ctx, target.paneID, key, atCount)
		return confirmed, "no transcript could be resolved for this session; fell back to the screen-based signal (composer emptying or this delivery's own paste marker clearing)"
	}

	start := d.now()
	deadline := start.Add(submitConfirmWindow)
	for {
		result, err := transcriptTailScan(src.path, src.offset, sent)
		if err != nil {
			d.counters.incr(counterTranscriptScannerUnreadable)
		}
		switch result {
		case transcriptScanMatched:
			d.recordConfirmed(counterSubmitConfirmedByTranscript, d.now().Sub(start))
			return true, "the runtime's own transcript recorded this exact text as a turn (" + src.evidence + ")"
		case transcriptScanDifferentTurn:
			// Review fix: a transcript that recorded a DIFFERENT turn is not
			// silence, and must not be handed to the screen-based fallback
			// below — that fallback exists for "the transcript said nothing
			// at all", and a composer emptying for an unrelated reason (a
			// truncated echo of this same paste, or a person's own keystroke
			// merging in) would then be reported as "queued" for text the
			// transcript's own account says never arrived. Stop polling; more
			// time will not turn a recorded disagreement back into silence.
			d.counters.incr(counterTranscriptDifferentTurnRecorded)
			return false, "a transcript was resolved (" + src.evidence + ") but recorded a DIFFERENT " +
				"turn instead of this delivery's own text within the confirmation window — not " +
				"falling back to the screen signal, which could report this as queued for text " +
				"that, per the transcript's own account, never arrived"
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			d.counters.incr(counterSubmitConfirmTimeout)
			return false, "context cancelled while waiting for the transcript to confirm this delivery"
		case <-time.After(submitConfirmInterval):
		}
	}

	// The transcript's own window closed with nothing matching. Rather than
	// reporting unknown outright — see this function's own doc comment on
	// why that used to cause a duplicate delivery — give the pre-existing
	// screen-based signal one look before giving up: it costs one more
	// capture, and it is exactly the signal that would have confirmed this
	// delivery before D6 existed.
	if confirmed := d.confirmSubmitted(ctx, target.paneID, key, atCount); confirmed {
		d.recordConfirmed(counterSubmitConfirmedByScreenAfterSilentTranscript, d.now().Sub(start))
		return true, "a transcript was resolved (" + src.evidence + ") but recorded no matching turn " +
			"within the confirmation window; confirmed instead by the composer emptying or this " +
			"delivery's own paste marker clearing"
	}
	d.counters.incr(counterSubmitConfirmTimeout)
	return false, "a transcript was resolved (" + src.evidence + ") but recorded no matching turn " +
		"within the confirmation window, and the screen shows neither the composer emptying nor this " +
		"delivery's own paste marker clearing either"
}
