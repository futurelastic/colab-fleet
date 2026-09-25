# Several keys in one send-keys reach the runtime as one — and on a dialog with a preview pane a digit does not commit

**Issue:** #204 (found while answering a question whose options carry a preview)

## What happened

`respond` on such a question returned `unknown`, and the answer did not land. A
live run in a disposable session measured why, and found a second trap under the
first:

- A digit alone moves the highlight (and the pane) and commits nothing. Enter
  commits the highlighted row.
- The digit and Enter in **one** `send-keys` recorded the question's DEFAULT
  (option 1), not the digit, and the dialog advanced. The nonce changed, so a
  receipt built the way `respond` used to build one would have read `submitted`
  for a wrong answer. Three `Down` in one `send-keys` moved the highlight one
  row; four `Up` moved it one. One key per call, three times in a row, worked
  every time.
- A question has at most four options (a fifth was never drawn), so "digit 5 did
  nothing" was not a fault.

## The rule going forward

- On any dialog, **one key per `send-keys` call**, and read the screen before the
  next one. `walkHighlight` (#171) still sends all of its arrows in one call. It
  was only measured moving the highlight one row on the unnumbered menu; whether
  a longer walk there loses presses as it did beside a pane has not been
  measured. Arrival is read back, so a walk that comes up short reads `unknown`
  and confirms nothing — but do not copy that shape to a layout where more than
  one press is needed.
- Never carry what a key does from one layout to another. A digit commits on a
  numbered menu (#168), toggles on a checkbox (#176), and only moves the
  highlight beside a preview pane (this).
- A receipt may say `submitted` only when the answered prompt is gone; "the nonce
  changed" is evidence that the screen moved, not that the answer was the one
  chosen.
- A preview can be taller than `promptScanDepth`. Anything that reads "the last N
  rows" of a dialog with a pane must widen to the pane's top, or the dialog
  reads as idle.
