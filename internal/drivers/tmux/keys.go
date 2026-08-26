package tmux

import (
	"context"
	"fmt"
	"strings"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Raw key delivery for the dialogs `respond` cannot see (driver.KeySender).
//
// # The gap this fills, and the one it must not open
//
// `respond` answers a prompt the classifier RECOGNISED, by index, and refuses
// when it sees none — which is the state every full-screen dialog this driver
// does not parse leaves a session in. `send` refuses to produce a keystroke at
// all, by design. So a screen navigated with arrow keys was unreachable through
// this API, and the supervisor that met one kept a direct handle on the
// multiplexer in order to have any move at all.
//
// The danger is the mirror image of the gap. This delivers a keypress to a
// screen NOBODY CLASSIFIED, so the protections the neighbouring operations lean
// on are all absent: no prompt to check for, no nonce to compare, no option
// text to name in the receipt. Everything below is what replaces them, and each
// one is load-bearing rather than defensive:
//
//   - the caller quotes back a digest of what it read — the composer's text
//     when the composer holds unsent text, the whole screen otherwise,
//     because GET publishes exactly those two fields for exactly those two
//     cases (ComposerDigest, ScreenDigest) — and a mismatch is a refusal
//     (§5.4 — a proxy for identity is not identity, arriving here for the
//     fourth time; colab-fleet#127: this used to hash the whole screen
//     unconditionally, which a caller quoting ComposerDigest back could
//     never satisfy);
//   - a composer holding unsent text is refused outright, because `Enter` there
//     submits a human's half-typed message and `send` already refuses to touch
//     that composer for exactly this reason;
//   - a session at a prompt the classifier DID recognise is refused, because
//     `respond` can answer it with a nonce and name the option it chose, and
//     falling back to a blind arrow key would be a downgrade dressed as a
//     capability;
//   - and the delivery is confirmed by re-reading, because a key a dialog
//     swallows leaves the session exactly as stuck as before.
const (
	// keyConfirmWindow bounds how long to keep looking for the repaint that
	// says the key registered, and keyConfirmInterval how often. A dialog
	// redraws far faster than a turn produces output — this is a repaint, not
	// a round trip to anything — so the window is short and the usual answer
	// arrives on the first read.
	keyConfirmWindow   = 1 * time.Second
	keyConfirmInterval = 200 * time.Millisecond
)

// tmuxKey maps this API's closed vocabulary onto the multiplexer's key names.
//
// A map rather than passing the string through, so that the wire vocabulary and
// the substrate's vocabulary are separable and neither can quietly become the
// other. `Enter` is sent as C-m for the reason measured elsewhere in this
// driver: a prompt that swallows Enter leaves the session blocked, and C-m is
// what actually lands.
var tmuxKey = map[fleet.KeyName]string{
	fleet.KeyUp:     "Up",
	fleet.KeyDown:   "Down",
	fleet.KeyLeft:   "Left",
	fleet.KeyRight:  "Right",
	fleet.KeyEnter:  "C-m",
	fleet.KeyEscape: "Escape",
}

// Keys delivers one raw key event to a session's screen (driver.KeySender).
func (d *Driver) Keys(ctx context.Context, req fleet.Request, ref fleet.SessionRef, key fleet.KeyName, expectDigest string) (fleet.DeliveryReceipt, error) {
	send, ok := tmuxKey[key]
	if !ok {
		// Unreachable through the HTTP surface, which validates first. Kept
		// because a driver must not send a key it was never taught: an
		// unmapped name reaching send-keys would be interpreted by the
		// multiplexer, which has a far larger vocabulary than this API does.
		return fleet.DeliveryReceipt{}, fmt.Errorf("keys: %q is not a key this driver delivers", key)
	}

	ctx, cancel := d.bounded(ctx)
	defer cancel()

	rows, captures, err := d.enumerate(ctx)
	if err != nil {
		return fleet.DeliveryReceipt{}, err
	}
	var live *paneRow
	for i := range rows {
		if rows[i].session == ref.ID {
			live = &rows[i]
			break
		}
	}
	if live == nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("%w: %q", fleet.ErrNoSuchSession, ref.ID)
	}
	if want := req.Expect.StartedAt; want != nil && !live.created.Equal(*want) {
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"%w: id %q now holds a session started at %s; the caller meant the one started at %s",
			ErrAmbiguousTarget, ref.ID, live.created.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if live.dead {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "session process has exited"}, nil
	}

	text, captured := captures[live.paneID]
	if !captured {
		// Not a refusal and not a success: this driver could not read the
		// screen, which is a statement about the driver rather than about the
		// session (§5.7). Pressing a key against a screen nobody could read is
		// the blind delivery this whole operation is arranged to prevent.
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"keys: could not capture this session's screen, so nothing can be corroborated")
	}

	// Which digest this call must corroborate against depends on what the
	// screen holds RIGHT NOW, decided before the expectDigest check so the
	// error messages below can already name the right field.
	//
	// composer holds unsent text -> composer scope, screenDigest(pending).
	// This is the SAME value GET publishes as ComposerDigest (classify.go),
	// and the SAME value Discard corroborates against (tmux.go) — chosen
	// because every key this driver could deliver into that state is refused
	// below regardless of which key was asked for, so the corroboration only
	// ever has to prove the caller saw the composer, never the rest of the
	// screen (colab-fleet#127).
	//
	// composer empty (or absent) -> screen scope, screenDigest(text), same as
	// before this change. A composer-scope digest here would be the constant
	// hash of "" no matter what a dialog says, which corroborates nothing —
	// exactly the relaxation this driver must not make. GET publishes this as
	// ScreenDigest, unconditionally.
	screen := newScreen(text)
	pending, _ := composerText(screen)
	composerHoldsText := strings.TrimSpace(pending) != ""

	if expectDigest == "" {
		if composerHoldsText {
			return fleet.DeliveryReceipt{}, fmt.Errorf(
				"%w: refusing to press %s on a composer the caller has not read; "+
					"supply the composerDigest from a read as ?expect=<composerDigest> "+
					"(the same field discard uses)", ErrAmbiguousTarget, key)
		}
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"%w: refusing to press %s on a screen the caller has not read; supply "+
				"the screenDigest from a read as ?expect=<screenDigest> (a query "+
				"parameter, where startedAt goes)", ErrAmbiguousTarget, key)
	}

	var before string
	if composerHoldsText {
		before = screenDigest(pending)
	} else {
		before = screenDigest(text)
	}
	if before != expectDigest {
		if composerHoldsText {
			return fleet.DeliveryReceipt{}, fmt.Errorf(
				"%w: the composer changed since the caller read it (expected "+
					"composerDigest %s, found %s); a key sent now would be acting on "+
					"unsent text nobody has re-read", ErrAmbiguousTarget, expectDigest, before)
		}
		return fleet.DeliveryReceipt{}, fmt.Errorf(
			"%w: the screen changed since the caller read it (expected screenDigest %s, "+
				"found %s); a key sent by position now would be applied to a "+
				"different screen", ErrAmbiguousTarget, expectDigest, before)
	}

	// A recognised prompt has a better answer than this one. `respond` checks a
	// nonce, chooses by index, and names the option it took in the receipt;
	// arrow keys can do none of that, so accepting the fallback here would let
	// a caller silently trade all three away.
	if p := parsePrompt(screen); p != nil {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "this session is at a prompt the driver recognises; answer it " +
				"through respond, which verifies a nonce and can say which option " +
				"it chose",
		}, nil
	}

	// Unsent text in the composer. `Enter` submits it, and it is not this
	// caller's to submit — the runtime redraws a composer identically whether a
	// human typed into it or a delivery stranded text there, so the only safe
	// reading is that somebody meant it.
	if composerHoldsText {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "the composer holds unsent text; a key delivered now could submit " +
				"something nobody asked to send. Clear it with discard, or send it",
		}, nil
	}

	if _, err := d.run(ctx, d.bin, "send-keys", "-t", live.paneID, send); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("keys: %w", err)
	}

	// Confirm by looking. A key that landed on a dialog changes it; one the
	// dialog swallowed does not. Reporting the second as submitted is how a
	// supervisor concludes it has moved a selection it has not moved.
	//
	// Read FIRST, then decide whether to wait again — the same order
	// promptCleared uses, and for the same reason: an operation whose deadline
	// has already passed should still make the one observation it came for
	// rather than reporting "could not confirm" without having tried.
	//
	// Re-read through the SAME path the first reading came from. A digest is
	// only a comparison if both sides were produced identically, and this
	// driver has two capture shapes for reasons of their own — comparing
	// across them would report "changed" for a screen that did not, which is
	// the one direction of this confirmation that must never be wrong.
	deadline := d.now().Add(keyConfirmWindow)
	for {
		if _, afterCaptures, err := d.enumerate(ctx); err == nil {
			if afterText, reread := afterCaptures[live.paneID]; reread && screenDigest(afterText) != before {
				return fleet.DeliveryReceipt{
					Outcome: fleet.OutcomeSubmitted,
					Reason:  "sent " + string(key) + "; the screen changed in response",
				}, nil
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(keyConfirmInterval):
			continue
		}
		break
	}

	// Honest rather than convenient. A legitimate no-op — Down at the bottom of
	// a list — reports the same way, because from outside the dialog the two
	// are the same observation, and inventing a distinction here would mean
	// claiming to know what the dialog is.
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason: "sent " + string(key) + "; the screen did not change, so the key was " +
			"either swallowed or had nothing to do",
	}, nil
}
