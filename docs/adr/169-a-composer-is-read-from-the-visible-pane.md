# ADR 169 — A composer is read from the visible pane, not from the history margin

**Status:** accepted (2026-09-14)
**Issue:** colab-fleet #169 · builds on #134, #149

## Context

The classify capture is the visible pane plus N rows of history (`-S -N`, no
`-E`). `composerSpan` walks up from the composer's closing rule to the ❯ row and
the opening fence. Before this change it did not know where the visible pane
began. A composer taller than the pane, but still inside the margin, had its
fence and possibly its ❯ row and first text rows in scrollback, and still read
as `composerFound`. The published `composerDigest` could then be built partly
from rows the runtime no longer shows. That digest is what `discard`'s `expect`
corroborates against, and discard deletes text.

ADR 149 named this gap under Consequences and left it open. #169 asked which of
two states such a composer should be: **clipped**, or a weaker **"found in
scrollback"** that publishes no digest. It also asked for a measurement first,
including whether the change would make today's ordinary tall composers
unreadable.

### What was measured

One machine, every multiplexer pane at the time (29, all running the agent
runtime):

- **29 of 29 were on the alternate screen.**
- **28 had no scrollback at all.** Their `-S -24` capture returned exactly the
  pane height, so the margin held nothing.
- **One had 92 rows of history.** Its margin rows were startup warnings the
  runtime printed on the normal screen *before* entering the alternate screen.
  They are not an older frame of any composer.

The peer could not be sampled from this session. #149's refusal counters could
not be read either: the deployed build predates them.

Two findings follow. First, on this runtime the margin never holds a composer
row, so no composer found today relies on it. Treating a fence in the margin as
clipped makes nothing readable today unreadable. Second, when the margin does
hold rows, they are not merely stale. They can come from before the runtime drew
anything, so a composer "found" through them would be spliced from unrelated
output. That is worse than the stale-frame hazard #169 described.

## Options

- **A — clipped.** A composer whose opening fence is above the visible pane is
  `composerClipped`. Every verb already refuses that state (ADR 134), a read
  already publishes no digest for it, and ADR 149's remedy already applies.
- **B — a fourth scan state, "found in scrollback".** It publishes no digest.
  But without a trusted digest there is no `expect` to corroborate against, so
  discard must refuse. Send's busy-composer guard cannot prove emptiness either,
  so it must refuse, and keys the same. Every caller would have to handle a new
  state whose behaviour is exactly clipped.

## Decision

**A.** B's behaviour collapses into A's, and B would add a state that every
`composerScan` consumer must learn never to fold into found or absent. That rule
already holds for clipped, and it is the one ADR 134 had to teach the codebase
once already.

Mechanically:

- The pane height travels with the capture, in the same multiplexer invocation.
  The batched capture's marker becomes `display-message -t <pane> -p
  "<mark><index> #{pane_height}"`. `-t` is required: without it the format
  expands against the current pane, not the captured one. `captureForClassify`
  chains `display-message -t <pane> -p "<nonce>H#{pane_height}"` *after*
  `capture-pane` and reads only the output's last line as the trailer. A pane
  row that happens to look like a trailer is never taken for one.
- `screen.visibleTop` is the output row count minus the pane height, counted
  before trailing blank rows are dropped. `composerSpan` returns clipped when
  the fence row is above it. The fence is at or above the ❯ row and every text
  row, so a fence inside the pane puts the whole composer inside it.
- A height that is missing or unparseable is 0, meaning unknown. Every row is
  then treated as it was before this change. This is deliberately the old
  behaviour, not a refusal. A capture that failed outright still fails closed as
  before; only the boundary is unknown here.
- Refusals that happen *only* because of this rule, where the same rows without
  the boundary would read as found, increment
  `composer_clipped.fence_above_visible_pane` alongside the existing per-verb
  `composer_clipped.refused_*` counter.

## Consequences

- `FLEET_CAPTURE_LINES` no longer makes a tall composer readable at all. The
  only composer this driver reads is one that fits the visible pane. A wider
  margin still gives the rest of the classification more transcript context.
  ADR 149's description of that setting as a way to avoid *entering* the
  clipped state no longer holds.
- A composer taller than the pane on a runtime that does **not** use the
  alternate screen was read as found before and is now refused, including by
  send. That is the intended trade: its digest rested on rows this driver
  cannot vouch for. `composer_clipped.fence_above_visible_pane` is how often it
  happens.
- The digest of a whole screen (`screenDigest`) is unchanged. It fingerprints
  what was captured, and the boundary is not part of that.
- Each batched row carries two more arguments and the pane id a second time.
  The #141 chunk estimate counts both, so chunks get slightly smaller.

## Reopen when

`composer_clipped.fence_above_visible_pane` shows the rule refusing ordinary
traffic often enough to matter, **and** a runtime version offers a redraw
contract that makes scrollback trustworthy. That second condition is the same as
ADR 149's second reopen condition.
