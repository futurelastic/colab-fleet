# ADR: a clipped composer has no in-driver proof of emptiness — refuse truthfully, and count it

**Issue:** #149
**Status:** decided

## Context

ADR 134 gave `composerSpan` a third outcome, `composerClipped`: the walk up
from the composer's closing rule ran off the top of the capture before it
found the ❯-marked prompt row or the opening fence. Every destructive or
delivering verb refuses on it — `Discard` (409), `Send` (refused), `Keys`
(refused) — because a composer this driver could not read may hold a person's
unsubmitted text. ADR 134 is right, and this ADR does not loosen it.

What #148 exposed is that the state has no exit. A runtime that paints a
multi-line notice for every held message can stack those notices until the
composer is taller than the capture. The driver then refuses everything, and
each refusal told the caller to "wait for the composer to shrink into the
capture window". On an unattended session with a retrying caller, that never
happens. The observed exit was a person attaching to the multiplexer by hand.
#148 made the capture window configurable (`FLEET_CAPTURE_LINES`). That is a
ceiling an operator sets in advance, not a way out once a composer is already
past it.

#149 asked the hard half: can `Discard` go ahead when it can **prove** the
composer holds nothing that needs keeping? It also said plainly that "no such
proof exists" is an acceptable answer, if recorded as one.

### What a proof would have to establish

Discard deletes only text that someone has seen. ADR 87 refuses a blind
discard, and #136's `force` never relaxes `expect`. So "nothing is destroyed"
needs one of two things for the rows this driver cannot read:

1. an **authoritative read** of those rows, or
2. complete knowledge of **every writer** to the composer since it was last
   known to be empty.

## Decision

**No evidence available to this driver meets either requirement. The refusal
stays exactly as ADR 134 left it, and #149's deliverable is that finding.**

Two things change, so the finding is usable rather than just correct:

- **The refusal stops promising a recovery that does not come.** The three
  refusal sites now share one sentence, `clippedComposerRemedy`, so they
  cannot drift apart again. It says that retrying the same call, with any
  `expect` or `force`, gets the same refusal while the composer stays taller
  than the capture window. It says nothing this driver reads proves the unseen
  rows are disposable, and that the way out is a person reading or clearing
  the composer at the pane. It names no API call as the fix; the alternatives
  below explain why no call qualifies.
- **The refusal is counted**, once per verb:
  `composer_clipped.refused_discard`, `composer_clipped.refused_keys` and
  `composer_clipped.refused_send`. A read that sees the state and does nothing
  destroys nothing and is not counted. The counters measure how often real
  traffic hits this state and through which verb, which is the input the
  reopen condition below needs.

## Alternatives rejected

**A wider capture, on the discard path only.** The capture is the visible pane
plus N rows of history (`-S -N`, no `-E`). When `composerSpan` returns clipped,
the composer's opening fence is above the visible pane, so it can only be in
scrollback, if it exists at all. Scrollback rows are frozen when they scroll
off. A runtime that redraws its composer in place cannot update anything above
row 0, and whether the scrollback copy is the current frame or an older one
depends on how that runtime redraws, which this driver cannot observe. A
widened read can therefore join a stale fence and ❯ row onto the live tail and
return `composerFound`, with a digest of text that is no longer there. That is
ADR 134's stale-marker hazard, now on the one path that deletes. On a runtime
using the alternate screen there is no scrollback, so widening has nothing to
find. And even when the widened read happened to be right, nobody could use
it: a state read publishes no `composerDigest` for a clipped screen, so no
caller holds an `expect` that could match. It would also spend unbounded
history out of the per-call budget ADR 156 divides.

**Resizing the window until the composer fits the visible pane.** When a pane
grows, the multiplexer pulls old scrollback back into view, and nothing signals
that the runtime has finished redrawing into the new space. Resizing also
changes what an attached person sees, and text with real newlines does not
take fewer rows when the window gets wider.

**A digest supplied by the caller.** The caller reads through the same capture
and gets a clipped screen with no digest. A hash of text nobody could read
proves nothing, and text supplied by the caller cannot be compared against rows
this driver cannot see.

**Provenance: this service knows what it pasted.** `Send`'s busy-composer guard
does prove the composer was empty before a delivery. But the composer now holds
that delivery plus whatever any other writer added since, and this service is
not the only writer. Other processes on the same multiplexer server can send
keys, the runtime can take input over its own remote channel without going
through the multiplexer at all, and the runtime can put notices back into the
composer itself. #148's field case was exactly that last one, so the
motivating incident was not a service paste. The multiplexer's view of attached
clients covers only one of those writers.

**An explicit destructive opt-in on discard.** A caller setting a flag is not
evidence, because the caller cannot see the text either. This is exactly the
loosening #149 ruled out, and it would be the first path in this driver that
deletes text nobody read. #136's `force` is not a precedent: it strengthens a
clear only after corroboration has passed.

**A runtime keybinding that moves the draft aside instead of deleting it.**
Deferred rather than rejected. It depends on the runtime version, and this
driver cannot confirm that the text was saved rather than lost, so it has the
same verification gap and no scrollback to fall back on.

## Consequences

- Every clipped-composer refusal is permanent for as long as the state lasts,
  and now says so. A caller that follows the message stops retrying and goes to
  a person, instead of looping on advice that cannot be followed.
- `FLEET_CAPTURE_LINES` is still the operator lever against *entering* the
  state, and only for composers no taller than the visible pane plus the larger
  margin. Those extra rows come from scrollback, so they have the same
  staleness caveat as the rejected wider read.
- **An existing gap, named here and not fixed.** `composerFound` can already
  depend on scrollback rows inside the `-S -N` margin. A composer taller than
  the visible pane, but within the margin, gets a digest built partly from
  frozen rows, and raising `FLEET_CAPTURE_LINES` widens that margin. It is a
  separate question from #149 and needs its own issue if it is to be pursued.
  *Taken up by #169: such a composer is now clipped, not found — see
  `169-a-composer-is-read-from-the-visible-pane.md`.*
- **Not yet observed on a live pane:** whether a real runtime's composer grows
  past pane height plus N (ADR 134, Consequences). The argument above does not
  depend on it. The counters answer it.

## Reopen when

Either of these becomes true — and the counters above show the state is common
enough to be worth the work:

- the runtime offers an authoritative way to read its composer that does not
  go through the terminal, or
- a measured redraw contract, pinned to a runtime version, proves that
  scrollback above the visible pane holds the current composer frame.
