package tmux

import (
	"context"
	"fmt"
	"strconv"

	fleet "github.com/godx-jp/colab-fleet"
)

// answerPreview answers a question whose options are drawn beside a preview
// pane (colab-fleet#204). It moves the highlight onto the chosen option, reads
// the screen to prove the highlight is there, and only then presses Enter.
//
// # What was measured (one runtime build, live, disposable session)
//
// On a dialog with a preview pane a digit does NOT commit an answer, which is
// what it does on every numbered menu without one (#168). It moves the
// highlight — and with it the pane — and the tab bar stays ☐. Enter then
// commits the HIGHLIGHTED row and advances: to the next question, to the
// review screen after the last, or, on a single-question dialog, out of the
// dialog altogether.
//
// Sent together, the two keys go wrong the same way #168 measured on the other
// dialog, and worse for what it looks like. `2` and Enter in one send-keys
// recorded the second question's DEFAULT, not option 2: the highlight had not
// repainted when Enter arrived, so Enter confirmed the row it was still on. The
// answer is wrong, the dialog advances, and the nonce changed — the receipt
// would have read submitted. A burst of arrows is no better: `Down Down Down`
// moved one row, `Up` four times moved one. Only one key per send-keys is
// reliable, so every key here is its own call and each is read back before the
// next is sent.
//
// # Why the digit and not arrows
//
// It is one key rather than up to three (a question has at most four options),
// and there is no free-text row in this layout for a digit to be typed into —
// the mode #176 found a digit typed into a text field. The read-back is what
// makes that a measurement and not a hope: if the highlight does not arrive,
// nothing is confirmed.
//
// # What the receipt means
//
// submitted is reported only when the prompt that was answered is no longer on
// screen. Moving on to the next tab counts as landed, as in #168: the header's
// ☐ becomes ☒ and the question changes, so the nonce does too.
func (d *Driver) answerPreview(ctx context.Context, paneID string, before *fleet.SessionPrompt, shape menuShape, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	if before.Selected < 1 || before.Selected > len(before.Options) {
		// Off the list the highlight is on a row of its own — the chat row
		// below it — and a digit sent from there may be typed into a field
		// instead of moving anything (#176 measured that on a free-text row).
		// That is true of a choice as much as of accepting the highlighted
		// option, so neither is sent from here.
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason: "the highlight is not on one of this question's options (it is on a row " +
				"below the list), so a digit sent from there could be typed into a field " +
				"instead of moving the highlight, and there is no highlighted option to " +
				"accept. Move the highlight back onto the list with keys, then answer",
		}, nil
	}
	choice := resp.Choice
	accepting := choice == 0
	if accepting {
		choice = before.Selected
	}
	switch {
	case choice < 1 || choice > len(before.Options):
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: "no such option on this prompt"}, nil
	case choice > 9:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  "a digit reaches options 1-9 only; this question has more",
		}, nil
	}

	label := before.Options[choice-1]
	verb := "chose option " + strconv.Itoa(choice)
	if accepting {
		verb = "accepted the highlighted option " + strconv.Itoa(choice)
	}
	answered := verb + " (" + label + ")" + tabWhere(shape.tab)
	if resp.Nonce == "" {
		answered += "; answered without a nonce, so nothing verified the prompt " +
			"had not changed since it was read"
	}

	if choice != before.Selected {
		if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, strconv.Itoa(choice)); err != nil {
			return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
		}
		verdict, _ := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
			switch {
			case now == nil || now.Nonce != before.Nonce:
				return promptDiverged
			case now.Selected == choice:
				return promptArrived
			}
			return promptPending
		})
		switch verdict {
		case promptDiverged:
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeUnknown,
				Reason: answered + "; the prompt changed right after the digit was pressed, " +
					"before the highlight was read back on the chosen row. No confirm key was " +
					"sent, so nothing further was answered. Read the state again",
			}, nil
		case promptPending:
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeUnknown,
				Reason: answered + "; the digit did not move the highlight onto the chosen " +
					"row, and pressing Enter then would confirm whichever row it is on. No " +
					"confirm key was sent, so the question is still up and unanswered. Read " +
					"the state again",
			}, nil
		}
	}

	// Enter alone. Never a leading Space (the accept-highlighted path of a
	// plain menu sends one, measured there to be swallowed in place of a first
	// Enter): what a Space does on this layout has not been measured, and it is
	// the one key that could answer a question here by itself.
	if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, "C-m"); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
	}
	if d.promptCleared(ctx, paneID, before.Nonce) {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeSubmitted, Reason: answered}, nil
	}
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeUnknown,
		Reason: answered + "; the highlight was on the chosen row and Enter was pressed, but " +
			"the prompt is still on screen, so the keypress may not have registered",
	}, nil
}

// tabWhere names the question of a tabbed dialog a receipt is about, or
// nothing when the position could not be read or there is only the one
// question to be about.
func tabWhere(t tabPosition) string {
	if t.At < 1 || t.Of < 2 {
		return ""
	}
	return " on question " + strconv.Itoa(t.At) + " of " + strconv.Itoa(t.Of)
}
