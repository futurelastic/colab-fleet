# A footer notice below the composer can pass every structural test a real status line has to

**Issue:** #229

## What happened

The runtime painted a footer notice below the composer's closing rule after a
disk-full condition on the machine cleared:

```
  ⚠ Transcript writes are failing (disk full — ENOSPC) · recent messages may …
```

`spinner()` scans the whole screen backward for the last line matching the
turn-status line's shape (`hasSpinnerGlyph` + a capitalised first word + a
tense marker — see that function's own doc comment for why the glyph test is a
shape, not a specific character). This notice matches it exactly: a single
non-ASCII symbol, a space, a capitalised word, and — because the runtime
truncated the notice — the running tense's own `…` marker. The backward scan
found this line before it ever reached the real, *finished* spinner drawn
above the composer, so an idle session (finished turn, empty composer) read as
`working`. Because nothing repaints the footer on its own, the misread
persisted 1–2 hours after the disk condition that triggered it was resolved,
across four sessions on one machine, until each was sent input for an
unrelated reason.

## The rule

Structural detection (glyph shape + capitalised word + tense marker, per
`hasSpinnerGlyph`'s own history of F37/F42) is necessary but not sufficient
when the scan itself is unbounded: the TUI's chrome below the composer
(mode indicator, keyboard hints, and now this notice) is drawn by the same
runtime and can accidentally satisfy the identical shape test a real status
line needs to pass. The fix that held was not a new exclusion for this one
notice's wording — that would only survive until the runtime rewords it, the
same trap `hasSpinnerGlyph`'s own comment already warns against for verbs and
footers — but a **boundary**: the turn-status line is transcript, and the TUI
never draws it beneath the composer, so `spinner()` now stops its backward
scan at the composer's own closing rule whenever `composerSpan` actually finds
one. That holds for any future footer content this driver has not been told
about yet, not only ENOSPC.

## What is still not covered

The boundary only applies when a composer is structurally found
(`composerSpan` returns `composerFound`). When the composer reads
`composerAbsent` or `composerClipped`, `spinner()` falls back to scanning the
whole screen, because the boundary itself is unknown in those cases — a
footer-shaped forgery below an *absent or clipped* composer is not yet
protected against. Nothing here helps either if a future chrome line is drawn
**above** the composer, in the transcript region a real spinner also occupies
— that is a different, not-yet-observed failure mode this bound does not
reach.
