package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #203: a session's conversation must not outlive the conversation the runtime
// is in. The runtime can start a new conversation inside the SAME process
// (`/clear`): the pid and its start time do not move, and only the `sessionId`
// in the runtime's per-process record does. #202 keyed the memo on the process,
// which is blind to this by construction.

const (
	// clearedConv is the conversation the process is in after a `/clear`.
	clearedConv = "3d3e3f40-3333-4333-8333-33333333bbb4"
	// clearedAgainConv is the one after a second `/clear`.
	clearedAgainConv = "4e4f5051-4444-4444-8444-44444444ccc5"
)

// clear is what a `/clear` looks like from outside: the same process, the same
// pid, the same start time — and a per-process record that now names another
// conversation.
func (r *conversationRig) clear(conv string) {
	r.t.Helper()
	r.processRecord(100, conv, "/work/alpha", processStart)
}

// removeProcessRecord is the runtime's record going away under a live process.
func (r *conversationRig) removeProcessRecord(pid int) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.sessions, itoa(pid)+".json")); err != nil {
		r.t.Fatal(err)
	}
}

// writeRawProcessRecord lays down bytes as they are, for a record that is not
// well-formed.
func (r *conversationRig) writeRawProcessRecord(pid int, raw string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.sessions, itoa(pid)+".json"), []byte(raw), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// The reported symptom. The first listing resolves from the process's record
// and the answer is remembered; the runtime then starts a new conversation in
// the same process, and the next listing must say so.
func TestConversationFollowsANewConversationStartedInTheSameProcess(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("precondition: the process's conversation must resolve, got %+v", got)
	}

	r.clear(clearedConv)

	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known || got.ID != clearedConv {
		t.Fatalf("the process is now in %s; the listing said %+v", clearedConv, got)
	}
	if !strings.Contains(got.Evidence, clearedConv) {
		t.Errorf("the evidence must quote the record as it reads now: %q", got.Evidence)
	}
}

// The session's name still points at the record its FIRST conversation titled.
// After a `/clear` that record is still there and still looks like the only
// candidate, so dropping the memo alone would hand the old id back one listing
// later — and every listing after. It is the previous conversation's answer.
func TestConversationAfterAClearIsNotReadBackFromTheNameOfItsPredecessor(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("precondition: %+v", got)
	}

	r.clear(clearedConv)

	for i := 0; i < 3; i++ {
		got := conversationOf(t, d, "alpha💬")
		if got == nil || got.ID == resumedConv {
			t.Fatalf("listing %d: the conversation the process left must not come back, got %+v", i, got)
		}
		if !got.Known || got.ID != clearedConv {
			t.Fatalf("listing %d: the process's own record names %s, got %+v", i, clearedConv, got)
		}
	}
}

// Two `/clear`s in a row: each is noticed, and neither earlier id comes back.
func TestConversationFollowsSeveralNewConversationsInTheSameProcess(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != resumedConv {
		t.Fatalf("precondition: %+v", got)
	}
	for _, want := range []string{clearedConv, clearedAgainConv} {
		r.clear(want)
		got := conversationOf(t, d, "alpha💬")
		if got == nil || !got.Known || got.ID != want {
			t.Fatalf("after clearing into %s the listing said %+v", want, got)
		}
	}
}

// The runtime can go back: `/resume` into a conversation the process left. The
// retirement is of the NAME's evidence, not of the id, so the process's own
// record still resolves it.
func TestConversationMayReturnToAnEarlierConversationWhenTheRecordSaysSo(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	_ = conversationOf(t, d, "alpha💬")
	r.clear(clearedConv)
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != clearedConv {
		t.Fatalf("precondition: %+v", got)
	}

	r.clear(resumedConv)
	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("the process's record names %s again, got %+v", resumedConv, got)
	}
}

// The cost contract. The check on a hit is a file read — the OS is not asked
// unless the record has changed, and then once, after which the new answer is
// remembered like any other.
func TestConversationCheckOnAHitCostsNoProcessQueryUntilTheRecordChanges(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	for i := 0; i < 5; i++ {
		if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != resumedConv {
			t.Fatalf("listing %d: %+v", i, got)
		}
	}
	if n := r.ps.calls(); n != 1 {
		t.Fatalf("five listings of an unchanged record asked the OS %d times, want 1", n)
	}

	r.clear(clearedConv)
	for i := 0; i < 5; i++ {
		if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != clearedConv {
			t.Fatalf("listing %d after the clear: %+v", i, got)
		}
	}
	if n := r.ps.calls(); n != 2 {
		t.Fatalf("noticing one clear must cost one more query and no more, asked %d times in all, want 2", n)
	}
}

// A record that cannot be read says nothing about whether the conversation
// changed. Dropping a sound answer every time a read came back thin would trade
// a stale answer for an intermittent unknown one.
func TestConversationMemoSurvivesARecordThatCannotBeRead(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != resumedConv {
		t.Fatalf("precondition: %+v", got)
	}

	for name, damage := range map[string]func(){
		"the record is gone":                         func() { r.removeProcessRecord(100) },
		"the record is not JSON":                     func() { r.writeRawProcessRecord(100, "{not json") },
		"the record's conversation id is not a UUID": func() { r.clear("../../elsewhere") },
		"the record names another working directory": func() { r.processRecord(100, clearedConv, "/work/elsewhere", processStart) },
	} {
		damage()
		if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
			t.Fatalf("%s: the remembered answer must stand, got %+v", name, got)
		}
	}
}

// A record written by a DIFFERENT process generation is not the runtime saying
// its own conversation changed. Its start time is not the one the memo was
// established against, so it is no evidence about this answer either way.
func TestConversationMemoIsNotDroppedByARecordOfAnotherProcessGeneration(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != resumedConv {
		t.Fatalf("precondition: %+v", got)
	}

	r.processRecord(100, clearedConv, "/work/alpha", processStart.Add(time.Hour))
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("a record another process generation wrote must not replace the answer, got %+v", got)
	}
}

// After a clear the new conversation cannot be corroborated — the OS will not
// say when the process started. The old id must still not be served: the answer
// is "cannot tell", with evidence, exactly as for a replaced process.
func TestConversationAfterAClearIsUnknownWhenTheNewOneCannotBeCorroborated(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || got.ID != resumedConv {
		t.Fatalf("precondition: %+v", got)
	}

	r.clear(clearedConv)
	delete(r.ps.inner.lines, 100)

	got := conversationOf(t, d, "alpha💬")
	if got == nil || got.Known || got.ID != "" {
		t.Fatalf("an uncorroborated new conversation must read unknown, never the old one, got %+v", got)
	}
}

// At the store: a hit is checked with the cheap read and never with the full
// one, whatever the record says. Asking the full source on a hit is what would
// put a `ps` in every listing.
func TestConversationStoreLookupOnAHitAsksOnlyThePeek(t *testing.T) {
	s := newConversationStore(t.TempDir())
	key := conversationKey{pane: "%1", created: sessionStart}
	src := liveAnswering(resumedConv, processStart)
	first := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, src)
	if first == nil || !first.Known {
		t.Fatalf("precondition: %+v", first)
	}

	asked := 0
	src.ask = func() liveConversation { asked++; return liveConversation{evidence: "must not be asked on a hit"} }
	src.peek = func() (recordedConversation, bool) {
		return recordedConversation{id: resumedConv, startedAt: processStart}, true
	}
	for i := 0; i < 3; i++ {
		if got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, src); got != first {
			t.Fatalf("a record that agrees must be answered from the memo, got %+v", got)
		}
	}
	if asked != 0 {
		t.Fatalf("the full source was asked %d times on hits, want 0", asked)
	}
}

// A memo that never held a start time cannot tell whose record it is looking at
// without asking the OS, and a hit does not ask the OS. It is served.
func TestConversationMemoWithoutAStartTimeIsNotDroppedByTheCheck(t *testing.T) {
	s := newConversationStore(t.TempDir())
	key := conversationKey{pane: "%1", created: sessionStart}
	first := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100},
		liveConversationSource{ask: func() liveConversation { return liveConversation{id: resumedConv, evidence: "test record"} }})
	if first == nil || !first.Known {
		t.Fatalf("precondition: %+v", first)
	}
	src := liveConversationSource{
		ask: func() liveConversation { return liveConversation{evidence: "must not be asked on a hit"} },
		peek: func() (recordedConversation, bool) {
			return recordedConversation{id: clearedConv, startedAt: processStart}, true
		},
	}
	if got := s.lookup(key, "/work/alpha", "alpha💬", sessionStart, processGeneration{pid: 100}, src); got != first {
		t.Fatalf("with no start time held there is nothing to corroborate against, got %+v", got)
	}
}
