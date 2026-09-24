package tmux

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/inboxclient"
)

// #192. The submit keystroke is the one step of the terminal path that can fail
// OUTRIGHT — tmux itself refuses the call — after the paste has been confirmed
// in the composer. Every other way the submit goes wrong (swallowed, preempted,
// a dialog appearing) already leaves a stranded record behind. This one used to
// return the error and leave nothing, so the composer held text no record knew
// about: a follow-up auto send saw no unconfirmed terminal delivery and took the
// inbox, and a resumeIfStranded retry was refused for want of a record.

// failingSubmit wraps a fake tmux so that, while fail is set, the submit
// keystroke (the send-keys call carrying C-m) fails outright. The paste, the
// captures and every other keystroke behave exactly as the fake models them.
func failingSubmit(f *fakeMux, fail *atomic.Bool) execFunc {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if fail.Load() && len(args) > 0 && args[0] == "send-keys" {
			for _, a := range args {
				if a == "C-m" {
					return nil, errors.New("tmux: send-keys: server exited unexpectedly")
				}
			}
		}
		return f.exec(ctx, name, args...)
	}
}

// The property itself: the paste was confirmed, the submit keystroke errored,
// and the driver now holds a stranded record for exactly that text — the record
// every later guard reads.
func TestFailedSubmitKeystrokeAfterConfirmedPasteLeavesAStrandedRecord(t *testing.T) {
	f := twoSessions()
	var fail atomic.Bool
	fail.Store(true)
	d := New("testbox", withExec(failingSubmit(f, &fail)), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the instruction"

	_, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err == nil {
		t.Fatal("setup: the submit keystroke failed but Send returned no error")
	}
	if n := submitsIn(f.callsSnapshot()); n != 0 {
		t.Fatalf("setup: %d submit keystrokes reached the fake, want 0 — the failure was not injected", n)
	}
	if pastesIn(f.callsSnapshot()) != 1 {
		t.Fatal("setup: the text was not pasted, so this is not the paste-confirmed case")
	}

	rec, ok := d.strandedRecordFor(ref.ID, "/work/alpha")
	if !ok {
		t.Fatal("a paste was confirmed in the composer and the submit keystroke failed, but no stranded record was left")
	}
	if rec.Text != text {
		t.Errorf("record text = %q, want %q", rec.Text, text)
	}
	if rec.NeverRendered {
		t.Error("record is marked NeverRendered, but the text DID render — a resume would paste it a second time")
	}
	if rec.ComposerDigest == "" {
		t.Error("record carries no composer digest, although the composer was readable and held the text")
	}
	if !d.terminalUnconfirmed(ref.ID, text) {
		t.Error("terminalUnconfirmed = false for text still sitting in the composer")
	}
}

// The recovery the record exists for: the keystroke works the second time, and
// resumeIfStranded submits the text already in the composer rather than being
// refused for want of a record — and without pasting it a second time.
func TestResumeFinishesAStrandCreatedByAFailedSubmitKeystroke(t *testing.T) {
	f := twoSessions()
	var fail atomic.Bool
	fail.Store(true)
	d := New("testbox", withExec(failingSubmit(f, &fail)), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "the instruction"

	if _, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true}); err == nil {
		t.Fatal("setup: the submit keystroke failed but Send returned no error")
	}
	pastesBefore := pastesIn(f.callsSnapshot())

	fail.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume outcome = %s (%s), want queued", got.Outcome, got.Reason)
	}
	if n := submitsIn(f.callsSnapshot()); n != 1 {
		t.Errorf("submit keystrokes = %d, want exactly 1 (the resume's)", n)
	}
	if n := pastesIn(f.callsSnapshot()); n != pastesBefore {
		t.Errorf("pastes went from %d to %d — the resume pasted the text again", pastesBefore, n)
	}
	if _, kept := d.strandedRecordFor(ref.ID, "/work/alpha"); kept {
		t.Error("the record survived a confirmed resume")
	}
}

// #184's guard, which the record is what feeds: after the failed keystroke the
// same text is not ALSO sent as a peer message. Asserted on the receivers, not
// the receipts — how many times the message reached a session.
func TestFailedSubmitKeystroke_FollowUpNeverTakesInbox(t *testing.T) {
	rcv := newInboxReceiver(t)
	f := twoSessions()
	ps := &fakePS{}
	ps.set(100, time.Now())
	ps.set(200, time.Now())
	var fail atomic.Bool
	fail.Store(true)
	d := newInboxTestDriverWith(f, ps, attestableResolver(inboxclient.ModeBypass),
		rcv.dialer(receiverRecordsEnvelope), append(rcv.options(), withExec(failingSubmit(f, &fail)))...)

	if _, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(fleet.RouteTerminal)); err == nil {
		t.Fatal("setup: the submit keystroke failed but Send returned no error")
	}

	for _, route := range []fleet.Route{fleet.RouteAuto, fleet.RouteInbox} {
		got, err := d.Send(context.Background(), testCaller, alphaRef, "hello", routeOpts(route))
		if err != nil {
			t.Fatalf("route %s: %v", route, err)
		}
		if rcv.dialCount() != 0 || rcv.received() != 0 {
			t.Fatalf("route %s: the inbox was used for text already sitting in the composer (%+v)", route, got)
		}
	}
	if n := d.Counters()[counterRouteGuardTerminalUnconfirm]; n != 2 {
		t.Errorf("route.guard.terminal_unconfirmed = %d, want 2", n)
	}
}
