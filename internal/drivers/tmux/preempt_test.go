package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// #180 M4, the reviewer's reproduction: a permission dialog is painted right
// after the paste, and a respond to it arrives 300 ms later with a 3 s
// deadline. The send must not hold the session's lock through its whole
// landed-check window: the respond is delivered, never refused as busy.
func TestRespondToAModalThatAppearedDuringASendIsDelivered(t *testing.T) {
	f := twoSessions()
	f.noEcho = true
	f.keyRepaint = map[string]bool{"%1": true}
	exec := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.exec(ctx, name, args...)
		if len(args) > 0 && args[0] == "load-buffer" {
			f.setCapture("%1", fixturePermissionDialog)
		}
		return out, err
	}
	d := New("testbox", withExec(exec), withNonce(func() string { return testNonce }), withClock(time.Now))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	done := make(chan fleet.DeliveryReceipt, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, _ := d.Send(ctx, testCaller, ref, "a message sent just before the modal", driver.SendOptions{Submit: true})
		done <- r
	}()
	time.Sleep(300 * time.Millisecond)
	p := parsePrompt(newScreen(fixturePermissionDialog))
	if p == nil {
		t.Fatal("setup: fixture is not a prompt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := d.Respond(ctx, testCaller, ref, fleet.Response{Choice: 3, Nonce: p.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got.Reason, composerBusyPrefix) {
		t.Fatalf("the respond to the modal was refused as busy: %s", got.Reason)
	}
	sent := <-done
	if submitsIn(f.callsSnapshot()) > 0 && sent.Outcome == fleet.OutcomeQueued {
		t.Fatalf("the send reported queued with a dialog on screen: %s", sent.Reason)
	}
}

// A respond waiting for the lock makes a send in its landed-check window stop
// at once, keep its record, and say why — never press Enter.
func TestSendGivesTheLockUpWhenPreempted(t *testing.T) {
	f := twoSessions()
	f.noEcho = true // nothing renders, so the send would otherwise poll the full window
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }), withClock(time.Now))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	c := d.preemptCounter(ref.ID)
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.Add(1)
	}()
	start := time.Now()
	got, err := d.Send(context.Background(), testCaller, ref, "hello", driver.SendOptions{Submit: true})
	c.Add(-1)
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the send held the lock for %s after being pre-empted", el)
	}
	if got.Outcome != fleet.OutcomeUnknown || !strings.Contains(got.Reason, "priority") {
		t.Fatalf("outcome = %s (%s), want unknown naming the priority respond", got.Outcome, got.Reason)
	}
	if submitsIn(f.callsSnapshot()) != 0 {
		t.Fatal("Enter was pressed after the send was pre-empted")
	}
	if _, kept := d.strandedRecordFor(ref.ID, "/work/alpha"); !kept {
		t.Fatal("the stranded record was not kept")
	}
}

// A lock the caller's deadline runs out waiting for is refused with a stable,
// documented prefix, so a consumer can tell this transient refusal from a
// final one, and it is counted as busy.
func TestLockTimeoutIsRecognisablyRetryable(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	unlock := d.lockComposerOps(ref.ID)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := d.Send(ctx, testCaller, ref, "hello", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused || !strings.HasPrefix(got.Reason, composerBusyPrefix) {
		t.Fatalf("outcome = %s (%s), want refused with the retryable prefix", got.Outcome, got.Reason)
	}
	if n := d.Counters()["delivery.tmux.refused.busy"]; n != 1 {
		t.Fatalf("delivery.tmux.refused.busy = %d, want 1", n)
	}
}
