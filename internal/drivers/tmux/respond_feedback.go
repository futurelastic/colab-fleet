package tmux

import (
	"context"
	"fmt"
	"strconv"
	"time"

	fleet "github.com/godx-jp/colab-fleet"
)

// Answering the runtime's feedback-draft card (colab-fleet #215) — see
// feedbackcard.go for what the card is and why it is a prompt only while it
// hides the composer.
//
// # The key is the card's own, and it goes once, alone
//
// A Choice is a 1-based index into ["review", "send", "dismiss"], and the card
// answers 1, 2 and 0: choice 3 is delivered as the key 0, which is why the keys
// live in menuShape.shortcuts and are not derived from the index. The key goes
// alone and once. Nothing follows it — an Enter here would submit the
// composer — and it is never retried: a second digit spends the caller's answer
// twice, and lands in whatever screen the first one opened.
//
// # What is refused, and why each is a refusal and not a guess
//
//   - Cancel. Escape on an empty composer dismisses the card (the draft stays
//     queued) and is sometimes followed by the runtime asking whether to turn
//     drafts off. A caller that wants the card dismissed says "dismiss".
//   - No choice. The card highlights nothing, so there is no default to accept,
//     and the keys that accept a default would submit the composer.
//   - Choices and Text. They answer multi-select and free-text questions; this
//     is neither.
//   - No nonce. The options are the same on every draft, so the nonce is the
//     only thing that ties an answer to THIS draft. It is required for this
//     kind alone; a nonce-less answer would send feedback, or throw it away, on
//     the strength of a screen nobody has read.
//
// Whether to review, send or dismiss is a person's decision about what leaves
// their machine. This function carries a caller's answer out; it never chooses
// one, and no consent (SessionSpec.Consents) reaches it.
func (d *Driver) answerFeedbackCard(ctx context.Context, paneID string, before *fleet.SessionPrompt, shape menuShape, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	refuse := func(reason string) (fleet.DeliveryReceipt, error) {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: reason}, nil
	}
	switch {
	case resp.Cancel:
		return refuse("cancel is not accepted on the feedback-draft card: Escape on an empty " +
			"composer dismisses the card and is sometimes followed by a question about turning " +
			"drafts off. Answer with choice 3 (dismiss) if that is what is meant")
	case len(resp.Choices) > 0 || resp.Text != nil:
		return refuse("the feedback-draft card is a single-choice notice: answer it with choice " +
			"(1 review, 2 send, 3 dismiss), not choices or text")
	case resp.Choice < 1:
		return refuse("the feedback-draft card highlights nothing, so there is no default to " +
			"accept: answer with choice 1 (review), 2 (send) or 3 (dismiss)")
	case resp.Nonce == "":
		return refuse("the feedback-draft card is answered only with the nonce of the draft that " +
			"was read: its options are the same on every draft, so nothing else ties an answer to " +
			"this one")
	case resp.Choice > len(shape.shortcuts):
		return refuse("no such option on this prompt")
	}
	key := shape.shortcuts[resp.Choice-1]
	verb := before.Options[resp.Choice-1]

	if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, key); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
	}

	answered := "chose option " + strconv.Itoa(resp.Choice) + " (" + verb + ")"
	verdict, after, haveAfter := d.feedbackCardAnswered(ctx, paneID, before.Nonce, key)
	switch verdict {
	case feedbackCardLeft:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeSubmitted,
			Reason:  answered + "; " + feedbackNext(key, after, haveAfter),
		}, nil
	case feedbackCardKeyTyped:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: answered + ", but the key was typed into the composer instead of reaching the " +
				"card, and the composer still holds it. Discard it before sending anything else",
		}, nil
	default:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: answered + "; the card is still on screen, so the keypress may not have " +
				"registered. It was not sent again",
		}, nil
	}
}

type feedbackCardVerdict int

const (
	// feedbackCardLeft: the card with this nonce is no longer on screen. The
	// session moved on — the card was replaced by the runtime's next screen, or
	// dismissed — which is the answer having its effect.
	feedbackCardLeft feedbackCardVerdict = iota
	// feedbackCardKeyTyped: the same card is still there and the key sits in
	// the composer row: it never reached the card's own handler.
	feedbackCardKeyTyped
	// feedbackCardStayed: the same card is still there and the row is empty.
	feedbackCardStayed
)

// feedbackCardAnswered waits briefly for the answered card to leave the screen.
//
// Read first, then decide whether to wait again — an operation whose deadline
// has passed still makes the one observation it came for. A digit that sits in
// the composer row is judged only at the END of the window: the runtime clears
// it as it acts on it, so seeing it mid-window says the key was received, not
// that it was lost.
//
// It also returns the screen it saw the card leave on, so the receipt can say
// what the session is showing now instead of what the key is expected to do.
func (d *Driver) feedbackCardAnswered(ctx context.Context, paneID, nonce, key string) (feedbackCardVerdict, screen, bool) {
	deadline := d.now().Add(promptClearWindow)
	last := feedbackCardStayed
	for {
		if sc, ok := d.captureForClassify(ctx, paneID); ok {
			card, found := liveFeedbackCard(sc)
			if !found || card.nonce() != nonce {
				return feedbackCardLeft, sc, true
			}
			last = feedbackCardStayed
			if card.rowText == key {
				last = feedbackCardKeyTyped
			}
		}
		if d.now().After(deadline) || ctx.Err() != nil {
			return last, screen{}, false
		}
		select {
		case <-ctx.Done():
			return last, screen{}, false
		case <-time.After(promptClearInterval):
		}
	}
}

// feedbackNext says what the session shows after the card was answered: read
// from the screen when one was read, and otherwise what the key is expected to
// have done, worded as an expectation. Every state named here is one a person
// answers at the terminal; none is answerable through respond (ADR 217).
func feedbackNext(key string, sc screen, have bool) string {
	if have {
		if _, ok := liveFeedbackPanel(sc); ok {
			return "the runtime opened its feedback panel over the composer, where a person reviews the " +
				"draft; nothing has been sent. Escape, through keys, closes the panel"
		}
		if c, ok := liveFeedbackCard(sc); ok {
			switch c.state {
			case feedbackConfirm:
				return "the runtime now asks to confirm before sending — nothing has been sent, and " +
					"the confirmation (2 to send · Esc to back) is a person's to give"
			case feedbackSending:
				return "the runtime is sending the draft"
			case feedbackError:
				return "the runtime could not send the draft; it stays queued, and the card offers to " +
					"review and retry or to dismiss"
			}
		}
		if liveFeedbackQuestion(sc) {
			return "the card is dismissed (the draft stays queued, and /feedback still lists it) and " +
				"the runtime now asks whether to turn drafts off — a person's to answer"
		}
	}
	switch key {
	case "1":
		return "the runtime opens the draft for review, which this driver did not read back"
	case "2":
		return "the runtime should now ask to confirm before sending — nothing has been sent unless " +
			"the runtime skipped its own confirmation, which this driver did not read back"
	}
	return "the card is dismissed and the draft stays queued (/feedback still lists it); the runtime " +
		"sometimes follows a dismissal with a question about turning drafts off, which is left for a person"
}
