package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// This file holds the regression tests for the second round of review
// findings against terminal-path-v2 (colab-fleet round-1 D1-D7 prototype).
// Each test names, in its own doc comment, the finding it reproduces and
// proves fixed. See terminalpath2.go / terminalpath2_transcript.go /
// terminalpath2_lock.go / inputguard.go for the fixes themselves.

// --- #6/#24: the "!" shell guard must see what actually gets pasted -------

// TestSendGuardSeesSanitisedBytesNotRawOnes reproduces the review's
// TestReview_GuardBypassViaSanitiser / TestReviewGuardSeesWhatIsPasted: a
// control byte the guard's own trim did not recognise, but the sanitiser
// dropped anyway before pasting, used to manufacture a leading "!" the guard
// never saw. Sanitising once, before the guard runs (Send, tmux.go), closes
// this: the guard now sees exactly what will be pasted.
func TestSendGuardSeesSanitisedBytesNotRawOnes(t *testing.T) {
	cases := []string{
		"\v!id",
		"\f !id",
		"\x00!touch /tmp/pwned",
		"\x1b!id",
		"\x7f!id",
		"\x01\x02!id",
		" !id", // NBSP — not a control byte, caught by the widened trim instead
	}
	for _, raw := range cases {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			f := twoSessions()
			d := newTestDriver(f)
			ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

			got, err := d.Send(context.Background(), testCaller, ref, raw, driver.SendOptions{Submit: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != fleet.OutcomeRefused {
				t.Fatalf("outcome = %s (%s), want refused — sanitising before the guard must catch this", got.Outcome, got.Reason)
			}
			if f.pasted["%1"] != "" {
				t.Errorf("pasted = %q, want nothing pasted at all for a refused send", f.pasted["%1"])
			}
		})
	}
}

// --- #11/#19/P3: one sanitised string used for the guard, the paste, and
// every comparison ----------------------------------------------------------

// TestSendSanitisesOnceAndConfirmsANonPrintableMessage reproduces
// TestReview_SanitisedVsUnsanitisedCompare / P3 (control/ctrl-esc201,
// control/ctrl-mixed): a message containing control bytes used to be pasted
// in its SANITISED form but compared against the RAW form everywhere else
// (composerRegionMatch, the legacy needle, transcriptTurnMatches, the
// stranded record), so it could never confirm and a resume delivered it a
// second time. Sanitising once, at the top of Send, and using that one
// string throughout removes the mismatch structurally.
func TestSendSanitisesOnceAndConfirmsANonPrintableMessage(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	raw := "build log says \x1b[31mFAILED\x1b[0m and stopped"
	wantPasted := "build log says [31mFAILED[0m and stopped" // ESC dropped, everything else kept

	// Submit:false, deliberately: a successful SUBMIT clears fakeMux's own
	// f.pasted record (modelling a real composer emptying), which would
	// hide the very thing this test checks. Leaving the text queued in the
	// composer keeps it there to inspect, and still proves the point: the
	// bytes that actually landed are the sanitised ones.
	got, err := d.Send(context.Background(), testCaller, ref, raw, driver.SendOptions{Submit: false})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — a control byte must not leave this delivery "+
			"permanently unconfirmable", got.Outcome, got.Reason)
	}
	if f.pasted["%1"] != wantPasted {
		t.Errorf("pasted = %q, want the sanitised form %q", f.pasted["%1"], wantPasted)
	}

	// And the outcome-confirmed path, unaffected by the paste map cleanup:
	// a SUBMITTED sanitised message must also confirm, not stick at unknown.
	f2 := twoSessions()
	d2 := newTestDriver(f2)
	got2, err := d2.Send(context.Background(), testCaller, ref, raw, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got2.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued for a SUBMITTED sanitised message too", got2.Outcome, got2.Reason)
	}
}

// --- #9/#17: the legacy needle and dialogs -------------------------------

// TestConfirmLandedV2RefusesALandedMatchWhileASelectionMenuIsShowing
// reproduces the review's TestReview_LegacyNeedleLandsOnModalAndApprovesIt: a
// permission dialog appearing exactly when this driver polls must not have
// the OLD raw-pane needle mistake the dialog's own visible text (or text
// still visible in the transcript above it) for this delivery having landed
// — which would go on to press Enter and approve whatever the dialog is
// showing instead of submitting the message.
func TestConfirmLandedV2RefusesALandedMatchWhileASelectionMenuIsShowing(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	// The needle's own last-24-bytes shape: pick text whose tail is a
	// literal substring already sitting inside the menu fixture, modelling
	// "this text is separately visible on screen" without a fresh paste at
	// all — the exact shape the legacy needle cannot tell apart from a live
	// re-render.
	text := "please pick the-first-option (Recommended)"
	f.setCapture("%1", fixtureMenuSelected)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, _, landed := d.confirmLandedV2(ctx, "%1", text, map[pasteKey]int{}, false, false)
	if landed {
		t.Fatal("confirmLandedV2 confirmed landed while a selection menu was showing on the very " +
			"same capture — this is the dialog-approval race")
	}
	snap := d.counters.Snapshot()
	if snap[counterSendRefusedDialogRace] == 0 {
		t.Errorf("counterSendRefusedDialogRace = %d, want at least 1", snap[counterSendRefusedDialogRace])
	}
}

// TestConfirmLandedV2LegacyNeedleOnlyFiresWhenAComposerIsActuallyFound is the
// positive counterpart: composerAbsent (no fenced composer at all — the
// review's own reproduction shape) must never let the legacy needle
// confirm, while a genuinely composerFound-but-unmatching screen (this
// package's own default paste model) still may — unchanged from before this
// review pass.
func TestConfirmLandedV2LegacyNeedleOnlyFiresWhenAComposerIsActuallyFound(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	text := "the-first-option (Recommended)"
	// composerAbsent: a menu, no fence at all.
	f.setCapture("%1", fixtureMenuSelected)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, _, landed := d.confirmLandedV2(ctx, "%1", text, map[pasteKey]int{}, false, false); landed {
		t.Fatal("legacy needle must not fire against a composerAbsent screen")
	}
}

// --- #8/#26/#27: resume must never submit a person's unrelated draft ------

// TestResumeNeverSubmitsAPersonsDraftEvenWhenTheOriginalIsVisibleElsewhere
// reproduces the review's TestReviewResumeSubmitsPersonsDraftViaEmptyDigest-
// AndLegacyNeedle / TestReviewResumeNeverSubmitsAPersonsDraft: an empty
// recorded ComposerDigest (this driver's own primary, intended resume case)
// must not let confirmLandedV2's OLD legacy-needle fallback confirm a
// resume against a person's own, unrelated draft just because this
// delivery's earlier text is separately visible somewhere in the same raw
// capture — the exact shape a stranded-then-actually-delivered message
// leaves behind. strict=true on the resume call (terminalpath2.go) closes
// this by disabling the legacy needle (and the suffix rule) for resume
// entirely, requiring composerRegionMatch's own exact-equality check.
func TestResumeNeverSubmitsAPersonsDraftEvenWhenTheOriginalIsVisibleElsewhere(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // never echoes the paste — currentComposerDigest reads "" at strand time
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const original = "deploy the staging branch when the tests pass"

	strand, err := d.Send(context.Background(), testCaller, ref, original, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", strand.Outcome)
	}
	rec, ok := d.strandedRecordFor("alpha💬", "/work/alpha")
	if !ok || rec.ComposerDigest != "" {
		t.Fatalf("setup: rec = %+v, ok=%v — want a record with an EMPTY ComposerDigest", rec, ok)
	}

	// A person now types something unrelated into the composer. The
	// ORIGINAL text is also visible elsewhere in the very same capture (past
	// the composer's closing fence, modelling either this package's own
	// paste-append test shape or a transcript row above an actually-cleared
	// composer) — the shape the OLD legacy needle could not tell apart from
	// a live re-render of THIS delivery.
	f.setCapture("%1", composerHolding("WAIT do not deploy, the db is still migrating"))
	f.pasted["%1"] = original

	got, err := d.Send(context.Background(), testCaller, ref, original,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %s (%s), want unknown — resume must refuse rather than press Enter on "+
			"a person's unrelated draft", got.Outcome, got.Reason)
	}
	if countCalls(f, "send-keys") != 0 {
		t.Errorf("send-keys was called %d times — Enter must never be pressed here", countCalls(f, "send-keys"))
	}
	if _, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); !ok {
		t.Error("the stranded record must be KEPT on a refused resume, not discarded")
	}
}

// TestResumeRefusesATailOnlyComposerMatch reproduces the review's
// TestReviewResumeRefusesATailOnlyComposer: composerRegionMatch's own
// scrolled-composer suffix rule is disabled for resume (allowSuffix=false),
// so a composer holding only the tail of the stranded text is no longer
// treated as a confirmed, complete re-render.
func TestResumeRefusesATailOnlyComposerMatch(t *testing.T) {
	f := twoSessions()
	f.noEcho = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const original = "please deploy the staging branch now, thanks very much for handling this"

	strand, err := d.Send(context.Background(), testCaller, ref, original, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strand.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown", strand.Outcome)
	}

	tail := original[len(original)-30:] // a substantial (>= composerSuffixMinBytes) suffix
	f.setCapture("%1", composerHolding(tail))

	got, err := d.Send(context.Background(), testCaller, ref, original,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("outcome = %s (%s), want unknown — a tail-only composer must not confirm a resume", got.Outcome, got.Reason)
	}
	if countCalls(f, "send-keys") != 0 {
		t.Error("Enter must never be pressed against a tail-only composer match on resume")
	}
}

// --- #4/review-safety: Discard must not forget a stranded record on a
// composerAbsent screen (a dialog may be covering real, unfinished text) ---

// TestDiscardDoesNotForgetAStrandedRecordWhenNoComposerIsFoundAtAll
// reproduces the review's finding that Discard's "already clear" branch
// forgot a stranded record even when scan==composerAbsent (a dialog, not a
// confirmed-empty composer) — a screen with no fenced composer at all is not
// proof this driver's own stranded text went away; it may reappear the
// moment the dialog closes.
func TestDiscardDoesNotForgetAStrandedRecordWhenNoComposerIsFoundAtAll(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	d.noteStranded("alpha💬", "/work/alpha", "some earlier stranded text", "")

	// composerAbsent: a full-screen dialog, no fence at all — NOT the same
	// fact as "the composer emptied".
	f.setCapture("%1", fixtureMenuSelected)

	ack, err := d.Discard(context.Background(), testCaller, ref, "", driver.DiscardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatal("Discard was not accepted for a composerAbsent screen (unchanged baseline behaviour)")
	}
	if _, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); !ok {
		t.Fatal("Discard forgot a stranded record on a composerAbsent (dialog) screen — this is not " +
			"proof the stranded text is gone")
	}
}

// TestDiscardStillForgetsAStrandedRecordOnAConfirmedEmptyComposer is the
// positive counterpart, restated here for contrast with the test
// immediately above (TestDiscardForgetsAStrandedRecordItClears,
// terminalpath2_test.go, covers the "cleared via an active C-u pass" shape;
// this one covers the "already found empty" shape the review's finding was
// specifically about).
func TestDiscardStillForgetsAStrandedRecordOnAConfirmedEmptyComposer(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	d.noteStranded("alpha💬", "/work/alpha", "some earlier stranded text", "")
	// idleFixtureFor's own composer is composerFound and empty.

	ack, err := d.Discard(context.Background(), testCaller, ref, "", driver.DiscardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatal("Discard was not accepted")
	}
	if _, ok := d.strandedRecordFor("alpha💬", "/work/alpha"); ok {
		t.Fatal("Discard must forget a stranded record on a CONFIRMED empty composer")
	}
}

// --- #7/#14/#21/P2: busy-session queue-operation must confirm, not duplicate

// TestSendConfirmsSubmitViaARealQueueOperationEnqueue reproduces the
// review's core busy-session finding: Claude Code's own writer puts a
// queued turn's text in a top-level "content" field on operation=="enqueue"
// (round-1 research), which extractTranscriptText did not read at all
// before this fix — every busy-session send fell back to "unknown", and the
// documented resumeIfStranded retry delivered it a second time. This proves
// the real shape is now recognised and confirms the submit directly,
// without ever going through a resume at all.
func TestSendConfirmsSubmitViaARealQueueOperationEnqueue(t *testing.T) {
	recordRoot := t.TempDir()
	cwd := "/work/alpha"
	sessionName := "alpha💬"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, "conv-1.jsonl")
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": sessionName, "sessionId": "conv-1",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := twoSessions()
	d := New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Now() }),
		WithRecordRoot(recordRoot),
	)

	const text = "PROBE busy session: queued while the runtime is generating"
	ref := fleet.SessionRef{Machine: "testbox", ID: sessionName}

	go func() {
		time.Sleep(30 * time.Millisecond)
		appendLine(t, convPath, mustJSONLine(t, map[string]any{
			"type": "queue-operation", "operation": "enqueue", "sessionId": "conv-1",
			"timestamp": time.Now().Format(time.RFC3339Nano),
			"content":   text,
		}))
	}()

	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — a real enqueue queue-operation must confirm the "+
			"submit directly, not report unknown and force a duplicating resume", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "transcript") {
		t.Errorf("reason = %q, want it to name the transcript as the confirming evidence", got.Reason)
	}
	snap := d.counters.Snapshot()
	if snap[counterSubmitConfirmedByTranscript] != 1 {
		t.Errorf("counterSubmitConfirmedByTranscript = %d, want 1", snap[counterSubmitConfirmedByTranscript])
	}
}

// TestExtractTranscriptTextQueueOperationRequiresEnqueueOperation proves the
// other half directly: a dequeue/remove/popAll entry (which can carry the
// SAME text a moment later, once withdrawn rather than accepted) must never
// be read as confirmation.
func TestExtractTranscriptTextQueueOperationRequiresEnqueueOperation(t *testing.T) {
	for _, op := range []string{"dequeue", "remove", "popAll", ""} {
		line := fmt.Sprintf(`{"type":"queue-operation","operation":%q,"content":"already withdrawn"}`, op)
		if _, _, ok := extractTranscriptText([]byte(line)); ok {
			t.Errorf("operation=%q: extractTranscriptText treated it as a candidate turn", op)
		}
	}
}

func TestExtractTranscriptTextQueueOperationPrefersTopLevelContent(t *testing.T) {
	line := `{"type":"queue-operation","operation":"enqueue","content":"the real queued text"}`
	kind, text, ok := extractTranscriptText([]byte(line))
	if !ok || kind != "queue-operation" || text != "the real queued text" {
		t.Fatalf("got kind=%q text=%q ok=%v", kind, text, ok)
	}
}

// TestSendConfirmsSubmitViaScreenAfterASilentResolvedTranscript reproduces
// the review's fix for a resolved-but-silent transcript: the exact
// queue-operation shape is not independently verified against every real
// transcript, so a resolved transcript that never matches must not be
// reported as "unknown" outright — the screen-based signal (composer
// emptying) gets a look first, exactly as it would have before D6 existed.
func TestSendConfirmsSubmitViaScreenAfterASilentResolvedTranscript(t *testing.T) {
	recordRoot := t.TempDir()
	cwd := "/work/alpha"
	sessionName := "alpha💬"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, "conv-1.jsonl")
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": sessionName, "sessionId": "conv-1",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately never append a matching turn: the transcript resolves but
	// stays silent for the whole confirmation window.

	f := twoSessions()
	d := New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Now() }),
		WithRecordRoot(recordRoot),
	)
	const text = "an ordinary message the transcript parser happens to miss"
	ref := fleet.SessionRef{Machine: "testbox", ID: sessionName}

	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued — the screen must still confirm a submit the "+
			"transcript's own unverified shape missed", got.Outcome, got.Reason)
	}
	snap := d.counters.Snapshot()
	if snap[counterSubmitConfirmedByScreenAfterSilentTranscript] != 1 {
		t.Errorf("counterSubmitConfirmedByScreenAfterSilentTranscript = %d, want 1",
			snap[counterSubmitConfirmedByScreenAfterSilentTranscript])
	}
}

// --- extractTranscriptText's review filters --------------------------------

func TestExtractTranscriptTextExcludesMetaEntries(t *testing.T) {
	line := `{"type":"user","isMeta":true,"message":{"role":"user","content":` +
		`"Here is how the sorting algorithm should go: first partition, then recurse."}}`
	if _, _, ok := extractTranscriptText([]byte(line)); ok {
		t.Fatal("a meta entry (e.g. a skill body) must not be a candidate turn")
	}
}

func TestExtractTranscriptTextExcludesCompactSummaryEntries(t *testing.T) {
	line := `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"deploy the staging branch"}}`
	if _, _, ok := extractTranscriptText([]byte(line)); ok {
		t.Fatal("a compaction summary must not be a candidate turn, even if it quotes the sent text verbatim")
	}
}

func TestExtractTranscriptTextExcludesSidechainEntries(t *testing.T) {
	line := `{"type":"user","isSidechain":true,"message":{"role":"user","content":"deploy the staging branch"}}`
	if _, _, ok := extractTranscriptText([]byte(line)); ok {
		t.Fatal("a sidechain entry must not be a candidate turn")
	}
}

func TestExtractTranscriptTextExcludesNonHumanOrigin(t *testing.T) {
	line := `{"type":"user","origin":{"kind":"peer"},"message":{"role":"user","content":"ok"}}`
	if _, _, ok := extractTranscriptText([]byte(line)); ok {
		t.Fatal("a non-human origin (e.g. a cross-session peer message) must not be a candidate turn")
	}
}

func TestExtractTranscriptTextExcludesUnrecognisedPromptSource(t *testing.T) {
	line := `{"type":"user","promptSource":"replayed","message":{"role":"user","content":"deploy"}}`
	if _, _, ok := extractTranscriptText([]byte(line)); ok {
		t.Fatal("an unrecognised promptSource must not be a candidate turn")
	}
}

func TestExtractTranscriptTextAcceptsOrdinaryHumanTypedEntry(t *testing.T) {
	line := `{"type":"user","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"deploy"}}`
	_, text, ok := extractTranscriptText([]byte(line))
	if !ok || text != "deploy" {
		t.Fatalf("got text=%q ok=%v, want %q/true", text, ok, "deploy")
	}
}

// --- #22 (review-regression): the record-root cache must not survive a
// runtime-side session-id change under an unchanged pane -------------------

// TestResolveTranscriptSourceDetectsCacheDisagreementWithLiveIdentity
// reproduces the review's "stale transcript after /clear" finding:
// conversationStore's own resolved cache is keyed on (pane, created) for the
// life of the daemon and has no way to notice Claude Code regenerating its
// own session id under an unchanged pane. Cross-checking against the
// runtime's own live per-process identity (resolveLiveProcessSessionID) on
// every call, rather than trusting a cache hit unconditionally, closes it.
func TestResolveTranscriptSourceDetectsCacheDisagreementWithLiveIdentity(t *testing.T) {
	recordRoot := t.TempDir()
	sessionsRoot := t.TempDir()
	cwd := "/work/alpha"

	// The record-root lookup resolves to conv-1 (titled with the session's
	// name) — what a cache, once populated, would keep returning forever.
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldConvPath := filepath.Join(convDir, "conv-1.jsonl")
	if err := os.WriteFile(oldConvPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": "alpha💬", "sessionId": "conv-1",
		"timestamp": time.Now().Add(-time.Hour).Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The LIVE conversation, per the runtime's own per-process identity file
	// — what /clear regenerated, under the SAME pane and the SAME pid.
	newConvPath := filepath.Join(convDir, "22222222-2222-4222-8222-222222222222.jsonl")
	if err := os.WriteFile(newConvPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": "conv-2-has-no-title-match", "sessionId": "22222222-2222-4222-8222-222222222222",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &fakeMux{
		sessions: []fakeSession{
			{name: "alpha💬", paneID: "%1", cwd: cwd, pid: 707070, created: 1785600000, title: "2_1_220"},
		},
		captures: map[string]string{"%1": idleFixtureFor("alpha")},
	}
	fps := &fakePS{}
	instant := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)
	fps.set(707070, instant)
	if err := os.WriteFile(filepath.Join(sessionsRoot, "707070.json"), []byte(mustJSONLine(t, map[string]any{
		"pid": 707070, "sessionId": "22222222-2222-4222-8222-222222222222", "cwd": cwd, "procStart": instant.UTC().Format(psStartTimeLayout),
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

	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	target := &paneRow{session: "alpha💬", paneID: "%1", cwd: cwd, pid: 707070, created: time.Unix(1785600000, 0)}

	src, ok := d.resolveTranscriptSource(context.Background(), ref, target)
	if !ok {
		t.Fatal("resolveTranscriptSource did not resolve at all")
	}
	if src.path != newConvPath {
		t.Errorf("resolved path = %q, want the LIVE conversation %q, not the stale cached one %q",
			src.path, newConvPath, oldConvPath)
	}
	snap := d.counters.Snapshot()
	if snap[counterTranscriptCacheStaleAfterClear] != 1 {
		t.Errorf("counterTranscriptCacheStaleAfterClear = %d, want 1", snap[counterTranscriptCacheStaleAfterClear])
	}
}

// --- #10/#18/#23: the transcript offset must be taken BEFORE Enter --------

// TestSendCatchesATurnWrittenSynchronouslyWithTheSubmitKeystroke reproduces
// the review's TestReviewTurnWrittenBeforeOffsetIsNotMissed /
// TestReviewOffsetCapturedAfterSubmitMissesAPromptWrite: a runtime that
// writes its transcript entry essentially AT THE SAME MOMENT as the submit
// keystroke (rather than the measured 0.1-0.6s later) must still be caught.
// The wrapped exec below appends the matching turn SYNCHRONOUSLY, inside the
// very same call that delivers "send-keys ... C-m" — before Send's own
// resolveTranscriptSource call would learn about it, IF that call happened
// AFTER the keystroke (the old, buggy order). Resolving it BEFORE the
// keystroke (this review's fix) fixes the offset first, so the
// synchronously-appended line lands strictly AFTER it and is found.
func TestSendCatchesATurnWrittenSynchronouslyWithTheSubmitKeystroke(t *testing.T) {
	recordRoot := t.TempDir()
	cwd := "/work/alpha"
	sessionName := "alpha💬"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, "conv-1.jsonl")
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": sessionName, "sessionId": "conv-1",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const text = "a message the runtime records the instant it is submitted"

	f := twoSessions()
	realExec := f.exec
	wrapped := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		isSubmit := false
		if len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args {
				if a == "C-m" || a == "Enter" {
					isSubmit = true
				}
			}
		}
		out, err := realExec(ctx, name, args...)
		if isSubmit {
			// Append SYNCHRONOUSLY, inside this same call — no goroutine, no
			// sleep. If the offset were captured AFTER this call returns
			// (the old order), it would already include this very line.
			appendLine(t, convPath, mustJSONLine(t, map[string]any{
				"type": "user", "sessionId": "conv-1",
				"message": map[string]any{"role": "user", "content": text},
			}))
		}
		return out, err
	}

	d := New("testbox",
		withExec(wrapped),
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
		t.Fatalf("outcome = %s (%s), want queued — a turn written synchronously with the submit "+
			"keystroke must still be found by an offset taken BEFORE that keystroke", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "transcript") {
		t.Errorf("reason = %q, want it to name the transcript as the confirming evidence", got.Reason)
	}
	// The DISCRIMINATING assertion: this must be confirmed BY THE TRANSCRIPT
	// ITSELF, not by the screen-based fallback rescuing a transcript that
	// looked silent. Taking the offset AFTER the keystroke (the defect this
	// test reproduces) makes the synchronously-written line invisible to
	// transcriptTailMatches (it already precedes the offset), so the
	// transcript's own window closes with nothing — and only the SEPARATE
	// screen-fallback fix (confirmSubmittedFromSource) would then rescue the
	// outcome several seconds later. Requiring the TRANSCRIPT counter here,
	// and requiring the screen-fallback counter to stay at zero, is what
	// makes this test fail specifically for the offset-ordering defect
	// rather than being silently rescued by an unrelated fix.
	snap := d.counters.Snapshot()
	if snap[counterSubmitConfirmedByTranscript] != 1 {
		t.Errorf("counterSubmitConfirmedByTranscript = %d, want 1 — confirmed by the transcript "+
			"itself, not by the screen-fallback rescuing an offset taken too late",
			snap[counterSubmitConfirmedByTranscript])
	}
	if snap[counterSubmitConfirmedByScreenAfterSilentTranscript] != 0 {
		t.Errorf("counterSubmitConfirmedByScreenAfterSilentTranscript = %d, want 0 — the transcript "+
			"should never have gone silent in the first place",
			snap[counterSubmitConfirmedByScreenAfterSilentTranscript])
	}
}

// --- scanner errors are "cannot tell", never silently "no match" ----------

// TestConfirmSubmittedFromSourceCountsScannerErrorsAndStillFallsBackToScreen
// reproduces the review's finding that a bufio.Scanner failure (one
// transcript line over recordLineLimit) was silently discarded and
// indistinguishable from an ordinary non-match. It is now counted
// separately, and — thanks to the resolved-but-silent-transcript screen
// fallback (the same fix P2/#7 needed) — a submit that actually DID land on
// screen is still confirmed rather than reported unknown just because this
// one transcript line could not be read.
func TestConfirmSubmittedFromSourceCountsScannerErrorsAndStillFallsBackToScreen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// One line far larger than recordLineLimit, sitting AT the offset this
	// call will scan from — bufio.Scanner's own "token too long".
	huge := strings.Repeat("x", recordLineLimit+1024)
	appendLine(t, path, huge)

	f := twoSessions()
	// The composer already reads empty (idleFixtureFor's own shape) — the
	// screen-based fallback confirmSubmitted can succeed on its very first
	// look, once the transcript's own window gives up.
	d := New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Now() }),
	)
	target := &paneRow{session: "alpha💬", paneID: "%1", cwd: "/work/alpha", created: time.Unix(1785600000, 0)}
	src := transcriptSource{path: path, offset: 0, evidence: "test fixture"}

	confirmed, evidence, _ := d.confirmSubmittedFromSource(context.Background(), target, "anything at all",
		pasteKey{}, 0, src, true)
	if !confirmed {
		t.Fatalf("confirmSubmittedFromSource did not fall back to the screen after an unreadable "+
			"transcript line: %s", evidence)
	}
	snap := d.counters.Snapshot()
	if snap[counterTranscriptScannerUnreadable] == 0 {
		t.Error("counterTranscriptScannerUnreadable = 0, want at least 1 — a scanner failure must be " +
			"counted, not silently folded into an ordinary non-match")
	}
	if snap[counterSubmitConfirmedByScreenAfterSilentTranscript] == 0 {
		t.Error("counterSubmitConfirmedByScreenAfterSilentTranscript = 0, want at least 1")
	}
}

// --- bare collapsed-paste marker attribution (P1, direct confirmLandedV2 case)

// TestConfirmLandedV2AttributesABareCollapsedPasteMarker is the direct,
// dedicated regression test for P1 (single-line pastes over ~800 bytes never
// confirming landed): a composer showing ONLY "[Pasted text #N]" — no line
// count at all, the real shape for a collapsed SINGLE-LINE paste — must
// still be attributed to this delivery via the marker-gained path, even
// though composerRegionMatch itself cannot match a marker against the full
// pasted text.
func TestConfirmLandedV2AttributesABareCollapsedPasteMarker(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	text := strings.Repeat("a", 900) // single line, no newlines, over the collapse threshold
	f.setCapture("%1", composerHolding("[Pasted text #1]"))

	key, atCount, landed := d.confirmLandedV2(context.Background(), "%1", text, map[pasteKey]int{}, false, false)
	if !landed {
		t.Fatal("confirmLandedV2 did not attribute a bare (no line count) collapsed-paste marker")
	}
	if key.index != 1 || atCount != 1 {
		t.Errorf("key = %+v atCount = %d, want index=1 atCount=1", key, atCount)
	}
	snap := d.counters.Snapshot()
	if snap[counterLandConfirmByMarker] != 1 {
		t.Errorf("counterLandConfirmByMarker = %d, want 1", snap[counterLandConfirmByMarker])
	}
}
