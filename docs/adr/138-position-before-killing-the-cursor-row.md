# ADR: position the cursor before killing, and flip key shape when a press proves itself a no-op

**Issue:** #138
**Status:** decided

## Context

`#132` established the right mechanism — a composer clear needs both `C-u`
(unix-line-discard) and `Backspace`, because `C-u` cannot cross a line
boundary. `clearComposer` chooses between them using `composerCursorRowBlank`,
which reports whether the composer's bottom content row is empty right now.

The gap: **which key to press is chosen by testing whether a ROW is blank,
but what actually determines whether `C-u` can do anything is whether there
is content between the start of the line and the CURSOR.** Those two
questions agree on #132's shape (trailing newlines → blank rows) and
disagree on the shape that wedged a session again on 2026-08-28 — 305
characters of residue sitting on the composer's own ❯-marked row, `C-u`
pressed to budget exhaustion, zero movement.

`composerCursorRowBlank`'s own doc comment names the failure mode directly: a
composer that has shrunk to (or never had more than) its own ❯-marked line
reports `blank=false, never true` — that line always carries the marker glyph
as visible content, so it can never look like the empty-row shape the
function exists to detect. `clearComposer` then does `key := "C-u"; if
curBlank { key = "BSpace" }` — so for a composer whose remaining text sits on
(or has collapsed to) the ❯ row, `curBlank` is structurally `false`, and the
loop presses **C-u and only C-u** until the budget runs out. If the cursor
was not already sitting after that row's content — the assumption
`composerCursorRowBlank` never actually established — every press reports an
identical capture: `moved` never becomes `true`, and the pass ends having
done nothing.

## Decision

**Two mechanisms, one for the ordinary case and one as a convergence
guarantee:**

1. **Position before killing.** When the current row is not blank (or is
   `composerClipped` — see below), `clearComposer` sends `composerLineEndKey`
   (`End`) immediately before `C-u`, **in the same `send-keys` call**, as one
   press-budget slot — not `C-u` alone. `End` is non-destructive (unlike
   sending `Backspace` unconditionally, which #132 already rejected as an
   alternative for exactly this reason: it eats a real character on a
   non-blank row). Once the cursor is provably after the row's content, `C-u`
   is well-defined regardless of where it started.
2. **The no-movement latch.** If an iteration — whichever shape it used —
   produces **no movement** (neither the joined text nor
   `composerVisualLines`' row count changed), the **next** iteration uses the
   **other** shape (`End`+`C-u` ⇄ `BSpace`) rather than repeating the one
   that just proved itself a no-op. A press that changes nothing is itself
   the evidence the row-blankness guess was wrong for this row; alternating
   costs one budget slot and converges without ever needing to observe the
   cursor's column directly. A press that **does** move resets the latch, so
   the ordinary blank-based choice governs again once progress resumes.

`composerCursorRowBlank` returning `composerClipped` (colab-fleet#134: this
driver could not read the row at all) is treated the same as non-blank —
default to `End`+`C-u` and let the latch correct course if that guess is
wrong. "Assume nothing is there to kill" is the direction that reproduces
#134's own false negative one level down, inside the clear loop itself.

`composerLineEndKey` is a named constant (`"End"`), not an inlined literal:
if a substrate is ever measured not to bind `End`, the field fix is swapping
this one constant to `"C-e"`, not re-deriving the mechanism.

`clearComposer`'s `got=="" → cleared` success branch additionally requires
`gotScan == composerFound` (colab-fleet#134's own requirement, landed with
that issue and unaffected by this one) — a mid-pass capture that comes back
`composerClipped` must never be read as "cleared" just because the *text*
this driver could extract happens to be empty.

Budget semantics are untouched: still `expectedLines + clearPressMargin`
capped at `maxClearPresses` (#129), so a blank row and an `End`+`C-u` press
each still cost exactly one budget slot regardless of which shape was
chosen. Wall time roughly doubles for the `End`+`C-u` shape versus plain
`C-u` (two keys sent per `send-keys` call instead of one), which fits
comfortably inside `defaultDeadlineMs` (30s).

## Alternatives considered

**Read the cursor's actual column** (e.g., via `tmux display-message -p
'#{cursor_x}'`) and use it directly instead of a row-blankness proxy at all.
This is the theoretically correct signal, and it was the first design
considered. Rejected for this change: it costs a new subprocess call per
press (this driver already has one documented capture-shape discipline —
`classifyCaptureArgs`/`capture_shape_test.go` — specifically to stop capture
invocations proliferating, and a second, differently-shaped observation adds
a second discipline to keep in sync with it), and it still needs the
composer box's content-start column, which is TUI chrome this driver
otherwise never parses. Positioning with `End` needs no new observation at
all — it does not need to know where the cursor is, only that it can be
moved to a known-good place. Recorded here as the escalation path if `End`+
latch is ever measured to fail in the field: cursor position remains
observable even when the composer is `composerClipped`, which `End` alone
does not resolve (a clipped row is still positioned by `End`, but this
driver still cannot corroborate what was there beforehand).

**Always send `Backspace` before `C-u`, unconditionally, on every press,
regardless of row blankness.** Already rejected by #132 for the identical
reason: against a non-blank row this deletes one real character before
`C-u` kills the rest of the line, silently corrupting content rather than
merely costing an extra harmless press. `End` does not have this problem —
it repositions, it does not delete — which is exactly why it is the
positioning primitive used here and `Backspace` is not.

**Detect "no movement" and immediately fall back to `Backspace` for the rest
of the pass, rather than alternating.** Rejected: a one-way fallback assumes
`Backspace` is now permanently the right shape, which is not established —
the row that failed to move may itself change (shrink, or a later row
becomes current) such that `End`+`C-u` becomes correct again. Alternating,
governed by the blank-based choice resuming the instant a press moves
something, keeps the ordinary heuristic in charge whenever it is working and
only overrides it exactly as long as it keeps failing.

## Consequences

- The measured field shape (residue on the ❯ row, plain `C-u` pressed to
  budget exhaustion with zero movement) is closed for the ordinary case by
  mechanism 1 alone; mechanism 2 is the safety net for a substrate where
  `End` itself does not help.
- `End`'s own hazard is real and undefended beyond the latch: if this TUI
  does not bind `End`, tmux sends the terminal's `kend` escape sequence, and
  an application that does not recognise it could echo it as literal bytes —
  **adding** to the composer instead of leaving it alone. The no-movement
  latch bounds this to one iteration (a press that adds content is not "no
  movement", it registers as movement via the text/row-count comparison and
  the pass continues from there rather than repeating blindly) but does not
  prevent it. This is the highest-risk edit in the `composer-detection-clear`
  group and was not separately verified against a live pane in this change —
  a field measurement remains open.
- `#132`'s own blank-row matrix is unaffected: `TestClearComposerStillSendsBSpaceAloneOnABlankRow`
  pins that the first press against a blank bottom row stays bare
  `Backspace`, never `End` first.
- `#136`'s escape-hatch design is informed directly by whether this fix alone
  closes the measured failure — see that issue's own ADR for the answer.
