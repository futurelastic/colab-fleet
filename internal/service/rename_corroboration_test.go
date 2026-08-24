package service

import (
	"testing"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
	"github.com/godx-jp/colab-fleet/internal/driver"
)

// recvRenamed reads the next event off ch, failing the test if it is not a
// session.renamed, and returns its payload.
func recvRenamed(t *testing.T, ch <-chan fleet.Event) fleet.SessionRenamed {
	t.Helper()
	select {
	case ev := <-ch:
		p, ok := ev.Payload.(fleet.SessionRenamed)
		if !ok || ev.Kind != fleet.EventSessionRenamed {
			t.Fatalf("got %v, want a session.renamed", ev)
		}
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("no session.renamed arrived")
		return fleet.SessionRenamed{}
	}
}

func assertNoEventWithin(t *testing.T, ch <-chan fleet.Event, d time.Duration) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("expected silence, got %v", ev)
	case <-time.After(d):
	}
}

// colab-fleet #103: a rename that nothing ever disturbs must be told, once,
// that it held — not left as a permanent "accepted" that a subscriber has to
// take on faith.
func TestRenameCorroboratesWhenWindowElapsesCleanly(t *testing.T) {
	h := newHub("testbox", "e")
	h.rename.window = 20 * time.Millisecond
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	started := time.Now()
	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionRenamed,
		Payload: fleet.SessionRenamed{
			Machine: "testbox", From: "old💬", To: "new💬",
			StartedAt: &started, Corroboration: fleet.RenameAccepted,
		},
	})
	h.watchRename("testbox", "old💬", "new💬", &started)

	first := recvRenamed(t, sub.ch)
	if first.Corroboration != fleet.RenameAccepted {
		t.Fatalf("first event = %q, want accepted", first.Corroboration)
	}

	second := recvRenamed(t, sub.ch)
	if second.Corroboration != fleet.RenameCorroborated {
		t.Fatalf("follow-up = %q, want corroborated", second.Corroboration)
	}
	if second.From != "old💬" || second.To != "new💬" {
		t.Errorf("follow-up ids = %q -> %q, want old💬 -> new💬", second.From, second.To)
	}

	assertNoEventWithin(t, sub.ch, 50*time.Millisecond)
}

// The exact shape #97 measured: the new id stops resolving and the old id's
// identity — matched by StartedAt, never by name alone (§5.4) — comes back.
// This must be reported as CONTESTED, and promptly: it does not wait out the
// rest of the corroboration window once it already knows.
func TestRenameContestedWhenOldIdentityReappearsAfterNewIdCloses(t *testing.T) {
	h := newHub("testbox", "e")
	h.rename.window = time.Hour // long enough that a timely contested verdict proves it wasn't the deadline firing
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	started := time.Now()
	h.watchRename("testbox", "old💬", "new💬", &started)

	// The old id's session reappears — reported the way session.created
	// always is, full Session, StartedAt included.
	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionCreated,
		Payload: fleet.Session{
			SessionRef: fleet.SessionRef{Machine: "testbox", ID: "old💬"},
			StartedAt:  &started,
		},
	})
	drainOne(t, sub.ch) // the session.created itself

	// Then the new id stops resolving.
	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionClosed,
		Payload: fleet.SessionStatePayload{
			Ref: fleet.SessionRef{Machine: "testbox", ID: "new💬"},
		},
	})
	drainOne(t, sub.ch) // the session.closed itself

	got := recvRenamed(t, sub.ch)
	if got.Corroboration != fleet.RenameContested {
		t.Fatalf("verdict = %q, want contested", got.Corroboration)
	}
	if got.From != "old💬" || got.To != "new💬" {
		t.Errorf("ids = %q -> %q, want old💬 -> new💬", got.From, got.To)
	}
}

// A session.created for the old id with a DIFFERENT StartedAt is a
// coincidental reuse of the name, not a revert — §5.4's rule that an id
// match alone is never identity. It must not be mistaken for the #97
// signature.
func TestRenameNotContestedWhenStartedAtDiffers(t *testing.T) {
	h := newHub("testbox", "e")
	h.rename.window = 30 * time.Millisecond
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	original := time.Now()
	other := original.Add(time.Hour)
	h.watchRename("testbox", "old💬", "new💬", &original)

	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionCreated,
		Payload: fleet.Session{
			SessionRef: fleet.SessionRef{Machine: "testbox", ID: "old💬"},
			StartedAt:  &other,
		},
	})
	drainOne(t, sub.ch)

	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionClosed,
		Payload: fleet.SessionStatePayload{Ref: fleet.SessionRef{Machine: "testbox", ID: "new💬"}},
	})
	drainOne(t, sub.ch)

	got := recvRenamed(t, sub.ch)
	if got.Corroboration != fleet.RenameUnconfirmed {
		t.Fatalf("verdict = %q, want unconfirmed (an id match alone must never read as the revert signature)", got.Corroboration)
	}
}

// The new id closing on its own — no reappearance of the old identity — is
// exactly as consistent with an ordinary DELETE as with an unattributable
// revert. It must not be overclaimed as CONTESTED.
func TestRenameUnconfirmedWhenClosedWithNoRevertSignature(t *testing.T) {
	h := newHub("testbox", "e")
	h.rename.window = time.Hour
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	started := time.Now()
	h.watchRename("testbox", "old💬", "new💬", &started)

	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSessionClosed,
		Payload: fleet.SessionStatePayload{Ref: fleet.SessionRef{Machine: "testbox", ID: "new💬"}},
	})
	drainOne(t, sub.ch)

	got := recvRenamed(t, sub.ch)
	if got.Corroboration != fleet.RenameUnconfirmed {
		t.Fatalf("verdict = %q, want unconfirmed", got.Corroboration)
	}
}

// A hole in this hub's own observation (a degraded source, or a resync)
// during the window means "corroborated" is not a claim it earned — it did
// not watch continuously, so it cannot swear nothing happened in the gap.
func TestRenameUnconfirmedWhenFeedGapsDuringWindow(t *testing.T) {
	h := newHub("testbox", "e")
	h.rename.window = 20 * time.Millisecond
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	started := time.Now()
	h.watchRename("testbox", "old💬", "new💬", &started)

	h.publish(fleet.Event{
		Machine: "testbox", Kind: fleet.EventSourceStatus,
		Payload: fleet.SourceStatus{Machine: "testbox", Status: fleet.SourceDegraded, ObservedAt: time.Now()},
	})
	drainOne(t, sub.ch)

	got := recvRenamed(t, sub.ch)
	if got.Corroboration != fleet.RenameUnconfirmed {
		t.Fatalf("verdict = %q, want unconfirmed", got.Corroboration)
	}
}

// A rename to an id no earlier rename is still watching is unaffected by
// another machine's pending watch, or by a same-id event on a DIFFERENT
// machine — the key is (machine, to), not to alone.
func TestRenameWatchIsScopedPerMachine(t *testing.T) {
	h := newHub("testbox", "e")
	h.rename.window = 20 * time.Millisecond
	sub, _, _ := h.add(ScopeFleet, driver.SubscribeFilter{}, 0, "")

	started := time.Now()
	h.watchRename("testbox", "old💬", "new💬", &started)

	// A same-id closed event on a DIFFERENT machine must not resolve this
	// pending watch.
	h.publish(fleet.Event{
		Machine: "otherbox", Kind: fleet.EventSessionClosed,
		Payload: fleet.SessionStatePayload{Ref: fleet.SessionRef{Machine: "otherbox", ID: "new💬"}},
	})
	drainOne(t, sub.ch)

	got := recvRenamed(t, sub.ch)
	if got.Corroboration != fleet.RenameCorroborated {
		t.Fatalf("verdict = %q, want corroborated — a different machine's event must not resolve this watch", got.Corroboration)
	}
	if got.Machine != "testbox" {
		t.Errorf("machine = %q, want testbox", got.Machine)
	}
}

// Service.publishRename is http.go's one entry point for announcing a
// rename (handleRename) — this exercises it the way that handler does,
// confirming the accept event still fires at the same moment it always did
// and that the watch it registers is the same one rename_corroboration.go
// tests exercise directly against the hub.
func TestServicePublishRenameEmitsAcceptedThenWatches(t *testing.T) {
	svc := New("testbox")
	svc.events.rename.window = 20 * time.Millisecond

	ctx := t.Context()
	ch, _, _, cancel := svc.Events(ctx, ScopeFleet, driver.SubscribeFilter{}, 0, "")
	defer cancel()

	started := time.Now()
	svc.publishRename("testbox", "old💬", "new💬", &started)

	first := recvRenamed(t, ch)
	if first.Corroboration != fleet.RenameAccepted {
		t.Fatalf("first event = %q, want accepted", first.Corroboration)
	}

	second := recvRenamed(t, ch)
	if second.Corroboration != fleet.RenameCorroborated {
		t.Fatalf("follow-up = %q, want corroborated", second.Corroboration)
	}
}

func drainOne(t *testing.T, ch <-chan fleet.Event) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected an event, got none")
	}
}
