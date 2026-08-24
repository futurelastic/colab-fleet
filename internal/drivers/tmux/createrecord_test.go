package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// colab-fleet #86: a create-time prompt gets no delivery receipt directly on
// create, so a caller cannot tell "not delivered yet" from "never
// delivered". These tests exercise the fix through the real driver path —
// delivery_test.go in the root package already covers the fleet.PromptDelivery
// type's own round-trip and refusal-to-encode discipline.

// A create with no prompt must leave PromptDelivery absent — never a claim
// that a prompt was carried and delivered.
func TestCreate_NoPromptLeavesDeliveryAbsent(t *testing.T) {
	f := twoSessions()
	d := newTestDriver(f)
	sess, err := d.Create(context.Background(), testCaller, "key-quiet",
		fleet.SessionSpec{Name: "quiet", Cwd: "/work/x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.PromptDelivery != nil {
		t.Errorf("PromptDelivery = %+v, want nil (no prompt was carried)", sess.PromptDelivery)
	}
}

// A create carrying a prompt must report it as pending immediately, on the
// 201 body itself — before the async settle goroutine has had any real
// chance to run. The session it targets never becomes ready (no composer
// ever paints), so this is not a race against the goroutine: pending is the
// only value it can ever produce here.
func TestCreate_PromptDeliveryPendingWhenNotYetDelivered(t *testing.T) {
	f := twoSessions()
	f.captures["%1"] = "  loading...\n" // no composer painted — never "ready"
	d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
		withClock(func() time.Time { return time.Unix(1785760000, 0) }),
		withPromptDeliveryWindow(3*time.Second))

	sess, err := d.Create(context.Background(), testCaller, "key-1",
		fleet.SessionSpec{Name: "alpha", Cwd: "/work/alpha", Prompt: "do the thing"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.PromptDelivery == nil {
		t.Fatal("PromptDelivery is nil on a create that carried a prompt")
	}
	if sess.PromptDelivery.Outcome != nil {
		t.Errorf("Outcome = %v, want nil (not yet delivered) — this is exactly the "+
			"ambiguity #86 measured: idle-and-never-sent must not be confused with "+
			"idle-and-not-yet-delivered", *sess.PromptDelivery.Outcome)
	}
	if sess.PromptDelivery.Evidence == "" {
		t.Error("a pending delivery must still explain itself")
	}
}

// The invariant that matters most: every settleNewSession/deliverInitialPrompt
// exit that could otherwise leave a create's own prompt record unresolved
// forever must write a terminal outcome. A permanently-pending record is a
// worse false negative than the ambiguity #86 measured, because unlike an
// ordinary poll it never clears.
func TestPromptDeliveryAlwaysResolves(t *testing.T) {
	const cwd = "/work/alpha"
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	// seedRecord mimics what Create itself does (noteCreateRecord) without
	// going through the full create path — these tests drive
	// deliverInitialPrompt/settleNewSession directly, the same technique
	// TestDeliverInitialPromptCountsAStrandTheRetryCannotClear and
	// TestSettleNewSessionRecoversFromASwallowedInitialPromptSubmit already
	// use, for the same reason: pinning the exact exit under test rather
	// than routing it through Create's own naming/argv machinery.
	seedRecord := func(d *Driver, prompt string) {
		d.noteCreateRecord(ref.ID, cwd, fleet.SessionSpec{Cwd: cwd, Prompt: prompt})
	}

	t.Run("first attempt resolves without a retry", func(t *testing.T) {
		f := twoSessions()
		f.captures["%1"] = idleFixtureFor("alpha") // empty composer, ready
		d := newTestDriver(f)
		seedRecord(d, "hello")

		d.deliverInitialPrompt(context.Background(), testCaller, ref, "hello")

		rec, ok := d.createRecordFor(ref.ID, cwd)
		if !ok {
			t.Fatal("create record vanished")
		}
		if rec.PromptOutcome == "" {
			t.Fatal("PromptOutcome never resolved")
		}
		if rec.PromptOutcome == string(fleet.OutcomeUnknown) {
			t.Errorf("PromptOutcome = unknown on a delivery this substrate never needed to retry")
		}
		if rec.PromptEvidence == "" {
			t.Error("a resolved delivery must still carry evidence")
		}
	})

	t.Run("first attempt refused outright", func(t *testing.T) {
		// "beta" (paneID %2) already holds unsent text in twoSessions'
		// default fixture — the same setup TestSendRefusesWhenComposerHoldsUnsentInput
		// uses to reach OutcomeRefused on the very first Send.
		betaRef := fleet.SessionRef{Machine: "testbox", ID: "beta"}
		f := twoSessions()
		d := newTestDriver(f)
		d.noteCreateRecord(betaRef.ID, "/work/beta", fleet.SessionSpec{Cwd: "/work/beta", Prompt: "hi"})

		d.deliverInitialPrompt(context.Background(), testCaller, betaRef, "hi")

		rec, ok := d.createRecordFor(betaRef.ID, "/work/beta")
		if !ok || rec.PromptOutcome != string(fleet.OutcomeRefused) {
			t.Errorf("record = %+v (found=%v), want resolved refused", rec, ok)
		}
		if rec.PromptEvidence == "" {
			t.Error("a refusal must still explain itself in the evidence")
		}
	})

	t.Run("unknown then queued on the one retry", func(t *testing.T) {
		// #101: the retry's own submit must be genuinely confirmed now, not
		// reported blind — so the swallow that manufactures the strand must
		// be transient (swallowFirstSubmitOnly), matching #44's own measured
		// recovery ("receptive again seconds later, nothing changed but
		// time"), not a permanent swallow that would strand the retry too.
		f := twoSessions()
		f.captures["%1"] = idleFixtureFor("alpha")
		d := New("testbox", withExec(swallowFirstSubmitOnly(f)), withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Unix(1785760000, 0) }))
		const text = "the work it was created for, long enough to strand"
		seedRecord(d, text)

		d.deliverInitialPrompt(context.Background(), testCaller, ref, text)

		rec, ok := d.createRecordFor(ref.ID, cwd)
		if !ok || rec.PromptOutcome != string(fleet.OutcomeQueued) {
			t.Errorf("record = %+v (found=%v), want resolved queued (delivered on retry; this "+
				"substrate cannot confirm agent receipt on either attempt)", rec, ok)
		}
		if !strings.Contains(rec.PromptEvidence, "second attempt") {
			t.Errorf("evidence = %q, does not say this was the retry", rec.PromptEvidence)
		}
	})

	t.Run("unknown after the retry too (stranded)", func(t *testing.T) {
		f := twoSessions()
		f.captures["%1"] = idleFixtureFor("alpha")
		f.swallowSubmit = true
		exec := promptDeliveredThenInterrupted(f, "%1", "starting up\nloading configuration\n")
		d := New("testbox", withExec(exec), withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Unix(1785760000, 0) }))
		const text = "an instruction that must not vanish silently"
		seedRecord(d, text)

		d.deliverInitialPrompt(context.Background(), testCaller, ref, text)

		rec, ok := d.createRecordFor(ref.ID, cwd)
		if !ok || rec.PromptOutcome != string(fleet.OutcomeUnknown) {
			t.Errorf("record = %+v (found=%v), want resolved unknown (still stranded after the retry)", rec, ok)
		}
		if !strings.Contains(rec.PromptEvidence, "unsent") {
			t.Errorf("evidence = %q, does not say the text is sitting there unsent", rec.PromptEvidence)
		}
	})

	t.Run("a transport error on the first attempt", func(t *testing.T) {
		failing := func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "list-panes" {
				return nil, errors.New("boom: multiplexer unreachable")
			}
			return nil, nil
		}
		d := New("testbox", withExec(failing), withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Unix(1785760000, 0) }))
		seedRecord(d, "hello")

		d.deliverInitialPrompt(context.Background(), testCaller, ref, "hello")

		rec, ok := d.createRecordFor(ref.ID, cwd)
		if !ok || rec.PromptOutcome != string(fleet.OutcomeUnknown) {
			t.Errorf("record = %+v (found=%v), want resolved unknown (a transport failure is "+
				"not a refusal — nothing declined the delivery)", rec, ok)
		}
		if !strings.Contains(rec.PromptEvidence, "boom") {
			t.Errorf("evidence = %q, does not carry the underlying error", rec.PromptEvidence)
		}
	})

	// The window-expiry exit lives in settleNewSession, not
	// deliverInitialPrompt — the session never becomes ready, so
	// deliverInitialPrompt is never even reached.
	t.Run("the window closes before the session is ever ready", func(t *testing.T) {
		f := twoSessions()
		f.captures["%1"] = "  loading...\n" // no composer ever paints
		d := New("testbox", withExec(f.exec), withNonce(func() string { return testNonce }),
			withClock(func() time.Time { return time.Unix(1785760000, 0) }),
			withPromptDeliveryWindow(300*time.Millisecond))
		const text = "never delivered"
		seedRecord(d, text)

		done := make(chan struct{})
		go func() {
			defer close(done)
			d.settleNewSession(testCaller, ref, fleet.SessionSpec{Cwd: cwd, Prompt: text})
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("settleNewSession never returned")
		}

		rec, ok := d.createRecordFor(ref.ID, cwd)
		if !ok || rec.PromptOutcome != string(fleet.OutcomeUnknown) {
			t.Errorf("record = %+v (found=%v), want resolved unknown (window expired)", rec, ok)
		}
		if !strings.Contains(rec.PromptEvidence, "never became ready") {
			t.Errorf("evidence = %q, does not explain the window expired before readiness", rec.PromptEvidence)
		}
	})
}

// The resolved outcome on the create record must be the same value send()
// itself would have answered with — no separate vocabulary, no lossy
// mapping.
func TestPromptDeliveryOutcomeMatchesSendReceipt(t *testing.T) {
	betaRef := fleet.SessionRef{Machine: "testbox", ID: "beta"}
	f := twoSessions() // beta/%2 already holds unsent text: Send refuses
	d := newTestDriver(f)

	direct, err := d.Send(context.Background(), testCaller, betaRef, "hi", driver.SendOptions{Submit: true})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	d2 := newTestDriver(twoSessions())
	d2.noteCreateRecord(betaRef.ID, "/work/beta", fleet.SessionSpec{Cwd: "/work/beta", Prompt: "hi"})
	d2.deliverInitialPrompt(context.Background(), testCaller, betaRef, "hi")
	rec, ok := d2.createRecordFor(betaRef.ID, "/work/beta")
	if !ok {
		t.Fatal("create record vanished")
	}
	if rec.PromptOutcome != string(direct.Outcome) {
		t.Errorf("create-record outcome %q does not match what send() itself answers (%q) "+
			"for the identical delivery", rec.PromptOutcome, direct.Outcome)
	}
}

// promptDeliveryFor's own three states, exercised directly against a
// createRecord rather than through the driver — the mapping from stored
// record to fleet.PromptDelivery is itself worth pinning.
func TestPromptDeliveryFor(t *testing.T) {
	if got := promptDeliveryFor(createRecord{PromptCarried: false}); got != nil {
		t.Errorf("no prompt carried: got %+v, want nil", got)
	}
	pending := promptDeliveryFor(createRecord{PromptCarried: true})
	if pending == nil || pending.Outcome != nil || pending.Evidence == "" {
		t.Errorf("prompt carried, not yet resolved: got %+v, want pending with evidence", pending)
	}
	resolved := promptDeliveryFor(createRecord{
		PromptCarried: true, PromptOutcome: string(fleet.OutcomeSubmitted), PromptEvidence: "e",
	})
	if resolved == nil || resolved.Outcome == nil || *resolved.Outcome != fleet.OutcomeSubmitted || resolved.Evidence != "e" {
		t.Errorf("resolved delivery: got %+v, want submitted/e", resolved)
	}
}
