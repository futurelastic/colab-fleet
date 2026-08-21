package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// resumeOutcomeOf is conversationOf's counterpart for ResumeOutcome.
func resumeOutcomeOf(t *testing.T, d *Driver, id string) *fleet.ResumeOutcome {
	t.Helper()
	got, err := d.List(context.Background(), testCaller, driver.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Items() {
		if s.ID == id {
			return s.ResumeOutcome
		}
	}
	t.Fatalf("session %q not in listing", id)
	return nil
}

// THE property #72 asked for: a resume that cannot be honoured must be
// reported as such, not silently substituted with a fresh conversation
// that reads as an ordinary healthy start.
func TestResumeOutcomeReportsASilentlyIgnoredResume(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-r",
		fleet.SessionSpec{Name: "r", Cwd: "/work/r", Resume: "wanted-conv"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The fake multiplexer's new-session is a no-op against its own session
	// table (see tmux_test.go) — simulate the session actually materialising,
	// the way a real one would moments after create returns.
	f.addSession(fakeSession{name: "r", paneID: "%r", cwd: "/work/r",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("r"))

	// The runtime started a FRESH conversation instead of the one requested
	// — exactly the defect #72 measured under a concurrent burst.
	writeRecord(t, root, "/work/r", "fresh-conv", "r", created.Add(time.Second))

	got := resumeOutcomeOf(t, d, "r")
	if got == nil {
		t.Fatal("a session created with resume must report a ResumeOutcome once its conversation resolves")
	}
	if got.Requested != "wanted-conv" {
		t.Errorf("Requested = %q, want wanted-conv", got.Requested)
	}
	if got.Honoured == nil {
		t.Fatal("the conversation resolved (fresh-conv exists); Honoured must not still be nil")
	}
	if *got.Honoured {
		t.Error("Honoured = true; the session's own record is a DIFFERENT conversation " +
			"(fresh-conv, not wanted-conv) — this is the silent downgrade #72 measured, " +
			"reported as success would be exactly the defect")
	}
	if got.Evidence == "" {
		t.Error("a false verdict must carry evidence, the same discipline every other honest answer here holds to")
	}
}

// The other half of the same property: when the runtime DOES continue the
// requested conversation, that must read as success — this is not a
// one-way "always report false" gate.
func TestResumeOutcomeReportsAnHonouredResume(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-r",
		fleet.SessionSpec{Name: "r", Cwd: "/work/r", Resume: "wanted-conv"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "r", paneID: "%r", cwd: "/work/r",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("r"))

	// This time the runtime actually continued the requested conversation:
	// the record on disk is the one that was asked for.
	writeRecord(t, root, "/work/r", "wanted-conv", "r", created.Add(time.Second))

	got := resumeOutcomeOf(t, d, "r")
	if got == nil {
		t.Fatal("a session created with resume must report a ResumeOutcome once its conversation resolves")
	}
	if got.Honoured == nil || !*got.Honoured {
		t.Errorf("Honoured = %v, want true — the session's own record is the requested conversation", got.Honoured)
	}
}

// Too early to say is a real, distinct answer (§5.7) — never collapsed into
// either a false verdict (which would read as the #72 defect when it is
// really just early) or a true one (which would be a guess).
func TestResumeOutcomeIsUnresolvedBeforeTheConversationResolves(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-r",
		fleet.SessionSpec{Name: "r", Cwd: "/work/r", Resume: "wanted-conv"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "r", paneID: "%r", cwd: "/work/r",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("r"))
	// No record written at all yet — the runtime has not filed anything.

	got := resumeOutcomeOf(t, d, "r")
	if got == nil {
		t.Fatal("a resume was requested; absent would claim nobody asked for one")
	}
	if got.Requested != "wanted-conv" {
		t.Errorf("Requested = %q, want wanted-conv", got.Requested)
	}
	if got.Honoured != nil {
		t.Errorf("Honoured = %v, want nil — the conversation has not resolved yet, "+
			"which must not read as a \"no\"", *got.Honoured)
	}
}

// A session created WITHOUT a resume request must carry no ResumeOutcome at
// all — absent is not a claim, but it must also never appear where nothing
// was ever asked for (§5.7's other direction).
func TestResumeOutcomeAbsentWhenNoResumeWasRequested(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-f",
		fleet.SessionSpec{Name: "f", Cwd: "/work/f"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "f", paneID: "%f", cwd: "/work/f",
		pid: 901, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("f"))
	writeRecord(t, root, "/work/f", "some-conv", "f", created.Add(time.Second))

	if got := resumeOutcomeOf(t, d, "f"); got != nil {
		t.Errorf("ResumeOutcome = %+v, want nil — this session's create never requested a resume", *got)
	}
}

// #72's honesty property has to survive a restart, not just a single
// process's lifetime — a fleet-wide recovery IS a restart, and that is
// exactly the scenario the resume defect was measured under (#48, #70).
// Same shape as TestStrandedRecordSurvivesARestart: a second Driver built
// over the same state directory, standing in for the process this service
// restarts as part of its own deploy.
func TestResumeIntentSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	f := twoSessions()
	created := sessionStart.Add(10 * time.Second)

	newDriver := func() *Driver {
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return created.Add(time.Second) }),
			WithState(st), WithRecordRoot(root))
	}

	first := newDriver()
	if _, err := first.Create(context.Background(), testCaller, "key-r",
		fleet.SessionSpec{Name: "r", Cwd: "/work/r", Resume: "wanted-conv"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "r", paneID: "%r", cwd: "/work/r",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("r"))

	// The restart. Nothing about the record store or the multiplexer
	// changed — only the driver process, the same way idempotency_test.go
	// and stranded_test.go simulate it.
	second := newDriver()

	// The runtime started a fresh conversation instead of the one
	// requested, discovered only now — after the restart — the way a real
	// recovery script's first listing would.
	writeRecord(t, root, "/work/r", "fresh-conv", "r", created.Add(time.Second))

	got := resumeOutcomeOf(t, second, "r")
	if got == nil {
		t.Fatal("the resume intent did not survive the restart — a second Driver over the " +
			"same state directory reported no ResumeOutcome at all")
	}
	if got.Requested != "wanted-conv" {
		t.Errorf("Requested = %q, want wanted-conv (lost across the restart)", got.Requested)
	}
	if got.Honoured == nil || *got.Honoured {
		t.Errorf("Honoured = %v, want false — the surviving intent must still catch the mismatch", got.Honoured)
	}
}
