# A fixed window cuts a tall dialog below its tab bar, and the tab bar was what said "multi-select"

**Issue:** #219 (a multi-select question with a wrapped question block was reported
without `multiSelect`, so `respond {choices}` was refused and the person had to go
to the terminal)

## What broke

The classifier reads a menu from a fixed window, the last `promptScanDepth` (24)
rows. A dialog is as tall as its question and its option descriptions make it: a
question that wraps over a dozen rows, or a description under every option, and the
dialog is taller than that. Two things then fall off the top of the window, in this
order as the dialog grows:

1. **The tab bar** (`←  ☐ … ✔ Submit  →`). It is the one row that tells a checkbox
   question from an agent's own `[ ]` list, so without it the boxes are options like
   any others: `multiSelect` and `freeText` are absent and `choices` is refused.
2. **The first option.** The run then starts at 2, has a gap, and is not read as a
   menu at all.

The report blamed the `│` rail the runtime draws down a wrapped question, which is
what made the shape memorable and was not what broke it. A tall dialog **without**
a rail read the same way. The rail was a second, smaller defect: it stayed in
`question`.

## The evidence

A probe of one dialog shape (four boxed options, then the free-text, Submit and chat
rows) over the question's height and the description rows under each option, run
against the classifier as it was: with no description rows it read as
multi-select up to 11 question rows and stopped at 12; with one under each option it
stopped at 8 (7 still read); with two, at 4 (3 still read); with four, the first
option itself was cut off — its slot came back empty — at a three-row question. The dialog reads correctly the moment it fits — nothing else in
the shape changed. Every existing multi-select fixture fits the window, which is why
none of them saw it.

The same shape drawn in a real multiplexer, read through the driver's own capture,
reproduced it (`multiSelect` and `freeText` absent, options intact), and after the
fix the same run ticks exactly the set it was given (`TestLiveWrappedMultiSelectIsReadAndAnswered`).

## The rule it left

- **A fixed depth is a guess about the height of something whose height an agent
  chooses.** Anchor on the dialog's structure instead: it opens with a rule, so a
  window that starts below its opening rule is widened to it (`dialogTop`), and
  never past it — the prose above a dialog can hold a numbered list, and widening
  further would have read it as options.
- **When a heuristic keys on a corroborating row, test the height that pushes that
  row out, not only the shape that carries it.** The `[ ]`-list guard was correct;
  its input could be cut off.
- The capture is the pane plus 24 rows of history, so this reaches only what the
  capture holds. A dialog taller than that has lost its tab bar to the capture, not
  to the window, and reads as before.

## Not changed here

- `question` still keeps only the last three rows above the options, so a long
  wrapped question loses its first rows. That was a deliberate bound when the rows
  above a dialog could be transcript; with the header found they no longer can be.
  Widening it is a separate decision about what a client shows a person.
- The corpus case for this shape is reconstructed from the measured layout, not a raw
  capture, and its rail rows are redacted to plain placeholder text (the redactor keeps
  no `│` on a prose row), so the rail itself is pinned by a unit test that draws it.
