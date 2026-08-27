# ADR: cross a blank composer row with Backspace, not C-u

**Issue:** #132
**Status:** decided

## Context

`clearComposer` (#87, sized by #129 to the composer's own row count) presses
C-u repeatedly to walk a composer's unsent text backward until it empties.
C-u is readline's unix-line-discard: it kills from the cursor back to the
start of the CURRENT line, and nothing more — it was never defined to cross
a line boundary.

A payload ending in one or more real newlines leaves exactly that boundary
behind: a blank row, with nothing on it for C-u to kill. Every further C-u
press against that row comes back byte-for-byte identical. Depending on
whether an earlier row had already cleared, that reads as either #87's
stall path or straight into `noteFutile` — in both cases the residue is
recorded as proven-unclearable and every later `discard`/`replaceIfStranded`
call is refused before a key is ever pressed again (#87's own guard against
spending a second full pass on evidence a first one already produced).

This is the field shape #132 reports directly: `replaceIfStranded` cleared
down to 306 characters and stopped, the record's digest no longer matched
what the composer actually held, and no combination of `resumeIfStranded`,
`replaceIfStranded`, or a plain retry could reach the composer again. A
human had to clear it by hand — confirming, by the fix that worked, that a
line-kill does nothing when the cursor sits at the start of a blank line.

## Decision

**Detect a blank current row before choosing the key, and cross it with
Backspace instead of C-u.** `composerCursorRowBlank` (classify.go) reports
whether the composer's bottom content row — the row the cursor is assumed to
sit on, the same row `composerVisualLines`' count shrinks from as rows clear
— is empty right now. `clearComposer` checks this once per iteration, before
sending anything: blank means send `BSpace` (which deletes the ONE character
behind the cursor — on an empty row, the newline itself, merging the row into
the end of the row above); anything else means send `C-u`, unchanged.

**A blank-row merge counts as progress even when the joined text does not
change.** `composerText` already drops every blank continuation row from
what it concatenates (so a caller reading `pending` never sees them at all),
which means a Backspace that only removes a blank row is invisible to a
text-only comparison. `clearComposer` now also compares `composerVisualLines`'
row count between presses; either signal changing counts as movement for
`stallPresses` (#87) and `noteFutile`'s purposes. Without this, a session
whose payload happens to be ALL blank-row merges (unlikely, but not
impossible for a very sparse paste) would look permanently unmoved by text
alone and be marked futile despite genuinely shrinking every press.

**Both `clearComposer` callers now pass the initial screen through**, not
just `pending` and `expectedLines`: `Discard` already had it (`sc`);
`tryReplaceStranded` now threads `screenNow` from `Send`'s own call site.
Neither caller re-reads the pane to get it — it is the identical screen
`pending`/`expectedLines` were already read from, so no new corroboration
requirement is introduced.

**No third key, no changed budget shape.** The press count is still
`expectedLines + clearPressMargin`, capped at `maxClearPresses` (#129) — a
blank row still costs exactly one slot in that budget, whichever key
clears it, so #129's sizing rationale (one press-equivalent per on-screen
row) is unchanged. Escape and C-a C-k remain what #87/#129 already measured
them to be (not helpful here); Backspace is not offered as a third
alternative to try, it is what runs INSTEAD of C-u for the one row shape
C-u structurally cannot touch, alternating back the moment that shape is
gone.

## Alternatives considered

**Always send Backspace before C-u, unconditionally, on every press.**
Simpler — no per-iteration blank check needed. Rejected: against a NON-blank
row this would delete one character of real content before C-u kills the
rest of the line, silently corrupting whatever was on that row rather than
merely taking one extra (harmless) press. The whole point of detecting
blank first is that Backspace and C-u are not interchangeable against real
text.

**Send C-u twice per row unconditionally** (on the theory that a second
press might somehow "catch up"). Rejected outright without needing to test
it: unix-line-discard against an already-empty line has no state to
accumulate across presses — a second no-op is still a no-op, and this would
only have doubled the wasted budget on every blank row while changing
nothing about whether it clears.

**Detect blank rows from `pending` (the already-corroborated joined text)
instead of re-inspecting the screen each iteration.** Rejected: #129's own
finding is that `composerText` already collapses blank continuation rows
out of what it joins — by the time `pending` exists, the information this
fix needs (which specific row is blank, right now) is already gone. The
screen has to be read structurally, the same way `composerVisualLines`
already does.

**Give `clearComposer` its own capped “Backspace budget” separate from the
row-based press budget**, on the theory that Backspace and C-u are different
operations and might deserve different bounds. Rejected as unnecessary
complexity: a blank row still corresponds to exactly one on-screen row
`expectedLines` already counted, so it already has a budget slot; a second,
parallel budget would only create a case where the two disagree with no
principled way to reconcile them.

## Consequences

- A composer whose payload ends in (or contains) real blank lines now
  clears fully through `discard` and `replaceIfStranded`, instead of
  stranding partway with a digest mismatch and no API path back.
- `clearComposer`'s two callers (`Discard`, `tryReplaceStranded`) both now
  pass the screen they already read `pending`/`expectedLines` from, rather
  than only the two derived values — a small signature change, no new
  corroboration.
- `noteFutile`/`futileClearAttempts` (#87) now see fewer false positives:
  a residue that merely had a blank row in it is no longer indistinguishable
  from one that genuinely resists clearing, because the row-count signal
  lets a Backspace-only pass register as movement even where the text
  comparison alone would have missed it.
