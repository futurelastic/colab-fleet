package tmux

import (
	"context"
	"sync"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// strandThenAge strands text with a swallowed submit, then moves the clock
// past strandedRetention so the live record lapses.
func strandThenAge(t *testing.T, text string) (*fakeMux, *Driver, fleet.SessionRef) {
	t.Helper()
	f := twoSessions()
	f.swallowSubmit = true
	var mu sync.Mutex
	now := time.Unix(1785760000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }), withClock(clock))
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: want unknown, got %s (%s)", first.Outcome, first.Reason)
	}
	f.mu.Lock()
	f.swallowSubmit = false
	f.mu.Unlock()
	mu.Lock()
	now = now.Add(strandedRetention + time.Minute)
	mu.Unlock()
	if _, live := d.strandedRecordFor(ref.ID, "/work/alpha"); live {
		t.Fatal("setup: the live record should have lapsed")
	}
	return f, d, ref
}

// #180 M2 (#135's own field incident): a retry that outlives the live
// record's retention still finds the composer holding this driver's own
// text. The tombstone proves it is the driver's own, so resumeIfStranded
// clears it and delivers — without the unconditional clear #135 once meant.
func TestResumeAfterStrandedRetentionLapsedStillRecoversOwnText(t *testing.T) {
	const text = "status report please"
	_, d, ref := strandThenAge(t, text)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	retry, err := d.Send(ctx, testCaller, ref, text, driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Outcome != fleet.OutcomeQueued {
		t.Fatalf("outcome = %s (%s), want queued", retry.Outcome, retry.Reason)
	}
}

// The tombstone proves only the driver's OWN text. After the record lapsed, a
// person edited it — whitespace only — and it is theirs now: refused, kept.
func TestTombstoneNeverMatchesAnEditedDraft(t *testing.T) {
	const text = "rm -rf /tmp/build"
	f, d, ref := strandThenAge(t, text)
	f.setCapture("%1", composerHolding("rm -rf / tmp/build"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, opts := range []driver.SendOptions{
		{Submit: true, ResumeIfStranded: true},
		{Submit: true, ReplaceIfStranded: true},
	} {
		got, err := d.Send(ctx, testCaller, ref, text, opts)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != fleet.OutcomeRefused {
			t.Fatalf("%+v: outcome = %s (%s), want refused", opts, got.Outcome, got.Reason)
		}
	}
	if n := countClears(f.callsSnapshot()); n != 0 {
		t.Fatalf("%d clear keystroke(s) against a person's edited draft", n)
	}
}

// Tombstones survive a restart with the rest of the stranded document.
func TestTombstonesArePersisted(t *testing.T) {
	dir := t.TempDir()
	d := stateDriver(t, twoSessions(), dir)
	d.noteStranded("s1", "/work", "first text", composerTextDigest("first text"))
	d.noteStranded("s1", "/work", "second text", composerTextDigest("second text")) // buries the first

	d2 := stateDriver(t, twoSessions(), dir)
	if !d2.tombstoneProves("s1", "/work", "first text", newScreen(composerHolding("first text"))) {
		t.Fatal("a tombstone did not survive a restart")
	}
	if d2.tombstoneProves("s1", "/elsewhere", "first text", newScreen(composerHolding("first text"))) {
		t.Fatal("a tombstone matched a different working directory")
	}
}
