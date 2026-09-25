package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #202: a session's conversation must not outlive the process it was
// established against. A recovery tool that relaunches the runtime inside the
// same multiplexer session leaves the pane, its creation time and the session's
// name as they were, and the process in it — with its own conversation — is a
// different one.

const (
	// replacedPid is the pane's process after the relaunch.
	replacedPid = 300
	// replacedConv is the conversation the relaunched process is in.
	replacedConv = "2c2d2e2f-2222-4222-8222-22222222aaa3"
)

// replacedStart is when the relaunched process started: an hour after the first.
var replacedStart = processStart.Add(time.Hour)

// replaceProcess is what the relaunch looks like from outside: the pane keeps
// its id and its creation time, the old process is gone with its per-process
// record, and a new process holds the pane under a new pid. The new process has
// written no record yet — a test that wants one writes it.
func (r *conversationRig) replaceProcess() {
	r.t.Helper()
	r.mux.mu.Lock()
	r.mux.sessions[0].pid = replacedPid
	r.mux.mu.Unlock()
	delete(r.ps.inner.lines, 100)
	r.ps.inner.set(replacedPid, replacedStart)
	if err := os.Remove(filepath.Join(r.sessions, "100.json")); err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
}

// The reported symptom. The first listing resolves from the old process's
// record and the answer is remembered; after the relaunch the next listing must
// name the new process's conversation, not the remembered one.
func TestConversationIsNotKeptAfterThePanesProcessIsReplaced(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("precondition: the first process's conversation must resolve, got %+v", got)
	}

	r.replaceProcess()
	r.processRecord(replacedPid, replacedConv, "/work/alpha", replacedStart)

	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known || got.ID != replacedConv {
		t.Fatalf("the relaunched process is in %s; the listing said %+v", replacedConv, got)
	}
	if !strings.Contains(got.Evidence, "pid 300") {
		t.Errorf("the evidence must be about the process running now: %q", got.Evidence)
	}
}

// A relaunched process has not written its record yet. Until it does, the only
// honest answer is "cannot tell" — never the conversation of the process that is
// gone.
func TestConversationOfAReplacedProcessIsUnknownUntilItsRecordExists(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(-72*time.Hour))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known {
		t.Fatalf("precondition: %+v", got)
	}

	r.replaceProcess()
	got := conversationOf(t, d, "alpha💬")
	if got == nil || got.Known || got.ID != "" {
		t.Fatalf("a replaced process with no record must read unknown, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "pid 300") {
		t.Errorf("the refusal must say which process it could not corroborate: %q", got.Evidence)
	}

	r.processRecord(replacedPid, replacedConv, "/work/alpha", replacedStart)
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != replacedConv {
		t.Fatalf("the record appeared; the next listing must resolve it, got %+v", got)
	}
}

// The session's name still points at the record its FIRST process titled, and
// after a relaunch that record is still there and still the only candidate. It
// is the predecessor's answer: serving it because the memo was dropped would
// hand the old id back one listing later, and it must stay refused on every
// listing after that too, not only the first.
func TestConversationOfAReplacedProcessIsNotReadBackFromTheNameOfItsPredecessor(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("precondition: %+v", got)
	}

	r.replaceProcess()
	for i := 0; i < 3; i++ {
		got := conversationOf(t, d, "alpha💬")
		if got == nil || got.Known || got.ID != "" {
			t.Fatalf("listing %d: the predecessor's conversation must not come back, got %+v", i, got)
		}
		if !strings.Contains(got.Evidence, "replaced") {
			t.Errorf("listing %d: the refusal must say the process was replaced: %q", i, got.Evidence)
		}
	}

	// And once the new process names its own conversation, the predecessor's
	// title record is not a rival for it: it is not evidence about this process,
	// so the two sources do not "disagree".
	r.processRecord(replacedPid, replacedConv, "/work/alpha", replacedStart)
	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known || got.ID != replacedConv {
		t.Fatalf("the new process's own record must resolve, got %+v", got)
	}
}

// A relaunch may continue the very conversation the old process was in. When
// the new process's own record says so, that is a corroborated answer about the
// process running now — the retirement is of the NAME's evidence, not of the id.
func TestConversationOfAReplacedProcessMayResolveToTheSameIdWhenItsOwnRecordSaysSo(t *testing.T) {
	r := newConversationRig(t)
	writeRecord(t, r.records, "/work/alpha", resumedConv, "alpha💬", sessionStart.Add(4*time.Second))
	r.processRecord(100, resumedConv, "/work/alpha", processStart)

	d := r.driver()
	if got := conversationOf(t, d, "alpha💬"); got == nil || !got.Known {
		t.Fatalf("precondition: %+v", got)
	}

	r.replaceProcess()
	r.processRecord(replacedPid, resumedConv, "/work/alpha", replacedStart)

	got := conversationOf(t, d, "alpha💬")
	if got == nil || !got.Known || got.ID != resumedConv {
		t.Fatalf("the new process's own record names the same conversation, got %+v", got)
	}
	if !strings.Contains(got.Evidence, "pid 300") {
		t.Errorf("the evidence must be the new process's record, not the old answer: %q", got.Evidence)
	}
}
