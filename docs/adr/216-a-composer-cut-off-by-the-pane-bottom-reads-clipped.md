# ADR 216 — A composer whose closing rule is cut off by the pane's bottom edge reads clipped, not absent

**Status:** accepted (2026-09-26)
**Issue:** colab-fleet #216 · builds on #134, #149, #169, #215

## Context

`composerSpan` takes the last rule on screen for the composer's closing fence and
walks up from it looking for a `❯` row. When the pane is too short to show the
closing rule and the mode row, the screen ends on the composer's *opening* rule and
its prompt row, nothing below. The last rule is then the opening one, the walk goes
up out of the composer into whatever box border sits above it, and the verdict is
`composerAbsent` — a *positive* finding that there is nothing to protect — where the
honest answer is "the composer is there and its bottom is not visible".

Anything tall enough above the composer does this on the 24-row pane a created
session gets. The feedback-draft card (#215) is the measured case, about seven rows.
#215 guarded `send`, `keys`, `discard` and `respond` for the card only, by
recognising the card. Every other notice of that height reproduced the same list:

- the state read's finished-spinner branch never asked whether a composer had been
  read and said `idle` / "composer empty";
- `send` asked, was told there is none, and refused with a message about startup or a
  full-screen interface;
- `keys` did not see typed text on the unreadable row, and `discard` answered
  "already clear" for a row it could not read.

## Decision

**A prompt row that is the last row of the pane, opened by a plain rule, with no
rule below it, reads `composerClipped`**, and every refusal that already exists for
a clipped composer applies (ADR 134, 149). Three conditions keep it narrow, because
it changes what three verbs decide for every session:

- **The last row of the capture, not the last non-blank line.** `capture-pane` pads
  to the pane's height (measured on the real multiplexer: a 24-row pane captures as
  24 rows whatever it holds, and the batched capture the driver uses per pane ends
  the same way as a direct one). A prompt row with blank rows under it is the runtime
  having had room and painted nothing there, which the pane's edge did not cause and
  this rule does not claim. `screen` now records the blank rows it dropped.
- **A plain rule above it.** A box's bottom border (`╰───╯`) has rule runes in it and
  is exactly what the old walk mistook for the opening fence.
- **Not a numbered option.** A menu's highlighted option is drawn with the same `❯`.
  Menus are read as prompts first by every verb; nothing here changes how one reads.

**The finished-spinner branch no longer says "composer empty" for a composer it did
not find.** A finished spinner with no composer read at all is `unknown`, evidence
naming the missing composer. It used to fall through to the `idle` case, so a screen
the send gate refuses ("no composer has been painted") read as `idle`, the one
status that means "send it work". The healthy shape — a finished spinner and a
composer that reads whole and empty — is unchanged.

**The reasons name the cause.** A composer cut off at the bottom has its top in plain
view; telling a caller it is "taller than the capture window" sends it looking for a
paste that is not there. `composerClippedCause` gives each refusal and the state
read's evidence the right clause, and the one shared remedy sentence names all three
causes and the exit for the new one (a pane tall enough to draw the composer, or a
person at the pane).

**The card keeps refusing by name.** #215's refusals sat behind `scan == composerAbsent`
and a readiness gate that this shape no longer fails. `send` and `discard` now check
the card before the generic clipped refusal, so the card is still named where it is
what cut the composer off.

**A counter for the rule:** `composer_clipped.bottom_cut`, incremented beside the
per-verb clipped counter (as `fence_above_visible_pane` is for #169), so the rate at
which real traffic reaches the refusals through this rule is readable without
inferring it from the total.

## Measured before changing it

The Issue asked for the rate first: the clipped-refusal counters give the rate at
which the state is reached today, and this rule moves a shape from unmeasured
("absent") into them. The counters were not readable from the session that made
this change (the service's API needs a credential it did not hold), so the check is
an **offline replay** instead: the old and the new `composerSpan` over the 20
committed corpus captures and every pane of the multiplexer on one machine, read
once with the driver's own capture arguments (45 panes).

- The verdict changed on **one** screen of 65: the corpus's real capture of the card
  (absent → clipped), the case this exists for.
- **None of the 45 live panes changed.** 44 read `found`, one `absent` (not this
  shape), none `clipped`.
- The population the finished-spinner change can touch — a finished spinner with no
  composer found — was two corpus screens, both menus recognised as prompts *before*
  that branch is reached. The state read's answer changed on none of the 65.

What this cannot say: it is one machine at one moment, not a traffic-weighted rate.
It bounds the blast radius (a screen of this shape is rare enough that a sample of
45 live sessions held none) and does not replace the counters, which keep running.

## Alternatives rejected

- **Cover a wrapped composer too** — a `❯` row with continuation rows below it, cut
  off by the same edge. Same defect, and `discard` still answers "already clear"
  for it. Left out on purpose: it is a superset of this rule, it flips more screens
  than the Issue's stated shape and than this measurement covers, and the state read
  no longer says `idle` for it (the finished-spinner change). Recorded as a reopen.
- **Count a blank-padded prompt row as cut off.** It would turn a torn frame (the
  runtime mid-repaint, rule and prompt painted, nothing below yet) into a clipped
  verdict, and a young session's readiness would flip from "still starting" to "the
  composer is clipped". The last row of the pane is the only place the edge can be
  what hid the rest.
- **A fourth `composerScan` value.** Every site that branches on the scan compares
  against `composerClipped`; a variant would need each of them taught a second spelling of
  the same refusal. The cause is a property of the screen, asked for where wording
  needs it (`composerClippedCause`).
- **Size new panes explicitly.** A mitigation only: a client that attaches resizes the
  window, and sessions not created through this service are untouched.

## Consequences

- **`discard` on the card over an empty prompt row is now refused.** #215 answered
  "already clear" (nothing on the row, nothing to discard), and pinned it in a test.
  The composer's bottom is not visible, so the row being empty does not say the
  composer is; this is the answer every other clipped composer gets, and it agrees
  with what `send` says of the same screen (the card is a prompt, a message is
  refused until a person answers it). The refusal names the card.
- **`send` to a bottom-cut composer that is not the card** is refused with the
  bottom-edge cause instead of "no composer has been painted", which blamed startup.
  It counts under `composer_clipped.refused_send` and `composer_clipped.bottom_cut`.
- **A young session's prompt-delivery readiness** for this screen reads "cannot
  confirm the composer is empty" (`unsent_input`) instead of "still starting". The
  screen is only reached on a short pane with something tall above the composer.
- **The one shape this can over-refuse:** an *unrecognised* full-screen list, drawn
  with an unnumbered `❯`-marked row, whose highlighted row is the pane's last row
  directly under a plain rule. It cannot be told from a prompt row, so it reads
  clipped and `keys` refuses it, where it read absent and `keys` was the documented
  way to reach a dialog the driver does not know. Recognised dialogs are not
  affected (every verb reads a prompt before it reads the composer). This is the
  fail-closed direction (ADR 134: an over-refusal is visible and recoverable, a false
  "absent" is neither), and none of the 65 replayed screens was this shape.
- **The test fake's batched capture** appended one blank row after every pane; real
  captures do not. Harmless while trailing blanks were dropped and not counted, and a
  false negative for this rule through `keys` and `discard` once they were. It now
  ends a capture on the last row's newline, as measured.
- The `composer_clipped.refused_*` counters now also count this shape. A rise after
  this ships is the rule working, not a new fault; `composer_clipped.bottom_cut` says
  how much of it.

## Reopen when

- `composer_clipped.bottom_cut` shows the rule refusing something a person can see
  is a working session (a shape it should not have caught), or reads zero across a
  long run of a fleet with taller notices — at which point a wider rule is worth
  measuring.
- A wrapped composer cut off by the bottom edge is seen holding text a `discard`
  called clear.
- The runtime changes where it draws the composer's fences, or stops padding its
  frame to the pane's height.
