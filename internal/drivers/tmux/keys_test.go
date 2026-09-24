package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestKeysEscapeSkipsTheComposerLockEvenWhileHeld is the review's regression
// test for D4's lock ignoring the caller's deadline (terminalpath2_lock.go):
// Escape is the key a caller reaches for to get OUT of a stuck dialog, and
// making it wait behind another call's confirmLandedV2/confirmSubmittedV2
// poll (up to ~2*submitConfirmWindow) would refuse the escape hatch for
// exactly the situation it exists to escape. It must proceed even while this
// session's composer lock is held by another, still-running call.
func TestKeysEscapeSkipsTheComposerLockEvenWhileHeld(t *testing.T) {
	f := dialogMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	unlock, ok := d.lockComposerOpsCtx(context.Background(), "alpha💬")
	if !ok {
		t.Fatal("setup: could not acquire the composer lock")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got, err := d.Keys(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyEscape, want)
	if err != nil {
		t.Fatalf("Keys(Escape): %v", err)
	}
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("outcome = %q (%s), want submitted — Escape must not wait on a composer lock "+
			"another call is holding", got.Outcome, got.Reason)
	}
}

// TestKeysNonEscapeRefusesFastWhenTheComposerLockIsHeld is the positive
// counterpart: every OTHER key still goes through the lock (D4's own
// serialisation is unchanged for them), and a caller whose deadline runs out
// waiting for it gets a fast, honest refusal rather than blocking past its
// own declared patience.
func TestKeysNonEscapeRefusesFastWhenTheComposerLockIsHeld(t *testing.T) {
	f := dialogMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	unlock, ok := d.lockComposerOpsCtx(context.Background(), "alpha💬")
	if !ok {
		t.Fatal("setup: could not acquire the composer lock")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := d.Keys(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyDown, want)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Keys(Down): %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s), want refused — the composer lock is held by another call", got.Outcome, got.Reason)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Keys(Down) took %s to refuse — the caller's own short deadline must not be held "+
			"hostage by an unrelated call holding this session's composer lock", elapsed)
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
	assertClippedRemedy(t, got.Reason)
	if n := d.Counters()[counterComposerClippedRefusedKeys]; n != 1 {
		t.Errorf("%s = %d, want 1", counterComposerClippedRefusedKeys, n)
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

// fixtureUncorroboratedPrompt is a structural prompt with no footer, no chrome
// and no kind — #58's hold applies to it, so a read withholds it until the
// same screen has been seen unchanged past spinnerPaintGrace.
const fixtureUncorroboratedPrompt = "  A question nobody has written a matcher for\n" +
	"❯ 1. Do the risky thing\n" +
	"  2. Do the safe thing"

// #159 ask 1: keys and the state read consult one classification. Reads come
// every 700ms — faster than spinnerPaintGrace, the cadence a polling client
// produces — against a screen that never changes. On every read the keys
// gate must refuse exactly when that read handed out a prompt, and once a
// prompt has been handed out an unchanged screen must never withdraw it.
//
// Before the fix both failed: keys asked parsePrompt directly and refused
// while the read published no prompt, and the hold measured "unchanged" from
// the previous READ, so at this cadence an unrecognised prompt was never
// corroborated at all (and at a mixed cadence it flapped).
func TestKeysAndStateReadAgreeOnEveryReadOfOneScreen(t *testing.T) {
	cases := []struct {
		name         string
		screen       string
		promptAtOnce bool
	}{
		{"review screen", fixtureReviewScreen, true},
		{"uncorroborated structural prompt", fixtureUncorroboratedPrompt, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := dialogMux()
			f.captures["%1"] = tc.screen
			clock := time.Unix(1785760000, 0)
			d := New("testbox",
				withExec(f.exec),
				withNonce(func() string { return testNonce }),
				withClock(func() time.Time { return clock }),
			)
			ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}
			ctx := context.Background()

			sawPrompt := false
			nonce := ""
			for i := 0; i < 10; i++ {
				st, err := d.State(ctx, testCaller, ref)
				if err != nil {
					t.Fatalf("read %d: State: %v", i, err)
				}
				switch {
				case st.Prompt != nil:
					if nonce != "" && st.Prompt.Nonce != nonce {
						t.Fatalf("read %d: nonce %s, want the stable %s", i, st.Prompt.Nonce, nonce)
					}
					nonce, sawPrompt = st.Prompt.Nonce, true
				case sawPrompt:
					t.Fatalf("read %d: status %s with no prompt, on the same screen an earlier "+
						"read reported as a prompt (%s)", i, st.Status, st.Evidence)
				}
				if i == 0 && (st.Prompt != nil) != tc.promptAtOnce {
					t.Errorf("first read: prompt present = %v, want %v (%s)",
						st.Prompt != nil, tc.promptAtOnce, st.Evidence)
				}

				got, err := d.Keys(ctx, testCaller, ref, fleet.KeyDown, st.ScreenDigest)
				if err != nil {
					t.Fatalf("read %d: Keys: %v", i, err)
				}
				refusedForPrompt := got.Outcome == fleet.OutcomeRefused && strings.Contains(got.Reason, "respond")
				if refusedForPrompt != (st.Prompt != nil) {
					t.Errorf("read %d: the read published prompt=%v but keys answered %s (%s) — "+
						"one screen, two classifications", i, st.Prompt != nil, got.Outcome, got.Reason)
				}

				// A delivered key repaints the fake dialog; put the unchanged
				// screen back so every read in this loop sees one screen.
				f.mu.Lock()
				f.captures["%1"] = tc.screen
				f.mu.Unlock()
				clock = clock.Add(700 * time.Millisecond)
			}
			if !sawPrompt {
				t.Error("an unchanged prompt read every 700ms for 7s was never reported; " +
					"how long a screen has held must not depend on how often it is read")
			}
		})
	}
}

// idleMux is a session at an idle, EMPTY composer with no dialog on screen —
// the state BTab exists for, and the one the arrow-key guard (#180 L7) refuses
// every move key in. keyRepaint models the runtime rewriting its mode indicator
// under the key, which is what a real Shift+Tab does to the footer.
func idleMux() *fakeMux {
	f := twoSessions()
	f.keyRepaint = map[string]bool{"%1": true}
	return f
}

// sendKeyCalls returns the key names of every send-keys the driver made, in
// order, so an assertion is about what reached the multiplexer and not about
// what the driver said it did.
func sendKeyCalls(f *fakeMux) [][]string {
	var out [][]string
	for _, c := range f.callsSnapshot() {
		if len(c) > 0 && c[0] == "send-keys" {
			out = append(out, sentKeys(c))
		}
	}
	return out
}

// #188: BTab is delivered to an idle, empty composer, spelled as the
// multiplexer spells it, and confirmed by the footer repainting. This is the
// test that separates BTab from the arrows: TestKeysRefusesArrowsOnAnEmptyComposer
// refuses every one of them on this exact screen, and BTab's whole use is here.
func TestKeysDeliversBTabToAnIdleEmptyComposer(t *testing.T) {
	f := idleMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyBTab, want)
	if err != nil {
		t.Fatalf("Keys(BTab): %v", err)
	}
	if got.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("outcome = %q (%s); BTab on an idle composer must be delivered and "+
			"confirmed by the repaint — the arrow-key guard must not catch it", got.Outcome, got.Reason)
	}
	sent := sendKeyCalls(f)
	if len(sent) != 1 || len(sent[0]) != 1 || sent[0][0] != "BTab" {
		t.Errorf("multiplexer saw %v; want exactly one send-keys of the literal name BTab", sent)
	}
}

// The counterpart to the test above, so the pair proves the guard is keyed on
// the KEY and not simply switched off for this screen: an arrow on the very
// same idle composer is still refused, and a BTab is still delivered.
func TestKeysBTabIsExemptFromTheArrowGuardAndArrowsAreNot(t *testing.T) {
	f := idleMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	arrow, err := d.Keys(context.Background(), testCaller, ref, fleet.KeyLeft, want)
	if err != nil {
		t.Fatalf("Keys(Left): %v", err)
	}
	if arrow.Outcome != fleet.OutcomeRefused || !strings.Contains(arrow.Reason, "arrow") {
		t.Fatalf("Left on an idle composer = %s (%s); the arrow guard must still hold", arrow.Outcome, arrow.Reason)
	}
	if n := len(sendKeyCalls(f)); n != 0 {
		t.Fatalf("a refused arrow still reached the multiplexer (%d send-keys)", n)
	}

	btab, err := d.Keys(context.Background(), testCaller, ref, fleet.KeyBTab, want)
	if err != nil {
		t.Fatalf("Keys(BTab): %v", err)
	}
	if btab.Outcome != fleet.OutcomeSubmitted {
		t.Fatalf("BTab on the same screen = %s (%s); want submitted", btab.Outcome, btab.Reason)
	}
}

// Every refusal that protects the other keys protects BTab too. Each case is a
// screen on which a key must not be delivered, and each is checked against the
// multiplexer's own call log — a driver that said "refused" and pressed anyway
// would pass a test that only read the receipt.
func TestKeysBTabIsRefusedWhereEveryOtherKeyIs(t *testing.T) {
	cases := []struct {
		name    string
		screen  string
		digest  func(*testing.T, *Driver) string
		mention string
	}{
		{"composer holds unsent text", fixtureUnsent,
			func(t *testing.T, d *Driver) string { return composerDigestOf(t, d, "alpha💬") }, "unsent text"},
		{"a prompt respond can answer", fixtureMenu,
			func(t *testing.T, d *Driver) string { return digestOf(t, d, "alpha💬") }, "respond"},
		{"a composer taller than the capture window", clippedComposerFixture(),
			func(t *testing.T, d *Driver) string { return digestOf(t, d, "alpha💬") }, "capture window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := idleMux()
			f.captures["%1"] = tc.screen
			d := newTestDriver(f)
			want := tc.digest(t, d)

			got, err := d.Keys(context.Background(), testCaller,
				fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyBTab, want)
			if err != nil {
				t.Fatalf("Keys(BTab): %v", err)
			}
			if got.Outcome != fleet.OutcomeRefused || !strings.Contains(got.Reason, tc.mention) {
				t.Fatalf("outcome = %s (%s); want refused naming %q", got.Outcome, got.Reason, tc.mention)
			}
			if sent := sendKeyCalls(f); len(sent) != 0 {
				t.Errorf("a refused BTab reached the multiplexer: %v", sent)
			}
		})
	}
}

// BTab changes what an unattended agent may do, so a caller who has not read
// the screen — or read a screen that has since moved — must not be able to
// press it. Both are refusals with no key sent.
func TestKeysBTabNeedsACurrentDigest(t *testing.T) {
	f := idleMux()
	d := newTestDriver(f)
	ref := fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}

	if _, err := d.Keys(context.Background(), testCaller, ref, fleet.KeyBTab, ""); err == nil {
		t.Error("BTab with no digest at all must be refused")
	}
	if _, err := d.Keys(context.Background(), testCaller, ref, fleet.KeyBTab, "a-digest-from-some-other-screen"); err == nil {
		t.Error("BTab against a digest from another screen must be refused")
	}
	if sent := sendKeyCalls(f); len(sent) != 0 {
		t.Errorf("an uncorroborated BTab reached the multiplexer: %v", sent)
	}
}

// A footer that did not repaint is reported unknown, never submitted, exactly
// as for every other key. For BTab this is the sentence a mode-changing client
// most needs to be true: `submitted` means the screen changed and nothing more,
// so `unknown` must stay available for the press the runtime swallowed.
func TestKeysBTabReportsUnknownWhenTheScreenDoesNotMove(t *testing.T) {
	f := idleMux()
	f.keyRepaint = nil
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	got, err := d.Keys(context.Background(), testCaller,
		fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyBTab, want)
	if err != nil {
		t.Fatalf("Keys(BTab): %v", err)
	}
	if got.Outcome != fleet.OutcomeUnknown {
		t.Errorf("outcome = %q (%s); an unchanged screen confirms nothing", got.Outcome, got.Reason)
	}
}

// BTab is NOT Escape: it is not the escape hatch, so it queues behind the
// composer lock like every other key and refuses fast when the caller's
// deadline runs out, instead of being waved through because it sits next to a
// key that is. (The Escape exemption exists for one reason, stated in Keys.)
func TestKeysBTabWaitsBehindTheComposerLockLikeAnyOtherKey(t *testing.T) {
	f := idleMux()
	d := newTestDriver(f)
	want := digestOf(t, d, "alpha💬")

	unlock, ok := d.lockComposerOpsCtx(context.Background(), "alpha💬")
	if !ok {
		t.Fatal("setup: could not acquire the composer lock")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, err := d.Keys(ctx, testCaller, fleet.SessionRef{Machine: "testbox", ID: "alpha💬"}, fleet.KeyBTab, want)
	if err != nil {
		t.Fatalf("Keys(BTab): %v", err)
	}
	if got.Outcome != fleet.OutcomeRefused {
		t.Fatalf("outcome = %q (%s); BTab must not skip the lock the way Escape does", got.Outcome, got.Reason)
	}
	if sent := sendKeyCalls(f); len(sent) != 0 {
		t.Errorf("a BTab that lost the lock still reached the multiplexer: %v", sent)
	}
}
