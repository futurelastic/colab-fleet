package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// strandNeverRendered makes a first send whose paste never renders (a lost
// paste or a startup race), then lets the pane recover.
func strandNeverRendered(t *testing.T, text string) (*fakeMux, *Driver, fleet.SessionRef) {
	t.Helper()
	f := twoSessions()
	f.noEcho = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	first, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s (%s), want unknown", first.Outcome, first.Reason)
	}
	if rec, ok := d.strandedRecordFor(ref.ID, "/work/alpha"); !ok || !rec.NeverRendered {
		t.Fatalf("setup: record = %+v (found=%v), want a never-rendered strand", rec, ok)
	}
	if n := submitsIn(f.callsSnapshot()); n != 0 {
		t.Fatalf("setup: %d Enter(s) pressed for text that never rendered", n)
	}
	f.mu.Lock()
	f.noEcho = false
	f.mu.Unlock()
	return f, d, ref
}

// #180 M1: the receipt said "retry with resumeIfStranded"; for a paste that
// never rendered and never had Enter pressed, the retry pastes it again — the
// runtime cannot have taken it as a turn — and delivers it.
func TestResumeAfterAPasteThatNeverRenderedRepastes(t *testing.T) {
	const text = "the startup message that was lost"
	f, d, ref := strandNeverRendered(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued", got.Outcome, got.Reason)
	}
	if n := pastesIn(f.callsSnapshot()); n != 2 {
		t.Fatalf("pastes = %d, want 2 (the lost one and the re-paste)", n)
	}
	if n := d.counters.Snapshot()[counterResumeRepastedNeverRendered]; n != 1 {
		t.Fatalf("resume.repasted_never_rendered = %d, want 1", n)
	}
	if n := d.Counters()["delivery.tmux.resumed"]; n != 1 {
		t.Fatalf("delivery.tmux.resumed = %d, want 1", n)
	}
}

// M1 must not reopen H2: the re-paste goes through every check a fresh
// delivery makes, including who holds the foreground.
func TestResumeOfANeverRenderedStrandOnAShellPaneRefuses(t *testing.T) {
	const text = "touch a-file"
	f, d, ref := strandNeverRendered(t, text)
	f.mu.Lock()
	f.currentCommand = map[string]string{"%1": "zsh"}
	f.mu.Unlock()
	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %s (%s), want refused", got.Outcome, got.Reason)
	}
	if n := pastesIn(f.callsSnapshot()); n != 1 {
		t.Fatalf("pastes = %d, want only the original lost one", n)
	}
}

// Only the never-rendered class re-pastes: a strand whose Enter WAS pressed
// may already be a turn, and an empty composer is exactly what that looks
// like. Without transcript evidence, the resume refuses to paste again.
func TestResumeAfterASubmittedStrandDoesNotRepaste(t *testing.T) {
	f := twoSessions()
	f.swallowSubmit = true
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "an instruction whose Enter was pressed"
	if got, _ := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true}); got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: %s (%s)", got.Outcome, got.Reason)
	}
	f.setCapture("%1", idleFixtureFor("alpha")) // the composer emptied since
	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeQueued || pastesIn(f.callsSnapshot()) != 1 {
		t.Fatalf("outcome = %s (%s); a possibly-accepted delivery was pasted again", got.Outcome, got.Reason)
	}
}
