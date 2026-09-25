package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	fleet "github.com/godx-jp/colab-fleet"
)

// answerFreeText answers a single-select question in the caller's own words
// (colab-fleet#206): it puts the highlight on the runtime's free-text row,
// types the text into it, reads the row back, and only then confirms. A
// multi-select question takes the same text through answerMultiSelect, which
// owns the boxes around it.
//
// # What was measured (one runtime build, live, disposable session)
//
// On a numbered menu — a single question, or one tab of a multi-question
// dialog — the free-text row behaves differently from every other row:
//
//   - a digit naming it moves the highlight there and COMMITS NOTHING (a digit
//     on any other row commits, #168). The footer gains an "edit in an
//     external editor" hint, which is the field taking focus;
//   - text pasted in lands INLINE: the row's label becomes the text
//     (`❯ 3. <text>`), an embedded newline stays in the field as a continuation
//     row, and a long line wraps under the label. A leading `!` or `/` is
//     plain text there — it reached the agent as the answer, unexecuted;
//   - Enter then commits it, and advances exactly as a digit does — to the next
//     tab, to the review screen after the last, or out of a single-question
//     dialog;
//   - Enter on the row while it is EMPTY does not answer with nothing: the
//     runtime treated it as declining the whole dialog, every question in it.
//
// # What follows from that
//
//   - The text is read back off the row before the confirm is ever sent. That
//     is the only thing standing between an empty field and a declined
//     dialog, so a paste that did not land (or landed somewhere else) confirms
//     nothing.
//   - A digit typed while the field already has focus is typed INTO it. So the
//     digit is sent only when the highlight is elsewhere; if it is already on
//     the row, the text goes straight in.
//   - Typing changes the row's label, and the nonce digests the options, so
//     the nonce changes on every keystroke's repaint. Nothing after the first
//     read can be judged by "same nonce"; it is judged by "same question, this
//     row aside" (sameQuestionAround), and "did the confirm land" is judged
//     against the nonce read AFTER the text was in, never the caller's.
//   - Every key is its own send-keys call and is read back before the next,
//     for the reason gotchas.d/204 records.
//
// # What the receipt means
//
// submitted is reported only when the question that was answered is no longer
// on screen. It never carries the text back: a receipt is not a place to
// copy an answer to.
func (d *Driver) answerFreeText(ctx context.Context, target *paneRow, before *fleet.SessionPrompt, resp fleet.Response) (fleet.DeliveryReceipt, error) {
	text, refusal := freeTextPayload(resp)
	if refusal != "" {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: refusal}, nil
	}
	if !before.FreeText {
		return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: notFreeTextReason(before)}, nil
	}
	idx := freeTextRow(before)
	paneID := target.paneID
	nonceNote := freeTextNonceNote(resp)

	// The highlight goes onto the row first — unless it is already there, when
	// a digit would be typed into the field as text.
	cur := before
	if cur.Selected != idx {
		if idx > 9 {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeRefused,
				Reason:  "a digit reaches options 1-9 only, and the free-text row of this question is option " + strconv.Itoa(idx),
			}, nil
		}
		if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, strconv.Itoa(idx)); err != nil {
			return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
		}
		pre := cur
		verdict, now := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
			switch {
			case now == nil:
				return promptPending
			case !sameQuestionAround(pre, now, idx):
				return promptDiverged
			case now.Selected == idx:
				return promptArrived
			}
			return promptPending
		})
		if verdict != promptArrived {
			return fleet.DeliveryReceipt{
				Outcome: fleet.OutcomeUnknown,
				Reason: "pressed " + strconv.Itoa(idx) + " to put the highlight on the free-text row, and " +
					freeTextNotArrived(verdict) + "; nothing was typed and nothing was confirmed. " +
					"Read the state again before answering" + nonceNote,
			}, nil
		}
		cur = now
	}

	rcpt, typed, refused, err := d.typeIntoFreeTextRow(ctx, target, cur, idx, text, func(now *fleet.SessionPrompt) bool {
		return sameQuestionAround(cur, now, idx)
	})
	if err != nil || refused {
		return rcpt, err
	}
	if typed == nil {
		return rcpt, nil
	}

	if _, err := d.run(ctx, d.bin, "send-keys", "-t", paneID, "C-m"); err != nil {
		return fleet.DeliveryReceipt{}, fmt.Errorf("respond: %w", err)
	}
	was := typed.Nonce
	verdict, next := d.awaitPrompt(ctx, paneID, func(now *fleet.SessionPrompt) promptVerdict {
		if now == nil || now.Nonce != was {
			return promptArrived
		}
		return promptPending
	})

	answered := freeTextTyped(text, idx)
	switch {
	case verdict != promptArrived:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: answered + " and pressed Enter, but the question is still on screen with the text in " +
				"its row, so the keypress may not have registered. Read the state again before " +
				"answering further" + nonceNote,
		}, nil
	case next == nil:
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeSubmitted,
			Reason:  answered + " and confirmed it; the question left the screen with no prompt in its place" + nonceNote,
		}, nil
	case reviewScreenPrompt(next):
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeSubmitted,
			Reason: answered + " and confirmed it, which moved the dialog on to its review screen. The " +
				"answers are NOT handed over yet: answer that screen with choice 1 (Submit answers) and " +
				"its own nonce" + nonceNote,
		}, nil
	}
	return fleet.DeliveryReceipt{
		Outcome: fleet.OutcomeSubmitted,
		Reason:  answered + " and confirmed it, which moved the dialog on to its next question" + nonceNote,
	}, nil
}

// typeIntoFreeTextRow pastes text into the free-text row the highlight is on
// and reads the row back. It is the step the single-select and multi-select
// paths share, and it never confirms anything.
//
// refused is true when the receipt is final (a refusal or an unknown) and the
// caller must return it; otherwise typed is the prompt as read back, with the
// text on its row. same judges "still the question we started on" — the
// caller's own notion of that, because a multi-select question also ignores
// its tick state. A multi-select caller additionally needs the row ticked
// (see rowReady).
func (d *Driver) typeIntoFreeTextRow(ctx context.Context, target *paneRow, cur *fleet.SessionPrompt, idx int, text string,
	same func(*fleet.SessionPrompt) bool) (rcpt fleet.DeliveryReceipt, typed *fleet.SessionPrompt, refused bool, err error) {

	// #180 H2: a pane whose foreground is a shell reads exactly like an
	// empty prompt under a frame the runtime left behind, and text pasted
	// there reaches the shell. Checked before anything is pasted, as send is.
	if ok, why := d.foregroundIsRuntime(ctx, target); !ok {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeRefused,
			Reason:  why + "; nothing was pasted",
		}, nil, true, nil
	}
	if err := d.pasteBracketed(ctx, target.paneID, text); err != nil {
		if errors.Is(err, errBracketPasteUnavailable) {
			return fleet.DeliveryReceipt{Outcome: fleet.OutcomeRefused, Reason: err.Error()}, nil, true, nil
		}
		return fleet.DeliveryReceipt{}, nil, true, fmt.Errorf("respond: %w", err)
	}

	verdict, now := d.awaitPrompt(ctx, target.paneID, func(now *fleet.SessionPrompt) promptVerdict {
		switch {
		case now == nil:
			return promptPending // mid-repaint, or a field taller than the scanned window: read on
		case !same(now):
			return promptDiverged
		case now.Selected == idx && freeTextLanded(now.Options[idx-1], text):
			return promptArrived
		}
		return promptPending
	})
	if verdict != promptArrived {
		return fleet.DeliveryReceipt{
			Outcome: fleet.OutcomeUnknown,
			Reason: "pasted " + strconv.Itoa(len(text)) + " byte(s) into the free-text row (option " +
				strconv.Itoa(idx) + "), and " + freeTextNotArrived(verdict) + " as text on that row; " +
				"nothing was confirmed, because confirming an empty or wrong field can decline the " +
				"whole dialog. The field may still hold the text: a text that wraps over many rows can " +
				"push the question out of the scanned window, and then keys() can reach it",
		}, nil, true, nil
	}
	return fleet.DeliveryReceipt{}, now, false, nil
}

// freeTextPayload turns Response.Text into the exact text to type, or the
// reason it must not be.
//
// The text is put through the same sanitiser send() uses (control bytes and
// the paste-bracket escapes are dropped — the C1 form of CSI included) and
// then trimmed of surrounding whitespace, because a leading blank row would
// leave the option's own row empty and unreadable, and a trailing newline is
// not part of an answer. What is left must not be empty: an empty field
// confirmed declines the whole dialog.
//
// send()'s other refusals — a leading "!" or "/" — are deliberately NOT
// applied. They exist because the composer reads those as its own syntax, and
// the answer field was measured not to (both arrive as plain text; "/var/log"
// is an ordinary answer). The byte cap is applied by the HTTP handler, as it is
// for input.
func freeTextPayload(resp fleet.Response) (text, refusal string) {
	if resp.Text == nil {
		return "", "no text to type"
	}
	text = strings.TrimSpace(sanitizeForBracketedPaste(*resp.Text))
	if text == "" {
		return "", "text is empty once control characters and surrounding whitespace are removed; " +
			"an empty free-text answer is never sent, because the runtime reads it as declining " +
			"the whole dialog"
	}
	return text, ""
}

// notFreeTextReason explains why a prompt cannot be answered through its
// free-text row, for the caller that sent text anyway.
func notFreeTextReason(p *fleet.SessionPrompt) string {
	answerWith := "choice"
	if p.MultiSelect {
		answerWith = "choices"
	}
	reason := "text answers through the free-text row, and this prompt is not recognised as " +
		"offering one (prompt.freeText is not set); answer it with " + answerWith
	if freeTextRow(p) == 0 {
		for _, o := range p.Options {
			if label, _, _ := checkboxLabel(o); isChatLabel(label) {
				// The runtime appends the free-text and chat rows together, so a
				// chat row with no placeholder above it is a free-text row that
				// no longer reads as one.
				return reason + ". It has the runtime's chat row but no free-text placeholder: the " +
					"row already holds text, typed by a person or by an earlier attempt, and nothing " +
					"is typed over it"
			}
		}
	}
	return reason
}

func freeTextNonceNote(resp fleet.Response) string {
	if resp.Nonce == "" {
		return "; answered without a nonce, so nothing verified the prompt had not changed since it was read"
	}
	return ""
}

func freeTextTyped(text string, idx int) string {
	return "typed " + strconv.Itoa(len(text)) + " byte(s) into the free-text row (option " + strconv.Itoa(idx) + ")"
}

func freeTextNotArrived(v promptVerdict) string {
	if v == promptDiverged {
		return "the question on screen changed"
	}
	return "it did not read back"
}

// sameQuestionAround reports whether b is still the question a was, with
// option skip (1-based, the free-text row) set aside because typing into it is
// what changes it. Ticks are set aside too: on a multi-select question they
// are what the caller is changing.
func sameQuestionAround(a, b *fleet.SessionPrompt, skip int) bool {
	if a == nil || b == nil || len(a.Options) != len(b.Options) || a.Question != b.Question {
		return false
	}
	for i := range a.Options {
		if i == skip-1 {
			continue
		}
		la, _, _ := checkboxLabel(a.Options[i])
		lb, _, _ := checkboxLabel(b.Options[i])
		if la != lb {
			return false
		}
	}
	return true
}

// freeTextLanded reports whether the free-text row's option text shows the
// text that was pasted into it.
//
// The row shows the FIRST row of what was typed: an embedded newline continues
// on rows of its own (which carry no number, so the parser does not see them),
// and a long line wraps under the label. So the label must be a prefix of the
// text's first line, whitespace runs folded — and when that first line is
// short enough to certainly fit on one row it must show whole, which is what
// catches a paste that lost its tail. A label that is empty, or still the
// placeholder, is a paste that did not land.
func freeTextLanded(option, text string) bool {
	label, _, _ := checkboxLabel(option)
	fold := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	label = fold(label)
	if label == "" || isFreeTextLabel(label) {
		return false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	first = fold(first)
	if !strings.HasPrefix(first, label) {
		return false
	}
	const fitsOneRow = 40 // runes; well inside any pane a prompt could be read from
	if len([]rune(first)) <= fitsOneRow && label != first {
		return false
	}
	return true
}
