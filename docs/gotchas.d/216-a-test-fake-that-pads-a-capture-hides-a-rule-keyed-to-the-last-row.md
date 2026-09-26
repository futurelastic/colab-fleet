# A test fake that pads a capture by one row hides a rule keyed to the pane's last row

**Issue:** #216 (a composer whose closing rule is cut off the bottom of the pane)

## What broke

The new rule reads a `❯` row as cut off by the pane's bottom edge only when it is
the last row of the capture. `send` passed its test on the first run and `keys` and
`discard` did not: through those two the composer read as *absent*, the old answer.

## The evidence

`send` reads the pane with one capture; `keys`, `discard` and the state read take it
from the batched capture that lists every session. The test fake's batched capture
ended every pane with the row's newline **and** one more, a blank row no pane has, so
a screen that ended on the prompt row read as having blank rows under it. Real
captures do not: measured against the real multiplexer, a batched section is exactly
the pane's height and ends the same way a direct capture does (46 live panes agreed).

## Why nothing noticed before

Trailing blank rows were dropped and never counted, so an extra one changed no
verdict anywhere. Counting them (`screen.blankBelow`) is what made the fake's habit
visible, and only through the paths that use the batched capture.

## The rule it left

A fake that has to model a capture models its **last byte** too: one newline ends the
last row, and the next marker follows it directly. When a classifier starts to depend
on where a capture ends, check that every path that feeds it ends the same way, on the
real multiplexer, before trusting a green test on one of them.
