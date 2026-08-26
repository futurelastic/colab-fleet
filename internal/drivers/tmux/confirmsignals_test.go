package tmux

import (
	"context"
	"testing"
	"time"
)

// colab-fleet #104: confirmSubmitted's own doc comment names two independent
// confirming signals. #104 suspects one of them could be dead code nobody
// removed, and asks for a counter per signal instead of a live capture nobody
// can take (driving the multiplexer directly from a session is refused by
// design). This file is the oracle for that instrumentation: each signal
// fires its OWN counter and not the other's, a timeout fires neither, and the
// latency bucketing is correct at its boundaries.

// TestConfirmLatencyBucket is deliberately clock-free: confirmLatencyBucket
// is a pure function, and every boundary named in counters.go is a case here
// rather than something inferred from a fake clock elsewhere in the suite.
func TestConfirmLatencyBucket(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    string
	}{
		{0, counterSubmitConfirmLatencyUnder250ms},
		{100 * time.Millisecond, counterSubmitConfirmLatencyUnder250ms},
		{249 * time.Millisecond, counterSubmitConfirmLatencyUnder250ms},
		{250 * time.Millisecond, counterSubmitConfirmLatencyUnder500ms},
		{499 * time.Millisecond, counterSubmitConfirmLatencyUnder500ms},
		{500 * time.Millisecond, counterSubmitConfirmLatencyUnder1s},
		{999 * time.Millisecond, counterSubmitConfirmLatencyUnder1s},
		{time.Second, counterSubmitConfirmLatencyUnder2s},
		{1999 * time.Millisecond, counterSubmitConfirmLatencyUnder2s},
		{2 * time.Second, counterSubmitConfirmLatencyUnder4s},
		{3999 * time.Millisecond, counterSubmitConfirmLatencyUnder4s},
		{4 * time.Second, counterSubmitConfirmLatencyUnder4s},
		{10 * time.Second, counterSubmitConfirmLatencyUnder4s},
	}
	for _, tc := range cases {
		if got := confirmLatencyBucket(tc.elapsed); got != tc.want {
			t.Errorf("confirmLatencyBucket(%s) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}

// TestConfirmSubmittedCountsComposerEmptySignal covers the literal,
// atCount==0 case: the composer starts holding unsent text and then empties.
// Only the composer-empty counter should move.
func TestConfirmSubmittedCountsComposerEmptySignal(t *testing.T) {
	const before = "transcript\n" + rule + "\n❯ hello\n" + rule
	const after = "transcript\n" + rule + "\n❯ \n" + rule

	f := twoSessions()
	f.setCapture("%1", before)
	d := New("testbox", withExec(captureCounter(f, 2, func() {
		f.setCapture("%1", after)
	})), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))

	if !d.confirmSubmitted(context.Background(), "%1", pasteKey{}, 0) {
		t.Fatal("confirmSubmitted did not detect the composer emptying")
	}

	got := d.counters.Snapshot()
	if got[counterSubmitConfirmedByComposerEmpty] != 1 {
		t.Errorf("counterSubmitConfirmedByComposerEmpty = %d, want 1", got[counterSubmitConfirmedByComposerEmpty])
	}
	if got[counterSubmitConfirmedByMarkerCleared] != 0 {
		t.Errorf("counterSubmitConfirmedByMarkerCleared = %d, want 0 — this call never had a marker to clear", got[counterSubmitConfirmedByMarkerCleared])
	}
	if got[counterSubmitConfirmTimeout] != 0 {
		t.Errorf("counterSubmitConfirmTimeout = %d, want 0 — this call confirmed, it did not time out", got[counterSubmitConfirmTimeout])
	}
	// The test clock is frozen, so elapsed is always 0 — the under-250ms
	// bucket is the only one a confirmed call in this suite can ever land in.
	if got[counterSubmitConfirmLatencyUnder250ms] != 1 {
		t.Errorf("counterSubmitConfirmLatencyUnder250ms = %d, want 1", got[counterSubmitConfirmLatencyUnder250ms])
	}
}

// TestConfirmSubmittedCountsMarkerClearedSignal is the same fixture
// TestConfirmSubmittedDetectsOurBlockLeavingDespiteResidue uses (#37's
// residue case, where the composer never reads empty), asserting the counter
// side of it: this is the branch #104 suspects could be dead, so proving it
// increments its OWN counter — not the composer-empty one — is the whole
// point of this test.
func TestConfirmSubmittedCountsMarkerClearedSignal(t *testing.T) {
	const bothBlocks = "transcript\n" + rule +
		"\n❯ [Pasted text #10 +12 lines][Pasted text #11 +30 lines]\n" + rule
	const residueOnly = "transcript\n" + rule + "\n❯ [Pasted text #10 +12 lines]\n" + rule

	f := twoSessions()
	f.setCapture("%1", bothBlocks)

	ours := pasteKey{index: 11, lines: 30}
	d := New("testbox", withExec(captureCounter(f, 2, func() {
		f.setCapture("%1", residueOnly) // our block (#11) submitted and cleared; #10 remains
	})), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !d.confirmSubmitted(ctx, "%1", ours, 1) {
		t.Fatal("confirmSubmitted did not detect the marker clearing despite residue")
	}

	got := d.counters.Snapshot()
	if got[counterSubmitConfirmedByMarkerCleared] != 1 {
		t.Errorf("counterSubmitConfirmedByMarkerCleared = %d, want 1 — residue keeps the composer "+
			"non-empty forever in this fixture, so only the marker signal can have decided this",
			got[counterSubmitConfirmedByMarkerCleared])
	}
	if got[counterSubmitConfirmedByComposerEmpty] != 0 {
		t.Errorf("counterSubmitConfirmedByComposerEmpty = %d, want 0 — the composer is never empty in this fixture",
			got[counterSubmitConfirmedByComposerEmpty])
	}
}

// TestConfirmSubmittedCountsTimeout covers the case neither signal ever
// fires: the composer is never empty and the attributed marker never clears.
// The frozen test clock means confirmSubmitted's own deadline check never
// trips on its own — same as the existing residue test above — so this
// relies on the same ctx-cancellation exit the retry loop already has,
// bounded well under submitConfirmWindow so the suite fails fast rather than
// waiting out the real 4s budget.
func TestConfirmSubmittedCountsTimeout(t *testing.T) {
	// Our own marker (#11) is present at count 1 and NEVER changes — unlike
	// the residue test above, nothing ever clears it, so
	// paintedMarkers()[ours] stays == atCount forever ("< atCount" never
	// goes true) and the composer never reads empty either. Both signals are
	// unreachable here by construction, not by omission.
	const stuck = "transcript\n" + rule +
		"\n❯ [Pasted text #10 +12 lines][Pasted text #11 +30 lines]\n" + rule

	f := twoSessions()
	f.setCapture("%1", stuck)
	d := newTestDriver(f)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if d.confirmSubmitted(ctx, "%1", pasteKey{index: 11, lines: 30}, 1) {
		t.Fatal("confirmSubmitted reported confirmed on a fixture where neither signal can fire")
	}

	got := d.counters.Snapshot()
	if got[counterSubmitConfirmTimeout] != 1 {
		t.Errorf("counterSubmitConfirmTimeout = %d, want 1", got[counterSubmitConfirmTimeout])
	}
	if got[counterSubmitConfirmedByComposerEmpty] != 0 || got[counterSubmitConfirmedByMarkerCleared] != 0 {
		t.Errorf("a timed-out call must not also claim a confirming signal fired: got composer_empty=%d marker_cleared=%d",
			got[counterSubmitConfirmedByComposerEmpty], got[counterSubmitConfirmedByMarkerCleared])
	}
	// No latency bucket should move for a call that never confirmed — a
	// "latency" is only meaningful for a call that actually decided.
	for _, bucket := range []string{
		counterSubmitConfirmLatencyUnder250ms, counterSubmitConfirmLatencyUnder500ms,
		counterSubmitConfirmLatencyUnder1s, counterSubmitConfirmLatencyUnder2s,
		counterSubmitConfirmLatencyUnder4s,
	} {
		if got[bucket] != 0 {
			t.Errorf("latency bucket %q = %d on a timed-out call, want 0", bucket, got[bucket])
		}
	}
}
