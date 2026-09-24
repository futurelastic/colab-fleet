package tmux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

func briefText(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("brief line %d: do the thing for issue 42", i)
	}
	return strings.Join(lines, "\n")
}

// #180 M6: the last look before Enter must POSITIVELY show a composer holding
// this delivery. A permission dialog painted only down to its command block —
// no options yet, so no menu is recognised — is not a composer, and Enter
// into it could approve whatever it becomes.
func TestPreSubmitRefusesAComposerlessHalfPaintedDialog(t *testing.T) {
	f := twoSessions()
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	halfPainted := "  transcript\n" + rule + "\n Bash command\n\n   rm -rf build\n"
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
				f.captures["%1"] = halfPainted
				delete(f.pasted, "%1")
				f.mu.Unlock()
			}
		}
		return f.exec(ctx, name, args...)
	}
	d := New("testbox", withExec(wrapped), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	got, err := d.Send(context.Background(), testCaller, ref, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := submitsIn(f.callsSnapshot()); n != 0 {
		t.Fatalf("Enter was pressed into a composer-less screen (%s: %s)", got.Outcome, got.Reason)
	}
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "resumeIfStranded") {
		t.Fatalf("outcome = %s (%s), want unknown pointing at resumeIfStranded", got.Outcome, got.Reason)
	}
	if _, kept := d.strandedRecordFor(ref.ID, "/work/alpha"); !kept {
		t.Fatal("the stranded record was not kept")
	}
	if n := d.counters.Snapshot()[counterSendRefusedNotHeldPreSubmit]; n != 1 {
		t.Fatalf("refused_not_held_presubmit = %d, want 1", n)
	}
}

// M6 must not cost the collapsed paste its submit: a fresh long paste lands
// as a marker, and the last look finds that same marker.
func TestFreshCollapsedPasteStillSubmits(t *testing.T) {
	f := twoSessions()
	f.collapsePastes = true
	d := newTestDriver(f)
	got, err := d.Send(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"},
		briefText(20), driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued", got.Outcome, got.Reason)
	}
}

// #180 H1, the real flow: a long paste lands collapsed, its submit is
// swallowed, and the caller does what the receipt says — retries with
// resumeIfStranded. The record kept the marker the paste landed as, and the
// resume finishes it.
func TestResumeFinishesOwnCollapsedPasteStrand(t *testing.T) {
	f := twoSessions()
	f.collapsePastes = true
	f.swallowSubmit = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	text := briefText(20)

	first, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s (%s), want unknown", first.Outcome, first.Reason)
	}
	rec, ok := d.strandedRecordFor(ref.ID, "/work/alpha")
	if !ok || !rec.PasteLanded || rec.pasteKey() != (pasteKey{index: 1, lines: 19}) {
		t.Fatalf("record = %+v, want the marker the paste landed as", rec)
	}

	f.mu.Lock()
	f.swallowSubmit = false
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume outcome = %s (%s), want queued", got.Outcome, got.Reason)
	}
	if n := d.counters.Snapshot()[counterLandConfirmByOwnMarker]; n == 0 {
		t.Fatal("the resume did not recognise its own marker")
	}
}

// H1 with a record that predates the stored marker (or never had one): the
// composer's digest matching the record proves the marker there is the one
// the driver left — the reviewer's own reproduction.
func TestResumeFinishesADigestVerifiedCollapsedStrand(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	text := briefText(20)
	f.setCapture("%1", composerHolding("[Pasted text #1 +19 lines]"))
	d.noteStranded(ref.ID, "/work/alpha", text, d.currentComposerDigest(context.Background(), "%1"))

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

// A marker whose line count does not fit the text is not this text's, even
// with a matching digest.
func TestResumeRefusesAMarkerWhoseLineCountDoesNotFit(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	text := briefText(20)
	f.setCapture("%1", composerHolding("[Pasted text #1 +7 lines]"))
	d.noteStranded(ref.ID, "/work/alpha", text, d.currentComposerDigest(context.Background(), "%1"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued || submitsIn(f.callsSnapshot()) != 0 {
		t.Fatalf("a marker for a 7-line paste was submitted as a 20-line text (%s)", got.Reason)
	}
	t.Logf("outcome=%s reason=%s", got.Outcome, got.Reason)
}
