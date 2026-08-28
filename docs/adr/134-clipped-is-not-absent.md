# ADR: a composer this driver could not fully read is a third state, not "absent"

**Issue:** #134
**Status:** decided

## Context

`captureForClassify` reads a pane's tail with `capture-pane -e -S -N`
(`classifyCaptureArgs`, N = `defaultCaptureLines`, currently 24) and **no**
`-E` bound. `composerSpan` finds a composer by scanning UP from the closing
rule for the ❯-marked prompt row, and used to return not-found if the walk
reached the top of the captured lines without finding one — indistinguishable,
to every caller, from a screen that genuinely has no composer at all.

That collapse is dangerous specifically because `ok=false` (as it was) reads
identically to "nothing here" everywhere it is consulted: `Discard`'s own
early return (`pending, _ := composerText(sc); if pending == "" { return
Accepted: true }`) reports SUCCESS on a composer that, off-screen, still holds
dozens of rows of unsent text; `Send`'s composer-busy guard never fires, so a
delivery proceeds to concatenate onto a composer §2.4 exists to protect.
`colab-fleet#129`'s own field case measured on the order of eighty on-screen
rows — more than three times the current 24-line scrollback margin.

`TestComposerSpanMissesAComposerTallerThanTheCaptureWindow` (added by #133)
reproduces it directly: a screen built from only the tail 24 rows of a 30-row
composer — no opening fence, no ❯-marked row in view, exactly what a real
`-S -24` capture would hand the classifier for that shape.

## Decision

**Give `composerSpan` a third outcome, `composerClipped`, distinct from
`composerFound` and `composerAbsent` — and change every direct caller's
signature so the compiler forces a decision at each site, rather than
inheriting the old boolean's silent collapse.**

```go
type composerScan int

const (
    composerFound   composerScan = iota
    composerAbsent
    composerClipped
)

func composerSpan(s screen) (prompt, last int, scan composerScan)
```

`composerClipped` fires in exactly two places inside the existing walk, both
previously mis-reported as `composerAbsent`:

- the walk up from `last` looking for a ❯-marked row reaches `i == 0` without
  hitting either a rule or the marker (`TestComposerSpanMissesAComposerTallerThanTheCaptureWindow`'s
  own shape — no opening rule *and* no prompt row anywhere in view);
- the fence check (walking up from `prompt-1`) runs off the top having seen
  only blank lines — it never got to look at a real line to judge fenced or
  not.

Both are deliberately **narrow**: a rule found with no marker between it and
another rule stays `composerAbsent` (a positive finding — this driver looked
and confirmed there is no composer here), and a real non-blank, non-rule line
sitting above the prompt (`fixtureMenuSelected`'s own shape — a real question
line) also stays `composerAbsent`, not clipped. Only "the search ran out of
capture before it could decide" is clipped.

`composerText`, `composerVisualLines`, and `composerCursorRowBlank` all return
`composerScan` instead of `bool`. The contract every caller must now honour:
only `composerFound` and `composerAbsent` license any inference about what the
composer holds. `composerClipped` must never be treated as empty and must
never be treated as absent.

Eleven call sites across `classify.go`, `tmux.go`, and `keys.go` were updated
to name their clipped branch explicitly (`_ = scan` at an inference site was
treated as a review defect, not a shortcut):

| site | before | under `composerClipped` |
|---|---|---|
| `Discard` early return | `pending==""` → 202 Accepted | 409 `ErrAmbiguousTarget` (`discardComposerClipped`) |
| `Send` composer-busy guard | `ok && pending!=""` → refuse | refuse (fail closed), own reason |
| `keys.go` `composerHoldsText` | `pending!=""` | after any recognised-prompt refusal takes priority, refuse with its own reason — see "Where the keys.go refusal had to move", below |
| `clearComposer`'s `got==""→cleared` | success | additionally requires `composerFound` |
| `respond`'s `hasComposer` | `false` | `true` (a composer is definitely painted) |
| readiness (`promptReadiness`) | "still starting" | not ready, own `waitingOn` reason |
| `receptive` | `ready = found` | `ready = scan != composerAbsent` |
| `confirmSubmitted` | `found && text==""` | unchanged, asserted explicitly (`scan == composerFound`) |
| `currentComposerDigest` | `""` | `""` (honest degrade, unchanged) |
| `composerVisualLines` budget calls in `Send`/`Discard` | `0` | `0`; unreachable in practice because the refusals above fire first |
| `classifyAgedDetail` state inference | `hasComposer=false` → starting/unknown | new explicit branches ahead of every "composer empty" case; never asserts empty for a clipped screen |

## Where the `keys.go` refusal had to move

The first version of this change refused on `composerClipped` before any
digest corroboration was even computed. That broke
`TestKeysRefusesWhenRespondCouldAnswerInstead`: its fixture (`fixtureMenu`, a
real, complete menu capture with a rule but no ❯-marked line — because no
option is highlighted in that particular fixture) is legitimately ambiguous
under the new predicate — it structurally matches "ran off the top without a
rule or marker" even though nothing was actually truncated; there is no way
for `composerSpan` to tell "ran off the top of a genuinely short screen" apart
from "ran off the top of a truncated capture", because a `screen` only ever
holds what was captured.

`parsePrompt`, however, is a fully independent, structural scanner (numbered
options plus a footer) that does not depend on `composerSpan` at all, and it
recognises `fixtureMenu` as a real dialog. The fix: let the digest-scope
decision degrade to screen-scope for a clipped composer (the same branch an
absent composer already takes — there is no composer-scope digest to form
either way, since `pending` is always `""` for a non-`composerFound` scan),
keep the existing precedence where a recognised prompt (`respond` can do
better) is checked before any composer-based refusal, and only then refuse
specifically for `composerClipped` — parallel to, and just above, the existing
`composerHoldsText` refusal. A caller sitting on a genuinely ambiguous
composer with **no** recognised prompt to fall back on still gets refused
before any key reaches the pane; a caller sitting on a recognised dialog still
gets routed to `respond`, exactly as before.

## Alternatives considered

**(a) Raise `defaultCaptureLines`.** Rejected — it cannot address the
failure. `classifyCaptureArgs` emits `capture-pane -p -e -t <pane> -S -N` with
no `-E`; tmux's `-S -N` means "start N lines above the top of the *visible
pane*" and the default `-E` is the bottom of the visible pane, so a capture is
already `paneHeight + defaultCaptureLines` rows, not `defaultCaptureLines`
rows. `defaultCaptureLines` only controls the scrollback margin, and the
dominant term (pane height) is not this driver's to set — no constant bump
gives a real ceiling. Worse, under the reading where a composer's top
genuinely scrolled into history, raising `-S` makes `composerSpan` more likely
to find a **stale** `❯` from a previous frame and report stale text as live —
a false positive strictly more dangerous than today's false negative. It also
leaves the bug fully intact for any pane shorter than the composer, which no
constant fixes. Separately, `keys.go` hashes the raw capture as part of its
screen-scope digest, so a bigger window makes that digest more volatile and
produces spurious 409s for unrelated reasons.

**(c) Bound composer height and refuse/warn past it.** Rejected — this driver
does not control what a human pastes; a refusal with no remedy behind it. It
would also have to detect the same "ran off the top" condition `(b)` detects
anyway, just to decide when to fire, so it buys nothing over `(b)` and adds a
second predicate to keep in sync with the first.

**(d) Let `keys.go` refuse on `composerClipped` before digest scoping,
unconditionally.** Tried first, reverted — see "Where the `keys.go` refusal
had to move", above. It regressed a real dialog into an unhelpful refusal
message instead of routing to `respond`.

## Consequences

- Every caller that used to read a tall composer's "not found" as "nothing to
  protect" now fails closed instead: `Discard` refuses rather than claiming
  success, `Send` refuses rather than concatenating, `keys.go` refuses rather
  than pressing blind (unless a recognised prompt offers a better path).
- The blast radius is a **new class of 409/refused outcomes**, not new
  destructive behaviour — the worst this change can do wrong is over-refuse a
  screen this driver could, in fact, read safely. That is visible and
  recoverable; the false success it replaces was neither.
- `composerClipped` is reachable off a fixture (`TestComposerSpanMissesAComposerTallerThanTheCaptureWindow`)
  but has not been separately confirmed against a live pane in this change —
  it requires a composer whose box is taller than `paneHeight +
  defaultCaptureLines`, which real terminals can produce (colab-fleet#129's
  own ~80-row field case), but if the TUI caps its own composer box height
  below that, this path may be rare or unreachable off a real pane. The
  fixture-level false-negative it closes is real regardless.
- `#138`'s key-selection fix (clearComposer's `composerCursorRowBlank`
  consumption) and `#136`'s escape-hatch design both build on this contract
  directly — see their own ADRs.
