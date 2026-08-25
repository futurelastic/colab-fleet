package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// colab-fleet #111: the delivery mark itself — the denominator `turns` is
// counted relative to, written once at the moment Send's own paste
// succeeds, independent of whatever record-reading machinery later counts
// against it (covered separately in runtimerecord_test.go and
// runtimerecord_integration_test.go).

// A successful paste writes a mark even when nothing else about the
// delivery is confirmed — Submit:false never reaches confirmLanded at all,
// and the mark must exist anyway: it answers "was anything delivered",
// which does not depend on whether that delivery went on to be submitted.
func TestNoteDeliveryFiresOnAnOrdinaryUnconfirmedSend(t *testing.T) {
	f := twoSessions()
	deliveredAt := sessionStart.Add(1 * time.Hour)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt }))

	got, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "do the thing", driver.SendOptions{Submit: false})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome == fleet.OutcomeRefused {
		t.Fatalf("setup: send was refused: %s", got.Reason)
	}

	mark, ok := d.deliveryMarkFor("alpha💬", "/work/alpha")
	if !ok {
		t.Fatal("no delivery mark after a successful paste")
	}
	if !mark.At.Equal(deliveredAt) {
		t.Errorf("mark.At = %v, want %v", mark.At, deliveredAt)
	}
}

// A SECOND delivery into a composer that has gone quiet since the first
// resets the denominator — `turns` counts "since the most recent delivery",
// not the session's lifetime.
func TestNoteDeliveryResetsOnASecondDelivery(t *testing.T) {
	f := twoSessions()
	now := sessionStart.Add(1 * time.Hour)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return now }))

	if _, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "first", driver.SendOptions{Submit: false}); err != nil {
		t.Fatal(err)
	}
	first, ok := d.deliveryMarkFor("alpha💬", "/work/alpha")
	if !ok {
		t.Fatal("setup: no mark after the first delivery")
	}

	// The composer settles between deliveries — modelled directly on the
	// fake, the same way other tests reset a pane's state between two Send
	// calls in one test.
	f.setCapture("%1", idleFixtureFor("alpha"))
	f.pasted["%1"] = ""
	now = now.Add(30 * time.Minute)

	if _, err := d.Send(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, "second", driver.SendOptions{Submit: false}); err != nil {
		t.Fatal(err)
	}
	second, ok := d.deliveryMarkFor("alpha💬", "/work/alpha")
	if !ok {
		t.Fatal("mark disappeared after the second delivery")
	}
	if second.At.Equal(first.At) {
		t.Errorf("mark.At did not move: still %v after a second, later delivery", second.At)
	}
	if !second.At.Equal(now) {
		t.Errorf("mark.At = %v, want the second delivery's own time %v", second.At, now)
	}
}

// The one case #111's design singles out: resumeIfStranded completes the
// delivery already sitting in the composer — the SAME delivery, not a new
// one — so it must never write a fresh mark.
func TestNoteDeliveryDoesNotMoveWhenAResumeFinishesTheSameDelivery(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // composer never renders what was pasted, so it strands
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "text that stranded"

	deliveredAt := sessionStart.Add(1 * time.Hour)
	now := deliveredAt
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return now }))

	got, err := d.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text strands", got.Outcome)
	}
	mark, ok := d.deliveryMarkFor("alpha💬", "/work/alpha")
	if !ok || !mark.At.Equal(deliveredAt) {
		t.Fatalf("setup: mark = %+v, ok=%v, want At=%v", mark, ok, deliveredAt)
	}

	// Resume, later — same text, so the driver finishes the SAME delivery.
	// #109's echo requirement: the capture must show OUR text so
	// confirmLanded's resume-time gate can attribute it.
	now = deliveredAt.Add(10 * time.Minute)
	f.setCapture("%1", "transcript\n✻ Brewed for 1m 0s\n"+rule+"\n❯ "+text+"\n"+rule+"\n")

	resumed, err := d.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume: outcome = %s (%s), want queued", resumed.Outcome, resumed.Reason)
	}

	mark2, ok := d.deliveryMarkFor("alpha💬", "/work/alpha")
	if !ok {
		t.Fatal("delivery mark disappeared after a resume")
	}
	if !mark2.At.Equal(deliveredAt) {
		t.Errorf("mark moved on resume: At = %v, want unchanged %v — a resume finishes the "+
			"SAME delivery and must not reset the #111 denominator", mark2.At, deliveredAt)
	}
}

// #111: Close forgets a destroyed session's delivery mark for the same
// reason it already forgets a stranded record — nothing left to count
// turns relative to once the composer it described no longer exists.
func TestCloseForgetsADeliveryMark(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	d.noteDelivery("alpha💬", "/work/alpha")

	if _, err := d.Close(context.Background(), expectStarted(1785600000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}); err != nil {
		t.Fatal(err)
	}

	if _, ok := d.deliveryMarkFor("alpha💬", "/work/alpha"); ok {
		t.Error("a delivery mark survived Close of the session it was made for")
	}
}

// §5.4 applied to this record, the same rule TestStrandedMatchRequiresTheSameCwd
// already pins for the stranded map: an id match alone is not identity.
func TestDeliveryMarkRequiresTheSameCwd(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	d.noteDelivery("alpha💬", "/work/alpha")

	if _, ok := d.deliveryMarkFor("alpha💬", "/somewhere/else"); ok {
		t.Error("matched a delivery mark against a different cwd")
	}
	if _, ok := d.deliveryMarkFor("alpha💬", "/work/alpha"); !ok {
		t.Error("the mark's own cwd should still match")
	}
}

// A delivery mark durably survives a restart the same way a stranded
// record does (#11) — a dispatched worker is meant to be checked on across
// a service restart, and `turns` going silently absent the moment this
// machine redeploys would defeat the field's own purpose.
func TestDeliveryMarkSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	deliveredAt := sessionStart.Add(1 * time.Hour)

	first := New("testbox", withExec((&fakeMux{}).exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt }), WithState(st))
	first.noteDelivery("alpha💬", "/work/alpha")

	second := New("testbox", withExec((&fakeMux{}).exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return deliveredAt.Add(1 * time.Hour) }), WithState(st))
	mark, ok := second.deliveryMarkFor("alpha💬", "/work/alpha")
	if !ok {
		t.Fatal("delivery mark did not survive the restart")
	}
	if !mark.At.Equal(deliveredAt) {
		t.Errorf("mark.At = %v, want the original delivery time %v to survive the restart", mark.At, deliveredAt)
	}
}

// The lifetime half, mirroring TestStrandedRecordExpiresOnReload: a mark
// kept forever eventually describes a delivery so old it is no longer
// useful evidence about anything. Swept on load, using the LOADING driver's
// own clock.
func TestDeliveryMarkExpiresOnReload(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := sessionStart

	first := New("testbox", withExec((&fakeMux{}).exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return old }), WithState(st))
	first.noteDelivery("alpha💬", "/work/alpha")

	second := New("testbox", withExec((&fakeMux{}).exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return old.Add(deliveryMarkRetention + time.Minute) }), WithState(st))
	if _, ok := second.deliveryMarkFor("alpha💬", "/work/alpha"); ok {
		t.Error("an expired delivery mark survived a reload")
	}
}
