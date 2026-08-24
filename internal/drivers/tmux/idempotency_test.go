package tmux

import (
	"context"
	"reflect"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/state"
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

	// A new process, same state directory — this is the restart.
	second := stateDriver(t, f, dir)
	before := countCalls(f, "new-session")
	again, err := second.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Cwd: "/work/new", Name: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	// DeepEqual, not !=: colab-fleet #84/#85/#86 added pointer-typed fields
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
	before := countCalls(f, "new-session")
	b, err := d.Create(context.Background(), testCaller, "k", fleet.SessionSpec{Cwd: "/w", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || countCalls(f, "new-session") != before {
		t.Error("in-process idempotency broke")
	}
}
