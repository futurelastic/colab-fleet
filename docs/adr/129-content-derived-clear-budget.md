# ADR: a clear pass is sized to the composer's own row count, not a clock

**Issue:** #129
**Status:** decided

## Context

`discard`'s clear loop (`clearComposer`) pressed a line-kill keystroke
repeatedly against a composer, bounded by a flat 3-second window
(`promptClearWindow`) regardless of how much text the composer held. #87
established the loop's stall-detection (stop early once real progress has
definitively stopped) but left the window itself as an unexamined constant.

Field evidence (#124, restated in #129) showed the gap this left: a real
stranded composer holding a large paste could not be cleared by `discard`
inside the window, but was cleared immediately by a human at the terminal
pressing the same key repeatedly. "One press clears the line the cursor is
on, not the whole buffer" was already understood — the missing piece was
that the number of presses a composer needs scales with how much it holds,
while the budget allowed did not scale with anything.

## Decision

**Size a clear pass to the composer's own on-screen row count, not to a
duration.** `composerVisualLines` counts how many rows a composer's fenced
box occupies right now (a small extraction from `composerText`'s existing
structural search, `composerSpan`); `clearComposer` presses
`expectedLines + clearPressMargin` times, capped at `maxClearPresses`, in
place of the old wall-clock loop. `stallPresses`'s early-exit (#87) is
unchanged — it still fires before the budget is necessarily exhausted, for
the same "real progress that then genuinely stops" case it always covered.

**Visual rows, not logical lines — measured, not assumed.** `composerText`
already collapses every continuation row into one space-joined string, so
there is no logical line count left to recover from it even in principle.
More importantly, the one real field measurement available (#32: a composer
holding 209 characters of continuously-typed text, no line break the human
put there, that needed four presses to empty and corrupted on the third
under the old un-repeated fix) is only consistent with a **visual** count —
a logical count would have read that same text as one line and predicted a
single press was enough, which #32 already disproved. A captured pane's
rows are wrapped at whatever width the terminal rendered them at, which
settles "which count" as a side effect of counting the screen directly
rather than requiring a separate pane-width query and an independently
re-implemented wrap simulation.

**The driver's declared deadline (`defaultDeadlineMs`) is raised from 5000
to 30000ms.** A press budget sized to #129's own field case (~80 rows) needs
on the order of `maxClearPresses` presses at `promptClearInterval` (200ms)
apart — arithmetic that cannot fit inside the original 5-second bound
regardless of what "wedged" means, and §4.4 ties the declared deadline to
the whole driver, not to one verb.

**The second-call refusal (#87's `futileClearAttempts`, `attempts > 0` →
refuse before pressing) is kept exactly as it was — reconsidered, not
changed.** The field evidence that prompted reconsidering it (a human's
persistence succeeding where the API gave up) is fully explained by the
budget the API's own pass got to spend, which used to be clock-bound and
too small for a paste #129's size at any machine speed. Nothing in that
evidence says a SECOND automated pass, sized the same way, would succeed
where the first content-sized one did not — only that the FIRST pass this
driver used to run was never given a fair shot. Once the budget is
content-derived, a residue that still has not moved after a full pass is
materially stronger evidence than the old 3-second pass ever produced, so
the refusal's underlying premise ("a full pass is real evidence") is more
true now than when #87 wrote it, not less.

## Alternatives considered

**A larger flat window** (e.g. 30s instead of 3s), keeping the clock-based
model. Rejected: still the wrong unit. Any single flat duration is either
wasteful for a short composer or insufficient for a long one; #129's own
point is that the quantity that matters is row count, not time.

**Loosen the second-call refusal to allow one extra pass before refusing
(a two-strike threshold) instead of keeping it unconditional.** Considered
seriously, because #129 explicitly asked for this to be reconsidered.
Rejected because it does not follow from the evidence: the evidence shows
the FIRST pass was underpowered, not that a repeat of an equally-sized pass
would behave differently against the identical residue. Loosening the
threshold would also have required `Discard` to inspect the post-pass
`attempts` count and choose between `discardIncomplete`'s bounded "safe to
retry once" message and `discardProvenFutile`'s message on the SAME call —
existing test coverage
(`TestDiscardIncompleteFirstMessageBoundsItsOwnSafetyPromise`) already pins
the former as a promise that must hold for exactly one retry, which the
two-strike design would have broken on the second occurrence unless that
selection logic were added. Simpler and better-justified to fix the
upstream cause (the budget) and leave the guard as #87 designed it.

**A per-route deadline override** (give `handleDiscard` and the
`ReplaceIfStranded` Send path their own larger deadline in the service
layer, leaving `defaultDeadlineMs` untouched for every other verb).
Considered for its smaller blast radius — other operations' "wedged
multiplexer" detection would stay at the original 5s. Rejected for this
change: it spreads the fix across two files instead of one, and diverges
what `Capabilities().DeadlineMs` reports from what `Discard` actually
honours, which is a subtler thing for a future reader to notice than "every
operation gets more slack." Worth revisiting if 30s headroom on fast verbs
(Close, Rename, State) turns out to cost something observed in practice.

**`maxClearPresses` sized to remove the ceiling entirely** (no cap, rely
solely on `ctx`). Rejected: §4.4 requires a driver to declare and honour a
bound, and #129 itself says not to remove futility protection — a composer
whose row count has no natural ceiling still needs one imposed, and hitting
it produces the same honest "made progress, ran out of budget" report a
genuine stall already produces, so a caller does not need to know which of
the two happened to react correctly.

## Consequences

- A composer well beyond the old 3-second budget's reach now clears in one
  `discard` call, without a caller needing to retry or intervene by hand.
- Every driver operation's "genuinely wedged multiplexer" detection now has
  6x the slack it had before (30s vs 5s) — a deliberate trade, made because
  one verb's legitimate worst case is now far larger than the rest, and
  §4.4 declares one deadline per driver rather than one per verb.
- A composer that genuinely never moves is still refused a repeat clear
  before any keys are pressed — the #87 discipline this ADR keeps in place
  — but the pass that refusal now guards is materially more thorough than
  the one it used to guard, since it is sized to the residue's own content
  rather than to a clock.
