# ADR: a stuck composer gets a stronger clear, not a destroyed session

**Issue:** #136
**Status:** decided

## Context

When a composer reaches a state `/discard` cannot clear, `/keys` refuses
(correctly — the composer holds text), and `/input` cannot complete, the only
remedy this driver offered was `discardProvenFutile`'s own refusal message,
which said so in as many words: re-read the composer, and "if it genuinely
must go, close the session (`DELETE /v1/machines/{machine}/sessions/{id}`)
rather than retrying the same digest indefinitely."

Deleting a session to clear a text box is disproportionate. A session
carries a conversation, a bridge, in-flight work and, for a caller that
binds them, a claim and a worktree — none of which respawning recovers. The
gap was never the refusal itself (refusing to press blindly against a
proven-futile residue is correct, and `discardProvenFutile` is a genuine
improvement over the "safe to retry" lie it replaced — #87) — the gap is
that the honest refusal dead-ended at a remedy that costs orders of
magnitude more than the problem.

**Measured 2026-08-28** (see #136's own filed body for the full sequence): a
305-character residue, three `resumeIfStranded` retries all `unknown`, a
`discard` refused for lacking a digest (`composerDigest` read `null` while
the session was `working` — the only place the digest appeared was inside
the refusal body itself), a subsequent `discard?expect=<that digest>` 409'd
as "damaged, not merely unclear", and finally `discardProvenFutile` itself.
`keys {"key":"Escape"}` refused (composer holds text, correct by design).
**The three write paths — `discard`, `keys`, `input` — referred to each
other in a cycle, and `DELETE` was the only exit.**

## Did #138 alone close this measured case?

**Not verified against a live pane in this change.** #138 (positioning with
`composerLineEndKey` before `C-u`, plus the no-movement latch alternating to
`Backspace`) targets the identical mechanism that produced the #136 field
case — both were measured in the same session, against what is plausibly the
same underlying residue shape (text sitting on or near the composer's own
❯-marked row, where the structural key choice's cursor-position assumption
does not hold). It is plausible, on that basis, that #138 alone would have
cleared #136's specific measured residue.

But this repository's development environment has no live tmux multiplexer
to replay the sequence against, and no captured transcript of the exact
failing screen was preserved verbatim in a form this change could feed back
through `clearComposer` directly. The fake-mux test suite (see #138's own
`TestClearComposerAlternatesAfterAPressThatMovedNothing`) proves the
mechanism converges in a *model* of the failure shape; it does not prove the
live #136 incident specifically would have converged, because the fake does
not simulate real terminal cursor-position state — it simulates "the
positioning key does or does not help", which is an assumption about the
world, not a fact observed from one.

**Given that uncertainty, the decision below does not branch on the answer.**
The mechanism is the same whether #138 already closed the measured case or
not: a caller-visible escape hatch that does not require destroying the
session is worth having regardless, because the NEXT residue shape that
defeats the structural pass — whatever shape that turns out to be — will not
necessarily be the same shape #138 fixed. #136's own filed body already
names this explicitly: "informed by whether #138 alone closes the measured
failure" was the plan, and the honest answer this ADR can give is
**"plausibly yes, not confirmed"** — which is itself the argument for adding
the safety net anyway rather than declaring victory on an unconfirmed
inference.

## Decision

**Add `?force=true` on `POST .../discard`, backed by a new character-budgeted
Backspace sweep (`clearComposerSweep`), reachable only once the ordinary pass
has already been proven futile against this exact residue.**

- **The mechanism (issue's option 1).** `clearComposerSweep` positions with
  `composerLineEndKey`, then presses `Backspace` `len([]rune(pending)) +
  sweepMargin` times (capped at `maxSweepBackspaces`), batched into a small
  number of `send-keys` calls rather than one call per character. Backspace
  is the universal primitive here: it deletes the ONE character behind the
  cursor unconditionally, crosses a newline exactly the way it crosses any
  other character (merging rows the way #132's own Backspace branch already
  does at the structural level), and cannot escape the composer widget — a
  Backspace at the very start of a text input is a no-op, not a way out of
  the box. It does not depend on getting a row/cursor reading right the way
  `clearComposer`'s structural choice does; it is deliberately blunter and
  more exhaustive in exchange for not needing to be clever.
- **Gated behind an explicit flag (issue's option 2), because it is both.**
  `driver.DiscardOptions{Force: bool}` — a new field on a new options struct,
  following the `SendOptions` precedent rather than a positional bool.
  `Force` has no effect until `futileClearAttempts` has already fired for
  this exact residue: forcing is a remedy for PROVEN futility, not a
  shortcut around attempting the ordinary pass first. It never relaxes
  `expectDigest` — the corroboration check in `Discard` runs identically
  whether `Force` is set or not, because a forced clear is *more*
  destructive than the ordinary pass, not less.
- **`discardProvenFutile`'s message rewritten** to name `?force=true` as the
  next step instead of `DELETE`. This is the single grep-able deliverable
  for this issue: the message no longer terminates in "close the session" as
  the only remedy.
- **Signature ripple, deliberately touching every implementation.**
  `driver.Driver.Discard` gained a `DiscardOptions` parameter, updated in all
  five implementations (`tmux`, `stub`, `opencode`, `remote`, and
  `driver_test.go`'s `fakeDriver`) plus `internal/service/http.go`'s
  `handleDiscard`, which parses `force=true` as its own query parameter —
  never folded into or inferred from `expect`. `remote.go` forwards `Force`
  on the wire as its own query parameter for the identical reason
  `classifyCaptureArgs` centralises the capture argv shape: a capability
  that works on a local driver and silently vanishes at the federation
  boundary is exactly the class of drift a single forwarding point exists to
  prevent, pinned by `TestDiscardForwardsForceOnTheWire`.

## Alternatives considered

**Let `keys` send `Escape` when the caller has corroborated the composer
digest (issue's option 3).** Rejected on this repository's own prior
measurement: `clearComposer`'s doc comment already records "C-a C-k and
Escape were tried too; C-u alone was enough for a single line" — Escape does
not clear this composer. `discardProvenFutile`'s own message already cited
this. Opening `keys` to Escape here would hand a caller a door that
measurably does not open, and would collide with #127's composer-scope
digest reasoning: a composer this driver cannot corroborate by content (the
`composerClipped` case, #134) has no meaningful composer digest to check
`keys` against in the first place.

**Make the sweep automatic after futility, with no flag.** A caller cannot
currently express "I want the stronger, more destructive clear" as an
explicit decision under this shape — the driver would simply escalate on its
own. Rejected: the entire point of `discardProvenFutile`'s message is to
give the caller a nameable next step, and a flag is what makes the escape
hatch *nameable* in that refusal — "retry with `force=true`" is something a
caller (or a human reading the error) can act on directly, where "the driver
will try harder on the next call automatically" is not a caller decision at
all and removes the corroboration checkpoint a second explicit call
provides.

## Consequences

- The three-way refusal cycle #136 measured (`discard` → `keys` → `input` →
  `discard`...) now has an exit that does not require `DELETE`: `discard
  ?expect=<digest>&force=true`.
- `force` is a real capability expansion on a public API surface — it
  deletes by character count from text whose structure this driver has
  already admitted (by reaching `discardProvenFutile` at all) it could not
  fully reason about. `expectDigest`'s unconditional requirement is the only
  guard standing between this and blind destruction of a composer's
  content; nothing about `force`'s convenience should ever be allowed to
  relax that guard later.
- Whether #138 alone would have closed the specific 2026-08-28 measured case
  remains genuinely unconfirmed (see above) — this is recorded here rather
  than asserted either way, so a later reader does not mistake this ADR's
  silence for a claim that was never actually verified.
- `clearComposerSweep` was not measured against a live stuck composer in
  this change either; like #138's own `End`-positioning fix, it is validated
  against the fake-mux model, not a field case. A field measurement of both
  #138 and #136 together, against a residue that reproduces the original
  incident, remains open work.
