# A box above a composer whose closing rule is off the bottom reads as no composer at all

**Issue:** #215 (tmux: the feedback-review footer reads as idle with no prompt)

## What broke

A session showed the runtime's feedback-draft card and sat there for hours. The
state read said `idle`, `composerDigest: null`, `prompt: null`; every send was
refused with "no composer has been painted ... a full-screen interface". A
supervisor tried eight times, got the identical refusal eight times, and backed
off.

## The evidence

The pane was 24 rows by 80, the size the multiplexer gives a detached session.
The card is about seven rows. What was left ended on the composer's *opening*
rule — labelled with the session's name — and its empty `❯` row. No closing rule,
no mode row. `composerSpan` takes the LAST rule on screen as the closing fence,
walks up from it looking for a `❯` row, and met the card's bottom border
(`╰──╯`, which `isRule` accepts: three rule runes) first. That is a positive
"absent", not "cannot tell".

The two readers of the screen then disagreed. The state read reached the
finished-spinner branch, which never asks whether a composer exists, and said
"composer empty". The send gate asked, and was told no. Same screen, two answers.

## The rule it produced

- The card is not a dialog (the runtime's own source: its keys act only while the
  composer is empty). On a pane tall enough to show the composer whole it blocks
  nothing and is not reported. It is a prompt only while it hides the composer.
- A refusal that guesses a cause is worse than one that names none. The old
  wording blamed startup and pointed at `keys`, where Escape on an empty composer
  dismisses the draft — the one key a client should not be steered towards.
- A box is untrusted where it is drawn from agent text: the card's body is the
  agent's own draft, and it sits where the usage-limit and failed-turn scans read
  the runtime's notices from. Those scans end above a recognised card.

## Not fixed here (filed on the issue; done in #216, `docs/adr/216-a-composer-cut-off-by-the-pane-bottom-reads-clipped.md`)

A `❯` row below the last rule at the bottom of the visible pane is an unclosed
composer and should read *clipped*, not *absent*; the finished-spinner branch
should not say "composer empty" for a composer it did not find. Both change what
`keys`, `discard` and `send` decide for every session, so they need their own
measured set. Sizing new panes explicitly is a mitigation only: a client that
attaches resizes the window.
