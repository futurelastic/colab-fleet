package tmux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleet "github.com/futurelastic/muster"
	"github.com/futurelastic/muster/internal/driver"
	"github.com/futurelastic/muster/internal/state"
)

// #240: a delivery confirmed by evidence that does not show a turn started —
// the runtime queueing the text, or the composer reading empty — used to leave
// no memory behind. When the runtime then handed the text back to the composer,
// the session sat unsent, the sender had been told `queued`, and the driver
// refused every attempt to finish it for want of a record. These tests drive
// that sequence end to end against the fake multiplexer.

const handbackText = "release is green, please merge when you are ready"

var handbackFrom = &fleet.MessageFrom{Agent: "agent-x", Machine: "entrybox"}

// handBack puts the labelled delivery back in the composer, unsent, the way a
// runtime that returned a queued message does.
func handBack(f *fakeMux, text string, from *fleet.MessageFrom) {
	f.setCapture("%1", composerHoldingRows(strings.Split(paneLabelled(text, from), "\n")))
}

func stateOf(t *testing.T, d *Driver, ref fleet.SessionRef) fleet.SessionState {
	t.Helper()
	st, err := d.State(context.Background(), testCaller, ref)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The oracle. Screen-only confirmation (no transcript is configured): the
// composer empties, the receipt is queued, then the text comes back.
func TestHandedBackQueuedDeliveryIsKnownAsOurOwnAndResumable(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	sent, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Outcome != fleet.OutcomeQueued {
		t.Fatalf("setup: outcome = %s (%s), want queued", sent.Outcome, sent.Reason)
	}
	if !strings.Contains(sent.Reason, "strandedDelivery") {
		t.Errorf("reason = %q: a confirmation that shows no turn started must say what the sender will see if the text is handed back", sent.Reason)
	}
	if got := d.counters.Snapshot()[counterStrandedProvisionalKept]; got != 1 {
		t.Fatalf("provisional_kept = %d, want 1", got)
	}

	handBack(f, handbackText, handbackFrom)

	st := stateOf(t, d, ref)
	if st.WaitingOn != fleet.WaitingUnsentInput {
		t.Fatalf("setup: waitingOn = %q (%s), want unsent-input", st.WaitingOn, st.Evidence)
	}
	if !st.StrandedDelivery {
		t.Fatal("the unsent text is a message this driver delivered and reported queued; state must say so")
	}

	// A different message is still refused, and the refusal now says whose text is there.
	other, err := d.Send(context.Background(), testCaller, ref, "something else",
		driver.SendOptions{Submit: true, From: handbackFrom})
	if err != nil {
		t.Fatal(err)
	}
	if other.Outcome != fleet.OutcomeRefused || !strings.Contains(other.Reason, "handed it back") {
		t.Fatalf("different text: outcome = %s (%s), want a refusal naming the hand-back", other.Outcome, other.Reason)
	}

	// The sender's own resume finishes it, with no digest and no screen reading.
	resumed, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Outcome != fleet.OutcomeQueued {
		t.Fatalf("resume: outcome = %s (%s), want queued — the sender must be able to finish a handed-back delivery", resumed.Outcome, resumed.Reason)
	}
	if got := d.counters.Snapshot()[counterStrandedProvisionalHandedBack]; got < 1 {
		t.Errorf("provisional_handed_back = %d, want at least 1", got)
	}
}

// §2.4 still holds: a composer holding anything other than the delivered text
// is not ours, whatever the driver remembers.
func TestHandedBackProofIsTheTextNotTheSession(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
	if _, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom}); err != nil {
		t.Fatal(err)
	}

	f.setCapture("%1", composerHoldingRows([]string{"a draft a person is typing"}))
	st := stateOf(t, d, ref)
	if st.WaitingOn != fleet.WaitingUnsentInput {
		t.Fatalf("setup: waitingOn = %q (%s)", st.WaitingOn, st.Evidence)
	}
	if st.StrandedDelivery {
		t.Fatal("a person's draft was reported as this driver's own delivery")
	}
	got, err := d.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom, ResumeIfStranded: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("resume over a person's draft: outcome = %s (%s), want refused", got.Outcome, got.Reason)
	}
}

// With a transcript: the runtime queued the text (an enqueue — not proof a
// turn started), later returned it (a popAll), and the memory survives a
// restart of the service.
func TestHandedBackAfterAnEnqueueSurvivesARestart(t *testing.T) {
	recordRoot := t.TempDir()
	cwd := "/work/alpha"
	sessionName := "alpha💬"
	convDir := filepath.Join(recordRoot, recordDirFor(cwd))
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convPath := filepath.Join(convDir, "conv-1.jsonl")
	if err := os.WriteFile(convPath, []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": sessionName, "sessionId": "conv-1",
		"timestamp": time.Now().Format(time.RFC3339Nano),
	})+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	labelled := paneLabelled(handbackText, handbackFrom)
	dir := t.TempDir()
	f := twoSessions()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	build := func() *Driver {
		return New("testbox",
			withExec(appendOnSubmit(t, f.exec, convPath, mustJSONLine(t, map[string]any{
				"type": "queue-operation", "operation": "enqueue", "sessionId": "conv-1",
				"timestamp": time.Now().Format(time.RFC3339Nano), "content": labelled,
			}))),
			withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Now() }),
			WithRecordRoot(recordRoot),
			WithState(st),
		)
	}
	first := build()
	ref := fleet.SessionRef{Machine: "testbox", ID: sessionName}
	sent, err := first.Send(context.Background(), testCaller, ref, handbackText,
		driver.SendOptions{Submit: true, From: handbackFrom})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Outcome != fleet.OutcomeQueued {
		t.Fatalf("setup: outcome = %s (%s)", sent.Outcome, sent.Reason)
	}
	snap := first.counters.Snapshot()
	if snap[counterSubmitConfirmedByEnqueue] != 1 || snap[counterStrandedProvisionalKept] != 1 {
		t.Fatalf("by_enqueue = %d, provisional_kept = %d, want 1 and 1", snap[counterSubmitConfirmedByEnqueue], snap[counterStrandedProvisionalKept])
	}

	appendLine(t, convPath, `{"type":"queue-operation","operation":"popAll","sessionId":"conv-1"}`)
	handBack(f, handbackText, handbackFrom)

	second := build() // a restart: new process, same state directory
	got := stateOf(t, second, ref)
	if got.WaitingOn != fleet.WaitingUnsentInput || !got.StrandedDelivery {
		t.Fatalf("after a restart: waitingOn = %q, strandedDelivery = %v (%s)", got.WaitingOn, got.StrandedDelivery, got.Evidence)
	}

	// Had the runtime started a turn on the text, what is in the composer now is
	// the same words again — nothing says whose.
	appendLine(t, convPath, mustJSONLine(t, map[string]any{
		"type": "user", "sessionId": "conv-1",
		"message": map[string]any{"role": "user", "content": labelled},
	}))
	got = stateOf(t, second, ref)
	if got.StrandedDelivery {
		t.Fatal("the runtime ran this text after the delivery; a copy in the composer is not proven to be ours")
	}
}
