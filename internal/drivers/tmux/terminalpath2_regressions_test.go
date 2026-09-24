package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// This file holds the fixes for round 3's findings — live A/B testing
// (P1-P5) and three adversarial reviews (#180 review, #180 review,
// #180 review) against the round-2 terminal-path-v2 prototype
// (commit 9cb796c). Each test is named for the finding it reproduces and
// fails against the pre-fix code (verified individually while writing this file).

// --- #180 review: the composer digest must be resize-tolerant -----

// TestRV_ResumeSurvivesAPaneResizeBetweenStrandAndResume reproduces the
// finding's own example: a long, spaceless token (a job URL) that a narrower
// pane hard-wraps mid-token (no space to drop, so composerText's own
// continuation join inserts one that was never in the original text) and a
// wider pane renders on one row. Both are captures of byte-for-byte the SAME
// underlying composer content — a resize between "this driver stranded a
// delivery" and "a later resumeIfStranded reads the composer back" (a peer's
// client attaching at a different terminal size is the ordinary way this
// happens) must not be read as "something changed the composer since".
//
// Before the fix (raw screenDigest(pending)), the narrow-render digest and
// the wide-render digest disagree — this test failed with OutcomeRefused
// naming a digest mismatch, and no consumer implements the replaceIfStranded
// escape the refusal named (colab-fleet's own #112/#135 deadlock class).
func TestRV_ResumeSurvivesAPaneResizeBetweenStrandAndResume(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	const text = "check .../job/9876543210 and fix it"
	// Narrow pane: the TUI hard-broke the digit run with no space to drop,
	// so composerText's own continuation join inserts one that the original
	// text never had.
	narrow := composerHoldingRows([]string{"check .../job/987654321", "0 and fix it"})
	// Wide pane: same logical text, fits on one row, no join at all.
	wide := composerHoldingRows([]string{text})

	f.setCapture("%1", narrow)
	narrowScreen, ok := d.captureForClassify(context.Background(), "%1")
	if !ok {
		t.Fatal("setup: could not capture the narrow render")
	}
	narrowPending, scan := composerText(narrowScreen)
	if scan != composerFound || narrowPending == "" {
		t.Fatalf("setup: narrow render did not read as a held composer (%v, %q)", scan, narrowPending)
	}
	if narrowPending == text {
		t.Fatalf("setup: the narrow render must NOT already be byte-identical to text — "+
			"want the wrap-inserted-space shape this finding is about, got %q", narrowPending)
	}

	// The delivery stranded while the pane was narrow, recording the digest
	// of what the composer held THEN.
	d.noteStranded(ref.ID, "/work/alpha", text, composerTextDigest(narrowPending))

	// The resize: a wider client attaches (or the same one grows), and the
	// SAME underlying text now renders without the wrap.
	f.setCapture("%1", wide)

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("resume refused on a pane resize alone: %s", got.Reason)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — a resize alone must not block a resume of "+
			"this driver's own unchanged text", got.Outcome, got.Reason)
	}
}

// --- #180 review: the runtime's real <pasted_content> closing tag ---

// TestRV_PastedContentCloseTagCarriesTheSameIdAsTheOpenTag reproduces the
// measured real shape (cc 2.1.281): the closing tag repeats the SAME id
// attribute the opening tag carries, not a bare `</pasted_content>`. Before
// the fix, normalizeTranscriptText left this trailing tag intact, so a
// transcript entry carrying a wrapped, real paste never normalised down to
// its bare text and never matched sent — exactly the messages most likely to
// strand (long enough to trigger the wrapper) fell straight through to the
// weaker screen signal every time.
func TestRV_PastedContentCloseTagCarriesTheSameIdAsTheOpenTag(t *testing.T) {
	recorded := "before\n\n<pasted_content id=\"c702\">\nthe pasted body\n</pasted_content id=\"c702\">\nafter"
	sent := "before\n\nthe pasted body\nafter "
	if !transcriptTurnMatches(recorded, sent) {
		t.Fatal("transcriptTurnMatches did not unwrap the runtime's own id-bearing closing tag " +
			"(</pasted_content id=\"...\">) — real transcripts use this shape, not a bare " +
			"</pasted_content>")
	}
}

// --- #180 review / #180 review / #180 review: the ---
// --- marker path must not submit a person's draft on a strict resume ------

// TestRV_StrictResumeDoesNotSubmitAPersonsCollapsedPasteViaMarkerPath is the
// #180 review reproduction: a delivery strands with an EMPTY recorded
// digest (the composer was unreadable at strand time — a modal covering it,
// or the ctx already expired), and by the time of resume, a PERSON has
// pasted their own, unrelated block into the composer, which collapses to
// exactly the marker shape (`[Pasted text #1 +12 lines]`) this driver's own
// marker/gained attribution was built to recognise. Passing map[pasteKey]int{}
// as `before` on every resume means ANY single marker present "gains" from
// zero — there is no way, from an empty before-map alone, to tell the
// person's draft apart from this driver's own earlier delivery.
func TestRV_StrictResumeDoesNotSubmitAPersonsCollapsedPasteViaMarkerPath(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const strandedText = "the delivery this driver made and could not confirm"

	// Empty digest: this driver could not read a definite composer state at
	// strand time (see strandedRecord.ComposerDigest's own doc comment for
	// why an empty digest deliberately does not refuse the resume outright).
	d.noteStranded(ref.ID, "/work/alpha", strandedText, "")

	// A person's own, unrelated collapsed paste sits in the composer now.
	f.setCapture("%1", composerHolding("[Pasted text #1 +12 lines]"))

	got, err := d.Send(context.Background(), testCaller, ref, strandedText,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued {
		t.Fatalf("outcome = queued (%s) — this driver submitted a PERSON'S collapsed paste as "+
			"if it were its own stranded delivery, via the marker path's empty before-map", got.Reason)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			for _, a := range call {
				if a == "C-m" {
					t.Fatalf("Enter was pressed against a person's draft: %v", call)
				}
			}
		}
	}
}

// TestRV_FreshSendMarkerAttributionStillWorks pins that disabling the marker
// path on a STRICT resume did not disable it for an ordinary FRESH send —
// P1's own fix (a bare, count-less "[Pasted text #N]" for a collapsed
// single-line paste over ~800B) must keep working; only the resume path's
// use of an empty before-map was ever the hazard. Exercised directly against
// confirmLandedV2 (strict=false, the fresh-send shape), the same level
// terminalpath2_test.go's own existing tests already use.
func TestRV_FreshSendMarkerAttributionStillWorks(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	text := strings.Repeat("x", 900)

	f.setCapture("%1", idleFixtureFor("alpha"))
	// The runtime always draws a collapsed marker on its own composer-prompt
	// row (the ❯-prefixed row markerCounts requires) — round-1's own
	// background evidence: 'Composer capture: ["❯ [Pasted text #1]"]'.
	f.pasted = map[string]string{"%1": "❯ [Pasted text #1]"} // no line count (P1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, landed := d.confirmLandedV2(ctx, "%1", text, map[pasteKey]int{}, false, false)
	if !landed {
		t.Fatal("a fresh send's own bare collapsed-paste marker (no line count) must still " +
			"confirm landed — only the RESUME path's use of an empty before-map is the hazard " +
			"this file's other tests close")
	}
}

// --- #180 review: resumeIfStranded must not clear an unrecorded composer ---

// TestRV_ResumeIfStrandedRefusesWhenItsOwnRecordWasForgotten reproduces the
// review's own sequence: a send strands (unknown), Discard runs against a
// composer that is now genuinely empty (round-2's own fix: forget the record
// on a CONFIRMED empty composer) and forgets the record, a PERSON then types
// their own draft, and the caller retries EXACTLY as the original "unknown"
// receipt instructed — resumeIfStranded, same text. Before the fix, #135's
// original "no record => clear and deliver" door was still open to
// resumeIfStranded alone, so this retry cleared the person's draft with C-u
// and reported queued.
func TestRV_ResumeIfStrandedRefusesWhenItsOwnRecordWasForgotten(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	// The record is simply absent — the same state Discard's own forget (or
	// a confirmed send elsewhere clearing an unrelated record) leaves
	// behind. A person has since typed their own draft.
	f.setCapture("%1", composerHolding("PERSONDRAFT a half-typed note from a person"))

	got, err := d.Send(context.Background(), testCaller, ref, "the agent's own text",
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued {
		t.Fatalf("outcome = queued (%s) — resumeIfStranded with no record cleared and submitted "+
			"over a person's draft", got.Reason)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			for _, a := range call {
				if a == "C-u" || a == "BSpace" {
					t.Fatalf("a clear keystroke was sent against a person's draft with no "+
						"stranded record to justify it: %v", call)
				}
			}
		}
	}
	if !strings.Contains(got.Reason, "replaceIfStranded") {
		t.Errorf("reason = %q; must point at replaceIfStranded as the explicit door for "+
			"discarding whatever is there", got.Reason)
	}
}

// TestReplaceIfStrandedRefusesAnUnrecordedComposerWithoutExpect (#180 M9,
// the draft rule): with no record, replaceIfStranded alone is a wish, not
// proof — the composer may hold a person's draft, and only the send grant
// stood between it and C-u. The caller's expect digest, matching the
// composer now, is the proof that opens the door; a stale one keeps it shut.
func TestReplaceIfStrandedRefusesAnUnrecordedComposerWithoutExpect(t *testing.T) {
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const draft = "some unrecorded text"
	for _, tc := range []struct {
		name   string
		expect string
		want   fleet.Outcome
	}{
		{"no expect", "", fleet.OutcomeRefused},
		{"stale expect", composerTextDigest("what the caller saw earlier"), fleet.OutcomeRefused},
		{"matching expect", composerTextDigest(draft), fleet.OutcomeQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			f.setCapture("%1", composerHolding(draft))
			got, err := d.Send(context.Background(), testCaller, ref, "the replacement text",
				driver.SendOptions{Submit: true, ReplaceIfStranded: true, ExpectComposerDigest: tc.expect})
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %s (%s), want %s", got.Outcome, got.Reason, tc.want)
			}
			if tc.want == fleet.OutcomeRefused {
				if n := countClears(f.callsSnapshot()); n != 0 {
					t.Fatalf("%d clear keystroke(s) pressed on text nobody proved was anyone's to clear", n)
				}
				if n := d.Counters()["delivery.tmux.refused.draft_kept"]; n != 1 {
					t.Fatalf("delivery.tmux.refused.draft_kept = %d, want 1", n)
				}
			}
		})
	}
}

// --- #180 review: the dialog gate and the needle/marker checks must ----
// --- read the SAME capture, not two independent ones ------------------------

// TestRV_ConfirmLandedV2MakesOnlyOneCaptureEachPoll pins the fix's own
// mechanism directly: confirmLandedV2 must never issue its own SEPARATE
// `capture-pane -J` call on top of captureForClassify's — the review's own
// reproduction relied on exactly that second, independent subprocess call
// racing a modal into view between the two. Asserted structurally (no `-J`
// flag appears on any capture-pane call this function makes) rather than by
// timing, which a fake multiplexer cannot model.
func TestRV_ConfirmLandedV2MakesOnlyOneCaptureEachPoll(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	f.setCapture("%1", idleFixtureFor("alpha"))
	f.pasted = map[string]string{"%1": "hello"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, landed := d.confirmLandedV2(ctx, "%1", "hello", map[pasteKey]int{}, false, false)
	if !landed {
		t.Fatal("setup: expected the legacy needle to confirm landed against the pasted text")
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "capture-pane" {
			for _, a := range call {
				if a == "-J" {
					t.Fatalf("confirmLandedV2 made its own -J capture-pane call: %v — the needle/"+
						"marker checks must read the SAME capture captureForClassify already took, "+
						"never a second, independently-timed one", call)
				}
			}
		}
	}
}

// --- #180 review: a numbered composer message is not a menu --------

// TestRV_NumberedTwoLineComposerMessageIsNotReadAsAMenu reproduces the
// finding: a multi-line message typed into a real composer, whose first line
// happens to start with "1." and second with "2.", used to parse as a
// two-option menu with option 1 "selected" (the composer's own ❯ leader is
// indistinguishable from a menu's highlighted-option marker without also
// checking whether a real composer fence is what is actually there).
func TestRV_NumberedTwoLineComposerMessageIsNotReadAsAMenu(t *testing.T) {
	render := composerHoldingRows([]string{
		"1. merge PR 12 once CI is green",
		"2. then deploy staging",
	})
	sc := newScreen(render)
	if awaitingSelection(sc) {
		t.Fatal("a numbered, multi-line COMPOSER message was read as a blocking selection menu")
	}
	if p := parsePrompt(sc); p != nil && len(p.Options) > 0 {
		t.Fatalf("parsePrompt found a menu (%d options) inside a composer's own numbered text", len(p.Options))
	}
}

// TestRV_ARealMenuIsStillRecognisedNextToAComposerlessScreen pins that the
// masking above only ever removes rows a genuine composer owns — an actual
// selection menu (composerAbsent, since nothing fences it) must still be
// recognised exactly as before.
func TestRV_ARealMenuIsStillRecognisedNextToAComposerlessScreen(t *testing.T) {
	sc := newScreen(fixtureMenu)
	if !awaitingSelection(sc) {
		t.Fatal("a genuine selection menu was no longer recognised after the composer-masking fix")
	}
}

// --- #180 review / D7: route:"terminal" must not launder an unlabelled --
// --- agent send as human-typed input --------------------------------------

// (http-level test lives in internal/service; see http_route_terminal_test.go)

// --- #180 review: a dequeue is only attributed to THIS delivery --
// --- if an enqueue for the same text was ALSO seen after this delivery's ---
// --- own offset -------------------------------------------------------------

// TestRV_EarlierIdenticalDequeueDoesNotConfirmASwallowedSubmit reproduces the
// review's own repro: an identical message enqueued BEFORE this delivery's
// offset (a routine re-ping, or a consumer.s automatic resumeIfStranded
// retry), whose DEQUEUE lands AFTER the offset — inside this delivery's own
// confirmation window — even though THIS delivery's own Enter was swallowed
// and produced no enqueue of its own at all. Before the fix, the dequeue's
// text matched sent with no regard for which enqueue it belonged to, so a
// swallowed submit was confirmed as queued and the caller's own retry then
// delivered a duplicate.
func TestRV_EarlierIdenticalDequeueDoesNotConfirmASwallowedSubmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")

	earlierEnqueue := mustJSONLine(t, map[string]any{
		"type": "queue-operation", "operation": "enqueue", "content": "the repeated text",
	})
	if err := os.WriteFile(path, []byte(earlierEnqueue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	offset := info.Size() // this delivery's own offset — AFTER the earlier enqueue

	laterDequeue := mustJSONLine(t, map[string]any{
		"type": "user", "promptSource": "queued",
		"message": map[string]any{"role": "user", "content": "the repeated text"},
	})
	appendLine(t, path, laterDequeue)

	result, err := transcriptTailScan(path, offset, "the repeated text")
	if err != nil {
		t.Fatal(err)
	}
	if result == transcriptScanMatched {
		t.Fatal("an identical message enqueued BEFORE this delivery's own offset must not " +
			"confirm THIS delivery's submit when only its dequeue lands inside the window " +
			"(no enqueue for it was ever seen AFTER the offset)")
	}
}

// TestRV_OwnEnqueueAfterOffsetStillConfirmsQuickly pins the fast path stays
// fast: THIS delivery's own enqueue, seen strictly after offset, confirms
// immediately without waiting for its eventual dequeue.
func TestRV_OwnEnqueueAfterOffsetStillConfirmsQuickly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	offset := info.Size()

	ownEnqueue := mustJSONLine(t, map[string]any{
		"type": "queue-operation", "operation": "enqueue", "content": "PROBE this delivery's own text",
	})
	appendLine(t, path, ownEnqueue)

	result, err := transcriptTailScan(path, offset, "PROBE this delivery's own text")
	if err != nil {
		t.Fatal(err)
	}
	if result != transcriptScanMatched {
		t.Fatalf("result = %v, want matched — this delivery's own enqueue, seen after its own "+
			"offset, must confirm without waiting for the dequeue", result)
	}
}

// --- #180 review: a transcript recording a DIFFERENT turn is -----
// --- not silence, and must not be papered over by the screen fallback ------

func TestRV_TranscriptDifferentTurnIsNotTreatedAsSilence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	offset := info.Size()

	// The runtime recorded only the TAIL of the message (the D3 shape) —
	// a candidate turn, attributable to this window, that does not match.
	diffLine := mustJSONLine(t, map[string]any{
		"type": "user", "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": "only the tail of the message"},
	})
	appendLine(t, path, diffLine)

	result, err := transcriptTailScan(path, offset, "the full original message, longer than its tail")
	if err != nil {
		t.Fatal(err)
	}
	if result != transcriptScanDifferentTurn {
		t.Fatalf("result = %v, want transcriptScanDifferentTurn — a recorded turn that does not "+
			"match sent must not be reported as silence", result)
	}
}

// TestRV_ConfirmSubmittedDoesNotFallBackToScreenOnADifferentRecordedTurn is
// the end-to-end version: even though the SCREEN would confirm (composer
// reads empty, the pre-existing weaker signal), a transcript that recorded a
// DIFFERENT turn must not be treated as the silence that fallback exists
// for — the caller would otherwise read "queued" for text the transcript's
// own account says never arrived.
func TestRV_ConfirmSubmittedDoesNotFallBackToScreenOnADifferentRecordedTurn(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)

	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	src := transcriptSource{path: path, offset: info.Size(), evidence: "test fixture"}

	diffLine := mustJSONLine(t, map[string]any{
		"type": "user", "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": "a different, unrelated turn"},
	})
	appendLine(t, path, diffLine)

	// The screen ALSO shows an empty composer — confirmSubmitted (the
	// pre-existing fallback) would confirm on this alone.
	f.setCapture("%1", idleFixtureFor("alpha"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	confirmed, evidence, _ := d.confirmSubmittedFromSource(ctx, &paneRow{paneID: "%1"},
		"the actual sent text", pasteKey{}, 0, src, true)
	if confirmed {
		t.Fatalf("confirmed = true (%s), want false — a transcript recording a DIFFERENT turn "+
			"must not be papered over by the screen's own composer-empty signal", evidence)
	}
	if !strings.Contains(evidence, "DIFFERENT") {
		t.Errorf("evidence = %q; must say a different turn was recorded, not merely silence", evidence)
	}
}

// --- #180 review: resumeIfStranded on an EMPTY composer must -----
// --- check this driver's own transcript record before re-pasting -----------

// TestRV_ResumeOnEmptyComposerConfirmsFromItsOwnTranscriptRecord reproduces
// the finding: the composer this driver stranded text into no longer holds
// it (composerFound, pending==""), and this driver's own transcript record
// (resolved and stored at strand time) shows the runtime already accepted
// it. Before the fix, Send had no gate at all for this shape and fell
// straight through to an ordinary fresh paste — delivering a byte-for-byte
// duplicate.
func TestRV_ResumeOnEmptyComposerConfirmsFromItsOwnTranscriptRecord(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the delivery this driver made and could not confirm"

	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	strandOffset := info.Size()

	// The record this driver made when the delivery stranded, WITH its own
	// transcript path/offset — exactly noteStrandedWithTranscript's shape.
	d.noteStrandedWithTranscript(ref.ID, "/work/alpha", text, "",
		transcriptSource{path: path, offset: strandOffset, evidence: "test fixture"}, true)

	// Between the strand and the resume, the runtime accepted it — recorded
	// in the transcript AND the composer emptied.
	acceptedLine := mustJSONLine(t, map[string]any{
		"type": "user", "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": text},
	})
	appendLine(t, path, acceptedLine)
	f.setCapture("%1", idleFixtureFor("alpha")) // composer now reads empty

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — this driver's own transcript record shows "+
			"the runtime already accepted this text", got.Outcome, got.Reason)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "load-buffer" {
			t.Fatalf("a fresh paste was made even though this driver's own transcript record "+
				"already showed this text accepted: %v", call)
		}
	}
}

// TestRV_ResumeOnEmptyComposerRefusesWhenUnconfirmed is the negative
// sibling: no transcript could be resolved at strand time (TranscriptPath
// empty, the ordinary noteStranded shape every existing caller still uses),
// so the resume must refuse rather than silently pasting the same text
// again as an ordinary fresh delivery.
func TestRV_ResumeOnEmptyComposerRefusesWhenUnconfirmed(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the delivery this driver made and could not confirm"

	d.noteStranded(ref.ID, "/work/alpha", text, "") // no transcript recorded
	f.setCapture("%1", idleFixtureFor("alpha"))     // composer now reads empty

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued {
		t.Fatalf("outcome = queued (%s) — an unconfirmable resume on an empty composer must "+
			"not silently deliver a fresh paste of the same text", got.Reason)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "load-buffer" {
			t.Fatal("a fresh paste was made for an unconfirmable resume — exactly the " +
				"duplicate-delivery hazard this fix exists to prevent")
		}
	}
}

// --- #180 review: a digest-verified resume may still finish a real --
// --- 7-row composer's own tail-only render ---------------------------------

// TestRV_DigestVerifiedResumeFinishesATailOnlyComposerRender reproduces the
// #143/M2 shape: Claude Code's own composer caps at a fixed row count and
// scrolls INSIDE it, so a resume's re-read of a real, tall composer can see
// only the TAIL of what this driver sent. Before the fix, strict=true
// disabled composerRegionMatch's suffix rule unconditionally on every
// resume, so this could never confirm — even though the record's own
// ComposerDigest, verified equal to the composer's CURRENT digest, already
// proves this is exactly the composer this driver's own earlier attempt
// stranded into (the person's-unrelated-draft hazard the suffix rule is
// otherwise refused for cannot apply here).
func TestRV_DigestVerifiedResumeFinishesATailOnlyComposerRender(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	const text = "a message long enough that a real, row-capped composer would " +
		"scroll internally and show only its own tail once it lands"
	// Only the TAIL is visible — the composer's own internal scroll, not
	// tmux's `-S` history margin (composerClipped's own, different shape).
	tailOnly := composerHoldingRows([]string{"show only its own tail once it lands"})

	f.setCapture("%1", tailOnly)
	tailScreen, ok := d.captureForClassify(context.Background(), "%1")
	if !ok {
		t.Fatal("setup: could not capture the tail-only render")
	}
	tailPending, scan := composerText(tailScreen)
	if scan != composerFound || tailPending == "" {
		t.Fatalf("setup: tail-only render did not read as a held composer (%v, %q)", scan, tailPending)
	}

	// The record this driver made when ITS OWN earlier attempt stranded —
	// the digest of the SAME tail-only render it saw at that moment.
	d.noteStranded(ref.ID, "/work/alpha", text, composerTextDigest(tailPending))

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — a digest-verified resume must be able to "+
			"finish a real composer's own tail-only render", got.Outcome, got.Reason)
	}
}

// TestRV_UnverifiedResumeStillRefusesATailOnlyComposerMatch is the negative
// sibling: an EMPTY recorded digest (this driver never corroborated the
// composer's content at strand time) must keep refusing a tail-only suffix
// match — digestVerified is never assumed from silence.
func TestRV_UnverifiedResumeStillRefusesATailOnlyComposerMatch(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	const text = "a message long enough that a real, row-capped composer would " +
		"scroll internally and show only its own tail once it lands"
	tailOnly := composerHoldingRows([]string{"show only its own tail once it lands"})
	f.setCapture("%1", tailOnly)

	d.noteStranded(ref.ID, "/work/alpha", text, "") // no digest recorded

	got, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued {
		t.Fatal("outcome = queued — an UNVERIFIED resume must not accept a tail-only suffix " +
			"match; only a digest-verified one may")
	}
}

// --- #180 review: the window resolveTranscriptSource itself opens ------
// --- before the submit keystroke must be re-checked for a dialog -----------

// TestRV_DialogAppearingJustBeforeSubmitIsNotApproved models a selection
// menu appearing on the pane strictly AFTER confirmLandedV2's own landed
// check succeeded (via the fake's dialog-arming hook, which changes what the
// NEXT capture-pane call sees) — the exact gap resolveTranscriptSource opens
// between that check and the submit keystroke. Before the fix, nothing
// re-checked the screen in that gap, so Enter answered the menu instead of
// submitting this delivery.
func TestRV_DialogAppearingJustBeforeSubmitIsNotApproved(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "hello"

	menu := "  1. Yes, proceed\n  2. No, cancel\n" + rule + "\n  Enter to select · Tab/Arrow keys to navigate"

	// capturesSincePaste tracks capture-pane calls from the moment the paste
	// lands: the FIRST one is confirmLandedV2's own poll (which must still
	// see the pasted text, so it returns landed=true — this is NOT the
	// window this test is about); the SECOND is refuseIfDialogAppeared's own
	// fresh look immediately before send-keys, modelling a menu that painted
	// in the gap resolveTranscriptSource itself opens. Everything before the
	// paste (the readiness gate, the pending-composer re-read) is untouched.
	capturesSincePaste := -1
	wrapped := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "load-buffer" {
			out, err := f.exec(ctx, name, args...)
			capturesSincePaste = 0
			return out, err
		}
		if len(args) > 0 && args[0] == "capture-pane" && capturesSincePaste >= 0 {
			capturesSincePaste++
			if capturesSincePaste >= 2 {
				f.mu.Lock()
				f.captures["%1"] = menu
				f.mu.Unlock()
			}
		}
		return f.exec(ctx, name, args...)
	}
	dd := New("testbox", withExec(wrapped), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	_ = d // unused now that dd is the driver under test; kept for readability of the diff

	got, err := dd.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) >= 4 && call[0] == "send-keys" && call[len(call)-1] == "C-m" {
			t.Fatalf("Enter was sent while a selection menu was showing: %v (outcome %s: %s)",
				call, got.Outcome, got.Reason)
		}
	}
}
