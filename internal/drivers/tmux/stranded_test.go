package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// #11: a delivery this driver made and could not confirm used to live only in
// d.stranded, a plain in-memory map. A restart dropped it — and restarting is
// how this service is deployed — so resumeIfStranded, the one door out of
// §2.4's busy-composer refusal, stopped working on exactly the deploys it
// exists to survive. These tests exercise the fix: a durable record,
// corroborated on more than the id (§5.4), swept after strandedRetention.
//
// The restart itself is simulated exactly as idempotency_test.go's
// TestIdempotencyKeysSurviveARestart does: a new Driver constructed over the
// same state directory. Confirmed to fail against the pre-fix code (reverting
// noteStranded/strandedMatches to the plain map[string]string and dropping
// loadStranded makes TestStrandedRecordSurvivesARestart's resume return
// refused instead of submitted, because the second Driver's d.stranded starts
// empty).
func TestStrandedRecordSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	f.noEcho = true // composer never renders what was pasted, so it strands
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	const text = "text that stranded before the restart"

	first := stateDriver(t, f, dir)
	got, err := first.Send(context.Background(), testCaller, ref, text, driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Fatalf("setup: outcome = %s, want unknown so the text is recorded as stranded", got.Outcome)
	}

	// A new process, same state directory — this is the restart. Nothing
	// about the composer changed: the text this driver placed is still
	// sitting there, unsubmitted, exactly as a restart leaves it.
	second := stateDriver(t, f, dir)
	f.setCapture("%1", fixtureUnsent)

	resumed, err := second.Send(context.Background(), testCaller, ref, text,
		driver.SendOptions{Submit: true, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	// #101: a confirmed resume now reports the same outcome the first-attempt
	// path reports for the same evidence — queued, never submitted, since
	// this substrate cannot observe agent receipt either way (§4.3). Refused
	// or unknown here would mean the record did not survive the restart.
	if resumed.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume after restart: outcome = %s (%s), want queued — the stranded "+
			"record did not survive the restart", resumed.Outcome, resumed.Reason)
	}
}

// The lifetime half of #11: a record kept forever eventually matches text a
// human typed that happens to be identical, which is precisely what
// strandedMatches's exact-match rule exists to exclude. Mirrors
// TestExpiredKeysAreSweptOnLoad's shape: sweep happens on LOAD, using the
// loading driver's own clock, not the clock that wrote the record.
func TestStrandedRecordExpiresOnReload(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1785760000, 0)

	first := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return old }), WithState(st))
	first.noteStranded("alpha💬", "/work/alpha", "text nobody came back for")

	// Reload well past strandedRetention.
	second := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return old.Add(strandedRetention + time.Minute) }), WithState(st))
	if second.strandedMatches("alpha💬", "/work/alpha", "text nobody came back for") {
		t.Error("an expired stranded record survived a reload")
	}
}

// §5.4 applied to this record: an id match alone is not identity. A durable
// record can outlive the session it describes, and a resume must not adopt
// whatever unrelated session later recycled the same id — corroborated here
// by cwd, the same attribute resolvePending already uses for the analogous
// idempotency case.
func TestStrandedMatchRequiresTheSameCwd(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	d.noteStranded("alpha💬", "/work/alpha", "the original delivery")

	if d.strandedMatches("alpha💬", "/somewhere/else", "the original delivery") {
		t.Error("matched a stranded record against a different cwd — the exact " +
			"failure §5.4 exists to prevent: a recycled id is not the session " +
			"that record was made for")
	}
	if !d.strandedMatches("alpha💬", "/work/alpha", "the original delivery") {
		t.Error("the record's own cwd should still match")
	}
}

// Once a session is destroyed there is nothing left to resume into, so Close
// forgets any stranded record for it rather than leaving it to wait out
// strandedRetention — a context-free leftover is exactly what could collide
// if the same name were reused before the window lapsed.
func TestCloseForgetsAStrandedRecord(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	d.noteStranded("alpha💬", "/work/alpha", "never resumed")

	if _, err := d.Close(context.Background(), expectStarted(1785600000),
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}); err != nil {
		t.Fatal(err)
	}

	if d.strandedMatches("alpha💬", "/work/alpha", "never resumed") {
		t.Error("a stranded record survived Close of the session it was made for")
	}
}
