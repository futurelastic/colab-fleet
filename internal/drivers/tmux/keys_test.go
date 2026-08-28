package tmux

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/godx-jp/colab-fleet"
)

// A dialog this driver's classifier does not recognise: no options it can
// parse, no composer, just a full screen waiting on arrow keys. This is the
// shape the whole operation exists for.
const fixtureOpaqueDialog = `  Reconnect to the session?

    ▸ Restore the previous conversation
      Start fresh

  Use ↑/↓ to choose, Enter to confirm.`

func dialogMux() *fakeMux {
	return &fakeMux{
		sessions: []fakeSession{
			{name: "alpha💬", paneID: "%1", cwd: "/work/alpha", pid: 100, created: 1785600000, title: "2_1_220"},
		},
		captures:   map[string]string{"%1": fixtureOpaqueDialog},
		keyRepaint: map[string]bool{"%1": true},
	}
}

func digestOf(t *testing.T, d *Driver, id string) string {
	t.Helper()
	st, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.ScreenDigest == "" {
		t.Fatal("a session whose screen was read must publish a digest to quote back")
	}
	return st.ScreenDigest
}

// composerDigestOf reads the composer-scope digest a caller sees when the
// composer holds unsent text — the value GET publishes as ComposerDigest,
// not ScreenDigest, and the one keys.go's composer-holds-text branch now
// corroborates against (colab-fleet#127).
func composerDigestOf(t *testing.T, d *Driver, id string) string {
	t.Helper()
	st, err := d.State(context.Background(), testCaller, fleet.SessionRef{Machine: "testbox", ID: id})
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.ComposerDigest == "" {
		t.Fatal("a session whose composer holds unsent text must publish a composerDigest to quote back")
	}
	return st.ComposerDigest
}

// The whole point: a key lands on a screen nothing classified, and the driver
// confirms it landed by watching the dialog redraw.
func TestKeysDeliversToAnUnrecognisedDialogAndConfirmsTheRedraw(t *testing.T) {
	f := dialogMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyDown, want)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Errorf("outcome = %q (%s); a screen that changed under the key is the "+
			"confirmation this operation has", got.Outcome, got.Reason)
	}
}

// A screen that did not move is reported as unknown, never as submitted. A
// supervisor told a keypress landed stops trying.
func TestKeysReportsUnknownWhenTheScreenDoesNotMove(t *testing.T) {
	f := dialogMux()
	f.keyRepaint = nil // the dialog swallows it
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyDown, want)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Errorf("outcome = %q (%s); an unchanged screen confirms nothing", got.Outcome, got.Reason)
	}
}

// The nonce's replacement. A caller quoting a digest from a screen that has
// since moved on is refused, because the key it chose was chosen against a
// screen that no longer exists.
func TestKeysRefusesAStaleScreenDigest(t *testing.T) {
	d := newTestDriver(dialogMux())

	_, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, "a-digest-from-some-other-screen")
	if err == nil {
		t.Fatal("a key sent against a screen the caller did not see must be refused")
	}
	if !strings.Contains(err.Error(), "changed since the caller read it") {
		t.Errorf("error = %v; it must say the screen moved, so a caller knows to re-read", err)
	}
}

// Pressing keys on a screen nobody has read is the blind delivery the digest
// exists to prevent, and it is refused outright — the same ruling discard makes
// about deleting text nobody looked at.
func TestKeysRefusesWithoutADigestAtAll(t *testing.T) {
	d := newTestDriver(dialogMux())

	_, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, "")
	if err == nil {
		t.Fatal("a key with no corroboration at all must be refused")
	}
	if !strings.Contains(err.Error(), "has not read") {
		t.Errorf("error = %v; it must say what is missing and where to get it", err)
	}
}

// Enter into a composer holding somebody's half-typed line submits it. `send`
// refuses to append to that composer for exactly this reason, and this must not
// become the way around it.
func TestKeysRefusesWhenTheComposerHoldsUnsentText(t *testing.T) {
	f := dialogMux()
	f.captures["%1"] = fixtureUnsent
	d := newTestDriver(f)
	// composerDigestOf, not digestOf — this is exactly the value a real
	// caller reads off GET while the composer holds text (colab-fleet#127:
	// keys used to reject this and only accept the whole-screen digest).
	want := composerDigestOf(t, d, "alpha💬")

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, want)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s); unsent text must not be submittable by a raw key",
			got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "unsent text") {
		t.Errorf("reason = %q; it must name what is in the way", got.Reason)
	}
}

// colab-fleet#134: a composer taller than this driver's capture window has
// no composer-scope digest to corroborate (composerText returns "" for it,
// same as an absent composer) — so this falls into the SCREEN-scope digest
// branch, same as TestKeysDeliversToAnUnrecognisedDialogAndConfirmsTheRedraw,
// and the caller must quote back digestOf (ScreenDigest), not a composer
// digest that was never published. It must still refuse the key itself:
// a clipped composer is unsent text this driver could not read, not text
// that was proven absent.
func TestKeysRefusesOnAClippedComposer(t *testing.T) {
	f := dialogMux()
	f.captures["%1"] = clippedComposerFixture()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬") // screen scope: no composer digest exists for a clipped read

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, want)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s); a clipped composer must not be pressed into blind", got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "capture window") {
		t.Errorf("reason = %q; it must say why this driver could not corroborate the composer", got.Reason)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			t.Errorf("a clipped composer must never be pressed against; saw %v", call)
		}
	}
}

// The bug this whole change exists for: GET's ScreenDigest and ComposerDigest
// are two different values whenever the composer holds text. A caller that
// quotes the screen-scope digest back at a composer-holds-text session must
// be refused for a corroboration mismatch that NAMES composerDigest — never
// silently accepted, and never told to supply "screenDigest" (colab-fleet#127).
func TestKeysNamesComposerDigestWhenComposerHoldsText(t *testing.T) {
	f := dialogMux()
	f.captures["%1"] = fixtureUnsent
	d := newTestDriver(f)
	screenScope := digestOf(t, d, "alpha💬") // the whole-screen value, wrong scope here

	_, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, screenScope)
	if err == nil {
		t.Fatal("the whole-screen digest must not corroborate a composer-scope check")
	}
	if !strings.Contains(err.Error(), "composerDigest") {
		t.Errorf("error = %v; it must name composerDigest as the field to use here", err)
	}
	if !strings.Contains(err.Error(), "composer changed") {
		t.Errorf("error = %v; it must say what changed (the composer, not the screen)", err)
	}
}

// A recognised prompt has a better answer available: respond verifies a nonce
// and can name the option it chose. Falling back to a blind arrow key would
// trade all of that away silently.
func TestKeysRefusesWhenRespondCouldAnswerInstead(t *testing.T) {
	f := dialogMux()
	f.captures["%1"] = fixtureMenu
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyDown, want)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s); a recognised prompt belongs to respond",
			got.Outcome, got.Reason)
	}
	if !strings.Contains(got.Reason, "respond") {
		t.Errorf("reason = %q; it must point at the operation that can do better", got.Reason)
	}
}

// A key this driver was never taught must never reach the multiplexer, whose
// key vocabulary is far larger than this API's.
func TestKeysRefusesAKeyOutsideTheVocabulary(t *testing.T) {
	f := dialogMux()
	d := newTestDriver(f)

	_, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyName("C-c"), "whatever")
	if err == nil {
		t.Fatal("an unmapped key name must not be forwarded to the substrate")
	}
	for _, call := range f.callsSnapshot() {
		for _, a := range call {
			if a == "C-c" {
				t.Fatal("the unmapped key reached send-keys anyway")
			}
		}
	}
}

// Enter is sent as C-m. Measured elsewhere in this driver: a prompt that
// swallows Enter leaves the session blocked, and C-m is what lands.
func TestKeysSendsEnterAsControlM(t *testing.T) {
	f := dialogMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	if _, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEnter, want); err != nil {
		t.Fatalf("Keys: %v", err)
	}
	for _, call := range f.callsSnapshot() {
		if len(call) > 0 && call[0] == "send-keys" {
			for _, a := range call {
				if a == "Enter" {
					t.Error("Enter was sent literally; C-m is the one measured to land")
				}
				if a == "C-m" {
					return
				}
			}
		}
	}
	t.Error("no key was sent at all")
}
