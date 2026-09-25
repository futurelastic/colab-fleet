package tmux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// --- parsePastedTextMarker / transcriptTurnMatches -----------------------

func TestParsePastedTextMarker(t *testing.T) {
	cases := []struct {
		name      string
		s         string
		wantLines int
		wantOK    bool
	}{
		{"with line count", "[Pasted text #3 +12 lines]", 12, true},
		{"singular line", "[Pasted text #1 +1 line]", 1, true},
		{"no line count", "[Pasted text #7]", 0, true},
		{"surrounded by other prose is NOT a bare marker", "see [Pasted text #1 +2 lines] above", 0, false},
		{"ordinary text", "just a normal message", 0, false},
		{"whitespace padded still matches (trimmed)", "  [Pasted text #2 +4 lines]  ", 4, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines, ok := parsePastedTextMarker(c.s)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && lines != c.wantLines {
				t.Errorf("lines = %d, want %d", lines, c.wantLines)
			}
		})
	}
}

func TestTranscriptTurnMatchesExactAndComposingForm(t *testing.T) {
	sent := "Xin chào, đây là một câu tiếng Việt."
	recorded := "Xin chào, đây là một câu tiếng Việt."
	if !transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches did not match across composing-form differences")
	}
}

func TestTranscriptTurnMatchesSenderLabelledExact(t *testing.T) {
	// paneLabelled (#158) prepends a "[from: ...]" line before pasting; every
	// PRODUCTION caller of transcriptTurnMatches (Send, tmux.go) already
	// passes the LABELLED text as `sent`, so the label appears on BOTH sides
	// and an exact match is what actually happens live.
	sent := "[from: alex · session-1 · machine-a]\nplease deploy the staging branch"
	recorded := "[from: alex · session-1 · machine-a]\nplease deploy the staging branch"
	if !transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches did not match a labelled sent text against the same labelled recorded turn")
	}
}

// TestTranscriptTurnMatchesNoLongerAcceptsAPartialLabelSubstring is the
// review fix's own regression test: this driver used to accept an
// UNLABELLED sent as a substring match of a labelled recorded turn — a
// tolerance no production caller ever needed (see the exact-match test
// above) and the exact mechanism the review's adversarial findings rode in
// on (a short sent string matching as a substring of unrelated, longer
// recorded prose: a skill body, a compaction summary, a peer message).
func TestTranscriptTurnMatchesNoLongerAcceptsAPartialLabelSubstring(t *testing.T) {
	sent := "please deploy the staging branch"
	recorded := "[from: alex · session-1 · machine-a]\nplease deploy the staging branch"
	if transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches must require an EXACT match; a substring match is what let a " +
			"skill body, a compaction summary and a peer message all falsely confirm an unrelated send")
	}
}

// TestTranscriptTurnMatchesRejectsMetaSkillBodyContainingSentWord and its two
// siblings are the review's own adversarial findings, each proving the old
// "contains, no minimum length" rule is gone.
func TestTranscriptTurnMatchesRejectsMetaSkillBodyContainingSentWord(t *testing.T) {
	sent := "go"
	recorded := "Here is how the sorting algorithm should go: first partition, then recurse."
	if transcriptTurnMatches(recorded, sent) {
		t.Fatal("a short sent word must not match as a substring of unrelated prose")
	}
}

func TestTranscriptTurnMatchesRejectsWhitespaceStrippedWordBoundaryCross(t *testing.T) {
	sent := "therest"
	recorded := "do the rest of it, please"
	if transcriptTurnMatches(recorded, sent) {
		t.Fatal("whitespace-insensitive comparison must not let a substring cross word boundaries " +
			"once whitespace is stripped from both sides")
	}
}

func TestTranscriptTurnMatchesRejectsBareCollapsedMarkerForAnySend(t *testing.T) {
	// A bare "[Pasted text #1]" (no printed line count) must only confirm a
	// sent text with the SAME (zero) newline count — a single-line paste's
	// own marker shape — never an arbitrary, unrelated multi-line send.
	sent := "line one\nline two\nline three"
	recorded := "[Pasted text #1]"
	if transcriptTurnMatches(recorded, sent) {
		t.Fatal("a bare collapsed-paste marker with no line count must not confirm a multi-line send")
	}
	if !transcriptTurnMatches("[Pasted text #1]", "a single line, no newlines, over 800 bytes long") {
		t.Fatal("a bare collapsed-paste marker SHOULD confirm a single-line (zero-newline) send — " +
			"that is the shape it actually describes")
	}
}

func TestTranscriptTurnMatchesUnwrapsPastedContentTag(t *testing.T) {
	sent := "a fairly long pasted block of text over 800 bytes"
	recorded := `<pasted_content id="7">a fairly long pasted block of text over 800 bytes</pasted_content>`
	if !transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches did not unwrap the runtime's own <pasted_content> tag")
	}
}

func TestTranscriptTurnMatchesAllowsTrailingWakeKeySpace(t *testing.T) {
	sent := "deploy now"
	recorded := "deploy now " // the wake key's own trailing space, tmux.go
	if !transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches did not allow the wake key's own trailing space")
	}
}

func TestTranscriptTurnMatchesPastedTextMarkerWithAgreeingLineCount(t *testing.T) {
	sent := "line one\nline two\nline three"
	recorded := "[Pasted text #4 +2 lines]"
	if !transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches did not accept a pasted-text marker whose line count agrees with the sent text's own newline count")
	}
}

func TestTranscriptTurnMatchesRejectsPastedTextMarkerWithDisagreeingLineCount(t *testing.T) {
	sent := "line one\nline two\nline three" // 2 newlines
	recorded := "[Pasted text #4 +9 lines]"
	if transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches accepted a pasted-text marker whose line count disagrees with the sent text")
	}
}

func TestTranscriptTurnMatchesRejectsUnrelatedText(t *testing.T) {
	if transcriptTurnMatches("something completely different", "the actual sent text") {
		t.Fatal("transcriptTurnMatches matched unrelated text")
	}
}

// --- extractTranscriptText -------------------------------------------

func TestExtractTranscriptTextUserPlainStringContent(t *testing.T) {
	line := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hello there"}}`
	kind, text, ok := extractTranscriptText([]byte(line))
	if !ok || kind != "user" || text != "hello there" {
		t.Fatalf("got kind=%q text=%q ok=%v", kind, text, ok)
	}
}

func TestExtractTranscriptTextUserContentBlocks(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]}}`
	kind, text, ok := extractTranscriptText([]byte(line))
	if !ok || kind != "user" || text != "part one part two" {
		t.Fatalf("got kind=%q text=%q ok=%v", kind, text, ok)
	}
}

func TestExtractTranscriptTextIgnoresAssistantEntries(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":"a reply"}}`
	_, _, ok := extractTranscriptText([]byte(line))
	if ok {
		t.Fatal("extractTranscriptText treated an assistant entry as a candidate turn")
	}
}

func TestExtractTranscriptTextQueueOperationTopLevelText(t *testing.T) {
	// Unverified shape (see terminalpath2_transcript.go's own top comment) —
	// tolerant parsing, exercised here as documentation of the assumption.
	line := `{"type":"queue-operation","operation":"enqueue","text":"queued while busy"}`
	kind, text, ok := extractTranscriptText([]byte(line))
	if !ok || kind != "queue-operation" || text != "queued while busy" {
		t.Fatalf("got kind=%q text=%q ok=%v", kind, text, ok)
	}
}

func TestExtractTranscriptTextMalformedLineIsNotACandidate(t *testing.T) {
	_, _, ok := extractTranscriptText([]byte("not even json"))
	if ok {
		t.Fatal("a malformed line must never be treated as a candidate turn")
	}
}

// --- transcriptTailMatches: the offset is what makes "earlier text must
// not count" structural rather than a rule the scanner has to enforce -----

func TestTranscriptTailMatchesIgnoresTextBeforeTheOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")

	earlierLine := mustJSONLine(t, map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": "the exact same text"},
	})
	if err := os.WriteFile(path, []byte(earlierLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	offset := info.Size() // everything written so far is "earlier"

	// Now append the SAME text again, AFTER the offset.
	laterLine := mustJSONLine(t, map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": "the exact same text"},
	})
	appendLine(t, path, laterLine)

	matched, err := transcriptTailMatches(path, offset, "the exact same text")
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("transcriptTailMatches did not find the turn written AFTER the offset")
	}

	// Sanity: scanning from offset 0 also matches (the earlier line is
	// itself a legitimate match) — confirms the fixture, not the function
	// under test, is what changed between the two calls.
	matchedFromStart, err := transcriptTailMatches(path, 0, "the exact same text")
	if err != nil {
		t.Fatal(err)
	}
	if !matchedFromStart {
		t.Fatal("sanity check failed: scanning from 0 should also match")
	}

	// The real property: a file containing ONLY the earlier (pre-offset)
	// line must NOT match when scanned from the post-offset position.
	onlyEarlier := filepath.Join(dir, "only-earlier.jsonl")
	if err := os.WriteFile(onlyEarlier, []byte(earlierLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	matchedOnlyEarlier, err := transcriptTailMatches(onlyEarlier, offset, "the exact same text")
	if err != nil {
		t.Fatal(err)
	}
	if matchedOnlyEarlier {
		t.Fatal("transcriptTailMatches matched text that exists only BEFORE the recorded offset " +
			"— identical earlier text must not count (task requirement)")
	}
}

func mustJSONLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// appendOnSubmit wraps a fake multiplexer's exec so that line is appended to
// the transcript at path in the call that delivers the submit key (C-m or
// Enter): the moment a real runtime records the turn it was just handed.
//
// What a transcript test needs is an ORDER, not a delay. The line has to land
// after Send has fixed its offset (it resolves the source before the
// keystroke, by contract) and inside the confirmation window. A goroutine that
// sleeps a fixed time promises neither: a scheduler stall before Send reaches
// its offset puts the line ahead of it, the transcript then reads as silent,
// the screen fallback confirms after the whole 4s window, and the
// by-transcript counter reads 0 — a red result that says nothing about the
// code (#207). Tying the append to the keystroke leaves nothing to race.
func appendOnSubmit(t *testing.T, run execFunc, path, line string) execFunc {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		submit := false
		if len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args {
				if a == "C-m" || a == "Enter" {
					submit = true
				}
			}
		}
		out, err := run(ctx, name, args...)
		if submit {
			appendLine(t, path, line)
		}
		return out, err
	}
}

// --- readProcessSessionRecord / resolveTranscriptSource -------------------

func TestReadProcessSessionRecordRejectsIncompleteRecords(t *testing.T) {
	dir := t.TempDir()
	d := &Driver{processSessionsRoot: dir}

	write := func(pid int, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, itoa(pid)+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(100, map[string]any{"pid": 100, "sessionId": "11111111-1111-4111-8111-111111111111", "cwd": "/work/a", "procStart": "Mon Jan  2 15:04:05 2026"})
	if _, ok := d.readProcessSessionRecord(100); !ok {
		t.Fatal("a complete record must be accepted")
	}

	write(101, map[string]any{"pid": 101, "cwd": "/work/a", "procStart": "Mon Jan  2 15:04:05 2026"}) // no sessionId
	if _, ok := d.readProcessSessionRecord(101); ok {
		t.Fatal("a record with no sessionId must be rejected")
	}

	write(102, map[string]any{"pid": 999, "sessionId": "11111111-1111-4111-8111-111111111111", "cwd": "/work/a", "procStart": "Mon Jan  2 15:04:05 2026"}) // pid mismatch
	if _, ok := d.readProcessSessionRecord(102); ok {
		t.Fatal("a record whose own pid field disagrees with the filename must be rejected")
	}

	if _, ok := d.readProcessSessionRecord(9999); ok {
		t.Fatal("a missing file must be rejected, not guessed")
	}
}

func TestReadProcessSessionRecordDisabledWhenRootUnconfigured(t *testing.T) {
	d := &Driver{}
	if _, ok := d.readProcessSessionRecord(1); ok {
		t.Fatal("a driver with no processSessionsRoot configured must never read a real file")
	}
}

// TestResolveTranscriptSourceFallsBackToProcessSessionsFileWhenRecordRootLookupFails
// exercises D6's own gap end to end: the record-root lookup finds nothing
// (no record.jsonl matches the session's name — modelling a resumed session),
// but the process-sessions file resolves the same conversation via the pid.
func TestResolveTranscriptSourceFallsBackToProcessSessionsFileWhenRecordRootLookupFails(t *testing.T) {
	recordRoot := t.TempDir()
	sessionsRoot := t.TempDir()

	cwd := "/work/alpha"
	convID := "0a0b0c0d-0000-4000-8000-00000000abc1"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT titled with the session's name — this is what makes
	// the record-root lookup fail (conversation.go's own resolution rule).
	convPath := filepath.Join(convDir, convID+".jsonl")
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "user", "sessionId": convID, "timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &fakeMux{
		sessions: []fakeSession{
			{name: "resumed💬", paneID: "%9", cwd: cwd, pid: 424242, created: 1785600000, title: "some-other-title"},
		},
		captures: map[string]string{"%9": idleFixtureFor("resumed")},
	}
	fps := &fakePS{}
	// The two clocks disagree on ZONE, not on the instant: `ps` reports this
	// machine's LOCAL wall clock (fakePS.set's own doc comment), while the
	// process-sessions file — written by the runtime's own Node.js process,
	// a different process across a serialisation boundary — is measured
	// (round-1 background; processSessionRecord.ProcStart's own doc comment)
	// to render in UTC. Picking one instant and formatting it once per zone
	// models that honestly, instead of the pre-review fixture's mistake of
	// writing the SAME string for both and only passing on a machine whose
	// local zone happens to be UTC.
	instant := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)
	fps.set(424242, instant)

	if err := os.WriteFile(filepath.Join(sessionsRoot, "424242.json"), []byte(mustJSONLine(t, map[string]any{
		"pid": 424242, "sessionId": convID, "cwd": cwd, "procStart": instant.UTC().Format(psStartTimeLayout),
	})), 0o600); err != nil {
		t.Fatal(err)
	}

	d := New("testbox",
		withExec(f.exec),
		withPSExec(fps.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(recordRoot),
		WithProcessSessionsRoot(sessionsRoot),
	)

	ref := fleet.SessionRef{Machine: "testbox", ID: "resumed💬"}
	target := &paneRow{session: "resumed💬", paneID: "%9", cwd: cwd, pid: 424242, created: time.Unix(1785600000, 0)}

	src, ok := d.resolveTranscriptSource(context.Background(), ref, target)
	if !ok {
		t.Fatal("resolveTranscriptSource did not fall back to the process-sessions file")
	}
	if src.path != convPath {
		t.Errorf("resolved path = %q, want %q", src.path, convPath)
	}
}

func TestResolveTranscriptSourceRefusesOnRecycledPid(t *testing.T) {
	recordRoot := t.TempDir()
	sessionsRoot := t.TempDir()
	cwd := "/work/alpha"

	f := &fakeMux{
		sessions: []fakeSession{
			{name: "resumed💬", paneID: "%9", cwd: cwd, pid: 555, created: 1785600000, title: "some-other-title"},
		},
		captures: map[string]string{"%9": idleFixtureFor("resumed")},
	}
	// The ACTUAL process (per ps) started at a DIFFERENT time than the file
	// on disk claims — the recycled-pid shape #116 already guards against.
	fps := &fakePS{}
	fps.set(555, mustParseProcessStartTime(t, "Tue Feb  3 10:00:00 2026"))

	if err := os.WriteFile(filepath.Join(sessionsRoot, "555.json"), []byte(mustJSONLine(t, map[string]any{
		"pid": 555, "sessionId": "33333333-3333-4333-8333-333333333333", "cwd": cwd, "procStart": "Mon Jan  2 15:04:05 2026",
	})), 0o600); err != nil {
		t.Fatal(err)
	}

	d := New("testbox",
		withExec(f.exec),
		withPSExec(fps.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithRecordRoot(recordRoot),
		WithProcessSessionsRoot(sessionsRoot),
	)

	ref := fleet.SessionRef{Machine: "testbox", ID: "resumed💬"}
	target := &paneRow{session: "resumed💬", paneID: "%9", cwd: cwd, pid: 555, created: time.Unix(1785600000, 0)}

	if _, ok := d.resolveTranscriptSource(context.Background(), ref, target); ok {
		t.Fatal("resolveTranscriptSource trusted a process-sessions file whose procStart disagrees " +
			"with the OS's own current answer for that pid (a recycled pid) — must refuse, not guess")
	}
}

func mustParseProcessStartTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := ParseProcessStartTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// --- confirmSubmittedV2 through Send, end to end --------------------------

// TestSendConfirmsSubmitViaTranscriptWhenOneIsResolvable is D6's end-to-end
// proof: with a record root and a title-matching transcript configured, a
// Send that lands and submits is confirmed by the RUNTIME'S OWN TRANSCRIPT
// recording the turn — not by the screen-based composer-empty/marker signal
// — and the outcome still stays Queued (this file's own documented decision
// on ConfirmsDelivery).
func TestSendConfirmsSubmitViaTranscriptWhenOneIsResolvable(t *testing.T) {
	recordRoot := t.TempDir()
	cwd := "/work/alpha"
	sessionName := "alpha💬"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, "conv-1.jsonl")
	// Title record only, at first — written BEFORE the session's own
	// created time is established below, well within recordDateSlack.
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": sessionName, "sessionId": "conv-1",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const text = "please confirm this via the transcript, not the screen"

	// The confirming turn is appended when the submit key is delivered, so the
	// offset resolveTranscriptSource captured (the file's size before that
	// keystroke) sits BEFORE this line — proving the match is found going
	// FORWARD, never by re-reading something already on disk when the call
	// began. A timed goroutine could not promise that order (#207).
	f := twoSessions()
	d := New("testbox",
		withExec(appendOnSubmit(t, f.exec, convPath, mustJSONLine(t, map[string]any{
			"type": "user", "sessionId": "conv-1",
			"message": map[string]any{"role": "user", "content": text},
		}))),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Now() }),
		WithRecordRoot(recordRoot),
	)
	ref := fleet.SessionRef{Machine: "testbox", ID: sessionName}

	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "transcript") {
		t.Errorf("reason = %q, want it to name the transcript as the confirming evidence", got.Reason)
	}
	snap := d.counters.Snapshot()
	if snap[counterSubmitConfirmedByTranscript] != 1 {
		t.Errorf("counterSubmitConfirmedByTranscript = %d, want 1", snap[counterSubmitConfirmedByTranscript])
	}
}
