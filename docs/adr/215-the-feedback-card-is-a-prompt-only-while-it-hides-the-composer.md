# ADR 215 — The feedback-draft card is a prompt only while it hides the composer

**Status:** accepted (2026-09-26)
**Issue:** colab-fleet #215 · builds on #134, #58

## Context

When an agent drafts product feedback the runtime paints a bordered card directly
above the composer: `1 to review · 2 to send · 0 to dismiss`. One such card sat on a
session for hours. The state read said `idle`, `composerDigest` and `prompt` were
absent, and every send was refused with "no composer has been painted", so a
supervisor's pings failed identically eight times and it backed off. The Issue asked
for the card to be classified as a prompt — `state.prompt` with a kind, the options
and a nonce, `status: waiting_input` — so the wait is visible and a person can answer
it through `respond`, and never answered by a client on its own.

### What was measured

- **Geometry.** The stalled pane was 24 rows by 80, the multiplexer's default for a
  detached session, which is what every created session is. The card is about seven
  rows. The screen ended on the composer's *opening* rule and its empty prompt row;
  the closing rule and the mode row were below the pane. `composerSpan` takes the
  last rule as the closing fence, walked up from the opening rule, met the card's
  bottom border before any prompt row, and ruled the composer *absent*. The state
  read's finished-spinner branch never asks about the composer and said "composer
  empty"; the send gate asked, and refused.
- **The card is not modal.** Read out of the runtime's own binary (one build): its
  three keys are read through the composer's input value and act only while that
  value is empty. A message pasted into the composer is delivered normally. Only a
  lone digit, or Escape on an empty composer, reaches the card. The card is hidden
  while a turn runs and returns after.
- **A taller pane is fine.** With the closing rule visible the composer reads
  whole, the session is idle, and sends work. A human-attended capture showed text
  typed into the composer under the card.

## Decision

**Report the card as `waiting_input` with a `feedback-review` prompt only when it is
the one thing between a caller and the composer:** a validated card directly above
the composer's fence, the composer *not* readable as a whole, and its prompt row
visible, empty (or the dim placeholder) and last on screen. Everywhere else the
screen reads as it did.

- **Card, composer readable (a taller pane):** no prompt. Idle, as it is.
- **Card, composer unreadable, prompt row holds text:** not a prompt (a digit would
  be appended to the text) and not idle: `unknown`, evidence naming the card, and
  `send`, `keys`, `discard` and `respond` refuse by name.
- **Options** are the runtime's verbs, `["review","send","dismiss"]`, nothing
  highlighted. The **keys** that answer them (1, 2, 0) live in the menu shape, not the
  option index: choice 3 is delivered as `0`.
- **Never answered on a client's behalf.** Not consentable. `respond` refuses
  `cancel` (Escape dismisses the draft and may be followed by a question about
  turning drafts off), a missing choice (no default to accept) and a missing nonce
  (the options are identical on every draft, so only the nonce ties an answer to
  this one). One key, once, then a read-back.
- **Recognised structurally**, never by the words of the draft: the exact key row
  inside a box borders-and-all, directly above the live composer's fence, at the
  chrome's column. The kind is set by the parser and stands without option matching.

## Alternatives rejected

- **Always `waiting_input` when the card is on screen** (the Issue's literal ask).
  It would stop every session that drafts feedback from taking work until a person
  decides about the draft, including sessions on a pane tall enough to read the
  composer and that would take the message. Turning an ambient notice into a gate is
  a regression for the working case to fix the broken one.
- **A prompt with `status: idle`.** Every consumer treats `prompt` as the sign of
  `waiting_input`; about eight call sites and every supervisor rely on it.
- **Fix `composerSpan` so a bottom-clipped composer reads as found or clipped.** The
  right long-term repair for the underlying disagreement, and left out on purpose: it
  changes what `keys`, `discard` and `send` decide for every session, and needs its
  own measured set. Recorded as a follow-up.
- **Size new panes explicitly.** A mitigation only: a client that attaches resizes
  the window, and sessions not created through this service are untouched.

## Consequences

- A session on a short pane with the card up is now visible, answerable and refused
  by name instead of reading as healthy and refusing as broken. On a taller pane
  nothing changes.
- The status can flip between `waiting_input` and `idle` when a client attaches and
  resizes. Width changes truncate the draft's rows and so change the nonce; a stale
  answer is refused, which is the safe direction.
- The card's other states — the review view, the send confirmation, an error line,
  the question about turning drafts off — were read from the runtime's source and
  never captured, so they are deliberately unrecognised. After choosing `send` the
  runtime asks to confirm; the receipt says so, and the screen reads as before until
  it is captured.
- Because the card sits where the usage-limit and failed-turn scans read the
  runtime's notices from, those scans now end above a recognised card; its body is
  the agent's own prose and no longer reads as one.
- A message that is only `1`, `2` or `0` would be read by the card as its shortcut, so
  `send` refuses one while the card is on screen.
- No compat check can produce the card (it needs the agent to draft feedback), so the
  compat pack has no entry for it. A static wording check needs the key row's words
  measured as literals in a build first.

## Reopen when

A second card-shaped notice appears in the same place (the recogniser keys on this
one's exact key row), or `composerSpan` learns to read a bottom-clipped composer — at
which point whether the card should be a prompt on a short pane is worth asking
again.
