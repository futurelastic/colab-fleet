# Confirming an empty free-text field declines the whole dialog — and typing into the row changes the nonce and hides the row

**Issue:** #206 (a free-text `respond` form)

## What happened

Answering a question in the caller's own words means using the runtime's free-text
row (`Type something`). Nothing here ran that end to end before, so a live run in a
disposable session measured what the row does. The fixtures the earlier respond
work used do not model any of it, and two of the results are the kind a model
written from the docs gets wrong:

- **Enter on the row while it is EMPTY does not answer with nothing.** It declined
  the whole dialog — every question in it, not just this one. A driver that
  pressed the confirm key after a paste that had not landed would have reported
  `submitted` for an action that withdrew every answer the caller had given.
- **On a numbered menu a digit naming the row does not commit** (a digit on any
  other row does, #168): it moves the highlight there and the field takes focus.
  Once the field has focus, a digit is TYPED into it. So the digit is right when
  the highlight is elsewhere and wrong when it is already on the row.
- **On a multi-select question the same digit only TOGGLES the row's tick.** The
  field takes focus when the highlight is on the row, so the way there is Down,
  one key per call. Typing ticks the row itself; Enter on the row toggles the tick
  OFF again (the text stays); Down from it lands on the unnumbered Next/Submit row.
  What advances the dialog is Right from a checkbox row — the #176 route — so the
  highlight has to come back up after the text is in.
- **The text becomes the row's label.** `❯ 3. Type something.` turns into
  `❯ 3. <the text>`; an embedded newline continues on rows of its own (no number,
  so the parser does not see them) and a long line wraps under the label. The
  nonce digests the options, so **every change to the text changes the nonce**, and
  the placeholder is gone — a row that holds text can no longer be found by what it
  says.
- A leading `!` or `/` is plain text in the field. Both reached the agent, verbatim,
  as the answer.

## The rule going forward

- **Never press the confirm key on a free-text row you have not read back.** The
  row must show what was typed, checked against what was sent — a first line that
  fits one row whole, a longer one as a prefix — before Enter. An unread paste is
  how an empty field gets confirmed.
- **Judge everything after the first read by "same question, this row aside", never
  by the nonce** — and judge "did the confirm land" against the nonce read AFTER the
  text was in. The caller's nonce is checked once, up front.
- **Carry the row's index; do not look for its label.** The index is found before
  typing and used after. `prompt.freeText` is absent once the row holds text, on
  purpose: typing over a person's text is refused, and `choice: 0` accepts what is
  there.
- **Send the digit only when the highlight is off the row; use Down on a
  multi-select question.** Neither is a guess about a layout: each was read back on
  the screen, one key per call (see gotcha 204).
- **Text over ~24 rows of screen can push the dialog out of `promptScanDepth`.** The
  field is not capped and does not scroll, so a very tall answer makes the question
  unreadable. The read-back then fails and nothing is confirmed, which is the safe
  outcome; the field may still hold the text, and `keys` can reach a dialog
  `respond` cannot classify.
- Not measured, and therefore not offered (`freeText` stays absent): the row on an
  unnumbered menu, beside a preview pane, on a boxed list with no tab bar.
