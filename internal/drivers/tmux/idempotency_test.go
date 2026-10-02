package tmux

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/state"
)

func stateDriver(t *testing.T, f *fakeMux, dir string) *Driver {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New("testbox",
		withExec(f.exec),
		withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		WithState(st),
	)
}

// §10's actual requirement: retention must outlive the caller's retry window,
// and a service restart is inside that window.
func TestIdempotencyKeysSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()

	first := stateDriver(t, f, dir)
	ref, err := first.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Cwd: "/work/new", Name: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	// #234: the replay below now corroborates the record against what is
	// actually running, so the fixture has to say the session survived —
	// same convention conversationintent_test.go and resumeintent_test.go
	// already follow for a session a Create call is expected to outlive.
	f.addSession(fakeSession{name: "gamma", paneID: "%gamma", cwd: "/work/new",
		pid: 900, created: 1785760000, title: "2_1_220"}, idleFixtureFor("gamma"))

	// A new process, same state directory — this is the restart.
	second := stateDriver(t, f, dir)
	before := countCalls(f, "new-session")
	again, err := second.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Cwd: "/work/new", Name: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	// DeepEqual, not !=: muster #84/#85/#86 added pointer-typed fields
	// (Pins, RuntimeSurface, PromptDelivery) that are freshly allocated on
	// every call even for identical content — a struct-identity != would
	// spuriously fail on two independently-built Sessions describing the
	// same thing.
	if !reflect.DeepEqual(again, ref) {
		t.Errorf("after restart the key returned %+v, want the original %+v", again, ref)
	}
	if countCalls(f, "new-session") != before {
		t.Error("a retry across a restart started a second session — §10's exact disaster")
	}
}

// Intent is recorded before the session starts, so an interrupted create is
// recoverable. Here the record survives but the session does exist: the retry
// must adopt it rather than start another.
func TestInterruptedCreateAdoptsWhatItStarted(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the crash: a pending reservation, and a session that matches.
	idem, err := newIdemStore(st, time.Hour, func() time.Time { return time.Unix(1785760000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := idem.reserve("key-c", "alpha💬", "/work/alpha"); err != nil {
		t.Fatal(err)
	}

	d := stateDriver(t, f, dir)
	before := countCalls(f, "new-session")
	ref, err := d.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Cwd: "/work/alpha", Name: "alpha💬"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "alpha💬" {
		t.Errorf("ref = %+v, want the session the interrupted create started", ref)
	}
	if countCalls(f, "new-session") != before {
		t.Error("adopted nothing and started a duplicate instead")
	}
}

// The other branch: a pending record whose session does not exist means the
// create never took effect, and proceeding is safe.
func TestInterruptedCreateThatStartedNothingProceeds(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	st, _ := state.Open(dir)
	idem, _ := newIdemStore(st, time.Hour, func() time.Time { return time.Unix(1785760000, 0) })
	if err := idem.reserve("key-d", "never-started", "/work/ghost"); err != nil {
		t.Fatal(err)
	}

	d := stateDriver(t, f, dir)
	before := countCalls(f, "new-session")
	if _, err := d.Create(context.Background(), testCaller, "key-d",
		fleet.SessionSpec{Cwd: "/work/ghost", Name: "never-started"}); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "new-session") != before+1 {
		t.Error("a reservation for a session that never existed blocked a legitimate create")
	}
}

// §5.4's lesson applied to adoption: a recycled name with a different working
// directory is a different session, and adopting it would hand a caller
// something it never asked for.
func TestPendingAdoptionCorroboratesMoreThanTheName(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	st, _ := state.Open(dir)
	idem, _ := newIdemStore(st, time.Hour, func() time.Time { return time.Unix(1785760000, 0) })
	// Same name as a live session, different cwd.
	if err := idem.reserve("key-e", "alpha💬", "/somewhere/else"); err != nil {
		t.Fatal(err)
	}

	d := stateDriver(t, f, dir)
	before := countCalls(f, "new-session")
	if _, err := d.Create(context.Background(), testCaller, "key-e",
		fleet.SessionSpec{Cwd: "/somewhere/else", Name: "alpha💬"}); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "new-session") != before+1 {
		t.Error("adopted a session that merely shared a name (§5.4)")
	}
}

// muster #234: a replay whose recorded session has since ended must not
// come back as an ordinary 201 — that was indistinguishable from a live
// create succeeding, so a caller checking only the status code never learned
// its session was gone. Lucy's ruling (option a): 409, naming the ended
// session and when this driver observed the absence, and the key itself
// stays spent — no duplicate session, and no release that would let a THIRD
// replay quietly start one.
func TestReplayOfAnEndedSessionIsRefusedNotAnsweredAsLive(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	d := stateDriver(t, f, dir)

	created, err := d.Create(context.Background(), testCaller, "key-234",
		fleet.SessionSpec{Cwd: "/work/ghost", Name: "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	// The runtime process exits — same shape as a crash or a human closing it
	// from a terminal. Nothing about that goes through this service, so the
	// only trace is the pane disappearing from the next listing.
	f.dropSession("ghost")

	before := countCalls(f, "new-session")
	_, err = d.Create(context.Background(), testCaller, "key-234",
		fleet.SessionSpec{Cwd: "/work/ghost", Name: "ghost"})
	if err == nil {
		t.Fatal("expected the replay to be refused, not answered as a live create")
	}
	if countCalls(f, "new-session") != before {
		t.Error("a replay of an ended session must never start a second one")
	}

	var ferr *fleet.Error
	if !errors.As(err, &ferr) {
		t.Fatalf("error = %v, want a *fleet.Error the HTTP layer can pass straight through", err)
	}
	if ferr.Kind != fleet.ErrorConflict {
		t.Errorf("Kind = %q, want %q (409, not a fresh classification of what is really a stale key)", ferr.Kind, fleet.ErrorConflict)
	}
	if ferr.Reason != fleet.ReasonReplayOfEndedSession {
		t.Errorf("Reason = %q, want %q — a caller must be able to branch on this without parsing Message", ferr.Reason, fleet.ReasonReplayOfEndedSession)
	}
	if ferr.Session == nil || ferr.Session.ID != created.ID {
		t.Errorf("Session = %+v, want the ended session's own ref (%q)", ferr.Session, created.ID)
	}
	if ferr.ClosedAt == nil {
		t.Error("ClosedAt is nil; the caller has no bound on when the session it named actually ended")
	}

	// §10: "same key, same answer" — the key is not released, so it keeps
	// naming the SAME ended session rather than being silently reusable for a
	// brand new one.
	ref, rec, found := d.idem.lookup("key-234")
	if !found || rec.Phase != idemComplete || ref.ID != created.ID {
		t.Errorf("lookup(key-234) = %+v, %+v, %v; the key must stay spent on the same record, not be cleared by the refusal", ref, rec, found)
	}
}

// A failed create must not leave a reservation, or a retry is answered with a
// session that was never started.
func TestFailedCreateReleasesItsReservation(t *testing.T) {
	dir := t.TempDir()
	f := twoSessions()
	f.failCreate = true
	d := stateDriver(t, f, dir)

	if _, err := d.Create(context.Background(), testCaller, "key-f",
		fleet.SessionSpec{Cwd: "/w", Name: "doomed"}); err == nil {
		t.Fatal("expected the create to fail")
	}
	if _, _, found := d.idem.lookup("key-f"); found {
		t.Error("a failed create left a reservation behind")
	}
}

// Keys expire, and expiry survives a restart too.
func TestExpiredKeysAreSweptOnLoad(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	old := time.Unix(1785000000, 0)
	idem, _ := newIdemStore(st, time.Minute, func() time.Time { return old })
	if err := idem.complete("stale", fleet.SessionRef{Machine: "testbox", ID: "x"}); err != nil {
		t.Fatal(err)
	}
	// Reload much later.
	later, _ := newIdemStore(st, time.Minute, func() time.Time { return old.Add(time.Hour) })
	if _, _, found := later.lookup("stale"); found {
		t.Error("an expired key survived a reload")
	}
}

// Without a store the driver still works — in-memory is a legitimate
// configuration, not a degraded one.
func TestNoStoreStillEnforcesIdempotencyWithinTheProcess(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	a, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	// #234: the replay is now corroborated against what is actually running.
	f.addSession(fakeSession{name: "n", paneID: "%n", cwd: "/w",
		pid: 900, created: 1785760000, title: "2_1_220"}, idleFixtureFor("n"))
	before := countCalls(f, "new-session")
	b, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || countCalls(f, "new-session") != before {
		t.Error("in-process idempotency broke")
	}
}
