package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// submitsIn counts submit keystrokes this driver issued.
func submitsIn(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if c[0] != "send-keys" {
			continue
		}
		for _, a := range c {
			if a == "C-m" || a == "Enter" {
				n++
			}
		}
	}
	return n
}

// #180 L4: a whitespace edit that changes meaning changes the digest.
func TestComposerDigestKeepsInteriorWhitespace(t *testing.T) {
	if composerTextDigest("rm -rf /tmp/build") == composerTextDigest("rm -rf / tmp/build") {
		t.Fatal("two different commands digest equal")
	}
	// What a resize can do to composerText's output — a different row break
	// at a space, a continuation indent — does not.
	if composerTextDigest("deploy  the\tstaging branch") != composerTextDigest("deploy the staging branch") {
		t.Fatal("whitespace runs must collapse, or a wrap at a space would change the digest")
	}
}

func TestSegmentsMatchText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		segs   []string
		sent   string
		suffix bool
		want   bool
	}{
		{"one row", []string{"hello world"}, "hello world", false, true},
		{"wrapped at a space", []string{"hello", "world"}, "hello world", false, true},
		{"real newline", []string{"first line", "second line"}, "first line\nsecond line", false, true},
		{"token broken inside itself", []string{"abcdefgh", "ijkl"}, "abcdefghijkl", false, true},
		{"interior whitespace differs", []string{"rm -rf / tmp/build"}, "rm -rf /tmp/build", false, false},
		{"extra text", []string{"hello world again"}, "hello world", false, false},
		{"missing text", []string{"hello"}, "hello world", false, false},
		{"tail, suffix off", []string{"the last words visible after scrolling"}, "word word the last words visible after scrolling", false, false},
		{"tail, suffix on", []string{"the last words visible after scrolling"}, "word word the last words visible after scrolling", true, true},
		{"short tail, suffix on", []string{"please."}, "deploy it now please.", true, false},
		{"NFD against NFC", []string{"Việt"}, "Việt", false, true},
	} {
		if got := segmentsMatchText(tc.segs, tc.sent, tc.suffix); got != tc.want {
			t.Errorf("%s: segmentsMatchText(%q, %q, %v) = %v, want %v", tc.name, tc.segs, tc.sent, tc.suffix, got, tc.want)
		}
	}
}

// #180 L1: a stranded record written by the build before #180 carries the
// composer digest in the old, raw form. After an upgrade and restart it must
// still resume — stranded.json survives restarts by design (#11).
func TestStrandedRecordPersistedBeforeUpgradeStillResumes(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the old build stranded this message,  with a double space"
	f.setCapture("%1", composerHolding(text))
	d.noteStranded(ref.ID, "/work/alpha", text, legacyComposerDigest(text))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("a pre-upgrade stranded record no longer resumes: %s — %s", got.Outcome, got.Reason)
	}
}

// #180 L4 end to end: a person edited only whitespace in text this driver
// left in the composer. That is the person's text now; a resume refuses and
// the draft stays.
func TestWhitespaceEditedStrandIsNotResumed(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "rm -rf /tmp/build"
	d.noteStranded(ref.ID, "/work/alpha", text, composerTextDigest(text))
	f.setCapture("%1", composerHolding("rm -rf / tmp/build"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s (%s), want refused", got.Outcome, got.Reason)
	}
	if n := submitsIn(f.callsSnapshot()); n != 0 {
		t.Fatalf("%d submit keystroke(s) pressed on a person's edited draft", n)
	}
	if !strings.Contains(f.captures["%1"], "rm -rf / tmp/build") {
		t.Fatal("the person's draft was touched")
	}
}

// A resize between strand and resume can move a row break inside an
// over-long token, which changes the digest. The driver's own record still
// proves the composer holds exactly its text, so the resume goes ahead.
func TestResumeSurvivesAResizeBetweenStrandAndResume(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	token := strings.Repeat("0123456789", 10)
	text := "fetch " + token
	// At strand time the token was broken across two rows.
	d.noteStranded(ref.ID, "/work/alpha", text, composerTextDigest("fetch "+token[:70]+" "+token[70:]))
	// Wider now: one row.
	f.setCapture("%1", composerHolding(text))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued", got.Outcome, got.Reason)
	}
}

// #180 L2: a collapsed-paste marker in the history margin above the visible
// pane, or in transcript above the composer, is never counted — only the
// composer's own rows are.
func TestComposerMarkersIgnoreTheHistoryMarginAndTranscript(t *testing.T) {
	raw := "❯ [Pasted text #10 +12 lines]\n" + // history margin
		"filler 1\nfiller 2\n" +
		"❯ [Pasted text #11 +3 lines]\n" + // transcript echo of an earlier paste
		rule + "\n❯ \n" + rule + "\n  status\n"
	sc := newScreenVisible(raw, 6)
	if sc.visibleTop == 0 {
		t.Fatal("setup: expected a history margin")
	}
	if got := composerMarkers(sc); len(got) != 0 {
		t.Fatalf("composerMarkers = %v, want none: the composer is empty", got)
	}
	// Nothing was pasted; a landed check with the before-snapshot taken the
	// same way must not attribute either marker to this delivery.
	sc2 := newScreenVisible(strings.Replace(raw, "❯ \n", "❯ [Pasted text #12 +40 lines]\n", 1), 6)
	if got := composerMarkers(sc2); len(got) != 1 || got[pasteKey{index: 12, lines: 40}] != 1 {
		t.Fatalf("composerMarkers = %v, want exactly the composer's own marker", got)
	}
}

// #149, decided by #180: a FRESH send whose composer grew past the visible top
// confirms when the visible rows render a substantial tail of the text; a
// RESUME never accepts a clipped composer — it cannot see the whole of what
// it would submit.
func TestClippedComposerConfirmsAFreshSendButNeverAResume(t *testing.T) {
	full := sequentialFiller(80 * 12)
	var rows []string
	for i := 0; i < len(full); i += 80 {
		rows = append(rows, full[i:i+80])
	}
	painted := strings.Join(rows[len(rows)-7:], "\n") + "\n" + rule + "\n  status"
	f := twoSessions()
	f.setCapture("%2", painted)
	d := newTestDriver(f)
	if _, _, ok := d.confirmLandedV2(context.Background(), "%2", full, map[pasteKey]int{}, false, false); !ok {
		t.Fatal("a fresh send into a top-clipped composer did not confirm")
	}
	if n := d.counters.Snapshot()[counterLandConfirmByClippedTail]; n != 1 {
		t.Fatalf("by_clipped_tail = %d, want 1", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, ok := d.confirmLandedV2(ctx, "%2", full, map[pasteKey]int{}, true, true); ok {
		t.Fatal("a resume accepted a clipped composer")
	}
}
