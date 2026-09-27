package tmux

import (
	"context"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/state"
)

// THE property #224 asked for: a create's 201 already carries the captured
// id, so a caller wanting "click → open the session" never has to poll.
func TestConversationCapturedAtCreate(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	sess, err := d.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Name: "c", Cwd: "/work/c", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Conversation == nil {
		t.Fatal("the 201 must already carry the captured conversation, before anything the runtime wrote can be read")
	}
	if !sess.Conversation.Known || sess.Conversation.ID != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("Conversation = %+v, want Known=true ID=7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70", sess.Conversation)
	}
	if sess.Conversation.Source != fleet.ConversationCaptured {
		t.Errorf("Source = %q, want %q — this is the first real producer of that value", sess.Conversation.Source, fleet.ConversationCaptured)
	}
}

// A session created WITHOUT a conversationId must carry no Conversation from
// create at all — absent is not a claim, but it must never appear where
// nothing was ever asked for (§5.7's other direction, the same rule
// TestResumeOutcomeAbsentWhenNoResumeWasRequested holds for resume).
func TestConversationAbsentAtCreateWhenNoConversationIdWasRequested(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-p",
		fleet.SessionSpec{Name: "p", Cwd: "/work/p"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Conversation != nil {
		t.Errorf("Conversation = %+v, want nil — this create never requested one", sess.Conversation)
	}
}

// Before the runtime has written anything the record store can read, a
// listing must still answer with the captured id — never regress to "nobody
// looked", which would be a caller-visible downgrade from what create already
// reported.
func TestConversationIntentSurvivesUntilTheRuntimeWritesARecord(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Name: "c", Cwd: "/work/c", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "c", paneID: "%c", cwd: "/work/c",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("c"))
	// No record written yet — the runtime has not filed anything.

	got := conversationOf(t, d, "c")
	if got == nil {
		t.Fatal("a conversationId was requested; absent would claim nobody asked for one")
	}
	if !got.Known || got.ID != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("Conversation = %+v, want the captured id even before the runtime has written a record", got)
	}
}

// The agreement case: once the runtime's own record shows up naming the SAME
// id, the listing keeps reporting it — not a regression, and not a spurious
// mismatch either.
func TestConversationIntentAgreesWithTheRuntimesOwnRecord(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Name: "c", Cwd: "/work/c", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "c", paneID: "%c", cwd: "/work/c",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("c"))
	writeRecord(t, root, "/work/c", "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70", "c", created.Add(time.Second))

	got := conversationOf(t, d, "c")
	if got == nil || !got.Known || got.ID != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("Conversation = %+v, want the same id the runtime's own record now confirms", got)
	}
}

// THE mismatch property #224 named explicitly: a runtime that started a
// DIFFERENT conversation than the one this create asked for must be reported
// loudly, never silently overwritten — the #202 invariant applied to a
// requested id instead of a prior one.
func TestConversationIntentReportsALoudMismatchNeverASilentOverwrite(t *testing.T) {
	f := twoSessions()
	root := t.TempDir()
	created := sessionStart.Add(10 * time.Second)
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return created.Add(time.Second) }), WithRecordRoot(root))

	if _, err := d.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Name: "c", Cwd: "/work/c", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "c", paneID: "%c", cwd: "/work/c",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("c"))
	// The runtime ignored --session-id and started a conversation of its own.
	writeRecord(t, root, "/work/c", "unasked-for-conv", "c", created.Add(time.Second))

	got := conversationOf(t, d, "c")
	if got == nil {
		t.Fatal("a mismatch must still be a real answer, not absence")
	}
	if got.Known {
		t.Errorf("Known = true with ID %q; a disagreement between the requested id and the runtime's own "+
			"record must be reported as Known:false with evidence, never as either id alone", got.ID)
	}
	if got.Evidence == "" {
		t.Error("a disagreement must carry evidence naming both ids")
	}
}

func TestConversationIntentSurvivesARestart(t *testing.T) {
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
	if _, err := first.Create(context.Background(), testCaller, "key-c",
		fleet.SessionSpec{Name: "c", Cwd: "/work/c", ConversationId: "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.addSession(fakeSession{name: "c", paneID: "%c", cwd: "/work/c",
		pid: 900, created: created.Unix(), title: "2_1_220"}, idleFixtureFor("c"))

	// The restart. Nothing about the record store or the multiplexer
	// changed — only the driver process, the same shape as
	// TestResumeIntentSurvivesARestart.
	second := newDriver()

	got := conversationOf(t, second, "c")
	if got == nil {
		t.Fatal("the conversation intent did not survive the restart")
	}
	if !got.Known || got.ID != "7f3a1c22-0b9e-4d51-9f2a-8e6b1d4c5a70" {
		t.Errorf("Conversation = %+v, want the captured id to survive the restart", got)
	}
}
